# shellcheck shell=sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Sourced by every examples/*/run.sh. It starts the local stub of GitHub in
# internal/demostub on a free port, builds ghattach against it, and moves into
# a temporary directory that is removed on exit. Nothing an
# example runs can reach GitHub or the machine's own session:
#
#   - ghattach's base URL is the stub, set at build time.
#   - A fake gh first on PATH answers the repository id lookup for OWNER/REPO.
#   - HTTP_PROXY, HTTPS_PROXY and their lowercase forms point at a closed
#     loopback port, and NO_PROXY exempts only 127.0.0.1 and localhost, so a
#     request to any host other than the stub fails instead of leaving the
#     machine.
#   - HOME is an empty temporary directory. The cookie store is written there,
#     and a Chrome re-read finds no cookie store there and fails.
#
# The stub accepts only the made-up session "fake".

repo=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
stub_pid=
cleanup() {
	if [ -n "$stub_pid" ]; then
		kill "$stub_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

mkdir "$work/bin" "$work/home"
# Built before HOME moves, so go keeps using its own module and build caches.
(cd "$repo" && go build -o "$work/stub" ./internal/demostub) || exit 1

# The stub binds a free port and writes the address it got to stub.addr, so
# examples started side by side, or next to anything else on this machine,
# never compete for a port. ghattach is built only once that address is known.
"$work/stub" -addr 127.0.0.1:0 -addr-file "$work/stub.addr" 2>"$work/stub.log" &
stub_pid=$!
stub_failed() {
	echo "the stub did not start (address: ${addr:-none written}):" >&2
	cat "$work/stub.log" >&2
	exit 1
}
tries=0
until [ -s "$work/stub.addr" ]; do
	tries=$((tries + 1))
	if [ "$tries" -ge 100 ] || ! kill -0 "$stub_pid" 2>/dev/null; then
		stub_failed
	fi
	sleep 0.1
done
addr=$(cat "$work/stub.addr")
tries=0
until curl -fs --noproxy '*' -o /dev/null -b user_session=fake "http://$addr/OWNER/REPO"; do
	tries=$((tries + 1))
	if [ "$tries" -ge 100 ] || ! kill -0 "$stub_pid" 2>/dev/null; then
		stub_failed
	fi
	sleep 0.1
done

(
	cd "$repo" &&
		go build -o "$work/bin/ghattach" \
			-ldflags "-X github.com/tetriz-ai/ghattach/internal/cmd.githubBaseURL=http://$addr" \
			./cmd/ghattach
) || exit 1

cat >"$work/bin/gh" <<'GH'
#!/bin/sh
# Stands in for the GitHub CLI. It answers ghattach's repository id lookup
# for OWNER/REPO and refuses everything else.
if [ "$*" = "api repos/OWNER/REPO --jq .id" ]; then
	echo 123456789
	exit 0
fi
echo "fake gh: refusing: gh $*" >&2
exit 1
GH
chmod +x "$work/bin/gh"

PATH=$work/bin:$PATH
HOME=$work/home
# An inherited NO_PROXY, such as NO_PROXY=*, would let requests bypass the
# closed proxy, so both forms are cleared and only loopback, the stub, is exempt.
unset NO_PROXY no_proxy
HTTP_PROXY=http://127.0.0.1:9
HTTPS_PROXY=$HTTP_PROXY
http_proxy=$HTTP_PROXY
https_proxy=$HTTP_PROXY
NO_PROXY=127.0.0.1,localhost
export PATH HOME HTTP_PROXY HTTPS_PROXY http_proxy https_proxy NO_PROXY
cd "$work" || exit 1

#!/bin/sh
# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT
#
# Runs every example under examples/ against the local stub of GitHub. For
# each one it runs, from the repository root, exactly the command in the sh
# block of that example's README.md, and compares standard output with the
# README's last fenced block. CI runs this script as the Examples job.
#
# Before the comparison, the temporary home directory is written as /home/you
# and each asset id as <uuid>, the way the README shows them. Nothing else is
# rewritten, so any other change in the output fails the run.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

if ! command -v shellcheck > /dev/null 2>&1; then
	echo "scripts/run-examples.sh needs shellcheck on PATH" >&2
	exit 1
fi
shellcheck -x scripts/run-examples.sh scripts/example-setup.sh examples/*/run.sh

# The index links every example folder, and nothing else.
listed=$(grep -o '](\([a-z0-9-]*\)/)' examples/README.md | sed 's#^](##; s#/)$##' | sort)
present=$(for d in examples/*/; do basename "$d"; done | sort)
if [ "$listed" != "$present" ]; then
	echo "examples/README.md does not list exactly the example folders" >&2
	echo "listed:  $(echo "$listed" | tr '\n' ' ')" >&2
	echo "present: $(echo "$present" | tr '\n' ' ')" >&2
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
status=0
for dir in examples/*/; do
	name=$(basename "$dir")
	awk '/^```/ { if (f) { f = 0; last = buf } else { f = 1; buf = "" } next }
		f { buf = buf $0 "\n" }
		END { printf "%s", last }' "${dir}README.md" > "$tmp/want"
	# The command a reader copies: the README's first sh block, run as is.
	awk '/^```/ { if (f) { f = 0; if (lang == "sh" && !done) { cmd = buf; done = 1 } }
			else { f = 1; buf = ""; lang = substr($0, 4) } next }
		f { buf = buf $0 "\n" }
		END { printf "%s", cmd }' "${dir}README.md" > "$tmp/cmd"
	if [ ! -s "$tmp/cmd" ]; then
		echo "FAIL $name: ${dir}README.md has no sh block with the run command" >&2
		status=1
		continue
	fi
	if ! sh "$tmp/cmd" > "$tmp/raw" 2> "$tmp/stderr"; then
		echo "FAIL $name: the README's command exited non-zero: $(cat "$tmp/cmd")" >&2
		cat "$tmp/stderr" >&2
		status=1
		continue
	fi
	sed -e 's#/[^ ]*/\.config/ghattach/#/home/you/.config/ghattach/#g' \
		-e 's#assets/[0-9a-f-]\{36\}#assets/<uuid>#g' "$tmp/raw" > "$tmp/got"
	if diff -u "$tmp/want" "$tmp/got" > "$tmp/diff"; then
		echo "ok   $name"
	else
		echo "FAIL $name: output differs from ${dir}README.md" >&2
		cat "$tmp/diff" >&2
		status=1
	fi
done
exit "$status"

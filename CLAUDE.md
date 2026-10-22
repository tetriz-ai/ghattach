## What this is

A Go CLI and its only public API. `upload` sends local files to GitHub's user-attachments CDN
and prints each URL or markdown embed, `login` stores the browser session it needs, and
`logout` deletes it. There is no library API, no token login, and no delete, list or download.

## Layout

- `cmd/ghattach/`: `main.go`. `main_test.go` builds the binary and runs it with an empty HOME.
  `examples_test.go` ties the README's Quick start to `examples/embed-one-screenshot`.
- `internal/cmd/`: every command and the upload client. Tests sit beside the code and run
  against a local stub from `newCLIStub` in `cli_test.go`. Help goldens are in `testdata/help/`.
- `examples/`: one folder per task, and the index `examples/README.md`. `scripts/run-examples.sh`
  runs them. `scripts/example-setup.sh` is the setup each `run.sh` sources.
- `internal/demostub/`: the local stub of GitHub behind the examples.
  It refuses an image over 10 MiB, the server-side failure `examples/handle-failures` shows.
- `.github/workflows/ci.yml` is CI. `third_party/licenses/` is made by `.github/scripts/`.

## Commands

The minimum Go is 1.26.10, the `go` line in `go.mod`. An older local Go fetches it through
`GOTOOLCHAIN=auto`. CI tests Go 1.26 and 1.27 on Linux and Go 1.27 on macOS. Lint,
examples, licenses and the release dry run use the newest Go 1.27 patch. CI pins golangci-lint
v2.14.0, which needs Go 1.27: with `GOTOOLCHAIN=auto` its `go install` stops at Go 1.26, so set
`GOTOOLCHAIN` to a Go 1.27 release. The release dry run is goreleaser v2.18.2 with
`release --snapshot --clean`. The examples runner also needs `shellcheck` and `curl`.

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
golangci-lint run   # golangci-lint v2.14.0, reads .golangci.yml
scripts/run-examples.sh
go install github.com/google/go-licenses/v2@v2.0.1
.github/scripts/third-party-licenses.sh --check
```

## Contract to keep

- **No test or example reads a real session.** `TestMain` in `internal/cmd/main_test.go` moves
  HOME to a temp dir and panics if the Chrome reader or keychain is reached without a fake.
- **The cookie goes only to GitHub.** No redirect is followed, and the storage upload carries
  no cookie. Pinned in `client_test.go` and in `contract_test.go` by `TestS3Upload`.
- **A failure is an error, never silence:** exit 1, one `Error: <message>` line, no usage.
  Pinned in `cmd/ghattach/main_test.go` and in `help_test.go`.
- **Every file is checked before any request,** so a bad argument leaves nothing uploaded.
  Pinned by `TestUploadChecksEveryFileBeforeAnyRequest`.
- **Flags, standard output, exit status and the cookie store path are the public contract,**
  pinned by `help_test.go` against the goldens and by `cli_test.go`. The store is mode 0600.
- **Only a 401, or a redirect to `/login` on github.com, is an expired session,** and Chrome is
  re-read at most once per run. Pinned in `contract_test.go` and by `TestUploadSelfHeal`.
- **What ghattach prints is safe to paste:** the URL must be a user-attachments URL, the embed
  text is escaped, and server text in an error has no control characters.
- **`docs/limitations.md` holds only limits of GitHub, Chrome or macOS and stated design
  choices,** each pinned by a test, the design choices in `limits_test.go`. A bug is fixed, not
  listed there. With no stored session, an upload fails before any request and never falls
  back to Chrome.

## Making a change

- A behaviour change needs a test that fails before it and passes after. Tests are
  table-driven. Command cases go in `cli_test.go`, request cases in `client_test.go`.
- Help text changes regenerate the goldens with CONTRIBUTING.md's command, then review the diff:
  `go test ./internal/cmd -run TestHelpText -update`
- A behaviour change adds a `CHANGELOG.md` line under Unreleased. A new example needs none.
- A dependency change reruns `.github/scripts/third-party-licenses.sh` and updates the
  module's version in NOTICE by hand. No check covers NOTICE.
- Fix lint findings in the code, not `.golangci.yml`. A `//nolint` names its linter and reason.
- A change to what ghattach prints also updates the example READMEs and the blocks in `README.md` and `docs/`.
- **A new example** is `examples/<task-in-words>/` with an executable `run.sh` and a
  `README.md`. Copy an existing one, including its `# shellcheck source=` line. The setup
  gives `$repo`, the session `fake`, a fake `gh` for `OWNER/REPO`, a temp cwd, and an EXIT
  trap that stops the stub. A `run.sh` sets no trap of its own.
- The runner runs the README's `sh` block from the repo root, then compares standard output
  with its last block, a `text` block. It rewrites the temp HOME as `/home/you` and asset ids as
  `<uuid>`. The index table must link every folder as `[name](name/)`, and nothing else. Link
  a new example under the code block of its section in `docs/usage.md` or the README's Common
  tasks.

## Do not

- Do not run or edit `live-upload.yml`. Never let a test or example reach github.com or a real
  `gh`. The setup fakes `gh`, clears any inherited `NO_PROXY`, and blocks all but loopback.
- Do not run a built `ghattach login` or `upload` outside an example or test. They use the
  real `~/.config/ghattach` and, on macOS, Chrome's cookie store and keychain.
- Do not edit `internal/cmd/testdata/help/` by hand, and do not raise the `go` line in `go.mod`.

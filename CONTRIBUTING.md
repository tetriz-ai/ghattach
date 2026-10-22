# Contributing

Thank you for helping. The scope is deliberately small (see the README), so please open an
issue before starting on a new command or a change to what ghattach prints. That way we can
agree on it before you write the code.

## Run the checks

CI runs these checks on every pull request, with the license check below and a release dry run.
The minimum Go version is 1.26.10.

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
scripts/run-examples.sh   # needs curl and shellcheck
golangci-lint run         # v2.14.0 as in CI, reads .golangci.yml
```

To build and install the code you checked out, run `go install ./cmd/ghattach` from the clone.

The tests and the examples use a local stub of GitHub. They never need a GitHub account or a
cookie, and they never reach GitHub. The command tests run the real commands against the stub
and never read a real cookie or the keychain. The help text of every command is compared with
the files in `internal/cmd/testdata/help`.

[The live upload workflow](.github/workflows/live-upload.yml) uploads one file to a real
repository. It runs only when started by hand.

## Every change needs a test

A pull request without a test is not merged. Tests are table-driven, so follow the existing
files. A change to a flag, to what is printed or to an exit status is a breaking change (see
"Versioning and compatibility" in [docs/reference.md](docs/reference.md#versioning-and-compatibility)),
so it also needs a `CHANGELOG.md` entry.

If you change help text, regenerate the golden files and review the diff:

```sh
go test ./internal/cmd -run TestHelpText -update
```

Never put a real cookie in a test, a fixture or an issue.

## Third-party licenses

The release archives ship `third_party/licenses`, the license texts of Go and of every module
compiled into the binary. After a dependency change, regenerate the folder and commit it:

```sh
.github/scripts/third-party-licenses.sh
```

CI fails when the folder does not match what is compiled in. The script needs the
license-report tool, installed with `go install github.com/google/go-licenses/v2@v2.0.1`.

## Commit style

Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/): a short subject
such as `fix: report a 303 to the login page as an expired session`, a blank line, then a
body that says why.

## Licensing of contributions

No sign-off line and no separately signed agreement are needed. By opening a pull request you
agree that your contribution is released under the project's [LICENSE](LICENSE).

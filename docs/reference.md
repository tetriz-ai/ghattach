# Reference

The exact rules for each command. The full help for each one is `ghattach <command> --help`.
Every upload needs a stored session from `login`, and every upload goes to one repository.

## `ghattach upload <file>...`

| Flag | Meaning |
|---|---|
| `--repo`, `-R` | Target repository as `OWNER/REPO`. Required. |
| `--repo-id` | The repository's numeric id. `0`, the default, means look it up with `gh`. A negative id is refused. |
| `--markdown` | Print the Markdown embed instead of the bare URL. |

What `--markdown` prints, by the media type GitHub records for the upload:

| Media type | Printed |
|---|---|
| `image/*` | `![name](url)` |
| `video/*` | `url` |
| anything else | `[name](url)` |

A video prints the bare URL because GitHub shows an inline player only for a user-attachments
URL on a line of its own.

GitHub records the media type ghattach sends, which comes from the file's extension, ignoring
case. Six extensions are pinned, so they map the same way on every machine:

| Extension | Media type |
|---|---|
| `.mp4` | `video/mp4` |
| `.mov` | `video/quicktime` |
| `.webm` | `video/webm` |
| `.txt` | `text/plain` |
| `.zip` | `application/zip` |
| `.log` | `text/x-log` |

### Rules

| Case | What happens |
|---|---|
| `--repo` | takes letters, digits, `-` and `_` for the owner, and also `.` for the repository. Anything else, such as a third `/` or a `?`, is refused before any request. |
| Other extensions | use Go's and the operating system's MIME tables, with any parameter such as `charset` removed. An unknown extension is `application/octet-stream`. When GitHub records no media type, the one ghattach sent is used. |
| Every file is checked | before the repository id lookup and the first upload, and again just before its own upload. A missing file, a directory, an empty file or a file over 100 MiB fails the run with nothing uploaded. GitHub's own limits are in [usage.md](usage.md#githubs-size-limits). |
| File names | lose every control character before upload. |
| Output | is one line per uploaded file on standard output, in the order given. A line that cannot be written fails the run. A successful run writes to standard error only after a session re-read from Chrome, which prints `Re-read the session from Chrome; retrying.` |
| The printed URL | is always a `https://github.com/user-attachments/assets/` URL. Any other answer from GitHub fails the upload. |
| The embed's text | is the name GitHub returns, or the local file name when it returns none. A `[`, `]` or `\` in it is escaped. |
| Session expiry | is a 401, or a redirect to `/login`, relative or on github.com or www.github.com, ignoring case and a trailing slash. |
| A redirect elsewhere | from the repository page, such as for a renamed repository, is reported as a move. |
| A redirect with no Location | reads as no access. |
| On expiry | at most once per run, ghattach re-reads Chrome from the pinned profile, if there is one. It retries, and stores the new cookie, only if Chrome holds a different one. |
| A 404 or 403 | from the repository page means no access or no such repository. It never triggers a re-read. |
| An organisation that enforces SAML SSO | serves its repository page without an upload token until the session is authorised. The error then names `https://github.com/orgs/<owner>/sso`, where you authorise it in a browser. |
| Visibility | An upload inherits the repository's visibility. On a private repository, only people with access to it can open the link. |

### The requests

An upload makes up to four requests, in this order.

1. `GET /OWNER/REPO` with the session cookie reads the page's `uploadToken`, once per run and repository, and after a re-read.
2. `POST /upload/policies/assets` with the cookie, token, repository id and file size returns a presigned storage form and an asset record.
3. `POST` of the file to the storage URL from step 2, with no GitHub cookie.
4. `PUT` to the asset's finalize path with the cookie. GitHub answers with the
   `https://github.com/user-attachments/assets/<uuid>` URL.

## `ghattach login`

| Flag | Meaning |
|---|---|
| `--from-chrome` | Read `user_session` from Chrome's cookie store. macOS only. |
| `--chrome-profile` | With `--from-chrome`, a file-name pattern for the profile directory, such as `Default` or `Profile 1`. A pattern that matches several profiles takes the freshest session. |
| `--cookie` | The `user_session` value or a full Cookie header. Pass `-` to read one line from standard input, since a value on the command line lands in shell history and `ps` output. |

### Rules

| Case | What happens |
|---|---|
| With no flag | `login` prints how to log in and stores nothing. It takes no arguments, so a value given without `--cookie` is an error, and the error does not repeat the value. |
| `--cookie` and `--from-chrome` | cannot be combined, and `--chrome-profile` needs `--from-chrome`. |
| An empty `--cookie` | is an error, and any stored session stays as it was. |
| A pasted value is checked | for bytes a cookie cannot carry, such as a space, `\`, `"` or `,`, and refused if it has any. |
| Only the cookie's form is checked | The first upload checks the value with GitHub, and a wrong or expired value fails there with `expired session`. |
| A piped value or a whole Cookie header both work | ghattach picks out `user_session`, with or without the `Cookie:` name in front. At a terminal prompt, paste only the value. |
| The prompt goes to standard error | so with a pipe it shares a line with the output. |
| Without `--chrome-profile` | ghattach reads every Chrome profile and takes the unexpired session that expires last. A profile whose store cannot be read is skipped, unless no profile yields a session. |
| A pinned profile is remembered | so an expired session is re-read from the same profile, and so the same GitHub account. |
| `--from-chrome` reads Google Chrome | from `~/Library/Application Support/Google/Chrome`. Chrome Beta, Canary and other Chromium browsers are not read. It also uses macOS's own `security` and `sqlite3` commands. |
| The decrypted cookie is checked | against the `github.com` hash that Chrome stores with it. A wrong key or a changed Chrome format fails with an error instead of storing a corrupt cookie. |
| A terminal paste of 1,000 bytes or more | is refused, because the terminal may have cut it short. On macOS, a paste of 1,024 bytes or more never arrives and the prompt waits. Pipe a long header instead, such as `pbpaste \| ghattach login --cookie -` on macOS or `xclip -o -selection clipboard \| ghattach login --cookie -` on Linux. |

## The session file

- **Location:** `~/.config/ghattach/cookies.json`, the same path on macOS and Linux. Each write
  sets the directory to mode 0700.
- **Contents:** JSON with `user_session` and, after `login --from-chrome --chrome-profile`,
  `chrome_profile`.
- **Writes** go to a temporary file with mode 0600 that is renamed into place.
- **A leftover `cookies-*.json` copy** is deleted by the next write once a minute old, and by `logout` at once.
- **Two runs** that store a session at the same moment both succeed.
- **State:** this file is the only state ghattach keeps. Each run is otherwise independent.

## `ghattach logout`

- **Deletes** `~/.config/ghattach/cookies.json` and any leftover `cookies-*.json` copies. It
  never contacts GitHub.
- **Prints** `Removed <path>` and a reminder to revoke the browser session at
  `https://github.com/settings/sessions`.
- **With nothing stored,** `logout` prints `No stored session.` and succeeds.
- **Only the local copy goes.** The session stays valid on GitHub until it expires or you
  revoke it.

## `ghattach completion`

`ghattach completion bash`, `fish`, `powershell` or `zsh` prints a shell completion script.
`ghattach completion <shell> --help` says how to load it.

## Exit status, errors and version

- **Exit 0** when the command succeeds. For `upload`, that means every file uploaded.
- **Exit 1** on any failure, with `Error: <message>` on standard error.
- **Control characters** in the message, as from a file name or a server's response, show as spaces.
- **The message** is always one line.
- **Usage text** is never printed on an error.
- **`--version`**, or `-v`, prints `ghattach version <version>`.
- **The version** is the one set with `-ldflags "-X main.version=..."`, else the module version recorded at build time.
- **`go install ...@v0.1.0`** builds a binary whose version is `v0.1.0`.
- **`-h`** is short for `--help` on every command.

## Versioning and compatibility

Releases follow [Semantic Versioning](https://semver.org). A breaking change is a flag renamed
or removed, a change to what is printed on standard output, a change to an exit status, or a new
location or format for the session file.

Before v1.0.0, such a change ships in a new minor version. Every release lists its changes in
[CHANGELOG.md](../CHANGELOG.md).

The minimum Go version is 1.26.10, the `go` line in `go.mod`. Build with a current Go patch
release, since the standard library's TLS and certificate fixes arrive that way.

- **CI** tests Go 1.26 and 1.27 on Linux, and Go 1.27 on macOS.
- **Release binaries** are built for Linux, macOS and Windows, on amd64 and arm64.
- **Windows** binaries are built, but CI does not run the tests on Windows.

Release archives carry the license texts of Go and of every module compiled in, from
`third_party/licenses`.

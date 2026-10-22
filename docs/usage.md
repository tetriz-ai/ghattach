# Usage

How-tos beyond the README's common tasks. The exact rules for each command are in
[reference.md](reference.md).

## Run it again

The session stays in `~/.config/ghattach/cookies.json`, so later runs need only
`ghattach upload`.

When GitHub expires the session, `upload` fails with `expired session (re-run: ghattach login)`,
and you log in again the same way. On macOS, `upload` first tries once to re-read the session
from Chrome, and prints `Re-read the session from Chrome; retrying.` when that works. Without a
pinned profile, that re-read can pick another GitHub account, as
[Keep one GitHub account](#keep-one-github-account) shows.

## Embed screenshots

Each file prints one line, in the order given. Every file is checked before the first upload,
so a missing or empty file leaves nothing uploaded.

```console
$ ghattach upload before.png after.png --repo OWNER/REPO --markdown
![before.png](https://github.com/user-attachments/assets/<uuid>)
![after.png](https://github.com/user-attachments/assets/<uuid>)
```

A refusal that only GitHub can make stops the run at that file, after the files before it have
uploaded. [examples/handle-failures](../examples/handle-failures/) shows a script that keeps the
embeds already printed.

## GitHub's size limits

ghattach refuses a file over 100 MiB before any upload. GitHub's limits are mostly lower. GitHub
documents 10 MB for images, 25 MB for other files, and 10 MB for videos on a free plan or 100 MB
on a paid plan, in
[Attaching files](https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/attaching-files).
A file over GitHub's limit fails with GitHub's error, not ghattach's.

## Use ghattach in a CI job

The session still comes from `login`. Store the `user_session` value from
[install.md](install.md#log-in-with-the-cookie-value) as a secret, and pipe it into
`ghattach login --cookie -`, as the README's
[Log in from a script or CI job](../README.md#log-in-from-a-script-or-ci-job) shows. Treat that
secret as your whole GitHub account, as [limitations.md](limitations.md#the-session-cookie)
explains. A stolen session can bypass a fresh password and two-factor challenge.

Use a dedicated machine account with access only to one throwaway public repository. Do not
use a personal account or grant that account access to organisation or production repositories.

The manual `live-upload.yml` workflow runs only from `main` and uses the `live-upload`
environment. Before running it, a repository administrator must configure that environment
with required reviewers, prevent self-review, restrict deployment branches to `main`, and
store `GHATTACH_TEST_SESSION` as an environment secret. Remove any repository or organisation
secret with that name so the environment is the only source. These protections require GitHub
settings; the workflow file does not configure reviewers. If the repository’s plan does not
support required reviewers, do not provision this secret or run the workflow. Review the
selected commit and target repository before approving each run.

Never pass the session to pull-request code, untrusted dependencies or a shared runner.
`logout` deletes only the local copy; revoke the machine account’s session on GitHub after
testing. Attachments can remain public after session revocation.

[examples/ci-comment](../examples/ci-comment/) is the whole upload step of such a job, from the
stored secret to a comment body.

## Keep one GitHub account

On macOS, an expired session is re-read from Chrome. Without a pinned profile, the freshest
Chrome profile wins, and it may be signed in to another GitHub account. Pin a profile to keep
one account:

```sh
ghattach login --from-chrome --chrome-profile Default
```

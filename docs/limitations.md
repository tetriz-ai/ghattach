# Limitations

Tests pin the design choices and ghattach's side of each GitHub, Chrome or macOS limit.
"Declined" marks a limit the project chose not to fix.

Each group lists the harmful direction first: what can expose your data or act as the wrong
account, then what fails when it need not.

## The session cookie

**Can expose or misdirect:**

- Anyone who can read `~/.config/ghattach/cookies.json` can act as you on GitHub until you revoke the session.
- Without a pin, an expired session on macOS is re-read from the freshest Chrome profile, maybe another account.
- Only `login --from-chrome --chrome-profile <profile>` pins an account, so a `--cookie` login can switch too.
- `--chrome-profile` is a file-name pattern. `Profile *` reads every matching profile and is stored as the pin.

**Fails when it need not:**

- Outside macOS, Chrome cannot be re-read, so an expired session fails the upload with that error.
- `--from-chrome` refuses a profile on Chrome's app-bound `v20` encryption, with an error that points at `--cookie -`.
- **Declined:** a cookie stored by older Chrome versions without the host-hash prefix fails with the wrong-key error.
- **Declined:** on macOS, a terminal paste of 1,024 bytes or more never arrives, so pipe it as [`ghattach login`](reference.md#ghattach-login) shows.

## Uploads

**Can expose or misdirect:**

- An upload to a public repository is readable by anyone who has the link. ghattach cannot delete it.
- A file only GitHub refuses stops the run, leaving earlier files uploaded, as [examples/handle-failures](../examples/handle-failures/) shows.

**Fails when it need not:**

- **GitHub can change the upload endpoint at any time.** Do not let a failed upload fail a build.
- A file under 100 MiB but over a GitHub limit fails with GitHub's error, not ghattach's own check.

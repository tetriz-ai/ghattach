# Install and log in

ghattach is tested on Linux and macOS. Windows binaries are published but not tested.

## What you supply

| You supply | What it is for |
|---|---|
| Go 1.26.10 or later | `go install` builds ghattach. The release archives need no Go. |
| A browser signed in to github.com | ghattach uploads with that session's `user_session` cookie. |
| Write access to the target repository | GitHub does not document the minimum. ghattach is tested with write access. |
| The [GitHub CLI](https://cli.github.com), signed in with `gh auth login` | It looks up the repository id. Without it, pass `--repo-id`, as in the [README](../README.md#upload-without-the-github-cli). |

Nothing else needs installing. On macOS, `login --from-chrome` also uses the system's own
`security` and `sqlite3` commands.

## Build from a clone

Run `go install ./cmd/ghattach` in a clone to build that code. Like `go install` with a module
path, it puts `ghattach` in `$(go env GOPATH)/bin`, or in `GOBIN` if you set it.

## Install from a release archive

Use this route without Go, such as on a CI runner.

1. Download `ghattach_<version>_<os>_<arch>.tar.gz` and `checksums.txt` from the [Releases page](https://github.com/tetriz-ai/ghattach/releases).

   `<os>` is `linux` or `darwin`, and `<version>` has no leading `v`. Windows gets a `.zip`.

2. Check the archive against `checksums.txt` in the same folder.

   ```sh
   sha256sum --ignore-missing -c checksums.txt          # Linux
   shasum -a 256 --ignore-missing -c checksums.txt      # macOS
   ```

3. Extract the archive and put the `ghattach` binary on your `PATH`.
4. On macOS, clear the quarantine flag if a browser downloaded the archive, since the binary is not notarized.

   ```sh
   xattr -d com.apple.quarantine ghattach
   ```

   A curl download sets no flag, so it skips this step.

### Verify build provenance

The public release workflow keeps a release in draft until GitHub has attested the archives
listed in `checksums.txt`. It refuses to update a release that is already published; draft
retries remain allowed. For releases produced by that workflow, use the GitHub CLI to verify
a downloaded archive before extracting it:

```sh
gh attestation verify ghattach_0.1.0_darwin_arm64.tar.gz \
  --repo tetriz-ai/ghattach \
  --signer-workflow tetriz-ai/ghattach/.github/workflows/release.yml
```

Substitute the version and platform you downloaded. Verification requires GitHub access and
fails if no matching attestation exists; older releases may have none. It checks the archive's
digest and the repository/workflow that attested it. It does not notarize the macOS binary.

## Log in with the cookie value

On Linux, or when your session is in another browser, copy the cookie value instead of using
`--from-chrome`.

1. Open github.com in a browser tab that is signed in.
2. Open DevTools with F12, or Cmd+Option+I on macOS.
3. Go to Application, then Cookies, then `https://github.com`. In Firefox the tab is Storage,
   not Application.
4. Copy only the value of the `user_session` cookie.
5. Run `ghattach login --cookie -`, paste the value at the prompt and press Enter. Nothing is
   shown as you paste.

   ```console
   $ ghattach login --cookie -
   Paste the user_session VALUE, then Enter:
   Session stored at /home/you/.config/ghattach/cookies.json
   Verify with: ghattach upload <file> --repo OWNER/REPO
   ```

`login` checks only that the value can be sent as a cookie. The first upload checks it with
GitHub, and a wrong or expired value fails there with `expired session`.

To log in from a script or CI job, pipe the value instead, as the
[README](../README.md#log-in-from-a-script-or-ci-job) shows.

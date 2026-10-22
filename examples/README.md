# Examples

| You want | Example |
|---|---|
| To log in and embed one screenshot, as the Quick start does | [embed-one-screenshot](embed-one-screenshot/) |
| To embed a video or a log file next to a screenshot | [video-and-files](video-and-files/) |
| To keep the uploads that finished when GitHub refuses a later file | [handle-failures](handle-failures/) |
| To embed screenshots from a CI job that has no GitHub CLI | [ci-comment](ci-comment/) |

Run them all from the repository root with `scripts/run-examples.sh`. It checks each script
with shellcheck, runs it, and compares its output with the last block in that example's
README. Each example needs Go and curl.

Each script runs ghattach against the local stub of GitHub in
`internal/demostub`, with an empty temporary home directory. None of them needs a
GitHub account or a cookie, and none of them talks to GitHub. The output shown in each README
writes the temporary home as `/home/you` and each asset id as `<uuid>`.

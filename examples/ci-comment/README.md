This is the upload step of a CI job on a runner with no signed-in GitHub CLI. The session
comes from a stored secret, and `--repo-id` replaces the `gh` lookup. The embeds go into
`comment.md` for a later step to post, which the example leaves out because posting needs a
real pull request.

```sh
examples/ci-comment/run.sh
```

```text
Screenshots from this build:

![before.png](https://github.com/user-attachments/assets/<uuid>)
![after.png](https://github.com/user-attachments/assets/<uuid>)
```

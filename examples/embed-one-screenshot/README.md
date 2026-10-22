This is the README's quick start, run against a local stub of GitHub. It logs in with a
made-up session value read from standard input instead of Chrome, then uploads one screenshot and prints the markdown
to paste into a pull request. Start here to see what ghattach prints before you log in with
your own session.

```sh
examples/embed-one-screenshot/run.sh
```

```text
Session stored at /home/you/.config/ghattach/cookies.json
Verify with: ghattach upload <file> --repo OWNER/REPO
![shot.png](https://github.com/user-attachments/assets/<uuid>)
```

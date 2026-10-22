Use this when a pull request needs a recording or a log beside a screenshot. It uploads all
three in one run, and `--markdown` prints a different embed for each media type, one line per
file in the order given. The [upload reference](../../docs/reference.md#ghattach-upload-file) lists
which embed each media type gets.

```sh
examples/video-and-files/run.sh
```

```text
![shot.png](https://github.com/user-attachments/assets/<uuid>)
https://github.com/user-attachments/assets/<uuid>
[build.log](https://github.com/user-attachments/assets/<uuid>)
```

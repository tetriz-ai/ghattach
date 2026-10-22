ghattach checks every file before the first upload, so a missing, empty or oversized file fails
the run with nothing uploaded. A refusal only GitHub can make, such as its lower limit for
images, stops the run at that file and exits with status 1. The files uploaded before it are
real uploads that ghattach cannot delete, so a script should keep their embeds. This one
uploads `before.png` and an 11 MiB `after.png` that the stub refuses, then prints the error and
the embed it keeps.

```sh
examples/handle-failures/run.sh
```

```text
ghattach exited with status 1.
Error: uploading after.png: request upload policy: policy endpoint returned 422 (want 201): stub: an image must be at most 10 MiB
Uploaded before the failure:
![before.png](https://github.com/user-attachments/assets/<uuid>)
```

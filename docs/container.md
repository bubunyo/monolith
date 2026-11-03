# Containers

`monolith container` runs OCI container images, each in its own small VM,
in the style of Apple's `container` tool. There's no Docker engine and no
shared Linux VM: a container is a VM that boots Kata's Linux kernel with
monolith's own init as PID 1 and the image's filesystem as its root disk.

```bash
monolith container run --rm alpine echo hello      # about 0.7 s, start to exit
monolith container run -it --rm alpine sh          # interactive shell
monolith container run -d --name web nginx         # in the background
monolith container logs -f web
monolith container stop web
monolith container rm web
```

`monolith c` is short for `monolith container`.

## How it works

1. **Images** are pulled from their registry (Docker Hub by default) for
   linux/arm64 and converted once into an ext4 disk image under
   `~/.cache/monolith/images`. Registry logins come from
   `~/.docker/config.json` and its credential helpers, as with `docker`.
   Layers download 8 at a time and are kept, so images that share a base
   (or a re-pull) reuse them: the output says `Already exists (<image>)`.
   On a terminal the progress is shown per layer and updated in place, in
   colour (set `NO_COLOR` to turn colour off).
2. **A container's disk** is an APFS clone of the image's disk: instant, and
   it only takes space for what the container writes. It's `--disk` big
   (default 8G, image included); the filesystem grows to fill it at boot.
   Writes persist across `stop` and `start`; `rm` throws them away.
3. **The VM** boots Kata's kernel (downloaded once, shared with `--image
   kata` VMs) and `monolith-init`, which mounts the disk, sets up `/dev`,
   `/proc` and networking, and runs the container's process the way Docker
   would: the image's entrypoint, cmd, environment, user and working
   directory, with your overrides.
4. **When the process exits**, its exit code reaches the Mac and the VM
   powers off. `run` in the foreground exits with that code.

Each container has its own kernel, so a process that escapes its
container still only has a VM with nothing in it but itself.

## Commands

| Command | |
|---|---|
| `run [flags] IMAGE [COMMAND...]` | Create and start a container. `-d` background, `-it` interactive terminal, `--rm` remove on exit, `--name`, `-e KEY=VALUE` / `-e KEY` (copied from your shell), `--env-file`, `-w DIR`, `-u USER[:GROUP]`, `--entrypoint`, `--cpus` (1), `-m/--memory` (512M), `--disk` (8G: the container's disk, image included; only what's written takes space on the Mac, and it can't be smaller than the image's filesystem, at least 1G), `--pull missing\|always\|never`. Flags after IMAGE belong to the command. |
| `ps [-a] [-q]` | Running containers; `-a` includes stopped ones. |
| `logs [-f] [-n N] NAME` | The container's output (stdout and stderr together). |
| `stop [-t 10] NAME...` | Send the image's stop signal (`STOPSIGNAL`, default SIGTERM), then SIGKILL after `-t` seconds. |
| `start [-a] NAME...` | Run a stopped container again with its disk as it was. `-a` attaches and exits with its code. |
| `attach NAME` | Attach to a running container. |
| `rm [-f] NAME...` | Remove stopped containers; `-f` kills running ones first. |
| `prune [-f]` | Remove all stopped containers, after listing them with the space each gives back (what it wrote on top of its image). |
| `pull IMAGE`, `images`, `rmi [-f] IMAGE...` | Manage local images. |
| `image prune [-a] [-f]` | Remove untagged images; with `-a`, every image no container was created from. Cached layers no image needs go too. |
| `image inspect [--json] IMAGE` | What the image runs (entrypoint, cmd, env, workdir, user, stop signal), its ports, volumes, labels, layers and sizes. |
| `image history [--no-trunc] IMAGE` | The build steps (Dockerfile lines) that made the image, newest first, with each layer's size. |
| `image ls [-R] IMAGE [PATH]` | List files in the image, like `ls -l` (default `/`). |
| `image cat IMAGE PATH` | Print a file from the image. |
| `image cp IMAGE:PATH DEST [-f]` | Copy a file or directory out of the image (`nginx:1.29:/etc/nginx`). |
| `image sh IMAGE [-c COMMANDS]` | A shell for exploring the image and each of its layers (see below). |

## Looking inside an image without running it

The `image inspect`, `history`, `ls`, `cat` and `cp` commands read the image's
disk on the Mac. Nothing from the image runs and no VM starts, so they work on
images without a shell (distroless, `FROM scratch`), and images that aren't
local are pulled first.

```bash
monolith c image inspect nginx                      # what it runs, its settings, layers
monolith c image history nginx                      # how it was built
monolith c image ls gcr.io/distroless/static /etc   # files, like ls -l
monolith c image cat alpine /etc/os-release
monolith c image cp nginx:/etc/nginx ./nginx-conf   # copy out to the Mac
```

### Exploring layers: `image sh`

`monolith c image sh IMAGE` opens monolith's own shell over the image (again,
nothing from the image runs). Besides browsing (`ls`, `cd`, `tree`, `find`,
`cat`, `head`, `less`, `stat`, `du`, `cp`), it knows the layers:

```
nginx:1.29:/$ layers
#  LAYER         SIZE    ADDED  CHANGED  DELETED  STEP
0  9ebf9a1d0c9c  30.1MB  3256   0        0        # debian.sh --arch 'arm64' …
1  0cbb36225b64  31.1MB  1736   20       0        RUN /bin/sh -c set -x && groupadd …
2  723390212024  627B    1      0        0        COPY docker-entrypoint.sh /
nginx:1.29:/$ blame /etc/passwd
layer 0 (9ebf9a1d0c9c) added it: # debian.sh --arch 'arm64' …
layer 1 (0cbb36225b64) changed it: RUN /bin/sh -c set -x && groupadd …
nginx:1.29:/$ layer 2                  # browse only what layer 2 contains
nginx:1.29[layer 2]:/$ changes        # what it added (A), changed (M), deleted (D)
nginx:1.29[layer 2]:/$ merged         # back to the whole image
```

It has line editing, history and Tab completion. Commands can be joined with
`;` and `&&` (no pipes or redirection); `-c` runs commands without a prompt,
and its output can be piped on the Mac:
`monolith c image sh nginx -c "find / -name '*.conf'" | grep ssl`. Images
pulled before monolith kept layers fetch them the first time they're needed.

They treat the image as untrusted: it's opened read-only; symlinks are resolved
inside the image and can't point out of it; `cp` drops setuid, setgid and
sticky bits, keeps symlinks inside copied folders as links, never writes through
a symlink on the Mac, and doesn't replace existing files without `-f`.

With `-it`, every key goes to the container, Ctrl+C included. **Ctrl+P
Ctrl+Q** detaches and leaves it running. Without `-t`, Ctrl+C is delivered to
the container's process as SIGINT.

Containers are VMs, so `monolith vm list` shows them (IMAGE `container
(<image>)`), and `monolith vm pause|resume` work on them.

## Not yet

These are planned:

- `exec`, publishing ports (`-p`), volumes (`-v`)
- `-i` without `-t` (piping stdin into a container)
- Suspending a container to disk (`vm suspend`); `pause` works
- linux/amd64 images through Rosetta
- Network policy (`--network none`), named networks between containers
- `build`

## Differences from Docker

- Each container has its own kernel (Kata's guest kernel, Linux 6.18), its
  own memory limit (`-m`, default 512M), vCPUs (`--cpus`, default 1) and
  disk (`--disk`, default 8G). A full disk fails writes in that container
  only.
- `/dev` is the kernel's devtmpfs: the container sees its VM's devices.
- Extended attributes in images are dropped when converting, so binaries
  that rely on file capabilities (for example `ping` as a non-root user)
  lose them.
- The terminal is `/dev/hvc0` (the VM's console) rather than `/dev/pts/0`.
- The open-file limit is 1048576.

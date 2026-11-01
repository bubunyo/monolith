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
2. **A container's disk** is an APFS clone of the image's disk: instant, and
   it only takes space for what the container writes. Writes persist across
   `stop` and `start`; `rm` throws them away.
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
| `run [flags] IMAGE [COMMAND...]` | Create and start a container. `-d` background, `-it` interactive terminal, `--rm` remove on exit, `--name`, `-e KEY=VALUE` / `-e KEY` (copied from your shell), `--env-file`, `-w DIR`, `-u USER[:GROUP]`, `--entrypoint`, `--cpus` (1), `-m/--memory` (512M), `--pull missing\|always\|never`. Flags after IMAGE belong to the command. |
| `ps [-a] [-q]` | Running containers; `-a` includes stopped ones. |
| `logs [-f] [-n N] NAME` | The container's output (stdout and stderr together). |
| `stop [-t 10] NAME...` | Send the image's stop signal (`STOPSIGNAL`, default SIGTERM), then SIGKILL after `-t` seconds. |
| `start [-a] NAME...` | Run a stopped container again with its disk as it was. `-a` attaches and exits with its code. |
| `attach NAME` | Attach to a running container. |
| `rm [-f] NAME...` | Remove stopped containers; `-f` kills running ones first. |
| `pull IMAGE`, `images`, `rmi [-f] IMAGE...` | Manage local images. |

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
  own memory limit (`-m`, default 512M) and vCPUs (`--cpus`, default 1).
- `/dev` is the kernel's devtmpfs: the container sees its VM's devices.
- Extended attributes in images are dropped when converting, so binaries
  that rely on file capabilities (for example `ping` as a non-root user)
  lose them.
- The terminal is `/dev/hvc0` (the VM's console) rather than `/dev/pts/0`.
- The open-file limit is 1048576.

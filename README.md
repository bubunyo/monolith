# monolith

[![CI](https://github.com/bubunyo/monolith/actions/workflows/ci.yml/badge.svg)](https://github.com/bubunyo/monolith/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/bubunyo/monolith?include_prereleases)](https://github.com/bubunyo/monolith/releases)

Lightweight Linux VMs on macOS (Apple Silicon), built on Apple's
Virtualization.framework: plain VMs, Colima-style Docker, containerd and
Kubernetes hosts, sandboxes for code you don't trust, and its own container
engine that runs each container in its own VM.

- `monolith vm`: Kata (default) or Alpine VMs that boot in seconds, with
  pause, suspend-to-disk and several network modes.
- `monolith docker`: Docker Engine or containerd hosts for the CLI on your Mac,
  with docker contexts, localhost port forwarding (TCP and UDP), `/Users`
  bind mounts with file-change events, pause/resume to disk, optional Rosetta
  for amd64 images, and optional Kubernetes (k3s).
- `monolith sandbox`: run a repo you don't trust in a lith, like a virtualenv:
  only the repo is visible (read-only, changes reviewed with `diff`/`apply`),
  and the code can reach the internet but not your Mac or local network.
- `monolith container`: monolith's own container engine, in the style of
  Apple's `container` tool. It pulls OCI images from any registry and runs
  each container in its own small VM (Kata's kernel, monolith's own init), so
  every container has its own kernel. Docker-style commands (`run -it/-d`,
  `ps`, `logs`, `stop`, `start`, `rm`, `prune`, `images`), image inspection
  without running it (`image inspect`, `history`, `ls`, `cat`, `cp`, and
  `image sh`, a shell for exploring each layer, even for images without a
  shell), about 0.6s from
  `run` to exit, per-container `--cpus`/`--memory`/`--disk`, and live,
  coloured pull progress with a shared layer cache. Not yet: `exec`, `-p`
  and `-v` ([docs/container.md](docs/container.md)).
- `monolith network`: named networks shared between VMs.

### Which one?

| You want | Use |
|---|---|
| A Linux machine to work in | `monolith vm` |
| The Docker ecosystem: the `docker` CLI, Compose, buildx, Kubernetes | `monolith docker` |
| Quick runs of single images, each container isolated by its own VM | `monolith container` |
| A safe place to try a repo you don't trust | `monolith sandbox` |

`monolith container` is a separate engine, not a Docker runtime: it runs
standard OCI images with `docker run` semantics, but Docker tools can't talk to
it. For those, use `monolith docker`.

## Install

Requires macOS on Apple Silicon:

```bash
curl -fsSL https://raw.githubusercontent.com/bubunyo/monolith/master/install.sh | bash
```

The script downloads the latest release, checks it against the release's
SHA-256 checksums and its code signature, and installs `monolith` to
`/usr/local/bin` (using sudo if needed). Options, as environment variables
for the `bash` side of the pipe:

```bash
curl -fsSL https://raw.githubusercontent.com/bubunyo/monolith/master/install.sh | MONOLITH_VERSION=v0.1.0 MONOLITH_INSTALL_DIR=~/.local/bin bash
```

To install by hand, download `monolith_<version>_darwin_arm64.tar.gz` and
`checksums.txt` from [Releases](https://github.com/bubunyo/monolith/releases),
run `shasum -a 256 -c checksums.txt`, and copy the binary onto your PATH. If
macOS then refuses to run it ("cannot be opened because the developer cannot
be verified"), the release wasn't notarized and a browser added the quarantine
flag: `xattr -d com.apple.quarantine /path/to/monolith`.

### Build from source

Needs Go (see `go.mod`) and Xcode command line tools:

```bash
make build    # ./bin/monolith, codesigned with entitlements.plist
make test     # unit tests
```

`make build` signs ad-hoc unless `.codesign_identity` names a certificate
(see [docs/codesigning.md](docs/codesigning.md)). It also cross-compiles
`monolith-init`, the PID 1 of container VMs, and embeds it. Don't use a bare
`go build`: without the virtualization entitlement the binary is killed on
launch, and without the init `monolith container` can't run anything.

## Quick start

```bash
monolith vm run dev                 # boot a VM and attach to its console (Ctrl+] then q to detach)
monolith docker start               # a Docker host; the docker CLI's context switches to it
docker run --rm -it busybox sh
monolith docker start --name k8s --kubernetes
kubectl get nodes

monolith container run --rm alpine echo hello      # a container in its own VM
monolith container run -it --rm alpine sh          # interactive (Ctrl+P Ctrl+Q detaches)
monolith container run -d --name db -e POSTGRES_PASSWORD=secret postgres
monolith container logs -f db

cd ~/Downloads/some-repo && monolith sandbox activate   # a shell in a sandbox for this repo
```

Flags for `container run` go before the image; anything after it is the
command to run in the container, as with `docker run`.

## Documentation

- [docs/vm.md](docs/vm.md): VMs, Docker hosts, containerd, Kubernetes
- [docs/container.md](docs/container.md): containers, each in its own VM
- [docs/sandbox.md](docs/sandbox.md): sandboxes for untrusted repos
- [docs/networking.md](docs/networking.md): network modes and named networks
- [docs/codesigning.md](docs/codesigning.md): signing local builds
- [docs/releasing.md](docs/releasing.md): CI, releases, signing and notarization
- [AGENTS.md](AGENTS.md): architecture and internals

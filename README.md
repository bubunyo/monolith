# monolith

[![CI](https://github.com/bubunyo/monolith/actions/workflows/ci.yml/badge.svg)](https://github.com/bubunyo/monolith/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/bubunyo/monolith?include_prereleases)](https://github.com/bubunyo/monolith/releases)

Lightweight Linux VMs on macOS (Apple Silicon), built on Apple's
Virtualization.framework, plus Colima-style Docker, containerd and Kubernetes
hosts.

- `monolith vm`: Kata (default) or Alpine VMs that boot in seconds, with
  pause, suspend-to-disk and several network modes.
- `monolith docker`: Docker Engine or containerd hosts for the CLI on your Mac,
  with docker contexts, localhost port forwarding (TCP and UDP), `/Users`
  bind mounts with file-change events, pause/resume to disk, optional Rosetta
  for amd64 images, and optional Kubernetes (k3s).
- `monolith network`: named networks shared between VMs.

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
(see [docs/codesigning.md](docs/codesigning.md)). Don't use a bare `go build`:
without the virtualization entitlement the binary is killed on launch.

## Quick start

```bash
monolith vm run dev                 # boot a VM and attach to its console (Ctrl+] then q to detach)
monolith docker start               # a Docker host; the docker CLI's context switches to it
docker run --rm -it busybox sh
monolith docker start --name k8s --kubernetes
kubectl get nodes
```

## Documentation

- [docs/vm.md](docs/vm.md): VMs, Docker hosts, containerd, Kubernetes
- [docs/networking.md](docs/networking.md): network modes and named networks
- [docs/codesigning.md](docs/codesigning.md): signing local builds
- [docs/releasing.md](docs/releasing.md): CI, releases, signing and notarization
- [AGENTS.md](AGENTS.md): architecture and internals

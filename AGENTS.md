# AGENTS.md

## Build & Run

```bash
make build   # builds to ./bin/vm AND codesigns with entitlements.plist
make run     # runs ./bin/vm
```

`go build` alone is not enough. The binary **must** be codesigned with `entitlements.plist` (grants `com.apple.security.virtualization`) or it will crash at runtime. Always use `make build`.

## Tests

```bash
go test ./...             # all tests
go test -run TestRun ./   # single test
```

**Tests boot a real VM.** They require macOS on Apple Silicon with Virtualization.framework. There is no mock/unit-only test path. Do not expect tests to pass in CI or on non-macOS hosts.

## Platform Constraint

macOS + aarch64 only. The entire codebase depends on Apple Virtualization.framework via `github.com/Code-Hex/vz/v3`. No cross-platform build targets exist.

## Package Layout

Single Go package `monolith` with one CLI entry point:

- `runtime.go` — `Run()` orchestrates VM boot (boot loader, vCPUs, RAM, network, serial console, virtio entropy)
- `kernel.go` — `DefaultKernel()` downloads Alpine aarch64 ISO, extracts/decompresses kernel+initrd to `~/.cache/monlith/`. `DefaultRootFS()` downloads the Alpine minirootfs tarball.
- `storage.go` — `DefaultDisk()` creates a 1 GiB ext4 disk image via go-diskfs and populates it with the Alpine minirootfs. Writes a custom `/etc/inittab` for hvc0 console.
- `cmd/vm/main.go` — CLI entry point, calls `monolith.Run(".")`

## Boot Flow

1. `DefaultKernel()` — download Alpine ISO, extract `vmlinuz-virt` and `initramfs-virt`, decompress the EFI-stub kernel to raw ARM64 Image
2. `DefaultRootFS()` — download `alpine-minirootfs-3.21.6-aarch64.tar.gz`
3. `DefaultDisk()` — create 1 GiB sparse file, format ext4 (4096-byte blocks via go-diskfs), extract minirootfs tarball into it, write custom `/etc/inittab`
4. Boot VM with kernel args `console=hvc0 root=/dev/vda rootfstype=ext4 rw` using stock Alpine initramfs
5. Alpine Init loads virtio drivers, mounts `/dev/vda` as root, runs `switch_root`
6. Busybox init reads `/etc/inittab`, remounts root rw, spawns `getty` on `hvc0`

## Gotchas

- First run downloads ~60MB Alpine ISO + ~4MB minirootfs from `dl-cdn.alpinelinux.org`; subsequent runs use cached assets.
- Cached assets: `~/.cache/monlith/{vmlinuz-virt,initramfs-virt,minirootfs.tar.gz,disk.img}`. Delete this directory to force re-download.
- The cache directory is `monlith` (typo of "monolith") — intentionally kept for consistency.
- Delete just `disk.img` to regenerate the disk without re-downloading.
- `vmlinuz-virt` is an EFI stub — `decompressKernel()` in `kernel.go` extracts the gzip-compressed raw ARM64 Image that `VZLinuxBootLoader` requires.
- go-diskfs `recalculateBlocksize()` always picks 1024-byte blocks regardless of disk size. We call `ext4.Create()` directly with `SectorsPerBlock: 8` (4096-byte blocks) to avoid a journal allocation bug.
- go-diskfs ext4 does not support hardlinks (`Link()` returns `ErrNotImplemented`). Tar hardlinks are handled by copying the target file's content.
- Networking (`udhcpc`) currently fails with `AF_PACKET` not supported — the virt kernel may need the `af_packet` module loaded.
- No linter, formatter, or CI config exists. No pre-commit hooks.

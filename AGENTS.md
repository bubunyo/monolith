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

- `runtime.go` — `Run()` orchestrates VM boot (boot loader, vCPUs, RAM, network, serial console)
- `kernel.go` — `DefaultKernel()` downloads Alpine aarch64 ISO, extracts kernel/initrd to `~/.cache/myruntime/`
- `storage.go` — `DefaultDisk()` creates a sparse disk via `diskutil` (currently commented out in `runtime.go`)
- `cmd/vm/main.go` — CLI entry point, calls `monolith.Run(".")`

## Gotchas

- Disk storage setup is **commented out** in `runtime.go:58-72`. The VM boots without persistent storage.
- First run downloads ~60MB Alpine ISO from `dl-cdn.alpinelinux.org`; subsequent runs use cached kernel/initrd.
- Cached assets: `~/.cache/myruntime/{vmlinuz-virt,initramfs-virt}`. Delete this directory to force re-download.
- `storage.go` shells out to `diskutil` — only available on macOS.
- No linter, formatter, or CI config exists. No pre-commit hooks.

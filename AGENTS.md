# AGENTS.md

## Build & Run

```bash
make build   # builds to ./bin/monolith AND codesigns with entitlements.plist
make run     # builds and runs: monolith vm run (auto-generated name)
```

`go build` alone is not enough. The binary **must** be codesigned with `entitlements.plist` (grants `com.apple.security.virtualization` and `com.apple.vm.networking`) or it will crash at runtime. Always use `make build`. Set `CODESIGN_IDENTITY` env var for Developer ID signing (defaults to `-` for ad-hoc).

## CLI

```bash
monolith vm run [name]                   # boot a VM (creates on first run, reuses if exists)
monolith vm run [name] --network shared  # boot with shared vmnet networking (macOS 26+)
monolith vm run [name] --network host --host-interface en0  # bridge to physical NIC
monolith vm run [name] --network isolated  # isolated network (macOS 26+)
monolith vm run [name] --network none    # no network device (air-gapped)
monolith vm list          # list all VMs (name, state, ip, net, cpus, mem, disk, uptime)
monolith vm rm <name>     # delete a VM (prompts for confirmation)
monolith vm top <name>    # live TUI dashboard for a running VM
```

If no name is given to `run`, one is auto-generated as `lith-NNNNN` (random 5-digit number). VM names must be lowercase alphanumeric with hyphens, max 63 characters (valid hostnames).

## Tests

```bash
go test ./...             # all tests
go test -run TestRun ./   # single test
```

**Tests boot a real VM.** They require macOS on Apple Silicon with Virtualization.framework. There is no mock/unit-only test path. Do not expect tests to pass in CI or on non-macOS hosts.

## Platform Constraint

macOS + aarch64 only. The entire codebase depends on Apple Virtualization.framework via `github.com/Code-Hex/vz/v3` (currently using norio-nomura's fork with `VZVmnetNetworkDeviceAttachment` support for macOS 26+, via `replace` directive in go.mod). No cross-platform build targets exist.

## Package Layout

```
monolith/
  cmd/monolith/main.go   — CLI entry point, calls cli.Execute()
  cli/
    root.go               — root cobra command ("monolith")
    vm/
      vm.go               — "vm" parent command
      run.go              — "vm run [name]" with name generation and validation
      list.go             — "vm list" reads vms/ directory, prints table
      rm.go               — "vm rm <name>" with confirmation prompt
  runtime.go              — Run(name) orchestrates VM boot (boot loader, vCPUs, RAM, network, serial, entropy)
  kernel.go               — DefaultKernel() downloads Alpine ISO, extracts/decompresses kernel+initrd. DefaultRootFS() downloads minirootfs tarball. Shared assets cached under ~/.cache/monlith/alpine/.
  storage.go              — DefaultDisk(tarball, name) creates per-VM 1 GiB ext4 disk image under ~/.cache/monlith/vms/<name>/. Populates with Alpine minirootfs, configures user accounts, autologin, MOTD, hostname.
  config.go               — VMConfig with CPUs, Memory, Network, HostInterface. Per-VM config at vms/<name>/config.json.
  state.go                — VMState/VMStatus, IP discovery (DHCP leases + ARP fallback), per-VM network stats via vmenet.
  runtime_test.go         — test that boots a real VM
  entitlements.plist      — codesigning entitlements (com.apple.security.virtualization, com.apple.vm.networking)
```

## Network Modes

Five network modes are supported via the `--network` flag on `vm run`:

| Mode | Flag | Implementation | Requires |
|------|------|---------------|----------|
| `nat` (default) | `--network nat` | `VZNATNetworkDeviceAttachment` | macOS 11+ |
| `shared` | `--network shared` | `vmnet.SharedMode` + `VZVmnetNetworkDeviceAttachment` | macOS 26+ |
| `host` | `--network host --host-interface en0` | `VZBridgedNetworkDeviceAttachment` | `com.apple.vm.networking` entitlement |
| `isolated` | `--network isolated` | `vmnet.HostMode` + `VZVmnetNetworkDeviceAttachment` | macOS 26+ |
| `none` | `--network none` | No network device | - |

- **nat**: Standard NAT networking. VM gets IP from macOS DHCP server. No inter-VM communication.
- **shared**: macOS 26 vmnet SharedMode. NAT + inter-VM communication (when sharing the same `vmnet.Network` instance). Configurable subnets, DHCP reservations, port forwarding.
- **host**: Bridged directly to a physical NIC (e.g. en0). VM gets IP from the router. Per-VM network stats not available.
- **isolated**: macOS 26 vmnet HostMode. VM can talk to host only, no NAT, no inter-VM communication.
- **none**: Air-gapped. No network device attached to the VM.

The network mode is persisted in `config.json` and reused on subsequent boots.

## Boot Flow

1. `DefaultKernel()` — download Alpine ISO, extract `vmlinuz-virt` and `initramfs-virt`, decompress the EFI-stub kernel to raw ARM64 Image
2. `DefaultRootFS()` — download `alpine-minirootfs-3.21.6-aarch64.tar.gz`
3. `DefaultDisk(tarball, name)` — create 1 GiB sparse file at `vms/<name>/disk.img`, format ext4 (4096-byte blocks via go-diskfs), extract minirootfs tarball, write VM config files (inittab, passwd, shadow, group, motd, hostname, autologin, sudo, .profile)
4. Boot VM with kernel args `console=hvc0 root=/dev/vda rootfstype=ext4 rw` using stock Alpine initramfs
5. Alpine Init loads virtio drivers, mounts `/dev/vda` as root, runs `switch_root`
6. Busybox init reads `/etc/inittab`, remounts root rw, auto-logs in as user `mono`

## VM Configuration

- **User**: `mono` (uid 1000, gid 1000), member of `wheel` group
- **Autologin**: `getty -n -l /usr/local/bin/autologin` on hvc0 — no login prompt
- **sudo**: `/usr/local/bin/sudo` wrapper script using `su`. `sudo <cmd>` runs as root, `sudo -i` opens root shell.
- **Hostname**: set to the VM name (both `/etc/hostname` and `::sysinit:/bin/hostname <name>` in inittab)
- **MOTD**: shows VM name, Alpine version, user info, root access, disk info
- **Root password**: empty (allows `su` without password prompt)

## Cache Directory

```
~/.cache/monlith/
  alpine/                  — shared Alpine assets (kernel, initrd, minirootfs)
    vmlinuz-virt
    initramfs-virt
    minirootfs.tar.gz
  vms/                     — per-VM directories
    my-dev-box/
      disk.img
    lith-48271/
      disk.img
```

- The cache directory is `monlith` (typo of "monolith") — intentionally kept for consistency.
- Delete `~/.cache/monlith/alpine/` to force re-download of shared assets.
- Delete `~/.cache/monlith/vms/<name>/disk.img` to regenerate a VM's disk without re-downloading.
- Delete `~/.cache/monlith/vms/<name>/` entirely to remove a VM (or use `monolith vm rm <name>`).

## Gotchas

- First run downloads ~60MB Alpine ISO + ~4MB minirootfs from `dl-cdn.alpinelinux.org`; subsequent runs use cached assets.
- `vmlinuz-virt` is an EFI stub — `decompressKernel()` in `kernel.go` extracts the gzip-compressed raw ARM64 Image that `VZLinuxBootLoader` requires.
- go-diskfs `recalculateBlocksize()` always picks 1024-byte blocks regardless of disk size. We call `ext4.Create()` directly with `SectorsPerBlock: 8` (4096-byte blocks) to avoid a journal allocation bug.
- go-diskfs ext4 does not support hardlinks (`Link()` returns `ErrNotImplemented`). Tar hardlinks are handled by copying the target file's content.
- go-diskfs `OpenFile` with `O_CREATE|O_RDWR` does not truncate existing files. `configureVM` calls `Remove()` before writing config files that may already exist from the tarball extraction.
- Networking (`udhcpc`) currently fails with `AF_PACKET` not supported — the virt kernel may need the `af_packet` module loaded.
- No linter, formatter, or CI config exists. No pre-commit hooks.

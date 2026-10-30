# AGENTS.md

## Build & Run

```bash
make build   # builds to ./bin/monolith AND codesigns with entitlements.plist
make run     # builds and runs: monolith vm run (auto-generated name)
```

`go build` alone is not enough. The binary **must** be codesigned with `entitlements.plist` (grants `com.apple.security.virtualization`) or it will crash at runtime. Always use `make build`. Set `CODESIGN_IDENTITY` env var to override the signing identity (defaults to ad-hoc signing with `-`). See `docs/codesigning.md` for setup instructions.

## CLI

```bash
# VM lifecycle
monolith vm run [name]                                      # boot a VM (creates on first run, reattaches if running)
monolith vm run [name] --network-mode shared                # boot with shared vmnet networking (macOS 26+)
monolith vm run [name] --network-mode shared --network lan  # join named network "lan"
monolith vm run [name] --network-mode host --host-interface en0  # bridge to physical NIC
monolith vm run [name] --network-mode isolated              # isolated network (macOS 26+)
monolith vm run [name] --network-mode none                  # no network device (air-gapped)
monolith vm attach <name>     # attach to a running VM's serial console (one client at a time)
monolith vm pause <name>      # pause vCPUs in memory
monolith vm resume <name>     # resume from pause or suspend
monolith vm suspend <name>    # snapshot VM state to disk
monolith vm stop <name>       # graceful stop (--force for hard stop)
monolith vm list              # table: name, state, ip, net, cpus, mem, disk, uptime
monolith vm rm <name>         # delete a VM (prompts for confirmation)
monolith vm prune             # remove all stopped VMs
monolith vm top <name>        # bubbletea TUI dashboard for a running VM
monolith vm logs <name>       # stream the daemon log for a VM

# Named networks
monolith network create <name>                    # create + start a named shared network daemon
monolith network create <name> --mode isolated    # create + start an isolated network daemon
monolith network start <name>                     # start the daemon for an existing named network
monolith network stop <name>                      # stop the daemon (network config persists)
monolith network list                             # list networks with attached VMs
monolith network rm <name>                        # stop daemon and delete the named network
monolith network logs <name>                      # stream the daemon log
```

If no name is given to `run`, one is auto-generated as `lith-NNNNN` (random 5-digit number). VM names must be lowercase alphanumeric with hyphens, max 63 characters (valid hostnames). The `network` command is also aliased as `net`.

## Tests

```bash
go test ./...             # all tests
go test -run TestRun ./   # single test
```

**Tests boot a real VM.** They require macOS on Apple Silicon with Virtualization.framework. There is no mock/unit-only test path. Do not expect tests to pass in CI or on non-macOS hosts.

## Platform Constraint

macOS + aarch64 only. The entire codebase depends on Apple Virtualization.framework via `github.com/Code-Hex/vz/v3` (currently using norio-nomura's fork with `VZVmnetNetworkDeviceAttachment` support for macOS 26+, via `replace` directive in go.mod). No cross-platform build targets exist.

## Package Layout

The repo is a single Go module split into a library package (`monolith`, repo root) and a `cli/` package tree. The CLI never touches `vz` directly — it spawns and talks to per-VM daemons over unix sockets.

```
monolith/
  cmd/monolith/main.go   — CLI entry point, calls cli.Execute()
  cli/
    root.go               — root cobra command, registers vm + network subcommands
    netd.go               — hidden "__netd" command, entry point for the network daemon process
    vmd.go                — hidden "__vmd" command, entry point for the per-VM daemon process
    vm/
      vm.go               — "vm" parent command (registers all subcommands)
      run.go              — "vm run [name]" with --cpus, --memory, --network-mode, --network, --host-interface
      attach.go           — "vm attach <name>" attaches to the daemon's console (boot log replayed from console_bus)
      pause/resume/suspend/stop.go — control-plane commands sent over the daemon socket
      list.go             — "vm list" reads vms/ directory, prints table
      rm.go               — "vm rm <name>" with confirmation prompt
      prune.go            — "vm prune" removes all stopped VMs
      logs.go             — "vm logs <name>" tails the daemon log
      top.go / top_model.go — bubbletea TUI dashboard for a running VM
      helpers.go          — shared CLI utilities (name resolution, prompts, formatting)
    network/
      network.go          — "network" parent command (alias "net")
      create.go           — "network create <name>" writes config and starts daemon
      start.go / stop.go  — start/stop the daemon for an existing named network
      list.go             — "network list" shows networks with attached VMs
      rm.go               — "network rm <name>" stops daemon and removes network
      logs.go             — "network logs <name>" tails the daemon log
      topology.go         — topology view helper
      style.go            — shared lipgloss styles for network output
  vmd.go                  — RunDaemon(name, restore) is the per-VM daemon. Owns vz.VirtualMachine,
                            listens on a unix socket, dispatches console-attach and control
                            (pause/resume/suspend/stop/status) requests.
  client.go               — client side of the VM daemon protocol used by the CLI.
  console_bus.go          — drains the console PTY from boot; 64 KiB ring buffer replayed to the attached client.
  protocol.go             — wire types shared by client and daemon.
  network.go              — configureNetwork(), createNetworkAttachment(), joinNamedNetwork()
  netd.go                 — RunNetworkDaemon(), network persistence (config, PID, socket),
                            serialization over unix socket.
  kernel.go               — DefaultKernel() downloads Alpine ISO, extracts/decompresses kernel+initrd.
                            DefaultRootFS() downloads minirootfs tarball.
  storage.go              — DefaultDisk(tarball, name) creates per-VM 1 GiB ext4 disk image and
                            writes Alpine config files.
  config.go               — VMConfig (CPUs, Memory, NetworkMode, NetworkName, HostInterface).
                            Per-VM config at vms/<name>/config.json. Defaults: 1 vCPU, 512 MiB, NAT.
  state.go                — VMState/VMStatus, IP discovery (DHCP leases + ARP fallback),
                            per-VM network stats via vmenet.
  logger.go / logs.go     — daemon log file management.
  entitlements.go         — runtime entitlement checks.
  entitlements.plist      — codesigning entitlements (com.apple.security.virtualization).
  vmd_test.go / vmd_console_log_test.go / network_subnet_test.go — tests (boot real VMs).
  docs/                   — codesigning.md, networking.md, vm.md (deeper references).
```

### Daemon model

Every running VM is a separate `monolith __vmd <name>` process. The `vm run` CLI command spawns this daemon, the daemon owns the `vz.VirtualMachine`, and all subsequent CLI commands (`attach`, `pause`, `resume`, `suspend`, `stop`, `top`, `list` for live status) go through the daemon's unix socket at `~/.cache/monolith/vms/<name>/daemon.sock`. Only one console client may attach at a time; a second gets a NACK (`0xFF`). `console_bus.go` drains the PTY continuously from before boot and replays the last 64 KiB to whoever attaches, so early kernel output is not lost.

Socket protocol (first byte selects the request type, see `protocol.go`):
- `0x01` console attach: daemon ACKs with `0x01`, then raw console bytes flow both ways. Nothing else is ever sent on this stream.
- `0x02` control: 4-byte big-endian length + JSON `ControlRequest` (`pause`, `resume`, `suspend`, `stop`, `status`, `resize`), answered the same way with a `ControlResponse`. `vm attach` sends `resize` (rows/cols) on attach and on `SIGWINCH` over a separate control connection; the daemon applies it to the host PTY with `TIOCSWINSZ`. The guest's `hvc0` does not see host PTY size changes, so the guest still needs `stty rows R cols C` to match. This is also what enables `restore` (resume from a suspended snapshot) on the next `vm run`.

## Network Modes

Five network modes are supported via the `--network-mode` flag on `vm run`:

| Mode | Flag | Implementation | Requires |
|------|------|---------------|----------|
| `nat` (default) | `--network-mode nat` | `VZNATNetworkDeviceAttachment` | macOS 11+ |
| `shared` | `--network-mode shared` | `vmnet.SharedMode` + `VZVmnetNetworkDeviceAttachment` | macOS 26+ |
| `host` | `--network-mode host --host-interface en0` | `VZBridgedNetworkDeviceAttachment` | `com.apple.vm.networking` entitlement (requires provisioning profile or SIP disabled) |
| `isolated` | `--network-mode isolated` | `vmnet.HostMode` + `VZVmnetNetworkDeviceAttachment` | macOS 26+ |
| `none` | `--network-mode none` | No network device | - |

- **nat**: Standard NAT networking. VM gets IP from macOS DHCP server. No inter-VM communication.
- **shared**: macOS 26 vmnet SharedMode. NAT + inter-VM communication (when sharing the same `vmnet.Network` instance). Configurable subnets, DHCP reservations, port forwarding.
- **host**: Bridged directly to a physical NIC (e.g. en0). VM gets IP from the router. Per-VM network stats not available.
- **isolated**: macOS 26 vmnet HostMode. VM can talk to host only, no NAT, no inter-VM communication.
- **none**: Air-gapped. No network device attached to the VM.

The network mode is persisted in `config.json` and reused on subsequent boots.

## Named Networks

Named networks allow multiple VMs to share the same vmnet.Network instance, enabling inter-VM communication for `shared` and `isolated` modes.

### Architecture

Named networks use a **daemon process** to keep the vmnet kernel resource alive:

1. `monolith network create <name>` spawns a background daemon (`monolith __netd <name> <mode>`)
2. The daemon creates a `vmnet.Network`, serializes it via `CopySerialization()`, and listens on a unix domain socket at `~/.cache/monolith/networks/<name>/daemon.sock`
3. When a VM starts with `--network <name>`, it connects to the socket, receives the serialized network bytes, and reconstructs the `vmnet.Network` via `NewNetworkWithSerialization()`
4. The daemon stays alive to keep the vmnet kernel resource open. Stopping the daemon tears down the network.

### Serialization Protocol

Over the unix domain socket, the daemon uses a simple length-prefixed protocol:
- **4 bytes**: big-endian uint32 length of the serialization data
- **N bytes**: raw serialization bytes (extracted from `xpc.Dictionary` via `GetData("networkSerialization")`)

The client reconstructs the XPC dictionary with `xpc.NewDictionary(xpc.KeyValue("networkSerialization", xpc.NewData(bytes)))` and passes it to `vmnet.NewNetworkWithSerialization()`.

### Same-Executable Constraint

The `vmnet_network_create_with_serialization` API requires the deserializing process to be the same executable as the serializing process (checked by CDHash on macOS <26.2, or path on macOS 26.2+). This is why both the daemon and VM processes use the same `monolith` binary.

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
~/.cache/monolith/
  alpine/                  — shared Alpine assets (kernel, initrd, minirootfs)
    vmlinuz-virt
    initramfs-virt
    minirootfs.tar.gz
  vms/                     — per-VM directories
    my-dev-box/
      disk.img
      config.json          # {"cpus":1,"memory":536870912,"network_mode":"nat"}
      state.json           # {"pid":12345,"mac":"...","started_at":..., "status":"running"}
      daemon.sock          # unix domain socket the per-VM daemon listens on
      daemon.log           # daemon stdout/stderr (consumed by `vm logs`)
    lith-48271/
      disk.img
  networks/                — named networks
    my-net/
      config.json          # {"mode":"shared","subnet":"192.168.64.0/24"}
      pid                  # daemon PID
      daemon.sock          # unix domain socket
      daemon.log           # daemon stdout/stderr
```

- Delete `~/.cache/monolith/alpine/` to force re-download of shared assets.
- Delete `~/.cache/monolith/vms/<name>/disk.img` to regenerate a VM's disk without re-downloading.
- Delete `~/.cache/monolith/vms/<name>/` entirely to remove a VM (or use `monolith vm rm <name>`).
- Delete `~/.cache/monolith/networks/<name>/` entirely to remove a network (or use `monolith network rm <name>`).

## Gotchas

- First run downloads ~60MB Alpine ISO + ~4MB minirootfs from `dl-cdn.alpinelinux.org`; subsequent runs use cached assets.
- `vmlinuz-virt` is an EFI stub — `decompressKernel()` in `kernel.go` extracts the gzip-compressed raw ARM64 Image that `VZLinuxBootLoader` requires.
- go-diskfs `recalculateBlocksize()` always picks 1024-byte blocks regardless of disk size. We call `ext4.Create()` directly with `SectorsPerBlock: 8` (4096-byte blocks) to avoid a journal allocation bug.
- go-diskfs ext4 does not support hardlinks (`Link()` returns `ErrNotImplemented`). Tar hardlinks are handled by copying the target file's content.
- go-diskfs `OpenFile` with `O_CREATE|O_RDWR` does not truncate existing files. `configureVM` uses `Truncate()` after writing.
- Named network daemons must use the same binary as VMs (same-executable constraint for vmnet serialization).
- No linter, formatter, or CI config exists. No pre-commit hooks.

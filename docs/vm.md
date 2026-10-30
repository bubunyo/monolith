# Virtual Machines

monolith manages lightweight Linux virtual machines on macOS using
Apple's Virtualization.framework. The guest OS is a pluggable base image:
Kata Containers' guest (default for new VMs), Alpine Linux (`--image alpine`),
or a Docker host built on Alpine (`--image docker`, see [Docker Host](#docker-host)).
See AGENTS.md § Base Images for how each boots. VMs run as background daemon processes,
allowing you to detach, reattach, pause, suspend to disk, and resume without
losing state.

## Quick Start

```bash
# Build (must codesign — see docs/codesigning.md)
make build

# Boot a VM with an auto-generated name
monolith vm run

# Boot a named VM
monolith vm run my-vm

# Detach from console: press Ctrl+C
# Reattach to a running VM
monolith vm attach my-vm

# List all VMs
monolith vm list

# Stop a VM
monolith vm stop my-vm

# Delete a VM and all its data
monolith vm rm my-vm
```

The first VM of each image downloads its assets once: ~65 MB of Kata kernel
and rootfs from `ghcr.io` (cached at `~/.cache/monolith/kata/`), or ~60 MB of
Alpine kernel, initramfs and minirootfs from `dl-cdn.alpinelinux.org`
(cached at `~/.cache/monolith/alpine/`). Subsequent runs reuse the cache.

## VM Naming

VM names must be valid hostnames:

- Lowercase letters, digits, and hyphens only
- Must start and end with a letter or digit
- Maximum 63 characters
- Pattern: `^[a-z0-9][a-z0-9-]*[a-z0-9]$` (single characters also allowed)

If no name is provided to `vm run`, one is auto-generated in the form
`lith-NNNNN` (e.g. `lith-48271`). The generator retries up to 10 times if
the name collides with an existing VM.

The VM name is also used as the guest hostname and appears in the shell
prompt: `mono@my-vm:~$`.

## Commands

### `vm run [name]`

Boot a VM or reattach to an already running one.

```bash
monolith vm run my-vm
monolith vm run my-vm --cpus 2 --memory 1G
monolith vm run my-vm --network-mode shared --network my-net
```

Behavior depends on the VM's current state:

| Current State | Action |
|---------------|--------|
| stopped | Create disk (if needed), spawn daemon, boot VM, attach console |
| running | Reattach to existing console |
| paused | Reattach to existing console |
| suspended | Spawn daemon with `--restore`, resume from suspend file, attach console |

**Flags:**

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--cpus` | `-c` | 1 | Number of vCPUs |
| `--memory` | `-m` | `512M` | RAM size (e.g. `512M`, `1G`, `2048M`) |
| `--network-mode` | | `nat` | Network mode: `nat`, `shared`, `host`, `isolated`, `none`, `user` (`user` for the docker image) |
| `--network` | `-n` | | Named network to join (shared/isolated only) |
| `--host-interface` | | | Physical NIC for host mode (e.g. `en0`) |

Flags are persisted in the VM's `config.json`. On subsequent boots, saved
values are reused unless explicitly overridden with flags.

### `vm attach <name>`

Attach to the serial console of a running or paused VM.

```bash
monolith vm attach my-vm
```

Only one client can be attached at a time. If another client is already
connected, the attach is rejected with an error.

### `vm pause <name>`

Freeze a running VM in memory.

```bash
monolith vm pause my-vm
```

The guest clock stops and no CPU cycles are consumed, but the daemon process
stays alive and RAM is still held. The VM can be resumed instantly with
`vm resume`.

### `vm resume <name>`

Resume a paused or suspended VM.

```bash
monolith vm resume my-vm
```

| Source State | Action |
|-------------|--------|
| paused | Send resume command to daemon; guest clock resumes immediately |
| suspended | Spawn a new daemon with `--restore`; restore VM state from disk file |

### `vm suspend <name>`

Save the VM's full state (CPU registers, memory, device state) to disk and
stop the daemon process.

```bash
monolith vm suspend my-vm
```

The VM is paused, its state is written to `suspend.vmstate`, and the daemon
exits. No host resources are consumed while suspended. Resume later with
`vm resume`.

Requirements and constraints:

- Requires macOS 14+ (uses `SaveMachineStateToPath` / `RestoreMachineStateFromURL`)
- The suspend file is encrypted and tied to the host machine
- Restoring needs the Mac unlocked: the suspend file's key lives in the Secure Enclave and can only decrypt while the user's screen is unlocked. Suspending works while locked
- The disk image must not be modified between suspend and resume
- The VM must support save/restore (validated before saving)

### `vm stop <name>`

Stop a running or paused VM.

```bash
monolith vm stop my-vm          # graceful shutdown
monolith vm stop my-vm --force  # immediate termination
```

Without `--force`, a graceful shutdown request is sent to the guest OS. If
the guest does not respond, monolith falls back to a forceful stop.

| Flag | Short | Description |
|------|-------|-------------|
| `--force` | `-f` | Force stop without graceful shutdown |

### `vm list`

List all VMs with their current status.

```bash
monolith vm list
monolith vm ls              # alias
monolith vm list --wide     # additional columns
```

Output columns:

| Column | Description |
|--------|-------------|
| NAME | VM name |
| STATE | `running`, `paused`, `suspended`, or `stopped` |
| IMAGE | Base image: `kata`, `alpine` or `docker`; docker VMs show the engine in brackets, e.g. `docker (containerd)` |
| IP | Guest IP address (from DHCP leases) |
| NET | Network mode, or `mode:name` for named networks |
| CPUS | Number of vCPUs |
| MEM | Configured RAM |
| DISK | Actual / logical disk usage (sparse-file aware); `-` for kata VMs, which share a read-only base image |
| UPTIME | Time since boot (for running/paused VMs) |

With `--wide` (`-w`), additional columns are shown:

| Column | Description |
|--------|-------------|
| CPU% | Host-side CPU usage of the daemon process |
| RSS | Host-side resident memory of the daemon process |

### `vm top <name>`

Live TUI dashboard for monitoring a running VM.

```bash
monolith vm top my-vm
```

Displays real-time metrics updated every second:

- **CPU**: Usage percentage, cumulative CPU time, sparkline history
- **Memory**: RSS vs configured limit, percentage, sparkline
- **Network**: RX/TX rates and totals, sparkline (per-VM via vmenet interface)
- **Disk**: Actual vs logical usage with progress bar

All UI elements use a monotone (whites and grays) color palette. Press `q`,
`Esc`, or `Ctrl+C` to exit.

Works with both running and paused VMs (paused VMs show frozen metrics).

### `vm rm <name>`

Delete a VM and all its data.

```bash
monolith vm rm my-vm
monolith vm remove my-vm    # alias
monolith vm delete my-vm    # alias
```

Prompts for confirmation before deleting. If the VM is running or paused,
it is force-stopped first. Removes the entire VM directory including disk
image, config, state, logs, and suspend files.

### `vm prune`

Remove all stopped VMs.

```bash
monolith vm prune
```

Lists all VMs that are in the `stopped` state (not running, paused, or
suspended) and prompts for confirmation before deleting them. Shows the
name and actual disk usage of each VM to be pruned.

## Docker Host

`monolith docker` runs Docker Engine in VMs and points the docker CLI on the
Mac at them, like Colima. Every command except `list` takes `--name`
(default `docker-lith-default`).

```bash
monolith docker start                  # first run installs Docker in the VM (~20s)
docker run --rm -it busybox sh         # context docker-docker-lith-default is now current
docker run -d -p 8080:80 nginx         # http://localhost:8080
docker run --rm -v "$PWD":/src busybox ls /src   # paths under /Users work
monolith docker pause                  # save the whole VM to disk; frees its memory
monolith docker resume                 # containers carry on where they were
monolith docker start --name lith-2    # a second host; context docker-lith-2
monolith docker list
monolith docker stop
monolith docker rm --name lith-2       # deletes the VM and its context
```

- `start` creates the context `docker-<name>` and makes it current (like
  `docker context use`). It resumes a paused VM. It takes `--cpus` (default 2)
  and `--memory` (default `2G`). If `DOCKER_HOST` is set in your shell, it
  overrides the context; `start` warns about it.
- `pause` is `vm suspend`: memory and devices go to `suspend.vmstate`.
  Resuming needs the Mac unlocked. Connections open at pause time are dropped
  on resume, and the guest clock is corrected within seconds.
- `stop` on a paused VM discards its saved state (it asks first, or pass
  `--force`); images and volumes stay.
- Images and containers live on a 64 GiB sparse `data.img` per VM.
- Published TCP ports are forwarded to `127.0.0.1`. If two VMs publish the same
  port, the first one keeps it; `docker list` marks the other with `!`. UDP
  ports and ports bound to a specific address (`-p 127.0.0.1:80:80`) aren't
  forwarded.
- `monolith vm attach <name>` opens a VM's console; dockerd logs to
  `/var/log/docker.log`.

### containerd engine

`--engine containerd` creates a host running containerd, nerdctl and BuildKit
instead of dockerd. It has no Docker API, so no docker context; use nerdctl
through monolith:

```bash
monolith docker start --name ctr --engine containerd
monolith docker nerdctl --name ctr run --rm -it busybox sh
monolith docker nerdctl --name ctr run -d -p 8080:80 nginx   # http://localhost:8080
monolith docker nerdctl --name ctr build -t app .            # paths under /Users work
monolith docker nerdctl --name ctr compose up -d
alias nerdctl='monolith docker nerdctl --name ctr'
```

The engine is fixed when the VM is created. Pause/resume, port forwarding,
`/Users` and `list` work the same as with the docker engine. nerdctl runs as
root over an internal sshd that only monolith can reach; logs are in
`/var/log/containerd.log` and `/var/log/buildkitd.log`.


### Kubernetes (k3s)

`--kubernetes` runs a single-node [k3s](https://k3s.io) cluster on the host's
engine, so images you build locally can run in pods without a registry:

```bash
monolith docker start --name dev --kubernetes      # 4 GiB by default; ~25s first start
kubectl get nodes                                  # context k3s-dev is now current
docker build -t app:1 .                            # containerd engine: monolith docker nerdctl --name dev build -t app:1 .
kubectl run app --image=app:1 --image-pull-policy=Never
kubectl create deployment web --image=nginx
kubectl expose deployment web --type=LoadBalancer --port=8080 --target-port=80
curl localhost:8080
monolith docker start --name dev --kubernetes=false   # off on the next start
```

- `start` merges the cluster, user and context `k3s-<name>` into your
  kubeconfig (the first file in `$KUBECONFIG`, else `~/.kube/config`; the
  previous version is kept as `<file>.monolith.bak`) and makes it current.
  `monolith docker kubeconfig` prints a standalone copy.
- LoadBalancer ports and NodePorts are forwarded to `localhost`, like
  published container ports. Traefik is disabled.
- The k3s version is pinned (`--k3s-version` to choose another) and downloaded
  once to `~/.cache/monolith/k3s/`.
- Turn Kubernetes on or off only on a stopped host. Hosts created before
  Kubernetes support need recreating first.
- Pause/resume keeps the cluster and its pods as they were.

## Console Interaction

When attached to a VM console (via `vm run` or `vm attach`), the terminal
is set to raw mode and all input/output flows through the serial console.

### Detaching

- **Ctrl+C** (0x03): Quick detach. The VM keeps running in the background.

### Escape Prompt

- **Ctrl+]** (0x1D): Enter command mode. A `monolith>` prompt appears.

From the escape prompt, single-key commands are available:

| Key | Action |
|-----|--------|
| `q` | Detach (VM keeps running) |
| `s` | Stop VM (graceful shutdown) |
| `p` | Pause VM |
| `r` | Resume VM |
| `z` | Suspend VM to disk |
| `?` or `h` | Show help |
| `Ctrl+]` | Send literal Ctrl+] to guest |

Any other key cancels the escape prompt and returns to the console.

## VM Lifecycle

### States

| State | Daemon alive? | VM in memory? | Suspend file? | Console? |
|-------|--------------|---------------|---------------|----------|
| stopped | No | No | No | No |
| running | Yes | Yes | No | Yes |
| paused | Yes | Yes | No | Yes |
| suspended | No | No | Yes | No |

### State Transitions

```
                     vm run
            ┌──────────────────┐
            │                  ▼
         stopped ◄─── vm stop ── running
            ▲                   ╱  │  ╲
            │          vm pause╱   │   ╲vm suspend
            │                ╱     │    ╲
            │               ▼      │     ▼
            │            paused    │  suspended
            │               │      │     │
            │    vm resume  │      │     │ vm resume
            │               └──►running◄─┘
            │                      │
            └──────── vm stop ─────┘
```

Detailed transitions:

| From | To | Trigger | What happens |
|------|----|---------|-------------|
| stopped | running | `vm run` | Spawn daemon, `vm.Start()`, write state |
| running | running (reattach) | `vm run` / `vm attach` | Connect to existing daemon console |
| running | paused | `vm pause` | `vm.Pause()`, guest clock stops |
| paused | running | `vm resume` | `vm.Resume()`, guest clock resumes |
| running | stopped | `vm stop` | `vm.RequestStop()` or `vm.Stop()`, daemon exits |
| paused | stopped | `vm stop` | `vm.Stop()`, daemon exits |
| running | suspended | `vm suspend` | `vm.Pause()` -> `vm.SaveMachineStateToPath()` -> `vm.Stop()`, daemon exits |
| paused | suspended | `vm suspend` | `vm.SaveMachineStateToPath()` -> `vm.Stop()`, daemon exits |
| suspended | running | `vm resume` | Spawn daemon with `--restore`, `vm.RestoreMachineStateFromURL()` -> `vm.Resume()` |
| suspended | running | `vm run` | Same as `vm resume` for suspended VMs |

## Architecture

### Daemon Model

Each running VM is owned by a background daemon process. The CLI client
communicates with the daemon over a Unix domain socket.

```
┌──────────────────┐          ┌──────────────────────────────────────┐
│  CLI Client      │          │  VM Daemon (monolith __vmd <name>)   │
│  (foreground)    │          │  (background)                        │
│                  │          │                                      │
│  Terminal ←──────┼── unix ──┼─→ PTY master ←→ VZ Serial Console   │
│  (raw mode)      │  socket  │                                      │
│                  │          │  Virtio Block (disk.img)             │
│  Control cmds ───┼── unix ──┼─→ VZ API (pause/resume/stop/save)   │
│  (JSON)          │  socket  │                                      │
└──────────────────┘          └──────────────────────────────────────┘
```

The daemon (`monolith __vmd <name>`) is a hidden CLI command that:

1. Reads the VM config from `config.json`
2. Prepares its base image (`vm.LookupImage(cfg.Image).Prepare`): downloads
   and verifies kernel and rootfs if not cached; for Alpine, creates the
   per-VM disk image on first boot
4. Creates a PTY pair for the serial console
5. Configures and boots the VM via Virtualization.framework
6. Listens on a Unix domain socket for client connections
7. Handles console attachment and control commands
8. Manages state transitions and writes state files
9. Exits when the VM stops (cleaning up state)

### Control Protocol

The Unix domain socket at `~/.cache/monolith/vms/<name>/daemon.sock`
accepts two types of connections, distinguished by the first byte:

#### Console Attach (`0x01`)

```
Client → Daemon:  [0x01]                    # request attachment
Daemon → Client:  [0x01]                    # ACK (attached)
                  [0xFF]                    # NACK (another client attached)
              ... bidirectional raw bytes (PTY relay) ...
```

After the ACK, raw bytes flow in both directions: client input goes to the
PTY (and thus the VM's serial console), and VM output flows back to the
client. Only one console client can be attached at a time.

#### Control Command (`0x02`)

```
Client → Daemon:  [0x02] [4-byte BE length] [JSON request]
Daemon → Client:  [0x02] [4-byte BE length] [JSON response]
```

Request format:
```json
{"cmd": "pause|resume|suspend|stop|status", "force": false}
```

Response format:
```json
{"ok": true, "status": "running|paused|suspended|stopped", "error": ""}
```

### Stale State Detection

When reading VM state, monolith validates that the daemon process is still
alive by sending signal 0 to the recorded PID. If the process is dead:

- **Running/paused state**: Treated as stale (daemon crashed). State file is
  removed and the VM is reported as stopped.
- **Suspended state**: Expected (daemon intentionally exited). State is kept.

## Guest Environment

### Operating System

This section describes the **Alpine** image. Kata VMs instead boot Kata's
kernel and its read-only Ubuntu rootfs (dm-verity), get a root bash from
kata-agent's debug console, and keep no state across boots; see AGENTS.md §
Base Images.

Alpine VMs run Alpine Linux 3.21 with the `virt` kernel variant, optimized for
virtualization. The guest boots from:

- **Kernel**: `vmlinuz-virt` (extracted and decompressed from the Alpine ISO)
- **Initramfs**: `initramfs-virt` (stock Alpine, loads virtio drivers)
- **Root filesystem**: 1 GiB ext4 disk image populated with `alpine-minirootfs`
- **Kernel modules**: modloop squashfs mounted at `/lib/modules` (provides
  `af_packet`, `virtio_net`, etc.)

### Boot Sequence

1. `VZLinuxBootLoader` loads kernel with args: `console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop`
2. Alpine initramfs loads virtio drivers, discovers `/dev/vda`
3. `switch_root` to ext4 filesystem
4. Busybox init reads `/etc/inittab`:
   - Remounts root read-write
   - Sets hostname
   - Mounts modloop squashfs
   - Loads `af_packet` module (for DHCP)
   - Configures loopback and eth0
   - Runs DHCP with retry/backoff (`dhcp-configure`)
   - Starts `getty` with auto-login on `hvc0`

### User Account

| Property | Value |
|----------|-------|
| Username | `mono` |
| UID/GID | 1000/1000 |
| Home | `/home/mono` |
| Shell | `/bin/sh` (busybox) |
| Groups | `mono`, `wheel` |
| Auto-login | Yes (via custom `getty` on `hvc0`) |
| Sudo | `sudo <cmd>` runs as root via `su` wrapper |
| Root password | Empty (allows `su` without prompt) |

### Shell Prompt

```
mono@my-vm:~$
```

The prompt format is `mono@<vm-name>:<cwd>$`, set via `PS1` in `.profile`.

### MOTD

On login, the message of the day displays the VM name, Alpine version,
user info, root access instructions, and disk information.

## Configuration

### Per-VM Config (`config.json`)

Stored at `~/.cache/monolith/vms/<name>/config.json`:

```json
{
  "cpus": 2,
  "memory": 1073741824,
  "network_mode": "shared",
  "network_name": "my-net",
  "host_interface": ""
}
```

| Field | JSON Key | Default | Description |
|-------|----------|---------|-------------|
| CPUs | `cpus` | 1 | Number of virtual CPUs |
| Memory | `memory` | 536870912 (512 MiB) | RAM in bytes |
| NetworkMode | `network_mode` | `nat` | One of: nat, shared, host, isolated, none |
| NetworkName | `network_name` | (empty) | Named network to join |
| HostInterface | `host_interface` | (empty) | Physical NIC for host mode |

Default config is applied when a VM is created for the first time. CLI
flags override the defaults and are persisted for subsequent boots.

### Memory Format

The `--memory` flag accepts human-readable sizes:

| Format | Example | Bytes |
|--------|---------|-------|
| Plain number | `512` | 512 MiB (treated as mebibytes) |
| `M` suffix | `512M` | 512 MiB |
| `MiB` suffix | `512MiB` | 512 MiB |
| `MB` suffix | `500MB` | 500,000,000 bytes |
| `G` suffix | `1G` | 1 GiB |
| `GiB` suffix | `1GiB` | 1 GiB |
| `GB` suffix | `1GB` | 1,000,000,000 bytes |

### Per-VM State (`state.json`)

Stored at `~/.cache/monolith/vms/<name>/state.json`. Written by the daemon
process and read by CLI commands for status reporting:

```json
{
  "pid": 12345,
  "mac": "ce:b7:d1:c3:89:1d",
  "started_at": 1718900000,
  "status": "running",
  "suspend_file": ""
}
```

| Field | Description |
|-------|-------------|
| `pid` | Daemon process ID (0 when stopped/suspended) |
| `mac` | VM's MAC address (assigned by Virtualization.framework) |
| `started_at` | Unix timestamp of last boot |
| `status` | Current status: `running`, `paused`, `suspended` |
| `suspend_file` | Path to `.vmstate` file (only when suspended) |

## Filesystem Layout

```
~/.cache/monolith/
  alpine/                      # shared Alpine assets (downloaded once)
    vmlinuz-virt               # decompressed ARM64 kernel image
    initramfs-virt             # stock Alpine initramfs
    minirootfs.tar.gz          # Alpine minirootfs tarball
    modloop-virt               # kernel modules squashfs

  vms/                         # per-VM directories
    my-vm/
      disk.img                 # 1 GiB ext4 root filesystem (sparse)
      config.json              # VM hardware configuration
      state.json               # runtime state (PID, MAC, status)
      daemon.sock              # Unix domain socket (while daemon runs)
      daemon.log               # daemon stdout/stderr
      suspend.vmstate          # VM state snapshot (only when suspended)
```

### Disk Image

Each VM gets a 1 GiB sparse ext4 disk image. The actual disk usage is much
smaller than the logical size — `vm list` shows both (e.g. `45.2 MiB / 1 GiB`).

The disk is created once on first boot and reused for all subsequent boots.
It contains the full Alpine root filesystem plus VM-specific configuration
(hostname, user accounts, inittab, etc.).

To regenerate a VM's disk (e.g. after changing embedded templates):

```bash
rm ~/.cache/monolith/vms/my-vm/disk.img
monolith vm run my-vm
```

To force re-download of shared Alpine assets:

```bash
rm -rf ~/.cache/monolith/alpine/
```

## Troubleshooting

### VM fails to start

Check the daemon log:

```bash
cat ~/.cache/monolith/vms/<name>/daemon.log
```

Common causes:

- **Binary not codesigned**: The binary must be signed with
  `com.apple.security.virtualization`. Always use `make build`.
- **Missing kernel/initrd**: Delete `~/.cache/monolith/alpine/` to force
  re-download.
- **Port already in use**: A stale daemon socket may exist. Remove it:
  `rm ~/.cache/monolith/vms/<name>/daemon.sock`

### `vm attach` says another client is attached

Only one console client can be attached at a time. Detach the other client
first (Ctrl+C) or stop and restart the VM.

### VM shows no IP address

IP discovery relies on the macOS DHCP leases file (`/var/db/dhcpd_leases`).
The DHCP server can be slow; wait a few seconds after boot and try
`vm list` again. The guest runs a DHCP client with retry/backoff
(`dhcp-configure`) that handles slow lease assignment.

For `none` and `isolated` network modes, no IP is expected.

### `vm suspend` fails with "save/restore validation failed"

Suspend requires macOS 14+ and a VM configuration that supports save/restore.
The `ValidateSaveRestoreSupport()` check must pass. This is a
Virtualization.framework constraint.

### `vm resume` fails with "the Mac is locked"

Restoring decrypts the suspend file with a Secure Enclave key that macOS only
releases while the screen is unlocked (otherwise Virtualization.framework
reports error 12 "permission denied"). Unlock the Mac and run `vm resume`
again; the snapshot is kept.

### `vm resume` fails with "suspend file not found"

The `.vmstate` file may have been deleted or corrupted. The VM must be
started fresh:

```bash
monolith vm run my-vm
```

### Stale state after host crash or reboot

If the host crashes or reboots while a VM is running, the state file becomes
stale (PID no longer exists). monolith detects this automatically: the next
`vm list` or `vm run` will clean up the stale state and report the VM as
stopped.

### Guest changes lost after disk regeneration

The disk image is only created once. If you delete `disk.img` and re-run the
VM, all guest-side changes (installed packages, files, etc.) are lost. The
disk is recreated from the Alpine minirootfs template.

## Implementation Reference

| File | Purpose |
|------|---------|
| `vm/vmd.go` | VM daemon: `RunDaemon()`, PTY setup, socket server, control handlers |
| `vm/client.go` | Client library: `AttachConsole()`, `SendControlCommand()`, `SpawnVMDaemon()` |
| `vm/protocol.go` | Shared types: message types, control commands, request/response structs |
| `vm/state.go` | VM state: `ReadState()`, `WriteState()`, `UpdateStateStatus()` (IP discovery is in `net/ip.go`) |
| `vm/config.go` | VM config: `VMConfig`, `ReadConfig()`, `WriteConfig()` (mode constants are in `net/modes.go`) |
| `vm/image.go` | Base image interface (`Image`, `BootSpec`), `LookupImage()`, `Exists()` |
| `vm/image_kata.go` | Kata image (default): GHCR download, dm-verity kernel command line |
| `vm/image_alpine.go` | Alpine image adapter |
| `vm/alpine_kernel.go` | Alpine asset download: kernel, initramfs, minirootfs, modloop |
| `vm/alpine_disk.go` | Alpine disk image creation: ext4 formatting, rootfs extraction, VM configuration |
| `cli/vmd.go` | Hidden `__vmd <name>` command for daemon process |
| `cli/vm/run.go` | `vm run` command with flags and state-aware dispatch |
| `cli/vm/attach.go` | `vm attach` command |
| `cli/vm/pause.go` | `vm pause` command |
| `cli/vm/resume.go` | `vm resume` command |
| `cli/vm/suspend.go` | `vm suspend` command |
| `cli/vm/stop.go` | `vm stop` command |
| `cli/vm/list.go` | `vm list` command with table formatting |
| `cli/vm/top.go` | `vm top` launcher |
| `cli/vm/top_model.go` | Bubbletea TUI model for live monitoring |
| `cli/vm/rm.go` | `vm rm` command with confirmation |
| `cli/vm/prune.go` | `vm prune` command for batch cleanup |
| `cli/vm/helpers.go` | Shared escape command handler |
| `vm/embeddings/` | Alpine guest configuration templates (inittab, passwd, motd, etc.) |
| `entitlements.plist` | Code signing entitlements |

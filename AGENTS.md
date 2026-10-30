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
monolith vm run [name] --image alpine                       # base image: kata (default for new VMs), alpine or docker
monolith vm run [name] --network-mode shared                # boot with shared vmnet networking (macOS 26+)
monolith vm run [name] --network-mode shared --network lan  # join named network "lan"
monolith vm run [name] --network-mode host --host-interface en0  # bridge to physical NIC
monolith vm run [name] --network-mode isolated              # isolated network (macOS 26+)
monolith vm run [name] --network-mode none                  # no network device (air-gapped)
monolith vm run [name] --network-mode user                  # user-mode networking in the daemon (gvisor-tap-vsock)
monolith vm run [name] --log-console PATH                  # tee raw guest console output to PATH from boot
monolith vm attach <name>     # attach to a running VM's serial console (one client at a time)
monolith vm pause <name>      # pause vCPUs in memory
monolith vm resume <name>     # resume from pause or suspend
monolith vm suspend <name>    # snapshot VM state to disk (restore needs the Mac unlocked, see Gotchas)
monolith vm stop <name>       # stop: request shutdown, force after 10s (--force: immediately). On a suspended VM, discards the snapshot
monolith vm list              # table: name, state, image, ip, net, cpus, mem, disk, uptime
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

# Docker hosts (Colima-style); every command but list takes --name (default docker-lith-default)
monolith docker start [--cpus N --memory 2G]      # boot (or resume) the VM, wait for dockerd, make context docker-<name> current
monolith docker start --engine containerd         # new VM with containerd + nerdctl + BuildKit instead of dockerd (fixed at creation)
monolith docker nerdctl [--name N] run -d nginx   # run nerdctl in a containerd host (args pass through; -it gets a TTY)
monolith docker start --kubernetes [--k3s-version v1.36.5+k3s1]  # run k3s on the engine; merges kubectl context k3s-<name> and makes it current
monolith docker start --kubernetes=false          # turn k3s off on the next boot (data kept; kubectl context removed)
monolith docker kubeconfig                        # print the standalone kubeconfig (context k3s-<name>)
monolith docker pause                             # save the whole VM to disk and stop it (vm suspend)
monolith docker resume                            # restore it, wait for dockerd and the clock, make its context current
monolith docker stop [--force]                    # stop it (data persists); on a paused VM, discards the snapshot after asking
monolith docker list                              # docker VMs: state, engine, context (* = current), cpus, mem, data, ports
monolith docker rm [--force]                      # delete the VM and its docker context
monolith docker env                               # print: export DOCKER_HOST=unix://…/vms/<name>/docker.sock
```

If no name is given to `run`, one is auto-generated as `lith-NNNNN` (random 5-digit number). VM names must be lowercase alphanumeric with hyphens, max 63 characters (valid hostnames). The `network` command is also aliased as `net`.

## Tests

```bash
go test ./...             # all tests
go test -run TestRun ./vm # single test
MONOLITH_TEST_DOCKER=1 go test -run TestDockerPauseResume ./vm  # docker host pause/resume (needs internet, docker CLI, unlocked Mac)
MONOLITH_TEST_K8S=1 go test -run TestKubernetesPauseResume ./vm  # k3s on both engines: LB service, pause/resume (needs internet, kubectl, unlocked Mac)
```

**Tests boot a real VM.** They require macOS on Apple Silicon with Virtualization.framework. There is no mock/unit-only test path. Do not expect tests to pass in CI or on non-macOS hosts.

## Platform Constraint

macOS + aarch64 only. The entire codebase depends on Apple Virtualization.framework via upstream `github.com/Code-Hex/vz/v4`, pinned in go.mod to an untagged master commit because vmnet support (macOS 26+) only exists on master; the latest release, v3.8.0, has none. The previous `norio-nomura/vz/v3` fork was dropped: its `NewFileHandleSerialPortAttachment` always returned nil, so guest console output was silently discarded (issue 0001). No cross-platform build targets exist.

## Package Layout

The repo is a single Go module: package `vm` (VM daemon, client, state, disk/kernel), package `net` (vmnet, named-network daemons, IP discovery), a small root package `monolith` with code both share (logging, cache paths, process and codesign helpers), and the `cli/` package tree. Imports go one way: `vm` → `net` → `monolith`. The CLI never touches `vz` directly — it spawns and talks to per-VM daemons over unix sockets.

```
monolith/
  cmd/monolith/main.go   — CLI entry point, calls cli.Execute()
  cli/
    root.go               — root cobra command, registers vm + network + docker subcommands
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
    docker/
      docker.go           — "docker start|stop|pause|resume|rm|env" for Docker host VMs (image docker, user mode), --engine
      ssh.go              — "docker nerdctl": execs ssh to the containerd host's internal sshd via ssh.sock
      kube.go             — "docker kubeconfig", kubeconfig fetch/rewrite, merge into $KUBECONFIG or ~/.kube/config, readiness
      context.go          — creates/selects/removes the docker CLI context docker-<name> (shells out to docker)
      list.go             — "docker list": docker VMs with context and forwarded ports (from the daemon's status reply)
  vm/                     — package vm: everything about a single VM. Imports net and the root package.
    vmd.go                — RunDaemon(name, restore, consoleLogPath): the per-VM daemon. Owns vz.VirtualMachine,
                            listens on a unix socket, dispatches console-attach and control
                            (pause/resume/suspend/stop/status/resize) requests.
    client.go             — client side of the daemon protocol (AttachConsole, SendControlCommand, SpawnVMDaemon).
    console_bus.go        — drains the console PTY from boot; 64 KiB ring buffer replayed to the attached client.
    protocol.go           — wire types shared by client and daemon.
    config.go             — VMConfig (CPUs, Memory, NetworkMode, NetworkName, HostInterface, Image, MAC).
                            Per-VM config at vms/<name>/config.json. Defaults: 1 vCPU, 512 MiB, NAT, kata.
    state.go              — VMState/VMStatus, state.json lifecycle, host-side process stats, DiscardSuspended.
    image.go              — Image interface (Prepare(name) → BootSpec), LookupImage, ValidImages, Exists.
    image_kata.go         — kata image: fetch kernel + rootfs from GHCR, dm-verity command line.
    image_alpine.go       — alpine image: wraps alpine_kernel.go + alpine_disk.go.
    image_docker.go       — docker image: alpine + embeddings/docker overlay (+ embeddings/containerd for that engine),
                            sparse data.img, /Users virtio-fs share; EngineDocker/EngineContainerd, EngineOf.
    sshkey.go             — per-VM ed25519 key for the containerd engine's internal sshd.
    k3s.go                — k3s download/verify into the cache (ensureK3s), DefaultK3sVersion, GuestLayout, FreeLocalPort.
    docker.go             — daemon-side services: engine socket relay (docker.sock or ssh.sock) and published-port
                            forwarding (ports from dockerd's API or `nerdctl ps --format json`).
    alpine_kernel.go      — DefaultKernel() downloads Alpine ISO, extracts/decompresses kernel+initrd.
                            DefaultRootFS() downloads minirootfs tarball.
    alpine_disk.go        — DefaultDisk(tarball, modloop, name, overlays...) creates the per-VM 1 GiB ext4 disk image
                            and writes guest config files from embeddings/manifest.yaml plus overlay manifests
                            (embeddings/docker/manifest.yaml) (go:embed needs them inside vm/).
    paths.go              — VMsDir, VMDir, VMSockPath, VMSuspendPath, VMLogPath, VMMachineIDPath, VMDockerSockPath,
                            VMSSHKeyPath, VMSSHSockPath.
    keybag.go             — keybagLocked() (MobileKeyBag via dlopen, cgo): restore is refused while the Mac is locked.
    *_test.go             — tests (boot real VMs via ../bin/monolith; image_kata_test.go also has unit tests).
  net/                    — package net: networking. Imports only the root package (never vm).
                            Callers that also use the standard library import it as `mnet`.
    modes.go              — network mode constants (NetworkNAT, NetworkShared, …) and ValidNetworkModes.
    network.go            — ConfigureNetwork(vmCfg, mode, networkName, hostInterface, mac): builds a VM's
                            network device (NAT, vmnet shared/isolated, bridged, user, or none).
    usernet.go            — UserNetwork: gvisor-tap-vsock virtual network for "user" mode, DialGuest.
    netd.go               — RunNetworkDaemon() (vmnet.ShareNetwork), joinNamedNetwork()
                            (vmnet.ReceiveNetwork), network config/PID persistence, packet monitor.
    launchd.go            — StartNetworkDaemon()/StopNetworkDaemon(): per-network launchd job
                            (plist, launchctl bootstrap/bootout), Mach service naming.
    ip.go                 — LookupIPForMode (DHCP leases + ARP fallback), VmenetForMAC, NetworkStats.
    paths.go              — NetworksDir, NetworkDir, NetworkLogPath.
    network_subnet_test.go — named-network subnet tests (via ../bin/monolith).
  paths.go                — CachePath(): root of every ~/.cache/monolith path.
  process.go              — ProcessAlive(pid).
  logger.go / logs.go     — Logger and log-file streaming used by both daemons and `logs` commands.
  entitlements.go         — runtime entitlement checks and OwnCodeRequirement() (CDHash of this binary).
  entitlements.plist      — codesigning entitlements (com.apple.security.virtualization).
  docs/                   — codesigning.md, networking.md, vm.md (deeper references).
```

### Daemon model

Every running VM is a separate `monolith __vmd <name>` process. The `vm run` CLI command spawns this daemon, the daemon owns the `vz.VirtualMachine`, and all subsequent CLI commands (`attach`, `pause`, `resume`, `suspend`, `stop`, `top`, `list` for live status) go through the daemon's unix socket at `~/.cache/monolith/vms/<name>/daemon.sock`. Only one console client may attach at a time; a second gets a NACK (`0xFF`). `console_bus.go` drains the PTY continuously from before boot and replays the last 64 KiB to whoever attaches, so early kernel output is not lost.

Socket protocol (first byte selects the request type, see `protocol.go`):
- `0x01` console attach: daemon ACKs with `0x01`, then raw console bytes flow both ways. Nothing else is ever sent on this stream.
- `0x02` control: 4-byte big-endian length + JSON `ControlRequest` (`pause`, `resume`, `suspend`, `stop`, `status`, `resize`), answered the same way with a `ControlResponse`. `vm attach` sends `resize` (rows/cols) on attach and on `SIGWINCH` over a separate control connection; the daemon applies it to the host PTY with `TIOCSWINSZ`. The guest's `hvc0` does not see host PTY size changes, so the guest still needs `stty rows R cols C` to match. This is also what enables `restore` (resume from a suspended snapshot) on the next `vm run`.

## Network Modes

Six network modes are supported via the `--network-mode` flag on `vm run`:

| Mode | Flag | Implementation | Requires |
|------|------|---------------|----------|
| `nat` (default) | `--network-mode nat` | `VZNATNetworkDeviceAttachment` | macOS 11+ |
| `shared` | `--network-mode shared` | `vmnet.ModeShared` + `vz.NewVMNetNetworkDeviceAttachment` | macOS 26+ |
| `host` | `--network-mode host --host-interface en0` | `VZBridgedNetworkDeviceAttachment` | `com.apple.vm.networking` entitlement (requires provisioning profile or SIP disabled) |
| `isolated` | `--network-mode isolated` | `vmnet.ModeHost` + `vz.NewVMNetNetworkDeviceAttachment` | macOS 26+ |
| `none` | `--network-mode none` | No network device | - |
| `user` | `--network-mode user` | gvisor-tap-vsock in the VM daemon, via `VZFileHandleNetworkDeviceAttachment` | - |

- **nat**: Standard NAT networking. VM gets IP from macOS DHCP server. No inter-VM communication.
- **shared**: macOS 26 vmnet SharedMode. NAT + inter-VM communication (when sharing the same `vmnet.Network` instance). Configurable subnets, DHCP reservations, port forwarding.
- **host**: Bridged directly to a physical NIC (e.g. en0). VM gets IP from the router. Per-VM network stats not available.
- **isolated**: macOS 26 vmnet HostMode. VM can talk to host only, no NAT, no inter-VM communication.
- **none**: Air-gapped. No network device attached to the VM.
- **user**: User-mode networking, as in Lima/Colima and Podman machine. The guest NIC is attached to one end of a `SOCK_DGRAM` socket pair carrying raw Ethernet frames (vfkit protocol); `net/usernet.go` runs gvisor-tap-vsock's userspace TCP/IP stack on the other end inside the VM daemon. The guest gets `192.168.127.2` by static DHCP lease for its MAC (gateway/DNS `192.168.127.1`; `192.168.127.254` is NATed to the Mac's `127.0.0.1`). Guest connections are re-opened by the daemon process, so outbound traffic works wherever the Mac is online, including Macs whose endpoint security drops vmnet's forwarded traffic. The Mac can't reach the guest by IP; the daemon reaches it with `UserNetwork.DialGuest`. Each VM gets its own private network (no inter-VM traffic). Required by the docker image.

The network mode is persisted in `config.json` and reused on subsequent boots.

## Named Networks

Named networks allow multiple VMs to share the same vmnet.Network instance, enabling inter-VM communication for `shared` and `isolated` modes.

### Architecture

vz v4 shares a vmnet network between processes only through a **launchd Mach service** (`vmnet.ShareNetwork` / `vmnet.ReceiveNetwork`); the serialization is an opaque XPC object with no byte encoding. So each named network runs as a per-user launchd job:

1. `monolith network create|start <name>` writes `~/.cache/monolith/networks/<name>/launchd.plist` (label and Mach service `dev.monolith.network.<name>`, `ProgramArguments` = this binary + `__netd <name> <mode> [--verbose]`) and loads it with `launchctl bootstrap gui/<uid>`. Nothing is installed in `~/Library/LaunchAgents`, so jobs don't survive logout.
2. The daemon creates the `vmnet.Network` (applying `user_subnet` via `vmnet.WithIPv4Subnet`), writes the actual subnet to `config.json`, publishes it with `vmnet.ShareNetwork`, then writes its PID file. The PID file is what `IsNetworkDaemonAlive` checks.
3. A VM started with `--network <name>` calls `vmnet.ReceiveNetwork` on that service (5s timeout) and attaches with `vz.NewVMNetNetworkDeviceAttachment`.
4. `monolith network stop|rm` runs `launchctl bootout`, which SIGTERMs the daemon and unregisters the service. Killing the daemon directly leaves the job loaded (`network start` boots out stale jobs first).

### Peer authentication

Both `ShareNetwork` and `ReceiveNetwork` require a code signing requirement for the other side. monolith uses `cdhash H"<hash>"` of its own binary (`OwnCodeRequirement()` in `entitlements.go`), so only the exact same build can join. **Rebuilding or re-signing changes the hash**: after `make build`, restart running networks (`monolith network stop <n> && monolith network start <n>`) or VMs will time out joining them.

## Base Images

The guest OS is pluggable (`vm.Image`, `vm/image.go`). `vm run --image kata|alpine` picks one and saves it in `config.json`. New VMs default to **kata**. A VM whose config has no `image` field, or whose directory has a `disk.img` but no config, is treated as **alpine**, so pre-existing VMs keep booting from their disks. `vm run` calls `Image.Prepare` in the foreground before spawning the daemon, so first-run downloads are visible and can't trip the daemon's 10s socket timeout; the daemon calls it again (cached, instant). Both images share the daemon, console, networking and control plane.

| | kata (default) | alpine | docker |
|---|---|---|---|
| Kernel | Kata 4.3.1 `vmlinux` (raw ARM64 Image, no modules, virtio/vsock/dm-verity built in) | Alpine 3.21 `vmlinuz-virt` + `initramfs-virt` from the ISO | same as alpine |
| Root disk | Kata Ubuntu 26.04 rootfs image, **read-only, shared by all kata VMs** | per-VM writable 1 GiB ext4 `disk.img` | alpine `disk.img` + `embeddings/docker` overlay, plus a 64 GiB sparse `data.img` (`/dev/vdb`) on `/var/lib/docker` |
| Persistence | none (ephemeral; tmpfs only) | disk persists across boots | both disks persist |
| Init / shell | systemd → `kata-containers.target` → kata-agent; `agent.debug_console` gives a root bash on hvc0 | busybox init, autologin as `mono` | busybox init runs `docker-setup`, then respawns dockerd and the API relay; autologin as `mono` |
| Network config | kernel `ip=dhcp` (no DHCP client or resolv.conf in the image) | `udhcpc` via `dhcp-configure` | `udhcpc`; network mode must be `user` |
| Hostname | `systemd.hostname=<vm name>` | `/etc/hostname` + inittab | same as alpine |
| Download | ~65 MB, once | ~64 MB, once | alpine assets + ~70 MB of apk packages inside the VM on first boot |

### Kata

`vm/image_kata.go` fetches two single-layer OCI artifacts from Kata's CI cache on GHCR, pinned to the release tag `4.3.1-aarch64`: `ghcr.io/kata-containers/cached-artefacts/kernel` and `.../rootfs-image`. These are the per-component builds of that release (~65 MB together) rather than the 680 MB all-in-one `kata-static` release tarball. The client gets an anonymous pull token, reads the manifest, streams the zstd tarball while hashing it, and only moves files into `~/.cache/monolith/kata/4.3.1/` once the blob matches the manifest digest. Bump `kataVersion` to update.

The rootfs image is an MBR disk: partition 1 is the ext4 root, partition 2 its dm-verity hash tree. `root_hash.txt` (Kata's `root_hash_base.txt`) supplies the root hash, salt and block sizes, from which the command line maps `/dev/dm-0` with `dm-mod.create=...` exactly as the Kata runtime does:

```
console=hvc0 dm-mod.create="dm-verity,,,ro,0 <sectors> verity 1 /dev/vda1 /dev/vda2 4096 4096 <blocks> 0 sha256 <hash> <salt>"
root=/dev/dm-0 rootflags=data=ordered,errors=remount-ro ro rootfstype=ext4 ip=dhcp
systemd.hostname=<name> systemd.unit=kata-containers.target agent.debug_console agent.log=warn
```

kata-agent's ttRPC API listens on vsock port 1024 inside the guest. monolith doesn't use it yet; it's the hook for running OCI containers in the guest.

### Alpine boot flow

1. `DefaultKernel()` — download Alpine ISO, extract `vmlinuz-virt` and `initramfs-virt`, decompress the EFI-stub kernel to raw ARM64 Image
2. `DefaultRootFS()` — download `alpine-minirootfs-3.21.6-aarch64.tar.gz`
3. `DefaultDisk(tarball, modloop, name)` — create 1 GiB sparse file at `vms/<name>/disk.img`, format ext4 (4096-byte blocks via go-diskfs), extract minirootfs tarball, write VM config files (inittab, passwd, shadow, group, motd, hostname, autologin, sudo, .profile)
4. Boot VM with kernel args `console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop` using stock Alpine initramfs
5. Alpine Init loads virtio drivers, mounts `/dev/vda` as root, runs `switch_root`
6. Busybox init reads `/etc/inittab`, remounts root rw, auto-logs in as user `mono`

Alpine guest configuration:

- **User**: `mono` (uid 1000, gid 1000), member of `wheel` group
- **Autologin**: `getty -n -l /usr/local/bin/autologin` on hvc0 — no login prompt
- **sudo**: `/usr/local/bin/sudo` wrapper script using `su`. `sudo <cmd>` runs as root, `sudo -i` opens root shell.
- **Hostname**: set to the VM name (both `/etc/hostname` and `::sysinit:/bin/hostname <name>` in inittab)
- **MOTD**: shows VM name, Alpine version, user info, root access, disk info
- **Root password**: empty (allows `su` without password prompt)

### Docker host

`monolith docker` manages Docker host VMs: any VM with image `docker` and network mode `user` (default 2 vCPU / 2 GiB). `--name` picks one (default `docker-lith-default`); commands refuse VMs with another image. `vm run --image docker` also works and defaults to user mode. It is Colima's design on monolith. Each VM has an **engine**, `engine` in `config.json` (empty = docker), chosen by `start --engine` when the VM is created and fixed afterwards because it's installed on the VM's disk; `start` refuses a different `--engine` for an existing VM. See [containerd engine](#containerd-engine) for the second one.

- **First boot**: `docker-setup` (sysinit, after DHCP) reads the engine from `/etc/monolith-engine` (written by the containerd overlay; absent = docker), mounts cgroup2, loads modules, runs `apk add` for the engine's packages if any is missing (docker: `docker socat e2fsprogs`), formats `/dev/vdb` if it has no ext4, mounts it on `/var/lib/docker` (containerd: see below), and mounts the virtio-fs tag `users` (the Mac's `/Users`) at `/Users` so `-v ~/x:/x` resolves. About 20s with a warm Alpine cache; later boots take ~3s to a ready dockerd.
- **API**: `docker-serve relay` runs `socat TCP-LISTEN:2375,bind=192.168.127.2,range=192.168.127.1/32` → `/var/run/docker.sock`. The daemon (`vm/docker.go`) listens on `vms/docker/docker.sock` on the Mac and relays each connection through `UserNetwork.DialGuest(2375)`. `range=` admits only the user-mode gateway, so containers can't reach dockerd. vsock was tried first, but vz's `VirtioSocketConnection` can't half-close (no `CloseWrite`), which broke attach (output lost, and `docker run -i` never sees stdin EOF). socat runs with `-t 86400` because its default closes a connection 0.5s after either side half-closes.
- **Ports**: the daemon polls `GET /containers/json` every second and keeps a `127.0.0.1:<PublicPort>` listener for each published TCP port, relaying to the same port on the guest. Ports published on a specific guest address (`-p 127.0.0.1:80:80`) and UDP ports are not forwarded; listeners bind to localhost only.
- **Contexts**: `start`/`resume` wait for `GET /_ping` (10 min timeout for slow first boots), then create or update the docker context `docker-<name>` (endpoint `unix://…/vms/<name>/docker.sock`) and `docker context use` it, which sets `currentContext` in `~/.docker/config.json` for all shells. `rm` deletes the context, switching to `default` first if it is current. They shell out to the `docker` CLI with `DOCKER_HOST`/`DOCKER_CONTEXT` removed from its environment; without the CLI on PATH, `start` just prints the socket. `start` warns when `DOCKER_HOST` is set, since it overrides contexts. `env` remains for scripts.
- **Several hosts**: each VM has its own private user-mode network (all use guest IP `192.168.127.2`), socket and context. Host ports are first come, first served: a port another VM already forwards is retried every poll, logged once, and reported with its bind error in the daemon's `status` reply (`ControlResponse.Ports`), which `docker list` shows as `8080!`.
- **Pause/resume** are `vm suspend`/restore: the whole VM, virtio-fs share and file-handle NIC included, saves and restores (snapshot ≈ guest memory in use, ~100 MB for an idle 2 GiB host; restore ~3s). Containers carry on mid-instruction. The user-mode network stack lives in the daemon, which exits on pause, so TCP connections open through it (a `docker logs -f`, a container's outbound connection) are dropped on resume. `start` on a paused VM resumes it; `stop` on one discards the snapshot (asks unless `--force`). Restoring needs the Mac unlocked (see Gotchas). Don't touch `disk.img`/`data.img` while paused.
- **Clock**: a restored guest's system clock is behind by the paused time; the RTC (pl031) follows the Mac's clock. `docker-serve clock` checks `/sys/class/rtc/rtc0/since_epoch` every 2s and runs `hwclock -s -u` when they differ by more than 2s. `resume` also waits (up to 10s) until dockerd's `/info` `SystemTime` is within 2s of the Mac's, so commands run right after it see the right time.
- **Port forwards** are re-created by the restored daemon's first poll, which runs immediately.
- Guest scripts are written into `disk.img` when it is created; after changing `embeddings/docker` or `embeddings/containerd`, recreate the VM (`docker rm --name <n>`), which also reinstalls the engine and discards images.

#### containerd engine

`start --engine containerd` builds the same VM with the `embeddings/containerd` overlay on top of `embeddings/docker`: its own inittab and motd, `/etc/monolith-engine`, `/etc/nerdctl/nerdctl.toml` (`cni_path = "/usr/libexec/cni"`, where Alpine puts the CNI plugins) and `/etc/ssh/monolith_sshd_config`. There is no Docker API, so no `docker.sock` and no docker context (`env` refuses; `list` shows `-`).

- **Guest**: first boot installs `containerd nerdctl cni-plugins buildkit buildctl iptables ip6tables openssh-server socat e2fsprogs` (~10s). The data disk is mounted on `/data`, with `/data/{containerd,nerdctl,buildkit}` bind-mounted onto `/var/lib/…`. init respawns `docker-serve containerd|buildkitd|sshd|ports|clock`; buildkitd runs with the containerd worker so `nerdctl build` uses containerd's image store.
- **Command channel**: `monolith docker nerdctl …` execs the Mac's `ssh` as root into the VM and runs `nerdctl` with the arguments shell-quoted (`-t` when stdin and stdout are terminals, so `-it`, exit codes, signals and stdin all behave). The connection goes `ssh -o ProxyCommand="nc -U vms/<name>/ssh.sock"` → daemon → `UserNetwork.DialGuest(22)`. Host keys aren't checked (local socket to the VM's own daemon; the key changes when the VM is recreated). This is the same approach as `colima nerdctl`.
- **SSH key**: `ensureSSHKey` (`vm/sshkey.go`) creates `vms/<name>/ssh_key{,.pub}` (ed25519) on first `Prepare`; the public key's base64 body goes on the kernel command line as `monolith.ssh_pubkey=…`, and `docker-setup` writes it to `/root/.ssh/authorized_keys` on every boot. sshd listens on `192.168.127.2:22` with `AllowUsers root@192.168.127.1`, key-only, and an iptables rule rejects port 22 from any other source, so containers can't reach it.
- **Ports**: `docker-serve ports` answers each connection to guest port 2377 (gateway only, via socat `range=`) with `nerdctl ps --format json`; the daemon polls it every second and parses the `Ports` column (`0.0.0.0:8080->80/tcp, …`; `parseNerdctlPorts`). nerdctl publishes with CNI's portmap plugin (iptables DNAT, no listening socket), which still applies to the forwarded connections because they arrive on eth0.
- **Readiness**: `start`/`resume` poll `nerdctl info` over ssh, then wait for the guest clock (`date +%s` over ssh) like the docker engine does through `/info`.
- Pause/resume, `/Users`, the clock loop, port clash reporting and `rm` work as for the docker engine.

#### Kubernetes (k3s)

`start --kubernetes` runs a single-node k3s server on the VM's engine (Colima-style), so images built with `docker build` or `nerdctl build` are usable in pods without a registry. It's saved in `config.json` (`kubernetes`, `k3s_version`, `kube_port`) and can be turned on or off on any start of a stopped VM; `start` refuses to change it on a running or paused one (it changes the VM's devices and boot arguments). New VMs created with `--kubernetes` default to 4 GiB.

- **k3s binary**: `ensureK3s` downloads `k3s-arm64` for the pinned version (`DefaultK3sVersion`, override with `--k3s-version`) from GitHub releases into `~/.cache/monolith/k3s/<version>/k3s`, verified against the release's `sha256sum-arm64.txt`. That directory is shared read-only over virtio-fs (tag `k3s`, mounted at `/opt/monolith/k3s`), so every VM reuses one download, and `monolith.k3s=1` goes on the kernel command line. Turning Kubernetes off drops both.
- **Guest layout**: the k3s services live in the inittab written when the disk is created, so `config.json` records `guest_layout` (`vm.GuestLayout`, currently 1) for new docker image VMs. VMs created earlier have none and are refused `--kubernetes` with a hint to recreate them.
- **Guest**: with `monolith.k3s=1`, `docker-setup` loads `vxlan`, makes `/` rshared, mounts the share, and bind-mounts `/var/lib/rancher` and `/var/lib/kubelet` from the data disk (`/var/lib/docker/.monolith/…` for docker, `/data/…` for containerd). `docker-serve k3s` runs `k3s server --disable=traefik --node-name=<vm> --write-kubeconfig-mode=600` with `--docker` (k3s's built-in cri-dockerd) or `--container-runtime-endpoint=unix:///run/containerd/containerd.sock`. For containerd, `docker-setup` also points the CRI's CNI `bin_dir`/`conf_dir` at k3s's flannel (`/var/lib/rancher/k3s/data/current/bin`, `/var/lib/rancher/k3s/agent/etc/cni/net.d`) because nerdctl's own conflist sits in `/etc/cni/net.d`, and sets nerdctl's and BuildKit's namespace to `k8s.io` so built images are visible to pods; turning Kubernetes off reverts both. Without the flag, `docker-serve k3s|k8s-info` just sleep.
- **API**: the daemon forwards `127.0.0.1:<kube_port>` (chosen once with `FreeLocalPort`, so several clusters coexist) to guest port 6443. k3s's serving certificate already covers 127.0.0.1.
- **kubeconfig**: `docker-serve k8s-info` (guest port 2378, gateway only) answers a one-line request: `kubeconfig` → `/etc/rancher/k3s/k3s.yaml`, `services` → `k3s kubectl get svc -A -o json`. The daemon's `kubeconfig` control command returns the first; the CLI renames cluster/user/context to `k3s-<name>`, points the server at `kube_port`, and merges the entries into the first file in `$KUBECONFIG` (else `~/.kube/config`) as a generic YAML document, keeping other fields and saving the previous file as `<file>.monolith.bak`, then sets `current-context`. `rm` and `--kubernetes=false` remove the entries (clearing `current-context` if it was this VM's). `docker kubeconfig` prints the standalone file.
- **Readiness**: after the engine, `start`/`resume` wait (up to 10 min; the first start pulls k3s's system images, ~25s on a warm cache) for the kubeconfig, `/readyz`, and the node's Ready condition, using the kubeconfig's client certificate.
- **Service ports**: every poll the daemon adds Kubernetes service ports to the engine's: LoadBalancer ports (k3s's servicelb serves them on the node) and NodePorts, TCP only (`parseKubeServicePorts`). They're forwarded to localhost like container ports, with the same clash rules.
- **list**: `docker list` has a K8S column with the k3s version; `vm list` shows `docker (docker+k3s)`.

### Per-VM identity

Each VM gets a MAC address (saved as `mac` in `config.json`) and a generic machine identifier (`vms/<name>/machine-identifier`) on first boot, reused on every later boot. Restoring a saved state requires both to match what it was saved with (a fresh random machine identifier made every restore fail with "invalid argument"); the stable MAC also keeps the VM's DHCP address stable.

## Cache Directory

```
~/.cache/monolith/
  alpine/                  — shared Alpine assets (kernel, initrd, minirootfs)
    vmlinuz-virt
    initramfs-virt
    minirootfs.tar.gz
  k3s/v1.36.5+k3s1/k3s     — k3s binary, shared read-only into VMs with --kubernetes
  kata/4.3.1/              — shared Kata assets (read-only for VMs)
    vmlinux
    rootfs.img
    root_hash.txt
  vms/                     — per-VM directories
    my-dev-box/
      disk.img             # alpine and docker only
      data.img             # docker only: /var/lib/docker (containerd engine: /data), sparse 64 GiB
      docker.sock          # docker engine only: dockerd API relay (docker context docker-<name>)
      ssh.sock             # containerd engine only: relay to the internal sshd, used by `docker nerdctl`
      ssh_key, ssh_key.pub # containerd engine only: key for the internal sshd
      config.json          # {"cpus":1,"memory":536870912,"network_mode":"nat","image":"alpine","mac":"..."}
      machine-identifier   # generic machine identifier, created on first boot
      state.json           # {"pid":12345,"mac":"...","started_at":..., "status":"running"}
      daemon.sock          # unix domain socket the per-VM daemon listens on
      daemon.log           # daemon stdout/stderr (consumed by `vm logs`)
    lith-48271/
      disk.img
  networks/                — named networks
    my-net/
      config.json          # {"mode":"shared","subnet":"192.168.64.0/24"}
      pid                  # daemon PID
      launchd.plist        # launchd job for the daemon (label dev.monolith.network.<name>)
      daemon.log           # daemon stdout/stderr
```

- Delete `~/.cache/monolith/alpine/` or `~/.cache/monolith/kata/` to force re-download of shared assets.
- Delete `~/.cache/monolith/vms/<name>/disk.img` to regenerate a VM's disk without re-downloading.
- Delete `~/.cache/monolith/vms/<name>/` entirely to remove a VM (or use `monolith vm rm <name>`).
- Delete `~/.cache/monolith/networks/<name>/` entirely to remove a network (or use `monolith network rm <name>`).

## Gotchas

- First kata VM downloads ~65 MB from `ghcr.io`; first alpine VM downloads ~60 MB ISO + ~4 MB minirootfs from `dl-cdn.alpinelinux.org`. Later runs use the cache.
- `vm list` treats a directory as a VM if it has `config.json` or `disk.img` (`vm.Exists`); kata VMs have no `disk.img`.
- No guest acts on Virtualization.framework's stop request, so `vm stop` (and `docker stop`) always ends in the forced stop after 10s (the CLI says so). For the docker host that means containers are killed without a clean dockerd shutdown; ext4 journaling recovers.
- **Suspend/resume needs the Mac unlocked to restore.** Virtualization.framework encrypts the snapshot with a Secure Enclave key: saving only uses the public half and works on a locked Mac, but restoring decrypts with the private half, which AppleKeyStore refuses once the user keybag locks (shortly after the screen locks; `loginwindow` logs `lockKeybagForScreenLock`). VZ then fails with error 12 "permission denied" (the VZ XPC service logs `SecKeyCreateDecryptedDataWithParameters failed ... -25308`, AKS `-536870174`). This was the "intermittent" failure: it tracked whether the screen was locked, not rebuilds or code signing. `SpawnVMDaemon` refuses to restore while `keybagLocked()` and the daemon adds the same hint if the lock races it; the snapshot is kept, so unlock and run `vm resume` again. A restore also needs the persisted machine identifier and MAC (see Per-VM identity). `vm stop <name>` discards a snapshot.
- `vmlinuz-virt` is an EFI stub — `decompressKernel()` in `vm/alpine_kernel.go` extracts the gzip-compressed raw ARM64 Image that `VZLinuxBootLoader` requires.
- go-diskfs `recalculateBlocksize()` always picks 1024-byte blocks regardless of disk size. We call `ext4.Create()` directly with `SectorsPerBlock: 8` (4096-byte blocks) to avoid a journal allocation bug.
- go-diskfs ext4 does not support hardlinks (`Link()` returns `ErrNotImplemented`). Tar hardlinks are handled by copying the target file's content.
- go-diskfs `OpenFile` with `O_CREATE|O_RDWR` does not truncate existing files. `configureVM` uses `Truncate()` after writing.
- Named network daemons and VMs must be the same build (CDHash peer requirement). Restart networks after rebuilding.
- On this dev machine, vmnet modes (nat, shared) have no outbound internet: DHCP and guest→host TCP to `192.168.64.1` work, but the Mac drops traffic it forwards for the guest (to the LAN router and beyond), and vmnet's DNS fails. IP forwarding is on; the likely cause is the CrowdStrike Falcon network filter. `user` mode avoids it because the daemon process opens the connections itself. Not a monolith bug.
- gvisor-tap-vsock v0.9.0 requires Go 1.26 (go.mod `go 1.26.0`). It logs through logrus at warn level; one "use of closed network connection" error at daemon exit is expected.
- No linter, formatter, or CI config exists. No pre-commit hooks.

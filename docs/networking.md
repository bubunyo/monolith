# Networking

monolith supports five network modes that control how a VM connects to the
host and the outside world. The mode is selected with `--network-mode` on
`vm run` and persisted in the VM's `config.json` for subsequent boots.

## Quick Reference

```bash
# Default — NAT (internet access, no inter-VM)
monolith vm run my-vm

# Shared — NAT + inter-VM communication (macOS 26+)
monolith vm run my-vm --network-mode shared

# Shared on a named network (multiple VMs can talk to each other)
monolith network create my-net
monolith vm run vm-1 --network-mode shared --network my-net
monolith vm run vm-2 --network-mode shared --network my-net

# Bridged — VM appears on physical LAN
monolith vm run my-vm --network-mode host --host-interface en0

# Isolated — host-only, no NAT, no inter-VM (macOS 26+)
monolith vm run my-vm --network-mode isolated

# Air-gapped — no network device at all
monolith vm run my-vm --network-mode none
```

## Network Modes

### `nat` (default)

Uses `VZNATNetworkDeviceAttachment`. The VM gets an IP address from the macOS
built-in DHCP server (typically in the `192.168.64.0/24` range). Outbound
traffic is NATed through the host.

| Property | Value |
|----------|-------|
| Internet access | Yes (via host NAT) |
| Host → VM | No (NAT, no port forwarding) |
| VM → Host | Yes |
| Inter-VM | No |
| macOS version | 11+ |
| Entitlement | `com.apple.security.virtualization` |
| IP discovery | macOS DHCP leases (`/var/db/dhcpd_leases`) |
| Per-VM stats | Yes (via `vmenet<N>` interface) |

Each VM gets a dedicated `vmenet<N>` virtual interface on the host, which
the Virtualization framework creates automatically. This interface is used
for per-VM network statistics in `vm top` and `vm list --wide`.

### `shared` (macOS 26+)

Uses `vz.NewVMNetNetworkDeviceAttachment` with `vmnet.ModeShared`. Similar to NAT
but with the ability for VMs sharing the same `vmnet.Network` instance to
communicate with each other.

| Property | Value |
|----------|-------|
| Internet access | Yes (via host NAT) |
| Host → VM | Configurable (DHCP reservations, port forwarding) |
| VM → Host | Yes |
| Inter-VM | Yes (when on the same named network) |
| macOS version | 26+ |
| Entitlement | `com.apple.security.virtualization` |
| IP discovery | macOS DHCP leases |
| Per-VM stats | Yes (via `vmenet<N>`) |

Without `--network`, each `vm run` creates its own `vmnet.Network` instance.
To enable inter-VM communication, create a named network and have VMs join it:

```bash
monolith network create my-net                                # shared by default
monolith vm run web --network-mode shared --network my-net
monolith vm run db  --network-mode shared --network my-net
```

The VMs `web` and `db` now share a vmnet network and can reach each other by
IP. See [Named Networks](#named-networks) below.

### `host` (bridged)

Uses `VZBridgedNetworkDeviceAttachment`. The VM is bridged directly to a
physical network interface on the host. The VM appears as a separate device
on the LAN and gets an IP from the router's DHCP server.

| Property | Value |
|----------|-------|
| Internet access | Yes (directly on LAN) |
| Host → VM | Yes (LAN IP) |
| VM → Host | Yes |
| Inter-VM | Yes (via LAN) |
| macOS version | 11+ |
| Entitlement | `com.apple.vm.networking` (restricted) |
| IP discovery | DHCP leases + ARP table fallback |
| Per-VM stats | No (no dedicated vmenet interface) |

**Requires the `--host-interface` flag** to specify which physical NIC to
bridge to (e.g. `en0` for Wi-Fi, `en6` for USB Ethernet).

```bash
monolith vm run my-vm --network-mode host --host-interface en0
```

> **Entitlement restriction (read this before trying host mode):**
>
> Using `VZBridgedNetworkDeviceAttachment` requires the
> `com.apple.vm.networking` entitlement. This is a **restricted entitlement**
> in Apple's terminology — simply adding it to `entitlements.plist` is not
> enough. macOS validates at runtime that the binary's signature was produced
> with a provisioning profile that authorizes the entitlement; otherwise the
> VM fails to start with:
>
> ```
> Error Domain=VZErrorDomain Code=2
> Using VZBridgedNetworkDeviceAttachment in a process that lacks the
> "com.apple.vm.networking" entitlement.
> ```
>
> There are exactly two ways to satisfy this requirement:
>
> 1. **Provisioning profile from Apple.** Register an App ID in the Apple
>    Developer portal with the Virtualization → Bridged Networking capability,
>    download the provisioning profile, and embed it during signing. As of
>    this writing Apple does not grant this capability automatically — you
>    must request it from Apple, and approval is uncommon for personal
>    accounts. Organization accounts with documented use cases have a better
>    chance.
>
> 2. **Disable System Integrity Protection (SIP).** With SIP off, macOS skips
>    the provisioning-profile check and accepts the entitlement based purely
>    on the signature (ad-hoc or Developer ID). Run `csrutil disable` from
>    Recovery Mode to do this. **Strongly discouraged on any machine you care
>    about**, and explicitly forbidden on most managed/MDM-enrolled Macs
>    (the org's MDM profile prevents Recovery Mode changes, and EDR tools
>    like CrowdStrike will flag/refuse to run with SIP disabled).
>
> The default `entitlements.plist` does not include
> `com.apple.vm.networking` because shipping it would be misleading: the
> entitlement is silently ignored at runtime without one of the two
> conditions above. The CLI checks for the entitlement before spawning the
> VM daemon and fails with a clear error message rather than letting the
> daemon crash. See [docs/codesigning.md](codesigning.md) for the signing
> mechanics.
>
> **If you're on a managed Mac, host mode is effectively unavailable. Use
> `nat` (default), `shared`, or `isolated` instead.** All three give the VM
> internet access (except `isolated`) and work without any restricted
> entitlements.

### `isolated` (macOS 26+)

Uses `vz.NewVMNetNetworkDeviceAttachment` with `vmnet.ModeHost`. The VM can
communicate with the host but has no NAT and no internet access.

| Property | Value |
|----------|-------|
| Internet access | No |
| Host → VM | Yes |
| VM → Host | Yes |
| Inter-VM | No (unless on the same named network) |
| macOS version | 26+ |
| Entitlement | `com.apple.security.virtualization` |
| IP discovery | Not applicable (no DHCP) |
| Per-VM stats | Yes (via `vmenet<N>`) |

Useful for security-sensitive workloads or testing without network access.

### `none` (air-gapped)

No network device is attached to the VM. The guest has no `eth0` interface.

| Property | Value |
|----------|-------|
| Internet access | No |
| Host → VM | No |
| VM → Host | No |
| Inter-VM | No |
| macOS version | 11+ |
| Entitlement | `com.apple.security.virtualization` |
| IP discovery | Not applicable |
| Per-VM stats | Not applicable |

## CLI Flags

All networking flags are on the `vm run` command:

| Flag | Short | Description |
|------|-------|-------------|
| `--network-mode` | | One of: `nat`, `shared`, `host`, `isolated`, `none`. Default: `nat`. |
| `--network` | `-n` | Named network to join (shared/isolated modes only). |
| `--host-interface` | | Physical NIC for host mode (e.g. `en0`). Required when mode is `host`. |

Flags are persisted in the VM's `config.json`. On subsequent boots, the
saved values are used unless explicitly overridden:

```bash
# First boot — sets network mode to shared
monolith vm run my-vm --network-mode shared

# Second boot — reuses shared mode from config.json
monolith vm run my-vm
```

### Validation Rules

- `--host-interface` is required when `--network-mode host` and rejected for
  all other modes.
- `--network` is only valid with `--network-mode shared` or
  `--network-mode isolated`.

## Named Networks

Named networks allow multiple VMs to share the same underlying vmnet kernel
resource, enabling inter-VM communication for `shared` and `isolated` modes.

### Commands

```bash
monolith network create <name>                    # create (default: shared mode)
monolith network create <name> --mode isolated    # create in isolated mode
monolith network create <name> --subnet 192.168.77.0/24  # custom IPv4 subnet
monolith network list                             # list all networks with status
monolith network rm <name>                        # stop daemon and delete network
```

`network list` (alias `network ls`) shows each network's name, mode, subnet,
daemon state, and which VMs reference it:

```
NAME      MODE     SUBNET              STATE     VMS
my-net    shared   192.168.64.0/24     running   web,db
lab-net   isolated 192.168.65.0/24     running   -
```

### Custom subnets

By default, vmnet picks a subnet from its internal pool (typically
`192.168.64.0/24`, `192.168.65.0/24`, ...). You can pin a network to a
specific subnet with `--subnet`:

```bash
monolith network create dev --subnet 192.168.77.0/24
```

**Rules:**

- IPv4 only. Must be a valid CIDR.
- Prefix length must be between `/16` and `/28` (inclusive).
- Currently restricted to `192.168.0.0/16`. vz v4 no longer enforces this,
  but the other RFC 1918 ranges (`10.0.0.0/8`, `172.16.0.0/12`) haven't been
  verified against vmnet yet; `validateSubnet` in `cli/network/create.go`
  holds the allow list.
- Must not overlap any existing named network's subnet (running or stopped).
  The check uses `netip.Prefix.Overlaps`, so e.g. `192.168.77.128/25` is
  rejected if any other network owns `192.168.77.0/24`.
- Inside the chosen subnet, vmnet reserves `.0` (network), `.1` (host
  gateway), and `.255` (broadcast). Everything else is available for VMs
  via DHCP.

The requested CIDR is persisted as `user_subnet` in `config.json` and
re-applied on every daemon start (`network start`), so a custom subnet
survives restarts.

If you omit `--subnet`, vmnet chooses for you and the chosen subnet is
recorded in `config.json` under `subnet` after the daemon comes up.

### Topology view

`network list --topology` (`-t`) renders a unified tree showing every VM
and every network grouped by **reachability** rather than just listing
named networks. It's the fastest way to answer "what can talk to what?"

```bash
monolith network list -t
monolith network ls -t
```

The tree has up to three top-level sections — **Internet**, **Host-only**,
and **Airgapped** — and only includes sections that contain something:

```
Internet
├── Bridged (physical NICs)
│   └── en0
│       └── lith-41530       stopped       -
├── NAT
│   ├── my-vm                running       192.168.64.26
│   └── my-vm-2              stopped       -
└── Shared networks
    ├── debug                stopped       192.168.71.1/24
    ├── shared-net-1         running       192.168.66.1/24
    │   ├── lith-20584       running       192.168.66.11
    │   ├── lith-24485       running       192.168.66.10
    │   └── lith-48756       running       192.168.66.9
    ├── test-3               stopped       192.168.64.1/24
    ├── test-network-2       stopped       192.168.65.1/24
    └── (anonymous)
        └── lith-69278       running       192.168.65.2

Host-only
└── Isolated networks
    └── (anonymous)
        └── lith-30507       running       -

Airgapped
└── lith-42593               running       -
```

Grouping rules:

- **Internet** contains everything that can reach the outside world: bridged
  (`host` mode) VMs grouped by physical NIC, anonymous `nat` VMs, and
  shared networks (named + an `(anonymous)` pseudo-group for `shared` VMs
  started without `--network`).
- **Host-only** contains `isolated` mode networks/VMs (host can reach them,
  no internet, no inter-VM communication unless they share a named network).
- **Airgapped** contains VMs started with `--network-mode none`.
- VMs grouped under the same named network are real layer-2 peers (sharing
  one `vmnet.Network` instance via the daemon). VMs in `(anonymous)` groups
  each have their own private vmnet instance and cannot see each other.
- Stopped VMs and unused named networks both appear in their assigned slots
  so the tree reflects the configured topology, not just live state.
- A VM that references a named network that has been deleted appears under
  a `(missing: <name>)` pseudo-network so it's not silently dropped.

### Architecture

The vmnet network must be kept alive by a process for as long as it exists,
and vz v4 can only hand a network to another process through a **launchd
Mach service** (`vmnet.ShareNetwork` / `vmnet.ReceiveNetwork`). The
serialization is an opaque XPC object with no byte encoding, so it can't be
sent over a plain socket. Each named network therefore runs as a per-user
launchd job.

```
        ┌──────────────────────────────────────────────┐
        │  monolith network create|start my-net        │
        │    1. write networks/my-net/launchd.plist    │
        │    2. launchctl bootstrap gui/<uid> <plist>  │
        └────────────┬─────────────────────────────────┘
                     │ launchd runs the job
                     ▼
        ┌──────────────────────────────────────────────┐
        │  monolith __netd my-net shared               │
        │    1. vmnet.NewNetwork(ModeShared, subnet)   │
        │    2. vmnet.ShareNetwork(                    │
        │         "dev.monolith.network.my-net", ...)  │
        │    3. write pid file                         │
        │    4. block until SIGTERM                    │
        └────────────┬─────────────────────────────────┘
                     │ XPC (Mach service)
                     ▼
        ┌──────────────────────────────────────────────┐
        │  monolith __vmd web   (--network my-net)     │
        │    1. vmnet.ReceiveNetwork(service, 5s)      │
        │    2. vz.NewVMNetNetworkDeviceAttachment()   │
        └──────────────────────────────────────────────┘
```

#### Daemon Details

`monolith network create` validates the name, mode and subnet, writes an
initial `config.json`, then calls `net.StartNetworkDaemon` (`net/launchd.go`),
which:

1. Writes `launchd.plist` in the network directory: label and Mach service
   `dev.monolith.network.<name>`, `ProgramArguments` = this binary +
   `__netd <name> <mode> [--verbose]`, `RunAtLoad`, `HOME`, and stdout/stderr
   to `daemon.log`.
2. Boots out any stale job with that label, then runs
   `launchctl bootstrap gui/<uid> <plist>`.

The CLI then waits up to 10s for the daemon's PID file. Nothing is
installed in `~/Library/LaunchAgents`, so jobs don't survive logout.

The hidden `__netd` command calls `monolith.RunNetworkDaemon`, which:

1. Creates a `vmnet.Network`, applying `user_subnet` with `vmnet.WithIPv4Subnet`.
2. Writes the assigned IPv4 subnet to `config.json`.
3. Publishes the network with `vmnet.ShareNetwork` on its Mach service.
4. Writes a PID file (what `IsNetworkDaemonAlive` checks).
5. With `--verbose`, starts a monitor interface on the network and logs
   decoded frames at TRC level.
6. Blocks until `SIGTERM` or `SIGINT`.

`network stop` and `network rm` call `monolith.StopNetworkDaemon`, which runs
`launchctl bootout gui/<uid>/dev.monolith.network.<name>`. That SIGTERMs the
daemon and unregisters the service. Killing the daemon process directly
leaves the job loaded until the next `network start` boots it out.

#### Peer Authentication

`ShareNetwork` and `ReceiveNetwork` each require an Apple code signing
requirement for the peer. monolith uses `cdhash H"<hash>"` of its own binary
(`OwnCodeRequirement()` in `entitlements.go`), so the network daemon only
serves VMs from the exact same build, and VMs only trust that daemon.

**Rebuilding or re-signing the binary changes its hash.** After `make build`,
restart running networks, or VMs will time out joining them:

```bash
monolith network stop my-net && monolith network start my-net
```

### Filesystem Layout

```
~/.cache/monolith/networks/
  my-net/
    config.json          # {"mode":"shared","subnet":"192.168.77.0/24","user_subnet":"192.168.77.0/24"}
    pid                  # daemon PID (plain text)
    launchd.plist        # launchd job definition
    daemon.log           # daemon stdout/stderr
```

### Lifecycle

| Action | What Happens |
|--------|-------------|
| `network create` | launchd job loaded, daemon starts, vmnet resource allocated and shared |
| `vm run --network` | VM receives the network over XPC and attaches to it |
| `vm stop` / VM shutdown | VM disconnects, daemon keeps running |
| `network stop` | `launchctl bootout`: daemon stopped, config kept |
| `network rm` | `launchctl bootout`, then removes the directory |
| Host reboot | Daemon dies, stale PID file cleaned up on next `network list` |

## Guest-Side Networking

Inside the VM, networking is configured by the custom `/etc/inittab`:

```bash
# Load af_packet module (required for DHCP raw sockets)
::sysinit:/sbin/modprobe af_packet

# Bring up loopback
::sysinit:/sbin/ifconfig lo 127.0.0.1 up

# Bring up eth0 and request DHCP lease
::sysinit:/sbin/ifconfig eth0 up
::sysinit:/sbin/udhcpc -i eth0 -q -n -f
```

The kernel modules required for networking (`af_packet`, `virtio_net`) come
from the modloop squashfs image, which is mounted during early init before
the network setup runs.

**`udhcpc` flags:**
- `-i eth0` — interface to configure
- `-q` — quit after obtaining a lease (don't background)
- `-n` — exit with failure if no lease obtained (non-blocking)
- `-f` — run in foreground

If `--network-mode none` is used, the `eth0` interface doesn't exist and
`ifconfig`/`udhcpc` print harmless errors that can be ignored.

## IP Discovery

monolith discovers VM IP addresses on the host side for display in `vm list`
and `vm top`. The discovery strategy depends on the network mode:

| Mode | Strategy |
|------|----------|
| `nat`, `shared` | Parse `/var/db/dhcpd_leases` (macOS DHCP server) |
| `host` | DHCP leases first, then `arp -an` fallback |
| `isolated`, `none` | No IP expected (returns empty) |

### DHCP Leases

The macOS built-in DHCP server writes leases to `/var/db/dhcpd_leases`:

```
{
    ip_address=192.168.64.14
    hw_address=1,ce:b7:d1:c3:89:1d
    identifier=1,ce:b7:d1:c3:89:1d
    lease=0x69db5aec
}
```

The `hw_address` field is prefixed with `1,` (Ethernet type) and uses
shortened MAC octets (e.g. `e:3d:be` instead of `0e:3d:be`). The lookup
code normalizes both sides before comparing.

### ARP Fallback

For host (bridged) mode, the VM gets an IP from the physical router, which
may not appear in the macOS DHCP leases file. As a fallback, monolith
parses the system ARP table via `arp -an` to match the VM's MAC address
to an IP.

## Per-VM Network Statistics

For modes that create a dedicated `vmenet<N>` host interface (nat, shared,
isolated), monolith can report per-VM network counters.

### How It Works

1. The VM's MAC address is stored in `state.json` at boot time.
2. `VmenetForMAC(mac)` parses `ifconfig bridge100` output to find the
   `vmenet<N>` interface associated with the VM's MAC in the bridge address
   cache.
3. `NetworkStats(iface)` runs `netstat -I vmenet<N> -b` and parses the
   byte/packet counters.

### Byte Counter Swap

From the **host's** perspective:
- `Ibytes` = bytes received from the guest (guest's TX)
- `Obytes` = bytes sent to the guest (guest's RX)

monolith swaps these so the displayed values are from the **guest's**
perspective: RX is what the guest received, TX is what the guest sent.

### Limitations

- **Host mode:** No dedicated `vmenet<N>` interface is created. The VM
  traffic goes directly through the bridged physical NIC, so per-VM
  statistics are not available.
- **None mode:** No network device, so no statistics.

## Configuration Persistence

Network settings are stored in the per-VM `config.json`:

```json
{
  "cpus": 2,
  "memory": 1073741824,
  "network_mode": "shared",
  "network_name": "my-net",
  "host_interface": ""
}
```

| Field | JSON Key | Description |
|-------|----------|-------------|
| `NetworkMode` | `network_mode` | One of: nat, shared, host, isolated, none |
| `NetworkName` | `network_name` | Named network to join (omitted if empty) |
| `HostInterface` | `host_interface` | Physical NIC for host mode (omitted if empty) |

On boot, `ReadConfig()` loads saved values. CLI flags override them only
when explicitly passed. The merged config is written back before the VM
starts, so `vm list` always reflects the active configuration.

## Entitlements

| Entitlement | Modes | Signing |
|-------------|-------|---------|
| `com.apple.security.virtualization` | nat, shared, isolated, none | Apple Development certificate |
| `com.apple.vm.networking` | host (bridged) | **Restricted** — provisioning profile from Apple, **or** SIP disabled. See the host-mode section above. |

The default build only includes `com.apple.security.virtualization`. Host
mode requires additional setup that is generally not feasible on
managed/MDM-enrolled Macs — see the host-mode section above and
[docs/codesigning.md](codesigning.md).

## Troubleshooting

### VM boots but `eth0` not found

The VM was started with `--network-mode none`. No network device is attached.
Restart with a different mode:

```bash
monolith vm run my-vm --network-mode nat
```

### VM has no internet (shared/isolated mode)

Shared and isolated modes require macOS 26+. On older macOS versions, the
vmnet APIs are not available and the VM will fail to start.

### `vm run --network-mode host` fails immediately

The CLI refuses to start a host-mode VM if the binary is missing the
`com.apple.vm.networking` entitlement, with a clear error message pointing
here. See the [host mode section above](#host-bridged) for the two ways to
satisfy the entitlement requirement and why neither is realistic on a
managed Mac. Use `nat` (default), `shared`, or `isolated` instead.

### Ping (ICMP) doesn't work but TCP/HTTP does

This is almost always a host-side firewall or EDR (endpoint detection &
response) tool dropping ICMP from the vmnet NAT subnet (`192.168.64.0/24`
by default). Common culprits on managed Macs: CrowdStrike Falcon, Cisco
Umbrella, Zscaler, Netskope, the macOS Application Firewall.

Diagnose by trying TCP from inside the VM:

```sh
wget -qO- https://1.1.1.1 >/dev/null && echo OK
```

If TCP works, networking is fine — your host security stack is just
filtering ICMP. There is nothing monolith can do about this; either
request an exception from your IT team, or use TCP-based reachability
tests (`nc -zv`, `wget`, `curl`) instead of `ping`.

### `vm list` shows no IP for a running VM

IP discovery takes a moment after boot. The DHCP lease may not have been
written yet. Wait a few seconds and try again. For host mode, the router's
DHCP server may be slow; the ARP fallback also requires the VM to have sent
at least one packet.

### Named network daemon fails to start

Check the daemon log:

```bash
cat ~/.cache/monolith/networks/<name>/daemon.log
```

Common causes:
- macOS version too old (shared/isolated require macOS 26+)
- Another daemon is already running for this network name
- Insufficient permissions

### `network create --subnet` rejected as out-of-range

monolith currently only accepts subnets within `192.168.0.0/16`. Other
RFC 1918 ranges (`10.0.0.0/8`, `172.16.0.0/12`) haven't been verified with
vmnet yet. For now, pick a subnet inside `192.168.x.x`.

### `network create --subnet` rejected as overlapping

The requested CIDR overlaps an existing named network (running or stopped).
The error message names the conflicting network. Either choose a
non-overlapping subnet, or `monolith network rm <conflict>` first.

### Inter-VM communication doesn't work

VMs must be on the **same named network** to communicate. Two VMs both
using `--network-mode shared` but without `--network` each create their
own independent vmnet.Network and cannot see each other.

```bash
# Wrong — two independent networks
monolith vm run vm-1 --network-mode shared
monolith vm run vm-2 --network-mode shared

# Right — shared named network
monolith network create my-net
monolith vm run vm-1 --network-mode shared --network my-net
monolith vm run vm-2 --network-mode shared --network my-net
```

### `network rm` says VMs still reference the network

Stop the VMs first, or reconfigure them to use a different network mode:

```bash
# Option 1: stop the VMs
# (kill the monolith vm run process or send SIGTERM)

# Option 2: change the VM's config (delete and recreate)
monolith vm rm my-vm
```

### Host mode SIGKILL on launch

The binary is signed with `com.apple.vm.networking` but lacks a provisioning
profile. macOS kills the process immediately. Either:

1. Remove `com.apple.vm.networking` from `entitlements.plist` (lose host mode)
2. Disable SIP (not recommended for production)
3. Create a provisioning profile via the Apple Developer portal

See [docs/codesigning.md](codesigning.md) for instructions.

## Implementation Reference

| File | Purpose |
|------|---------|
| `net/modes.go` | Network mode constants, `ValidNetworkModes` |
| `net/network.go` | `ConfigureNetwork()`, `createNetworkAttachment()` |
| `net/netd.go` | `RunNetworkDaemon()` (ShareNetwork), `joinNamedNetwork()` (ReceiveNetwork), persistence helpers, packet monitor |
| `net/launchd.go` | `StartNetworkDaemon()`, `StopNetworkDaemon()`, plist rendering, service naming |
| `net/ip.go` | `LookupIPForMode()`, `LookupIPByMAC()`, `VmenetForMAC()`, `NetworkStats()` |
| `vm/config.go` | `VMConfig` (persists `network_mode`, `network_name`, `host_interface`) |
| `cli/vm/run.go` | `--network-mode`, `--network`, `--host-interface` flags and validation |
| `cli/vm/list.go` | NET column display (`mode` or `mode:name`) |
| `cli/network/create.go` | Subnet validation, network creation |
| `cli/network/start.go` | `spawnDaemon()`: load launchd job, wait for daemon |
| `cli/network/list.go` | Network listing with VM cross-reference |
| `cli/network/rm.go` | Daemon termination and cleanup |
| `cli/netd.go` | Hidden `__netd` command for daemon process |
| `vm/embeddings/etc/inittab.tmpl` | Guest-side network init (modprobe, ifconfig, udhcpc) |
| `entitlements.plist` | Code signing entitlements |

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

Uses `VZVmnetNetworkDeviceAttachment` with `vmnet.SharedMode`. Similar to NAT
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

> **Entitlement restriction:** The `com.apple.vm.networking` entitlement is
> restricted by Apple and requires either a provisioning profile from the
> Apple Developer portal or SIP disabled. The default `entitlements.plist`
> does not include it. See [docs/codesigning.md](codesigning.md) for details.

### `isolated` (macOS 26+)

Uses `VZVmnetNetworkDeviceAttachment` with `vmnet.HostMode`. The VM can
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

### Architecture

Named networks use a **daemon process** architecture. The vmnet kernel
resource must be kept alive by a process for as long as the network exists.

```
                ┌─────────────────────────────────────────┐
                │  monolith network create my-net         │
                │    └─ spawns background daemon          │
                └────────────┬────────────────────────────┘
                             │ os/exec
                             ▼
        ┌─────────────────────────────────────────────┐
        │  monolith __netd my-net shared              │
        │    1. vmnet.NewNetwork(SharedMode)           │
        │    2. network.CopySerialization() → bytes    │
        │    3. Listen on unix socket                  │
        │    4. Serve serialization to clients         │
        │    5. Block until SIGTERM                    │
        └────────┬────────────────────────────────────┘
                 │ unix socket
                 ▼
        ┌─────────────────────────────────────────────┐
        │  monolith vm run web --network my-net       │
        │    1. Connect to daemon socket              │
        │    2. Receive serialized network bytes      │
        │    3. vmnet.NewNetworkWithSerialization()    │
        │    4. vz.NewVmnetNetworkDeviceAttachment()  │
        └─────────────────────────────────────────────┘
```

#### Daemon Details

The `monolith network create` command:

1. Validates the name and mode.
2. Writes an initial `config.json` to `~/.cache/monolith/networks/<name>/`.
3. Spawns a detached background process: `monolith __netd <name> <mode>`.
4. Waits 500ms, then verifies the daemon is alive.

The hidden `__netd` command (defined in `cli/netd.go`) calls
`monolith.RunNetworkDaemon(name, mode)`, which:

1. Creates a `vmnet.Network` with the requested mode.
2. Queries the assigned IPv4 subnet and updates `config.json`.
3. Serializes the network via `network.CopySerialization()`.
4. Extracts the raw bytes from the XPC dictionary.
5. Writes a PID file.
6. Listens on a unix domain socket at `~/.cache/monolith/networks/<name>/daemon.sock`.
7. Serves the serialization bytes to each connecting VM.
8. Blocks until `SIGTERM` or `SIGINT`.

#### Serialization Protocol

The daemon communicates with VMs over a unix socket using a simple
length-prefixed binary protocol:

```
┌──────────────┬──────────────────────────────┐
│ 4 bytes      │ N bytes                      │
│ uint32 BE    │ raw serialization data       │
│ (length = N) │                              │
└──────────────┴──────────────────────────────┘
```

- **4 bytes:** Big-endian `uint32` — length of the serialization payload.
- **N bytes:** Raw bytes extracted from the XPC dictionary's
  `"networkSerialization"` key.

On the client side (VM process), the bytes are reconstructed into an XPC
dictionary and passed to `vmnet.NewNetworkWithSerialization()`.

#### Same-Executable Constraint

The macOS `vmnet_network_create_with_serialization` API requires the
deserializing process to be **the same executable** as the serializing
process. On macOS <26.2 this is enforced by CDHash comparison; on macOS
26.2+ it's enforced by executable path comparison.

This is why both the daemon (`monolith __netd`) and the VM (`monolith vm run`)
use the same `monolith` binary. Building a separate daemon binary would break
network serialization.

### Filesystem Layout

```
~/.cache/monolith/networks/
  my-net/
    config.json          # {"mode":"shared","subnet":"192.168.64.0/24"}
    pid                  # daemon PID (plain text)
    daemon.sock          # unix domain socket
    daemon.log           # daemon stdout/stderr
```

### Lifecycle

| Action | What Happens |
|--------|-------------|
| `network create` | Daemon starts, vmnet resource allocated, socket created |
| `vm run --network` | VM connects to socket, receives serialization, joins network |
| `vm stop` / VM shutdown | VM disconnects, daemon keeps running |
| `network rm` | Sends SIGTERM to daemon, removes directory |
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
| `com.apple.vm.networking` | host (bridged) | Provisioning profile or SIP disabled |

The default build only includes `com.apple.security.virtualization`. Host
mode requires additional setup — see [docs/codesigning.md](codesigning.md).

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
| `config.go` | `VMConfig` struct, network mode constants, read/write config |
| `network.go` | `configureNetwork()`, `createNetworkAttachment()`, `joinNamedNetwork()` |
| `netd.go` | `RunNetworkDaemon()`, `FetchNetworkSerialization()`, network persistence helpers |
| `state.go` | `lookupIPForMode()`, `LookupIPByMAC()`, `VmenetForMAC()`, `NetworkStats()` |
| `cli/vm/run.go` | `--network-mode`, `--network`, `--host-interface` flags and validation |
| `cli/vm/list.go` | NET column display (`mode` or `mode:name`) |
| `cli/network/create.go` | Daemon spawning logic |
| `cli/network/list.go` | Network listing with VM cross-reference |
| `cli/network/rm.go` | Daemon termination and cleanup |
| `cli/netd.go` | Hidden `__netd` command for daemon process |
| `osfiles/etc/inittab.tmpl` | Guest-side network init (modprobe, ifconfig, udhcpc) |
| `entitlements.plist` | Code signing entitlements |

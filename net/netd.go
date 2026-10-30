package net

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/bubunyo/monolith"

	"github.com/Code-Hex/vz/v4/vmnet"
)

const (
	networkConfigFileName = "config.json"
	networkPidFileName    = "pid"
	networkLogFileName    = "daemon.log"
)

// NetworkConfig holds the persisted configuration for a named network.
type NetworkConfig struct {
	Mode       string `json:"mode"`                  // "shared" or "isolated"
	Subnet     string `json:"subnet"`                // actual IPv4 subnet allocated by vmnet (e.g. "192.168.64.0/24"), filled after creation
	UserSubnet string `json:"user_subnet,omitempty"` // caller-requested IPv4 subnet (CIDR); empty means vmnet picks
}

// ReadNetworkConfig reads a named network's configuration.
func ReadNetworkConfig(name string) (NetworkConfig, error) {
	dir, err := NetworkDir(name)
	if err != nil {
		return NetworkConfig{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, networkConfigFileName))
	if err != nil {
		return NetworkConfig{}, err
	}
	var cfg NetworkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return NetworkConfig{}, err
	}
	return cfg, nil
}

// WriteNetworkConfig writes a named network's configuration.
func WriteNetworkConfig(name string, cfg NetworkConfig) error {
	dir, err := NetworkDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, networkConfigFileName), data, 0644)
}

// ReadNetworkPid reads the daemon PID for a named network.
// Returns 0 if the PID file doesn't exist.
func ReadNetworkPid(name string) (int, error) {
	dir, err := NetworkDir(name)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(filepath.Join(dir, networkPidFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

// WriteNetworkPid writes the daemon PID file.
func WriteNetworkPid(name string, pid int) error {
	dir, err := NetworkDir(name)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, networkPidFileName), []byte(strconv.Itoa(pid)), 0644)
}

// CleanNetworkPid removes the PID file.
func CleanNetworkPid(name string) {
	dir, err := NetworkDir(name)
	if err != nil {
		return
	}
	os.Remove(filepath.Join(dir, networkPidFileName))
}

// IsNetworkDaemonAlive checks if the daemon for the given network is running.
func IsNetworkDaemonAlive(name string) bool {
	pid, err := ReadNetworkPid(name)
	if err != nil || pid <= 0 {
		return false
	}
	return monolith.ProcessAlive(pid)
}

// RunNetworkDaemon is the main loop for the network daemon process. It is
// started by launchd (see StartNetworkDaemon), creates the vmnet network,
// publishes it on the network's Mach service with vmnet.ShareNetwork, and
// keeps it alive until it receives SIGTERM or SIGINT. VM daemons join the
// network with joinNamedNetwork.
func RunNetworkDaemon(name, mode string, verbose bool) error {
	signal.Ignore(syscall.SIGPIPE)

	logger := monolith.NewLogger(os.Stderr)

	var vmnetMode vmnet.Mode
	switch mode {
	case NetworkShared:
		vmnetMode = vmnet.ModeShared
	case NetworkIsolated:
		vmnetMode = vmnet.ModeHost
	default:
		return fmt.Errorf("named networks only support shared or isolated modes, got %q", mode)
	}

	// Preserve a caller-requested subnet across restarts.
	existing, _ := ReadNetworkConfig(name)

	var opts []vmnet.NetworkConfigurationOption
	if existing.UserSubnet != "" {
		prefix, err := netip.ParsePrefix(existing.UserSubnet)
		if err != nil {
			return fmt.Errorf("parsing user_subnet %q: %w", existing.UserSubnet, err)
		}
		opts = append(opts, vmnet.WithIPv4Subnet(prefix))
		logger.Info("requesting subnet %s", existing.UserSubnet)
	}

	config, err := vmnet.NewNetworkConfiguration(vmnetMode, opts...)
	if err != nil {
		return fmt.Errorf("creating vmnet configuration: %w", err)
	}

	network, err := vmnet.NewNetwork(config)
	if err != nil {
		if existing.UserSubnet != "" {
			return fmt.Errorf("creating vmnet network with subnet %s: %w (common cause: overlap with another active vmnet network, or out-of-range CIDR)", existing.UserSubnet, err)
		}
		return fmt.Errorf("creating vmnet network: %w", err)
	}

	subnet, err := network.IPv4Subnet()
	if err != nil {
		logger.Warn("could not get IPv4 subnet: %v", err)
	}

	netCfg := NetworkConfig{
		Mode:       mode,
		Subnet:     subnet.String(),
		UserSubnet: existing.UserSubnet,
	}
	if err := WriteNetworkConfig(name, netCfg); err != nil {
		return fmt.Errorf("writing network config: %w", err)
	}

	requirement, err := monolith.OwnCodeRequirement()
	if err != nil {
		return fmt.Errorf("computing code requirement for clients: %w", err)
	}

	service, err := vmnet.ShareNetwork(NetworkServiceName(name), network, requirement)
	if err != nil {
		return fmt.Errorf("sharing network on %s: %w", NetworkServiceName(name), err)
	}
	defer service.Close()

	if err := WriteNetworkPid(name, os.Getpid()); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer CleanNetworkPid(name)

	logger.Info("network daemon %q started (mode=%s, subnet=%s, pid=%d, service=%s, verbose=%t)",
		name, mode, subnet.String(), os.Getpid(), NetworkServiceName(name), verbose)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if verbose {
		iface, err := vmnet.StartInterfaceWithNetwork(network, vmnet.WithInterfaceMACAllocation(false))
		if err != nil {
			logger.Warn("could not start monitor interface: %v", err)
		} else {
			defer iface.Stop()
			logger.Info("monitor interface started (mtu=%d, max_pkt=%d)", iface.MTU(), iface.MaxPacketSize())
			go monitorPackets(ctx, logger, name, iface)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	logger.Info("network daemon %q received %v, shutting down", name, sig)
	return nil
}

// joinNamedNetwork obtains a reference to the named network from its daemon
// over launchd/XPC. Both sides require the peer to be this same binary.
func joinNamedNetwork(name string) (*vmnet.Network, error) {
	if !IsNetworkDaemonAlive(name) {
		return nil, fmt.Errorf("network daemon for %q is not running (start it with 'monolith network start %s')", name, name)
	}

	requirement, err := monolith.OwnCodeRequirement()
	if err != nil {
		return nil, fmt.Errorf("computing code requirement for network daemon: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), networkReceiveTimeout)
	defer cancel()

	network, err := vmnet.ReceiveNetwork(ctx, NetworkServiceName(name), requirement)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("no answer from network daemon %q within %s; if monolith was rebuilt since the network started, restart it with 'monolith network stop %s && monolith network start %s'", name, networkReceiveTimeout, name, name)
		}
		return nil, fmt.Errorf("receiving network %q: %w", name, err)
	}
	return network, nil
}

// monitorPackets reads frames from a monitor interface on the network and
// logs them at TRC level until ctx is cancelled.
func monitorPackets(ctx context.Context, logger *monolith.Logger, networkName string, iface *vmnet.Interface) {
	frames := make([][]byte, 32)
	for i := range frames {
		frames[i] = make([]byte, iface.MaxPacketSize())
	}
	for {
		n, err := iface.ReadPackets(ctx, frames)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("[%s] monitor read error: %v", networkName, err)
			}
			return
		}
		for _, frame := range frames[:n] {
			logger.Trace("[%s] pkt size=%d", networkName, len(frame))
			logEthernetFrame(logger, networkName, frame)
		}
	}
}

// logEthernetFrame parses and logs an Ethernet frame.
func logEthernetFrame(logger *monolith.Logger, networkName string, frame []byte) {
	if len(frame) < 14 {
		return
	}

	dstMAC := formatMAC(frame[0:6])
	srcMAC := formatMAC(frame[6:12])
	etherType := binary.BigEndian.Uint16(frame[12:14])
	payload := frame[14:]

	switch etherType {
	case 0x0806: // ARP
		logARP(logger, networkName, srcMAC, dstMAC, payload)
	case 0x0800: // IPv4
		logIPv4(logger, networkName, srcMAC, dstMAC, payload)
	case 0x86DD: // IPv6
		if len(payload) >= 40 {
			srcIP := formatIPv6(payload[8:24])
			dstIP := formatIPv6(payload[24:40])
			nextHdr := payload[6]
			proto := ipv6ProtoName(nextHdr)
			logger.Trace("[%s] IPv6 %s -> %s  %s > %s  proto=%s  len=%d",
				networkName, srcMAC, dstMAC, srcIP, dstIP, proto, len(frame))
		}
	default:
		logger.Trace("[%s] ETH %s -> %s  type=0x%04x  len=%d",
			networkName, srcMAC, dstMAC, etherType, len(frame))
	}
}

// logARP parses and logs an ARP packet.
func logARP(logger *monolith.Logger, networkName, srcMAC, dstMAC string, payload []byte) {
	if len(payload) < 28 {
		logger.Trace("[%s] ARP %s -> %s  (truncated)", networkName, srcMAC, dstMAC)
		return
	}
	op := binary.BigEndian.Uint16(payload[6:8])
	senderIP := fmt.Sprintf("%d.%d.%d.%d", payload[14], payload[15], payload[16], payload[17])
	targetIP := fmt.Sprintf("%d.%d.%d.%d", payload[24], payload[25], payload[26], payload[27])
	senderMAC := formatMAC(payload[8:14])

	opStr := "unknown"
	switch op {
	case 1:
		opStr = "request"
	case 2:
		opStr = "reply"
	}
	logger.Trace("[%s] ARP %s  %s (%s) -> %s  who-has %s",
		networkName, opStr, senderIP, senderMAC, targetIP, targetIP)
}

// logIPv4 parses and logs an IPv4 packet, with special handling for DHCP/UDP.
func logIPv4(logger *monolith.Logger, networkName, srcMAC, dstMAC string, payload []byte) {
	if len(payload) < 20 {
		return
	}
	ihl := int(payload[0]&0x0f) * 4
	proto := payload[9]
	srcIP := fmt.Sprintf("%d.%d.%d.%d", payload[12], payload[13], payload[14], payload[15])
	dstIP := fmt.Sprintf("%d.%d.%d.%d", payload[16], payload[17], payload[18], payload[19])

	if proto == 17 && len(payload) >= ihl+8 { // UDP
		udp := payload[ihl:]
		srcPort := binary.BigEndian.Uint16(udp[0:2])
		dstPort := binary.BigEndian.Uint16(udp[2:4])
		udpPayload := udp[8:]

		// DHCP: client port 68, server port 67
		if (srcPort == 68 && dstPort == 67) || (srcPort == 67 && dstPort == 68) {
			logDHCP(logger, networkName, srcMAC, dstMAC, srcIP, dstIP, srcPort, udpPayload)
			return
		}
		// DNS
		if srcPort == 53 || dstPort == 53 {
			logger.Trace("[%s] DNS %s -> %s  %s:%d > %s:%d  len=%d",
				networkName, srcMAC, dstMAC, srcIP, srcPort, dstIP, dstPort, len(udpPayload))
			return
		}
		logger.Trace("[%s] UDP %s -> %s  %s:%d > %s:%d  len=%d",
			networkName, srcMAC, dstMAC, srcIP, srcPort, dstIP, dstPort, len(udpPayload))
		return
	}

	if proto == 6 { // TCP
		if len(payload) >= ihl+4 {
			tcp := payload[ihl:]
			srcPort := binary.BigEndian.Uint16(tcp[0:2])
			dstPort := binary.BigEndian.Uint16(tcp[2:4])
			logger.Trace("[%s] TCP %s -> %s  %s:%d > %s:%d  len=%d",
				networkName, srcMAC, dstMAC, srcIP, srcPort, dstIP, dstPort, len(payload)-ihl)
			return
		}
	}

	if proto == 1 { // ICMP
		logger.Trace("[%s] ICMP %s -> %s  %s > %s  len=%d",
			networkName, srcMAC, dstMAC, srcIP, dstIP, len(payload)-ihl)
		return
	}

	protoName := fmt.Sprintf("%d", proto)
	switch proto {
	case 6:
		protoName = "TCP"
	case 17:
		protoName = "UDP"
	case 1:
		protoName = "ICMP"
	case 2:
		protoName = "IGMP"
	}
	logger.Trace("[%s] IPv4 %s -> %s  %s > %s  proto=%s  len=%d",
		networkName, srcMAC, dstMAC, srcIP, dstIP, protoName, len(payload))
}

// logDHCP parses and logs a DHCP packet.
func logDHCP(logger *monolith.Logger, networkName, srcMAC, dstMAC, srcIP, dstIP string, srcPort uint16, payload []byte) {
	if len(payload) < 240 {
		logger.Trace("[%s] DHCP %s -> %s  %s > %s  (truncated, %d bytes)",
			networkName, srcMAC, dstMAC, srcIP, dstIP, len(payload))
		return
	}

	op := payload[0]
	opStr := "unknown"
	switch op {
	case 1:
		opStr = "request"
	case 2:
		opStr = "reply"
	}

	xid := binary.BigEndian.Uint32(payload[4:8])
	clientMAC := formatMAC(payload[28:34])
	yourIP := fmt.Sprintf("%d.%d.%d.%d", payload[16], payload[17], payload[18], payload[19])
	serverIP := fmt.Sprintf("%d.%d.%d.%d", payload[20], payload[21], payload[22], payload[23])

	// Parse DHCP message type from options (after 4-byte magic cookie at offset 236).
	msgType := "unknown"
	if len(payload) > 240 && payload[236] == 99 && payload[237] == 130 && payload[238] == 83 && payload[239] == 99 {
		opts := payload[240:]
		for len(opts) > 2 {
			optType := opts[0]
			if optType == 255 { // end
				break
			}
			if optType == 0 { // pad
				opts = opts[1:]
				continue
			}
			optLen := int(opts[1])
			if len(opts) < 2+optLen {
				break
			}
			if optType == 53 && optLen == 1 { // DHCP Message Type
				switch opts[2] {
				case 1:
					msgType = "DISCOVER"
				case 2:
					msgType = "OFFER"
				case 3:
					msgType = "REQUEST"
				case 4:
					msgType = "DECLINE"
				case 5:
					msgType = "ACK"
				case 6:
					msgType = "NAK"
				case 7:
					msgType = "RELEASE"
				case 8:
					msgType = "INFORM"
				}
			}
			opts = opts[2+optLen:]
		}
	}

	logger.Trace("[%s] DHCP %s (%s)  xid=0x%08x  client=%s  your-ip=%s  server-ip=%s  %s > %s  src-mac=%s",
		networkName, msgType, opStr, xid, clientMAC, yourIP, serverIP, srcIP, dstIP, srcMAC)
}

// formatMAC formats 6 bytes as a MAC address string.
func formatMAC(b []byte) string {
	if len(b) < 6 {
		return "??:??:??:??:??:??"
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4], b[5])
}

// formatIPv6 formats 16 bytes as a shortened IPv6 address string.
func formatIPv6(b []byte) string {
	if len(b) < 16 {
		return "?"
	}
	ip := net.IP(b)
	return ip.String()
}

// ipv6ProtoName returns a human-readable name for an IPv6 next header value.
func ipv6ProtoName(proto byte) string {
	switch proto {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	case 58:
		return "ICMPv6"
	case 59:
		return "NoNext"
	default:
		return fmt.Sprintf("%d", proto)
	}
}

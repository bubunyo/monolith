package monolith

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/Code-Hex/vz/v3/vmnet"
	"github.com/Code-Hex/vz/v3/xpc"
)

const (
	networkConfigFileName = "config.json"
	networkPidFileName    = "pid"
	networkSockFileName   = "daemon.sock"
	networkLogFileName    = "daemon.log"
)

// NetworkLogPath returns the path to the network daemon's log file.
func NetworkLogPath(name string) (string, error) {
	dir, err := NetworkDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, networkLogFileName), nil
}

// NetworkConfig holds the persisted configuration for a named network.
type NetworkConfig struct {
	Mode       string `json:"mode"`                  // "shared" or "isolated"
	Subnet     string `json:"subnet"`                // actual IPv4 subnet allocated by vmnet (e.g. "192.168.64.0/24"), filled after creation
	UserSubnet string `json:"user_subnet,omitempty"` // caller-requested IPv4 subnet (CIDR); empty means vmnet picks
}

// NetworkDir returns the path to the named network's directory.
func NetworkDir(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "monolith", "networks", name), nil
}

// NetworksDir returns the parent directory for all named networks.
func NetworksDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "monolith", "networks"), nil
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

// NetworkSockPath returns the path to the daemon's unix socket.
func NetworkSockPath(name string) (string, error) {
	dir, err := NetworkDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, networkSockFileName), nil
}

// IsNetworkDaemonAlive checks if the daemon for the given network is running.
func IsNetworkDaemonAlive(name string) bool {
	pid, err := ReadNetworkPid(name)
	if err != nil || pid <= 0 {
		return false
	}
	return processAlive(pid)
}

// RunNetworkDaemon is the main loop for the network daemon process.
// It creates a vmnet.Network, serializes it, and serves the serialization
// bytes to connecting clients over a unix domain socket.
//
// This function blocks until the daemon receives SIGTERM or SIGINT.
func RunNetworkDaemon(name, mode string, verbose bool) error {
	// Ignore SIGPIPE — the parent closes our stdout pipe after boot,
	// and we must not die when subsequent log writes fail.
	signal.Ignore(syscall.SIGPIPE)

	logger := NewLogger(os.Stderr)

	// Map monolith mode to vmnet mode.
	var vmnetMode vmnet.Mode
	switch mode {
	case NetworkShared:
		vmnetMode = vmnet.SharedMode
	case NetworkIsolated:
		vmnetMode = vmnet.HostMode
	default:
		return fmt.Errorf("named networks only support shared or isolated modes, got %q", mode)
	}

	// Read any existing config so we can honour a previously-set
	// UserSubnet on restart.
	existing, _ := ReadNetworkConfig(name)

	// Create the vmnet network.
	config, err := vmnet.NewNetworkConfiguration(vmnetMode)
	if err != nil {
		return fmt.Errorf("creating vmnet configuration: %w", err)
	}

	if existing.UserSubnet != "" {
		prefix, err := netip.ParsePrefix(existing.UserSubnet)
		if err != nil {
			return fmt.Errorf("parsing user_subnet %q: %w", existing.UserSubnet, err)
		}
		if err := config.SetIPv4Subnet(prefix); err != nil {
			return fmt.Errorf("applying user_subnet %s: %w (common cause: overlap with another active vmnet network, or out-of-range CIDR)", existing.UserSubnet, err)
		}
		logger.Info("applied user-requested subnet %s", existing.UserSubnet)
	}

	network, err := vmnet.NewNetwork(config)
	if err != nil {
		return fmt.Errorf("creating vmnet network: %w", err)
	}

	// Get the subnet for display and persistence.
	subnet, err := network.IPv4Subnet()
	if err != nil {
		logger.Warn("could not get IPv4 subnet: %v", err)
	}

	// Update config with actual subnet, preserving UserSubnet.
	netCfg := NetworkConfig{
		Mode:       mode,
		Subnet:     subnet.String(),
		UserSubnet: existing.UserSubnet,
	}
	if err := WriteNetworkConfig(name, netCfg); err != nil {
		return fmt.Errorf("writing network config: %w", err)
	}

	// Serialize the network — this is what clients will receive.
	serialization, err := network.CopySerialization()
	if err != nil {
		return fmt.Errorf("serializing network: %w", err)
	}

	// Extract the raw bytes from the XPC dictionary.
	// CopySerialization() returns an xpc.Object which is a *xpc.Dictionary
	// containing a "networkSerialization" key with xpc.Data (raw bytes).
	dict, ok := serialization.(*xpc.Dictionary)
	if !ok {
		return fmt.Errorf("serialization is not a dictionary (got %T)", serialization)
	}
	serData := dict.GetData("networkSerialization")
	if serData == nil {
		return fmt.Errorf("serialization dictionary missing 'networkSerialization' key")
	}

	// Write PID file.
	if err := WriteNetworkPid(name, os.Getpid()); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer CleanNetworkPid(name)

	// Create unix socket.
	sockPath, err := NetworkSockPath(name)
	if err != nil {
		return fmt.Errorf("getting socket path: %w", err)
	}

	// Remove stale socket file if it exists.
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listening on unix socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(sockPath)

	logger.Info("network daemon %q started (mode=%s, subnet=%s, pid=%d, verbose=%t)", name, mode, subnet.String(), os.Getpid(), verbose)
	logger.Info("serialization: %d bytes", len(serData))

	// Start a monitor interface on the network to observe all traffic.
	// Only active in verbose mode — logs DHCP, ARP, and other packets.
	if verbose {
		monIface, err := startMonitorInterface(network)
		if err != nil {
			logger.Warn("could not start monitor interface: %v", err)
		} else {
			logger.Info("monitor interface started (mac=%s, max_pkt=%d, max_read=%d)",
				formatMACFromParam(monIface.Param), monIface.MaxPacketSize, monIface.MaxReadPacketCount)
			go monitorPackets(logger, name, monIface)
		}
	}

	// Handle shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Accept connections in a goroutine.
	connCh := make(chan net.Conn)
	errCh := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				errCh <- err
				return
			}
			connCh <- conn
		}
	}()

	for {
		select {
		case sig := <-sigCh:
			logger.Info("network daemon %q received %v, shutting down", name, sig)
			return nil

		case conn := <-connCh:
			// Serve the serialization bytes to the client.
			// Protocol: 4-byte big-endian length prefix + raw bytes.
			logger.Info("client connected from %s", conn.RemoteAddr())
			go serveClient(logger, conn, serData)

		case err := <-errCh:
			// Accept error — likely listener closed.
			logger.Error("network daemon %q accept error: %v", name, err)
			return nil
		}
	}
}

// serveClient sends the serialized network bytes to a connected client.
// Protocol: 4-byte big-endian length prefix + raw bytes.
func serveClient(logger *Logger, conn net.Conn, data []byte) {
	defer conn.Close()

	// Write length prefix.
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := conn.Write(lenBuf); err != nil {
		logger.Error("writing length to client %s: %v", conn.RemoteAddr(), err)
		return
	}

	// Write data.
	if _, err := conn.Write(data); err != nil {
		logger.Error("writing data to client %s: %v", conn.RemoteAddr(), err)
		return
	}
	logger.Info("served %d bytes to client %s", len(data), conn.RemoteAddr())
}

// FetchNetworkSerialization connects to a network daemon's unix socket
// and receives the serialized network bytes.
func FetchNetworkSerialization(name string) ([]byte, error) {
	sockPath, err := NetworkSockPath(name)
	if err != nil {
		return nil, err
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to network daemon %q: %w (is the daemon running?)", name, err)
	}
	defer conn.Close()

	// Read 4-byte length prefix.
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, fmt.Errorf("reading length from daemon: %w", err)
	}
	dataLen := binary.BigEndian.Uint32(lenBuf)

	// Sanity check.
	if dataLen > 10*1024*1024 {
		return nil, fmt.Errorf("serialization data too large (%d bytes)", dataLen)
	}

	// Read data.
	data := make([]byte, dataLen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return nil, fmt.Errorf("reading data from daemon: %w", err)
	}

	return data, nil
}

// startMonitorInterface creates a vmnet.Interface on the network for packet monitoring.
// The interface is set to not allocate a MAC (passive observer).
func startMonitorInterface(network *vmnet.Network) (*vmnet.Interface, error) {
	desc := xpc.NewDictionary(
		xpc.KeyValue(vmnet.AllocateMacAddressKey, xpc.NewBool(false)),
	)
	return vmnet.StartInterfaceWithNetwork(network, desc)
}

// formatMACFromParam extracts and formats the MAC address from interface parameters.
func formatMACFromParam(param *xpc.Dictionary) string {
	if param == nil {
		return "unknown"
	}
	mac := param.GetString("mac_address")
	if mac == "" {
		return "none"
	}
	return mac
}

// monitorPackets reads all packets from the vmnet interface and logs them.
func monitorPackets(logger *Logger, networkName string, iface *vmnet.Interface) {
	mgr := vmnet.NewPktDescsManager(iface.MaxReadPacketCount, iface.MaxPacketSize)

	// Use event callback for efficient packet notification.
	iface.SetPacketsAvailableEventCallback(func(estimatedCount int) {
		for {
			count, err := iface.ReadPackets(mgr.PktDescs, iface.MaxReadPacketCount)
			if err != nil {
				logger.Error("[%s] monitor read error: %v", networkName, err)
				return
			}
			if count == 0 {
				return
			}
			for i := range count {
				pkt := mgr.At(i)
				size := pkt.GetPacketSize()
				if size < 14 {
					continue // too small for Ethernet header
				}
				// GetVmPktIov() returns a pointer to the struct iovec.
				// On arm64, struct iovec = { void *iov_base; size_t iov_len; }
				// The first 8 bytes are iov_base (pointer to the packet data).
				iovPtr := pkt.GetVmPktIov()
				iov_base := *(*unsafe.Pointer)(iovPtr)
				frame := unsafe.Slice((*byte)(iov_base), size)
				logger.Trace("[%s] pkt size=%d", networkName, size)
				logEthernetFrame(logger, networkName, frame)
			}
			mgr.Reset()
		}
	})
}

// logEthernetFrame parses and logs an Ethernet frame.
func logEthernetFrame(logger *Logger, networkName string, frame []byte) {
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
func logARP(logger *Logger, networkName, srcMAC, dstMAC string, payload []byte) {
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
func logIPv4(logger *Logger, networkName, srcMAC, dstMAC string, payload []byte) {
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
func logDHCP(logger *Logger, networkName, srcMAC, dstMAC, srcIP, dstIP string, srcPort uint16, payload []byte) {
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

// formatHexDump returns a hex dump of the first n bytes for debugging.
func formatHexDump(data []byte, maxLen int) string {
	if len(data) > maxLen {
		data = data[:maxLen]
	}
	parts := make([]string, len(data))
	for i, b := range data {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, " ")
}

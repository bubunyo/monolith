package monolith

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/Code-Hex/vz/v3/vmnet"
	"github.com/Code-Hex/vz/v3/xpc"
)

const (
	networkConfigFileName = "config.json"
	networkPidFileName    = "pid"
	networkSockFileName   = "daemon.sock"
)

// NetworkConfig holds the persisted configuration for a named network.
type NetworkConfig struct {
	Mode   string `json:"mode"`   // "shared" or "isolated"
	Subnet string `json:"subnet"` // IPv4 subnet (e.g. "192.168.64.0/24"), filled after creation
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
func RunNetworkDaemon(name, mode string) error {
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

	// Create the vmnet network.
	config, err := vmnet.NewNetworkConfiguration(vmnetMode)
	if err != nil {
		return fmt.Errorf("creating vmnet configuration: %w", err)
	}

	network, err := vmnet.NewNetwork(config)
	if err != nil {
		return fmt.Errorf("creating vmnet network: %w", err)
	}

	// Get the subnet for display and persistence.
	subnet, err := network.IPv4Subnet()
	if err != nil {
		log.Printf("warning: could not get IPv4 subnet: %v", err)
	}

	// Update config with actual subnet.
	netCfg := NetworkConfig{
		Mode:   mode,
		Subnet: subnet.String(),
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

	log.Printf("network daemon %q started (mode=%s, subnet=%s, pid=%d)", name, mode, subnet.String(), os.Getpid())

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
			log.Printf("network daemon %q received %v, shutting down", name, sig)
			return nil

		case conn := <-connCh:
			// Serve the serialization bytes to the client.
			// Protocol: 4-byte big-endian length prefix + raw bytes.
			go serveClient(conn, serData)

		case err := <-errCh:
			// Accept error — likely listener closed.
			log.Printf("network daemon %q accept error: %v", name, err)
			return nil
		}
	}
}

// serveClient sends the serialized network bytes to a connected client.
// Protocol: 4-byte big-endian length prefix + raw bytes.
func serveClient(conn net.Conn, data []byte) {
	defer conn.Close()

	// Write length prefix.
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := conn.Write(lenBuf); err != nil {
		log.Printf("error writing length to client: %v", err)
		return
	}

	// Write data.
	if _, err := conn.Write(data); err != nil {
		log.Printf("error writing data to client: %v", err)
		return
	}
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

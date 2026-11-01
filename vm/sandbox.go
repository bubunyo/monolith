package vm

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// A sandbox is a docker image VM that runs one repo's commands in a
// container, isolated from the Mac (`monolith sandbox`). Only the repo is
// shared, read-only; the guest overlays it with a writable layer at the
// same path, so the code can build and edit freely while the Mac's files
// stay untouched until changes are applied explicitly.

// SandboxConfig is the sandbox part of a VM's config.
type SandboxConfig struct {
	Repo    string   `json:"repo"`              // absolute path of the repo on the Mac
	Image   string   `json:"image,omitempty"`   // container image for the shell (empty: default/devcontainer)
	Network string   `json:"network,omitempty"` // SandboxNetwork* (empty: outbound)
	Allow   []string `json:"allow,omitempty"`   // host:port allowlist for SandboxNetworkAllow
}

// Sandbox network policies.
const (
	SandboxNetworkOutbound = "outbound" // internet, but nothing private: not the Mac, the LAN or the gateway
	SandboxNetworkNone     = "none"     // the shell container has no network at all
	SandboxNetworkAllow    = "allow"    // only the Allow list (plus DNS)
)

// ValidSandboxNetworks is the set of accepted --network values.
var ValidSandboxNetworks = []string{SandboxNetworkOutbound, SandboxNetworkNone, SandboxNetworkAllow}

// NetworkOf returns the sandbox's network policy.
func (s *SandboxConfig) NetworkOf() string {
	if s.Network == "" {
		return SandboxNetworkOutbound
	}
	return s.Network
}

const (
	// ListeningPortsPort is the guest TCP port (gateway only) that answers
	// with /proc/net/tcp and /proc/net/tcp6, so the daemon can forward
	// whatever a sandbox's processes listen on.
	ListeningPortsPort = 2380

	// GuestLayoutSandbox is the first guest layout with the sandbox pieces.
	GuestLayoutSandbox = 3

	sandboxShareTag     = "repo"
	sandboxCmdlineArg   = "monolith.sandbox"
	sandboxGuestRelayLo = DockerAPIPort // 2375..2380 are monolith's relays
	sandboxGuestRelayHi = ListeningPortsPort
)

// sandboxCmdline encodes the repo path for the kernel command line, which
// can't hold spaces.
func sandboxCmdline(repo string) string {
	return sandboxCmdlineArg + "=" + base64.StdEncoding.EncodeToString([]byte(repo))
}

// parseListeningPorts returns the TCP ports listening on all interfaces in
// /proc/net/tcp{,6} output, leaving out monolith's own relays and sshd.
// Sockets bound to loopback aren't reachable from the gateway.
func parseListeningPorts(data []byte) map[portKey]bool {
	ports := map[portKey]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" { // 0A = TCP_LISTEN
			continue
		}
		addr, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		raw, err := hex.DecodeString(addr)
		if err != nil || !net.IP(raw).IsUnspecified() {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || port == 0 || port == SSHPort || (port >= sandboxGuestRelayLo && port <= sandboxGuestRelayHi) {
			continue
		}
		ports[portKey{"tcp", int(port)}] = true
	}
	return ports
}

// validateSandbox checks a sandbox config before boot.
func validateSandbox(s *SandboxConfig) error {
	if s.Repo == "" || !strings.HasPrefix(s.Repo, "/") {
		return fmt.Errorf("sandbox repo path %q must be absolute", s.Repo)
	}
	return nil
}

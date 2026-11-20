//go:build e2e

package e2e

import (
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	mnet "github.com/bubunyo/monolith/net"
	"github.com/stretchr/testify/require"
)

// TestNetworkSubnet creates named vmnet networks with --subnet: honoured,
// kept across a restart, and refused when invalid, public or overlapping.
func TestNetworkSubnet(t *testing.T) {
	names := []string{"e2e-subnet", "e2e-bad", "e2e-public", "e2e-overlap-a", "e2e-overlap-b"}
	cleanup := func() {
		for _, n := range names {
			removeNetwork(n)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	t.Run("honoured", func(t *testing.T) {
		mustMonolith(t, "network", "create", "e2e-subnet", "--subnet", "192.168.77.0/24")
		waitNetwork(t, "e2e-subnet", true)
		requireSubnet(t, "e2e-subnet", "192.168.77.0/24")
	})

	t.Run("survives a restart", func(t *testing.T) {
		mustMonolith(t, "network", "stop", "e2e-subnet")
		waitNetwork(t, "e2e-subnet", false)
		mustMonolith(t, "network", "start", "e2e-subnet")
		waitNetwork(t, "e2e-subnet", true)
		requireSubnet(t, "e2e-subnet", "192.168.77.0/24")
	})

	t.Run("invalid CIDR", func(t *testing.T) {
		out, code := monolith(t, "network", "create", "e2e-bad", "--subnet", "not-a-cidr")
		require.NotZero(t, code, out)
		require.False(t, mnet.IsNetworkDaemonAlive("e2e-bad"))
		dir, err := mnet.NetworkDir("e2e-bad")
		require.NoError(t, err)
		require.NoDirExists(t, dir, "a refused create leaves nothing behind")
	})

	t.Run("public range", func(t *testing.T) {
		out, code := monolith(t, "network", "create", "e2e-public", "--subnet", "8.8.8.0/24")
		require.NotZero(t, code, out)
		require.Contains(t, strings.ToLower(out), "rfc 1918")
		require.False(t, mnet.IsNetworkDaemonAlive("e2e-public"))
	})

	t.Run("overlap", func(t *testing.T) {
		mustMonolith(t, "network", "create", "e2e-overlap-a", "--subnet", "192.168.88.0/24")
		waitNetwork(t, "e2e-overlap-a", true)
		out, code := monolith(t, "network", "create", "e2e-overlap-b", "--subnet", "192.168.88.128/25")
		require.NotZero(t, code, out)
		require.Contains(t, out, "overlap")
		require.Contains(t, out, "e2e-overlap-a")
		require.False(t, mnet.IsNetworkDaemonAlive("e2e-overlap-b"))
	})
}

func waitNetwork(t *testing.T, name string, alive bool) {
	t.Helper()
	require.Eventually(t, func() bool { return mnet.IsNetworkDaemonAlive(name) == alive },
		10*time.Second, 200*time.Millisecond, "network %q alive=%v never reached", name, alive)
}

// requireSubnet checks the network's requested and actual subnets. vmnet
// reports the gateway address with the prefix, so both are masked.
func requireSubnet(t *testing.T, name, want string) {
	t.Helper()
	cfg, err := mnet.ReadNetworkConfig(name)
	require.NoError(t, err)
	w := netip.MustParsePrefix(want).Masked()
	for _, got := range []string{cfg.UserSubnet, cfg.Subnet} {
		p, err := netip.ParsePrefix(got)
		require.NoError(t, err, got)
		require.Equal(t, w, p.Masked())
	}
}

// removeNetwork tears a network down, best effort.
func removeNetwork(name string) {
	mnet.StopNetworkDaemon(name)
	if pid, _ := mnet.ReadNetworkPid(name); pid > 0 {
		syscall.Kill(pid, syscall.SIGKILL)
	}
	if dir, err := mnet.NetworkDir(name); err == nil {
		os.RemoveAll(dir)
	}
}

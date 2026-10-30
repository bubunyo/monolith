package net

import (
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// equalPrefix asserts two CIDR strings represent the same network (after
// masking the host bits). vmnet reports the gateway IP with the prefix
// (e.g. "192.168.77.1/24") rather than the network address.
func equalPrefix(t *testing.T, want, got string) {
	t.Helper()
	wp, err := netip.ParsePrefix(want)
	require.NoError(t, err)
	gp, err := netip.ParsePrefix(got)
	require.NoError(t, err, "got=%q", got)
	require.Equal(t, wp.Masked(), gp.Masked(), "want %s, got %s", want, got)
}

// TestNetworkCustomSubnet exercises the `--subnet` flag end-to-end against
// the codesigned ./bin/monolith binary. It verifies that:
//   - a valid subnet is honoured and recorded in config.json
//   - the subnet survives a daemon stop/start cycle
//   - invalid CIDR strings are rejected
//   - public/non-RFC-1918 ranges are rejected
//   - an overlapping subnet against an existing network is rejected
//
// Network names used by this test (hardcoded; documented here so they can
// be cleaned up manually if a previous run was interrupted):
//
//	test-subnet, test-bad, test-public, test-overlap-a, test-overlap-b
//
// Subnets used: 192.168.77.0/24 and 192.168.77.128/25 (overlap probe);
// 192.168.88.0/24 (independent, used by overlap A); 8.8.8.0/24 (rejected).
//
// Skipped on non-darwin/arm64, when the binary is missing, or when running
// as root (vmnet requires the calling user's session).
func TestNetworkCustomSubnet(t *testing.T) {
	if testing.Short() {
		t.Skip("creates vmnet networks; skipped with -short (CI runners can't run nested VMs)")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("requires darwin/arm64")
	}
	if os.Geteuid() == 0 {
		t.Skip("must not run as root")
	}
	binary, err := filepath.Abs("../bin/monolith")
	require.NoError(t, err)
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}

	names := []string{"test-subnet", "test-bad", "test-public", "test-overlap-a", "test-overlap-b"}
	cleanup := func() {
		for _, n := range names {
			cleanupNetwork(t, binary, n)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	t.Run("SubnetHonoured", func(t *testing.T) {
		out, err := runMonolith(binary, "network", "create", "test-subnet", "--subnet", "192.168.77.0/24")
		require.NoErrorf(t, err, "create failed: %s", out)
		t.Cleanup(func() { cleanupNetwork(t, binary, "test-subnet") })

		waitForDaemon(t, "test-subnet", true, 10*time.Second)

		cfg, err := ReadNetworkConfig("test-subnet")
		require.NoError(t, err)
		equalPrefix(t, "192.168.77.0/24", cfg.UserSubnet)
		equalPrefix(t, "192.168.77.0/24", cfg.Subnet)
	})

	t.Run("SurvivesRestart", func(t *testing.T) {
		// Reuses the network from SubnetHonoured if still alive; otherwise creates fresh.
		if !IsNetworkDaemonAlive("test-subnet") {
			out, err := runMonolith(binary, "network", "create", "test-subnet", "--subnet", "192.168.77.0/24")
			require.NoErrorf(t, err, "create failed: %s", out)
			waitForDaemon(t, "test-subnet", true, 10*time.Second)
		}

		out, err := runMonolith(binary, "network", "stop", "test-subnet")
		require.NoErrorf(t, err, "stop failed: %s", out)
		waitForDaemon(t, "test-subnet", false, 10*time.Second)

		out, err = runMonolith(binary, "network", "start", "test-subnet")
		require.NoErrorf(t, err, "start failed: %s", out)
		waitForDaemon(t, "test-subnet", true, 10*time.Second)

		cfg, err := ReadNetworkConfig("test-subnet")
		require.NoError(t, err)
		equalPrefix(t, "192.168.77.0/24", cfg.UserSubnet)
		equalPrefix(t, "192.168.77.0/24", cfg.Subnet)
	})

	t.Run("InvalidCIDRRejected", func(t *testing.T) {
		out, err := runMonolith(binary, "network", "create", "test-bad", "--subnet", "not-a-cidr")
		require.Errorf(t, err, "expected failure, got output: %s", out)
		require.False(t, IsNetworkDaemonAlive("test-bad"), "no daemon should have spawned")
		// Config dir must not exist for a rejected create.
		dir, derr := NetworkDir("test-bad")
		require.NoError(t, derr)
		_, statErr := os.Stat(dir)
		require.True(t, os.IsNotExist(statErr), "dir should not exist, got %v", statErr)
	})

	t.Run("OutOfRangeRejected", func(t *testing.T) {
		out, err := runMonolith(binary, "network", "create", "test-public", "--subnet", "8.8.8.0/24")
		require.Errorf(t, err, "expected failure, got output: %s", out)
		require.Contains(t, strings.ToLower(out), "rfc 1918", "error should mention RFC 1918")
		require.False(t, IsNetworkDaemonAlive("test-public"))
	})

	t.Run("OverlapRejected", func(t *testing.T) {
		// Use a fresh subnet to avoid colliding with test-subnet (still alive).
		out, err := runMonolith(binary, "network", "create", "test-overlap-a", "--subnet", "192.168.88.0/24")
		require.NoErrorf(t, err, "create A failed: %s", out)
		t.Cleanup(func() { cleanupNetwork(t, binary, "test-overlap-a") })
		waitForDaemon(t, "test-overlap-a", true, 10*time.Second)

		// Overlapping /25 inside the existing /24 must be rejected.
		out, err = runMonolith(binary, "network", "create", "test-overlap-b", "--subnet", "192.168.88.128/25")
		require.Errorf(t, err, "expected overlap rejection, got output: %s", out)
		require.Contains(t, out, "overlap")
		require.Contains(t, out, "test-overlap-a")
		require.False(t, IsNetworkDaemonAlive("test-overlap-b"))
	})
}

// runMonolith executes ./bin/monolith with the given args and returns
// combined stdout+stderr along with the exit error.
func runMonolith(binary string, args ...string) (string, error) {
	cmd := exec.Command(binary, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// waitForDaemon blocks until IsNetworkDaemonAlive(name) matches `alive`,
// or fails the test if the deadline elapses.
func waitForDaemon(t *testing.T, name string, alive bool, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		return IsNetworkDaemonAlive(name) == alive
	}, timeout, 200*time.Millisecond, "daemon %q alive=%v not reached", name, alive)
}

// cleanupNetwork tears down a network best-effort. Never asserts.
func cleanupNetwork(t *testing.T, binary, name string) {
	t.Helper()
	// Always unload the launchd job, even if the daemon already exited.
	_, _ = StopNetworkDaemon(name)
	// Last-ditch: kill by PID if still alive.
	if pid, _ := ReadNetworkPid(name); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if dir, err := NetworkDir(name); err == nil {
		_ = os.RemoveAll(dir)
	}
}

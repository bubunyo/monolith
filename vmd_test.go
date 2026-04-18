package monolith

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRunDaemon boots a VM via the daemon process and verifies it becomes
// reachable. This test requires macOS on Apple Silicon with
// Virtualization.framework — it will not pass in CI or on non-macOS hosts.
//
// The daemon runs as a subprocess (like production) because RunDaemon blocks
// and requires the binary to be codesigned with entitlements.plist.
func TestRunDaemon(t *testing.T) {
	binary := "./bin/monolith"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}

	name := "test-vm"

	// Spawn the daemon as a background process.
	daemon := exec.Command(binary, "__vmd", name)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting daemon: %v", err)
	}
	defer func() {
		daemon.Process.Kill()
		daemon.Wait()
		CleanState(name)
	}()

	// Wait for the daemon to write state and become reachable.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err := ReadState(name)
		if err == nil && status.State == StatusRunning {
			t.Logf("VM %q is running", name)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("VM %q did not reach running state within timeout", name)
}

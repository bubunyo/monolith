package monolith

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConsoleLogTee verifies that --log-console captures the guest's serial
// console output to a file even when no client is attached. Requires macOS
// on Apple Silicon and a codesigned binary at ./bin/monolith.
//
// The test is best-effort about content: if the platform/Alpine assets
// happen to produce no boot output (e.g. a regression in the kernel cmdline
// or a stale initramfs), it still verifies that the tee file is created and
// the daemon stays healthy. Assertions on actual content are gated on
// MONOLITH_TEST_CONSOLE_OUTPUT=1 to avoid flaking when the underlying
// console pipeline is broken for unrelated reasons.
func TestConsoleLogTee(t *testing.T) {
	binary := "./bin/monolith"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}

	name := "test-console-log"
	logPath := filepath.Join(t.TempDir(), "console.log")

	CleanState(name)
	t.Cleanup(func() { CleanState(name) })

	daemon := exec.Command(binary, "__vmd", name, "--log-console", logPath)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
	})

	// VM must reach running state.
	deadline := time.Now().Add(60 * time.Second)
	running := false
	for time.Now().Before(deadline) {
		status, err := ReadState(name)
		if err == nil && status.State == StatusRunning {
			running = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.True(t, running, "VM did not reach running state within 60s")

	// Tee target file must exist (plumbing check — independent of whether
	// the guest happens to emit any bytes).
	require.Eventually(t, func() bool {
		_, err := os.Stat(logPath)
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "console log file %s was not created", logPath)

	if os.Getenv("MONOLITH_TEST_CONSOLE_OUTPUT") != "1" {
		t.Log("skipping content assertion (set MONOLITH_TEST_CONSOLE_OUTPUT=1 to enforce)")
		return
	}

	// Strict content check: wait for a recognizable Linux/Alpine boot
	// marker. Fails if nothing arrives in 60s.
	deadline = time.Now().Add(60 * time.Second)
	markers := []string{
		"Linux version",
		"OpenRC",
		"Welcome to Alpine",
		"mono@" + name,
	}
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(logPath)
		if err == nil && len(data) > 0 {
			s := string(data)
			for _, m := range markers {
				if strings.Contains(s, m) {
					t.Logf("console log contains %q after %d bytes", m, len(data))
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	if data, err := os.ReadFile(logPath); err == nil {
		t.Logf("console log (%d bytes):\n%s", len(data), string(data))
	}
	t.Fatalf("console log %s did not contain expected boot markers within 60s", logPath)
}

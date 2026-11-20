//go:build e2e

package e2e

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bubunyo/monolith/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spawnDaemon starts `monolith __vmd name args...` and kills it at cleanup.
func spawnDaemon(t *testing.T, name string, args ...string) {
	t.Helper()
	vm.CleanState(name)
	daemon := exec.Command(binary, append([]string{"__vmd", name}, args...)...)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
		if dir, err := vm.VMDir(name); err == nil {
			os.RemoveAll(dir)
		}
	})
}

func waitRunning(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := vm.ReadState(name)
		return err == nil && status.State == vm.StatusRunning
	}, timeout, 500*time.Millisecond, "VM %q never reached running", name)
}

// TestVMDaemon boots an alpine VM through the daemon process, teeing the
// console to a file.
func TestVMDaemon(t *testing.T) {
	const name = "e2e-vm"
	logPath := filepath.Join(t.TempDir(), "console.log")
	spawnDaemon(t, name, "--log-console", logPath)
	waitRunning(t, name, time.Minute)

	booted := func() bool {
		data, _ := os.ReadFile(logPath)
		return strings.Contains(string(data), "mono@"+name)
	}
	if !assert.Eventually(t, booted, time.Minute, 500*time.Millisecond, "the console log never showed the guest's shell prompt") {
		data, _ := os.ReadFile(logPath)
		t.Logf("console log (%d bytes):\n%s", len(data), data)
	}
}

// TestKataVM boots a Kata VM and waits for the agent's root shell.
func TestKataVM(t *testing.T) {
	const name = "e2e-kata"
	cfg := vm.DefaultConfig()
	cfg.Image = vm.ImageKata
	require.NoError(t, vm.WriteConfig(name, cfg))
	// Prepare first, as `vm run` does, so the download isn't counted against the boot.
	image, err := vm.LookupImage(vm.ImageKata)
	require.NoError(t, err)
	_, err = image.Prepare(name, io.Discard)
	require.NoError(t, err)

	logPath := filepath.Join(t.TempDir(), "console.log")
	spawnDaemon(t, name, "--log-console", logPath)
	require.Eventually(t, func() bool {
		data, _ := os.ReadFile(logPath)
		return strings.Contains(string(data), "root@"+name)
	}, time.Minute, 500*time.Millisecond, "the console never showed the agent's shell")
	waitRunning(t, name, 5*time.Second)
}

package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseVerityParams(t *testing.T) {
	p, err := parseVerityParams("root_hash=abc,salt=def,data_blocks=64000,data_block_size=4096,hash_block_size=4096\n")
	require.NoError(t, err)
	require.Equal(t, verityParams{rootHash: "abc", salt: "def", dataBlocks: 64000, dataBlockSize: 4096, hashBlockSize: 4096}, p)

	for _, bad := range []string{
		"",
		"root_hash=abc,salt=def,data_blocks=x,data_block_size=4096,hash_block_size=4096",
		"salt=def,data_blocks=1,data_block_size=4096,hash_block_size=4096",
		"root_hash=abc,salt=def,data_blocks=1,data_block_size=4096",
	} {
		_, err := parseVerityParams(bad)
		require.Error(t, err, "input %q", bad)
	}
}

func TestKataCommandLine(t *testing.T) {
	cl := kataCommandLine("my-vm", verityParams{rootHash: "abc", salt: "def", dataBlocks: 64000, dataBlockSize: 4096, hashBlockSize: 4096})
	// 64000 blocks of 4 KiB = 512000 sectors; hash tree on vda2.
	require.Contains(t, cl, `dm-mod.create="dm-verity,,,ro,0 512000 verity 1 /dev/vda1 /dev/vda2 4096 4096 64000 0 sha256 abc def"`)
	for _, want := range []string{"console=hvc0", "root=/dev/dm-0", "ip=dhcp", "systemd.hostname=my-vm", "agent.debug_console"} {
		require.Contains(t, strings.Fields(cl), want)
	}
}

// TestRunDaemonKata boots a Kata VM through the codesigned binary and checks
// that the guest reaches the agent's root shell on the console. It downloads
// the Kata assets (~65 MB) on first run. Requires macOS on Apple Silicon.
func TestRunDaemonKata(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a VM; skipped with -short (CI runners can't run nested VMs)")
	}
	binary := "../bin/monolith"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}

	name := "test-kata"
	cfg := DefaultConfig()
	cfg.Image = ImageKata
	require.NoError(t, WriteConfig(name, cfg))
	CleanState(name)
	t.Cleanup(func() { CleanState(name) })

	// Prepare in the test, as `vm run` does, so the first-run download
	// isn't counted against the boot deadline.
	_, err := kataImage{}.Prepare(name)
	require.NoError(t, err)

	logPath := filepath.Join(t.TempDir(), "console.log")
	daemon := exec.Command(binary, "__vmd", name, "--log-console", logPath)
	daemon.Stdout = os.Stdout
	daemon.Stderr = os.Stderr
	require.NoError(t, daemon.Start())
	t.Cleanup(func() {
		daemon.Process.Kill()
		daemon.Wait()
	})

	prompt := "root@" + name
	require.Eventually(t, func() bool {
		data, _ := os.ReadFile(logPath)
		return strings.Contains(string(data), prompt)
	}, 60*time.Second, 500*time.Millisecond, "console never showed %q", prompt)

	status, err := ReadState(name)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, status.State)
}

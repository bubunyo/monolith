package sandbox

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSandboxIsolation runs a hostile script in a real sandbox and checks
// what it can and can't reach, then diff and apply. It boots a VM, pulls
// images and needs the docker CLI and an unlocked Mac, so it only runs with
// MONOLITH_TEST_SANDBOX=1 and a built ../../bin/monolith.
func TestSandboxIsolation(t *testing.T) {
	if os.Getenv("MONOLITH_TEST_SANDBOX") != "1" {
		t.Skip("set MONOLITH_TEST_SANDBOX=1 to run (boots a sandbox VM)")
	}
	binary, err := filepath.Abs("../../bin/monolith")
	require.NoError(t, err)
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not found on PATH")
	}

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "keep.txt"), []byte("keep\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("original\n"), 0644))
	monolith := func(args ...string) (string, error) {
		cmd := exec.Command(binary, append([]string{"sandbox"}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "MONOLITH_SECRET_PROBE=leaked")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { monolith("rm", "--yes") })

	// A server only on the Mac's loopback: the sandbox must not reach it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("mac-secret")) }))
	port := l.Addr().(*net.TCPAddr).Port

	script := `
probe() { if timeout 5 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; then echo "reach:$3"; else echo "blocked:$3"; fi; }
ls /Users >/dev/null 2>&1 && echo "saw:/Users" || echo "no:/Users"
env | grep -q MONOLITH_SECRET_PROBE && echo "saw:env" || echo "no:env"
probe example.com 80 internet
probe 192.168.127.254 ` + strconv.Itoa(port) + ` mac-loopback
probe host.docker.internal ` + strconv.Itoa(port) + ` host-docker-internal
echo changed > a.txt; echo new > b.txt; rm keep.txt
`
	out, err := monolith("exec", "--", "bash", "-c", script)
	require.NoError(t, err, out)
	for _, want := range []string{"no:/Users", "no:env", "reach:internet", "blocked:mac-loopback", "blocked:host-docker-internal"} {
		require.Contains(t, out, want)
	}

	// The Mac's copy is untouched.
	data, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "original\n", string(data))
	require.FileExists(t, filepath.Join(repo, "keep.txt"))
	require.NoFileExists(t, filepath.Join(repo, "b.txt"))

	out, err = monolith("diff")
	require.NoError(t, err, out)
	for _, want := range []string{"M a.txt", "A b.txt", "D keep.txt"} {
		require.Contains(t, out, want)
	}

	out, err = monolith("apply", "--yes", "b.txt")
	require.NoError(t, err, out)
	data, err = os.ReadFile(filepath.Join(repo, "b.txt"))
	require.NoError(t, err)
	require.Equal(t, "new\n", string(data))
	require.FileExists(t, filepath.Join(repo, "keep.txt"), "only the applied path changes")

	out, err = monolith("diff")
	require.NoError(t, err, out)
	require.NotContains(t, out, "b.txt")
	require.True(t, strings.Contains(out, "M a.txt") && strings.Contains(out, "D keep.txt"), out)
}

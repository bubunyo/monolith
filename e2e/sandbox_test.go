//go:build e2e

package e2e

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

// TestSandbox runs a hostile script in a sandbox and checks what it can and
// can't reach, then diff and apply.
func TestSandbox(t *testing.T) {
	requireTool(t, "docker")

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "keep.txt"), []byte("keep\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.txt"), []byte("original\n"), 0644))
	sandbox := func(args ...string) (string, error) {
		cmd := exec.Command(binary, append([]string{"sandbox"}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "MONOLITH_SECRET_PROBE=leaked")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { sandbox("rm", "--yes") })

	// A server only on the Mac's loopback: the sandbox must not reach it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("mac-secret")) }))
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)

	script := `
probe() { if timeout 5 bash -c "exec 3<>/dev/tcp/$1/$2" 2>/dev/null; then echo "reach:$3"; else echo "blocked:$3"; fi; }
ls /Users >/dev/null 2>&1 && echo "saw:/Users" || echo "no:/Users"
env | grep -q MONOLITH_SECRET_PROBE && echo "saw:env" || echo "no:env"
probe example.com 80 internet
probe 192.168.127.254 ` + port + ` mac-loopback
probe host.docker.internal ` + port + ` host-docker-internal
echo changed > a.txt; echo new > b.txt; rm keep.txt
`
	out, err := sandbox("exec", "--", "bash", "-c", script)
	require.NoError(t, err, out)
	for _, want := range []string{"no:/Users", "no:env", "reach:internet", "blocked:mac-loopback", "blocked:host-docker-internal"} {
		require.Contains(t, out, want)
	}

	data, err := os.ReadFile(filepath.Join(repo, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "original\n", string(data), "the Mac's copy is untouched")
	require.FileExists(t, filepath.Join(repo, "keep.txt"))
	require.NoFileExists(t, filepath.Join(repo, "b.txt"))

	out, err = sandbox("diff")
	require.NoError(t, err, out)
	for _, want := range []string{"M a.txt", "A b.txt", "D keep.txt"} {
		require.Contains(t, out, want)
	}

	out, err = sandbox("apply", "--yes", "b.txt")
	require.NoError(t, err, out)
	data, err = os.ReadFile(filepath.Join(repo, "b.txt"))
	require.NoError(t, err)
	require.Equal(t, "new\n", string(data))
	require.FileExists(t, filepath.Join(repo, "keep.txt"), "only the applied path changes")

	out, err = sandbox("diff")
	require.NoError(t, err, out)
	require.NotContains(t, out, "b.txt")
	require.True(t, strings.Contains(out, "M a.txt") && strings.Contains(out, "D keep.txt"), out)

	// After a pause to disk the repo still reads: the restored VM's virtio-fs
	// handles are new, and the overlay must not serve stale ones.
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "src", "api"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "src", "api", "f.txt"), []byte("deep\n"), 0644))
	out, err = sandbox("exec", "--", "cat", "src/api/f.txt")
	require.NoError(t, err, out)
	out, err = sandbox("status")
	require.NoError(t, err, out)
	require.Contains(t, out, "on disk", "exec pauses the sandbox to disk")
	out, err = sandbox("exec", "--", "sh", "-c", "cd src/api && cat f.txt && cat ../../a.txt")
	require.NoError(t, err, out)
	require.Contains(t, out, "deep")
	require.Contains(t, out, "changed")
}

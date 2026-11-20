//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContainer runs container VMs: exit codes, the image's config and
// overrides, the network, published ports and the background lifecycle.
func TestContainer(t *testing.T) {
	ctr := func(args ...string) (string, int) {
		t.Helper()
		return monolith(t, append([]string{"container"}, args...)...)
	}
	const alpine = "alpine:3.21"

	t.Run("exit code and output", func(t *testing.T) {
		out, code := ctr("run", "--rm", alpine, "sh", "-c", "echo hello; exit 3")
		require.Equal(t, 3, code, out)
		require.Contains(t, out, "hello\n", "plain output, no CR")
	})

	t.Run("config and overrides", func(t *testing.T) {
		out, code := ctr("run", "--rm", "-e", "FOO=bar", "-w", "/work/dir", "-u", "nobody",
			"--name", "e2e-config", alpine, "sh", "-c", `echo "$FOO $(pwd) $(id -u) $(hostname) $HOME"`)
		require.Zero(t, code, out)
		require.Contains(t, out, "bar /work/dir 65534 e2e-config /\n")

		out, code = ctr("run", "--rm", "--entrypoint", "printenv", alpine, "PATH")
		require.Zero(t, code, out)
		require.Contains(t, out, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	})

	t.Run("command errors", func(t *testing.T) {
		out, code := ctr("run", "--rm", alpine, "no-such-command")
		require.Equal(t, 127, code, out)
		require.Contains(t, out, "executable file not found")
		_, code = ctr("run", "--rm", alpine, "/etc/passwd")
		require.Equal(t, 126, code)
	})

	t.Run("devices and network", func(t *testing.T) {
		out, code := ctr("run", "--rm", alpine, "sh", "-c",
			`ls -l /dev/stdout /dev/stderr >/dev/null && echo devs-ok; wget -q -O- http://example.com | grep -o '<title>.*</title>'`)
		require.Zero(t, code, out)
		require.Contains(t, out, "devs-ok")
		require.Contains(t, out, "<title>Example Domain</title>")
	})

	t.Run("disk size", func(t *testing.T) {
		size := func(args ...string) int {
			out, code := ctr(append(append([]string{"run", "--rm"}, args...), alpine, "sh", "-c", "df -k / | awk 'NR==2 {print $2}'")...)
			require.Zero(t, code, out)
			kb, err := strconv.Atoi(strings.TrimSpace(lastLine(out)))
			require.NoError(t, err, out)
			return kb
		}
		require.InDelta(t, 8<<20, size(), 0.06*(8<<20), "default 8G")
		require.InDelta(t, 20<<20, size("--disk", "20G"), 0.06*(20<<20))
		out, code := ctr("run", "--rm", "--disk", "100M", alpine, "true")
		require.NotZero(t, code)
		require.Contains(t, out, "--disk must be at least")
	})

	t.Run("--rm removes the container", func(t *testing.T) {
		_, code := ctr("run", "--rm", "--name", "e2e-rm", alpine, "true")
		require.Zero(t, code)
		_, code = ctr("logs", "e2e-rm")
		require.NotZero(t, code, "gone")
	})

	t.Run("background lifecycle", func(t *testing.T) {
		const name = "e2e-web"
		t.Cleanup(func() { ctr("rm", "-f", name) })
		out, code := ctr("run", "-d", "--name", name, "nginx:1.29")
		require.Zero(t, code, out)
		require.Equal(t, name+"\n", lastLine(out))

		require.Eventually(t, func() bool {
			out, _ := ctr("logs", name)
			return strings.Contains(out, "start worker process")
		}, 30*time.Second, 200*time.Millisecond, "nginx logs to /dev/stderr")
		out, _ = ctr("ps")
		require.Regexp(t, name+`\s+nginx:1.29\s+.*Up `, out)

		start := time.Now()
		out, code = ctr("stop", name)
		require.Zero(t, code, out)
		require.Less(t, time.Since(start), 8*time.Second, "SIGQUIT (the image's stop signal) stops nginx at once")
		out, _ = ctr("logs", name)
		require.Contains(t, out, "gracefully shutting down")
		out, _ = ctr("ps", "-a")
		require.Regexp(t, name+`.*Exited \(0\)`, out)

		out, code = ctr("start", name)
		require.Zero(t, code, out)
		out, _ = ctr("ps")
		require.Contains(t, out, name)

		out, code = ctr("rm", name)
		require.NotZero(t, code, "rm refuses a running container")
		require.Contains(t, out, "use -f")
		_, code = ctr("rm", "-f", name)
		require.Zero(t, code)
		out, _ = ctr("ps", "-a")
		require.NotContains(t, out, name)
	})

	t.Run("published ports", func(t *testing.T) {
		const name = "e2e-ports"
		t.Cleanup(func() { ctr("rm", "-f", name) })
		host := freePort(t)
		want := fmt.Sprintf("127.0.0.1:%d->80/tcp", host)

		out, code := ctr("run", "-d", "--name", name, "-p", fmt.Sprintf("%d:80", host), "nginx:1.29")
		require.Zero(t, code, out)
		waitServes(t, host, "nginx", time.Minute)
		out, _ = ctr("ps")
		require.Regexp(t, name+`\s.*`+regexp.QuoteMeta(want), out)

		out, code = ctr("run", "-d", "--name", name+"-2", "-p", fmt.Sprintf("%d:80", host), "nginx:1.29")
		require.NotZero(t, code, out)
		require.Contains(t, out, "already in use")
		ctr("rm", "-f", name+"-2")

		out, code = ctr("stop", name)
		require.Zero(t, code, out)
		require.False(t, serves(host, "nginx"), "the forward goes away with the container")
		out, _ = ctr("ps", "-a")
		require.Regexp(t, name+`\s.*`+regexp.QuoteMeta(want), out, "stopped containers still list their ports")
		out, code = ctr("start", name)
		require.Zero(t, code, out)
		waitServes(t, host, "nginx", time.Minute)

		low := freePrivilegedPort(t)
		out, code = ctr("run", "-d", "--name", name+"-low", "-p", fmt.Sprintf("%d:80", low), "nginx:1.29")
		require.Zero(t, code, out)
		t.Cleanup(func() { ctr("rm", "-f", name+"-low") })
		waitServes(t, low, "nginx", time.Minute)

		out, code = ctr("run", "--rm", "-p", "0.0.0.0:80:80", alpine, "true")
		require.NotZero(t, code)
		require.Contains(t, out, "only on the Mac's localhost")
	})

	t.Run("prune", func(t *testing.T) {
		// prune removes every stopped container, so it gets its own HOME
		// (sharing the Kata kernel). Under /tmp: socket paths must fit 104 bytes.
		home, err := os.MkdirTemp("/tmp", "ml-")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(home) })
		shared, err := os.UserHomeDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".monolith"), 0755))
		require.NoError(t, os.Symlink(filepath.Join(shared, ".monolith/kata"), filepath.Join(home, ".monolith/kata")))
		isolated := func(args ...string) (string, int) {
			return run(t, []string{"HOME=" + home}, binary, append([]string{"container"}, args...)...)
		}
		t.Cleanup(func() { isolated("rm", "-f", "p-running") })

		_, code := isolated("run", "--name", "p-stopped", alpine, "sh", "-c", "head -c 3000000 /dev/urandom > /blob")
		require.Zero(t, code)
		out, code := isolated("run", "-d", "--name", "p-running", alpine, "sleep", "300")
		require.Zero(t, code, out)

		out, code = isolated("prune", "-f")
		require.Zero(t, code, out)
		require.Contains(t, out, "Deleted p-stopped")
		require.NotContains(t, out, "p-running")
		require.Regexp(t, `Reclaimed [0-9.]+MB`, out, "the 3 MB the container wrote")
		out, _ = isolated("ps", "-a")
		require.Contains(t, out, "p-running")
		require.NotContains(t, out, "p-stopped")

		out, code = isolated("prune", "-f")
		require.Zero(t, code)
		require.Contains(t, out, "Nothing to prune.")

		out, _ = isolated("image", "prune", "-a", "-f")
		require.Contains(t, out, "Nothing to prune.", "alpine is still used by p-running")
		_, code = isolated("rm", "-f", "p-running")
		require.Zero(t, code)
		out, code = isolated("image", "prune", "-a", "-f")
		require.Zero(t, code, out)
		require.Contains(t, out, "Deleted alpine:3.21")
		out, _ = isolated("images")
		require.NotContains(t, out, "alpine")
	})

	t.Run("detached --rm removes itself", func(t *testing.T) {
		out, code := ctr("run", "-d", "--rm", "--name", "e2e-drm", alpine, "true")
		require.Zero(t, code, out)
		require.Eventually(t, func() bool {
			out, _ := ctr("ps", "-a")
			return !strings.Contains(out, "e2e-drm")
		}, 30*time.Second, 200*time.Millisecond)
	})
}

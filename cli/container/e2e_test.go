package container

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestContainerRun runs real container VMs: exit codes, the image's config
// and overrides, the network, and the background lifecycle. It pulls
// images, so it only runs with MONOLITH_TEST_CONTAINER=1 and a built
// ../../bin/monolith.
func TestContainerRun(t *testing.T) {
	if os.Getenv("MONOLITH_TEST_CONTAINER") != "1" {
		t.Skip("set MONOLITH_TEST_CONTAINER=1 to run (boots container VMs)")
	}
	binary, err := filepath.Abs("../../bin/monolith")
	require.NoError(t, err)
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}
	monolith := func(args ...string) (string, int) {
		out, err := exec.Command(binary, append([]string{"container"}, args...)...).CombinedOutput()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return string(out), exit.ExitCode()
		}
		require.NoError(t, err)
		return string(out), 0
	}
	const alpine = "alpine:3.21"

	t.Run("exit code and output", func(t *testing.T) {
		out, code := monolith("run", "--rm", alpine, "sh", "-c", "echo hello; exit 3")
		require.Equal(t, 3, code, out)
		require.Contains(t, out, "hello\n", "plain output, no CR")
	})

	t.Run("config and overrides", func(t *testing.T) {
		out, code := monolith("run", "--rm", "-e", "FOO=bar", "-w", "/work/dir", "-u", "nobody",
			"--name", "e2e-config", alpine, "sh", "-c", `echo "$FOO $(pwd) $(id -u) $(hostname) $HOME"`)
		require.Zero(t, code, out)
		require.Contains(t, out, "bar /work/dir 65534 e2e-config /\n")

		out, code = monolith("run", "--rm", "--entrypoint", "printenv", alpine, "PATH")
		require.Zero(t, code, out)
		require.Contains(t, out, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	})

	t.Run("command errors", func(t *testing.T) {
		out, code := monolith("run", "--rm", alpine, "no-such-command")
		require.Equal(t, 127, code, out)
		require.Contains(t, out, "executable file not found")
		_, code = monolith("run", "--rm", alpine, "/etc/passwd")
		require.Equal(t, 126, code)
	})

	t.Run("devices and network", func(t *testing.T) {
		out, code := monolith("run", "--rm", alpine, "sh", "-c",
			`ls -l /dev/stdout /dev/stderr >/dev/null && echo devs-ok; wget -q -O- http://example.com | grep -o '<title>.*</title>'`)
		require.Zero(t, code, out)
		require.Contains(t, out, "devs-ok")
		require.Contains(t, out, "<title>Example Domain</title>")
	})

	t.Run("disk size", func(t *testing.T) {
		// df -k: 1K blocks of /. The filesystem grows to fill --disk at boot.
		size := func(args ...string) int {
			out, code := monolith(append(append([]string{"run", "--rm"}, args...), alpine, "sh", "-c", "df -k / | awk 'NR==2 {print $2}'")...)
			require.Zero(t, code, out)
			kb, err := strconv.Atoi(strings.TrimSpace(lastLine(out)))
			require.NoError(t, err, out)
			return kb
		}
		require.InDelta(t, 8<<20, size(), 0.06*(8<<20), "default 8G")
		require.InDelta(t, 20<<20, size("--disk", "20G"), 0.06*(20<<20))
		out, code := monolith("run", "--rm", "--disk", "100M", alpine, "true")
		require.NotZero(t, code)
		require.Contains(t, out, "--disk must be at least")
	})

	t.Run("--rm removes the container", func(t *testing.T) {
		_, code := monolith("run", "--rm", "--name", "e2e-rm", alpine, "true")
		require.Zero(t, code)
		_, code = monolith("logs", "e2e-rm")
		require.NotZero(t, code, "gone")
	})

	t.Run("background lifecycle", func(t *testing.T) {
		const name = "e2e-web"
		t.Cleanup(func() { monolith("rm", "-f", name) })
		out, code := monolith("run", "-d", "--name", name, "nginx:1.29")
		require.Zero(t, code, out)
		require.Equal(t, name+"\n", lastLine(out))

		require.Eventually(t, func() bool {
			out, _ := monolith("logs", name)
			return strings.Contains(out, "start worker process")
		}, 30*time.Second, 200*time.Millisecond, "nginx logs to /dev/stderr")
		out, _ = monolith("ps")
		require.Regexp(t, name+`\s+nginx:1.29\s+.*Up `, out)

		start := time.Now()
		out, code = monolith("stop", name)
		require.Zero(t, code, out)
		require.Less(t, time.Since(start), 8*time.Second, "SIGQUIT (the image's stop signal) stops nginx at once")
		out, _ = monolith("logs", name)
		require.Contains(t, out, "gracefully shutting down")
		out, _ = monolith("ps", "-a")
		require.Regexp(t, name+`.*Exited \(0\)`, out)

		out, code = monolith("start", name)
		require.Zero(t, code, out)
		out, _ = monolith("ps")
		require.Contains(t, out, name)

		out, code = monolith("rm", name)
		require.NotZero(t, code, "rm refuses a running container")
		require.Contains(t, out, "use -f")
		_, code = monolith("rm", "-f", name)
		require.Zero(t, code)
		out, _ = monolith("ps", "-a")
		require.NotContains(t, out, name)
	})

	t.Run("image inspection", func(t *testing.T) {
		// Read on the Mac, so it works on images without a shell.
		const distroless = "gcr.io/distroless/static-debian12"
		out, code := monolith("image", "cat", distroless, "/etc/passwd")
		require.Zero(t, code, out)
		require.Contains(t, out, "nonroot:x:65532:65532")
		out, code = monolith("image", "ls", distroless, "/")
		require.Zero(t, code, out)
		require.Regexp(t, `(?m)^drwxrwxrwt root +root .* tmp$`, out)

		out, code = monolith("image", "cat", alpine, "/etc/os-release")
		require.Zero(t, code, out)
		require.Contains(t, out, `NAME="Alpine Linux"`, "through the os-release symlink")
		out, code = monolith("image", "inspect", "nginx:1.29")
		require.Zero(t, code, out)
		require.Regexp(t, `Stop signal: +SIGQUIT`, out)
		require.Contains(t, out, "80/tcp")
		out, code = monolith("image", "inspect", "--json", alpine)
		require.Zero(t, code, out)
		require.Contains(t, out, `"Cmd": [`)
		require.NotContains(t, out, `"built": "0001`, "zero times are left out")

		dest := t.TempDir()
		out, code = monolith("image", "cp", "nginx:1.29:/etc/nginx", dest)
		require.Zero(t, code, out)
		require.FileExists(t, filepath.Join(dest, "nginx", "nginx.conf"))
		out, code = monolith("image", "cp", "nginx:1.29:/etc/nginx", dest)
		require.NotZero(t, code)
		require.Contains(t, out, "already exists (use -f)")
	})

	t.Run("prune", func(t *testing.T) {
		// prune removes every stopped container, so this runs against its
		// own cache (sharing the Kata kernel) rather than the user's. Under
		// /tmp: the daemon's socket path must fit macOS's 104 bytes.
		home, err := os.MkdirTemp("/tmp", "ml-")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(home) })
		realHome, err := os.UserHomeDir()
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".cache/monolith"), 0755))
		require.NoError(t, os.Symlink(filepath.Join(realHome, ".cache/monolith/kata"), filepath.Join(home, ".cache/monolith/kata")))
		isolated := func(args ...string) (string, int) {
			cmd := exec.Command(binary, append([]string{"container"}, args...)...)
			cmd.Env = append(os.Environ(), "HOME="+home)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return string(out), exit.ExitCode()
			}
			require.NoError(t, err)
			return string(out), 0
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

		// alpine is used by p-running: image prune -a keeps it until it's gone.
		out, _ = isolated("image", "prune", "-a", "-f")
		require.Contains(t, out, "Nothing to prune.")
		_, code = isolated("rm", "-f", "p-running")
		require.Zero(t, code)
		out, code = isolated("image", "prune", "-a", "-f")
		require.Zero(t, code, out)
		require.Contains(t, out, "Deleted alpine:3.21")
		out, _ = isolated("images")
		require.NotContains(t, out, "alpine")
	})

	t.Run("detached --rm removes itself", func(t *testing.T) {
		out, code := monolith("run", "-d", "--rm", "--name", "e2e-drm", alpine, "true")
		require.Zero(t, code, out)
		require.Eventually(t, func() bool {
			out, _ := monolith("ps", "-a")
			return !strings.Contains(out, "e2e-drm")
		}, 30*time.Second, 200*time.Millisecond)
	})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1] + "\n"
}

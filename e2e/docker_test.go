//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDocker drives Docker host VMs through `monolith docker`: published
// ports (all interfaces, the guest's loopback, below 1024), bind mounts,
// pause/resume, growing the data disk, switching hosts and the containerd
// engine.
func TestDocker(t *testing.T) {
	requireTool(t, "docker")
	const host, other, ctrd = "e2e-dock", "e2e-dock2", "e2e-ctrd"
	t.Cleanup(func() {
		for _, name := range []string{host, other, ctrd} {
			monolith(t, "docker", "rm", "--force", "--name", name)
		}
	})

	mustMonolith(t, "docker", "start", "--name", host)
	env := []string{"DOCKER_HOST=" + dockerHost(t, host)}
	docker := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(must(t, env, "docker", args...))
	}
	// Pulled up front: a pull's progress would otherwise land in the output
	// the subtests compare.
	for _, image := range []string{"alpine:3.21", "busybox", "nginx:1.29"} {
		docker("pull", "-q", image)
	}

	t.Run("published ports", func(t *testing.T) {
		port := freePort(t)
		docker("run", "-d", "--name", "web", "-p", fmt.Sprintf("%d:80", port), "nginx:1.29")
		waitServes(t, port, "nginx", 2*time.Minute)

		loop := freePort(t)
		docker("run", "-d", "--name", "web-lo", "-p", fmt.Sprintf("127.0.0.1:%d:80", loop), "nginx:1.29")
		waitServes(t, loop, "nginx", time.Minute)

		low := freePrivilegedPort(t)
		docker("run", "-d", "--name", "web-low", "-p", fmt.Sprintf("%d:80", low), "nginx:1.29")
		waitServes(t, low, "nginx", time.Minute)

		require.Eventually(t, func() bool {
			out, _ := monolith(t, "docker", "list")
			return strings.Contains(out, strconv.Itoa(port)) && strings.Contains(out, strconv.Itoa(low))
		}, 10*time.Second, time.Second, "docker list shows the forwarded ports")
	})

	t.Run("bind mounts", func(t *testing.T) {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		users, err := os.MkdirTemp(home, "bind-")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(users) })
		require.NoError(t, os.WriteFile(filepath.Join(users, "f"), []byte("from-users"), 0644))
		require.Equal(t, "from-users", docker("run", "--rm", "-v", users+":/d", "alpine:3.21", "cat", "/d/f"))

		tmp, err := os.MkdirTemp(darwinTempDir(t), "e2e-")
		require.NoError(t, err)
		t.Cleanup(func() { os.RemoveAll(tmp) })
		require.NoError(t, os.WriteFile(filepath.Join(tmp, "f"), []byte("from-tmp"), 0644))
		require.Equal(t, "from-tmp", docker("run", "--rm", "-v", tmp+":/d", "alpine:3.21", "cat", "/d/f"))

		out, code := run(t, env, "docker", "run", "--rm", "-v", "/Library:/l", "alpine:3.21", "true")
		require.NotZero(t, code, out)
		require.Contains(t, out, "is not shared")
	})

	t.Run("pause and resume", func(t *testing.T) {
		docker("run", "-d", "--name", "counter", "busybox", "sh", "-c",
			"i=0; while true; do i=$((i+1)); echo $i > /tmp/n; sleep 1; done")
		counter := func() int {
			n, err := strconv.Atoi(docker("exec", "counter", "cat", "/tmp/n"))
			require.NoError(t, err)
			return n
		}
		time.Sleep(3 * time.Second)
		before := counter()

		mustMonolith(t, "docker", "pause", "--name", host)
		time.Sleep(10 * time.Second)
		mustMonolith(t, "docker", "resume", "--name", host)

		after := counter()
		require.GreaterOrEqual(t, after, before, "the container carried on, not restarted")
		require.LessOrEqual(t, after, before+2)
		guest, err := strconv.ParseInt(docker("exec", "counter", "date", "+%s"), 10, 64)
		require.NoError(t, err)
		require.InDelta(t, time.Now().Unix(), guest, 2, "the guest clock caught up")
	})

	t.Run("grow the data disk", func(t *testing.T) {
		mustMonolith(t, "docker", "stop", "--name", host)
		mustMonolith(t, "docker", "start", "--name", host, "--disk", "30G")
		out := mustMonolith(t, "docker", "list")
		require.Regexp(t, host+`\s.*/30 GiB`, out)

		df := docker("run", "--rm", "--privileged", "--pid=host", "alpine:3.21",
			"nsenter", "-t", "1", "-m", "df", "-k", "/var/lib/docker")
		fields := strings.Fields(strings.Split(df, "\n")[1])
		kb, err := strconv.Atoi(fields[1])
		require.NoError(t, err, df)
		require.InDelta(t, 30<<20, kb, 0.06*(30<<20), "the filesystem grew with the disk")

		out, code := monolith(t, "docker", "start", "--name", host, "--disk", "25G")
		require.NotZero(t, code, out)
		require.Contains(t, out, "not shrink")
	})

	t.Run("switch hosts", func(t *testing.T) {
		mustMonolith(t, "docker", "start", "--name", other)
		require.Equal(t, "docker-"+other, contextShow(t))

		mustMonolith(t, "docker", "use", host)
		require.Equal(t, "docker-"+host, contextShow(t))
		require.Equal(t, host, strings.TrimSpace(mustMonolith(t, "docker", "use")))
		names := mustMonolith(t, "docker", "list", "-q")
		require.Contains(t, names, host+"\n")
		require.Contains(t, names, other+"\n")

		mustMonolith(t, "docker", "rm", "--force", "--name", other)
		out := must(t, nil, "docker", "context", "ls", "-q")
		require.NotContains(t, out, "docker-"+other)
		require.Equal(t, "docker-"+host, contextShow(t), "removing another host keeps the current context")
	})

	t.Run("containerd engine", func(t *testing.T) {
		mustMonolith(t, "docker", "start", "--name", ctrd, "--engine", "containerd")
		port := freePort(t)
		mustMonolith(t, "docker", "nerdctl", "--name", ctrd, "run", "-d", "-p", fmt.Sprintf("%d:80", port), "nginx:1.29")
		waitServes(t, port, "nginx", 2*time.Minute)
		mustMonolith(t, "docker", "rm", "--force", "--name", ctrd)
	})
}

// dockerHost is the DOCKER_HOST `monolith docker env` prints for name.
func dockerHost(t *testing.T, name string) string {
	t.Helper()
	out := mustMonolith(t, "docker", "env", "--name", name)
	v, ok := strings.CutPrefix(strings.TrimSpace(out), "export DOCKER_HOST=")
	require.True(t, ok, out)
	return v
}

func contextShow(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(must(t, nil, "docker", "context", "show"))
}

// darwinTempDir is the user's /var/folders temp dir, whatever TMPDIR says.
func darwinTempDir(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("getconf", "DARWIN_USER_TEMP_DIR").Output()
	require.NoError(t, err)
	dir := strings.TrimSpace(string(out))
	require.True(t, strings.HasPrefix(dir, "/var/folders/"), dir)
	return dir
}

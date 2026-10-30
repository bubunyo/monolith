package vm

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	mnet "github.com/bubunyo/monolith/net"

	"github.com/stretchr/testify/require"
)

// TestDockerPauseResume boots a docker image VM, starts a container that
// counts seconds, saves the VM to disk and restores it, and checks that the
// container carried on from where it was and that the guest clock caught up
// with the Mac's.
//
// It installs Docker in the VM on first boot (needs internet) and needs the
// docker CLI and an unlocked Mac, so it only runs with MONOLITH_TEST_DOCKER=1.
// It talks to the VM through DOCKER_HOST and never changes docker contexts.
func TestDockerPauseResume(t *testing.T) {
	if os.Getenv("MONOLITH_TEST_DOCKER") != "1" {
		t.Skip("set MONOLITH_TEST_DOCKER=1 to run (boots a Docker host VM)")
	}
	binary := "../bin/monolith"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not found on PATH")
	}

	name := "test-docker"
	dir, err := VMDir(name)
	require.NoError(t, err)
	t.Cleanup(func() {
		SendControlCommand(name, ControlRequest{Cmd: CmdStop, Force: true})
		os.RemoveAll(dir)
	})

	cfg := DefaultConfig()
	cfg.Image = ImageDocker
	cfg.NetworkMode = mnet.NetworkUser
	cfg.CPUs = 2
	cfg.Memory = 2 << 30
	require.NoError(t, WriteConfig(name, cfg))
	_, err = dockerImage{}.Prepare(name)
	require.NoError(t, err)

	sock, err := VMDockerSockPath(name)
	require.NoError(t, err)
	docker := func(args ...string) (string, error) {
		cmd := exec.Command("docker", args...)
		cmd.Env = append(os.Environ(), "DOCKER_HOST=unix://"+sock, "DOCKER_CONTEXT=")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	spawn := func(args ...string) {
		daemon := exec.Command(binary, append([]string{"__vmd", name}, args...)...)
		require.NoError(t, daemon.Start())
		go daemon.Wait()
	}
	waitDocker := func() {
		require.Eventually(t, func() bool {
			_, err := docker("version")
			return err == nil
		}, 5*time.Minute, time.Second, "dockerd never answered")
	}
	counter := func() int {
		out, err := docker("exec", "counter", "cat", "/tmp/n")
		require.NoError(t, err, out)
		n, err := strconv.Atoi(out)
		require.NoError(t, err, out)
		return n
	}

	spawn()
	waitDocker()
	out, err := docker("run", "-d", "--name", "counter", "busybox", "sh", "-c",
		"i=0; while true; do i=$((i+1)); echo $i > /tmp/n; sleep 1; done")
	require.NoError(t, err, out)
	time.Sleep(3 * time.Second)
	before := counter()

	resp, err := SendControlCommand(name, ControlRequest{Cmd: CmdSuspend})
	require.NoError(t, err)
	require.True(t, resp.OK, resp.Error)
	time.Sleep(10 * time.Second) // long enough for a visible clock gap

	spawn("--restore")
	waitDocker()
	after := counter()
	// Saved mid-sleep, so the counter is where it was or one tick on; a
	// reboot would have lost the container entirely.
	require.GreaterOrEqual(t, after, before)
	require.LessOrEqual(t, after, before+2)

	require.Eventually(t, func() bool {
		out, err := docker("exec", "counter", "date", "+%s")
		if err != nil {
			return false
		}
		guest, err := strconv.ParseInt(out, 10, 64)
		if err != nil {
			return false
		}
		drift := time.Now().Unix() - guest
		return drift >= -2 && drift <= 2
	}, 15*time.Second, time.Second, "guest clock did not catch up after restore")
}

func TestParseNerdctlPorts(t *testing.T) {
	out := `{"Names":"web","Ports":"0.0.0.0:8080->80/tcp, 127.0.0.1:9000->9000/tcp"}
{"Names":"dns","Ports":"0.0.0.0:5353->53/udp"}
{"Names":"v6","Ports":":::8443->443/tcp,[::]:8444->443/tcp"}
{"Names":"none","Ports":""}
`
	ports, err := parseNerdctlPorts([]byte(out))
	require.NoError(t, err)
	require.Equal(t, map[int]bool{8080: true, 8443: true, 8444: true}, ports)

	_, err = parseNerdctlPorts([]byte("not json\n"))
	require.Error(t, err)
}

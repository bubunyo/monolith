package vm

import (
	"bufio"
	"io"
	"net"
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
	_, err = dockerImage{}.Prepare(name, io.Discard)
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
	require.Equal(t, map[portKey]bool{{"tcp", 8080, ""}: true, {"tcp", 9000, "127.0.0.1"}: true, {"udp", 5353, ""}: true, {"tcp", 8443, ""}: true, {"tcp", 8444, ""}: true}, ports)

	_, err = parseNerdctlPorts([]byte("not json\n"))
	require.Error(t, err)
}

// Docker hosts no longer run Kubernetes ('monolith cluster' does); their
// guest files must not start k3s.
func TestDockerGuestHasNoKubernetes(t *testing.T) {
	for _, f := range []string{
		"embeddings/docker/etc/inittab.tmpl",
		"embeddings/containerd/etc/inittab.tmpl",
		"embeddings/docker/usr/local/bin/docker-setup",
		"embeddings/docker/usr/local/bin/docker-serve",
	} {
		data, err := embeddings.ReadFile(f)
		require.NoError(t, err, f)
		require.NotContains(t, strings.ToLower(string(data)), "k3s", f)
	}
}

func TestParseListeningPorts(t *testing.T) {
	out := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1FA1 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2
   2: 0200A8C0:0947 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3
   3: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4
   4: 00000000:1388 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 5
  sl  local_address                         remote_address                        st
   0: 00000000000000000000000000000000:0BB8 00000000000000000000000000000000:0000 0A 0
`
	require.Equal(t, map[portKey]bool{{"tcp", 8097, ""}: true, {"tcp", 3000, ""}: true}, parseListeningPorts([]byte(out)))
}

func TestPublishScope(t *testing.T) {
	tests := []struct {
		ip     string
		bind   string
		wanted bool
	}{
		{"", "", true},
		{"0.0.0.0", "", true},
		{"::", "", true},
		{"[::]", "", true},
		{"127.0.0.1", "127.0.0.1", true},
		{"::1", "::1", true},
		{"[::1]", "::1", true},
		{"10.0.0.5", "", false},
		{"192.168.127.2", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			bind, ok := publishScope(tt.ip)
			require.Equal(t, tt.wanted, ok)
			require.Equal(t, tt.bind, bind)
		})
	}
}

func TestParseNerdctlPortsLoopback(t *testing.T) {
	out := `{"Names":"kind","Ports":"127.0.0.1:52093->6443/tcp, 0.0.0.0:8080->80/tcp"}
{"Names":"v6","Ports":"[::1]:9000->9000/tcp,10.0.0.5:9001->9001/tcp"}
{"Names":"udp","Ports":"127.0.0.1:5353->53/udp"}
`
	ports, err := parseNerdctlPorts([]byte(out))
	require.NoError(t, err)
	require.Equal(t, map[portKey]bool{
		{"tcp", 52093, "127.0.0.1"}: true,
		{"tcp", 8080, ""}:           true,
		{"tcp", 9000, "::1"}:        true,
		{"udp", 5353, "127.0.0.1"}:  true,
	}, ports)
}

func TestLoopbackRequest(t *testing.T) {
	require.Equal(t, "52093\n", portKey{"tcp", 52093, "127.0.0.1"}.loopbackRequest())
	require.Equal(t, "6 9000\n", portKey{"tcp", 9000, "::1"}.loopbackRequest())
}

// TestServeRelayHello checks the request line reaches the guest before the
// client's bytes, then data flows both ways and a half-close is passed on.
func TestServeRelayHello(t *testing.T) {
	guestL, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer guestL.Close()
	hostL, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer hostL.Close()

	go serveRelay(hostL, "52093\n", func() (net.Conn, error) {
		return net.Dial("tcp", guestL.Addr().String())
	})

	guestDone := make(chan string, 1)
	go func() {
		c, err := guestL.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		line, _ := br.ReadString('\n')
		// Read the rest until the client half-closes, then answer. From br:
		// the client's bytes can arrive in the same read as the line.
		rest, _ := io.ReadAll(br)
		c.Write([]byte("pong"))
		guestDone <- line + "|" + string(rest)
	}()

	client, err := net.Dial("tcp", hostL.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	_, err = client.Write([]byte("ping"))
	require.NoError(t, err)
	require.NoError(t, client.(*net.TCPConn).CloseWrite())

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply, err := io.ReadAll(client)
	require.NoError(t, err)
	require.Equal(t, "pong", string(reply))

	select {
	case got := <-guestDone:
		require.Equal(t, "52093\n|ping", got)
	case <-time.After(5 * time.Second):
		t.Fatal("guest never saw the request")
	}
}

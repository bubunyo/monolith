package vm

import (
	"bufio"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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

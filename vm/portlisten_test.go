package vm

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestListenHost(t *testing.T) {
	tests := []struct {
		name string
		goos string
		port int
		want string
	}{
		{"darwin privileged", "darwin", 443, "0.0.0.0"},
		{"darwin lowest", "darwin", 1, "0.0.0.0"},
		{"darwin last privileged", "darwin", 1023, "0.0.0.0"},
		{"darwin first unprivileged", "darwin", 1024, "127.0.0.1"},
		{"darwin high", "darwin", 28080, "127.0.0.1"},
		{"linux privileged", "linux", 443, "127.0.0.1"},
		{"linux high", "linux", 28080, "127.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, listenHost(tt.goos, tt.port))
		})
	}
}

func TestLoopbackOnlyListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l := loopbackOnly(inner)

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	defer client.Close()

	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("loopback client was not accepted")
	}
	defer server.Close()

	_, err = client.Write([]byte("ping"))
	require.NoError(t, err)
	buf := make([]byte, 4)
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = server.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))

	_, err = server.Write([]byte("pong"))
	require.NoError(t, err)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = client.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "pong", string(buf))

	require.NoError(t, l.Close())
	_, err = l.Accept()
	require.Error(t, err)
}

// fakeConn reports a fixed remote address.
type fakeConn struct {
	net.Conn
	remote net.Addr
	closed bool
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.remote }
func (c *fakeConn) Close() error         { c.closed = true; return nil }

// scriptedListener hands out preset connections, then errors.
type scriptedListener struct {
	net.Listener
	conns []net.Conn
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	if len(s.conns) == 0 {
		return nil, net.ErrClosed
	}
	c := s.conns[0]
	s.conns = s.conns[1:]
	return c, nil
}

func TestLoopbackOnlyDropsRemote(t *testing.T) {
	tests := []struct {
		name   string
		remote net.Addr
		keep   bool
	}{
		{"ipv4 loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, true},
		{"ipv6 loopback", &net.TCPAddr{IP: net.IPv6loopback}, true},
		{"lan", &net.TCPAddr{IP: net.ParseIP("192.168.1.20")}, false},
		{"non-tcp", &net.UnixAddr{Name: "x", Net: "unix"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := &fakeConn{remote: tt.remote}
			good := &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}}
			l := loopbackOnly(&scriptedListener{conns: []net.Conn{remote, good}})

			got, err := l.Accept()
			require.NoError(t, err)
			if tt.keep {
				require.Same(t, remote, got)
				require.False(t, remote.closed)
				return
			}
			require.True(t, remote.closed, "foreign connection must be closed")
			require.Same(t, good, got)
		})
	}
}

func TestMultiListener(t *testing.T) {
	a, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	b, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	m := newMultiListener(a, b)

	for _, addr := range []string{a.Addr().String(), b.Addr().String()} {
		client, err := net.Dial("tcp", addr)
		require.NoError(t, err)
		defer client.Close()
		conn, err := m.Accept()
		require.NoError(t, err)
		conn.Close()
	}

	require.NoError(t, m.Close())
	_, err = m.Accept()
	require.Error(t, err)
	require.NoError(t, m.Close(), "Close is idempotent")
}

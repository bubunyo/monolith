package vm

import (
	"net"
	"runtime"
	"strconv"
	"sync"
)

// privilegedPortLimit is the first port a normal user may bind on a specific
// address. On macOS (since Mojave) ports below it can be bound unprivileged
// only on the wildcard address.
const privilegedPortLimit = 1024

// wildcardHost is the macOS listen host for privileged ports; see listenHost.
const wildcardHost = "0.0.0.0"

// listenHost returns the host a localhost forward of port listens on. macOS
// refuses to bind a port below 1024 on 127.0.0.1 without root but allows the
// wildcard address, so those ports listen on 0.0.0.0 and the listener is
// wrapped with loopbackOnly. Everything else binds 127.0.0.1.
func listenHost(goos string, port int) string {
	if goos == "darwin" && port < privilegedPortLimit {
		return wildcardHost
	}
	return "127.0.0.1"
}

// listenLocalTCP listens for a localhost forward of port. For a privileged
// port on macOS it binds the wildcard address and drops connections that
// don't come from loopback (the same pseudo-loopback approach as Lima). The
// IPv6 wildcard is bound too so localhost works over ::1; failing to bind it
// is not an error.
func listenLocalTCP(port int) (net.Listener, error) {
	host := listenHost(runtime.GOOS, port)
	l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	if host != wildcardHost {
		return l, nil
	}
	l6, err := net.Listen("tcp6", net.JoinHostPort("::", strconv.Itoa(port)))
	if err != nil {
		return loopbackOnly(l), nil
	}
	return newMultiListener(loopbackOnly(l), loopbackOnly(l6)), nil
}

// loopbackOnly wraps l so Accept only returns connections from a loopback
// address; any other connection is closed. Accept errors pass through.
func loopbackOnly(l net.Listener) net.Listener { return loopbackListener{l} }

type loopbackListener struct{ net.Listener }

func (l loopbackListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok && addr.IP.IsLoopback() {
			return conn, nil
		}
		conn.Close()
	}
}

// multiListener accepts from several listeners as one. The first Accept error
// from any of them is returned, which in practice means Close was called.
type multiListener struct {
	listeners []net.Listener
	results   chan acceptResult
	done      chan struct{}
	closeOnce sync.Once
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func newMultiListener(ls ...net.Listener) *multiListener {
	m := &multiListener{
		listeners: ls,
		results:   make(chan acceptResult),
		done:      make(chan struct{}),
	}
	for _, l := range ls {
		go m.acceptLoop(l)
	}
	return m
}

func (m *multiListener) acceptLoop(l net.Listener) {
	for {
		conn, err := l.Accept()
		select {
		case m.results <- acceptResult{conn, err}:
		case <-m.done:
			if conn != nil {
				conn.Close()
			}
			return
		}
		if err != nil {
			return
		}
	}
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case r := <-m.results:
		return r.conn, r.err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	var first error
	m.closeOnce.Do(func() {
		close(m.done)
		for _, l := range m.listeners {
			if err := l.Close(); err != nil && first == nil {
				first = err
			}
		}
	})
	return first
}

func (m *multiListener) Addr() net.Addr { return m.listeners[0].Addr() }

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

// portPollInterval is how often the daemon asks dockerd for published ports.
const portPollInterval = time.Second

// dockerServices exposes a docker image VM's dockerd on the host: a unix
// socket at VMDockerSockPath, and a localhost listener for every TCP port a
// container publishes. Both relay to the guest through the user-mode
// network. (vsock would avoid the network, but vz's vsock connections can't
// half-close, which `docker run -i` needs to deliver stdin EOF.)
type dockerServices struct {
	name     string
	user     *mnet.UserNetwork
	logger   *monolith.Logger
	listener net.Listener
	client   *http.Client
	cancel   context.CancelFunc

	mu     sync.Mutex
	ports  map[int]net.Listener // published host port -> localhost listener
	failed map[int]bool         // ports already logged as unavailable
}

// startDockerServices starts the docker.sock relay and the port forwarder.
func startDockerServices(name string, user *mnet.UserNetwork, logger *monolith.Logger) (*dockerServices, error) {
	if user == nil {
		return nil, fmt.Errorf("the docker image requires user-mode networking")
	}
	sockPath, err := VMDockerSockPath(name)
	if err != nil {
		return nil, err
	}
	os.Remove(sockPath) // stale socket from a crashed daemon
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", sockPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &dockerServices{
		name:     name,
		user:     user,
		logger:   logger,
		listener: listener,
		cancel:   cancel,
		ports:    map[int]net.Listener{},
		failed:   map[int]bool{},
	}
	s.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return s.dialDockerd(ctx)
			},
		},
	}

	go s.serveSocket(ctx)
	go s.forwardPorts(ctx)
	logger.Info("docker API for %q on %s", name, sockPath)
	return s, nil
}

// dialTimeout bounds connecting to the guest through the user-mode network.
const dialTimeout = 10 * time.Second

func (s *dockerServices) dialDockerd(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return s.user.DialGuest(ctx, DockerAPIPort)
}

// serveSocket relays each docker.sock connection to dockerd. Connections
// fail while dockerd is still starting (the guest relay isn't listening).
func (s *dockerServices) serveSocket(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go func() {
			guest, err := s.dialDockerd(ctx)
			if err != nil {
				conn.Close()
				return
			}
			relay(conn, guest)
		}()
	}
}

// publishedPort is the subset of GET /containers/json we use.
type publishedPort struct {
	IP         string `json:"IP"`
	PublicPort int    `json:"PublicPort"`
	Type       string `json:"Type"`
}

// forwardPorts keeps one localhost listener per published TCP port. dockerd
// publishes on the guest; each listener relays to the same port on the
// guest through the user-mode network.
func (s *dockerServices) forwardPorts(ctx context.Context) {
	ticker := time.NewTicker(portPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		want, err := s.publishedPorts()
		if err != nil {
			continue // dockerd not up yet, or restarting
		}
		s.mu.Lock()
		for port := range s.failed {
			if !want[port] {
				delete(s.failed, port)
			}
		}
		for port, l := range s.ports {
			if !want[port] {
				l.Close()
				delete(s.ports, port)
				s.logger.Info("stopped forwarding localhost:%d", port)
			}
		}
		for port := range want {
			if _, ok := s.ports[port]; ok {
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				// Retried on every poll; log only the first failure.
				if !s.failed[port] {
					s.failed[port] = true
					s.logger.Warn("cannot forward localhost:%d: %v", port, err)
				}
				continue
			}
			delete(s.failed, port)
			s.ports[port] = l
			go s.servePort(ctx, l, port)
			s.logger.Info("forwarding localhost:%d to %s:%d", port, mnet.UserGuestIP, port)
		}
		s.mu.Unlock()
	}
}

// publishedPorts returns the host ports published on all guest interfaces.
// Ports bound to a specific guest address (-p 127.0.0.1:80:80) aren't
// reachable from the gateway, so they are skipped.
func (s *dockerServices) publishedPorts() (map[int]bool, error) {
	resp, err := s.client.Get("http://docker/containers/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("containers/json: %s", resp.Status)
	}
	var containers []struct {
		Ports []publishedPort `json:"Ports"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	ports := map[int]bool{}
	for _, c := range containers {
		for _, p := range c.Ports {
			if p.Type != "tcp" || p.PublicPort == 0 {
				continue
			}
			if p.IP != "" && p.IP != "0.0.0.0" && p.IP != "::" {
				continue
			}
			ports[p.PublicPort] = true
		}
	}
	return ports, nil
}

func (s *dockerServices) servePort(ctx context.Context, l net.Listener, port int) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		go func() {
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			guest, err := s.user.DialGuest(dialCtx, port)
			if err != nil {
				conn.Close()
				return
			}
			relay(conn, guest)
		}()
	}
}

// close stops all listeners and removes docker.sock.
func (s *dockerServices) close() {
	s.cancel()
	s.listener.Close()
	if sockPath, err := VMDockerSockPath(s.name); err == nil {
		os.Remove(sockPath)
	}
	s.mu.Lock()
	for _, l := range s.ports {
		l.Close()
	}
	s.mu.Unlock()
}

// relay copies between a and b until both directions finish, half-closing
// each side as its source ends (docker attach relies on this for stdin EOF).
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

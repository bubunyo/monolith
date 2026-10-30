package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

// portPollInterval is how often the daemon asks the engine for published
// ports.
const portPollInterval = time.Second

// dockerServices exposes a docker image VM's engine on the host: unix
// sockets relayed to guest ports (docker.sock to dockerd's API for the
// docker engine, ssh.sock to the internal sshd for containerd), and a
// localhost listener for every TCP port a container publishes. All of it
// relays to the guest through the user-mode network. (vsock would avoid the
// network, but vz's vsock connections can't half-close, which
// `docker run -i` needs to deliver stdin EOF.)
type dockerServices struct {
	name      string
	user      *mnet.UserNetwork
	logger    *monolith.Logger
	sockets   []string // host socket paths, removed on close
	listeners []net.Listener
	client    *http.Client
	cancel    context.CancelFunc

	// publishedPorts returns the host ports published on all guest
	// interfaces, from dockerd's API or nerdctl.
	publishedPorts func() (map[int]bool, error)

	mu     sync.Mutex
	ports  map[int]net.Listener // published host port -> localhost listener
	failed map[int]string       // published ports that couldn't be bound -> error
}

// startDockerServices starts the engine's socket relays and the port
// forwarder.
func startDockerServices(name, engine string, user *mnet.UserNetwork, logger *monolith.Logger) (*dockerServices, error) {
	if user == nil {
		return nil, fmt.Errorf("the docker image requires user-mode networking")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &dockerServices{
		name:   name,
		user:   user,
		logger: logger,
		cancel: cancel,
		ports:  map[int]net.Listener{},
		failed: map[int]string{},
	}

	var sockPath string
	var sockPort int
	var err error
	switch engine {
	case EngineContainerd:
		sockPath, err = VMSSHSockPath(name)
		sockPort = SSHPort
		s.publishedPorts = s.nerdctlPorts
	default:
		sockPath, err = VMDockerSockPath(name)
		sockPort = DockerAPIPort
		s.publishedPorts = s.dockerPorts
		s.client = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return s.dialGuest(ctx, DockerAPIPort)
				},
			},
		}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if err := s.relaySocket(ctx, sockPath, sockPort); err != nil {
		s.close()
		return nil, err
	}

	go s.forwardPorts(ctx)
	logger.Info("%s engine for %q: %s relays to guest port %d", engine, name, sockPath, sockPort)
	return s, nil
}

// dialTimeout bounds connecting to the guest through the user-mode network.
const dialTimeout = 10 * time.Second

func (s *dockerServices) dialGuest(ctx context.Context, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return s.user.DialGuest(ctx, port)
}

// relaySocket listens on a host unix socket and relays each connection to
// port on the guest. Connections fail while the guest service is still
// starting (nothing is listening yet).
func (s *dockerServices) relaySocket(ctx context.Context, path string, port int) error {
	os.Remove(path) // stale socket from a crashed daemon
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", path, err)
	}
	s.sockets = append(s.sockets, path)
	s.listeners = append(s.listeners, l)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return // listener closed
			}
			go func() {
				guest, err := s.dialGuest(ctx, port)
				if err != nil {
					conn.Close()
					return
				}
				relay(conn, guest)
			}()
		}
	}()
	return nil
}

// publishedPort is the subset of GET /containers/json we use.
type publishedPort struct {
	IP         string `json:"IP"`
	PublicPort int    `json:"PublicPort"`
	Type       string `json:"Type"`
}

// forwardPorts keeps one localhost listener per published TCP port. The
// engine publishes on the guest; each listener relays to the same port on the
// guest through the user-mode network.
// The first sync runs immediately, so after a restore the forwards are back
// as soon as dockerd answers.
func (s *dockerServices) forwardPorts(ctx context.Context) {
	ticker := time.NewTicker(portPollInterval)
	defer ticker.Stop()
	for {
		s.syncPorts(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *dockerServices) syncPorts(ctx context.Context) {
	want, err := s.publishedPorts()
	if err != nil {
		return // engine not up yet, or restarting
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
			if _, logged := s.failed[port]; !logged {
				s.logger.Warn("cannot forward localhost:%d: %v", port, err)
			}
			s.failed[port] = err.Error()
			continue
		}
		delete(s.failed, port)
		s.ports[port] = l
		go s.servePort(ctx, l, port)
		s.logger.Info("forwarding localhost:%d to %s:%d", port, mnet.UserGuestIP, port)
	}
}

// forwards lists the published ports, sorted, with the bind error for any
// that couldn't be forwarded (typically another VM already holds the port).
func (s *dockerServices) forwards() []PortForward {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PortForward
	for port := range s.ports {
		out = append(out, PortForward{Port: port})
	}
	for port, msg := range s.failed {
		out = append(out, PortForward{Port: port, Error: msg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// dockerPorts returns the TCP ports published on all guest interfaces, from
// dockerd's API. Ports bound to a specific guest address
// (-p 127.0.0.1:80:80) aren't reachable from the gateway, so they are
// skipped.
func (s *dockerServices) dockerPorts() (map[int]bool, error) {
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
			if !allInterfaces(p.IP) {
				continue
			}
			ports[p.PublicPort] = true
		}
	}
	return ports, nil
}

func allInterfaces(ip string) bool {
	return ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]"
}

// nerdctlPorts returns the TCP ports published on all guest interfaces, from
// `nerdctl ps --format json` (one JSON object per line), which the guest
// serves on NerdctlPortsPort. nerdctl publishes with CNI's portmap plugin
// (iptables DNAT), which applies to the forwarded connections because they
// arrive on eth0.
func (s *dockerServices) nerdctlPorts() (map[int]bool, error) {
	conn, err := s.dialGuest(context.Background(), NerdctlPortsPort)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	return parseNerdctlPorts(data)
}

// nerdctlPortRE matches one entry of nerdctl's Ports column, e.g.
// "0.0.0.0:8080->80/tcp" or "127.0.0.1:9000->9000/udp".
var nerdctlPortRE = regexp.MustCompile(`^(?:(.*):)?(\d+)->\d+/(tcp|udp|sctp)$`)

func parseNerdctlPorts(data []byte) (map[int]bool, error) {
	ports := map[int]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var c struct {
			Ports string `json:"Ports"`
		}
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("nerdctl ps: %w", err)
		}
		for _, entry := range strings.Split(c.Ports, ",") {
			m := nerdctlPortRE.FindStringSubmatch(strings.TrimSpace(entry))
			if m == nil || m[3] != "tcp" || !allInterfaces(m[1]) {
				continue
			}
			if port, err := strconv.Atoi(m[2]); err == nil && port > 0 {
				ports[port] = true
			}
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

// close stops all listeners and removes the host sockets.
func (s *dockerServices) close() {
	s.cancel()
	for _, l := range s.listeners {
		l.Close()
	}
	for _, path := range s.sockets {
		os.Remove(path)
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

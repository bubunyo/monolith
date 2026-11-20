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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

// dockerServices exposes a docker image VM's engine on the host: unix
// sockets relayed to guest ports (docker.sock to dockerd's API for the
// docker engine, ssh.sock to the internal sshd for containerd), and a
// localhost forward for every port a container publishes. All of it relays
// to the guest through the user-mode network. (vsock would avoid the
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
	fwd       *portForwarder // published ports: all guest interfaces or its loopback
}

// startDockerServices starts the engine's socket relay and the port
// forwarder.
func startDockerServices(name string, cfg VMConfig, user *mnet.UserNetwork, logger *monolith.Logger) (*dockerServices, error) {
	engine := EngineOf(cfg)
	if user == nil {
		return nil, fmt.Errorf("the docker image requires user-mode networking")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &dockerServices{
		name:   name,
		user:   user,
		logger: logger,
		cancel: cancel,
	}

	var sockPath string
	var sockPort int
	var published func() (map[portKey]bool, error)
	var err error
	switch engine {
	case EngineContainerd:
		sockPath, err = VMSSHSockPath(name)
		sockPort = SSHPort
		published = s.nerdctlPorts
	default:
		sockPath, err = VMDockerSockPath(name)
		sockPort = DockerAPIPort
		published = s.dockerPorts
		s.client = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialGuest(ctx, user, DockerAPIPort)
				},
			},
		}
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if cfg.Sandbox != nil {
		// The sandbox shell runs with the VM's network, so its servers
		// show up as listening sockets, not published container ports.
		published = s.listeningPorts
	}
	s.fwd = newPortForwarder(user, logger, published)
	relayFn := relay
	if engine == EngineDocker && cfg.Sandbox == nil {
		// Refuse bind mounts of Mac paths the guest can't see instead of
		// letting the container get an empty directory. (containerd's
		// nerdctl runs in the guest over SSH, out of the daemon's view.)
		roots := sharedRoots(cfg)
		relayFn = func(client, guest net.Conn) {
			relayDockerAPI(client, guest, func(body []byte) error {
				return checkBindSources(body, name, roots, pathExists)
			})
		}
	}
	if err := s.relaySocket(ctx, sockPath, sockPort, relayFn); err != nil {
		s.close()
		return nil, err
	}

	go s.fwd.run(ctx)
	if cfg.Sandbox != nil {
		go s.relayFileEvents(ctx, cfg.Sandbox.Repo, true)
	} else {
		go s.relayFileEvents(ctx, "/Users", false)
	}
	logger.Info("%s engine for %q: %s relays to guest port %d", engine, name, sockPath, sockPort)
	return s, nil
}

func (s *dockerServices) dialGuest(ctx context.Context, port int) (net.Conn, error) {
	return dialGuest(ctx, s.user, port)
}

// relaySocket listens on a host unix socket and relays each connection to
// port on the guest. Connections fail while the guest service is still
// starting (nothing is listening yet).
func (s *dockerServices) relaySocket(ctx context.Context, path string, port int, relayFn func(client, guest net.Conn)) error {
	os.Remove(path) // stale socket from a crashed daemon
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", path, err)
	}
	s.sockets = append(s.sockets, path)
	s.listeners = append(s.listeners, l)
	go s.serveListener(ctx, l, port, relayFn)
	return nil
}

// serveListener relays each connection accepted on l to port on the guest
// with relayFn (relay, or the Docker API relay that checks bind mounts).
// Published-port forwards use portForwarder instead.
func (s *dockerServices) serveListener(ctx context.Context, l net.Listener, port int, relayFn func(client, guest net.Conn)) {
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
			relayFn(conn, guest)
		}()
	}
}

// publishScope classifies the address a container port is published on:
// all interfaces ("") and the guest loopback ("127.0.0.1", "::1") are
// forwarded; any other specific address isn't reachable from the host.
func publishScope(ip string) (bind string, ok bool) {
	switch ip {
	case "", "0.0.0.0", "::", "[::]":
		return "", true
	case "127.0.0.1":
		return "127.0.0.1", true
	case "::1", "[::1]":
		return "::1", true
	}
	return "", false
}

// forwards lists the published ports and their forwarding errors.
func (s *dockerServices) forwards() []PortForward {
	return s.fwd.forwards()
}

// publishedPort is the subset of GET /containers/json we use.
type publishedPort struct {
	IP         string `json:"IP"`
	PublicPort int    `json:"PublicPort"`
	Type       string `json:"Type"`
}

// dockerPorts returns the TCP and UDP ports published on all guest
// interfaces or on its loopback, from dockerd's API. Ports on any other
// specific guest address aren't reachable from the host, so they are skipped.
func (s *dockerServices) dockerPorts() (map[portKey]bool, error) {
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
	ports := map[portKey]bool{}
	for _, c := range containers {
		for _, p := range c.Ports {
			if (p.Type != "tcp" && p.Type != "udp") || p.PublicPort == 0 {
				continue
			}
			if bind, ok := publishScope(p.IP); ok {
				ports[portKey{p.Type, p.PublicPort, bind}] = true
			}
		}
	}
	return ports, nil
}

// listeningPorts returns the TCP ports a sandbox's processes listen on (all
// interfaces), from the guest's /proc/net/tcp{,6} served on
// ListeningPortsPort.
func (s *dockerServices) listeningPorts() (map[portKey]bool, error) {
	conn, err := s.dialGuest(context.Background(), ListeningPortsPort)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	data, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	return parseListeningPorts(data), nil
}

// nerdctlPorts returns the TCP and UDP ports published on all guest
// interfaces, from `nerdctl ps --format json` (one JSON object per line),
// which the guest serves on NerdctlPortsPort. nerdctl publishes with CNI's
// portmap plugin (iptables DNAT), which applies to the forwarded traffic
// because it arrives on eth0.
func (s *dockerServices) nerdctlPorts() (map[portKey]bool, error) {
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

func parseNerdctlPorts(data []byte) (map[portKey]bool, error) {
	ports := map[portKey]bool{}
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
			if m == nil || m[3] == "sctp" {
				continue
			}
			bind, ok := publishScope(m[1])
			if !ok {
				continue
			}
			if port, err := strconv.Atoi(m[2]); err == nil && port > 0 {
				ports[portKey{m[3], port, bind}] = true
			}
		}
	}
	return ports, nil
}

// close stops all listeners and forwards and removes the host sockets.
func (s *dockerServices) close() {
	s.cancel()
	for _, l := range s.listeners {
		l.Close()
	}
	for _, path := range s.sockets {
		os.Remove(path)
	}
	if s.fwd != nil {
		s.fwd.stop()
	}
}

// relay copies between a and b until both directions finish, half-closing
// each side as its source ends (docker attach relies on this for stdin EOF).
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		closeWrite(dst)
	}
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// closeWrite half-closes c, or closes it if it can't half-close.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}

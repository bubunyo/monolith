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
	publishedPorts func() (map[portKey]bool, error)

	mu     sync.Mutex
	ports  map[portKey]func() // forwarded port -> stops its forward
	failed map[portKey]string // published ports that couldn't be forwarded -> error
}

// startDockerServices starts the engine's socket relays and the port
// forwarder, plus the Kubernetes API forward when k3s is on.
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
		ports:  map[portKey]func(){},
		failed: map[portKey]string{},
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
	if cfg.Kubernetes {
		if cfg.KubePort == 0 {
			s.close()
			return nil, fmt.Errorf("kubernetes is on but no kube_port is configured")
		}
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.KubePort)))
		if err != nil {
			s.close()
			return nil, fmt.Errorf("forwarding the Kubernetes API on localhost:%d: %w", cfg.KubePort, err)
		}
		s.listeners = append(s.listeners, l)
		go s.serveListener(ctx, l, KubeAPIPort)
		engineHostPorts := s.publishedPorts
		s.publishedPorts = func() (map[portKey]bool, error) {
			ports, err := engineHostPorts()
			if err != nil {
				return nil, err
			}
			// Services come and go with k3s; the engine's ports still
			// count while it's starting or restarting.
			if svc, err := s.kubeServicePorts(); err == nil {
				for p := range svc {
					ports[p] = true
				}
			}
			return ports, nil
		}
		logger.Info("Kubernetes API for %q on localhost:%d", name, cfg.KubePort)
	}

	go s.forwardPorts(ctx)
	go s.relayFileEvents(ctx, "/Users")
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
	go s.serveListener(ctx, l, port)
	return nil
}

// serveListener relays each connection accepted on l to port on the guest.
func (s *dockerServices) serveListener(ctx context.Context, l net.Listener, port int) {
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
}

// k8sInfo asks the guest's k8s-info relay for "kubeconfig" or "services".
func (s *dockerServices) k8sInfo(what string) ([]byte, error) {
	conn, err := s.dialGuest(context.Background(), K8sInfoPort)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, what+"\n"); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("k3s returned no %s (still starting?)", what)
	}
	return data, nil
}

// kubeconfig returns k3s's admin kubeconfig as written in the guest (server
// https://127.0.0.1:6443).
func (s *dockerServices) kubeconfig() ([]byte, error) {
	return s.k8sInfo("kubeconfig")
}

// kubeServicePorts returns the ports Kubernetes services expose on the node:
// LoadBalancer ports (served by k3s's servicelb on the node) and NodePorts.
func (s *dockerServices) kubeServicePorts() (map[portKey]bool, error) {
	data, err := s.k8sInfo("services")
	if err != nil {
		return nil, err
	}
	return parseKubeServicePorts(data)
}

func parseKubeServicePorts(data []byte) (map[portKey]bool, error) {
	var list struct {
		Items []struct {
			Spec struct {
				Type  string `json:"type"`
				Ports []struct {
					Protocol string `json:"protocol"`
					Port     int    `json:"port"`
					NodePort int    `json:"nodePort"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("kubectl get svc: %w", err)
	}
	ports := map[portKey]bool{}
	for _, svc := range list.Items {
		for _, p := range svc.Spec.Ports {
			proto := strings.ToLower(p.Protocol)
			if proto == "" {
				proto = "tcp"
			}
			if proto != "tcp" && proto != "udp" {
				continue
			}
			switch svc.Spec.Type {
			case "LoadBalancer":
				ports[portKey{proto, p.Port}] = true
				if p.NodePort > 0 {
					ports[portKey{proto, p.NodePort}] = true
				}
			case "NodePort":
				if p.NodePort > 0 {
					ports[portKey{proto, p.NodePort}] = true
				}
			}
		}
	}
	return ports, nil
}

// portKey is a published port and its protocol ("tcp" or "udp").
type portKey struct {
	Proto string
	Port  int
}

func (k portKey) String() string { return fmt.Sprintf("%d/%s", k.Port, k.Proto) }

// publishedPort is the subset of GET /containers/json we use.
type publishedPort struct {
	IP         string `json:"IP"`
	PublicPort int    `json:"PublicPort"`
	Type       string `json:"Type"`
}

// forwardPorts keeps a localhost forward for every published port. The
// engine publishes on the guest; TCP forwards are listeners relaying through
// DialGuest, UDP forwards use the user-mode network's port forwarder.
// The first sync runs immediately, so after a restore the forwards are back
// as soon as the engine answers.
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
	for key := range s.failed {
		if !want[key] {
			delete(s.failed, key)
		}
	}
	for key, stop := range s.ports {
		if !want[key] {
			stop()
			delete(s.ports, key)
			s.logger.Info("stopped forwarding localhost:%s", key)
		}
	}
	for key := range want {
		if _, ok := s.ports[key]; ok {
			continue
		}
		stop, err := s.forward(ctx, key)
		if err != nil {
			// Retried on every poll; log only the first failure.
			if _, logged := s.failed[key]; !logged {
				s.logger.Warn("cannot forward localhost:%s: %v", key, err)
			}
			s.failed[key] = err.Error()
			continue
		}
		delete(s.failed, key)
		s.ports[key] = stop
		s.logger.Info("forwarding localhost:%s to %s:%d", key, mnet.UserGuestIP, key.Port)
	}
}

// forward starts forwarding localhost:key to the same port on the guest and
// returns a func that stops it.
func (s *dockerServices) forward(ctx context.Context, key portKey) (func(), error) {
	if key.Proto == "udp" {
		if err := s.user.ExposeUDP(key.Port, key.Port); err != nil {
			return nil, err
		}
		return func() { s.user.UnexposeUDP(key.Port) }, nil
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(key.Port)))
	if err != nil {
		return nil, err
	}
	go s.serveListener(ctx, l, key.Port)
	return func() { l.Close() }, nil
}

// forwards lists the published ports, sorted, with the bind error for any
// that couldn't be forwarded (typically another VM already holds the port).
func (s *dockerServices) forwards() []PortForward {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PortForward
	for key := range s.ports {
		out = append(out, PortForward{Port: key.Port, Protocol: key.Proto})
	}
	for key, msg := range s.failed {
		out = append(out, PortForward{Port: key.Port, Protocol: key.Proto, Error: msg})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out
}

// dockerPorts returns the TCP and UDP ports published on all guest
// interfaces, from dockerd's API. Ports bound to a specific guest address
// (-p 127.0.0.1:80:80) aren't reachable from the gateway, so they are
// skipped.
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
			if (p.Type != "tcp" && p.Type != "udp") || p.PublicPort == 0 || !allInterfaces(p.IP) {
				continue
			}
			ports[portKey{p.Type, p.PublicPort}] = true
		}
	}
	return ports, nil
}

func allInterfaces(ip string) bool {
	return ip == "" || ip == "0.0.0.0" || ip == "::" || ip == "[::]"
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
			if m == nil || m[3] == "sctp" || !allInterfaces(m[1]) {
				continue
			}
			if port, err := strconv.Atoi(m[2]); err == nil && port > 0 {
				ports[portKey{m[3], port}] = true
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
	s.mu.Lock()
	for _, stop := range s.ports {
		stop()
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

package vm

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

// portPollInterval is how often the daemon asks the guest which ports to
// forward.
const portPollInterval = time.Second

// dialTimeout bounds connecting to the guest through the user-mode network.
const dialTimeout = 10 * time.Second

// portKey is a forwarded port and its protocol ("tcp" or "udp").
type portKey struct {
	Proto string
	Port  int
}

func (k portKey) String() string { return fmt.Sprintf("%d/%s", k.Port, k.Proto) }

// portForwarder keeps a localhost forward for every port the guest
// publishes: docker hosts' container ports, clusters' LoadBalancer and
// NodePort services. TCP forwards are 127.0.0.1 listeners relaying through
// DialGuest; UDP forwards use the user-mode network's port forwarder.
type portForwarder struct {
	user   *mnet.UserNetwork
	logger *monolith.Logger

	// published returns the guest ports to forward. An error leaves the
	// current forwards as they are (the guest service may be restarting).
	published func() (map[portKey]bool, error)

	mu     sync.Mutex
	ports  map[portKey]func() // forwarded port -> stops its forward
	failed map[portKey]string // ports that couldn't be forwarded -> error
}

func newPortForwarder(user *mnet.UserNetwork, logger *monolith.Logger, published func() (map[portKey]bool, error)) *portForwarder {
	return &portForwarder{
		user:      user,
		logger:    logger,
		published: published,
		ports:     map[portKey]func(){},
		failed:    map[portKey]string{},
	}
}

// run syncs the forwards every portPollInterval until ctx is done. The first
// sync runs immediately, so after a restore the forwards are back as soon as
// the guest answers.
func (f *portForwarder) run(ctx context.Context) {
	ticker := time.NewTicker(portPollInterval)
	defer ticker.Stop()
	for {
		f.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (f *portForwarder) sync(ctx context.Context) {
	want, err := f.published()
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.failed {
		if !want[key] {
			delete(f.failed, key)
		}
	}
	for key, stop := range f.ports {
		if !want[key] {
			stop()
			delete(f.ports, key)
			f.logger.Info("stopped forwarding localhost:%s", key)
		}
	}
	for key := range want {
		if _, ok := f.ports[key]; ok {
			continue
		}
		stop, err := f.forward(ctx, key)
		if err != nil {
			// Retried on every poll; log only the first failure.
			if _, logged := f.failed[key]; !logged {
				f.logger.Warn("cannot forward localhost:%s: %v", key, err)
			}
			f.failed[key] = err.Error()
			continue
		}
		delete(f.failed, key)
		f.ports[key] = stop
		f.logger.Info("forwarding localhost:%s to %s:%d", key, mnet.UserGuestIP, key.Port)
	}
}

// forward starts forwarding localhost:key to the same port on the guest and
// returns a func that stops it.
func (f *portForwarder) forward(ctx context.Context, key portKey) (func(), error) {
	if key.Proto == "udp" {
		if err := f.user.ExposeUDP(key.Port, key.Port); err != nil {
			return nil, err
		}
		return func() { f.user.UnexposeUDP(key.Port) }, nil
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(key.Port)))
	if err != nil {
		return nil, err
	}
	go serveGuest(ctx, f.user, l, key.Port)
	return func() { l.Close() }, nil
}

// forwards lists the ports, sorted, with the bind error for any that
// couldn't be forwarded (typically another VM already holds the port).
func (f *portForwarder) forwards() []PortForward {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []PortForward
	for key := range f.ports {
		out = append(out, PortForward{Port: key.Port, Protocol: key.Proto})
	}
	for key, msg := range f.failed {
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

// stop ends every forward.
func (f *portForwarder) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stop := range f.ports {
		stop()
	}
	f.ports = map[portKey]func(){}
}

// ForwardedPorts asks a docker or cluster VM's daemon for the ports it
// forwards to localhost, formatted for a list column: "8080,53/udp,9000!"
// ("!" = couldn't bind), "-" for none, "?" if the daemon didn't answer.
func ForwardedPorts(name string) string {
	resp, err := SendControlCommand(name, ControlRequest{Cmd: CmdStatus})
	if err != nil || !resp.OK {
		return "?"
	}
	return FormatPorts(resp.Ports)
}

// FormatPorts formats forwards as ForwardedPorts does.
func FormatPorts(ports []PortForward) string {
	if len(ports) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		s := strconv.Itoa(p.Port)
		if p.Protocol == "udp" {
			s += "/udp"
		}
		if p.Error != "" {
			s += "!"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

func dialGuest(ctx context.Context, user *mnet.UserNetwork, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	return user.DialGuest(ctx, port)
}

// serveGuest relays each connection accepted on l to port on the guest.
// Connections fail while nothing listens there yet.
func serveGuest(ctx context.Context, user *mnet.UserNetwork, l net.Listener, port int) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		go func() {
			guest, err := dialGuest(ctx, user, port)
			if err != nil {
				conn.Close()
				return
			}
			relay(conn, guest)
		}()
	}
}

package vm

import (
	"context"
	"fmt"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

// containerPorts forwards a container's `run -p` ports to localhost. They
// are fixed when the container is created, so the port list never changes;
// the forwarder still retries any it couldn't bind (another VM holds the
// port) every poll, and reports them in the status reply, as docker hosts
// and clusters do.
type containerPorts struct {
	fwd    *portForwarder
	cancel context.CancelFunc
}

// startContainerPorts starts forwarding ports, or returns nil if there are
// none.
func startContainerPorts(name string, ports []PublishedPort, user *mnet.UserNetwork, logger *monolith.Logger) (*containerPorts, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	if user == nil {
		return nil, fmt.Errorf("publishing container ports requires user-mode networking")
	}
	want, guest := containerPortKeys(ports)
	fwd := newPortForwarder(user, logger, func() (map[portKey]bool, error) {
		out := make(map[portKey]bool, len(want))
		for k := range want {
			out[k] = true
		}
		return out, nil
	})
	fwd.guestPorts = guest
	ctx, cancel := context.WithCancel(context.Background())
	go fwd.run(ctx)
	logger.Info("container %q publishes %d port(s) on localhost", name, len(ports))
	return &containerPorts{fwd: fwd, cancel: cancel}, nil
}

// containerPortKeys turns `run -p` ports into the forwarder's keys (host
// side) and the guest port each one goes to when it differs.
func containerPortKeys(ports []PublishedPort) (map[portKey]bool, map[portKey]int) {
	want := map[portKey]bool{}
	guest := map[portKey]int{}
	for _, p := range ports {
		k := portKey{Proto: p.Protocol, Port: p.Host}
		want[k] = true
		if p.Container != p.Host {
			guest[k] = p.Container
		}
	}
	return want, guest
}

func (c *containerPorts) forwards() []PortForward {
	return c.fwd.forwards()
}

func (c *containerPorts) close() {
	c.cancel()
	c.fwd.stop()
}

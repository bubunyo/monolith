package container

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/bubunyo/monolith/vm"
)

// parsePublish parses `run -p` values as docker run does, for the forms
// monolith supports:
//
//	CONTAINER[/PROTO]                  host port picked when the container is created
//	HOST:CONTAINER[/PROTO]
//	127.0.0.1:HOST:CONTAINER[/PROTO]   localhost is where forwards listen anyway
//
// PROTO is tcp (default) or udp. A host port of 0 means "pick one": the
// caller fills it in. Forwards only ever listen on the Mac's localhost, so
// any host address other than 127.0.0.1 (0.0.0.0 included) is refused
// rather than silently narrowed.
func parsePublish(values []string) ([]vm.PublishedPort, error) {
	var out []vm.PublishedPort
	seen := map[string]string{} // "<host>/<proto>" -> the -p value that took it
	for _, v := range values {
		p, err := parseOnePublish(v)
		if err != nil {
			return nil, fmt.Errorf("-p %s: %w", v, err)
		}
		if p.Host != 0 {
			key := fmt.Sprintf("%d/%s", p.Host, p.Protocol)
			if prev, dup := seen[key]; dup {
				return nil, fmt.Errorf("-p %s: host port %d/%s is already published by -p %s", v, p.Host, p.Protocol, prev)
			}
			seen[key] = v
		}
		out = append(out, p)
	}
	return out, nil
}

func parseOnePublish(v string) (vm.PublishedPort, error) {
	p := vm.PublishedPort{Protocol: "tcp"}
	spec := v
	if i := strings.LastIndex(spec, "/"); i >= 0 {
		p.Protocol = strings.ToLower(spec[i+1:])
		spec = spec[:i]
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			return p, fmt.Errorf("protocol must be tcp or udp")
		}
	}
	if strings.Contains(spec, "-") {
		return p, fmt.Errorf("port ranges aren't supported; publish each port with its own -p")
	}

	var host, ctr string
	switch parts := splitHostPorts(spec); len(parts) {
	case 1:
		ctr = parts[0]
	case 2:
		host, ctr = parts[0], parts[1]
	case 3:
		ip := strings.Trim(parts[0], "[]")
		if ip != "" && net.ParseIP(ip) == nil {
			return p, fmt.Errorf("%q isn't an IP address", parts[0])
		}
		if ip != "" && ip != "127.0.0.1" {
			return p, fmt.Errorf("forwards listen only on the Mac's localhost; use 127.0.0.1:%s:%s or %s:%s", parts[1], parts[2], parts[1], parts[2])
		}
		host, ctr = parts[1], parts[2]
	default:
		return p, fmt.Errorf("want [127.0.0.1:][HOST:]CONTAINER[/tcp|/udp]")
	}

	var err error
	if p.Container, err = parsePort(ctr, false); err != nil {
		return p, fmt.Errorf("container port: %w", err)
	}
	if host != "" {
		if p.Host, err = parsePort(host, true); err != nil {
			return p, fmt.Errorf("host port: %w", err)
		}
	}
	return p, nil
}

// splitHostPorts splits "a:b:c" on colons, keeping a bracketed IPv6 address
// ("[::1]:80:80") in one piece so it can be refused with a clear message.
func splitHostPorts(s string) []string {
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end >= 0 {
			rest := strings.TrimPrefix(s[end+1:], ":")
			return append([]string{s[:end+1]}, strings.Split(rest, ":")...)
		}
	}
	return strings.Split(s, ":")
}

// parsePort parses a port number; zero is allowed only where it means
// "pick one" (a host port).
func parsePort(s string, zeroOK bool) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 65535 || (n == 0 && !zeroOK) {
		return 0, fmt.Errorf("%q isn't a port number (1-65535)", s)
	}
	return n, nil
}

// assignHostPorts gives every port published without a host port (-p 80) a
// free one on the Mac's localhost. It is chosen once and saved with the
// container, so it stays the same across restarts.
func assignHostPorts(ports []vm.PublishedPort) error {
	for i := range ports {
		if ports[i].Host != 0 {
			continue
		}
		port, err := vm.FreeLocalPort()
		if err != nil {
			return fmt.Errorf("choosing a host port for -p %d: %w", ports[i].Container, err)
		}
		ports[i].Host = port
	}
	return nil
}

// checkHostPorts refuses to start a container whose TCP host ports are
// taken, as docker run does ("port is already allocated"). UDP ports aren't
// checked here: the daemon reports them if they can't be bound.
func checkHostPorts(name string, ports []vm.PublishedPort) error {
	for _, p := range ports {
		if p.Protocol != "tcp" {
			continue
		}
		if err := vm.CheckLocalPort(p.Host); err != nil {
			return fmt.Errorf("can't publish %s for %s: localhost:%d is already in use (%v)", p, name, p.Host, err)
		}
	}
	return nil
}

// liveForwards asks a running container's daemon which of its ports it
// forwards (and which it couldn't bind); nil if it isn't running, publishes
// nothing, or doesn't answer.
func liveForwards(c info) []vm.PortForward {
	if !c.running() || len(c.cfg.Container.Ports) == 0 {
		return nil
	}
	resp, err := vm.SendControlCommand(c.name, vm.ControlRequest{Cmd: vm.CmdStatus})
	if err != nil || !resp.OK {
		return nil
	}
	return resp.Ports
}

// portsText is the PORTS column of `container ps`: "127.0.0.1:8080->80/tcp,
// …", with "!" after a port the daemon couldn't forward (forwards, from its
// status reply, is nil for a container that isn't running).
func portsText(ports []vm.PublishedPort, forwards []vm.PortForward) string {
	if len(ports) == 0 {
		return ""
	}
	failed := map[string]bool{}
	for _, f := range forwards {
		if f.Error != "" {
			failed[fmt.Sprintf("%d/%s", f.Port, f.Protocol)] = true
		}
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		s := p.String()
		if failed[fmt.Sprintf("%d/%s", p.Host, p.Protocol)] {
			s += "!"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

package sandbox

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/bubunyo/monolith/vm"
)

// The sandbox's network policy is enforced in the VM with iptables, on
// traffic from container cgroups leaving eth0 (the user-mode network). The
// VM's own traffic (dockerd pulling images, DHCP, the relays monolith uses)
// isn't affected, and the shell container has no CAP_NET_ADMIN to change
// the rules. The user-mode network itself doesn't route to the Mac's
// loopback for sandboxes (no 192.168.127.254, no host.docker.internal).

const firewallChain = "MONOLITH-SBX"

// privateNets are unreachable under the outbound policy: the user-mode
// network, the Mac and the local network (private, CGNAT/Tailscale and
// link-local ranges), and multicast.
var privateNets = []string{
	"192.168.127.0/24", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"100.64.0.0/10", "169.254.0.0/16", "224.0.0.0/4",
}

// applyFirewall installs the policy; it replaces whatever was there.
func (s *sandbox) applyFirewall(cfg *vm.SandboxConfig) error {
	script, err := firewallScript(cfg)
	if err != nil {
		return err
	}
	return s.helper(script)
}

func firewallScript(cfg *vm.SandboxConfig) (string, error) {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	w("set -e")
	// The nf_tables iptables backend doesn't autoload match modules.
	w("modprobe -a xt_cgroup xt_conntrack")
	w("iptables -N %s 2>/dev/null || true", firewallChain)
	w("iptables -F %s", firewallChain)
	// Replies to connections made to the sandbox (forwarded dev-server
	// ports) and DNS through the gateway are always fine.
	w("iptables -A %s -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN", firewallChain)
	for _, proto := range []string{"udp", "tcp"} {
		w("iptables -A %s -d %s -p %s --dport 53 -j RETURN", firewallChain, "192.168.127.1", proto)
	}

	switch cfg.NetworkOf() {
	case vm.SandboxNetworkAllow:
		for _, entry := range cfg.Allow {
			ips, port, err := resolveAllow(entry)
			if err != nil {
				return "", err
			}
			for _, ip := range ips {
				w("iptables -A %s -d %s -p tcp --dport %d -j RETURN", firewallChain, ip, port)
			}
		}
		w("iptables -A %s -j REJECT", firewallChain)
	default: // outbound; none also gets it, as a second wall
		for _, n := range privateNets {
			w("iptables -A %s -d %s -j REJECT", firewallChain, n)
		}
	}

	// Only traffic from containers leaving eth0: locally generated traffic
	// from cgroups under /docker (the shell uses the VM's network), and all
	// forwarded traffic (bridged containers). xt_cgroup only works on local
	// traffic, so FORWARD has no cgroup match.
	for _, rule := range []string{
		"OUTPUT -o eth0 -m cgroup --path docker -j " + firewallChain,
		"FORWARD -o eth0 -j " + firewallChain,
	} {
		chain, rest, _ := strings.Cut(rule, " ")
		w("iptables -C %s 2>/dev/null || iptables -I %s 1 %s", rule, chain, rest)
	}
	// The user-mode network is IPv4; make sure containers have no IPv6 way out.
	w("ip6tables -C OUTPUT -o eth0 -m cgroup --path docker -j REJECT 2>/dev/null || ip6tables -I OUTPUT 1 -o eth0 -m cgroup --path docker -j REJECT")
	return b.String(), nil
}

// resolveAllow turns "host:port" into IPv4 addresses (resolved now, on the
// Mac) and the port. Hosts behind changing CDN addresses need re-activating.
func resolveAllow(entry string) ([]string, int, error) {
	host, portStr, err := net.SplitHostPort(entry)
	if err != nil {
		return nil, 0, fmt.Errorf("--allow %q: want host:port", entry)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, 0, fmt.Errorf("--allow %q: bad port", entry)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() == nil {
			return nil, 0, fmt.Errorf("--allow %q: only IPv4 is supported", entry)
		}
		return []string{ip.String()}, port, nil
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return nil, 0, fmt.Errorf("--allow %q: %w", entry, err)
	}
	var ips []string
	for _, a := range addrs {
		if v4 := a.To4(); v4 != nil {
			ips = append(ips, v4.String())
		}
	}
	if len(ips) == 0 {
		return nil, 0, fmt.Errorf("--allow %q: no IPv4 address", entry)
	}
	return ips, port, nil
}

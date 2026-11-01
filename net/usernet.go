package net

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"

	"github.com/Code-Hex/vz/v4"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// User-mode network addressing. Every VM in user mode gets its own private
// copy of this network, so the addresses are the same for all of them.
const (
	UserSubnet    = "192.168.127.0/24"
	UserGatewayIP = "192.168.127.1"   // DHCP and DNS server
	UserGuestIP   = "192.168.127.2"   // static lease for the VM's MAC
	UserHostIP    = "192.168.127.254" // NATed to the Mac's 127.0.0.1

	// UserHostName and UserGatewayName resolve to UserHostIP through the
	// gateway's DNS server, in the guest and in its containers and pods, as
	// with Docker Desktop.
	UserHostName    = "host.docker.internal"
	UserGatewayName = "gateway.docker.internal"

	userGatewayMAC = "5a:94:ef:e4:0c:dd"
	userMTU        = 1500
)

// UserNetwork is a user-mode virtual network for one VM, implemented by
// gvisor-tap-vsock inside the VM daemon. The guest's NIC is attached to one
// end of a datagram socket pair carrying raw Ethernet frames; a userspace
// TCP/IP stack on the other end terminates the guest's connections and
// re-opens them from the daemon process. Outbound traffic therefore looks
// like any other app's traffic to macOS, VPNs and endpoint filters, which
// is what lets it work where vmnet's NAT forwarding is blocked. The guest
// is not reachable from the Mac by IP; DialGuest connects to it instead.
type UserNetwork struct {
	vn     *virtualnetwork.VirtualNetwork
	guest  *os.File // VM side of the socket pair, handed to vz
	host   net.Conn // daemon side, served by the virtual network
	cancel context.CancelFunc
}

// NewUserNetwork creates the virtual network and starts serving it. mac is
// the VM's MAC address, which gets the static DHCP lease UserGuestIP.
// hostAccess maps UserHostIP (and host.docker.internal) to the Mac's
// 127.0.0.1; sandboxes leave it off so guests can't reach services that
// listen only on the Mac's loopback.
func NewUserNetwork(mac string, hostAccess bool) (*UserNetwork, error) {
	// gvisor-tap-vsock logs every connection at info level.
	logrus.SetLevel(logrus.WarnLevel)

	cfg := &types.Configuration{
		MTU:               userMTU,
		Subnet:            UserSubnet,
		GatewayIP:         UserGatewayIP,
		GatewayMacAddress: userGatewayMAC,
		DHCPStaticLeases:  map[string]string{UserGuestIP: mac},
		Protocol:          types.VfkitProtocol,
	}
	if hostAccess {
		cfg.NAT = map[string]string{UserHostIP: "127.0.0.1"}
		cfg.GatewayVirtualIPs = []string{UserHostIP}
		cfg.DNS = []types.Zone{{
			Name: "docker.internal.",
			Records: []types.Record{
				{Name: strings.TrimSuffix(UserHostName, ".docker.internal"), IP: net.ParseIP(UserHostIP)},
				{Name: strings.TrimSuffix(UserGatewayName, ".docker.internal"), IP: net.ParseIP(UserHostIP)},
			},
		}}
	}
	vn, err := virtualnetwork.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating user-mode network: %w", err)
	}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	// Frames are datagrams; small default buffers drop them under load.
	// These are the sizes vfkit uses.
	for _, fd := range fds {
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
		unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 4<<20)
	}
	guest := os.NewFile(uintptr(fds[0]), "usernet-guest")
	hostFile := os.NewFile(uintptr(fds[1]), "usernet-host")
	host, err := net.FileConn(hostFile)
	hostFile.Close() // FileConn dups the descriptor
	if err != nil {
		guest.Close()
		return nil, fmt.Errorf("socketpair conn: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go vn.AcceptVfkit(ctx, host)

	return &UserNetwork{vn: vn, guest: guest, host: host, cancel: cancel}, nil
}

// attachment returns the vz attachment connecting the guest NIC to the
// virtual network.
func (u *UserNetwork) attachment() (vz.NetworkDeviceAttachment, error) {
	a, err := vz.NewFileHandleNetworkDeviceAttachment(u.guest)
	if err != nil {
		return nil, err
	}
	if err := a.SetMaximumTransmissionUnit(userMTU); err != nil {
		return nil, err
	}
	return a, nil
}

// DialGuest opens a TCP connection to port on the guest through the virtual
// network, as if from the gateway.
func (u *UserNetwork) DialGuest(ctx context.Context, port int) (net.Conn, error) {
	return u.vn.DialContextTCP(ctx, net.JoinHostPort(UserGuestIP, strconv.Itoa(port)))
}

// ExposeUDP forwards UDP on 127.0.0.1:hostPort on the Mac to guestPort on
// the guest, using gvisor-tap-vsock's port forwarder (it tracks a session
// per client address). TCP goes through DialGuest instead.
func (u *UserNetwork) ExposeUDP(hostPort, guestPort int) error {
	return u.forwarder("/services/forwarder/expose", types.ExposeRequest{
		Local:    net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)),
		Remote:   net.JoinHostPort(UserGuestIP, strconv.Itoa(guestPort)),
		Protocol: types.UDP,
	})
}

// UnexposeUDP stops a forward started by ExposeUDP.
func (u *UserNetwork) UnexposeUDP(hostPort int) error {
	return u.forwarder("/services/forwarder/unexpose", types.UnexposeRequest{
		Local:    net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)),
		Protocol: types.UDP,
	})
}

// forwarder calls the forwarder's HTTP API in-process.
func (u *UserNetwork) forwarder(path string, req any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	rec := httptest.NewRecorder()
	u.vn.ServicesMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		return fmt.Errorf("%s", strings.TrimSpace(rec.Body.String()))
	}
	return nil
}

// Close stops serving the network.
func (u *UserNetwork) Close() {
	u.cancel()
	u.host.Close()
	u.guest.Close()
	u.vn.Close()
}

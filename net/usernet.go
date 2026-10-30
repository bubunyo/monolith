package net

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

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
func NewUserNetwork(mac string) (*UserNetwork, error) {
	// gvisor-tap-vsock logs every connection at info level.
	logrus.SetLevel(logrus.WarnLevel)

	vn, err := virtualnetwork.New(&types.Configuration{
		MTU:               userMTU,
		Subnet:            UserSubnet,
		GatewayIP:         UserGatewayIP,
		GatewayMacAddress: userGatewayMAC,
		DHCPStaticLeases:  map[string]string{UserGuestIP: mac},
		NAT:               map[string]string{UserHostIP: "127.0.0.1"},
		GatewayVirtualIPs: []string{UserHostIP},
		Protocol:          types.VfkitProtocol,
	})
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

// Close stops serving the network.
func (u *UserNetwork) Close() {
	u.cancel()
	u.host.Close()
	u.guest.Close()
	u.vn.Close()
}

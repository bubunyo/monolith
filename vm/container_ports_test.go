package vm

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContainerPortKeys(t *testing.T) {
	want, guest := containerPortKeys([]PublishedPort{
		{Host: 8080, Container: 80, Protocol: "tcp"},
		{Host: 9000, Container: 9000, Protocol: "tcp"},
		{Host: 5353, Container: 53, Protocol: "udp"},
	})
	require.Equal(t, map[portKey]bool{
		{Proto: "tcp", Port: 8080}: true,
		{Proto: "tcp", Port: 9000}: true,
		{Proto: "udp", Port: 5353}: true,
	}, want, "keys are the host side, all on the guest's interfaces (no loopback relay)")
	require.Equal(t, map[portKey]int{
		{Proto: "tcp", Port: 8080}: 80,
		{Proto: "udp", Port: 5353}: 53,
	}, guest, "only ports that differ are mapped")
}

func TestPortForwarderGuestPort(t *testing.T) {
	f := &portForwarder{guestPorts: map[portKey]int{{Proto: "tcp", Port: 8080}: 80}}
	require.Equal(t, 80, f.guestPort(portKey{Proto: "tcp", Port: 8080}))
	require.Equal(t, 8080, f.guestPort(portKey{Proto: "udp", Port: 8080}), "the protocol is part of the key")
	require.Equal(t, 3000, f.guestPort(portKey{Proto: "tcp", Port: 3000}), "unmapped ports go to the same port (docker hosts, clusters)")
	require.Equal(t, 443, (&portForwarder{}).guestPort(portKey{Proto: "tcp", Port: 443}))
}

func TestStartContainerPortsWithoutPorts(t *testing.T) {
	c, err := startContainerPorts("c", nil, nil, nil)
	require.NoError(t, err)
	require.Nil(t, c, "nothing to forward: no forwarder, and no user network needed")
}

func TestPublishedPortString(t *testing.T) {
	require.Equal(t, "127.0.0.1:8080->80/tcp", PublishedPort{Host: 8080, Container: 80, Protocol: "tcp"}.String())
}

package container

import (
	"testing"

	"github.com/bubunyo/monolith/vm"
	"github.com/stretchr/testify/require"
)

func TestParsePublish(t *testing.T) {
	tests := []struct {
		in   string
		want vm.PublishedPort
	}{
		{"8080:80", vm.PublishedPort{Host: 8080, Container: 80, Protocol: "tcp"}},
		{"8080:80/tcp", vm.PublishedPort{Host: 8080, Container: 80, Protocol: "tcp"}},
		{"5353:53/udp", vm.PublishedPort{Host: 5353, Container: 53, Protocol: "udp"}},
		{"5353:53/UDP", vm.PublishedPort{Host: 5353, Container: 53, Protocol: "udp"}},
		{"127.0.0.1:8080:80", vm.PublishedPort{Host: 8080, Container: 80, Protocol: "tcp"}},
		{":8080:80", vm.PublishedPort{Host: 8080, Container: 80, Protocol: "tcp"}},
		{"80", vm.PublishedPort{Host: 0, Container: 80, Protocol: "tcp"}},
		{"0:80", vm.PublishedPort{Host: 0, Container: 80, Protocol: "tcp"}},
		{"127.0.0.1::80", vm.PublishedPort{Host: 0, Container: 80, Protocol: "tcp"}},
		{"80:8080", vm.PublishedPort{Host: 80, Container: 8080, Protocol: "tcp"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parsePublish([]string{tt.in})
			require.NoError(t, err)
			require.Equal(t, []vm.PublishedPort{tt.want}, got)
		})
	}
}

func TestParsePublishRejects(t *testing.T) {
	for in, msg := range map[string]string{
		"0.0.0.0:8080:80":   "only on the Mac's localhost",
		"192.168.1.5:80:80": "only on the Mac's localhost",
		"[::1]:8080:80":     "only on the Mac's localhost",
		"nope:8080:80":      "isn't an IP address",
		"8080:80/sctp":      "tcp or udp",
		"8000-8010:80":      "ranges",
		"8080:0":            "container port",
		"8080:70000":        "container port",
		"x:80":              "host port",
		"1:2:3:4":           "want",
		"":                  "container port",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := parsePublish([]string{in})
			require.ErrorContains(t, err, msg)
		})
	}
}

func TestParsePublishDuplicateHostPort(t *testing.T) {
	_, err := parsePublish([]string{"8080:80", "8080:81"})
	require.ErrorContains(t, err, "already published by -p 8080:80")

	got, err := parsePublish([]string{"8080:80", "8080:80/udp", "80", "80"})
	require.NoError(t, err, "same number over tcp and udp, and two picked ports, don't clash")
	require.Len(t, got, 4)
}

func TestAssignHostPorts(t *testing.T) {
	ports := []vm.PublishedPort{{Host: 0, Container: 80, Protocol: "tcp"}, {Host: 9000, Container: 90, Protocol: "tcp"}}
	require.NoError(t, assignHostPorts(ports))
	require.NotZero(t, ports[0].Host)
	require.Equal(t, 9000, ports[1].Host, "explicit host ports stay")
}

func TestPortsText(t *testing.T) {
	ports := []vm.PublishedPort{
		{Host: 8080, Container: 80, Protocol: "tcp"},
		{Host: 5353, Container: 53, Protocol: "udp"},
	}
	require.Equal(t, "", portsText(nil, nil))
	require.Equal(t, "127.0.0.1:8080->80/tcp, 127.0.0.1:5353->53/udp", portsText(ports, nil))
	require.Equal(t, "127.0.0.1:8080->80/tcp!, 127.0.0.1:5353->53/udp", portsText(ports, []vm.PortForward{
		{Port: 8080, Protocol: "tcp", Error: "address already in use"},
		{Port: 5353, Protocol: "udp"},
	}))
}

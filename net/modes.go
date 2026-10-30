package net

// Network mode constants.
const (
	NetworkNAT      = "nat"
	NetworkShared   = "shared"
	NetworkHost     = "host"
	NetworkIsolated = "isolated"
	NetworkNone     = "none"
	NetworkUser     = "user" // user-mode networking in the VM daemon (gvisor-tap-vsock)
)

// ValidNetworkModes is the set of accepted --network-mode values.
var ValidNetworkModes = []string{NetworkNAT, NetworkShared, NetworkHost, NetworkIsolated, NetworkNone, NetworkUser}

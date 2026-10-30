package net

// Network mode constants.
const (
	NetworkNAT      = "nat"
	NetworkShared   = "shared"
	NetworkHost     = "host"
	NetworkIsolated = "isolated"
	NetworkNone     = "none"
)

// ValidNetworkModes is the set of accepted --network-mode values.
var ValidNetworkModes = []string{NetworkNAT, NetworkShared, NetworkHost, NetworkIsolated, NetworkNone}

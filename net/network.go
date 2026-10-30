package net

import (
	"fmt"

	"github.com/Code-Hex/vz/v4"
	"github.com/Code-Hex/vz/v4/vmnet"
)

// ConfigureNetwork sets up the VM's network device based on the config.
// It returns the MAC address string (even for "none" mode, for state tracking)
// and attaches the network device to the VM configuration when applicable.
func ConfigureNetwork(vmConfig *vz.VirtualMachineConfiguration, mode, networkName, hostInterface string) (string, error) {
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return "", fmt.Errorf("generating MAC address: %w", err)
	}

	if mode == "" {
		mode = NetworkNAT
	}

	if mode == NetworkNone {
		return mac.String(), nil
	}

	var attachment vz.NetworkDeviceAttachment

	// If a named network is specified, join it via the daemon.
	if networkName != "" {
		network, err := joinNamedNetwork(networkName)
		if err != nil {
			return "", fmt.Errorf("joining named network %q: %w", networkName, err)
		}
		attachment, err = vz.NewVMNetNetworkDeviceAttachment(network)
		if err != nil {
			return "", fmt.Errorf("attaching to named network %q: %w", networkName, err)
		}
	} else {
		attachment, err = createNetworkAttachment(mode, hostInterface)
		if err != nil {
			return "", fmt.Errorf("creating network attachment (%s): %w", mode, err)
		}
	}

	netDevConfig, err := vz.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return "", fmt.Errorf("creating virtio network device: %w", err)
	}
	netDevConfig.SetMACAddress(mac)
	vmConfig.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netDevConfig})

	return mac.String(), nil
}

// createNetworkAttachment builds the appropriate vz.NetworkDeviceAttachment for the given mode.
func createNetworkAttachment(mode, hostInterface string) (vz.NetworkDeviceAttachment, error) {
	switch mode {
	case NetworkNAT:
		return vz.NewNATNetworkDeviceAttachment()

	case NetworkShared:
		return newVMNetAttachment(vmnet.ModeShared)

	case NetworkHost:
		if hostInterface == "" {
			return nil, fmt.Errorf("host mode requires --host-interface (e.g. en0)")
		}
		ifaces := vz.NetworkInterfaces()
		for _, iface := range ifaces {
			if iface.Identifier() == hostInterface {
				return vz.NewBridgedNetworkDeviceAttachment(iface)
			}
		}
		// Build list of available interfaces for the error message.
		var available []string
		for _, iface := range ifaces {
			available = append(available, iface.Identifier())
		}
		return nil, fmt.Errorf("host interface %q not found; available: %v", hostInterface, available)

	case NetworkIsolated:
		return newVMNetAttachment(vmnet.ModeHost)

	default:
		return nil, fmt.Errorf("unknown network mode: %q", mode)
	}
}

// newVMNetAttachment creates a private vmnet network in the given mode for a
// single VM (no named network) and returns an attachment to it.
func newVMNetAttachment(mode vmnet.Mode) (vz.NetworkDeviceAttachment, error) {
	config, err := vmnet.NewNetworkConfiguration(mode)
	if err != nil {
		return nil, fmt.Errorf("vmnet configuration: %w", err)
	}
	network, err := vmnet.NewNetwork(config)
	if err != nil {
		return nil, fmt.Errorf("vmnet network: %w", err)
	}
	return vz.NewVMNetNetworkDeviceAttachment(network)
}

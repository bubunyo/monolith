package monolith

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/Code-Hex/vz/v3/vmnet"
)

// configureNetwork sets up the VM's network device based on the config.
// It returns the MAC address string (even for "none" mode, for state tracking)
// and attaches the network device to the VM configuration when applicable.
func configureNetwork(vmConfig *vz.VirtualMachineConfiguration, cfg VMConfig) (string, error) {
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return "", fmt.Errorf("generating MAC address: %w", err)
	}

	mode := cfg.Network
	if mode == "" {
		mode = NetworkNAT
	}

	if mode == NetworkNone {
		return mac.String(), nil
	}

	attachment, err := createNetworkAttachment(mode, cfg.HostInterface)
	if err != nil {
		return "", fmt.Errorf("creating network attachment (%s): %w", mode, err)
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
		config, err := vmnet.NewNetworkConfiguration(vmnet.SharedMode)
		if err != nil {
			return nil, fmt.Errorf("vmnet SharedMode config: %w", err)
		}
		network, err := vmnet.NewNetwork(config)
		if err != nil {
			return nil, fmt.Errorf("vmnet SharedMode network: %w", err)
		}
		return vz.NewVmnetNetworkDeviceAttachment(network)

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
		config, err := vmnet.NewNetworkConfiguration(vmnet.HostMode)
		if err != nil {
			return nil, fmt.Errorf("vmnet HostMode config: %w", err)
		}
		network, err := vmnet.NewNetwork(config)
		if err != nil {
			return nil, fmt.Errorf("vmnet HostMode network: %w", err)
		}
		return vz.NewVmnetNetworkDeviceAttachment(network)

	default:
		return nil, fmt.Errorf("unknown network mode: %q", mode)
	}
}

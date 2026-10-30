package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	mnet "github.com/bubunyo/monolith/net"
)

const configFileName = "config.json"

// VMConfig holds the per-VM hardware configuration.
type VMConfig struct {
	CPUs          uint   `json:"cpus"`
	Memory        uint64 `json:"memory"`                   // bytes
	NetworkMode   string `json:"network_mode"`             // "nat", "shared", "host", "isolated", "none"
	NetworkName   string `json:"network_name,omitempty"`   // named network to join (for shared/isolated modes)
	HostInterface string `json:"host_interface,omitempty"` // physical NIC for "host" mode (e.g. "en0")
	Image         string `json:"image,omitempty"`          // base image: "kata", "alpine" or "docker"
	Engine        string `json:"engine,omitempty"`         // docker image only: "docker" (default) or "containerd"
	Kubernetes    bool   `json:"kubernetes,omitempty"`     // docker image only: run k3s on the engine
	K3sVersion    string `json:"k3s_version,omitempty"`    // k3s release, e.g. "v1.36.5+k3s1" (default DefaultK3sVersion)
	KubePort      int    `json:"kube_port,omitempty"`      // host port on 127.0.0.1 forwarded to the k3s API
	GuestLayout   int    `json:"guest_layout,omitempty"`   // docker image only: GuestLayout its disk was created with
	MAC           string `json:"mac,omitempty"`            // assigned on first boot, then reused
}

// DefaultConfig returns the default VM configuration: 1 vCPU, 512 MiB RAM,
// NAT networking, Kata base image.
func DefaultConfig() VMConfig {
	return VMConfig{
		CPUs:        1,
		Memory:      512 * 1024 * 1024,
		NetworkMode: mnet.NetworkNAT,
		Image:       DefaultImage,
	}
}

// ReadConfig reads the VM configuration from its config file.
// If the file does not exist, default values are returned, except that a VM
// directory that already holds an Alpine disk.img stays on Alpine.
func ReadConfig(name string) (VMConfig, error) {
	dir, err := VMDir(name)
	if err != nil {
		return DefaultConfig(), nil
	}

	data, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			if _, err := os.Stat(filepath.Join(dir, alpineDiskName)); err == nil {
				cfg.Image = ImageAlpine
			}
			return cfg, nil
		}
		return VMConfig{}, fmt.Errorf("reading config: %w", err)
	}

	var cfg VMConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig(), nil
	}

	// Guard against zero values from corrupt or hand-edited files.
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}
	if cfg.Memory == 0 {
		cfg.Memory = 512 * 1024 * 1024
	}
	if cfg.NetworkMode == "" {
		cfg.NetworkMode = mnet.NetworkNAT
	}
	// Configs written before base images existed belong to Alpine VMs with
	// a persistent disk; keep booting them as Alpine.
	if cfg.Image == "" {
		cfg.Image = ImageAlpine
	}

	return cfg, nil
}

// WriteConfig persists the VM configuration to its config file.
func WriteConfig(name string, cfg VMConfig) error {
	dir, err := VMDir(name)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating VM directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	return os.WriteFile(filepath.Join(dir, configFileName), data, 0644)
}

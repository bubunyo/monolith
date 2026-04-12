package monolith

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const configFileName = "config.json"

// VMConfig holds the per-VM hardware configuration.
type VMConfig struct {
	CPUs   uint   `json:"cpus"`
	Memory uint64 `json:"memory"` // bytes
}

// DefaultConfig returns the default VM configuration: 1 vCPU, 512 MiB RAM.
func DefaultConfig() VMConfig {
	return VMConfig{
		CPUs:   1,
		Memory: 512 * 1024 * 1024,
	}
}

// ReadConfig reads the VM configuration from its config file.
// If the file does not exist, default values are returned.
func ReadConfig(name string) (VMConfig, error) {
	dir, err := vmDir(name)
	if err != nil {
		return DefaultConfig(), nil
	}

	data, err := os.ReadFile(filepath.Join(dir, configFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
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

	return cfg, nil
}

// WriteConfig persists the VM configuration to its config file.
func WriteConfig(name string, cfg VMConfig) error {
	dir, err := vmDir(name)
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

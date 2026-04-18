package network

import (
	"fmt"
	"os"
	"regexp"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

var (
	flagMode          string
	flagCreateVerbose bool
)

// networkCfg holds the parameters for creating a named network.
type networkCfg struct {
	Name    string
	Mode    string
	Verbose bool
}

var createCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a named network",
	Long:  "Create a named network with a background daemon. VMs can join this network with --network <name>.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return createNetwork(networkCfg{
			Name:    args[0],
			Mode:    flagMode,
			Verbose: flagCreateVerbose,
		})
	},
}

// createNetwork validates, persists, and starts a new named network.
func createNetwork(cfg networkCfg) error {
	if len(cfg.Name) > 63 {
		return fmt.Errorf("network name %q is too long (max 63 characters)", cfg.Name)
	}
	if !namePattern.MatchString(cfg.Name) {
		return fmt.Errorf("network name %q is invalid (must be lowercase alphanumeric with hyphens)", cfg.Name)
	}

	if cfg.Mode != monolith.NetworkShared && cfg.Mode != monolith.NetworkIsolated {
		return fmt.Errorf("named networks only support shared or isolated modes, got %q", cfg.Mode)
	}

	if monolith.IsNetworkDaemonAlive(cfg.Name) {
		return fmt.Errorf("network %q already exists and is running", cfg.Name)
	}

	dir, err := monolith.NetworkDir(cfg.Name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("network %q already exists (use 'network start' to restart it)", cfg.Name)
	}

	netCfg := monolith.NetworkConfig{
		Mode: cfg.Mode,
	}
	if err := monolith.WriteNetworkConfig(cfg.Name, netCfg); err != nil {
		return fmt.Errorf("writing network config: %w", err)
	}

	if err := spawnDaemon(cfg.Name, cfg.Mode, cfg.Verbose); err != nil {
		return err
	}

	netCfg, err = monolith.ReadNetworkConfig(cfg.Name)
	if err != nil {
		fmt.Printf("Network %q created (mode: %s)\n", cfg.Name, cfg.Mode)
	} else {
		fmt.Printf("Network %q created (mode: %s, subnet: %s)\n", cfg.Name, netCfg.Mode, netCfg.Subnet)
	}

	return nil
}

func init() {
	createCmd.Flags().StringVar(&flagMode, "mode", monolith.NetworkShared, `network mode: shared or isolated (default: shared)`)
	createCmd.Flags().BoolVarP(&flagCreateVerbose, "verbose", "v", false, "enable packet-level logging in daemon")
}

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

var createCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a named network",
	Long:  "Create a named network with a background daemon. VMs can join this network with --network <name>.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		// Validate name.
		if len(name) > 63 {
			return fmt.Errorf("network name %q is too long (max 63 characters)", name)
		}
		if !namePattern.MatchString(name) {
			return fmt.Errorf("network name %q is invalid (must be lowercase alphanumeric with hyphens)", name)
		}

		// Validate mode.
		if flagMode != monolith.NetworkShared && flagMode != monolith.NetworkIsolated {
			return fmt.Errorf("named networks only support shared or isolated modes, got %q", flagMode)
		}

		// Check if the network already exists and its daemon is running.
		if monolith.IsNetworkDaemonAlive(name) {
			return fmt.Errorf("network %q already exists and is running", name)
		}

		// Check if the network directory already exists (stopped network).
		dir, err := monolith.NetworkDir(name)
		if err != nil {
			return err
		}
		if _, err := os.Stat(dir); err == nil {
			return fmt.Errorf("network %q already exists (use 'network start' to restart it)", name)
		}

		// Write initial config.
		cfg := monolith.NetworkConfig{
			Mode: flagMode,
		}
		if err := monolith.WriteNetworkConfig(name, cfg); err != nil {
			return fmt.Errorf("writing network config: %w", err)
		}

		// Spawn the daemon.
		if err := spawnDaemon(name, flagMode, flagCreateVerbose); err != nil {
			return err
		}

		// Re-read config to get the subnet.
		cfg, err = monolith.ReadNetworkConfig(name)
		if err != nil {
			fmt.Printf("Network %q created (mode: %s)\n", name, flagMode)
		} else {
			fmt.Printf("Network %q created (mode: %s, subnet: %s)\n", name, cfg.Mode, cfg.Subnet)
		}

		return nil
	},
}

func init() {
	createCmd.Flags().StringVar(&flagMode, "mode", monolith.NetworkShared, `network mode: shared or isolated (default: shared)`)
	createCmd.Flags().BoolVarP(&flagCreateVerbose, "verbose", "v", false, "enable packet-level logging in daemon")
}

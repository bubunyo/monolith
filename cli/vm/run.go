package vm

import (
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

var (
	flagCPUs          uint
	flagMemory        string
	flagNetworkMode   string
	flagNetwork       string
	flagHostInterface string
)

// vmCfg holds optional overrides for VM configuration, set via CLI flags.
// Nil pointers indicate the flag was not explicitly set, so the saved
// config.json value (or default) should be used.
type vmCfg struct {
	CPUs          *uint
	Memory        *uint64
	NetworkMode   *string
	NetworkName   *string
	HostInterface *string
}

var runCmd = &cobra.Command{
	Use:   "run [name]",
	Short: "Boot a VM (creates on first run, reattaches if running)",
	Long: `Start a virtual machine by name. If the VM is already running, attaches to its console.
If the VM is suspended, restores from the suspend file. If no name is given, one is generated automatically.

Detach from the console with Ctrl+C or Ctrl+] q. The VM keeps running in the background.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) > 0 {
			name = args[0]
		}

		var cfg vmCfg
		if cmd.Flags().Changed("cpus") {
			cfg.CPUs = &flagCPUs
		}
		if cmd.Flags().Changed("memory") {
			mem, err := parseMemory(flagMemory)
			if err != nil {
				return err
			}
			cfg.Memory = &mem
		}
		if cmd.Flags().Changed("network-mode") {
			cfg.NetworkMode = &flagNetworkMode
		}
		if cmd.Flags().Changed("network") {
			cfg.NetworkName = &flagNetwork
		}
		if cmd.Flags().Changed("host-interface") {
			cfg.HostInterface = &flagHostInterface
		}

		return runVM(name, cfg)
	},
}

// runVM boots a VM (or reattaches/restores depending on state).
// If name is empty, a unique name is auto-generated.
// The vmCfg overrides are applied on top of the saved config for fresh boots.
func runVM(name string, cfg vmCfg) error {
	if name == "" {
		var err error
		name, err = generateUniqueName()
		if err != nil {
			return err
		}
	}
	if len(name) > 63 {
		return fmt.Errorf("VM name %q is too long (max 63 characters)", name)
	}
	if !namePattern.MatchString(name) {
		return fmt.Errorf("VM name %q is invalid (must be lowercase alphanumeric with hyphens, e.g. my-vm-01)", name)
	}

	// Check current state.
	status, err := monolith.ReadState(name)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}

	switch status.State {
	case monolith.StatusRunning, monolith.StatusPaused:
		// Already running — just attach.
		fmt.Printf("VM %q is already %s. Attaching to console...\n", name, status.State)
		return attachAndHandle(name)

	case monolith.StatusSuspended:
		// Restore from suspend file.
		fmt.Printf("Restoring VM %q from suspend file...\n", name)
		if err := monolith.SpawnVMDaemon(name, true); err != nil {
			return err
		}
		fmt.Printf("VM %q restored. Attaching to console...\n", name)
		return attachAndHandle(name)
	}

	// Stopped — need to boot. Read/apply config.
	savedCfg, err := monolith.ReadConfig(name)
	if err != nil {
		return err
	}

	// Apply overrides from CLI flags.
	if cfg.CPUs != nil {
		savedCfg.CPUs = *cfg.CPUs
	}
	if cfg.Memory != nil {
		savedCfg.Memory = *cfg.Memory
	}
	if cfg.NetworkMode != nil {
		savedCfg.NetworkMode = *cfg.NetworkMode
	}
	if cfg.NetworkName != nil {
		savedCfg.NetworkName = *cfg.NetworkName
	}
	if cfg.HostInterface != nil {
		savedCfg.HostInterface = *cfg.HostInterface
	}

	if err := validateNetwork(savedCfg); err != nil {
		return err
	}

	// Persist config so the daemon can read it.
	if err := monolith.WriteConfig(name, savedCfg); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	fmt.Printf("Booting VM %q (%d vCPU, %s RAM, network: %s)...\n", name, savedCfg.CPUs, formatMemory(savedCfg.Memory), savedCfg.NetworkMode)

	// Spawn the VM daemon.
	if err := monolith.SpawnVMDaemon(name, false); err != nil {
		return err
	}

	// Attach to the console.
	return attachAndHandle(name)
}

// attachAndHandle attaches to the VM console and handles any escape commands.
func attachAndHandle(name string) error {
	escapeCmd, err := monolith.AttachConsole(name)
	if err != nil {
		return err
	}

	if escapeCmd != nil {
		return executeEscapeCommand(name, *escapeCmd)
	}

	fmt.Printf("Detached from %s. VM is still running.\n", name)
	return nil
}

func init() {
	runCmd.Flags().UintVarP(&flagCPUs, "cpus", "c", 0, "number of vCPUs (default: 1)")
	runCmd.Flags().StringVarP(&flagMemory, "memory", "m", "", `RAM size, e.g. "512M", "1G", "2048M" (default: 512M)`)
	runCmd.Flags().StringVar(&flagNetworkMode, "network-mode", "", `network mode: nat, shared, host, isolated, none (default: nat)`)
	runCmd.Flags().StringVarP(&flagNetwork, "network", "n", "", `named network to join (for shared/isolated modes)`)
	runCmd.Flags().StringVar(&flagHostInterface, "host-interface", "", `physical NIC for host mode (e.g. "en0")`)
}

// isValidNetwork checks if the given string is a valid network mode.
func isValidNetwork(mode string) bool {
	for _, m := range monolith.ValidNetworkModes {
		if m == mode {
			return true
		}
	}
	return false
}

// validateNetwork validates the network-related fields of a VMConfig.
// It checks that the network mode is recognized and that flag combinations
// are consistent (e.g. --host-interface requires host mode, --network
// requires shared or isolated mode).
func validateNetwork(cfg monolith.VMConfig) error {
	if !isValidNetwork(cfg.NetworkMode) {
		return fmt.Errorf("invalid network mode %q; must be one of: nat, shared, host, isolated, none", cfg.NetworkMode)
	}
	if cfg.NetworkMode == monolith.NetworkHost && cfg.HostInterface == "" {
		return fmt.Errorf("host network mode requires --host-interface (e.g. --host-interface en0)")
	}
	if cfg.HostInterface != "" && cfg.NetworkMode != monolith.NetworkHost {
		return fmt.Errorf("--host-interface is only valid with --network-mode host")
	}
	if cfg.NetworkName != "" && cfg.NetworkMode != monolith.NetworkShared && cfg.NetworkMode != monolith.NetworkIsolated {
		return fmt.Errorf("--network is only valid with --network-mode shared or --network-mode isolated")
	}
	return nil
}

// parseMemory parses a human-readable memory string like "512M", "1G", "2048M"
// into bytes. Plain numbers are treated as mebibytes.
func parseMemory(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}

	// Check for suffix.
	upper := strings.ToUpper(s)
	var multiplier uint64 = 1
	numStr := s

	switch {
	case strings.HasSuffix(upper, "G"):
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-1]
	case strings.HasSuffix(upper, "GIB"):
		multiplier = 1024 * 1024 * 1024
		numStr = s[:len(s)-3]
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1000 * 1000 * 1000
		numStr = s[:len(s)-2]
	case strings.HasSuffix(upper, "M"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-1]
	case strings.HasSuffix(upper, "MIB"):
		multiplier = 1024 * 1024
		numStr = s[:len(s)-3]
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1000 * 1000
		numStr = s[:len(s)-2]
	default:
		// Plain number — treat as mebibytes.
		multiplier = 1024 * 1024
	}

	n, err := strconv.ParseUint(strings.TrimSpace(numStr), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("memory must be greater than 0")
	}

	return n * multiplier, nil
}

// generateUniqueName generates a unique VM name in the form "lith-NNNNN",
// retrying up to 10 times if a VM with that name already exists.
func generateUniqueName() (string, error) {
	const maxAttempts = 10
	for i := 0; i < maxAttempts; i++ {
		name := fmt.Sprintf("lith-%05d", rand.IntN(100000))
		dir, err := monolith.VMDir(name)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", fmt.Errorf("failed to generate a unique VM name after %d attempts", maxAttempts)
}

// formatMemory converts bytes to a human-readable string.
func formatMemory(bytes uint64) string {
	const (
		mib = 1024 * 1024
		gib = 1024 * mib
	)
	switch {
	case bytes >= gib && bytes%gib == 0:
		return fmt.Sprintf("%d GiB", bytes/gib)
	case bytes >= mib:
		return fmt.Sprintf("%d MiB", bytes/mib)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

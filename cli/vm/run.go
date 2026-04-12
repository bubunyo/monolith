package vm

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

var (
	flagCPUs   uint
	flagMemory string
)

var runCmd = &cobra.Command{
	Use:   "run [name]",
	Short: "Boot a VM (creates on first run)",
	Long:  "Start a virtual machine by name. If the name doesn't exist, a new VM is created. If no name is given, one is generated automatically.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		if name == "" {
			name = fmt.Sprintf("lith-%05d", rand.IntN(100000))
		}
		if len(name) > 63 {
			return fmt.Errorf("VM name %q is too long (max 63 characters)", name)
		}
		if !namePattern.MatchString(name) {
			return fmt.Errorf("VM name %q is invalid (must be lowercase alphanumeric with hyphens, e.g. my-vm-01)", name)
		}

		// Read existing config or start with defaults.
		cfg, err := monolith.ReadConfig(name)
		if err != nil {
			return err
		}

		// Override with flags if explicitly set.
		if cmd.Flags().Changed("cpus") {
			cfg.CPUs = flagCPUs
		}
		if cmd.Flags().Changed("memory") {
			mem, err := parseMemory(flagMemory)
			if err != nil {
				return err
			}
			cfg.Memory = mem
		}

		// Persist config so Run() and vm list can read it.
		if err := monolith.WriteConfig(name, cfg); err != nil {
			return fmt.Errorf("writing config: %w", err)
		}

		fmt.Printf("Booting VM %q (%d vCPU, %s RAM)...\n", name, cfg.CPUs, formatMemory(cfg.Memory))
		return monolith.Run(name)
	},
}

func init() {
	runCmd.Flags().UintVar(&flagCPUs, "cpus", 0, "number of vCPUs (default: 1)")
	runCmd.Flags().StringVar(&flagMemory, "memory", "", `RAM size, e.g. "512M", "1G", "2048M" (default: 512M)`)
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

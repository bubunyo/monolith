package vm

import (
	"fmt"
	"math/rand/v2"
	"regexp"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)

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
		fmt.Printf("Booting VM %q...\n", name)
		return monolith.Run(name)
	},
}

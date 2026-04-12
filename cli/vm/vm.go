package vm

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for all VM operations.
var Cmd = &cobra.Command{
	Use:   "vm",
	Short: "Manage virtual machines",
}

func init() {
	Cmd.AddCommand(runCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(rmCmd)
	Cmd.AddCommand(topCmd)
}

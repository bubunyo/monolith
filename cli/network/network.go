package network

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for all network operations.
var Cmd = &cobra.Command{
	Use:     "network",
	Aliases: []string{"net"},
	Short:   "Manage named networks",
}

func init() {
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(startCmd)
	Cmd.AddCommand(stopCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(rmCmd)
	Cmd.AddCommand(logsCmd)
}

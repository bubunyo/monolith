package cli

import (
	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

// netdCmd is a hidden command used to run the network daemon.
// It is invoked by `monolith network create` via os/exec.
var netdCmd = &cobra.Command{
	Use:    "__netd",
	Short:  "Internal: run network daemon",
	Hidden: true,
	Args:   cobra.ExactArgs(2), // name, mode
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		mode := args[1]
		return monolith.RunNetworkDaemon(name, mode)
	},
}

func init() {
	rootCmd.AddCommand(netdCmd)
}

package cli

import (
	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var vmdRestore bool

// vmdCmd is a hidden command used to run the VM daemon process.
// It is invoked by `monolith vm run` via os/exec to boot a VM in the background.
var vmdCmd = &cobra.Command{
	Use:    "__vmd",
	Short:  "Internal: run VM daemon",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		return monolith.RunDaemon(name, vmdRestore)
	},
}

func init() {
	vmdCmd.Flags().BoolVar(&vmdRestore, "restore", false, "restore from suspend file instead of fresh boot")
	rootCmd.AddCommand(vmdCmd)
}

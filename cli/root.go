package cli

import (
	"os"

	"github.com/bubunyo/monolith/cli/vm"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "monolith",
	Short: "Lightweight Linux VM manager for macOS",
	Long:  "monolith creates and manages lightweight Linux virtual machines using Apple Virtualization.framework.",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(vm.Cmd)
}

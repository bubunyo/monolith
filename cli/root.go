package cli

import (
	"fmt"
	"os"

	"github.com/bubunyo/monolith/cli/docker"
	"github.com/bubunyo/monolith/cli/network"
	"github.com/bubunyo/monolith/cli/vm"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:           "monolith",
	Short:         "Lightweight Linux VM manager for macOS",
	Long:          "monolith creates and manages lightweight Linux virtual machines using Apple Virtualization.framework.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(vm.Cmd)
	rootCmd.AddCommand(network.Cmd)
	rootCmd.AddCommand(docker.Cmd)
}

package vm

import (
	"fmt"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var flagForce bool

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a running VM",
	Long: `Gracefully stop a running or paused VM. Sends a shutdown request to the guest OS.

With --force/-f: immediately terminates the VM without giving the guest a chance
to shut down cleanly. Use this if the guest is unresponsive.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return stopVM(args[0], flagForce)
	},
}

// stopVM stops a running or paused VM, optionally with force.
func stopVM(name string, force bool) error {
	resp, err := monolith.SendControlCommand(name, monolith.ControlRequest{
		Cmd:   monolith.CmdStop,
		Force: force,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	if force {
		fmt.Printf("VM %q force stopped.\n", name)
	} else {
		fmt.Printf("VM %q stopped.\n", name)
	}
	return nil
}

func init() {
	stopCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "force stop without graceful shutdown")
}

package vm

import (
	"fmt"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var flagForce bool

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a running VM",
	Long: `Gracefully stop a running or paused VM. Sends a shutdown request to the guest OS
and waits up to 10s; if the guest hasn't stopped by then, it is force stopped.

With --force/-f: immediately terminates the VM without giving the guest a chance
to shut down cleanly. Use this if the guest is unresponsive.

For a suspended VM, discards the saved state so the next 'vm run' boots fresh.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return stopVM(args[0], flagForce)
	},
}

// stopVM stops a running or paused VM, optionally with force.
func stopVM(name string, force bool) error {
	// A suspended VM has no daemon; stopping it discards the snapshot.
	if status, err := vm.ReadState(name); err == nil && status.State == vm.StatusSuspended {
		if err := vm.DiscardSuspended(name); err != nil {
			return err
		}
		fmt.Printf("VM %q was suspended; discarded its saved state. It is now stopped.\n", name)
		return nil
	}

	resp, err := vm.SendControlCommand(name, vm.ControlRequest{
		Cmd:   vm.CmdStop,
		Force: force,
	})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	switch {
	case force:
		fmt.Printf("VM %q force stopped.\n", name)
	case resp.Forced:
		fmt.Printf("VM %q did not shut down in time; force stopped.\n", name)
	default:
		fmt.Printf("VM %q stopped.\n", name)
	}
	return nil
}

func init() {
	stopCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "force stop without graceful shutdown")
}

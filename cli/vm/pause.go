package vm

import (
	"fmt"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var pauseCmd = &cobra.Command{
	Use:   "pause <name>",
	Short: "Pause a running VM",
	Long:  "Freeze a running VM in memory. The guest clock stops and no CPU cycles are consumed, but RAM is still held. Resume with 'vm resume'.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return pauseVM(args[0])
	},
}

// pauseVM freezes a running VM in memory.
func pauseVM(name string) error {
	resp, err := vm.SendControlCommand(name, vm.ControlRequest{Cmd: vm.CmdPause})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	fmt.Printf("VM %q paused.\n", name)
	return nil
}

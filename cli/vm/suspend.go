package vm

import (
	"fmt"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var suspendCmd = &cobra.Command{
	Use:   "suspend <name>",
	Short: "Suspend a VM to disk",
	Long: `Save the VM's state to disk and stop it. The VM can be restored later with 'vm resume'.

The VM is paused, its full state (CPU, memory, devices) is written to a file,
and then the VM process exits. No resources are consumed while suspended.

Requires macOS 14+. The suspend file is encrypted and tied to this host machine.
The disk image must not be modified between suspend and resume.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return suspendVM(args[0])
	},
}

// suspendVM saves the VM's full state to disk and stops the daemon.
func suspendVM(name string) error {
	resp, err := vm.SendControlCommand(name, vm.ControlRequest{Cmd: vm.CmdSuspend})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	fmt.Printf("VM %q suspended to disk. Resume with 'monolith vm resume %s'.\n", name, name)
	return nil
}

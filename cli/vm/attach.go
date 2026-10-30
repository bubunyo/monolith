package vm

import (
	"fmt"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a running VM's console",
	Long:  "Connect to the serial console of a running or paused VM. Detach with Ctrl+C or Ctrl+] q.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return attachVM(args[0])
	},
}

// attachVM connects to the serial console of a running or paused VM.
func attachVM(name string) error {
	status, err := vm.ReadState(name)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}

	switch status.State {
	case vm.StatusRunning, vm.StatusPaused:
		// OK — attach.
	case vm.StatusSuspended:
		return fmt.Errorf("VM %q is suspended; use 'vm resume %s' to restore it first", name, name)
	default:
		return fmt.Errorf("VM %q is not running", name)
	}

	escapeCmd, err := vm.AttachConsole(name)
	if err != nil {
		return err
	}

	if escapeCmd != nil {
		return executeEscapeCommand(name, *escapeCmd)
	}

	fmt.Printf("Detached from %s. VM is still running.\n", name)
	return nil
}

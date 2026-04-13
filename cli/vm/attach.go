package vm

import (
	"fmt"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var attachCmd = &cobra.Command{
	Use:   "attach <name>",
	Short: "Attach to a running VM's console",
	Long:  "Connect to the serial console of a running or paused VM. Detach with Ctrl+C or Ctrl+] q.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		status, err := monolith.ReadState(name)
		if err != nil {
			return fmt.Errorf("reading VM state: %w", err)
		}

		switch status.State {
		case monolith.StatusRunning, monolith.StatusPaused:
			// OK — attach.
		case monolith.StatusSuspended:
			return fmt.Errorf("VM %q is suspended; use 'vm resume %s' to restore it first", name, name)
		default:
			return fmt.Errorf("VM %q is not running", name)
		}

		escapeCmd, err := monolith.AttachConsole(name)
		if err != nil {
			return err
		}

		// If the user issued a command from the escape prompt, execute it.
		if escapeCmd != nil {
			return executeEscapeCommand(name, *escapeCmd)
		}

		fmt.Printf("Detached from %s. VM is still running.\n", name)
		return nil
	},
}

package vm

import (
	"fmt"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var resumeCmd = &cobra.Command{
	Use:   "resume <name>",
	Short: "Resume a paused or suspended VM",
	Long: `Resume a VM that was paused (frozen in memory) or suspended (saved to disk).

For paused VMs: sends a resume command to the daemon. The guest clock resumes immediately.
For suspended VMs: spawns a new daemon that restores the VM state from the suspend file.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return resumeVM(args[0])
	},
}

// resumeVM resumes a paused VM or restores a suspended VM from disk.
func resumeVM(name string) error {
	status, err := monolith.ReadState(name)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}

	switch status.State {
	case monolith.StatusPaused:
		// VM is paused — send resume to daemon.
		resp, err := monolith.SendControlCommand(name, monolith.ControlRequest{Cmd: monolith.CmdResume})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}
		fmt.Printf("VM %q resumed.\n", name)
		return nil

	case monolith.StatusSuspended:
		// VM is suspended — spawn daemon with --restore.
		fmt.Printf("Restoring VM %q from suspend file...\n", name)
		if err := monolith.SpawnVMDaemon(name, true, ""); err != nil {
			return err
		}
		fmt.Printf("VM %q restored and running.\n", name)
		return nil

	case monolith.StatusRunning:
		return fmt.Errorf("VM %q is already running", name)

	default:
		return fmt.Errorf("VM %q is stopped and has no suspend file; use 'vm run %s' to start it", name, name)
	}
}

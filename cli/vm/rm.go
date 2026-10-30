package vm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var rmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"remove", "delete"},
	Short:   "Delete a VM",
	Long:    "Delete a virtual machine and all its data. This action is irreversible.",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return removeVM(args[0])
	},
}

// removeVM stops (if running) and deletes a VM after user confirmation.
func removeVM(name string) error {
	dir, err := vm.VMsDir()
	if err != nil {
		return err
	}

	vmPath := filepath.Join(dir, name)
	if _, err := os.Stat(vmPath); os.IsNotExist(err) {
		return fmt.Errorf("VM %q does not exist", name)
	}

	// Check if the VM is currently running or paused — stop it first.
	status, _ := vm.ReadState(name)
	if status != nil && (status.State == vm.StatusRunning || status.State == vm.StatusPaused) {
		fmt.Printf("VM %q is %s. Stopping it first...\n", name, status.State)
		resp, err := vm.SendControlCommand(name, vm.ControlRequest{
			Cmd:   vm.CmdStop,
			Force: true,
		})
		if err != nil {
			return fmt.Errorf("stopping VM %q: %w", name, err)
		}
		if !resp.OK {
			return fmt.Errorf("stopping VM %q: %s", name, resp.Error)
		}
	}

	fmt.Printf("Delete VM %q? This will remove all data. [y/N]: ", name)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "y" && answer != "yes" {
		fmt.Println("Cancelled.")
		return nil
	}

	if err := os.RemoveAll(vmPath); err != nil {
		return fmt.Errorf("removing VM %q: %w", name, err)
	}

	fmt.Printf("VM %q deleted.\n", name)
	return nil
}

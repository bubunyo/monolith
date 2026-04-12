package network

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var rmCmd = &cobra.Command{
	Use:     "rm <name>",
	Aliases: []string{"remove", "delete"},
	Short:   "Delete a named network",
	Long:    "Stop the network daemon and remove a named network. VMs using this network should be stopped first.",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		dir, err := monolith.NetworkDir(name)
		if err != nil {
			return err
		}

		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return fmt.Errorf("network %q does not exist", name)
		}

		// Check if any VMs are still using this network.
		vms := findVMsOnNetwork(name)
		if len(vms) > 0 {
			return fmt.Errorf("network %q is still referenced by VMs: %s. Stop and reconfigure them first", name, strings.Join(vms, ", "))
		}

		fmt.Printf("Delete network %q? [y/N]: ", name)
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))

		if answer != "y" && answer != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}

		// Stop the daemon if it's running.
		pid, err := monolith.ReadNetworkPid(name)
		if err == nil && pid > 0 {
			proc, err := os.FindProcess(pid)
			if err == nil {
				// Send SIGTERM to the daemon.
				if err := proc.Signal(syscall.SIGTERM); err == nil {
					fmt.Printf("Stopped network daemon (pid %d)\n", pid)
				}
			}
		}

		// Remove the network directory.
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing network %q: %w", name, err)
		}

		fmt.Printf("Network %q deleted.\n", name)
		return nil
	},
}

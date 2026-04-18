package network

import (
	"bufio"
	"fmt"
	"os"
	"strings"

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
		return removeNetwork(args[0])
	},
}

// removeNetwork stops the daemon and deletes a named network after confirmation.
func removeNetwork(name string) error {
	dir, err := monolith.NetworkDir(name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("network %q does not exist", name)
	}

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

	stopped, _ := stopDaemon(name)
	if stopped {
		fmt.Printf("Stopped network daemon.\n")
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing network %q: %w", name, err)
	}

	fmt.Printf("Network %q deleted.\n", name)
	return nil
}

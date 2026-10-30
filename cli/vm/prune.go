package vm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove all stopped VMs",
	Long:  "Delete all virtual machines that are not currently running. This action is irreversible.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return pruneVMs()
	},
}

// pruneVMs finds all stopped VMs and deletes them after user confirmation.
func pruneVMs() error {
	dir, err := vm.VMsDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Nothing to prune.")
			return nil
		}
		return err
	}

	type staleVM struct {
		name     string
		diskSize string
	}
	var stale []staleVM

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()

		// Skip VMs that are not stopped (running, paused, or suspended).
		status, _ := vm.ReadState(name)
		if status != nil && status.State != vm.StatusStopped {
			continue
		}

		// Check disk size for display.
		diskPath := filepath.Join(dir, name, "disk.img")
		diskSize := "no disk"
		var st syscall.Stat_t
		if err := syscall.Stat(diskPath, &st); err == nil {
			diskSize = formatSize(st.Blocks * 512)
		}

		stale = append(stale, staleVM{name: name, diskSize: diskSize})
	}

	if len(stale) == 0 {
		fmt.Println("Nothing to prune.")
		return nil
	}

	fmt.Println("The following stopped VMs will be deleted:")
	for _, vm := range stale {
		fmt.Printf("  %-20s (%s)\n", vm.name, vm.diskSize)
	}

	fmt.Printf("\nDelete %d stopped VM(s)? This is irreversible. [y/N]: ", len(stale))
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "y" && answer != "yes" {
		fmt.Println("Cancelled.")
		return nil
	}

	for _, vm := range stale {
		vmPath := filepath.Join(dir, vm.name)
		if err := os.RemoveAll(vmPath); err != nil {
			fmt.Fprintf(os.Stderr, "error removing %s: %v\n", vm.name, err)
			continue
		}
	}

	fmt.Printf("Pruned %d VM(s).\n", len(stale))
	return nil
}

package network

import (
	"fmt"
	"os"
	"syscall"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a network daemon",
	Long:  "Stop the background daemon for a named network. The network configuration is preserved and can be restarted with 'network start'.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return stopNetwork(args[0])
	},
}

// stopNetwork stops the daemon for a named network.
func stopNetwork(name string) error {
	stopped, err := stopDaemon(name)
	if err != nil {
		return err
	}
	if !stopped {
		fmt.Printf("Network %q daemon is not running.\n", name)
		return nil
	}
	fmt.Printf("Network %q daemon stopped.\n", name)
	return nil
}

// stopDaemon sends SIGTERM to the daemon for the named network.
// Returns true if a daemon was found and signaled, false if not running.
func stopDaemon(name string) (bool, error) {
	dir, err := monolith.NetworkDir(name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, fmt.Errorf("network %q does not exist", name)
	}

	pid, err := monolith.ReadNetworkPid(name)
	if err != nil || pid <= 0 {
		return false, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process already dead.
		monolith.CleanNetworkPid(name)
		return false, nil
	}

	return true, nil
}

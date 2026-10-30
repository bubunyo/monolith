package network

import (
	"fmt"
	"os"

	mnet "github.com/bubunyo/monolith/net"

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

// stopDaemon unloads the network's launchd job, which stops the daemon.
// Returns true if a running daemon was stopped, false if it wasn't running.
func stopDaemon(name string) (bool, error) {
	dir, err := mnet.NetworkDir(name)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return false, fmt.Errorf("network %q does not exist", name)
	}
	return mnet.StopNetworkDaemon(name)
}

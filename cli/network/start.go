package network

import (
	"fmt"
	"os"
	"strings"
	"time"

	mnet "github.com/bubunyo/monolith/net"

	"github.com/spf13/cobra"
)

var flagStartVerbose bool

var startCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a network daemon",
	Long:  "Start the background daemon for an existing named network. The network must have been previously created with 'network create'.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return startNetwork(args[0], flagStartVerbose)
	},
}

// startNetwork starts the daemon for an existing named network.
func startNetwork(name string, verbose bool) error {
	dir, err := mnet.NetworkDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("network %q does not exist (use 'network create' first)", name)
	}

	if mnet.IsNetworkDaemonAlive(name) {
		return fmt.Errorf("network %q daemon is already running", name)
	}

	cfg, err := mnet.ReadNetworkConfig(name)
	if err != nil {
		return fmt.Errorf("reading network config: %w", err)
	}

	if err := spawnDaemon(name, cfg.Mode, verbose); err != nil {
		return err
	}

	cfg, err = mnet.ReadNetworkConfig(name)
	if err != nil {
		fmt.Printf("Network %q started (mode: %s)\n", name, cfg.Mode)
	} else {
		fmt.Printf("Network %q started (mode: %s, subnet: %s)\n", name, cfg.Mode, cfg.Subnet)
	}
	return nil
}

func init() {
	startCmd.Flags().BoolVarP(&flagStartVerbose, "verbose", "v", false, "enable packet-level logging in daemon")
}

// spawnDaemon loads the network's launchd job and waits for the daemon to
// publish the network.
func spawnDaemon(name, mode string, verbose bool) error {
	if err := mnet.StartNetworkDaemon(name, mode, verbose); err != nil {
		return err
	}

	logPath, err := mnet.NetworkLogPath(name)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(10 * time.Second)
	for !mnet.IsNetworkDaemonAlive(name) {
		if time.Now().After(deadline) {
			// Unload the job so a failed start doesn't leave it registered.
			mnet.StopNetworkDaemon(name)
			tail := tailLog(logPath, 10)
			if tail != "" {
				return fmt.Errorf("daemon for network %q failed to start. Last log lines:\n%s", name, tail)
			}
			return fmt.Errorf("daemon for network %q failed to start (check %s)", name, logPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// tailLog reads the last n lines of a log file. Returns "" on any failure.
func tailLog(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "  " + strings.Join(lines, "\n  ")
}

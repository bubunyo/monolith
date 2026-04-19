package network

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/bubunyo/monolith"
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
	dir, err := monolith.NetworkDir(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("network %q does not exist (use 'network create' first)", name)
	}

	if monolith.IsNetworkDaemonAlive(name) {
		return fmt.Errorf("network %q daemon is already running", name)
	}

	cfg, err := monolith.ReadNetworkConfig(name)
	if err != nil {
		return fmt.Errorf("reading network config: %w", err)
	}

	if err := spawnDaemon(name, cfg.Mode, verbose); err != nil {
		return err
	}

	cfg, err = monolith.ReadNetworkConfig(name)
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

// spawnDaemon starts the network daemon as a background process.
func spawnDaemon(name, mode string, verbose bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	daemonArgs := []string{"__netd", name, mode}
	if verbose {
		daemonArgs = append(daemonArgs, "--verbose")
	}
	daemonCmd := exec.Command(exe, daemonArgs...)

	dir, err := monolith.NetworkDir(name)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(dir+"/daemon.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening daemon log: %w", err)
	}
	daemonCmd.Stdout = logFile
	daemonCmd.Stderr = logFile

	daemonCmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := daemonCmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("starting daemon: %w", err)
	}
	logFile.Close()

	time.Sleep(500 * time.Millisecond)

	if !monolith.IsNetworkDaemonAlive(name) {
		return fmt.Errorf("daemon for network %q failed to start (check %s/daemon.log)", name, dir)
	}

	return nil
}

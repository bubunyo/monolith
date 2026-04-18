package vm

import (
	"fmt"

	"github.com/bubunyo/monolith"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
)

var topCmd = &cobra.Command{
	Use:   "top <name>",
	Short: "Live monitoring dashboard for a VM",
	Long:  "Display a live-updating dashboard showing CPU, memory, disk, and network metrics for a running VM.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return topVM(args[0])
	},
}

// topVM launches a live TUI dashboard for a running or paused VM.
func topVM(name string) error {
	status, err := monolith.ReadState(name)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	if status.State != monolith.StatusRunning && status.State != monolith.StatusPaused {
		return fmt.Errorf("VM %q is not running (state: %s)", name, status.State)
	}

	p := tea.NewProgram(newModel(name), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

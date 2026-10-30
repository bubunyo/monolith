package network

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
	"github.com/muesli/termenv"
)

// styles holds all the lipgloss styles used by the topology renderer.
// They are no-ops when color output is disabled.
type styles struct {
	enabled bool

	// Section headings
	internet  lipgloss.Style
	hostOnly  lipgloss.Style
	airgapped lipgloss.Style

	// Sub-section headings ("Bridged (physical NICs)", "NAT", ...)
	subSection lipgloss.Style

	// Tree connectors and pseudo-labels
	tree   lipgloss.Style
	pseudo lipgloss.Style // (anonymous), (missing: ...)
	dim    lipgloss.Style // generic dim text

	// Names
	vmName  lipgloss.Style
	netName lipgloss.Style
	iface   lipgloss.Style // physical NIC name (e.g. "en0")

	// State labels
	stateRunning   lipgloss.Style
	stateStopped   lipgloss.Style
	statePaused    lipgloss.Style
	stateSuspended lipgloss.Style
	stateMissing   lipgloss.Style

	// IPs and subnets
	addr   lipgloss.Style
	subnet lipgloss.Style
}

// newStyles returns a styles configured for the given writer. When the
// writer is not a terminal, or NO_COLOR is set, colors are disabled.
func newStyles(w io.Writer) styles {
	enabled := isColorTerminal(w)

	s := styles{enabled: enabled}
	if !enabled {
		// All styles default to no-op (zero value) lipgloss.Style which
		// renders the input string unchanged.
		return s
	}

	s.internet = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))   // bright cyan
	s.hostOnly = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))  // amber
	s.airgapped = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")) // soft red

	s.subSection = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245"))

	s.tree = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	s.pseudo = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("244"))
	s.dim = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))

	s.vmName = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	s.netName = lipgloss.NewStyle().Foreground(lipgloss.Color("153"))
	s.iface = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("180"))

	s.stateRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))    // green
	s.stateStopped = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))   // dim grey
	s.statePaused = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))    // yellow
	s.stateSuspended = lipgloss.NewStyle().Foreground(lipgloss.Color("147")) // soft purple
	s.stateMissing = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))   // red

	s.addr = lipgloss.NewStyle().Foreground(lipgloss.Color("110"))  // soft blue (VM IPs)
	s.subnet = lipgloss.NewStyle().Foreground(lipgloss.Color("25")) // darker blue (network subnets)
	return s
}

// stateStyle returns the style appropriate for a given state string.
func (s styles) stateStyle(state string) lipgloss.Style {
	switch state {
	case "running":
		return s.stateRunning
	case "stopped":
		return s.stateStopped
	case "paused":
		return s.statePaused
	case "suspended":
		return s.stateSuspended
	case "missing":
		return s.stateMissing
	default:
		return s.dim
	}
}

// isColorTerminal returns true when w is a terminal that should receive
// ANSI colors. It honours the NO_COLOR environment variable.
func isColorTerminal(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	if !isatty.IsTerminal(f.Fd()) && !isatty.IsCygwinTerminal(f.Fd()) {
		return false
	}
	// Probe termenv to make sure the terminal advertises color support.
	prof := termenv.NewOutput(f).Profile
	return prof != termenv.Ascii
}

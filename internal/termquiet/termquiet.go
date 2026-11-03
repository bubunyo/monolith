// Package termquiet stops monolith asking the terminal for its background
// colour at startup.
//
// bubbletea's package init (v1) calls lipgloss.HasDarkBackground, which
// writes an OSC 11 query to the terminal and waits up to 5s for the reply,
// reading stdin raw. Terminals that don't answer stall every command, and
// keys typed ahead are read as a possible reply and lost. monolith uses fixed
// colours only, so the answer is never needed.
//
// Importing this package from main is enough. Go initialises ready packages
// in import-path order, and github.com/bubunyo/... sorts before
// github.com/charmbracelet/bubbletea, so this init runs first.
package termquiet

import "github.com/charmbracelet/lipgloss"

func init() { lipgloss.SetHasDarkBackground(true) }

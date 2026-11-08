// Package ansi has the terminal helpers shared by monolith's live views:
// whether to colour, SGR painting, the terminal's width, and turning raw
// console output into plain text.
package ansi

import (
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// IsTerminal reports whether w is a terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}

// UseColor reports whether to colour output to a terminal: not with
// NO_COLOR set or TERM=dumb.
func UseColor(tty bool) bool {
	return tty && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// Paint wraps s in an SGR style when color is on.
func Paint(color bool, style, s string) string {
	if !color || style == "" || s == "" {
		return s
	}
	return "\033[" + style + "m" + s + "\033[0m"
}

// Size returns w's terminal size, or def for either dimension it can't
// read.
func Size(w io.Writer, defCols, defRows int) (cols, rows int) {
	cols, rows = defCols, defRows
	f, ok := w.(*os.File)
	if !ok {
		return cols, rows
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return cols, rows
	}
	if ws.Col > 0 {
		cols = int(ws.Col)
	}
	if ws.Row > 0 {
		rows = int(ws.Row)
	}
	return cols, rows
}

// Plain turns one line of terminal output into text safe to redraw:
// escape sequences (CSI, OSC and the rest) are dropped, a carriage return
// keeps only what was written after it, text drawn between saving and
// restoring the cursor (ESC 7, ESC 8: apk's progress bar) goes, tabs
// become spaces and other control characters go.
func Plain(s string) string {
	if i := strings.LastIndexByte(strings.TrimRight(s, "\r"), '\r'); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	saved := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '7':
			saved, i = b.Len(), i+1
		case c == 0x1b && i+1 < len(s) && s[i+1] == '8':
			if saved >= 0 {
				t := b.String()[:saved]
				b.Reset()
				b.WriteString(t)
			}
			saved, i = -1, i+1
		case c == 0x1b:
			i = skipEscape(s, i)
		case c == '\t':
			b.WriteString("    ")
		case c < 0x20 || c == 0x7f:
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// skipEscape returns the index of the last byte of the escape sequence
// starting at s[i] (an ESC).
func skipEscape(s string, i int) int {
	if i+1 >= len(s) {
		return i
	}
	switch s[i+1] {
	case '[': // CSI: parameters, then a final byte in 0x40..0x7e
		for j := i + 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j
			}
		}
		return len(s) - 1
	case ']', 'P', '_', '^': // OSC and other strings: up to BEL or ESC \
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 1
			}
		}
		return len(s) - 1
	default: // two-byte sequences such as ESC 7 or ESC (B's first part
		return i + 1
	}
}

// Cut shortens s to at most width runes.
func Cut(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(s) <= width {
		return s
	}
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	return string(r[:width])
}

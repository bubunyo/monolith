package oci

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bubunyo/monolith/internal/ansi"
	"golang.org/x/sys/unix"
)

// pullView shows a pull's progress the way `docker pull` does: one line
// per layer plus a summary line. On a terminal the lines are redrawn in
// place; otherwise each status change is printed once.
type pullView struct {
	w   io.Writer
	tty bool

	mu     sync.Mutex
	lines  []viewLine
	drawn  int // lines on screen from the last redraw
	stop   chan struct{}
	done   chan struct{}
	width  int
	color  bool
	closed bool
}

type viewLine struct {
	id          string // short layer digest, or a label
	status      string
	done, total int64 // for the bar; total 0 means no bar
}

func newPullView(w io.Writer, tty bool, ids []string) *pullView {
	v := &pullView{w: w, tty: tty, color: ansi.UseColor(tty), stop: make(chan struct{}), done: make(chan struct{}), width: 100}
	for _, id := range ids {
		v.lines = append(v.lines, viewLine{id: id, status: "Waiting"})
	}
	if f, ok := w.(*os.File); ok && tty {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 20 {
			v.width = int(ws.Col) - 1
		}
	}
	if tty {
		go v.loop()
	} else {
		close(v.done)
	}
	return v
}

// set updates line i. Off a terminal, a line is printed when its status
// changes (not for every byte).
func (v *pullView) set(i int, status string, done, total int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || i < 0 || i >= len(v.lines) {
		return
	}
	l := &v.lines[i]
	changed := l.status != status
	l.status, l.done, l.total = status, done, total
	if !v.tty && changed {
		fmt.Fprintf(v.w, "%s: %s\n", l.id, status)
	}
}

// add appends a line and returns its index.
func (v *pullView) add(id, status string) int {
	v.mu.Lock()
	v.lines = append(v.lines, viewLine{id: id})
	i := len(v.lines) - 1
	v.mu.Unlock()
	v.set(i, status, 0, 0)
	return i
}

func (v *pullView) loop() {
	defer close(v.done)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			v.redraw()
			return
		case <-t.C:
			v.redraw()
		}
	}
}

// redraw moves the cursor back over the lines drawn last time and writes
// them again, each cut to the terminal's width so none wraps.
func (v *pullView) redraw() {
	v.mu.Lock()
	defer v.mu.Unlock()
	var b strings.Builder
	if v.drawn > 0 {
		fmt.Fprintf(&b, "\033[%dA", v.drawn)
	}
	for _, l := range v.lines {
		b.WriteString("\r\033[2K")
		b.WriteString(l.render(v.width, v.color))
		b.WriteByte('\n')
	}
	v.drawn = len(v.lines)
	io.WriteString(v.w, b.String())
}

// close draws the final state and stops redrawing.
func (v *pullView) close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()
	if v.tty {
		close(v.stop)
	}
	<-v.done
}

// render formats the line, cut to width before colouring so escape codes
// don't count: "id: Status      [====>   ] 1.2MB/3.4MB".
func (l viewLine) render(width int, color bool) string {
	type part struct{ text, style string }
	parts := []part{{l.id + ": ", styleID}, {fmt.Sprintf("%-17s", l.status), statusStyle(l.status)}}
	if l.total > 0 && l.done < l.total {
		parts = append(parts, part{" " + bar(l.done, l.total, 40), styleBar}, part{" " + HumanSize(l.done) + "/" + HumanSize(l.total), styleDim})
	} else {
		parts[1].text = l.status
	}
	var b strings.Builder
	for _, p := range parts {
		r := []rune(p.text)
		if len(r) > width {
			r = r[:width]
		}
		width -= len(r)
		b.WriteString(ansi.Paint(color, p.style, string(r)))
		if width <= 0 {
			break
		}
	}
	return b.String()
}

// Colours (SGR) for terminals: ids dim yellow, work in progress cyan or
// blue, finished green, the bar cyan.
const (
	styleID    = "2;33"
	styleDim   = "2"
	styleBar   = "36"
	styleBusy  = "36"
	styleWork  = "34"
	styleDone  = "32"
	styleWarn  = "33"
	styleBold  = "1"
	styleGreen = "1;32"
)

func statusStyle(status string) string {
	switch {
	case strings.HasPrefix(status, "Downloading"):
		return styleBusy
	case strings.HasPrefix(status, "Extracting"), strings.HasPrefix(status, "Converting"):
		return styleWork
	case strings.HasPrefix(status, "Retrying"):
		return styleWarn
	case strings.HasPrefix(status, "Waiting"):
		return styleDim
	default: // complete, exists, converted
		return styleDone
	}
}

// bar draws "[=====>     ]".
func bar(done, total int64, width int) string {
	n := int(int64(width) * done / total)
	if n >= width {
		return "[" + strings.Repeat("=", width) + "]"
	}
	return "[" + strings.Repeat("=", n) + ">" + strings.Repeat(" ", width-n-1) + "]"
}

// countingReader reports bytes read through fn.
type countingReader struct {
	r  io.Reader
	n  int64
	fn func(n int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.fn(c.n)
	return n, err
}

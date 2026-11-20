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

// LoadView shows several images loading at once, like `docker pull` per
// image: a header, one line per layer and a stage line. On a terminal the
// active images are redrawn in place; a finished image is printed once, as a
// single line, above them, so a long list never outgrows the screen. Off a
// terminal each image prints a line per stage.
type LoadView struct {
	w     io.Writer
	tty   bool
	color bool
	width int

	mu     sync.Mutex
	groups []*LoadGroup
	drawn  int // live lines on screen from the last redraw
	stop   chan struct{}
	done   chan struct{}
	closed bool
}

// LoadGroup is one image in a LoadView.
type LoadGroup struct {
	v     *LoadView
	name  string
	note  string
	state groupState
	layer []viewLine
	stage viewLine
	err   string
	start time.Time
	took  time.Duration
	shown bool // its final line is printed
}

type groupState int

const (
	groupWaiting groupState = iota
	groupActive
	groupDone
	groupFailed
)

// maxLayerLines caps an image's layer lines on screen; past it, finished
// layers fold into one count.
const maxLayerLines = 8

func NewLoadView(w io.Writer, tty bool) *LoadView {
	v := &LoadView{w: w, tty: tty, color: ansi.UseColor(tty), width: 100, stop: make(chan struct{}), done: make(chan struct{})}
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

// Add appends an image, waiting until Start.
func (v *LoadView) Add(name string) *LoadGroup {
	v.mu.Lock()
	defer v.mu.Unlock()
	g := &LoadGroup{v: v, name: name}
	v.groups = append(v.groups, g)
	return g
}

// Start marks the image active with a note ("arm64") and its layer ids.
func (g *LoadGroup) Start(note string, layerIDs []string) {
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	g.note, g.state, g.start = note, groupActive, time.Now()
	g.layer = g.layer[:0]
	for _, id := range layerIDs {
		g.layer = append(g.layer, viewLine{id: id, status: "Waiting"})
	}
	g.v.printf("%s: %s\n", g.title(), "Started")
}

// SetLayer updates layer line i.
func (g *LoadGroup) SetLayer(i int, status string, done, total int64) {
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	if i >= 0 && i < len(g.layer) {
		g.layer[i].status, g.layer[i].done, g.layer[i].total = status, done, total
	}
}

// Stage sets the image's stage line ("Loading into cluster-default").
// Off a terminal a changed stage is printed.
func (g *LoadGroup) Stage(status string, done, total int64) {
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	if g.state == groupWaiting {
		g.state, g.start = groupActive, time.Now()
	}
	changed := g.stage.status != status
	g.stage = viewLine{id: "", status: status, done: done, total: total}
	if changed {
		g.v.printf("%s: %s\n", g.title(), status)
	}
}

// Done finishes the image.
func (g *LoadGroup) Done() {
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	g.state, g.took = groupDone, time.Since(g.start)
	g.v.printf("%s: Loaded in %s\n", g.title(), g.took.Round(100*time.Millisecond))
}

// Fail finishes the image with an error.
func (g *LoadGroup) Fail(err error) {
	g.v.mu.Lock()
	defer g.v.mu.Unlock()
	g.state, g.err = groupFailed, err.Error()
	g.v.printf("%s: Error: %s\n", g.title(), g.err)
}

func (g *LoadGroup) title() string {
	if g.note == "" {
		return g.name
	}
	return g.name + " (" + g.note + ")"
}

// printf writes off a terminal only; v.mu is held.
func (v *LoadView) printf(format string, args ...any) {
	if !v.tty && !v.closed {
		fmt.Fprintf(v.w, format, args...)
	}
}

func (v *LoadView) loop() {
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

// redraw goes back over the live lines, prints images that finished since
// the last redraw for good, then draws the active ones and the queue.
func (v *LoadView) redraw() {
	v.mu.Lock()
	defer v.mu.Unlock()
	var b strings.Builder
	if v.drawn > 0 {
		fmt.Fprintf(&b, "\033[%dA", v.drawn)
	}
	b.WriteString("\r\033[J")
	for _, g := range v.groups {
		if (g.state == groupDone || g.state == groupFailed) && !g.shown {
			b.WriteString(v.final(g))
			b.WriteByte('\n')
			g.shown = true
		}
	}
	live := v.liveLines()
	for _, l := range live {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	v.drawn = len(live)
	io.WriteString(v.w, b.String())
}

// final is a finished image's one line.
func (v *LoadView) final(g *LoadGroup) string {
	if g.state == groupFailed {
		return v.cut([]part{{"✘ ", styleFail}, {g.title(), styleBold}, {"  " + g.err, styleFail}})
	}
	return v.cut([]part{{"✔ ", styleDone}, {g.title(), styleBold}, {"  Loaded", styleDone},
		{fmt.Sprintf("  %s", g.took.Round(100*time.Millisecond)), styleDim}})
}

func (v *LoadView) liveLines() []string {
	var lines []string
	var waiting []string
	for _, g := range v.groups {
		switch g.state {
		case groupWaiting:
			waiting = append(waiting, g.name)
			continue
		case groupActive:
		default:
			continue
		}
		lines = append(lines, v.cut([]part{{g.title(), styleBold}}))
		finished, shownLayers := 0, []viewLine{}
		for _, l := range g.layer {
			if len(g.layer) > maxLayerLines && layerFinished(l.status) {
				finished++
				continue
			}
			shownLayers = append(shownLayers, l)
		}
		if finished > 0 {
			lines = append(lines, "  "+ansi.Paint(v.color, styleDone, fmt.Sprintf("%d of %d layers ready", finished, len(g.layer))))
		}
		for _, l := range shownLayers {
			lines = append(lines, "  "+l.render(v.width-2, v.color))
		}
		if g.stage.status != "" {
			s := g.stage
			parts := []part{{"  " + s.status, statusStyle(s.status)}}
			if s.total > 0 && s.done < s.total {
				parts = append(parts, part{" " + bar(s.done, s.total, 30), styleBar}, part{" " + HumanSize(s.done) + "/" + HumanSize(s.total), styleDim})
			}
			lines = append(lines, v.cut(parts))
		}
	}
	if len(waiting) > 0 {
		shown := waiting
		more := ""
		if len(shown) > 3 {
			shown, more = shown[:3], fmt.Sprintf(" and %d more", len(waiting)-3)
		}
		lines = append(lines, v.cut([]part{{"Waiting: ", styleDim}, {strings.Join(shown, ", ") + more, styleDim}}))
	}
	return lines
}

func layerFinished(status string) bool {
	return strings.HasPrefix(status, "Already exists") || strings.HasPrefix(status, "Download complete")
}

type part struct{ text, style string }

// cut joins parts, cut to the terminal width before colouring.
func (v *LoadView) cut(parts []part) string {
	width := v.width
	var b strings.Builder
	for _, p := range parts {
		r := []rune(p.text)
		if len(r) > width {
			r = r[:width]
		}
		width -= len(r)
		b.WriteString(ansi.Paint(v.color, p.style, string(r)))
		if width <= 0 {
			break
		}
	}
	return b.String()
}

// Close draws the final state and stops redrawing.
func (v *LoadView) Close() {
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

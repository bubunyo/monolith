// Package steps shows a task as a list of steps the way `docker buildx`
// does: the running step streams its output in a small scrolling window,
// and folds to one line with its duration when it finishes.
//
// On a terminal the view is redrawn in place. Finished steps are printed
// once above the live area and never redrawn. Off a terminal, every line is
// printed once, prefixed with its step.
package steps

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/bubunyo/monolith/internal/ansi"
)

// DefaultLines is the height of a running step's log window.
const DefaultLines = 12

// Options configure a View.
type Options struct {
	TTY     bool // redraw in place
	Color   bool
	Lines   int  // log window height; DefaultLines if 0
	Verbose bool // print every line of finished steps, not just their summary
}

// View is a list of steps.
type View struct {
	w   io.Writer
	opt Options

	mu        sync.Mutex
	steps     []*Step
	committed int // steps printed for good above the live area
	drawn     int // live lines on screen from the last redraw
	frame     int
	closed    bool
	stop      chan struct{}
	done      chan struct{}
}

type state int

const (
	pending state = iota
	running
	succeeded
	failed
	stopped
)

// Step is one step of a View. Its Write method takes the step's output;
// complete lines go into its log, escape sequences stripped.
type Step struct {
	v       *View
	title   string
	state   state
	summary string
	start   time.Time
	took    time.Duration
	tail    []string // the last Lines lines
	all     []string // every line, kept when Verbose
	partial []byte
}

// New starts a view on w listing titles as pending steps; more can be added
// with Step.
func New(w io.Writer, opt Options, titles ...string) *View {
	if opt.Lines <= 0 {
		opt.Lines = DefaultLines
	}
	v := &View{w: w, opt: opt, stop: make(chan struct{}), done: make(chan struct{})}
	for _, t := range titles {
		v.steps = append(v.steps, &Step{v: v, title: t})
	}
	if opt.TTY {
		go v.loop()
	} else {
		close(v.done)
	}
	return v
}

// Step returns the step with title, adding it as pending if there's none.
// Titles are unique within a view.
func (v *View) Step(title string) *Step {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, s := range v.steps {
		if s.title == title {
			return s
		}
	}
	s := &Step{v: v, title: title}
	v.steps = append(v.steps, s)
	return s
}

// Remove drops pending steps that turned out not to apply.
func (v *View) Remove(titles ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	keep := v.steps[:0]
	for _, s := range v.steps {
		drop := false
		for _, t := range titles {
			drop = drop || (s.title == t && s.state == pending)
		}
		if !drop {
			keep = append(keep, s)
		}
	}
	v.steps = keep
}

// Start marks the step running and returns it.
func (s *Step) Start() *Step {
	v := s.v
	v.mu.Lock()
	defer v.mu.Unlock()
	if s.state == pending {
		s.state = running
		s.start = time.Now()
		if !v.opt.TTY {
			fmt.Fprintf(v.w, "[%s] started\n", s.key())
		}
	}
	return s
}

// Write adds output to the step's log; a line is taken when its newline
// arrives.
func (s *Step) Write(p []byte) (int, error) {
	v := s.v
	v.mu.Lock()
	defer v.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := strings.IndexByte(string(s.partial), '\n')
		if i < 0 {
			break
		}
		s.addLine(string(s.partial[:i]))
		s.partial = s.partial[i+1:]
	}
	return len(p), nil
}

// Logf adds one formatted line to the step's log.
func (s *Step) Logf(format string, args ...any) {
	s.v.mu.Lock()
	defer s.v.mu.Unlock()
	s.addLine(fmt.Sprintf(format, args...))
}

func (s *Step) addLine(raw string) {
	line := ansi.Plain(raw)
	if strings.TrimSpace(line) == "" || s.state >= succeeded {
		return
	}
	v := s.v
	if !v.opt.TTY {
		fmt.Fprintf(v.w, "[%s] %s\n", s.key(), line)
		return
	}
	if v.opt.Verbose {
		s.all = append(s.all, line)
	}
	s.tail = append(s.tail, line)
	if len(s.tail) > v.opt.Lines {
		s.tail = s.tail[len(s.tail)-v.opt.Lines:]
	}
}

// Done marks the step finished, with a short summary for its line.
func (s *Step) Done(summary string) { s.finish(succeeded, summary) }

// Fail marks the step failed; its last lines stay on screen.
func (s *Step) Fail(err error) { s.finish(failed, err.Error()) }

func (s *Step) finish(st state, summary string) {
	v := s.v
	v.mu.Lock()
	defer v.mu.Unlock()
	if s.state >= succeeded {
		return
	}
	if s.state == pending {
		s.start = time.Now()
	}
	if len(s.partial) > 0 {
		s.addLine(string(s.partial))
		s.partial = nil
	}
	s.state, s.summary, s.took = st, summary, time.Since(s.start)
	if !v.opt.TTY {
		word := "done"
		if st == failed {
			word = "failed"
		}
		if summary != "" {
			word += ": " + summary
		}
		fmt.Fprintf(v.w, "[%s] %s (%s)\n", s.key(), word, duration(s.took))
	}
}

// key is the step's prefix off a terminal: its title in lower case.
func (s *Step) key() string { return strings.ToLower(s.title) }

func (v *View) loop() {
	defer close(v.done)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			v.redraw(true)
			return
		case <-t.C:
			v.redraw(false)
		}
	}
}

// Close draws the final state and stops redrawing. A step still running
// is shown as stopped; pending steps are dropped.
func (v *View) Close() {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		return
	}
	v.closed = true
	v.mu.Unlock()
	if v.opt.TTY {
		close(v.stop)
	}
	<-v.done
}

// redraw moves the cursor back over the live area, prints steps that have
// finished since last time for good, then the live area again.
func (v *View) redraw(final bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cols, rows := ansi.Size(v.w, 100, 40)
	width := cols - 1
	v.frame++

	var b strings.Builder
	if v.drawn > 0 {
		fmt.Fprintf(&b, "\033[%dA", v.drawn)
	}
	b.WriteString("\r\033[J")
	for v.committed < len(v.steps) {
		s := v.steps[v.committed]
		if final && s.state == running {
			s.state, s.took = stopped, time.Since(s.start)
		}
		if s.state < succeeded {
			break
		}
		v.writeFinished(&b, s, width)
		v.committed++
	}

	var live []string
	if !final {
		window := v.opt.Lines
		if room := rows - (len(v.steps) - v.committed) - 2; room < window {
			window = max(room, 3)
		}
		for _, s := range v.steps[v.committed:] {
			switch s.state {
			case pending:
				live = append(live, "  "+ansi.Paint(v.opt.Color, styleDim, ansi.Cut(s.title, width-2)))
			case running:
				live = append(live, v.header(s, width))
				tail := s.tail
				if len(tail) > window {
					tail = tail[len(tail)-window:]
				}
				for _, l := range tail {
					live = append(live, v.logLine(l, width))
				}
			default:
				live = append(live, v.header(s, width))
			}
		}
	}
	for _, l := range live {
		b.WriteString(l)
		b.WriteString("\033[K\n")
	}
	v.drawn = len(live)
	io.WriteString(v.w, b.String())
}

// writeFinished prints a finished step: its line, then every log line with
// Verbose, or the last lines of a failed step.
func (v *View) writeFinished(b *strings.Builder, s *Step, width int) {
	b.WriteString(v.header(s, width))
	b.WriteString("\033[K\n")
	var lines []string
	switch {
	case v.opt.Verbose:
		lines = s.all
	case s.state == failed:
		lines = s.tail
	}
	for _, l := range lines {
		b.WriteString(v.logLine(l, width))
		b.WriteString("\033[K\n")
	}
}

// header is a step's own line: icon, title, summary, then its time.
func (v *View) header(s *Step, width int) string {
	c := v.opt.Color
	var icon, style string
	took := s.took
	summary := s.summary
	switch s.state {
	case running:
		icon, style = spinner[v.frame%len(spinner)], styleBusy
		took = time.Since(s.start)
		if summary == "" && len(s.tail) == 0 {
			summary = "waiting for output"
		}
	case succeeded:
		icon, style = "✔", styleDone
	case failed:
		icon, style = "✘", styleFail
	case stopped:
		icon, style, summary = "■", styleWarn, "stopped"
	}
	title := fmt.Sprintf("%-9s", s.title)
	t := duration(took)
	room := width - 2 - len(title) - 1 - len(t) - 2
	text := ansi.Cut(summary, room)
	pad := room - len([]rune(text))
	sumStyle := ""
	switch s.state {
	case failed:
		sumStyle = styleFail
	case running, stopped:
		sumStyle = styleDim
	}
	return ansi.Paint(c, style, icon) + " " + ansi.Paint(c, styleBold, title) + " " +
		ansi.Paint(c, sumStyle, text) + strings.Repeat(" ", max(pad, 0)+2) + ansi.Paint(c, styleDim, t)
}

func (v *View) logLine(l string, width int) string {
	return "  " + ansi.Paint(v.opt.Color, styleDim, "│ "+ansi.Cut(l, width-4))
}

// duration formats d like "0.4s", "12.3s" or "2m05s".
func duration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	styleDim  = "2"
	styleBold = "1"
	styleBusy = "36"
	styleDone = "32"
	styleFail = "31"
	styleWarn = "33"
)

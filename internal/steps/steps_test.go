package steps

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// syncBuffer is a bytes.Buffer the view's redraw goroutine and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestPlainOutput(t *testing.T) {
	var out bytes.Buffer
	v := New(&out, Options{}, "Image", "Boot")
	img := v.Step("Image").Start()
	fmt.Fprint(img, "downloading\npart")
	fmt.Fprint(img, "ial line\n\n")
	img.Done("alpine (cached)")
	boot := v.Step("Boot").Start()
	fmt.Fprint(boot, "\x1b[32mkernel\x1b[0m\r\n")
	boot.Fail(errors.New("VM stopped"))
	v.Close()

	got := regexp.MustCompile(`\(\d+\.\ds\)`).ReplaceAllString(out.String(), "(T)")
	require.Equal(t, `[image] started
[image] downloading
[image] partial line
[image] done: alpine (cached) (T)
[boot] started
[boot] kernel
[boot] failed: VM stopped (T)
`, got)
}

// screen replays the view's output through a tiny terminal that knows the
// handful of sequences the view writes, and returns what's visible.
func screen(out string) []string {
	var rows []string
	y := 0
	for i := 0; i < len(out); i++ {
		switch c := out[i]; c {
		case '\n':
			y++
		case '\r':
		case 0x1b:
			j := i + 2
			for j < len(out) && (out[j] < 0x40 || out[j] > 0x7e) {
				j++
			}
			arg, cmd := out[i+2:j], out[j]
			switch cmd {
			case 'A':
				var n int
				fmt.Sscan(arg, &n)
				y -= n
			case 'J':
				if y < len(rows) {
					rows = rows[:y]
				}
			}
			i = j
		default:
			for len(rows) <= y {
				rows = append(rows, "")
			}
			rows[y] += out[i : i+1]
		}
	}
	return rows
}

func TestTerminalView(t *testing.T) {
	var out syncBuffer
	v := New(&out, Options{TTY: true, Lines: 3}, "Image", "Boot", "Ready")
	v.Step("Image").Start().Done("cached")
	boot := v.Step("Boot").Start()
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(boot, "line %d\n", i)
	}
	v.redraw(false)
	got := screen(out.String())
	require.Len(t, got, 6, "%q", got)
	require.Regexp(t, `^✔ Image +cached +0\.\ds$`, got[0])
	require.Regexp(t, `^. Boot `, got[1])
	require.Equal(t, []string{"  │ line 3", "  │ line 4", "  │ line 5"}, got[2:5], "the window keeps the last 3 lines")
	require.Equal(t, "  Ready", got[5])

	boot.Done("up")
	v.Remove("Ready")
	v.Close()
	got = screen(out.String())
	require.Len(t, got, 2, "finished steps fold to one line each: %q", got)
	require.Regexp(t, `^✔ Boot +up +0\.\ds$`, got[1])
}

func TestTerminalFailureKeepsTail(t *testing.T) {
	var out syncBuffer
	v := New(&out, Options{TTY: true, Lines: 2}, "Boot")
	boot := v.Step("Boot").Start()
	fmt.Fprint(boot, "a\nb\nc\n")
	boot.Fail(errors.New("VM stopped"))
	v.Close()
	got := screen(out.String())
	require.Len(t, got, 3, "%q", got)
	require.Regexp(t, `^✘ Boot +VM stopped`, got[0])
	require.Equal(t, []string{"  │ b", "  │ c"}, got[1:])
}

func TestVerboseKeepsEveryLine(t *testing.T) {
	var out syncBuffer
	v := New(&out, Options{TTY: true, Lines: 1, Verbose: true}, "Boot")
	boot := v.Step("Boot").Start()
	fmt.Fprint(boot, "a\nb\n")
	boot.Done("")
	v.Close()
	got := screen(out.String())
	require.Equal(t, []string{"  │ a", "  │ b"}, got[1:])
}

func TestCloseStopsRunningStep(t *testing.T) {
	var out syncBuffer
	v := New(&out, Options{TTY: true}, "Boot", "Ready")
	v.Step("Boot").Start()
	time.Sleep(150 * time.Millisecond) // at least one tick
	v.Close()
	got := screen(out.String())
	require.Len(t, got, 1, "%q", got)
	require.True(t, strings.HasPrefix(got[0], "■ Boot"), got[0])
	require.Contains(t, got[0], "stopped")
}

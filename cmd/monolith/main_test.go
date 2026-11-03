//go:build darwin

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestNoStartupTerminalQuery runs this test binary (which links the same
// packages as monolith, so the same inits run) on a pty that never answers,
// and checks nothing queried the terminal while starting up.
func TestNoStartupTerminalQuery(t *testing.T) {
	if os.Getenv("MONOLITH_TERMQUIET_CHILD") != "" {
		return // the inits have run; that's all the child is for
	}
	ptm, pts, err := openPTY()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptm.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestNoStartupTerminalQuery$")
	cmd.Env = append(os.Environ(), "MONOLITH_TERMQUIET_CHILD=1", "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	// Its own session with the pty as controlling terminal, in the
	// foreground: what termenv requires before it queries.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	start := time.Now()
	require.NoError(t, cmd.Start())
	pts.Close()

	var out bytes.Buffer
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()
	require.NoError(t, cmd.Wait())
	took := time.Since(start)
	select {
	case <-read: // the pty reports EIO once the child has gone
	case <-time.After(5 * time.Second):
		t.Fatal("pty output never ended")
	}

	if strings.Contains(out.String(), "\x1b]11;?") {
		t.Fatalf("startup queried the terminal's background colour (took %v): %q", took, out.String())
	}
	if took > 3*time.Second {
		t.Fatalf("startup took %v on a pty that doesn't answer", took)
	}
}

// openPTY opens a pty pair the macOS way: /dev/ptmx, grant, unlock, name.
func openPTY() (ptm, pts *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	ptm = os.NewFile(uintptr(fd), "/dev/ptmx")
	fail := func(err error) (*os.File, *os.File, error) {
		ptm.Close()
		return nil, nil, err
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return fail(err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return fail(err)
	}
	var name [128]byte
	//nolint:staticcheck
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		return fail(e)
	}
	pts, err = os.OpenFile(string(bytes.TrimRight(name[:], "\x00")), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	return ptm, pts, nil
}

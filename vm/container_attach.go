package vm

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

// AttachContainer relays a container's console to the terminal until the
// container exits or the user detaches.
//
// With tty, the terminal is raw and every key goes to the container,
// Ctrl+C included; Ctrl+P Ctrl+Q detaches, as with Docker. Without tty,
// output is copied as is, stdin isn't read, and Ctrl+C (SIGINT) and SIGTERM
// are delivered to the container's process.
//
// detached reports that the user detached and the container keeps running.
func AttachContainer(name string, tty bool) (detached bool, err error) {
	sockPath, err := VMSockPath(name)
	if err != nil {
		return false, err
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return false, fmt.Errorf("connecting to container %q: %w (is it running?)", name, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{MsgConsoleAttach}); err != nil {
		return false, err
	}
	ack := make([]byte, 1)
	if _, err := io.ReadFull(conn, ack); err != nil {
		return false, fmt.Errorf("attaching to %q: %w", name, err)
	}
	if ack[0] != MsgConsoleAttach {
		return false, fmt.Errorf("another client is attached to %q", name)
	}

	done := make(chan bool, 2) // true: detached
	go func() {
		io.Copy(os.Stdout, conn)
		done <- false
	}()

	if !tty {
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(sigs)
		for {
			select {
			case d := <-done:
				return d, nil
			case s := <-sigs:
				SendControlCommand(name, ControlRequest{Cmd: CmdSignal, Signal: int(s.(syscall.Signal))})
			}
		}
	}

	var orig unix.Termios
	if err := termios.Tcgetattr(os.Stdin.Fd(), &orig); err == nil {
		raw := orig
		termios.Cfmakeraw(&raw)
		termios.Tcsetattr(os.Stdin.Fd(), termios.TCSANOW, &raw)
		defer termios.Tcsetattr(os.Stdin.Fd(), termios.TCSANOW, &orig)
	}
	sendCurrentWinsize(name)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			sendCurrentWinsize(name)
		}
	}()

	go func() {
		buf := make([]byte, 256)
		pendingCtrlP := false
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				// The console can't be half-closed; end-of-file on a
				// terminal is Ctrl+D.
				conn.Write([]byte{0x04})
				return
			}
			out := make([]byte, 0, n+1)
			for _, b := range buf[:n] {
				if pendingCtrlP {
					pendingCtrlP = false
					if b == 0x11 { // Ctrl+P Ctrl+Q
						done <- true
						return
					}
					out = append(out, 0x10)
				}
				if b == 0x10 {
					pendingCtrlP = true
					continue
				}
				out = append(out, b)
			}
			if len(out) > 0 {
				if _, err := conn.Write(out); err != nil {
					return
				}
			}
		}
	}()
	return <-done, nil
}

// WaitDaemonExit waits for a VM's daemon to exit.
func WaitDaemonExit(name string, timeout time.Duration) error {
	sockPath, err := VMSockPath(name)
	if err != nil {
		return err
	}
	return waitForDaemonExit(sockPath, timeout)
}

// WaitContainerExit waits for a container's daemon to exit and returns how
// the container ended.
func WaitContainerExit(name string, timeout time.Duration) (ExitStatus, error) {
	if err := WaitDaemonExit(name, timeout); err != nil {
		return ExitStatus{}, err
	}
	st, ok := ReadExit(name)
	if !ok {
		return ExitStatus{}, fmt.Errorf("container %q left no exit status", name)
	}
	return st, nil
}

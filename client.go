package monolith

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

// AttachConsole connects to the VM daemon's Unix socket and relays the
// console PTY to the user's terminal. It handles the Ctrl+] escape sequence
// for the command prompt and Ctrl+C for quick detach.
//
// Returns a ControlRequest if the user issued a command from the escape
// prompt (pause, resume, suspend, stop), or nil on clean detach.
func AttachConsole(name string) (*ControlRequest, error) {
	sockPath, err := VMSockPath(name)
	if err != nil {
		return nil, err
	}
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to VM daemon %q: %w (is the VM running?)", name, err)
	}
	defer conn.Close()

	// Send console attach request.
	if _, err := conn.Write([]byte{MsgConsoleAttach}); err != nil {
		return nil, fmt.Errorf("sending attach request: %w", err)
	}

	// Read ACK.
	ack := make([]byte, 1)
	if _, err := io.ReadFull(conn, ack); err != nil {
		return nil, fmt.Errorf("reading attach ACK: %w", err)
	}
	if ack[0] == 0xFF {
		return nil, fmt.Errorf("another client is already attached to %q", name)
	}
	if ack[0] != MsgConsoleAttach {
		return nil, fmt.Errorf("unexpected ACK byte: 0x%02x", ack[0])
	}

	// Save terminal state and switch to raw mode.
	var origAttrs unix.Termios
	termios.Tcgetattr(os.Stdin.Fd(), &origAttrs)
	defer termios.Tcsetattr(os.Stdin.Fd(), termios.TCSANOW, &origAttrs)

	var raw unix.Termios
	termios.Tcgetattr(os.Stdin.Fd(), &raw)
	raw.Iflag &^= syscall.ICRNL
	raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	termios.Tcsetattr(os.Stdin.Fd(), termios.TCSANOW, &raw)

	// Send initial terminal size and propagate SIGWINCH. Resizes go over
	// the control channel so they never mix with console bytes.
	sendCurrentWinsize(name)
	sigWinch := make(chan os.Signal, 1)
	signal.Notify(sigWinch, syscall.SIGWINCH)
	defer signal.Stop(sigWinch)
	go func() {
		for range sigWinch {
			sendCurrentWinsize(name)
		}
	}()

	doneCh := make(chan error, 2)
	var escapeCmd *ControlRequest

	// conn → stdout (VM output to terminal).
	go func() {
		_, err := io.Copy(os.Stdout, conn)
		doneCh <- err
	}()

	// stdin → conn with escape sequence detection.
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				doneCh <- err
				return
			}

			chunk := buf[:n]
			out := chunk // what we'll write to conn

			for i := 0; i < len(chunk); i++ {
				b := chunk[i]

				if b == 0x03 { // Ctrl+C — detach
					doneCh <- nil
					return
				}

				if b == 0x1D { // Ctrl+] — command mode
					// Flush bytes before the escape byte.
					if i > 0 {
						if _, err := conn.Write(chunk[:i]); err != nil {
							doneCh <- err
							return
						}
					}
					cmd := handleEscapePrompt(conn, &origAttrs)
					if cmd != nil {
						escapeCmd = cmd
						doneCh <- nil
						return
					}
					// Cancelled — continue with remainder.
					chunk = chunk[i+1:]
					out = chunk
					i = -1
					continue
				}
			}

			if len(out) > 0 {
				if _, err := conn.Write(out); err != nil {
					doneCh <- err
					return
				}
			}
		}
	}()

	<-doneCh
	return escapeCmd, nil
}

// sendCurrentWinsize reads the local terminal size and sends it to the
// daemon as a resize control command. Errors are ignored: a missed resize
// only affects layout, never the console session itself.
func sendCurrentWinsize(name string) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return
	}
	SendControlCommand(name, ControlRequest{Cmd: CmdResize, Rows: ws.Row, Cols: ws.Col})
}

// handleEscapePrompt shows the monolith command prompt after Ctrl+] is pressed.
// Returns a ControlRequest if the user chose a command, or nil to continue.
func handleEscapePrompt(conn net.Conn, origAttrs *unix.Termios) *ControlRequest {
	// Temporarily switch to cbreak mode so we can echo our prompt.
	var cbreak unix.Termios
	termios.Tcgetattr(os.Stdin.Fd(), &cbreak)
	cbreak.Lflag |= syscall.ECHO
	cbreak.Lflag &^= syscall.ICANON
	termios.Tcsetattr(os.Stdin.Fd(), termios.TCSANOW, &cbreak)
	defer func() {
		// Restore raw mode.
		var raw unix.Termios
		termios.Tcgetattr(os.Stdin.Fd(), &raw)
		raw.Iflag &^= syscall.ICRNL
		raw.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
		raw.Cc[syscall.VMIN] = 1
		raw.Cc[syscall.VTIME] = 0
		termios.Tcsetattr(os.Stdin.Fd(), termios.TCSANOW, &raw)
	}()

	fmt.Fprintf(os.Stdout, "\r\nmonolith> ")

	buf := make([]byte, 1)
	n, err := os.Stdin.Read(buf)
	if err != nil || n == 0 {
		return nil
	}

	switch buf[0] {
	case 'q': // detach
		fmt.Fprintf(os.Stdout, "detach\r\n")
		return nil // nil signals clean detach

	case 'p': // pause
		fmt.Fprintf(os.Stdout, "pause\r\n")
		return &ControlRequest{Cmd: CmdPause}

	case 'r': // resume
		fmt.Fprintf(os.Stdout, "resume\r\n")
		return &ControlRequest{Cmd: CmdResume}

	case 'z': // suspend
		fmt.Fprintf(os.Stdout, "suspend\r\n")
		return &ControlRequest{Cmd: CmdSuspend}

	case 's': // stop
		fmt.Fprintf(os.Stdout, "stop\r\n")
		return &ControlRequest{Cmd: CmdStop}

	case '?', 'h': // help
		fmt.Fprintf(os.Stdout, "help\r\n")
		fmt.Fprintf(os.Stdout, "\r\n  q  - detach (VM keeps running)\r\n")
		fmt.Fprintf(os.Stdout, "  s  - stop VM (graceful shutdown)\r\n")
		fmt.Fprintf(os.Stdout, "  p  - pause VM\r\n")
		fmt.Fprintf(os.Stdout, "  r  - resume VM\r\n")
		fmt.Fprintf(os.Stdout, "  z  - suspend VM to disk\r\n")
		fmt.Fprintf(os.Stdout, "  ?  - this help\r\n")
		fmt.Fprintf(os.Stdout, "  Ctrl+] - send literal Ctrl+] to guest\r\n\r\n")
		return nil // stay attached after showing help

	case 0x1D: // Ctrl+] again — send literal to guest
		conn.Write([]byte{0x1D})
		return nil

	default:
		// Unknown key — ignore, stay attached.
		fmt.Fprintf(os.Stdout, "\r\n")
		return nil
	}
}

// SendControlCommand connects to the VM daemon and sends a control command.
// Returns the daemon's response.
func SendControlCommand(name string, req ControlRequest) (ControlResponse, error) {
	sockPath, err := VMSockPath(name)
	if err != nil {
		return ControlResponse{}, err
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("connecting to VM daemon %q: %w (is the VM running?)", name, err)
	}
	defer conn.Close()

	// Write control message type.
	if _, err := conn.Write([]byte{MsgControl}); err != nil {
		return ControlResponse{}, fmt.Errorf("sending control type: %w", err)
	}

	// Marshal and send request.
	reqData, err := MarshalControlRequest(req)
	if err != nil {
		return ControlResponse{}, fmt.Errorf("marshaling request: %w", err)
	}

	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(reqData)))
	if _, err := conn.Write(lenBuf); err != nil {
		return ControlResponse{}, fmt.Errorf("sending request length: %w", err)
	}
	if _, err := conn.Write(reqData); err != nil {
		return ControlResponse{}, fmt.Errorf("sending request data: %w", err)
	}

	// Read response: type byte + length-prefixed JSON.
	respType := make([]byte, 1)
	if _, err := io.ReadFull(conn, respType); err != nil {
		return ControlResponse{}, fmt.Errorf("reading response type: %w", err)
	}

	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return ControlResponse{}, fmt.Errorf("reading response length: %w", err)
	}
	respLen := binary.BigEndian.Uint32(lenBuf)
	if respLen > 1024*1024 {
		return ControlResponse{}, fmt.Errorf("response too large: %d bytes", respLen)
	}

	respData := make([]byte, respLen)
	if _, err := io.ReadFull(conn, respData); err != nil {
		return ControlResponse{}, fmt.Errorf("reading response data: %w", err)
	}

	return UnmarshalControlResponse(respData)
}

// SpawnVMDaemon starts the VM daemon as a background process.
// It uses the same binary (os.Executable) with the hidden __vmd command.
// Returns after the daemon has started and its socket is ready.
// A non-empty consoleLogPath is forwarded as --log-console.
func SpawnVMDaemon(name string, restore bool, consoleLogPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}

	dir, err := vmDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating VM directory: %w", err)
	}

	logPath, err := VMLogPath(name)
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening daemon log: %w", err)
	}

	args := []string{"__vmd", name}
	if restore {
		args = append(args, "--restore")
	}
	if consoleLogPath != "" {
		args = append(args, "--log-console", consoleLogPath)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("starting VM daemon: %w", err)
	}
	logFile.Close()

	// Wait for the daemon's socket to become available.
	sockPath, err := VMSockPath(name)
	if err != nil {
		return err
	}

	for i := 0; i < 100; i++ { // up to 10 seconds
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		// Check if the daemon process died.
		if cmd.Process != nil {
			if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
				return fmt.Errorf("VM daemon exited unexpectedly (check %s)", logPath)
			}
		}
	}

	return fmt.Errorf("VM daemon socket not ready after 10s (check %s)", logPath)
}

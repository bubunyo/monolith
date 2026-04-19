package monolith

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/Code-Hex/vz/v3"
	"github.com/pkg/errors"
	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

// RunDaemon is the main entry point for the VM daemon process.
// It boots (or restores) a VM, connects its serial console to a PTY,
// and listens on a Unix domain socket for console attachment and control
// commands. It blocks until the VM stops or a fatal error occurs.
func RunDaemon(name string, restore bool) error {
	defer CleanState(name)

	// Ignore SIGPIPE — the parent closes our stdout pipe after boot,
	// and we must not die when subsequent log writes fail.
	signal.Ignore(syscall.SIGPIPE)

	logger := NewLogger(os.Stderr)

	cfg, err := ReadConfig(name)
	if err != nil {
		return errors.Wrap(err, "ReadConfig")
	}

	kernelConfig, err := DefaultKernel()
	if err != nil {
		return errors.Wrap(err, "DefaultKernel")
	}

	rootfs, err := DefaultRootFS()
	if err != nil {
		return errors.Wrap(err, "DefaultRootFS")
	}

	disk, err := DefaultDisk(rootfs, kernelConfig.ModloopPath, name)
	if err != nil {
		return errors.Wrap(err, "DefaultDisk")
	}

	// Create a PTY pair for the serial console.
	// The VM writes/reads the master side; clients relay the slave side.
	ptm, pts, err := termios.Pty()
	if err != nil {
		return errors.Wrap(err, "creating PTY")
	}
	defer ptm.Close()
	defer pts.Close()

	// Set the PTY slave to raw mode so the line discipline doesn't
	// echo characters or interpret special bytes. Without this, the
	// default cooked mode causes duplicate/garbled output.
	var ptsAttrs unix.Termios
	termios.Tcgetattr(pts.Fd(), &ptsAttrs)
	ptsAttrs.Iflag &^= syscall.ICRNL | syscall.IXON | syscall.IXOFF | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR
	ptsAttrs.Oflag &^= syscall.OPOST
	ptsAttrs.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ECHOE | syscall.ECHOK | syscall.ECHONL | syscall.ISIG | syscall.IEXTEN
	ptsAttrs.Cflag |= syscall.CS8
	ptsAttrs.Cc[syscall.VMIN] = 1
	ptsAttrs.Cc[syscall.VTIME] = 0
	termios.Tcsetattr(pts.Fd(), termios.TCSANOW, &ptsAttrs)

	// Boot loader.
	bootLoader, err := vz.NewLinuxBootLoader(
		kernelConfig.KernelPath,
		vz.WithCommandLine("console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop"),
		vz.WithInitrd(kernelConfig.InitrdPath),
	)
	if err != nil {
		return errors.Wrap(err, "vz.NewLinuxBootLoader")
	}

	// VM configuration.
	vmCfg, err := vz.NewVirtualMachineConfiguration(bootLoader, cfg.CPUs, cfg.Memory)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtualMachineConfiguration")
	}

	// Storage.
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachment(disk, false)
	if err != nil {
		return errors.Wrap(err, "vz.NewDiskImageStorageDeviceAttachment")
	}
	storageConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioBlockDeviceConfiguration")
	}
	vmCfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{storageConfig})

	// Network.
	macStr, err := configureNetwork(vmCfg, cfg)
	if err != nil {
		return errors.Wrap(err, "configureNetwork")
	}

	// Serial console — connected to the PTY master.
	portAttachment, err := vz.NewFileHandleSerialPortAttachment(ptm, ptm)
	if err != nil {
		return errors.Wrap(err, "vz.NewFileHandleSerialPortAttachment")
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(portAttachment)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioConsoleDeviceSerialPortConfiguration")
	}
	vmCfg.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig})

	// Entropy.
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioEntropyDeviceConfiguration")
	}
	vmCfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	// Validate.
	validated, err := vmCfg.Validate()
	if !validated || err != nil {
		return fmt.Errorf("invalid VM configuration: %w", err)
	}

	vm, err := vz.NewVirtualMachine(vmCfg)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtualMachine")
	}

	// Start or restore the VM.
	if restore {
		suspendPath, err := VMSuspendPath(name)
		if err != nil {
			return errors.Wrap(err, "VMSuspendPath")
		}
		if _, err := os.Stat(suspendPath); err != nil {
			return fmt.Errorf("suspend file not found: %s", suspendPath)
		}
		logger.Info("restoring VM %q from %s", name, suspendPath)
		if err := vm.RestoreMachineStateFromURL(suspendPath); err != nil {
			return errors.Wrap(err, "RestoreMachineStateFromURL")
		}
		// Restore puts the VM in Paused state — resume it.
		if err := vm.Resume(); err != nil {
			return errors.Wrap(err, "Resume after restore")
		}
		// Clean up the suspend file now that we've successfully restored.
		os.Remove(suspendPath)
		logger.Info("VM %q restored and running", name)
	} else {
		if err := vm.Start(); err != nil {
			return errors.Wrap(err, "vz.Start")
		}
	}

	// Write state.
	if err := WriteState(name, os.Getpid(), macStr, StatusRunning); err != nil {
		logger.Warn("failed to write state file: %v", err)
	}

	// Set up Unix socket for client connections.
	sockPath, err := VMSockPath(name)
	if err != nil {
		return errors.Wrap(err, "VMSockPath")
	}
	os.Remove(sockPath) // clean stale socket
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listening on unix socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(sockPath)

	logger.Info("VM daemon %q started (pid=%d, sock=%s)", name, os.Getpid(), sockPath)

	// Daemon state — tracks the current console client and VM status.
	d := &vmDaemon{
		name:     name,
		vm:       vm,
		vmCfg:    vmCfg,
		pts:      pts,
		mac:      macStr,
		listener: listener,
		logger:   logger,
	}

	// Accept connections in background.
	connCh := make(chan net.Conn)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed
			}
			connCh <- conn
		}
	}()

	// Handle signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Main event loop.
	for {
		select {
		case <-sigCh:
			logger.Info("VM daemon %q received signal, stopping", name)
			d.stopVM(false)
			return nil

		case conn := <-connCh:
			d.handleConnection(conn)

		case newState := <-vm.StateChangedNotify():
			logger.Info("VM %q state changed: %v", name, newState)
			switch newState {
			case vz.VirtualMachineStateStopped:
				logger.Info("VM %q stopped", name)
				return nil
			case vz.VirtualMachineStateError:
				return fmt.Errorf("VM %q entered error state", name)
			case vz.VirtualMachineStatePaused:
				UpdateStateStatus(name, StatusPaused)
				d.detachConsoleClient()
			case vz.VirtualMachineStateRunning:
				UpdateStateStatus(name, StatusRunning)
			}
		}
	}
}

// vmDaemon holds the daemon's runtime state.
type vmDaemon struct {
	name     string
	vm       *vz.VirtualMachine
	vmCfg    *vz.VirtualMachineConfiguration
	pts      *os.File // PTY slave for console relay
	mac      string
	listener net.Listener
	logger   *Logger

	consoleMu     sync.Mutex
	consoleClient net.Conn      // currently attached console client (nil if none)
	consoleCancel chan struct{} // closed to stop the relay goroutines
}

// handleConnection reads the first byte to determine the connection type.
func (d *vmDaemon) handleConnection(conn net.Conn) {
	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(conn, typeBuf); err != nil {
		conn.Close()
		return
	}

	switch typeBuf[0] {
	case MsgConsoleAttach:
		d.handleConsoleAttach(conn)
	case MsgControl:
		d.handleControl(conn)
	default:
		conn.Close()
	}
}

// handleConsoleAttach sets up bidirectional relay between the client and the PTY slave.
func (d *vmDaemon) handleConsoleAttach(conn net.Conn) {
	d.consoleMu.Lock()
	if d.consoleClient != nil {
		d.consoleMu.Unlock()
		// Another client is already attached — reject.
		conn.Write([]byte{0xFF}) // NACK
		conn.Close()
		return
	}

	d.consoleClient = conn
	d.consoleCancel = make(chan struct{})
	cancel := d.consoleCancel
	d.consoleMu.Unlock()

	// ACK the attachment.
	conn.Write([]byte{MsgConsoleAttach})
	d.logger.Info("console client attached to %q", d.name)

	// Bidirectional relay: conn ↔ PTY slave.
	// PTY slave → conn (VM output to client).
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-cancel:
				return
			default:
			}
			n, err := d.pts.Read(buf)
			if err != nil {
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	// conn → PTY slave (client input to VM).
	// This runs in the current goroutine — blocks until client disconnects.
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-cancel:
				return
			default:
			}
			n, err := conn.Read(buf)
			if err != nil {
				break
			}
			if _, err := d.pts.Write(buf[:n]); err != nil {
				break
			}
		}
		// Client disconnected.
		d.consoleMu.Lock()
		if d.consoleClient == conn {
			d.consoleClient = nil
			close(d.consoleCancel)
		}
		d.consoleMu.Unlock()
		conn.Close()
		d.logger.Info("console client detached from %q", d.name)
	}()
}

// detachConsoleClient forcibly disconnects the current console client.
func (d *vmDaemon) detachConsoleClient() {
	d.consoleMu.Lock()
	defer d.consoleMu.Unlock()
	if d.consoleClient != nil {
		close(d.consoleCancel)
		d.consoleClient.Close()
		d.consoleClient = nil
	}
}

// handleControl reads a JSON control command and sends a JSON response.
func (d *vmDaemon) handleControl(conn net.Conn) {
	defer conn.Close()

	// Read length-prefixed JSON request.
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return
	}
	dataLen := binary.BigEndian.Uint32(lenBuf)
	if dataLen > 1024*1024 {
		return // sanity check
	}

	data := make([]byte, dataLen)
	if _, err := io.ReadFull(conn, data); err != nil {
		return
	}

	req, err := UnmarshalControlRequest(data)
	if err != nil {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: "invalid request"})
		return
	}

	d.logger.Info("control command %q for %q", req.Cmd, d.name)

	switch req.Cmd {
	case CmdPause:
		d.handlePause(conn)
	case CmdResume:
		d.handleResume(conn)
	case CmdSuspend:
		d.handleSuspend(conn)
	case CmdStop:
		d.handleStop(conn, req.Force)
	case CmdStatus:
		d.handleStatus(conn)
	default:
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("unknown command: %s", req.Cmd)})
	}
}

func (d *vmDaemon) handlePause(conn net.Conn) {
	if !d.vm.CanPause() {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: "VM cannot be paused in current state"})
		return
	}
	if err := d.vm.Pause(); err != nil {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("pause failed: %v", err)})
		return
	}
	d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusPaused})
}

func (d *vmDaemon) handleResume(conn net.Conn) {
	if !d.vm.CanResume() {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: "VM cannot be resumed in current state"})
		return
	}
	if err := d.vm.Resume(); err != nil {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("resume failed: %v", err)})
		return
	}
	d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusRunning})
}

func (d *vmDaemon) handleSuspend(conn net.Conn) {
	// Pause first if running.
	if d.vm.CanPause() {
		if err := d.vm.Pause(); err != nil {
			d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("pause before suspend failed: %v", err)})
			return
		}
	}

	// Check if save/restore is supported.
	if ok, err := d.vmCfg.ValidateSaveRestoreSupport(); !ok || err != nil {
		errMsg := "VM configuration does not support save/restore"
		if err != nil {
			errMsg = fmt.Sprintf("save/restore validation failed: %v", err)
		}
		// Resume the VM since we paused it but can't save.
		d.vm.Resume()
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: errMsg})
		return
	}

	suspendPath, err := VMSuspendPath(d.name)
	if err != nil {
		d.vm.Resume()
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("getting suspend path: %v", err)})
		return
	}

	d.logger.Info("saving VM %q state to %s", d.name, suspendPath)
	if err := d.vm.SaveMachineStateToPath(suspendPath); err != nil {
		d.vm.Resume()
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("save failed: %v", err)})
		return
	}

	// Write the suspended state before stopping.
	WriteSuspendState(d.name, d.mac, suspendPath)

	d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusSuspended})

	// Detach any console client and stop the VM.
	d.detachConsoleClient()

	// Stop the VM — the daemon will exit via the state change handler.
	if d.vm.CanStop() {
		d.vm.Stop()
	}
}

func (d *vmDaemon) handleStop(conn net.Conn, force bool) {
	d.detachConsoleClient()

	if force {
		if d.vm.CanStop() {
			if err := d.vm.Stop(); err != nil {
				d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("force stop failed: %v", err)})
				return
			}
		}
	} else {
		if d.vm.CanRequestStop() {
			if ok, err := d.vm.RequestStop(); !ok || err != nil {
				// Guest didn't accept graceful stop — try force.
				if d.vm.CanStop() {
					d.vm.Stop()
				}
			}
		} else if d.vm.CanStop() {
			d.vm.Stop()
		}
	}

	d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusStopped})
}

func (d *vmDaemon) handleStatus(conn net.Conn) {
	state := d.vm.State()
	var status string
	switch state {
	case vz.VirtualMachineStateRunning:
		status = StatusRunning
	case vz.VirtualMachineStatePaused:
		status = StatusPaused
	case vz.VirtualMachineStateStopped:
		status = StatusStopped
	default:
		status = fmt.Sprintf("vz:%d", int(state))
	}
	d.sendControlResponse(conn, ControlResponse{OK: true, Status: status})
}

// stopVM stops the VM, optionally forcefully.
func (d *vmDaemon) stopVM(force bool) {
	d.detachConsoleClient()
	if force && d.vm.CanStop() {
		d.vm.Stop()
	} else if d.vm.CanRequestStop() {
		d.vm.RequestStop()
	} else if d.vm.CanStop() {
		d.vm.Stop()
	}
}

// sendControlResponse writes a JSON control response to the connection.
func (d *vmDaemon) sendControlResponse(conn net.Conn, resp ControlResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}

	// Write type byte.
	conn.Write([]byte{MsgControl})

	// Write length-prefixed JSON.
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	conn.Write(lenBuf)
	conn.Write(data)
}

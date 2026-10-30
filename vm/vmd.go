package vm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"

	"github.com/Code-Hex/vz/v4"
	"github.com/pkg/errors"
	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

// RunDaemon is the main entry point for the VM daemon process.
// It boots (or restores) a VM, connects its serial console to a PTY,
// and listens on a Unix domain socket for console attachment and control
// commands. It blocks until the VM stops or a fatal error occurs.
//
// If consoleLogPath is non-empty, all guest console output is appended to
// that file from boot onward, independent of any attached client.
func RunDaemon(name string, restore bool, consoleLogPath string) error {
	var d *vmDaemon
	keepState := false
	defer func() {
		// A suspended VM keeps its state file so the next run/resume
		// restores from the snapshot instead of booting fresh. So does one
		// whose restore failed, so the snapshot stays reachable.
		if keepState || (d != nil && d.suspended.Load()) {
			return
		}
		CleanState(name)
	}()

	// Ignore SIGPIPE — the parent closes our stdout pipe after boot,
	// and we must not die when subsequent log writes fail.
	signal.Ignore(syscall.SIGPIPE)

	logger := monolith.NewLogger(os.Stderr)

	cfg, err := ReadConfig(name)
	if err != nil {
		return errors.Wrap(err, "ReadConfig")
	}

	image, err := LookupImage(cfg.Image)
	if err != nil {
		return err
	}
	spec, err := image.Prepare(name)
	if err != nil {
		return errors.Wrapf(err, "preparing %s image", image.Name())
	}
	logger.Info("VM %q booting %s image (kernel=%s, disk=%s, read-only=%t)", name, image.Name(), spec.KernelPath, spec.DiskPath, spec.DiskReadOnly)

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

	// Start draining the PTY slave immediately. We must do this BEFORE
	// vm.Start() so that early kernel/init output (which the framework
	// writes to ptm as soon as the VM boots) is read out of the slave's
	// input buffer rather than being lost or backpressuring the framework.
	// The bus keeps a ring buffer of recent output and broadcasts live
	// bytes to any attached console client (replaying the buffer on attach).
	var consoleLog *os.File
	if consoleLogPath != "" {
		consoleLog, err = os.OpenFile(consoleLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return errors.Wrap(err, "opening console log")
		}
		defer consoleLog.Close()
		logger.Info("tee'ing console for %q to %s", name, consoleLogPath)
	}
	var tee io.Writer
	if consoleLog != nil {
		tee = consoleLog
	}
	bus := newConsoleBus(tee)
	go bus.drain(pts, monolith.NewLogger(os.Stderr))

	// Boot loader.
	bootOpts := []vz.LinuxBootLoaderOption{vz.WithCommandLine(spec.CommandLine)}
	if spec.InitrdPath != "" {
		bootOpts = append(bootOpts, vz.WithInitrd(spec.InitrdPath))
	}
	bootLoader, err := vz.NewLinuxBootLoader(spec.KernelPath, bootOpts...)
	if err != nil {
		return errors.Wrap(err, "vz.NewLinuxBootLoader")
	}

	// VM configuration.
	vmCfg, err := vz.NewVirtualMachineConfiguration(bootLoader, cfg.CPUs, cfg.Memory)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtualMachineConfiguration")
	}

	// Platform. Restoring a saved state requires the same machine
	// identifier it was saved with, so each VM keeps one.
	machineID, err := loadMachineIdentifier(name)
	if err != nil {
		return errors.Wrap(err, "machine identifier")
	}
	platform, err := vz.NewGenericPlatformConfiguration(vz.WithGenericMachineIdentifier(machineID))
	if err != nil {
		return errors.Wrap(err, "vz.NewGenericPlatformConfiguration")
	}
	vmCfg.SetPlatformVirtualMachineConfiguration(platform)

	// Storage.
	diskAttachment, err := vz.NewDiskImageStorageDeviceAttachment(spec.DiskPath, spec.DiskReadOnly)
	if err != nil {
		return errors.Wrap(err, "vz.NewDiskImageStorageDeviceAttachment")
	}
	storageConfig, err := vz.NewVirtioBlockDeviceConfiguration(diskAttachment)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioBlockDeviceConfiguration")
	}
	storageDevices := []vz.StorageDeviceConfiguration{storageConfig}
	if spec.DataDiskPath != "" {
		dataAttachment, err := vz.NewDiskImageStorageDeviceAttachment(spec.DataDiskPath, false)
		if err != nil {
			return errors.Wrap(err, "data disk attachment")
		}
		dataConfig, err := vz.NewVirtioBlockDeviceConfiguration(dataAttachment)
		if err != nil {
			return errors.Wrap(err, "data disk device")
		}
		storageDevices = append(storageDevices, dataConfig)
	}
	vmCfg.SetStorageDevicesVirtualMachineConfiguration(storageDevices)

	// Shared directories (virtio-fs).
	var shareDevices []vz.DirectorySharingDeviceConfiguration
	for _, sh := range spec.Shares {
		dir, err := vz.NewSharedDirectory(sh.HostPath, false)
		if err != nil {
			return errors.Wrapf(err, "shared directory %s", sh.HostPath)
		}
		share, err := vz.NewSingleDirectoryShare(dir)
		if err != nil {
			return errors.Wrapf(err, "directory share %s", sh.HostPath)
		}
		fsConfig, err := vz.NewVirtioFileSystemDeviceConfiguration(sh.Tag)
		if err != nil {
			return errors.Wrapf(err, "virtio-fs device %q", sh.Tag)
		}
		fsConfig.SetDirectoryShare(share)
		shareDevices = append(shareDevices, fsConfig)
	}
	if len(shareDevices) > 0 {
		vmCfg.SetDirectorySharingDevicesVirtualMachineConfiguration(shareDevices)
	}

	// Network.
	// Snapshots taken before MACs were persisted only recorded theirs in
	// the suspended state file; restore must reuse it.
	if cfg.MAC == "" && restore {
		cfg.MAC = suspendedMAC(name)
	}
	macStr, userNet, err := mnet.ConfigureNetwork(vmCfg, cfg.NetworkMode, cfg.NetworkName, cfg.HostInterface, cfg.MAC)
	if err != nil {
		return errors.Wrap(err, "configureNetwork")
	}
	if userNet != nil {
		defer userNet.Close()
	}
	if cfg.MAC == "" {
		cfg.MAC = macStr
		if err := WriteConfig(name, cfg); err != nil {
			logger.Warn("failed to persist MAC %s: %v", macStr, err)
		}
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

	// Subscribe before starting: notifications are not replayed, so a
	// subscription made after Start could miss an early Stopped/Error.
	stateCh := make(chan vz.VirtualMachineState, 8)
	stopStateNotify, err := vm.StateChangedNotify(stateCh)
	if err != nil {
		return errors.Wrap(err, "StateChangedNotify")
	}
	defer stopStateNotify()

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
			keepState = true
			if keybagLocked() {
				return fmt.Errorf("%w (RestoreMachineStateFromURL: %v)", errKeybagLocked, err)
			}
			return errors.Wrap(err, "RestoreMachineStateFromURL")
		}
		// Restore puts the VM in Paused state — resume it.
		if err := vm.Resume(); err != nil {
			keepState = true
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

	var services *dockerServices
	if cfg.Image == ImageDocker {
		services, err = startDockerServices(name, userNet, logger)
		if err != nil {
			return errors.Wrap(err, "docker services")
		}
		defer services.close()
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
	d = &vmDaemon{
		name:     name,
		vm:       vm,
		vmCfg:    vmCfg,
		pts:      pts,
		mac:      macStr,
		listener: listener,
		logger:   logger,
		bus:      bus,
		docker:   services,
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

	// Commands like stop reply after acting, so let in-flight replies reach
	// the client before the process exits.
	defer d.waitForControl(2 * time.Second)

	// Main event loop.
	lastState := vm.State()
	for {
		select {
		case <-sigCh:
			logger.Info("VM daemon %q received signal, stopping", name)
			d.stopVM(false)
			return nil

		case conn := <-connCh:
			go d.handleConnection(conn)

		case <-stateCh:
			// The delivered value may be stale and intermediate states can
			// be skipped, so act on the current state.
			newState := vm.State()
			if newState == lastState {
				continue
			}
			lastState = newState
			logger.Info("VM %q state changed: %v", name, newState)
			switch newState {
			case vz.VirtualMachineStateStopped:
				logger.Info("VM %q stopped", name)
				return nil
			case vz.VirtualMachineStateError:
				return fmt.Errorf("VM %q entered error state", name)
			case vz.VirtualMachineStatePaused:
				// Suspend pauses before saving; don't clobber the
				// suspended status it writes.
				if !d.suspended.Load() {
					UpdateStateStatus(name, StatusPaused)
				}
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
	logger   *monolith.Logger
	bus      *consoleBus
	docker   *dockerServices // nil unless the VM uses the docker image

	suspended atomic.Bool    // set while suspending; keeps state.json on exit
	controlWG sync.WaitGroup // in-flight control requests

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
		d.controlWG.Add(1)
		defer d.controlWG.Done()
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

	// Subscribe to the console bus. The subscribe call atomically:
	//   1. Captures the current ring buffer contents (boot replay).
	//   2. Returns a channel that receives all subsequent live bytes.
	// This guarantees no bytes are lost between replay and live stream.
	replay, liveCh, unsubscribe := d.bus.subscribe()

	// Send the boot replay first so the user sees the kernel/init log.
	if len(replay) > 0 {
		if _, err := conn.Write(replay); err != nil {
			unsubscribe()
			d.consoleMu.Lock()
			d.consoleClient = nil
			close(d.consoleCancel)
			d.consoleMu.Unlock()
			conn.Close()
			return
		}
	}

	// Live PTY → conn relay (via bus).
	go func() {
		defer unsubscribe()
		for {
			select {
			case <-cancel:
				return
			case chunk, ok := <-liveCh:
				if !ok {
					return
				}
				if _, err := conn.Write(chunk); err != nil {
					return
				}
			}
		}
	}()

	// conn → PTY slave (client input to VM).
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
	case CmdResize:
		d.handleResize(conn, req.Rows, req.Cols)
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
	// Mark suspended before pausing so the main loop's Paused handler never
	// overwrites the suspended status. Every failure path clears it.
	d.suspended.Store(true)
	fail := func(resume bool, msg string) {
		d.suspended.Store(false)
		if resume {
			d.vm.Resume()
		}
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: msg})
	}

	// Pause first if running.
	if d.vm.CanPause() {
		if err := d.vm.Pause(); err != nil {
			fail(false, fmt.Sprintf("pause before suspend failed: %v", err))
			return
		}
	}

	// Check if save/restore is supported.
	if ok, err := d.vmCfg.ValidateSaveRestoreSupport(); !ok || err != nil {
		errMsg := "VM configuration does not support save/restore"
		if err != nil {
			errMsg = fmt.Sprintf("save/restore validation failed: %v", err)
		}
		fail(true, errMsg)
		return
	}

	suspendPath, err := VMSuspendPath(d.name)
	if err != nil {
		fail(true, fmt.Sprintf("getting suspend path: %v", err))
		return
	}

	// A leftover snapshot (e.g. from a failed restore followed by a fresh
	// boot) is obsolete, and VZ refuses to overwrite it.
	os.Remove(suspendPath)
	d.logger.Info("saving VM %q state to %s", d.name, suspendPath)
	if err := d.vm.SaveMachineStateToPath(suspendPath); err != nil {
		fail(true, fmt.Sprintf("save failed: %v", err))
		return
	}

	if err := WriteSuspendState(d.name, d.mac, suspendPath); err != nil {
		// Without a state file the snapshot is unreachable; keep running.
		os.Remove(suspendPath)
		fail(true, fmt.Sprintf("writing suspend state: %v", err))
		return
	}

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
	} else if d.requestStopAndWait(gracefulStopTimeout) {
		d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusStopped})
		return
	} else if d.vm.CanStop() {
		// The guest ignored the request (neither image handles it today).
		d.logger.Info("VM %q did not stop within %s; forcing", d.name, gracefulStopTimeout)
		if err := d.vm.Stop(); err != nil {
			d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("force stop after timeout failed: %v", err)})
			return
		}
		d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusStopped, Forced: true})
		return
	}

	d.sendControlResponse(conn, ControlResponse{OK: true, Status: StatusStopped})
}

// gracefulStopTimeout is how long `vm stop` waits for the guest to shut
// down after a stop request before forcing it.
const gracefulStopTimeout = 10 * time.Second

// requestStopAndWait asks the guest to shut down and reports whether the VM
// reached the stopped state within timeout.
func (d *vmDaemon) requestStopAndWait(timeout time.Duration) bool {
	if !d.vm.CanRequestStop() {
		return false
	}
	if ok, err := d.vm.RequestStop(); !ok || err != nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.vm.State() == vz.VirtualMachineStateStopped {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
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
	resp := ControlResponse{OK: true, Status: status}
	if d.docker != nil {
		resp.Ports = d.docker.forwards()
	}
	d.sendControlResponse(conn, resp)
}

// handleResize records the client's terminal size on the console PTY.
func (d *vmDaemon) handleResize(conn net.Conn, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: "resize requires non-zero rows and cols"})
		return
	}
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(d.pts.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		d.sendControlResponse(conn, ControlResponse{OK: false, Error: fmt.Sprintf("resize failed: %v", err)})
		return
	}
	d.sendControlResponse(conn, ControlResponse{OK: true})
}

// waitForControl waits up to timeout for in-flight control requests to
// finish writing their responses.
func (d *vmDaemon) waitForControl(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		d.controlWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
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

// loadMachineIdentifier returns the VM's persisted generic machine
// identifier, creating and saving one on first boot.
func loadMachineIdentifier(name string) (*vz.GenericMachineIdentifier, error) {
	path, err := VMMachineIDPath(name)
	if err != nil {
		return nil, err
	}
	if data, err := os.ReadFile(path); err == nil {
		return vz.NewGenericMachineIdentifierWithData(data)
	}
	id, err := vz.NewGenericMachineIdentifier()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, id.DataRepresentation(), 0644); err != nil {
		return nil, err
	}
	return id, nil
}

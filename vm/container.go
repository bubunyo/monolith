package vm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v4"
	"github.com/bubunyo/monolith"
	ci "github.com/bubunyo/monolith/containerinit"
	"github.com/bubunyo/monolith/vm/guestinit"
	"golang.org/x/sys/unix"
)

// ImageContainer is the base image of `monolith container` VMs: one VM per
// container, booting Kata's kernel with monolith-init as PID 1 and an APFS
// clone of the container image's disk as root. It isn't a `vm run --image`
// choice; `container run` creates these VMs.
const ImageContainer = "container"

// ContainerConfig makes a VM a container (`monolith container`).
type ContainerConfig struct {
	Image   string  `json:"image"`    // reference as the user gave it, for display
	ImageID string  `json:"image_id"` // image store ID the disk was cloned from
	Spec    ci.Spec `json:"spec"`     // the process, handed to monolith-init
	// StopSignal is what `container stop` sends first (the image's
	// STOPSIGNAL, e.g. "SIGQUIT" for nginx); default SIGTERM.
	StopSignal string    `json:"stop_signal,omitempty"`
	Created    time.Time `json:"created"`
	DiskSize   int64     `json:"disk_size"` // bytes (--disk)

	// Attach makes the daemon hold the process until a console client
	// attaches, so a foreground run sees all output. `run`/`start -a` set
	// it; `run -d`/`start` clear it.
	Attach bool `json:"attach,omitempty"`
	// AutoRemove deletes the container when it exits (`run -d --rm`; a
	// foreground --rm is removed by the CLI after reading the exit code).
	AutoRemove bool `json:"auto_remove,omitempty"`
}

// containerCmdline boots quietly (only the process's output on the
// console), configures eth0 by DHCP in the kernel, and runs the initrd's
// /init. panic=-1 ends the VM if PID 1 ever dies instead of hanging.
const containerCmdline = "console=hvc0 quiet loglevel=1 ip=dhcp rdinit=/init panic=-1"

// VMContainerDiskPath is a container's root disk (an APFS clone of its image).
func VMContainerDiskPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "rootfs.img")
}

// VMConsoleLogPath is a container's output (`container logs`).
func VMConsoleLogPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "console.log")
}

// VMExitPath records how a container's last run ended.
func VMExitPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "exit.json")
}

func vmInitrdPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "initrd.img")
}

// ExitStatus is how a container's process ended.
type ExitStatus struct {
	Code       int       `json:"code"`
	Error      string    `json:"error,omitempty"` // why the process couldn't run
	FinishedAt time.Time `json:"finished_at"`
}

// ReadExit returns how the container last exited; ok is false if it never
// has (or is running again).
func ReadExit(name string) (ExitStatus, bool) {
	var st ExitStatus
	path, err := VMExitPath(name)
	if err != nil {
		return st, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, false
	}
	return st, json.Unmarshal(data, &st) == nil
}

func writeExit(name string, st ExitStatus) error {
	path, err := VMExitPath(name)
	if err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// CreateContainer creates container VM name from an image disk: the VM
// directory, an APFS clone of the disk (instant, copy-on-write) extended
// to cfg.Container.DiskSize (sparse; monolith-init grows the filesystem at
// boot) and the config.
func CreateContainer(name string, cfg VMConfig, imageDisk string) error {
	if cfg.Container == nil {
		return errors.New("not a container config")
	}
	img, err := os.Stat(imageDisk)
	if err != nil {
		return err
	}
	if cfg.Container.DiskSize < img.Size() {
		return fmt.Errorf("the disk must be at least %d MiB for this image", img.Size()>>20)
	}
	dir, err := VMDir(name)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("a VM or container named %q already exists", name)
		}
		return err
	}
	disk, err := VMContainerDiskPath(name)
	if err != nil {
		return err
	}
	if err := unix.Clonefile(imageDisk, disk, unix.CLONE_NOFOLLOW); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("cloning the image disk: %w", err)
	}
	if err := os.Truncate(disk, cfg.Container.DiskSize); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("sizing the container disk: %w", err)
	}
	if err := WriteConfig(name, cfg); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

type containerImage struct{}

func (containerImage) Name() string { return ImageContainer }

func (containerImage) Prepare(name string) (*BootSpec, error) {
	cfg, err := ReadConfig(name)
	if err != nil {
		return nil, err
	}
	if cfg.Container == nil {
		return nil, fmt.Errorf("%q is not a container", name)
	}
	kernel, err := kataKernel()
	if err != nil {
		return nil, err
	}
	disk, err := VMContainerDiskPath(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(disk); err != nil {
		return nil, fmt.Errorf("container disk: %w", err)
	}
	init, err := guestinit.Binary()
	if err != nil {
		return nil, err
	}
	initrd, err := vmInitrdPath(name)
	if err != nil {
		return nil, err
	}
	if err := guestinit.WriteInitrd(initrd, init, cfg.Container.Spec); err != nil {
		return nil, fmt.Errorf("writing initrd: %w", err)
	}
	return &BootSpec{
		KernelPath:  kernel,
		InitrdPath:  initrd,
		CommandLine: containerCmdline,
		DiskPath:    disk,
		HandlesStop: true,
	}, nil
}

// containerAgent is the daemon's side of the vsock connection to
// monolith-init: it starts the process, delivers signals and terminal
// sizes, and records the exit code.
type containerAgent struct {
	name   string
	logger *monolith.Logger

	mu       sync.Mutex
	conn     *vz.VirtioSocketConnection
	ready    bool // the init is waiting for start
	attached bool // a console client is attached
	hold     bool // wait for a console client before starting
	stopSig  unix.Signal
	started  bool
	exited   bool
	stopping bool // a stop was requested: a VM stop without exit is a kill
	finished bool
	rows     uint16
	cols     uint16
}

func newContainerAgent(name string, cfg *ContainerConfig, logger *monolith.Logger) *containerAgent {
	os.Remove(mustPath(VMExitPath(name))) // running again
	a := &containerAgent{name: name, logger: logger, hold: cfg.Attach, stopSig: unix.SIGTERM}
	if cfg.StopSignal != "" {
		if n, err := ci.LinuxSignal(cfg.StopSignal); err == nil {
			a.stopSig = unix.Signal(n)
		} else {
			logger.Warn("container %q: %v; stopping with SIGTERM", name, err)
		}
	}
	return a
}

func mustPath(p string, err error) string {
	if err != nil {
		return ""
	}
	return p
}

// connect dials the init, which listens as soon as it's set up, and
// handles its messages until the VM stops.
func (a *containerAgent) connect(vm *vz.VirtualMachine) {
	devs := vm.SocketDevices()
	if len(devs) == 0 {
		a.logger.Warn("container %q has no vsock device", a.name)
		return
	}
	var conn *vz.VirtioSocketConnection
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		c, err := devs[0].Connect(ci.VsockPort)
		if err == nil {
			conn = c
			break
		}
	}
	if conn == nil {
		a.logger.Warn("container %q: monolith-init never answered on vsock", a.name)
		return
	}
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	// Without a console client within 30s, start anyway.
	go func() {
		time.Sleep(30 * time.Second)
		a.mu.Lock()
		a.hold = false
		a.maybeStartLocked()
		a.mu.Unlock()
	}()

	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var m ci.Message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Op {
		case ci.OpReady:
			a.mu.Lock()
			a.ready = true
			a.maybeStartLocked()
			a.mu.Unlock()
		case ci.OpStarted:
			a.logger.Info("container %q process started (pid %d in the VM)", a.name, m.PID)
		case ci.OpExit:
			a.logger.Info("container %q exited with code %d", a.name, m.Code)
			a.mu.Lock()
			a.exited = true
			a.mu.Unlock()
			if err := writeExit(a.name, ExitStatus{Code: m.Code, Error: m.Error, FinishedAt: time.Now()}); err != nil {
				a.logger.Warn("recording exit code: %v", err)
			}
			a.send(ci.Message{Op: ci.OpAck})
		}
	}
}

// consoleAttached is called when a console client attaches.
func (a *containerAgent) consoleAttached() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attached = true
	a.maybeStartLocked()
}

func (a *containerAgent) maybeStartLocked() {
	if a.started || !a.ready || (a.hold && !a.attached) {
		return
	}
	a.started = true
	if a.rows > 0 {
		a.sendLocked(ci.Message{Op: ci.OpResize, Rows: a.rows, Cols: a.cols})
	}
	a.sendLocked(ci.Message{Op: ci.OpStart})
}

func (a *containerAgent) send(m ci.Message) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sendLocked(m)
}

func (a *containerAgent) sendLocked(m ci.Message) bool {
	if a.conn == nil {
		return false
	}
	data, _ := json.Marshal(m)
	_, err := a.conn.Write(append(data, '\n'))
	return err == nil
}

// signal delivers sig to the container's process.
func (a *containerAgent) signal(sig unix.Signal) bool {
	return a.send(ci.Message{Op: ci.OpSignal, Signal: int(sig)})
}

// resize sets the container's terminal size.
func (a *containerAgent) resize(rows, cols uint16) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows, a.cols = rows, cols
	if a.started {
		a.sendLocked(ci.Message{Op: ci.OpResize, Rows: rows, Cols: cols})
	}
}

// stop asks the process to exit (its stop signal, SIGTERM by default),
// then kills it (SIGKILL) after timeout. It reports whether the VM stopped.
func (a *containerAgent) stop(vm *vz.VirtualMachine, timeout time.Duration) bool {
	a.markStopping()
	waitStopped := func(d time.Duration) bool {
		for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if vm.State() == vz.VirtualMachineStateStopped {
				return true
			}
		}
		return false
	}
	if a.signal(a.stopSig) && waitStopped(timeout) {
		return true
	}
	return a.signal(unix.SIGKILL) && waitStopped(3*time.Second)
}

// markStopping notes that a stop was requested.
func (a *containerAgent) markStopping() {
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
}

// finish records an exit when the VM stopped without the init reporting
// one: killed (137) if a stop was requested, else 255. Calling it again
// does nothing.
func (a *containerAgent) finish() {
	a.mu.Lock()
	exited, stopping, finished := a.exited, a.stopping, a.finished
	a.finished = true
	if a.conn != nil {
		a.conn.Close()
	}
	a.mu.Unlock()
	if exited || finished {
		return
	}
	st := ExitStatus{Code: 255, Error: "the container VM stopped unexpectedly", FinishedAt: time.Now()}
	if stopping {
		st = ExitStatus{Code: 137, FinishedAt: time.Now()}
	}
	writeExit(a.name, st)
}

// removeContainer deletes a container's directory (`run -d --rm`).
func removeContainer(name string) {
	if dir, err := VMDir(name); err == nil && filepath.Base(dir) == name {
		os.RemoveAll(dir)
	}
}

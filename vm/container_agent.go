package vm

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/Code-Hex/vz/v4"
	"github.com/bubunyo/monolith"
	ci "github.com/bubunyo/monolith/containerinit"
	pb "github.com/bubunyo/monolith/containerinit/agentpb"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/unix"
)

// containerAgent is the daemon's side of monolith-init's Agent API (ttRPC
// over vsock, containerinit/agentpb): it starts the process, delivers
// signals and terminal sizes, and records the exit code.
type containerAgent struct {
	name    string
	spec    ci.Spec
	logger  *monolith.Logger
	stopSig unix.Signal

	mu       sync.Mutex
	ttc      *ttrpc.Client
	client   pb.AgentService // nil until connected
	attached bool            // a console client is attached
	hold     bool            // wait for a console client before starting
	started  bool
	exited   bool
	stopping bool // a stop was requested: a VM stop without exit is a kill
	finished bool
	rows     uint16
	cols     uint16
}

func newContainerAgent(name string, cfg *ContainerConfig, logger *monolith.Logger) *containerAgent {
	os.Remove(mustPath(VMExitPath(name))) // running again
	a := &containerAgent{name: name, spec: cfg.Spec, logger: logger, hold: cfg.Attach, stopSig: unix.SIGTERM}
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

// connect dials the init, which serves as soon as the container's disk is
// mounted, then starts the process when it may.
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
	ttc := ttrpc.NewClient(conn)
	a.mu.Lock()
	a.ttc, a.client = ttc, pb.NewAgentClient(ttc)
	a.maybeStartLocked()
	a.mu.Unlock()

	// Without a console client within 30s, start anyway.
	time.AfterFunc(30*time.Second, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hold = false
		a.maybeStartLocked()
	})
}

// consoleAttached is called when a console client attaches.
func (a *containerAgent) consoleAttached() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attached = true
	a.maybeStartLocked()
}

func (a *containerAgent) maybeStartLocked() {
	if a.started || a.client == nil || (a.hold && !a.attached) {
		return
	}
	a.started = true
	go a.run(a.client, a.rows, a.cols)
}

// run starts the process, waits for it to exit, records the exit and lets
// the init power off.
func (a *containerAgent) run(client pb.AgentService, rows, cols uint16) {
	ctx := context.Background()
	if rows > 0 && cols > 0 {
		client.ResizeTTY(ctx, &pb.ResizeTTYRequest{Rows: uint32(rows), Cols: uint32(cols)})
	}
	resp, err := client.Start(ctx, &pb.StartRequest{Spec: &pb.Spec{
		Args:     a.spec.Args,
		Env:      a.spec.Env,
		Workdir:  a.spec.Workdir,
		User:     a.spec.User,
		Hostname: a.spec.Hostname,
		Tty:      a.spec.TTY,
	}})
	if err != nil {
		a.logger.Warn("container %q: start: %v", a.name, err)
		return
	}
	if resp.GetPid() > 0 {
		a.logger.Info("container %q process started (pid %d in the VM)", a.name, resp.GetPid())
	}

	exit, err := client.Wait(ctx, &pb.WaitRequest{})
	if err != nil {
		a.logger.Warn("container %q: wait: %v", a.name, err)
		return // the VM went away; finish records it
	}
	a.logger.Info("container %q exited with code %d", a.name, exit.GetExitCode())
	a.mu.Lock()
	a.exited = true
	a.mu.Unlock()
	if err := writeExit(a.name, ExitStatus{Code: int(exit.GetExitCode()), Error: exit.GetError(), FinishedAt: time.Now()}); err != nil {
		a.logger.Warn("recording exit code: %v", err)
	}
	sctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	client.Shutdown(sctx, &pb.Empty{})
}

// signal delivers sig to the container's process.
func (a *containerAgent) signal(sig unix.Signal) bool {
	a.mu.Lock()
	client, started := a.client, a.started
	a.mu.Unlock()
	if client == nil || !started {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := client.Signal(ctx, &pb.SignalRequest{Signal: uint32(sig)})
	return err == nil
}

// resize sets the container's terminal size.
func (a *containerAgent) resize(rows, cols uint16) {
	a.mu.Lock()
	a.rows, a.cols = rows, cols
	client, started := a.client, a.started
	a.mu.Unlock()
	if client != nil && started {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		client.ResizeTTY(ctx, &pb.ResizeTTYRequest{Rows: uint32(rows), Cols: uint32(cols)})
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
	if a.ttc != nil {
		a.ttc.Close()
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

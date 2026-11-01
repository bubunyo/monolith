package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	ci "github.com/bubunyo/monolith/containerinit"
	pb "github.com/bubunyo/monolith/containerinit/agentpb"
	"github.com/containerd/ttrpc"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// agent implements the Agent ttRPC service (containerinit/agentpb).
type agent struct {
	setupErr error  // the container couldn't be set up; Start reports it
	pnp      string // DHCP results, for resolv.conf

	mu      sync.Mutex
	started bool
	pid     int

	exited   chan struct{} // closed when the process has exited
	code     int
	errMsg   string
	shutdown chan struct{} // closed by Shutdown
	once     sync.Once
}

func newAgent() *agent {
	return &agent{exited: make(chan struct{}), shutdown: make(chan struct{})}
}

// serve serves the API on vsock in the background.
func (a *agent) serve() error {
	l, err := vsock.Listen(ci.VsockPort, nil)
	if err != nil {
		return err
	}
	srv, err := ttrpc.NewServer()
	if err != nil {
		return err
	}
	pb.RegisterAgentService(srv, a)
	go func() {
		if err := srv.Serve(context.Background(), l); err != nil && !errors.Is(err, ttrpc.ErrServerClosed) {
			logf("agent API: %v", err)
		}
	}()
	return nil
}

// finish records how the process ended.
func (a *agent) finish(code int, msg string) {
	a.code, a.errMsg = code, msg
	close(a.exited)
}

func (a *agent) Start(ctx context.Context, req *pb.StartRequest) (*pb.StartResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return nil, errors.New("the process was already started")
	}
	a.started = true
	if a.setupErr != nil {
		a.finish(ci.ExitInitError, a.setupErr.Error())
		return &pb.StartResponse{}, nil
	}
	spec := req.GetSpec()
	pid, code, err := startProcess(ci.Spec{
		Args:     spec.GetArgs(),
		Env:      spec.GetEnv(),
		Workdir:  spec.GetWorkdir(),
		User:     spec.GetUser(),
		Hostname: spec.GetHostname(),
		TTY:      spec.GetTty(),
	}, a.pnp)
	if err != nil {
		a.finish(code, err.Error())
		return &pb.StartResponse{}, nil
	}
	a.pid = pid
	go func() { a.finish(reap(pid), "") }()
	return &pb.StartResponse{Pid: uint32(pid)}, nil
}

func (a *agent) Wait(ctx context.Context, _ *pb.WaitRequest) (*pb.WaitResponse, error) {
	select {
	case <-a.exited:
		return &pb.WaitResponse{ExitCode: int32(a.code), Error: a.errMsg}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *agent) Signal(ctx context.Context, req *pb.SignalRequest) (*pb.Empty, error) {
	a.mu.Lock()
	pid := a.pid
	a.mu.Unlock()
	if pid == 0 {
		return nil, errors.New("no process is running")
	}
	if err := unix.Kill(pid, unix.Signal(req.GetSignal())); err != nil {
		return nil, err
	}
	return &pb.Empty{}, nil
}

func (a *agent) ResizeTTY(ctx context.Context, req *pb.ResizeTTYRequest) (*pb.Empty, error) {
	setWinsize(uint16(req.GetRows()), uint16(req.GetCols()))
	return &pb.Empty{}, nil
}

func (a *agent) Shutdown(ctx context.Context, _ *pb.Empty) (*pb.Empty, error) {
	a.once.Do(func() { close(a.shutdown) })
	return &pb.Empty{}, nil
}

// startProcess runs the container's process on the console. On failure it
// returns the exit code to report (Docker's 125, 126, 127).
func startProcess(spec ci.Spec, pnp string) (pid, code int, err error) {
	writeNetworkFiles(spec.Hostname, pnp)

	cred, err := ci.ResolveUser("/", spec.User)
	if err != nil {
		return 0, ci.ExitInitError, err
	}
	env := ci.ProcessEnv(spec, cred.Home)
	dir := spec.Workdir
	if dir == "" {
		dir = "/"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return 0, ci.ExitInitError, fmt.Errorf("working directory %s: %v", dir, err)
	}
	if len(spec.Args) == 0 {
		return 0, ci.ExitInitError, errors.New("no command specified")
	}
	pathEnv, _ := ci.Getenv(env, "PATH")
	exe, failCode, err := ci.LookPath("/", dir, spec.Args[0], pathEnv)
	if err != nil {
		return 0, failCode, err
	}

	tty, err := os.OpenFile(console, os.O_RDWR, 0)
	if err != nil {
		return 0, ci.ExitInitError, fmt.Errorf("opening %s: %v", console, err)
	}
	defer tty.Close()
	stdin := tty
	if spec.TTY {
		setTerminal(tty, true)
	} else {
		// Plain output, exactly as written: no CR/LF translation or echo.
		setTerminal(tty, false)
		if stdin, err = os.Open(os.DevNull); err != nil {
			return 0, ci.ExitInitError, err
		}
		defer stdin.Close()
	}

	// Docker's open-file limit; the kernel's default (1024:4096) is too
	// low for many servers. The process inherits it.
	nofile := unix.Rlimit{Cur: 1 << 20, Max: 1 << 20}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &nofile); err != nil {
		logf("raising the open-file limit: %v", err)
	}

	pid, err = syscall.ForkExec(exe, spec.Args, &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []uintptr{stdin.Fd(), tty.Fd(), tty.Fd()},
		Sys: &syscall.SysProcAttr{
			Setsid:     true,
			Setctty:    spec.TTY,
			Ctty:       0,
			Credential: &syscall.Credential{Uid: cred.UID, Gid: cred.GID, Groups: cred.Groups},
		},
	})
	if err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC) {
			return 0, ci.ExitNotExecutable, fmt.Errorf("exec %s: %v", spec.Args[0], err)
		}
		return 0, ci.ExitInitError, fmt.Errorf("exec %s: %v", spec.Args[0], err)
	}
	return pid, 0, nil
}

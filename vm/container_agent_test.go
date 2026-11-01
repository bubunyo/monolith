package vm

import (
	"context"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bubunyo/monolith"
	ci "github.com/bubunyo/monolith/containerinit"
	pb "github.com/bubunyo/monolith/containerinit/agentpb"
	"github.com/containerd/ttrpc"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// fakeInit records the calls the daemon's agent makes.
type fakeInit struct {
	mu      sync.Mutex
	calls   []string
	spec    *pb.Spec
	signals []uint32
	exit    chan int32
}

func (f *fakeInit) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeInit) Start(ctx context.Context, req *pb.StartRequest) (*pb.StartResponse, error) {
	f.record("start")
	f.mu.Lock()
	f.spec = req.GetSpec()
	f.mu.Unlock()
	return &pb.StartResponse{Pid: 42}, nil
}

func (f *fakeInit) Wait(ctx context.Context, _ *pb.WaitRequest) (*pb.WaitResponse, error) {
	f.record("wait")
	code := <-f.exit
	return &pb.WaitResponse{ExitCode: code, Error: "boom"}, nil
}

func (f *fakeInit) Signal(ctx context.Context, req *pb.SignalRequest) (*pb.Empty, error) {
	f.mu.Lock()
	f.signals = append(f.signals, req.GetSignal())
	f.mu.Unlock()
	return &pb.Empty{}, nil
}

func (f *fakeInit) ResizeTTY(ctx context.Context, req *pb.ResizeTTYRequest) (*pb.Empty, error) {
	f.record("resize")
	return &pb.Empty{}, nil
}

func (f *fakeInit) Shutdown(ctx context.Context, _ *pb.Empty) (*pb.Empty, error) {
	f.record("shutdown")
	return &pb.Empty{}, nil
}

// singleConnListener hands out one connection.
type singleConnListener struct {
	conn   net.Conn
	once   sync.Once
	closed sync.Once
	done   chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleConnListener) Close() error {
	l.closed.Do(func() { close(l.done) })
	return nil
}
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func TestContainerAgentProtocol(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const name = "agent-test"
	dir, err := VMDir(name)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0755))

	init := &fakeInit{exit: make(chan int32, 1)}
	server, err := ttrpc.NewServer()
	require.NoError(t, err)
	pb.RegisterAgentService(server, init)
	daemonSide, initSide := net.Pipe()
	l := &singleConnListener{conn: initSide, done: make(chan struct{})}
	go server.Serve(context.Background(), l)
	defer server.Close()

	spec := ci.Spec{Args: []string{"sh", "-c", "exit 7"}, Env: []string{"A=1"}, Workdir: "/w", User: "app", Hostname: "c1", TTY: true}
	a := newContainerAgent(name, &ContainerConfig{Spec: spec, StopSignal: "SIGQUIT"}, monolith.NewLogger(os.Stderr))
	require.Equal(t, unix.Signal(3), a.stopSig)
	client := pb.NewAgentClient(ttrpc.NewClient(daemonSide))
	a.mu.Lock()
	a.client, a.started = client, true
	a.mu.Unlock()

	done := make(chan struct{})
	go func() {
		a.run(client, 24, 80)
		close(done)
	}()
	require.Eventually(t, func() bool {
		init.mu.Lock()
		defer init.mu.Unlock()
		return len(init.calls) >= 3
	}, 5*time.Second, 10*time.Millisecond)
	require.True(t, a.signal(unix.SIGTERM))
	init.exit <- 7
	<-done

	require.Equal(t, []string{"resize", "start", "wait", "shutdown"}, init.calls)
	require.Equal(t, spec.Args, init.spec.GetArgs())
	require.Equal(t, "app", init.spec.GetUser())
	require.True(t, init.spec.GetTty())
	require.Equal(t, []uint32{15}, init.signals)
	st, ok := ReadExit(name)
	require.True(t, ok)
	require.Equal(t, 7, st.Code)
	require.Equal(t, "boom", st.Error)

	a.finish() // already exited: keeps the recorded code
	st, _ = ReadExit(name)
	require.Equal(t, 7, st.Code)
}

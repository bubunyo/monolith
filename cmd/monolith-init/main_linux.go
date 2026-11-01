// monolith-init is PID 1 of a container VM (`monolith container`). It runs
// from the initrd: it mounts the image disk (/dev/vda) and moves into it,
// waits for the daemon on vsock, runs the container's process, reaps
// orphans, forwards signals, reports the exit code and powers off.
//
// A crash in PID 1 panics the kernel, so every failure is reported as exit
// code 125 and followed by a normal power-off.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	ci "github.com/bubunyo/monolith/containerinit"
	"golang.org/x/sys/unix"
)

const (
	newRoot = "/newroot"
	console = "/dev/hvc0"
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "monolith-init: "+format+"\n", args...)
}

func main() {
	// PID 1 has no default signal actions; take SIGTERM/SIGINT ourselves
	// (handlers, unlike ignored signals, are reset for the child on exec).
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	var agent *agent
	code, msg := run(&agent)
	if msg != "" {
		logf("%s", msg)
	}
	shutdown()
	if agent != nil {
		agent.send(ci.Message{Op: ci.OpExit, Code: code, Error: msg})
		agent.waitAck(time.Second)
	}
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	for {
		time.Sleep(time.Hour)
	}
}

// run sets up the container and runs its process. It returns the exit code
// and, when the process couldn't run, why.
func run(agentOut **agent) (code int, msg string) {
	defer func() {
		if r := recover(); r != nil {
			code, msg = ci.ExitInitError, fmt.Sprint(r)
		}
	}()

	spec, err := setup()
	if err != nil {
		return ci.ExitInitError, err.Error()
	}

	a := listen()
	*agentOut = a
	if a != nil {
		a.send(ci.Message{Op: ci.OpReady})
		a.waitStart()
	}

	cred, err := ci.ResolveUser("/", spec.User)
	if err != nil {
		return ci.ExitInitError, err.Error()
	}
	env := ci.ProcessEnv(spec, cred.Home)
	dir := spec.Workdir
	if dir == "" {
		dir = "/"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return ci.ExitInitError, fmt.Sprintf("working directory %s: %v", dir, err)
	}
	if len(spec.Args) == 0 {
		return ci.ExitInitError, "no command specified"
	}
	pathEnv, _ := ci.Getenv(env, "PATH")
	exe, failCode, err := ci.LookPath("/", dir, spec.Args[0], pathEnv)
	if err != nil {
		return failCode, err.Error()
	}

	tty, err := os.OpenFile(console, os.O_RDWR, 0)
	if err != nil {
		return ci.ExitInitError, fmt.Sprintf("opening %s: %v", console, err)
	}
	defer tty.Close()
	stdin := tty
	if spec.TTY {
		setTerminal(tty, true)
	} else {
		// Plain output, exactly as written: no CR/LF translation or echo.
		setTerminal(tty, false)
		if stdin, err = os.Open(os.DevNull); err != nil {
			return ci.ExitInitError, err.Error()
		}
		defer stdin.Close()
	}

	// Docker's open-file limit; the kernel's default (1024:4096) is too
	// low for many servers. The process inherits it.
	nofile := unix.Rlimit{Cur: 1 << 20, Max: 1 << 20}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &nofile); err != nil {
		logf("raising the open-file limit: %v", err)
	}

	attr := &syscall.ProcAttr{
		Dir:   dir,
		Env:   env,
		Files: []uintptr{stdin.Fd(), tty.Fd(), tty.Fd()},
		Sys: &syscall.SysProcAttr{
			Setsid:     true,
			Setctty:    spec.TTY,
			Ctty:       0,
			Credential: &syscall.Credential{Uid: cred.UID, Gid: cred.GID, Groups: cred.Groups},
		},
	}
	pid, err := syscall.ForkExec(exe, spec.Args, attr)
	if err != nil {
		if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.ENOEXEC) {
			return ci.ExitNotExecutable, fmt.Sprintf("exec %s: %v", spec.Args[0], err)
		}
		return ci.ExitInitError, fmt.Sprintf("exec %s: %v", spec.Args[0], err)
	}
	if a != nil {
		a.send(ci.Message{Op: ci.OpStarted, PID: pid})
		go a.forwardSignals(pid)
	}
	return reap(pid), ""
}

// setup mounts everything and moves into the image's filesystem.
func setup() (ci.Spec, error) {
	var spec ci.Spec
	data, err := os.ReadFile(ci.SpecPath)
	if err != nil {
		return spec, fmt.Errorf("reading spec: %w", err)
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("parsing spec: %w", err)
	}

	os.MkdirAll("/dev", 0755)
	os.MkdirAll("/proc", 0755)
	if err := unix.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return spec, fmt.Errorf("mounting /dev: %w", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return spec, fmt.Errorf("mounting /proc: %w", err)
	}
	pnp, _ := os.ReadFile("/proc/net/pnp") // DHCP results from ip=dhcp

	if err := os.MkdirAll(newRoot, 0755); err != nil {
		return spec, err
	}
	if err := unix.Mount("/dev/vda", newRoot, "ext4", 0, ""); err != nil {
		return spec, fmt.Errorf("mounting the image disk: %w", err)
	}
	for _, m := range []struct {
		src, dst, fstype, data string
		flags                  uintptr
	}{
		{"proc", "proc", "proc", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"sysfs", "sys", "sysfs", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"devtmpfs", "dev", "devtmpfs", "", unix.MS_NOSUID},
		{"devpts", "dev/pts", "devpts", "newinstance,ptmxmode=0666,mode=0620", unix.MS_NOSUID | unix.MS_NOEXEC},
		{"shm", "dev/shm", "tmpfs", "mode=1777", unix.MS_NOSUID | unix.MS_NODEV},
		{"mqueue", "dev/mqueue", "mqueue", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
		{"cgroup2", "sys/fs/cgroup", "cgroup2", "", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC},
	} {
		dst := filepath.Join(newRoot, m.dst)
		os.MkdirAll(dst, 0755)
		if err := unix.Mount(m.src, dst, m.fstype, m.flags, m.data); err != nil {
			logf("mounting /%s: %v", m.dst, err)
		}
	}
	if err := growRoot(newRoot); err != nil {
		logf("growing the root filesystem: %v", err)
	}

	// The links runc creates in every container's /dev (devtmpfs has none):
	// images log to /dev/stdout and /dev/stderr. /dev/ptmx goes through the
	// container's devpts instance.
	for link, target := range map[string]string{
		"dev/fd":     "/proc/self/fd",
		"dev/stdin":  "/proc/self/fd/0",
		"dev/stdout": "/proc/self/fd/1",
		"dev/stderr": "/proc/self/fd/2",
		"dev/core":   "/proc/kcore",
		"dev/ptmx":   "pts/ptmx",
	} {
		p := filepath.Join(newRoot, link)
		os.Remove(p)
		if err := os.Symlink(target, p); err != nil {
			logf("linking /%s: %v", link, err)
		}
	}

	writeNetworkFiles(spec.Hostname, string(pnp))

	if err := os.Chdir(newRoot); err != nil {
		return spec, err
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return spec, fmt.Errorf("moving the root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return spec, fmt.Errorf("chroot: %w", err)
	}
	return spec, os.Chdir("/")
}

// writeNetworkFiles writes hostname, hosts and resolv.conf, as Docker
// does for every container.
func writeNetworkFiles(hostname, pnp string) {
	etc := filepath.Join(newRoot, "etc")
	os.MkdirAll(etc, 0755)
	if hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			logf("hostname: %v", err)
		}
		replaceFile(filepath.Join(etc, "hostname"), hostname+"\n")
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if ip := eth0Address(); ip != "" && hostname != "" {
		hosts += ip + "\t" + hostname + "\n"
	}
	replaceFile(filepath.Join(etc, "hosts"), hosts)
	if conf := ci.ResolvConf(pnp); conf != "" {
		replaceFile(filepath.Join(etc, "resolv.conf"), conf)
	}
}

// replaceFile writes a regular file at p, replacing a symlink there (e.g.
// resolv.conf pointing into /run) rather than following it.
func replaceFile(p, data string) {
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		os.Remove(p)
	}
	if err := os.WriteFile(p, []byte(data), 0644); err != nil {
		logf("writing %s: %v", strings.TrimPrefix(p, newRoot), err)
	}
}

func eth0Address() string {
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		return ""
	}
	addrs, _ := iface.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return ""
}

// setTerminal configures the console: a normal terminal for -t, otherwise
// raw so output passes through unchanged.
func setTerminal(f *os.File, interactive bool) {
	t, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		return
	}
	if interactive {
		t.Iflag |= unix.ICRNL | unix.IXON
		t.Oflag |= unix.OPOST | unix.ONLCR
		t.Lflag |= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ISIG | unix.IEXTEN
	} else {
		t.Iflag &^= unix.ICRNL | unix.IXON
		t.Oflag &^= unix.OPOST
		t.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ECHOK | unix.ISIG | unix.IEXTEN
	}
	unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, t)
}

// setWinsize sets the console's size; the kernel sends SIGWINCH to the
// process on it.
func setWinsize(rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	f, err := os.OpenFile(console, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// reap waits for every child until the main process exits and returns its
// exit code (128+signal if killed).
func reap(main int) int {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			logf("wait: %v", err)
			return ci.ExitInitError
		}
		if pid != main {
			continue
		}
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
}

// shutdown ends whatever the process left behind and makes the root
// filesystem clean, so the next boot has no journal to replay.
func shutdown() {
	unix.Kill(-1, unix.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	unix.Kill(-1, unix.SIGKILL)
	for {
		if _, err := unix.Wait4(-1, nil, unix.WNOHANG, nil); err != nil {
			break
		}
	}
	unix.Sync()
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		logf("remounting / read-only: %v", err)
	}
}

// agent is the vsock connection to the daemon.
type agent struct {
	conn    *os.File
	in      *bufio.Scanner
	signals chan int
	start   chan struct{}
	ack     chan struct{}
}

// listen waits up to 10s for the daemon to connect. Without it the process
// still runs; only the exit code doesn't reach the Mac.
func listen() *agent {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		logf("vsock: %v", err)
		return nil
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: ci.VsockPort}); err != nil {
		logf("vsock bind: %v", err)
		return nil
	}
	if err := unix.Listen(fd, 1); err != nil {
		logf("vsock listen: %v", err)
		return nil
	}
	accepted := make(chan int, 1)
	go func() {
		nfd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
		if err != nil {
			logf("vsock accept: %v", err)
			return
		}
		accepted <- nfd
	}()
	select {
	case nfd := <-accepted:
		conn := os.NewFile(uintptr(nfd), "vsock")
		a := &agent{
			conn:    conn,
			in:      bufio.NewScanner(conn),
			signals: make(chan int, 8),
			start:   make(chan struct{}),
			ack:     make(chan struct{}),
		}
		go a.read()
		return a
	case <-time.After(10 * time.Second):
		logf("the daemon didn't connect; running without it")
		return nil
	}
}

func (a *agent) send(m ci.Message) {
	data, _ := json.Marshal(m)
	a.conn.Write(append(data, '\n'))
}

func (a *agent) read() {
	started, acked := false, false
	for a.in.Scan() {
		var m ci.Message
		if json.Unmarshal(a.in.Bytes(), &m) != nil {
			continue
		}
		switch m.Op {
		case ci.OpStart:
			if !started {
				started = true
				close(a.start)
			}
		case ci.OpSignal:
			select {
			case a.signals <- m.Signal:
			default:
			}
		case ci.OpResize:
			setWinsize(m.Rows, m.Cols)
		case ci.OpAck:
			if !acked {
				acked = true
				close(a.ack)
			}
		}
	}
	// The daemon went away: don't wait for it.
	if !started {
		close(a.start)
	}
	if !acked {
		close(a.ack)
	}
}

func (a *agent) waitStart() { <-a.start }

func (a *agent) waitAck(timeout time.Duration) {
	select {
	case <-a.ack:
	case <-time.After(timeout):
		logf("the daemon didn't acknowledge the exit")
	}
}

// forwardSignals delivers the daemon's signals (stop, kill, Ctrl+C) to the
// process.
func (a *agent) forwardSignals(pid int) {
	for sig := range a.signals {
		unix.Kill(pid, unix.Signal(sig))
	}
}

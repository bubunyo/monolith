// monolith-init is PID 1 of a container VM (`monolith container`). It runs
// from the initrd: it mounts the image disk (/dev/vda) and moves into it,
// then serves the Agent ttRPC API (containerinit/agentpb) on vsock for the
// VM daemon, which starts the container's process with it. The init reaps
// orphans, forwards signals, reports the exit code and powers off.
//
// A crash in PID 1 panics the kernel, so every failure is reported as exit
// code 125 and followed by a normal power-off.
package main

import (
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

	a := newAgent()
	pnp, err := setup()
	if err != nil {
		// Still serve: the daemon learns why from Wait.
		a.setupErr = err
	}
	a.pnp = pnp
	if err := a.serve(); err != nil {
		logf("serving the agent API: %v", err)
		powerOff()
	}

	<-a.exited
	if a.errMsg != "" {
		logf("%s", a.errMsg)
	}
	shutdown()
	// Wait for the daemon's Shutdown, which it sends once it has the exit
	// from Wait, so that reply isn't lost with the VM.
	select {
	case <-a.shutdown:
	case <-time.After(2 * time.Second):
		logf("the daemon didn't ask for shutdown")
	}
	powerOff()
}

func powerOff() {
	unix.Sync()
	unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	for {
		time.Sleep(time.Hour)
	}
}

// setup mounts everything and moves into the image's filesystem. It
// returns what the kernel learned by DHCP (/proc/net/pnp), for resolv.conf.
func setup() (pnp string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	os.MkdirAll("/dev", 0755)
	os.MkdirAll("/proc", 0755)
	if err := unix.Mount("devtmpfs", "/dev", "devtmpfs", 0, ""); err != nil && !errors.Is(err, unix.EBUSY) {
		return "", fmt.Errorf("mounting /dev: %w", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return "", fmt.Errorf("mounting /proc: %w", err)
	}
	data, _ := os.ReadFile("/proc/net/pnp") // DHCP results from ip=dhcp

	if err := os.MkdirAll(newRoot, 0755); err != nil {
		return "", err
	}
	if err := unix.Mount("/dev/vda", newRoot, "ext4", 0, ""); err != nil {
		return "", fmt.Errorf("mounting the image disk: %w", err)
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

	if err := os.Chdir(newRoot); err != nil {
		return "", err
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return "", fmt.Errorf("moving the root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return "", fmt.Errorf("chroot: %w", err)
	}
	return string(data), os.Chdir("/")
}

// writeNetworkFiles writes hostname, hosts and resolv.conf, as Docker
// does for every container.
func writeNetworkFiles(hostname, pnp string) {
	os.MkdirAll("/etc", 0755)
	if hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			logf("hostname: %v", err)
		}
		replaceFile("/etc/hostname", hostname+"\n")
	}
	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"
	if ip := eth0Address(); ip != "" && hostname != "" {
		hosts += ip + "\t" + hostname + "\n"
	}
	replaceFile("/etc/hosts", hosts)
	if conf := ci.ResolvConf(pnp); conf != "" {
		replaceFile("/etc/resolv.conf", conf)
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

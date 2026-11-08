package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bubunyo/monolith/internal/ansi"
	"github.com/bubunyo/monolith/internal/steps"
	"github.com/bubunyo/monolith/vm"
)

// dockerReadyTimeout bounds the wait for dockerd after the guest's last
// boot step; the engine's install (first boot) happens before that.
const dockerReadyTimeout = 2 * time.Minute

// errDetached reports that the user pressed Ctrl+C while the VM booted.
var errDetached = errors.New("detached")

// newStepView starts a step view on stdout.
func newStepView(titles ...string) *steps.View {
	tty := ansi.IsTerminal(os.Stdout)
	return steps.New(os.Stdout, steps.Options{TTY: tty, Color: ansi.UseColor(tty), Verbose: flagVerbose}, titles...)
}

// bootVM prepares the image, starts the daemon and follows the guest's
// boot as steps, then attaches the console.
func bootVM(name string, image vm.Image, logConsole string) error {
	v := newStepView("Image", "Start")

	// Image: downloads and disk creation, if any.
	st := v.Step("Image").Start()
	logged := &countingWriter{w: st}
	if _, err := image.Prepare(name, logged); err != nil {
		st.Fail(err)
		v.Close()
		return fmt.Errorf("preparing the %s image: %w", image.Name(), err)
	}
	summary := image.Name()
	if logged.n == 0 {
		summary += " (cached)"
	}
	st.Done(summary)

	// Prepare records the guest layout of a disk it creates, which decides
	// the boot steps.
	cfg, err := vm.ReadConfig(name)
	if err != nil {
		v.Close()
		return err
	}
	bootSteps := vm.BootSteps(cfg)
	for _, s := range bootSteps {
		v.Step(s.Title)
	}

	// Start: the daemon configures and starts the VM; its log is the step's.
	st = v.Step("Start").Start()
	if err := followDaemonLog(name, st, func() error { return vm.SpawnVMDaemon(name, false, logConsole) }); err != nil {
		st.Fail(err)
		v.Close()
		return fmt.Errorf("VM %q did not start: %w", name, err)
	}
	st.Done(fmt.Sprintf("%d vCPU, %s RAM, network: %s", cfg.CPUs, FormatMemory(cfg.Memory), cfg.NetworkMode))

	conn, rest, err := watchGuest(name, cfg, v, bootSteps)
	v.Close()
	if errors.Is(err, errDetached) {
		fmt.Printf("Detached; %s is still booting. Attach with: monolith vm attach %s\n", name, name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("VM %q failed to boot; see 'monolith vm logs %s'", name, name)
	}
	return relayConsole(name, conn, rest)
}

// restoreVM restores a suspended VM as one step, then attaches.
func restoreVM(name, logConsole string) error {
	v := newStepView("Restore")
	st := v.Step("Restore").Start()
	err := followDaemonLog(name, st, func() error { return vm.SpawnVMDaemon(name, true, logConsole) })
	if err != nil {
		st.Fail(err)
		v.Close()
		return fmt.Errorf("VM %q was not restored: %w", name, err)
	}
	st.Done("from the suspend file")
	v.Close()
	return attachAndHandle(name)
}

// relayConsole hands the console connection to the terminal and runs any
// command chosen at the escape prompt.
func relayConsole(name string, conn net.Conn, pending []byte) error {
	escapeCmd, err := vm.RelayConsole(name, conn, pending)
	if err != nil {
		return err
	}
	if escapeCmd != nil {
		return executeEscapeCommand(name, *escapeCmd)
	}
	fmt.Printf("Detached from %s. VM is still running.\n", name)
	return nil
}

// watchGuest attaches to the console and follows the guest's boot steps
// through it. It returns the connection and the output read past the last
// step, for the console relay. Ctrl+C detaches (errDetached).
func watchGuest(name string, cfg vm.VMConfig, v *steps.View, bootSteps []vm.BootStep) (net.Conn, []byte, error) {
	if len(bootSteps) == 0 {
		conn, err := vm.DialConsole(name)
		return conn, nil, err
	}
	current := v.Step(bootSteps[0].Title).Start()
	conn, err := vm.DialConsole(name)
	if err != nil {
		current.Fail(err)
		return nil, nil, err
	}

	// Ctrl+C (the terminal isn't raw yet) detaches: closing the connection
	// ends WatchBoot, and cancel ends the wait for dockerd.
	var detached atomic.Bool
	cancel := make(chan struct{})
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	stop := make(chan struct{})
	defer func() {
		signal.Stop(sigs)
		close(stop)
	}()
	go func() {
		select {
		case <-sigs:
			detached.Store(true)
			conn.Close()
			close(cancel)
		case <-stop:
		}
	}()

	last := len(bootSteps) - 1
	waitEngine := bootSteps[last].Title == "Engine" && vm.EngineOf(cfg) == vm.EngineDocker
	summaries := make([]string, len(bootSteps))
	rest, err := vm.WatchBoot(conn, bootSteps,
		func(i int, text string) {
			fmt.Fprintln(v.Step(bootSteps[i].Title), text)
			if m := leaseRE.FindStringSubmatch(ansi.Plain(text)); m != nil {
				summaries[i] = m[1]
			}
		},
		func(i int) {
			if i == last && waitEngine {
				return // done once dockerd answers
			}
			v.Step(bootSteps[i].Title).Done(summaries[i])
			if i < last {
				current = v.Step(bootSteps[i+1].Title).Start()
			}
		})
	if err == nil && waitEngine {
		current.Logf("waiting for dockerd")
		if err = waitDockerd(name, cancel); err == nil {
			current.Done("docker")
		}
	}
	if detached.Load() {
		return nil, nil, errDetached
	}
	if err != nil {
		if stoppedSoon(name) {
			err = errors.New("the VM stopped")
		}
		current.Fail(err)
		conn.Close()
		return nil, nil, err
	}
	return conn, rest, nil
}

// stoppedSoon reports whether the VM is stopped, giving the daemon, whose
// exit closed the console, a moment to record it.
func stoppedSoon(name string) bool {
	for range 20 {
		if status, err := vm.ReadState(name); err == nil && status.State == vm.StatusStopped {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// leaseRE finds the address in udhcpc's lease line, the Network step's
// summary.
var leaseRE = regexp.MustCompile(`lease of (\S+) obtained`)

// waitDockerd waits until dockerd answers on the VM's docker.sock.
func waitDockerd(name string, cancel <-chan struct{}) error {
	sock, err := vm.VMDockerSockPath(name)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
	}
	deadline := time.Now().Add(dockerReadyTimeout)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://docker/_ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if status, err := vm.ReadState(name); err == nil && status.State == vm.StatusStopped {
			return errors.New("the VM stopped")
		}
		select {
		case <-cancel:
			return errDetached
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("dockerd did not answer within %s", dockerReadyTimeout)
}

// followDaemonLog runs start (which spawns the VM daemon) while copying
// what the daemon appends to its log into w.
func followDaemonLog(name string, w io.Writer, start func() error) error {
	path, err := vm.VMLogPath(name)
	if err != nil {
		return err
	}
	var offset int64
	if fi, err := os.Stat(path); err == nil {
		offset = fi.Size()
	}
	copyNew := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		n, _ := io.Copy(w, f)
		offset += n
	}

	done := make(chan error, 1)
	go func() { done <- start() }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			copyNew()
			return err
		case <-tick.C:
			copyNew()
		}
	}
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return c.w.Write(p)
}

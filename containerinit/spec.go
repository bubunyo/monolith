// Package containerinit is shared by monolith-init, the PID 1 of container
// VMs (cmd/monolith-init), and the daemon that runs them: the spec the init
// reads from its initrd, the messages they exchange over vsock, and the
// logic that turns an image's config into a process (user, environment,
// PATH lookup). It has no Linux-only code, so it's tested on the Mac.
package containerinit

import (
	"fmt"
	"strconv"
	"strings"
)

// SpecPath is where the spec sits in the initrd.
const SpecPath = "/spec.json"

// VsockPort is the port the init listens on for the daemon.
const VsockPort = 1025

// Spec describes the container's process.
type Spec struct {
	Args     []string `json:"args"`              // command and arguments; Args[0] is looked up in PATH
	Env      []string `json:"env,omitempty"`     // KEY=VALUE, image env then user overrides
	Workdir  string   `json:"workdir,omitempty"` // default "/"
	User     string   `json:"user,omitempty"`    // "", "name", "uid", "name:group" or "uid:gid"
	Hostname string   `json:"hostname,omitempty"`
	TTY      bool     `json:"tty,omitempty"` // run on a terminal (the console), stdin attached
}

// Message is one line of JSON on the vsock connection between the daemon
// and the init. The daemon connects; the init waits for "start" before
// running the process, so no output or exit is missed, and waits for "ack"
// after "exit" before powering off, so the exit code isn't lost.
type Message struct {
	Op     string `json:"op"`
	PID    int    `json:"pid,omitempty"`    // started
	Code   int    `json:"code,omitempty"`   // exit
	Signal int    `json:"signal,omitempty"` // signal
	Rows   uint16 `json:"rows,omitempty"`   // resize
	Cols   uint16 `json:"cols,omitempty"`   // resize
	Error  string `json:"error,omitempty"`  // exit: why the process couldn't run
}

// Message ops. Daemon to init: start, signal, resize, ack. Init to daemon:
// ready, started, exit.
const (
	OpReady   = "ready"
	OpStart   = "start"
	OpStarted = "started"
	OpSignal  = "signal"
	OpResize  = "resize"
	OpExit    = "exit"
	OpAck     = "ack"
)

// Exit codes the init reports when the process can't run, as Docker does.
const (
	ExitInitError     = 125 // the init itself failed (mounts, bad spec)
	ExitNotExecutable = 126
	ExitNotFound      = 127
)

// DefaultPath is used when the image sets no PATH.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Command combines an image's entrypoint and cmd with the user's overrides
// the way `docker run` does: --entrypoint replaces the entrypoint and drops
// the image's cmd; arguments after the image replace the cmd.
func Command(entrypoint, cmd []string, overrideEntrypoint *string, args []string) []string {
	if overrideEntrypoint != nil {
		entrypoint = nil
		if *overrideEntrypoint != "" {
			entrypoint = []string{*overrideEntrypoint}
		}
		cmd = nil
	}
	if len(args) > 0 {
		cmd = args
	}
	return append(append([]string{}, entrypoint...), cmd...)
}

// MergeEnv appends overrides to base; a later KEY replaces an earlier one
// in place. An override without "=" (e.g. "-e FOO") is skipped here; the
// caller fills it from its own environment.
func MergeEnv(base []string, overrides ...string) []string {
	out := append([]string{}, base...)
	for _, kv := range overrides {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		replaced := false
		for i, e := range out {
			if k, _, _ := strings.Cut(e, "="); k == key {
				out[i] = kv
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

// Getenv returns KEY's value in env.
func Getenv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, _ := strings.Cut(env[i], "="); k == key {
			return v, true
		}
	}
	return "", false
}

// ProcessEnv is the environment the process gets: the spec's, plus PATH,
// HOSTNAME, HOME and (with a terminal) TERM unless already set.
func ProcessEnv(spec Spec, home string) []string {
	var defaults []string
	add := func(key, value string) {
		if _, ok := Getenv(spec.Env, key); !ok {
			defaults = append(defaults, key+"="+value)
		}
	}
	add("PATH", DefaultPath)
	if spec.Hostname != "" {
		add("HOSTNAME", spec.Hostname)
	}
	add("HOME", home)
	if spec.TTY {
		add("TERM", "xterm")
	}
	return append(defaults, spec.Env...)
}

// linuxSignals maps signal names to Linux's numbers (the Mac's differ for
// some, e.g. SIGUSR1).
var linuxSignals = map[string]int{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "BUS": 7, "FPE": 8,
	"KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15,
	"STKFLT": 16, "CHLD": 17, "CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22,
	"URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29,
	"PWR": 30, "SYS": 31,
}

// LinuxSignal parses a signal as images and `docker kill` give it
// ("SIGQUIT", "QUIT", "3") into its Linux number.
func LinuxSignal(s string) (int, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > 64 {
			return 0, fmt.Errorf("invalid signal %q", s)
		}
		return n, nil
	}
	if n, ok := linuxSignals[strings.TrimPrefix(s, "SIG")]; ok {
		return n, nil
	}
	return 0, fmt.Errorf("invalid signal %q", s)
}

// Package sandbox implements `monolith sandbox`: run a repo's commands in a
// lith, isolated from the Mac, the way a virtualenv scopes a project.
package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	clivm "github.com/bubunyo/monolith/cli/vm"
	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/vm"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

const (
	defaultImage  = "debian:stable-slim"
	helperImage   = "alpine:3.21" // runs firewall and layer commands in the VM
	shellName     = "monolith-sandbox"
	defaultCPUs   = 2
	defaultMemory = 2 << 30

	// upperDir is the overlay's writable layer in the VM (sandbox-mount).
	upperDir = "/var/lib/docker/.monolith/sandbox/upper"

	readyTimeout = 10 * time.Minute
)

// Cmd is the parent command for repo sandboxes.
var Cmd = &cobra.Command{
	Use:     "sandbox",
	Aliases: []string{"sbx"},
	Short:   "Run a repo's commands in an isolated lith",
	Long: `Run code you don't trust in a lith, scoped to one repo, like a virtualenv.

  cd some-repo
  monolith sandbox activate   # a shell inside the sandbox, at the repo
  npm install && npm test     # runs in the lith, not on your Mac
  deactivate                  # back on the Mac; the sandbox pauses to disk
  monolith sandbox diff       # what the code changed
  monolith sandbox apply PATH # copy chosen changes back to the Mac

Each repo (its git root, else the current directory) gets its own VM. Only the
repo is shared, read-only; writes land in a layer inside the VM, so your files
change only when you apply them. Nothing else from the Mac is visible: no
home directory, keys, environment or agent. By default the shell can reach the
internet but not your Mac or local network (--network). Commands run in a
container (--image, or the repo's .devcontainer, else ` + defaultImage + `).

The network policy limits where the code can connect, not what it sends: with
internet access, code can upload anything inside the sandbox, including the
repo. Use --network none or an allowlist for code you suspect.`,
}

var (
	flagPath    string
	flagImage   string
	flagNetwork string
	flagAllow   []string
	flagCPUs    uint
	flagMemory  string
	flagRosetta bool
	flagKeep    bool
	flagYes     bool
)

// sandbox is a resolved sandbox for a repo.
type sandbox struct {
	name string
	repo string
	sock string
}

// resolve finds the repo for the current directory (or --path): its git
// root if it's in one, else the directory itself. Symlinks are resolved so
// the share and the guest path match the real location.
func resolve() (*sandbox, error) {
	dir := flagPath
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	repo := dir
	// Look for .git by walking up; don't run git, since an untrusted repo's
	// .git/config can make it run commands. Without one, reuse a sandbox
	// that already exists for a parent directory, so running from a
	// subdirectory doesn't start a separate sandbox.
	if root, ok := walkUp(dir, func(d string) bool {
		_, err := os.Lstat(filepath.Join(d, ".git"))
		return err == nil
	}); ok {
		repo = root
	} else if root, ok := walkUp(dir, func(d string) bool {
		cfg, err := vm.ReadConfig(nameFor(d))
		return err == nil && vm.Exists(nameFor(d)) && cfg.Sandbox != nil && cfg.Sandbox.Repo == d
	}); ok {
		repo = root
	}
	if repo == "/" {
		return nil, fmt.Errorf("refusing to sandbox /")
	}
	if home, err := os.UserHomeDir(); err == nil && repo == home {
		return nil, fmt.Errorf("refusing to sandbox your home directory; cd into a repo")
	}
	name := nameFor(repo)
	sock, err := vm.VMDockerSockPath(name)
	if err != nil {
		return nil, err
	}
	return &sandbox{name: name, repo: repo, sock: sock}, nil
}

// walkUp returns the first of dir and its parents for which match is true.
func walkUp(dir string, match func(string) bool) (string, bool) {
	for d := dir; ; d = filepath.Dir(d) {
		if match(d) {
			return d, true
		}
		if d == filepath.Dir(d) {
			return "", false
		}
	}
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// nameFor is the VM name for a repo: sbx-<base>-<hash of the path>.
func nameFor(repo string) string {
	slug := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(filepath.Base(repo)), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "repo"
	}
	sum := sha256.Sum256([]byte(repo))
	return "sbx-" + slug + "-" + hex.EncodeToString(sum[:3])
}

// configure creates or updates the sandbox VM's config from the flags.
// It returns the config and whether the VM must be (re)booted for it.
func (s *sandbox) configure(cmd *cobra.Command) (vm.VMConfig, error) {
	if !validNetwork(flagNetwork) {
		return vm.VMConfig{}, fmt.Errorf("invalid --network %q; must be one of: %s", flagNetwork, strings.Join(vm.ValidSandboxNetworks, ", "))
	}
	if flagNetwork == vm.SandboxNetworkAllow && len(flagAllow) == 0 {
		return vm.VMConfig{}, fmt.Errorf("--network allow needs --allow host:port (repeatable)")
	}
	if !vm.Exists(s.name) {
		cfg := vm.DefaultConfig()
		cfg.CPUs = defaultCPUs
		cfg.Memory = defaultMemory
		cfg.Image = vm.ImageDocker
		cfg.Engine = vm.EngineDocker
		cfg.NetworkMode = mnet.NetworkUser
		cfg.Sandbox = &vm.SandboxConfig{Repo: s.repo}
		if err := applyFlags(cmd, &cfg); err != nil {
			return cfg, err
		}
		return cfg, vm.WriteConfig(s.name, cfg)
	}
	cfg, err := vm.ReadConfig(s.name)
	if err != nil {
		return cfg, err
	}
	if cfg.Sandbox == nil || cfg.Sandbox.Repo != s.repo {
		return cfg, fmt.Errorf("VM %q exists but isn't the sandbox for %s", s.name, s.repo)
	}
	// Rosetta is a device and a boot argument: only on a stopped VM.
	if cmd.Flags().Changed("rosetta") && flagRosetta != cfg.Rosetta {
		if st, err := vm.ReadState(s.name); err == nil && st.State != vm.StatusStopped {
			return cfg, fmt.Errorf("the sandbox is %s; 'monolith sandbox stop' it to change --rosetta", st.State)
		}
	}
	if err := applyFlags(cmd, &cfg); err != nil {
		return cfg, err
	}
	return cfg, vm.WriteConfig(s.name, cfg)
}

func applyFlags(cmd *cobra.Command, cfg *vm.VMConfig) error {
	f := cmd.Flags()
	if f.Changed("image") {
		cfg.Sandbox.Image = flagImage
	}
	if f.Changed("network") {
		cfg.Sandbox.Network = flagNetwork
	}
	if f.Changed("allow") {
		cfg.Sandbox.Allow = flagAllow
	}
	if f.Changed("cpus") {
		cfg.CPUs = flagCPUs
	}
	if f.Changed("memory") {
		mem, err := clivm.ParseMemory(flagMemory)
		if err != nil {
			return err
		}
		cfg.Memory = mem
	}
	if f.Changed("rosetta") {
		cfg.Rosetta = flagRosetta
	}
	return nil
}

func validNetwork(n string) bool {
	for _, v := range vm.ValidSandboxNetworks {
		if v == n {
			return true
		}
	}
	return false
}

// ensureRunning boots, restores or leaves the VM running and waits for
// dockerd. It reports whether the VM was already running.
func (s *sandbox) ensureRunning() (wasRunning bool, err error) {
	status, err := vm.ReadState(s.name)
	if err != nil {
		return false, err
	}
	switch status.State {
	case vm.StatusRunning:
		wasRunning = true
	case vm.StatusPaused:
		resp, err := vm.SendControlCommand(s.name, vm.ControlRequest{Cmd: vm.CmdResume})
		if err != nil {
			return false, err
		}
		if !resp.OK {
			return false, fmt.Errorf("%s", resp.Error)
		}
	case vm.StatusSuspended:
		fmt.Fprintf(os.Stderr, "Resuming sandbox %s...\n", s.name)
		if err := vm.SpawnVMDaemon(s.name, true, ""); err != nil {
			return false, err
		}
	default:
		image, err := vm.LookupImage(vm.ImageDocker)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(os.Stderr, "Starting sandbox %s for %s...\n", s.name, s.repo)
		if _, err := image.Prepare(s.name); err != nil {
			return false, fmt.Errorf("preparing the sandbox: %w", err)
		}
		if err := vm.SpawnVMDaemon(s.name, false, ""); err != nil {
			return false, err
		}
	}
	return wasRunning, s.waitDocker()
}

// waitDocker waits until the sandbox's dockerd answers.
func (s *sandbox) waitDocker() error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", s.sock)
			},
		},
	}
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://docker/_ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if status, err := vm.ReadState(s.name); err == nil && status.State == vm.StatusStopped {
			return fmt.Errorf("sandbox %s stopped while starting; see 'monolith vm logs %s'", s.name, s.name)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("the sandbox's Docker engine did not start within %s", readyTimeout)
}

// docker builds a docker CLI command against the sandbox's engine, never
// the user's current context.
func (s *sandbox) docker(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DOCKER_HOST=") && !strings.HasPrefix(kv, "DOCKER_CONTEXT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "DOCKER_HOST=unix://"+s.sock)
	return cmd
}

// dockerOutput runs docker and returns trimmed stdout, with stderr in the error.
func (s *sandbox) dockerOutput(args ...string) (string, error) {
	cmd := s.docker(args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// dockerRun runs docker with the terminal attached (pull/build progress).
func (s *sandbox) dockerRun(args ...string) error {
	cmd := s.docker(args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// ensureImage makes sure img exists in the VM, pulling it if needed.
func (s *sandbox) ensureImage(img string) error {
	if _, err := s.dockerOutput("image", "inspect", "--format", "{{.Id}}", img); err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "Pulling %s...\n", img)
	return s.dockerRun("pull", "--quiet", img)
}

// helper runs a shell script as root in the VM itself (not a container),
// through a privileged helper container that enters PID 1's namespaces.
// It also shares the VM's cgroup namespace: iptables' cgroup match resolves
// paths in the caller's, and the firewall matches /docker.
func (s *sandbox) helper(script string) error {
	if err := s.ensureImage(helperImage); err != nil {
		return err
	}
	_, err := s.dockerOutput("run", "--rm", "--privileged", "--pid=host", "--net=host", "--cgroupns=host", helperImage,
		"nsenter", "-t", "1", "-m", "-n", "--", "/bin/sh", "-c", script)
	return err
}

// ensureShell makes sure the shell container exists, runs the right image
// with the right network, and is started. The repo overlay is mounted at
// its Mac path, so paths inside match the Mac's.
func (s *sandbox) ensureShell(cfg vm.VMConfig) error {
	img, build, err := shellImage(s.repo, cfg.Sandbox.Image)
	if err != nil {
		return err
	}
	if build != nil {
		if _, err := s.dockerOutput("image", "inspect", img); err != nil {
			fmt.Fprintf(os.Stderr, "Building the devcontainer image %s...\n", img)
			if err := s.dockerRun(append([]string{"build", "-t", img}, build...)...); err != nil {
				return fmt.Errorf("building the devcontainer image: %w", err)
			}
		}
	} else if err := s.ensureImage(img); err != nil {
		return err
	}

	network := "host" // the VM's network, filtered by the firewall
	if cfg.Sandbox.NetworkOf() == vm.SandboxNetworkNone {
		network = "none"
	}
	want := img + "|" + network
	if got, err := s.dockerOutput("inspect", "--format", `{{index .Config.Labels "monolith.shell"}}`, shellName); err == nil {
		if got == want {
			if running, _ := s.dockerOutput("inspect", "--format", "{{.State.Running}}", shellName); running != "true" {
				_, err := s.dockerOutput("start", shellName)
				return err
			}
			return nil
		}
		fmt.Fprintln(os.Stderr, "Recreating the sandbox shell for the new image or network...")
		if _, err := s.dockerOutput("rm", "--force", shellName); err != nil {
			return err
		}
	}
	_, err = s.dockerOutput("run", "--detach", "--name", shellName, "--init",
		"--hostname", "lith", "--network", network,
		"--label", "monolith.shell="+want,
		"--volume", s.repo+":"+s.repo, "--workdir", s.repo,
		img, "tail", "-f", "/dev/null")
	if err != nil {
		return err
	}
	rc := fmt.Sprintf("PS1='(lith:%s) \\w \\$ '\ndeactivate() { exit 0; }\n", filepath.Base(s.repo))
	cmd := s.docker("exec", "-i", shellName, "sh", "-c", "cat > /etc/monolith-sandbox.rc")
	cmd.Stdin = strings.NewReader(rc)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("configuring the sandbox shell: %v: %s", err, out)
	}
	return nil
}

// prepare gets the sandbox ready to run commands: VM up, firewall applied,
// shell container running. It reports whether the VM was already running.
func (s *sandbox) prepare(cmd *cobra.Command) (bool, vm.VMConfig, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, vm.VMConfig{}, fmt.Errorf("the docker CLI is required (it talks to the sandbox's engine); install it, e.g. 'brew install docker'")
	}
	cfg, err := s.configure(cmd)
	if err != nil {
		return false, cfg, err
	}
	wasRunning, err := s.ensureRunning()
	if err != nil {
		return wasRunning, cfg, err
	}
	if err := s.applyFirewall(cfg.Sandbox); err != nil {
		return wasRunning, cfg, fmt.Errorf("applying the sandbox network policy: %w", err)
	}
	return wasRunning, cfg, s.ensureShell(cfg)
}

// workdir is the current directory if it's inside the repo, else the repo.
func (s *sandbox) workdir() string {
	cwd, err := os.Getwd()
	if err == nil {
		if real, err := filepath.EvalSymlinks(cwd); err == nil {
			cwd = real
		}
		if cwd == s.repo || strings.HasPrefix(cwd, s.repo+"/") {
			return cwd
		}
	}
	return s.repo
}

// Sessions: each activate/exec registers its PID so the last one out
// pauses the VM.

func (s *sandbox) sessionsDir() (string, error) {
	dir, err := vm.VMDir(s.name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

func (s *sandbox) beginSession() (func() (others int), error) {
	dir, err := s.sessionsDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	me := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(me, nil, 0644); err != nil {
		return nil, err
	}
	return func() int {
		os.Remove(me)
		others := 0
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err == nil && processAlive(pid) {
				others++
			} else {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
		return others
	}, nil
}

// openSessions counts live activate/exec sessions.
func (s *sandbox) openSessions() int {
	dir, err := s.sessionsDir()
	if err != nil {
		return 0
	}
	n := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil && processAlive(pid) {
			n++
		}
	}
	return n
}

// pause saves the sandbox to disk.
func (s *sandbox) pause() error {
	resp, err := vm.SendControlCommand(s.name, vm.ControlRequest{Cmd: vm.CmdSuspend})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

var activateCmd = &cobra.Command{
	Use:   "activate",
	Short: "Open a shell inside the repo's sandbox (deactivate or exit to leave)",
	Long: `Open a shell inside the sandbox for the current repo, creating or resuming it. Every
command you run there runs in the lith. 'deactivate' (or exit) returns to the Mac and
pauses the sandbox to disk, unless another session is still open or --keep-running.

--image, --network, --allow, --cpus, --memory and --rosetta are saved for the repo.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		if !isatty.IsTerminal(os.Stdin.Fd()) {
			return fmt.Errorf("activate needs a terminal; use 'monolith sandbox exec -- CMD' in scripts")
		}
		_, cfg, err := s.prepare(cmd)
		if err != nil {
			return err
		}
		end, err := s.beginSession()
		if err != nil {
			return err
		}
		banner(s, cfg)

		shell := s.docker("exec", "-it", "-e", "TERM="+os.Getenv("TERM"), "-w", s.workdir(), shellName,
			"sh", "-c", `if command -v bash >/dev/null; then exec bash --rcfile /etc/monolith-sandbox.rc -i; else ENV=/etc/monolith-sandbox.rc exec sh -i; fi`)
		shell.Stdin, shell.Stdout, shell.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr := shell.Run()

		others := end()
		switch {
		case flagKeep:
			fmt.Fprintf(os.Stderr, "Left sandbox %s running.\n", s.name)
		case others > 0:
			fmt.Fprintf(os.Stderr, "Left sandbox %s running (%d other session(s) open).\n", s.name, others)
		default:
			fmt.Fprintf(os.Stderr, "Pausing sandbox %s...\n", s.name)
			if err := s.pause(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: pausing the sandbox: %v\n", err)
			}
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return nil // the shell's own exit status
		}
		return runErr
	},
}

func banner(s *sandbox, cfg vm.VMConfig) {
	net := cfg.Sandbox.NetworkOf()
	var access string
	switch net {
	case vm.SandboxNetworkNone:
		access = "no network"
	case vm.SandboxNetworkAllow:
		access = "only " + strings.Join(cfg.Sandbox.Allow, ", ")
	default:
		access = "internet, but not your Mac or local network"
	}
	fmt.Fprintf(os.Stderr, `Sandbox %s for %s
  Mac files: this repo only, read-only (writes stay in the sandbox; 'monolith sandbox diff')
  Network:   %s
  Leave with 'deactivate'.
`, s.name, s.repo, access)
}

var execCmd = &cobra.Command{
	Use:   "exec -- COMMAND [ARGS...]",
	Short: "Run one command in the repo's sandbox",
	Long: `Run a command in the current repo's sandbox and exit with its status. A sandbox that
was paused is paused again afterwards (unless another session is open).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		wasRunning, _, err := s.prepare(cmd)
		if err != nil {
			return err
		}
		end, err := s.beginSession()
		if err != nil {
			return err
		}
		dockerArgs := []string{"exec", "-i", "-w", s.workdir()}
		if isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
			dockerArgs = append(dockerArgs, "-t", "-e", "TERM="+os.Getenv("TERM"))
		}
		run := s.docker(append(append(dockerArgs, shellName), args...)...)
		run.Stdin, run.Stdout, run.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr := run.Run()

		if others := end(); others == 0 && !wasRunning && !flagKeep {
			if err := s.pause(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: pausing the sandbox: %v\n", err)
			}
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return runErr
	},
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the current repo's sandbox",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		if !vm.Exists(s.name) {
			fmt.Printf("No sandbox for %s yet ('monolith sandbox activate' creates one).\n", s.repo)
			return nil
		}
		cfg, err := vm.ReadConfig(s.name)
		if err != nil {
			return err
		}
		st, _ := vm.ReadState(s.name)
		state := st.State
		if state == vm.StatusSuspended {
			state = "paused (on disk)"
		}
		img := cfg.Sandbox.Image
		if img == "" {
			img = "(devcontainer or " + defaultImage + ")"
		}
		fmt.Printf("Sandbox: %s\nRepo:    %s\nState:   %s\nImage:   %s\nNetwork: %s\n", s.name, s.repo, state, img, cfg.Sandbox.NetworkOf())
		if len(cfg.Sandbox.Allow) > 0 {
			fmt.Printf("Allow:   %s\n", strings.Join(cfg.Sandbox.Allow, ", "))
		}
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List sandboxes",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := vm.VMsDir()
		if err != nil {
			return err
		}
		entries, _ := os.ReadDir(dir)
		found := false
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "sbx-") || !vm.Exists(e.Name()) {
				continue
			}
			cfg, err := vm.ReadConfig(e.Name())
			if err != nil || cfg.Sandbox == nil {
				continue
			}
			if !found {
				fmt.Printf("%-32s %-18s %-9s %s\n", "NAME", "STATE", "NETWORK", "REPO")
				found = true
			}
			st, _ := vm.ReadState(e.Name())
			state := st.State
			if state == vm.StatusSuspended {
				state = "paused (on disk)"
			}
			fmt.Printf("%-32s %-18s %-9s %s\n", e.Name(), state, cfg.Sandbox.NetworkOf(), cfg.Sandbox.Repo)
		}
		if !found {
			fmt.Println("No sandboxes.")
		}
		return nil
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the current repo's sandbox (the writable layer is kept)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		st, err := vm.ReadState(s.name)
		if err != nil {
			return err
		}
		switch st.State {
		case vm.StatusStopped:
			fmt.Println("The sandbox isn't running.")
			return nil
		case vm.StatusSuspended:
			return vm.DiscardSuspended(s.name)
		}
		resp, err := vm.SendControlCommand(s.name, vm.ControlRequest{Cmd: vm.CmdStop})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}
		fmt.Printf("Sandbox %s stopped.\n", s.name)
		return nil
	},
}

var rmCmd = &cobra.Command{
	Use:     "rm",
	Aliases: []string{"remove", "delete"},
	Short:   "Delete the current repo's sandbox and everything written in it",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		if !vm.Exists(s.name) {
			return fmt.Errorf("no sandbox for %s", s.repo)
		}
		if !flagYes && !confirm(fmt.Sprintf("Delete sandbox %s and every change made in it (your Mac's files aren't touched)?", s.name)) {
			fmt.Println("Cancelled.")
			return nil
		}
		if st, err := vm.ReadState(s.name); err == nil && (st.State == vm.StatusRunning || st.State == vm.StatusPaused) {
			if _, err := vm.SendControlCommand(s.name, vm.ControlRequest{Cmd: vm.CmdStop, Force: true}); err != nil {
				return err
			}
		}
		dir, err := vm.VMDir(s.name)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Printf("Sandbox %s deleted.\n", s.name)
		return nil
	},
}

// confirm asks a yes/no question on stdin; anything but y/yes is no.
func confirm(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(nil) == nil
}

func init() {
	Cmd.AddCommand(activateCmd, execCmd, statusCmd, listCmd, stopCmd, rmCmd, diffCmd, applyCmd, resetCmd)
	Cmd.PersistentFlags().StringVar(&flagPath, "path", "", "repo directory (default: the git root of the current directory, else the directory)")
	for _, c := range []*cobra.Command{activateCmd, execCmd} {
		c.Flags().StringVar(&flagImage, "image", "", "container image for the shell (default: the repo's .devcontainer, else "+defaultImage+"; saved)")
		c.Flags().StringVar(&flagNetwork, "network", vm.SandboxNetworkOutbound, "network policy: outbound (internet, not the Mac or LAN), none, or allow (saved)")
		c.Flags().StringSliceVar(&flagAllow, "allow", nil, "with --network allow: host:port the sandbox may reach (repeatable; saved)")
		c.Flags().UintVar(&flagCPUs, "cpus", 0, "vCPUs (default 2; saved, applies on the next boot)")
		c.Flags().StringVar(&flagMemory, "memory", "", `RAM, e.g. "4G" (default 2G; saved, applies on the next boot)`)
		c.Flags().BoolVar(&flagRosetta, "rosetta", false, "run linux/amd64 binaries with Rosetta (saved)")
		c.Flags().BoolVar(&flagKeep, "keep-running", false, "don't pause the sandbox afterwards")
	}
	execCmd.Flags().SetInterspersed(false)
	rmCmd.Flags().BoolVarP(&flagYes, "yes", "y", false, "don't ask")
}

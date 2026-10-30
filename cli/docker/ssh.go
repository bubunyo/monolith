package docker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/bubunyo/monolith/vm"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var nerdctlCmd = &cobra.Command{
	Use:   "nerdctl [--name NAME] [nerdctl args...]",
	Short: "Run nerdctl in a containerd Docker host VM",
	Long: `Run nerdctl, containerd's Docker-compatible CLI, inside a containerd engine VM.
Arguments after --name are passed to nerdctl unchanged, e.g.

  monolith docker nerdctl run -d -p 8080:80 nginx
  monolith docker nerdctl --name lith-2 compose up -d

Interactive commands (-it) get a terminal. For a shorter command:

  alias nerdctl='monolith docker nerdctl --name <name>'

It runs as root over the VM's internal sshd, through the VM daemon.`,
	DisableFlagsInUseLine: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkEngine(flagName, vm.EngineContainerd); err != nil {
			return err
		}
		return execSSH(flagName, append([]string{"nerdctl"}, args...))
	},
}

// sshCommand builds the ssh invocation that runs remote (a command and its
// arguments) as root in the VM, through the daemon's ssh.sock. Host keys
// aren't checked: the connection goes over a local socket to this VM's own
// daemon, and the key changes whenever the VM is recreated.
func sshCommand(name string, tty bool, remote []string) (string, []string, error) {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return "", nil, fmt.Errorf("ssh not found on PATH")
	}
	key, err := vm.VMSSHKeyPath(name)
	if err != nil {
		return "", nil, err
	}
	sock, err := vm.VMSSHSockPath(name)
	if err != nil {
		return "", nil, err
	}
	args := []string{
		"ssh",
		"-i", key,
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ProxyCommand=nc -U " + shellQuote(sock),
	}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-T")
	}
	args = append(args, "root@"+name, "--")
	for _, a := range remote {
		args = append(args, shellQuote(a))
	}
	return path, args, nil
}

// execSSH replaces this process with ssh running remote in the VM, so the
// terminal, signals and exit code are nerdctl's own.
func execSSH(name string, remote []string) error {
	tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
	path, args, err := sshCommand(name, tty, remote)
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, os.Environ())
}

// runSSH runs remote in the VM without a terminal and returns its stdout.
func runSSH(name string, remote ...string) (string, error) {
	path, args, err := sshCommand(name, false, remote)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(path, args[1:]...)
	cmd.Stdin = nil
	out, err := cmd.Output()
	return string(out), err
}

// shellQuote quotes s for the remote POSIX shell ssh hands the command to.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func init() {
	// Stop flag parsing at the first argument so nerdctl's own flags
	// (run -d -p ...) pass through.
	nerdctlCmd.Flags().SetInterspersed(false)
}

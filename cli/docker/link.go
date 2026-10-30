package docker

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

// defaultDockerSocket is where tools that ignore contexts and DOCKER_HOST
// look for the Docker API.
const defaultDockerSocket = "/var/run/docker.sock"

var flagUnlink bool

var linkSocketCmd = &cobra.Command{
	Use:   "link-socket",
	Short: "Point /var/run/docker.sock at a Docker host (uses sudo)",
	Long: `Make /var/run/docker.sock a symlink to the host's docker.sock, for tools that only
look there and ignore docker contexts and DOCKER_HOST. Creating it in /var/run
needs root, so this runs sudo (it asks for your password). --unlink removes the
link if it points at this host.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkEngine(flagName, vm.EngineDocker); err != nil {
			return err
		}
		sock, err := vm.VMDockerSockPath(flagName)
		if err != nil {
			return err
		}
		target, _ := os.Readlink(defaultDockerSocket)
		if flagUnlink {
			if target != sock {
				return fmt.Errorf("%s does not point at %q's socket", defaultDockerSocket, flagName)
			}
			if err := sudo("rm", defaultDockerSocket); err != nil {
				return err
			}
			fmt.Printf("Removed %s.\n", defaultDockerSocket)
			return nil
		}
		if target == sock {
			fmt.Printf("%s already points at %q.\n", defaultDockerSocket, flagName)
			return nil
		}
		if fi, err := os.Lstat(defaultDockerSocket); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%s exists and isn't a symlink (another Docker installation?); not replacing it", defaultDockerSocket)
		}
		if err := sudo("ln", "-sfn", sock, defaultDockerSocket); err != nil {
			return err
		}
		fmt.Printf("%s now points at %q.\n", defaultDockerSocket, flagName)
		return nil
	},
}

// sudo runs a command as root, letting sudo prompt on the terminal.
func sudo(args ...string) error {
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo %v: %w", args, err)
	}
	return nil
}

func init() {
	linkSocketCmd.Flags().BoolVar(&flagUnlink, "unlink", false, "remove the link instead")
}

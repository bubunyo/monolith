// Package docker implements `monolith docker`: a Docker host VM for the
// docker CLI on the Mac, in the style of Colima.
package docker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	clivm "github.com/bubunyo/monolith/cli/vm"
	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

const (
	// vmName is the VM backing `monolith docker`.
	vmName = "docker"

	defaultCPUs   = 2
	defaultMemory = 2 << 30

	// readyTimeout covers the first boot, which installs Docker.
	readyTimeout = 10 * time.Minute
)

// Cmd is the parent command for the Docker host.
var Cmd = &cobra.Command{
	Use:   "docker",
	Short: "Run a Docker host VM for the docker CLI",
	Long: `Run Docker Engine in a monolith VM and expose it to the docker CLI on the Mac.

The VM ("docker", image docker) uses user-mode networking, so pulls work wherever
the Mac is online. Its dockerd API is available at a unix socket on the Mac;
point DOCKER_HOST at it (see 'monolith docker env'). Ports published with -p are
forwarded to localhost, and /Users is shared into the VM at the same path for -v.`,
}

var (
	flagCPUs   uint
	flagMemory string
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Docker host VM and wait for dockerd",
	Long: `Start the Docker host VM (creating it on first run) and wait until dockerd answers.
The first start downloads Alpine and installs Docker in the VM, which takes a minute or two.

--cpus and --memory apply when the VM boots; they are saved for later starts.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return start(cmd)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Docker host VM",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := vm.SendControlCommand(vmName, vm.ControlRequest{Cmd: vm.CmdStop})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}
		fmt.Println("Docker host stopped.")
		return nil
	},
}

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Print the DOCKER_HOST export for this shell",
	Long:  "Print the DOCKER_HOST export for the Docker host VM. Use: eval \"$(monolith docker env)\"",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		sock, err := vm.VMDockerSockPath(vmName)
		if err != nil {
			return err
		}
		fmt.Printf("export DOCKER_HOST=unix://%s\n", sock)
		return nil
	},
}

func start(cmd *cobra.Command) error {
	sock, err := vm.VMDockerSockPath(vmName)
	if err != nil {
		return err
	}

	status, err := vm.ReadState(vmName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusRunning:
		fmt.Println("Docker host is already running.")
	case vm.StatusPaused:
		return fmt.Errorf("the Docker host VM is paused; resume it with 'monolith vm resume %s'", vmName)
	default:
		if err := boot(cmd); err != nil {
			return err
		}
	}

	fmt.Println("Waiting for dockerd...")
	if err := waitForDockerd(sock, readyTimeout); err != nil {
		return fmt.Errorf("%w; see 'monolith vm attach %s' and /var/log/docker.log in the VM", err, vmName)
	}

	fmt.Printf(`Docker is ready. Point the docker CLI at it with:

  export DOCKER_HOST=unix://%s

or create a context once: docker context create monolith --docker host=unix://%s && docker context use monolith
`, sock, sock)
	return nil
}

// boot writes the docker VM's config and spawns its daemon.
func boot(cmd *cobra.Command) error {
	cfg := vm.DefaultConfig()
	cfg.CPUs = defaultCPUs
	cfg.Memory = defaultMemory
	if vm.Exists(vmName) {
		var err error
		if cfg, err = vm.ReadConfig(vmName); err != nil {
			return err
		}
		if cfg.Image != vm.ImageDocker {
			return fmt.Errorf("VM %q exists with image %s; remove it or rename it to use 'monolith docker'", vmName, cfg.Image)
		}
	}
	cfg.Image = vm.ImageDocker
	cfg.NetworkMode = mnet.NetworkUser
	if cmd.Flags().Changed("cpus") {
		cfg.CPUs = flagCPUs
	}
	if cmd.Flags().Changed("memory") {
		mem, err := clivm.ParseMemory(flagMemory)
		if err != nil {
			return err
		}
		cfg.Memory = mem
	}
	if err := vm.WriteConfig(vmName, cfg); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	image, err := vm.LookupImage(cfg.Image)
	if err != nil {
		return err
	}
	fmt.Println("Preparing docker base image...")
	if _, err := image.Prepare(vmName); err != nil {
		return fmt.Errorf("preparing docker image: %w", err)
	}

	fmt.Printf("Booting Docker host VM %q (%d vCPU, %s RAM)...\n", vmName, cfg.CPUs, clivm.FormatMemory(cfg.Memory))
	return vm.SpawnVMDaemon(vmName, false, "")
}

// waitForDockerd polls dockerd's /_ping through the host socket.
func waitForDockerd(sock string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://docker/_ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if status, err := vm.ReadState(vmName); err == nil && status.State == vm.StatusStopped {
			return fmt.Errorf("the Docker host VM stopped while starting")
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("dockerd did not answer within %s", timeout)
}

func init() {
	Cmd.AddCommand(startCmd)
	Cmd.AddCommand(stopCmd)
	Cmd.AddCommand(envCmd)

	startCmd.Flags().UintVarP(&flagCPUs, "cpus", "c", 0, "number of vCPUs (default: 2)")
	startCmd.Flags().StringVarP(&flagMemory, "memory", "m", "", `RAM size, e.g. "2G", "4096M" (default: 2G)`)
}

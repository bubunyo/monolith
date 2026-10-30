// Package docker implements `monolith docker`: Docker host VMs for the
// docker CLI on the Mac, in the style of Colima.
package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	clivm "github.com/bubunyo/monolith/cli/vm"
	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

const (
	defaultName   = "docker-lith-default"
	defaultCPUs   = 2
	defaultMemory = 2 << 30

	// readyTimeout covers the first boot, which installs Docker.
	readyTimeout = 10 * time.Minute
)

// Cmd is the parent command for Docker host VMs.
var Cmd = &cobra.Command{
	Use:   "docker",
	Short: "Run Docker host VMs for the docker CLI",
	Long: `Run Docker Engine in monolith VMs and expose each to the docker CLI on the Mac.

Each Docker host is a VM with the docker image and user-mode networking, so pulls
work wherever the Mac is online. 'start' points the docker CLI at it by creating
and selecting the context docker-<name>. Ports published with -p are forwarded to
localhost, and /Users is shared into the VM at the same path for -v.

Every command takes --name (default "lith-default") to pick the VM.`,
}

var (
	flagName   string
	flagCPUs   uint
	flagMemory string
	flagForce  bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start (or resume) a Docker host VM and make its context current",
	Long: `Start a Docker host VM, creating it on first run, wait until dockerd answers, and make
its docker context (docker-<name>) the current one. A paused VM is resumed instead.
The first start downloads Alpine and installs Docker in the VM, which takes a minute or so.

--cpus and --memory apply when the VM boots; they are saved for later starts.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return start(cmd)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a Docker host VM",
	Long: `Stop a Docker host VM. Images, containers and volumes stay on its data disk.

On a paused VM this discards the saved memory state (running containers are lost,
their data is not); it asks first unless --force is given.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return stop()
	},
}

var pauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Save a Docker host VM's entire state to disk and stop it",
	Long: `Save the VM's memory and device state to disk and stop it. 'resume' (or 'start')
restores it exactly, with running containers carrying on where they were.

Unlike 'docker pause', which freezes containers in memory, this frees the VM's
memory and CPU. Network connections open at the time are dropped on resume.
Resuming requires the Mac to be unlocked.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return pause()
	},
}

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Restore a paused Docker host VM and make its context current",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return resume()
	},
}

var rmCmd = &cobra.Command{
	Use:     "rm",
	Aliases: []string{"remove", "delete"},
	Short:   "Delete a Docker host VM, its images and containers, and its context",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return remove()
	},
}

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Print the DOCKER_HOST export for a Docker host VM",
	Long:  "Print the DOCKER_HOST export for a Docker host VM, for scripts that don't use contexts: eval \"$(monolith docker env)\"",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		sock, err := vm.VMDockerSockPath(flagName)
		if err != nil {
			return err
		}
		fmt.Printf("export DOCKER_HOST=unix://%s\n", sock)
		return nil
	},
}

func start(cmd *cobra.Command) error {
	if err := clivm.ValidateName(flagName); err != nil {
		return err
	}
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusRunning:
		fmt.Printf("Docker host %q is already running.\n", flagName)
	case vm.StatusPaused:
		return fmt.Errorf("VM %q is paused in memory; resume it with 'monolith vm resume %s'", flagName, flagName)
	case vm.StatusSuspended:
		if err := checkDockerVM(flagName); err != nil {
			return err
		}
		fmt.Printf("Resuming Docker host %q...\n", flagName)
		if err := vm.SpawnVMDaemon(flagName, true, ""); err != nil {
			return err
		}
	default:
		if err := boot(cmd); err != nil {
			return err
		}
	}
	return ready()
}

// boot writes the VM's config and spawns its daemon.
func boot(cmd *cobra.Command) error {
	cfg := vm.DefaultConfig()
	cfg.CPUs = defaultCPUs
	cfg.Memory = defaultMemory
	if vm.Exists(flagName) {
		if err := checkDockerVM(flagName); err != nil {
			return err
		}
		var err error
		if cfg, err = vm.ReadConfig(flagName); err != nil {
			return err
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
	if err := vm.WriteConfig(flagName, cfg); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	image, err := vm.LookupImage(cfg.Image)
	if err != nil {
		return err
	}
	fmt.Println("Preparing docker base image...")
	if _, err := image.Prepare(flagName); err != nil {
		return fmt.Errorf("preparing docker image: %w", err)
	}

	fmt.Printf("Booting Docker host %q (%d vCPU, %s RAM)...\n", flagName, cfg.CPUs, clivm.FormatMemory(cfg.Memory))
	return vm.SpawnVMDaemon(flagName, false, "")
}

// ready waits for dockerd and makes the VM's context current.
func ready() error {
	sock, err := vm.VMDockerSockPath(flagName)
	if err != nil {
		return err
	}
	fmt.Println("Waiting for dockerd...")
	client := dockerClient(sock)
	if err := waitForDockerd(client, flagName, readyTimeout); err != nil {
		return fmt.Errorf("%w; see 'monolith vm attach %s' and /var/log/docker.log in the VM", err, flagName)
	}
	waitForClock(client, clockSyncTimeout)

	ctxName := contextName(flagName)
	switch err := useContext(flagName, sock); {
	case errors.Is(err, errNoDockerCLI):
		fmt.Printf("Docker host %q is ready. The docker CLI isn't on PATH; its socket is %s\n", flagName, sock)
		return nil
	case err != nil:
		return fmt.Errorf("Docker host %q is ready, but setting the docker context failed: %w", flagName, err)
	}
	fmt.Printf("Docker host %q is ready (context %s is now current).\n", flagName, ctxName)
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		fmt.Printf("Warning: DOCKER_HOST=%s is set in this shell and overrides the context; run 'unset DOCKER_HOST'.\n", host)
	}
	return nil
}

func stop() error {
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusStopped:
		if !vm.Exists(flagName) {
			return fmt.Errorf("Docker host %q does not exist", flagName)
		}
		fmt.Printf("Docker host %q is already stopped.\n", flagName)
		return nil
	case vm.StatusSuspended:
		if !flagForce && !confirm(fmt.Sprintf("Docker host %q is paused. Discard its saved state? Running containers will be lost (images and volumes are kept).", flagName)) {
			fmt.Println("Cancelled.")
			return nil
		}
		if err := vm.DiscardSuspended(flagName); err != nil {
			return err
		}
		fmt.Printf("Docker host %q stopped; its saved state was discarded.\n", flagName)
		return nil
	}
	resp, err := vm.SendControlCommand(flagName, vm.ControlRequest{Cmd: vm.CmdStop})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Printf("Docker host %q stopped.\n", flagName)
	return nil
}

func pause() error {
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusSuspended:
		fmt.Printf("Docker host %q is already paused.\n", flagName)
		return nil
	case vm.StatusStopped:
		return fmt.Errorf("Docker host %q is not running", flagName)
	}
	if err := checkDockerVM(flagName); err != nil {
		return err
	}
	fmt.Printf("Saving Docker host %q to disk...\n", flagName)
	resp, err := vm.SendControlCommand(flagName, vm.ControlRequest{Cmd: vm.CmdSuspend})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Printf("Docker host %q paused. Resume with 'monolith docker resume --name %s'.\n", flagName, flagName)
	return nil
}

func resume() error {
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusRunning:
		fmt.Printf("Docker host %q is already running.\n", flagName)
	case vm.StatusSuspended:
		if err := checkDockerVM(flagName); err != nil {
			return err
		}
		fmt.Printf("Resuming Docker host %q...\n", flagName)
		if err := vm.SpawnVMDaemon(flagName, true, ""); err != nil {
			return err
		}
	case vm.StatusPaused:
		return fmt.Errorf("VM %q is paused in memory; resume it with 'monolith vm resume %s'", flagName, flagName)
	default:
		return fmt.Errorf("Docker host %q is not paused; start it with 'monolith docker start --name %s'", flagName, flagName)
	}
	return ready()
}

func remove() error {
	if !vm.Exists(flagName) {
		return fmt.Errorf("Docker host %q does not exist", flagName)
	}
	if err := checkDockerVM(flagName); err != nil {
		return err
	}
	if !flagForce && !confirm(fmt.Sprintf("Delete Docker host %q with all its images, containers and volumes?", flagName)) {
		fmt.Println("Cancelled.")
		return nil
	}

	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	if status.State == vm.StatusRunning || status.State == vm.StatusPaused {
		resp, err := vm.SendControlCommand(flagName, vm.ControlRequest{Cmd: vm.CmdStop, Force: true})
		if err != nil {
			return fmt.Errorf("stopping %q: %w", flagName, err)
		}
		if !resp.OK {
			return fmt.Errorf("stopping %q: %s", flagName, resp.Error)
		}
	}

	dir, err := vm.VMDir(flagName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing %q: %w", flagName, err)
	}
	if err := removeContext(flagName); err != nil {
		fmt.Printf("Warning: removing context %s: %v\n", contextName(flagName), err)
	}
	fmt.Printf("Docker host %q deleted.\n", flagName)
	return nil
}

// checkDockerVM refuses to act on an existing VM that isn't a Docker host.
func checkDockerVM(name string) error {
	cfg, err := vm.ReadConfig(name)
	if err != nil {
		return err
	}
	if cfg.Image != vm.ImageDocker {
		return fmt.Errorf("VM %q uses the %s image, not docker; pick another --name", name, cfg.Image)
	}
	return nil
}

// dockerClient talks to dockerd through the VM's host socket.
func dockerClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// waitForDockerd polls dockerd's /_ping.
func waitForDockerd(client *http.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := client.Get("http://docker/_ping"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if status, err := vm.ReadState(name); err == nil && status.State == vm.StatusStopped {
			return fmt.Errorf("Docker host %q stopped while starting", name)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("dockerd did not answer within %s", timeout)
}

// clockSyncTimeout bounds waiting for the guest clock after a restore; the
// guest's clock loop corrects it within about 2s.
const clockSyncTimeout = 10 * time.Second

// waitForClock waits until dockerd's clock is within 2s of the Mac's, so
// commands run right after a resume see the right time. A restored VM's
// clock starts out behind by however long it was paused. Gives up silently
// on timeout or error.
func waitForClock(client *http.Client, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://docker/info")
		if err != nil {
			return
		}
		var info struct {
			SystemTime time.Time `json:"SystemTime"`
		}
		err = json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
		if err != nil {
			return
		}
		if d := time.Since(info.SystemTime); d < 2*time.Second && d > -2*time.Second {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// confirm asks a yes/no question on stdin; anything but y/yes is no.
func confirm(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

func init() {
	Cmd.AddCommand(startCmd, stopCmd, pauseCmd, resumeCmd, listCmd, rmCmd, envCmd)

	for _, c := range []*cobra.Command{startCmd, stopCmd, pauseCmd, resumeCmd, rmCmd, envCmd} {
		c.Flags().StringVar(&flagName, "name", defaultName, "Docker host VM name")
	}
	startCmd.Flags().UintVarP(&flagCPUs, "cpus", "c", 0, "number of vCPUs (default: 2)")
	startCmd.Flags().StringVarP(&flagMemory, "memory", "m", "", `RAM size, e.g. "2G", "4096M" (default: 2G)`)
	stopCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "discard a paused VM's saved state without asking")
	rmCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "delete without asking")
}

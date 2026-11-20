// Package cluster implements `monolith cluster`: a single-node Kubernetes
// (k3s) cluster in a VM.
package cluster

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	clivm "github.com/bubunyo/monolith/cli/vm"
	"github.com/bubunyo/monolith/internal/kubeconfig"
	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

const (
	defaultName   = "cluster-default"
	defaultCPUs   = 2
	defaultMemory = 4 << 30

	// readyTimeout covers the first start, which installs packages and pulls
	// k3s's system images.
	readyTimeout = 10 * time.Minute

	// prevContextFile, in the VM's directory, holds the kubectl context that
	// was current before the cluster's became current.
	prevContextFile = "prev-context"
)

// Cmd is the parent command for cluster VMs.
var Cmd = &cobra.Command{
	Use:   "cluster",
	Short: "Run a single-node Kubernetes (k3s) cluster in a VM",
	Long: `Run a single-node Kubernetes cluster in a monolith VM and use it from the Mac.

Each cluster is a VM running k3s on its own containerd, with user-mode
networking so image pulls work wherever the Mac is online. 'start' merges the
cluster into your kubeconfig as the context cluster-<name> and makes it
current; the API is on localhost.

The VM is an ordinary monolith VM, so 'monolith vm attach <name>' opens its
console.

Every command except list takes --name (default "cluster-default").`,
}

var (
	flagName    string
	flagCPUs    uint
	flagMemory  string
	flagK3s     string
	flagRosetta bool
	flagForce   bool
	flagStart   bool
	flagQuiet   bool
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start (or resume) a cluster VM and make its context current",
	Long: `Start a cluster VM, creating it on first run, and wait until its node is Ready.
Its kubectl context (cluster-<name>) becomes the current one. A paused cluster is
resumed instead. The first start downloads k3s and Alpine and pulls k3s's system
images, which takes a minute or so.

--cpus, --memory, --k3s-version and --rosetta apply when the VM boots; they are
saved for later starts.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return start(cmd)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a cluster VM",
	Long: `Stop a cluster VM. The cluster's state (images, workloads) stays on its data disk.

On a paused cluster this discards the saved memory state (running pods are
restarted on the next start); it asks first unless --force is given.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return stop()
	},
}

var pauseCmd = &cobra.Command{
	Use:   "pause",
	Short: "Save a cluster VM's entire state to disk and stop it",
	Long: `Save the VM's memory and device state to disk and stop it. 'resume' (or 'start')
restores it exactly, with the pods carrying on where they were. This frees the
VM's memory and CPU. Network connections open at the time are dropped on resume.
Resuming requires the Mac to be unlocked.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return pause()
	},
}

var resumeCmd = &cobra.Command{
	Use:   "resume",
	Short: "Restore a paused cluster VM and make its context current",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return resume()
	},
}

var rmCmd = &cobra.Command{
	Use:     "rm",
	Aliases: []string{"remove", "delete"},
	Short:   "Delete a cluster VM, everything in it, and its kubectl context",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return remove()
	},
}

var kubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Print the kubeconfig for a cluster",
	Long: `Print a standalone kubeconfig for the cluster (context cluster-<name>, API on
localhost), e.g. for KUBECONFIG=<file> in scripts. 'start' already merges the
same entries into your kubeconfig.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkCluster(flagName); err != nil {
			return err
		}
		kc, err := readKubeconfig(flagName)
		if err != nil {
			return err
		}
		out, err := kc.Marshal()
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List cluster VMs",
	Long: `List cluster VMs with their state, k3s version, kubectl context (* = current), API
address and the service ports forwarded to localhost. A port marked "!" is
exposed by a service but couldn't be forwarded, usually because another VM
already uses it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return list()
	},
}

var useCmd = &cobra.Command{
	Use:     "use [name]",
	Aliases: []string{"switch"},
	Short:   "Make a cluster's kubectl context current",
	Long: `Make a cluster's kubectl context (cluster-<name>) the current one, without booting
anything. A cluster that isn't running is reported as such: bring it up with
'cluster start' or 'cluster resume', or pass --start to do it here.

With no name, print the current cluster.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return showCurrent()
		}
		return use(args[0])
	},
}

func start(cmd *cobra.Command) error {
	if err := clivm.ValidateName(flagName); err != nil {
		return err
	}
	if vm.Exists(flagName) {
		if err := checkCluster(flagName); err != nil {
			return err
		}
		cfg, err := vm.ReadConfig(flagName)
		if err != nil {
			return err
		}
		// k3s's version is a share and a boot argument: only on a fresh boot.
		if status, err := vm.ReadState(flagName); err == nil && status.State != vm.StatusStopped &&
			cmd.Flags().Changed("k3s-version") && flagK3s != vm.K3sVersionOf(cfg) {
			return fmt.Errorf("%q is %s; stop it to change --k3s-version", flagName, status.State)
		}
		// Rosetta is a device and a boot argument too.
		if status, err := vm.ReadState(flagName); err == nil && status.State != vm.StatusStopped &&
			cmd.Flags().Changed("rosetta") && flagRosetta != cfg.Rosetta {
			return fmt.Errorf("%q is %s; stop it to change --rosetta", flagName, status.State)
		}
	}
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusRunning:
		fmt.Printf("Cluster %q is already running.\n", flagName)
	case vm.StatusPaused:
		return fmt.Errorf("VM %q is paused in memory; resume it with 'monolith vm resume %s'", flagName, flagName)
	case vm.StatusSuspended:
		fmt.Printf("Resuming cluster %q...\n", flagName)
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

// startConfig is the cluster's saved config (or the defaults) with the
// start flags given on this command line applied.
func startConfig(cmd *cobra.Command) (vm.VMConfig, error) {
	cfg := vm.DefaultConfig()
	cfg.CPUs = defaultCPUs
	cfg.Memory = defaultMemory
	if vm.Exists(flagName) {
		var err error
		if cfg, err = vm.ReadConfig(flagName); err != nil {
			return cfg, err
		}
	}
	cfg.Image = vm.ImageCluster
	cfg.NetworkMode = mnet.NetworkUser
	if cmd.Flags().Changed("cpus") {
		cfg.CPUs = flagCPUs
	}
	if cmd.Flags().Changed("memory") {
		mem, err := clivm.ParseMemory(flagMemory)
		if err != nil {
			return cfg, err
		}
		cfg.Memory = mem
	}
	if cmd.Flags().Changed("k3s-version") {
		cfg.K3sVersion = flagK3s
	}
	if cmd.Flags().Changed("rosetta") {
		cfg.Rosetta = flagRosetta
	}
	return cfg, nil
}

// boot writes the VM's config and spawns its daemon.
func boot(cmd *cobra.Command) error {
	cfg, err := startConfig(cmd)
	if err != nil {
		return err
	}
	if cfg.KubePort == 0 {
		port, err := vm.FreeLocalPort()
		if err != nil {
			return fmt.Errorf("choosing a port for the Kubernetes API: %w", err)
		}
		cfg.KubePort = port
	}
	if err := vm.WriteConfig(flagName, cfg); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	// The previous run's kubeconfig must not pass for this boot's.
	if path, err := vm.VMKubeconfigPath(flagName); err == nil {
		os.Remove(path)
	}

	image, err := vm.LookupImage(cfg.Image)
	if err != nil {
		return err
	}
	fmt.Printf("Preparing cluster %q (k3s %s)...\n", flagName, vm.K3sVersionOf(cfg))
	if _, err := image.Prepare(flagName, os.Stdout); err != nil {
		return fmt.Errorf("preparing the cluster image: %w", err)
	}
	fmt.Printf("Booting cluster %q (%d vCPU, %s RAM)...\n", flagName, cfg.CPUs, clivm.FormatMemory(cfg.Memory))
	return vm.SpawnVMDaemon(flagName, false, "")
}

// ready waits for k3s to write its kubeconfig and for the node to be Ready,
// then merges the cluster into the user's kubeconfig.
func ready() error {
	cfg, err := vm.ReadConfig(flagName)
	if err != nil {
		return err
	}
	fmt.Println("Waiting for Kubernetes (the first start pulls k3s's system images)...")
	deadline := time.Now().Add(readyTimeout)
	var kc *kubeconfig.Config
	for {
		if kc, err = readKubeconfig(flagName); err == nil {
			break
		}
		if status, serr := vm.ReadState(flagName); serr == nil && status.State == vm.StatusStopped {
			return fmt.Errorf("cluster %q stopped while starting; see 'monolith vm logs %s'", flagName, flagName)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s did not start within %s: %w; see 'monolith vm attach %s' and /var/log/k3s.log in the VM", readyTimeout, err, flagName)
		}
		time.Sleep(2 * time.Second)
	}
	client, err := kc.Client()
	if err != nil {
		return err
	}
	for !kc.NodeReady(client) {
		if status, serr := vm.ReadState(flagName); serr == nil && status.State == vm.StatusStopped {
			return fmt.Errorf("cluster %q stopped while starting; see 'monolith vm logs %s'", flagName, flagName)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Kubernetes node was not ready within %s; see 'monolith vm attach %s' and /var/log/k3s.log in the VM", readyTimeout, flagName)
		}
		time.Sleep(2 * time.Second)
	}
	prev, err := prevContextPath(flagName)
	if err != nil {
		return err
	}
	if err := kubeconfig.Merge(kc, prev); err != nil {
		return fmt.Errorf("Kubernetes is ready, but updating your kubeconfig failed: %w", err)
	}
	path, _ := kubeconfig.Path()
	fmt.Printf("Kubernetes %s is ready (kubectl context %s is now current in %s).\n", vm.K3sVersionOf(cfg), kc.CurrentContext, path)
	return nil
}

// readKubeconfig reads the kubeconfig k3s wrote in the VM and rewrites it
// for the Mac.
func readKubeconfig(name string) (*kubeconfig.Config, error) {
	cfg, err := vm.ReadConfig(name)
	if err != nil {
		return nil, err
	}
	path, err := vm.VMKubeconfigPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("k3s hasn't written its kubeconfig yet (is %q running?)", name)
		}
		return nil, err
	}
	return kubeconfig.Parse(data, name, cfg.KubePort)
}

func prevContextPath(name string) (string, error) {
	dir, err := vm.VMDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prevContextFile), nil
}

func stop() error {
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusStopped:
		if !vm.Exists(flagName) {
			return fmt.Errorf("cluster %q does not exist", flagName)
		}
		if err := checkCluster(flagName); err != nil {
			return err
		}
		fmt.Printf("Cluster %q is already stopped.\n", flagName)
		return nil
	case vm.StatusSuspended:
		if !flagForce && !confirm(fmt.Sprintf("Cluster %q is paused. Discard its saved state? Running pods will restart on the next start (images and volumes are kept).", flagName)) {
			fmt.Println("Cancelled.")
			return nil
		}
		if err := vm.DiscardSuspended(flagName); err != nil {
			return err
		}
		fmt.Printf("Cluster %q stopped; its saved state was discarded.\n", flagName)
		return nil
	}
	if err := checkCluster(flagName); err != nil {
		return err
	}
	resp, err := vm.SendControlCommand(flagName, vm.ControlRequest{Cmd: vm.CmdStop})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Printf("Cluster %q stopped.\n", flagName)
	return nil
}

func pause() error {
	if !vm.Exists(flagName) {
		return fmt.Errorf("cluster %q does not exist", flagName)
	}
	if err := checkCluster(flagName); err != nil {
		return err
	}
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusSuspended:
		fmt.Printf("Cluster %q is already paused.\n", flagName)
		return nil
	case vm.StatusStopped:
		return fmt.Errorf("cluster %q is not running", flagName)
	}
	fmt.Printf("Saving cluster %q to disk...\n", flagName)
	resp, err := vm.SendControlCommand(flagName, vm.ControlRequest{Cmd: vm.CmdSuspend})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	fmt.Printf("Cluster %q paused. Resume with 'monolith cluster resume --name %s'.\n", flagName, flagName)
	return nil
}

func resume() error {
	if !vm.Exists(flagName) {
		return fmt.Errorf("cluster %q does not exist", flagName)
	}
	if err := checkCluster(flagName); err != nil {
		return err
	}
	status, err := vm.ReadState(flagName)
	if err != nil {
		return fmt.Errorf("reading VM state: %w", err)
	}
	switch status.State {
	case vm.StatusRunning:
		fmt.Printf("Cluster %q is already running.\n", flagName)
	case vm.StatusSuspended:
		fmt.Printf("Resuming cluster %q...\n", flagName)
		if err := vm.SpawnVMDaemon(flagName, true, ""); err != nil {
			return err
		}
	case vm.StatusPaused:
		return fmt.Errorf("VM %q is paused in memory; resume it with 'monolith vm resume %s'", flagName, flagName)
	default:
		return fmt.Errorf("cluster %q is not paused; start it with 'monolith cluster start --name %s'", flagName, flagName)
	}
	return ready()
}

func remove() error {
	if !vm.Exists(flagName) {
		return fmt.Errorf("cluster %q does not exist", flagName)
	}
	if err := checkCluster(flagName); err != nil {
		return err
	}
	if !flagForce && !confirm(fmt.Sprintf("Delete cluster %q with everything in it?", flagName)) {
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

	// Before the VM's directory goes: it holds the note of which context was
	// current before this cluster's.
	if prev, err := prevContextPath(flagName); err == nil {
		if err := kubeconfig.Remove(flagName, prev); err != nil {
			fmt.Printf("Warning: removing kubectl context %s: %v\n", kubeconfig.ContextName(flagName), err)
		}
	}
	dir, err := vm.VMDir(flagName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing %q: %w", flagName, err)
	}
	fmt.Printf("Cluster %q deleted.\n", flagName)
	return nil
}

// clusterNames lists the VMs that use the cluster image, sorted.
func clusterNames() ([]string, error) {
	dir, err := vm.VMsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !vm.Exists(e.Name()) {
			continue
		}
		if cfg, err := vm.ReadConfig(e.Name()); err == nil && cfg.Image == vm.ImageCluster {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func completeNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names, _ := clusterNames()
	return names, cobra.ShellCompDirectiveNoFileComp
}

func list() error {
	names, err := clusterNames()
	if err != nil {
		return err
	}
	if flagQuiet {
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	}
	if len(names) == 0 {
		fmt.Println("No clusters. Create one with 'monolith cluster start'.")
		return nil
	}

	current := kubeconfig.Current()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tK3S\tCONTEXT\tCPUS\tMEM\tDATA\tAPI\tPORTS")
	for _, name := range names {
		cfg, _ := vm.ReadConfig(name)
		state := vm.StatusStopped
		if status, err := vm.ReadState(name); err == nil {
			state = status.State
		}
		if state == vm.StatusSuspended {
			state = "paused (on disk)"
		}
		ctx := kubeconfig.ContextName(name)
		if ctx == current {
			ctx += " *"
		}
		api, ports := "-", "-"
		if state == vm.StatusRunning {
			if cfg.KubePort != 0 {
				api = fmt.Sprintf("localhost:%d", cfg.KubePort)
			}
			ports = vm.ForwardedPorts(name)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", name, state, vm.K3sVersionOf(cfg), ctx, cfg.CPUs, clivm.FormatMemory(cfg.Memory), dataUsage(name), api, ports)
	}
	return w.Flush()
}

// dataUsage is the space the VM's sparse data disk actually uses.
func dataUsage(name string) string {
	dir, err := vm.VMDir(name)
	if err != nil {
		return "-"
	}
	fi, err := os.Stat(filepath.Join(dir, "data.img"))
	if err != nil {
		return "-"
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "-"
	}
	used := uint64(st.Blocks) * 512
	const mib = 1 << 20
	if used < 1<<30 {
		return fmt.Sprintf("%.0f MiB", float64(used)/mib)
	}
	return fmt.Sprintf("%.1f GiB", float64(used)/(1<<30))
}

// checkCluster refuses to act on an existing VM that isn't a cluster.
func checkCluster(name string) error {
	if !vm.Exists(name) {
		return fmt.Errorf("cluster %q does not exist", name)
	}
	cfg, err := vm.ReadConfig(name)
	if err != nil {
		return err
	}
	if cfg.Image != vm.ImageCluster {
		return fmt.Errorf("VM %q uses the %s image, not cluster; pick another --name", name, cfg.Image)
	}
	return nil
}

// confirm asks a yes/no question on stdin; anything but y/yes is no.
func confirm(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}

func addStartFlags(c *cobra.Command) {
	c.Flags().UintVarP(&flagCPUs, "cpus", "c", 0, "number of vCPUs (default: 2)")
	c.Flags().StringVarP(&flagMemory, "memory", "m", "", `RAM size, e.g. "4G", "8192M" (default: 4G)`)
	c.Flags().StringVar(&flagK3s, "k3s-version", vm.DefaultK3sVersion, "k3s release (saved)")
	c.Flags().BoolVar(&flagRosetta, "rosetta", false, "run linux/amd64 binaries in pods with Rosetta (saved); the image must be labelled arm64, see docs/cluster.md")
}

func init() {
	Cmd.AddCommand(useCmd, startCmd, stopCmd, pauseCmd, resumeCmd, listCmd, rmCmd, kubeconfigCmd)

	for _, c := range []*cobra.Command{startCmd, stopCmd, pauseCmd, resumeCmd, rmCmd, kubeconfigCmd} {
		c.Flags().StringVar(&flagName, "name", defaultName, "cluster VM name")
		_ = c.RegisterFlagCompletionFunc("name", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			names, _ := clusterNames()
			return names, cobra.ShellCompDirectiveNoFileComp
		})
	}
	useCmd.Flags().BoolVar(&flagStart, "start", false, "start or resume the cluster first if it isn't running")
	listCmd.Flags().BoolVarP(&flagQuiet, "quiet", "q", false, "print only cluster names")
	addStartFlags(startCmd)
	stopCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "discard a paused cluster's saved state without asking")
	rmCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "delete without asking")
}

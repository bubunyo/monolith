package docker

import (
	"fmt"
	"os"
	"sort"
	"strings"

	clivm "github.com/bubunyo/monolith/cli/vm"
	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var (
	flagUseStart  bool
	flagUseKube   bool
	flagListQuiet bool
)

var useCmd = &cobra.Command{
	Use:     "use [name]",
	Aliases: []string{"switch"},
	Short:   "Switch the docker CLI to another Docker host VM",
	Long: `Make a Docker host's context (docker-<name>) the docker CLI's current one, without
booting anything. A stopped or paused host is reported as such: bring it up with
'docker start' or 'docker resume', or pass --start to do it here.

With --kubernetes, also make the host's kubectl context (k3s-<name>) current.
With no name, print the current host.`,
	Args:              cobra.MaximumNArgs(1),
	ValidArgsFunction: completeHostNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return showCurrent()
		}
		return use(args[0])
	},
}

// dockerHostNames lists the VMs that use the docker image, sorted.
func dockerHostNames() []string {
	dir, err := vm.VMsDir()
	if err != nil {
		return nil
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !vm.Exists(e.Name()) {
			continue
		}
		if cfg, err := vm.ReadConfig(e.Name()); err == nil && cfg.Image == vm.ImageDocker {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func completeHostNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return dockerHostNames(), cobra.ShellCompDirectiveNoFileComp
}

// hostOfContext returns the host name behind a docker context, or "" if the
// context isn't one of monolith's.
func hostOfContext(ctx string) string {
	return strings.TrimPrefix(ctx, "docker-")
}

func showCurrent() error {
	cur := currentContext()
	if !strings.HasPrefix(cur, "docker-") || !vm.Exists(hostOfContext(cur)) {
		if cur == "" {
			cur = "default"
		}
		fmt.Printf("Not using a monolith Docker host (context %q).\n", cur)
		return nil
	}
	fmt.Println(hostOfContext(cur))
	return nil
}

func use(name string) error {
	if err := clivm.ValidateName(name); err != nil {
		return err
	}
	if err := checkEngine(name, vm.EngineDocker); err != nil {
		return err
	}
	flagName = name

	status, err := vm.ReadState(name)
	state := vm.StatusStopped
	if err == nil {
		state = status.State
	}
	if flagUseStart && state != vm.StatusRunning {
		flagEngine = vm.EngineDocker
		if state == vm.StatusSuspended {
			err = resume()
		} else {
			err = start(startCmd)
		}
		if err != nil {
			return err
		}
		state = vm.StatusRunning
	}

	sock, err := vm.VMDockerSockPath(name)
	if err != nil {
		return err
	}
	if err := useContext(name, sock); err != nil {
		if err == errNoDockerCLI {
			fmt.Printf("%v; socket: %s\n", err, sock)
			return nil
		}
		return err
	}
	fmt.Printf("Using Docker host %q (context %s).\n", name, contextName(name))
	if state != vm.StatusRunning {
		fmt.Printf("It is %s: run 'monolith docker start --name %s' (or 'docker resume') to bring it up.\n", displayState(state), name)
	}
	if host := os.Getenv("DOCKER_HOST"); host != "" {
		fmt.Printf("Warning: DOCKER_HOST=%s is set in this shell and overrides the context; run 'unset DOCKER_HOST'.\n", host)
	}

	if flagUseKube {
		cfg, err := vm.ReadConfig(name)
		if err != nil {
			return err
		}
		if !cfg.Kubernetes {
			return fmt.Errorf("Docker host %q has no Kubernetes; start it with 'monolith docker start --kubernetes --name %s'", name, name)
		}
		if err := setKubeContext(name); err != nil {
			return err
		}
		fmt.Printf("kubectl context %s is current.\n", kubeContextName(name))
	}
	return nil
}

func displayState(s string) string {
	if s == vm.StatusSuspended {
		return "paused (on disk)"
	}
	return s
}

// setKubeContext makes the VM's kubectl context current, if it is in the
// user's kubeconfig.
func setKubeContext(vmName string) error {
	path, err := kubeconfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), kubeContextName(vmName)) {
		return fmt.Errorf("no kubectl context %s; run 'monolith docker start --name %s' to create it", kubeContextName(vmName), vmName)
	}
	return editKubeconfig(func(doc map[string]any) {
		doc["current-context"] = kubeContextName(vmName)
	})
}

func init() {
	useCmd.Flags().BoolVar(&flagUseStart, "start", false, "start or resume the host first if it isn't running")
	useCmd.Flags().BoolVar(&flagUseKube, "kubernetes", false, "also make the host's kubectl context current")
}

func init() {
	listCmd.Flags().BoolVarP(&flagListQuiet, "quiet", "q", false, "print only host names")
}

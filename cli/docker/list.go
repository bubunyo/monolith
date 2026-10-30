package docker

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"

	clivm "github.com/bubunyo/monolith/cli/vm"
	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List Docker host VMs",
	Long: `List Docker host VMs with their state, engine, docker context (* = current) and the
container ports forwarded to localhost. A port marked "!" is published in the VM
but couldn't be forwarded, usually because another VM already uses it.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return list()
	},
}

func list() error {
	dir, err := vm.VMsDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

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
	if len(names) == 0 {
		fmt.Println("No Docker hosts. Create one with 'monolith docker start'.")
		return nil
	}

	current := currentContext()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tENGINE\tK8S\tCONTEXT\tCPUS\tMEM\tDATA\tPORTS")
	for _, name := range names {
		cfg, _ := vm.ReadConfig(name)
		state := vm.StatusStopped
		if status, err := vm.ReadState(name); err == nil {
			state = status.State
		}
		if state == vm.StatusSuspended {
			state = "paused (on disk)"
		}
		engine := vm.EngineOf(cfg)
		ctx := "-" // containerd hosts have no Docker API, so no context
		if engine == vm.EngineDocker {
			ctx = contextName(name)
			if ctx == current {
				ctx += " *"
			}
		}
		ports := "-"
		if state == vm.StatusRunning {
			ports = forwardedPorts(name)
		}
		k8s := "-"
		if cfg.Kubernetes {
			k8s = vm.K3sVersionOf(cfg)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", name, state, engine, k8s, ctx, cfg.CPUs, clivm.FormatMemory(cfg.Memory), dataUsage(name), ports)
	}
	return w.Flush()
}

// forwardedPorts asks the VM daemon for its published ports.
func forwardedPorts(name string) string {
	resp, err := vm.SendControlCommand(name, vm.ControlRequest{Cmd: vm.CmdStatus})
	if err != nil || !resp.OK {
		return "?"
	}
	if len(resp.Ports) == 0 {
		return "-"
	}
	var parts []string
	for _, p := range resp.Ports {
		s := strconv.Itoa(p.Port)
		if p.Error != "" {
			s += "!"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

// dataUsage is the space the VM's sparse docker data disk actually uses.
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

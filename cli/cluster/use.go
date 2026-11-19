package cluster

import (
	"errors"
	"fmt"

	clivm "github.com/bubunyo/monolith/cli/vm"
	"github.com/bubunyo/monolith/internal/kubeconfig"
	"github.com/bubunyo/monolith/vm"
)

// clusterOfContext returns the cluster VM behind a kubectl context, or ""
// if the context isn't a monolith cluster's.
func clusterOfContext(ctx string) string {
	names, _ := clusterNames()
	for _, n := range names {
		if kubeconfig.ContextName(n) == ctx {
			return n
		}
	}
	return ""
}

func showCurrent() error {
	cur := kubeconfig.Current()
	if name := clusterOfContext(cur); name != "" {
		fmt.Println(name)
		return nil
	}
	if cur == "" {
		fmt.Println("No kubectl context is current.")
	} else {
		fmt.Printf("Not using a monolith cluster (kubectl context %q).\n", cur)
	}
	return nil
}

func use(name string) error {
	if err := clivm.ValidateName(name); err != nil {
		return err
	}
	if err := checkCluster(name); err != nil {
		return err
	}
	flagName = name

	state := vm.StatusStopped
	if status, err := vm.ReadState(name); err == nil {
		state = status.State
	}
	if flagStart && state != vm.StatusRunning {
		var err error
		if state == vm.StatusSuspended {
			err = resume()
		} else {
			err = start(startCmd) // merges the context and makes it current
		}
		return err
	}

	prev, err := prevContextPath(name)
	if err != nil {
		return err
	}
	switch err := kubeconfig.Use(name, prev); {
	case errors.Is(err, kubeconfig.ErrNoContext):
		return fmt.Errorf("kubectl context %s isn't in your kubeconfig; 'monolith cluster start --name %s' adds it", kubeconfig.ContextName(name), name)
	case err != nil:
		return err
	}
	fmt.Printf("Using cluster %q (kubectl context %s).\n", name, kubeconfig.ContextName(name))
	if state != vm.StatusRunning {
		shown := state
		if state == vm.StatusSuspended {
			shown = "paused (on disk)"
		}
		fmt.Printf("It is %s: run 'monolith cluster start --name %s' (or 'cluster resume') to bring it up.\n", shown, name)
	}
	return nil
}

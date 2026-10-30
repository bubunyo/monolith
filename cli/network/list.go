package network

import (
	"fmt"
	"os"
	"text/tabwriter"

	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var flagTopology bool

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all named networks",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagTopology {
			td, err := collectTopology()
			if err != nil {
				return err
			}
			return printTopology(os.Stdout, td)
		}
		return listNetworks()
	},
}

func init() {
	listCmd.Flags().BoolVarP(&flagTopology, "topology", "t", false, "render a tree of all VMs grouped by reachability and network")
}

// listNetworks lists all named networks with their status and attached VMs.
func listNetworks() error {
	dir, err := mnet.NetworksDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No networks found.")
			return nil
		}
		return err
	}

	type netInfo struct {
		name   string
		mode   string
		subnet string
		state  string
		vms    []string
	}
	var networks []netInfo

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()

		cfg, err := mnet.ReadNetworkConfig(name)
		if err != nil {
			continue
		}

		state := "stopped"
		if mnet.IsNetworkDaemonAlive(name) {
			state = "running"
		}

		vms := findVMsOnNetwork(name)

		networks = append(networks, netInfo{
			name:   name,
			mode:   cfg.Mode,
			subnet: cfg.Subnet,
			state:  state,
			vms:    vms,
		})
	}

	if len(networks) == 0 {
		fmt.Println("No networks found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tMODE\tSUBNET\tSTATE\tVMS")

	for _, n := range networks {
		vmList := "-"
		if len(n.vms) > 0 {
			vmList = ""
			for i, vm := range n.vms {
				if i > 0 {
					vmList += ","
				}
				vmList += vm
			}
		}
		subnet := n.subnet
		if subnet == "" {
			subnet = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			n.name, n.mode, subnet, n.state, vmList,
		)
	}

	return w.Flush()
}

// findVMsOnNetwork scans all VMs and returns the names of those
// whose config references the given network name.
func findVMsOnNetwork(networkName string) []string {
	vmsDir, err := vm.VMsDir()
	if err != nil {
		return nil
	}

	entries, err := os.ReadDir(vmsDir)
	if err != nil {
		return nil
	}

	var result []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cfg, err := vm.ReadConfig(e.Name())
		if err != nil {
			continue
		}
		if cfg.NetworkName == networkName {
			result = append(result, e.Name())
		}
	}
	return result
}

package network

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/vm"
)

// vmRow describes a single VM as it appears in the topology tree.
type vmRow struct {
	name      string
	state     string
	ip        string
	mode      string
	hostIface string
	netName   string
}

// namedNet describes a named network and the VMs attached to it.
type namedNet struct {
	name   string
	mode   string
	subnet string
	state  string
	vms    []vmRow
}

// topologyData is the structured snapshot rendered by printTopology.
type topologyData struct {
	bridgedByIface map[string][]vmRow // host mode VMs grouped by --host-interface
	natVMs         []vmRow            // mode=nat VMs
	namedShared    []namedNet         // shared, named (always shown)
	anonShared     []vmRow            // shared, no --network
	namedIsolated  []namedNet         // isolated, named
	anonIsolated   []vmRow            // isolated, no --network
	detached       []vmRow            // mode=none
}

// collectTopology scans VM and network directories and classifies each
// entity into the topology buckets.
func collectTopology() (topologyData, error) {
	td := topologyData{
		bridgedByIface: map[string][]vmRow{},
	}

	// Walk named networks first so we can attach VMs to them by name.
	netsDir, err := mnet.NetworksDir()
	if err != nil {
		return td, err
	}
	netByName := map[string]*namedNet{}
	if entries, err := os.ReadDir(netsDir); err == nil {
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
			n := &namedNet{
				name:   name,
				mode:   cfg.Mode,
				subnet: cfg.Subnet,
				state:  state,
			}
			netByName[name] = n
			switch cfg.Mode {
			case mnet.NetworkShared:
				td.namedShared = append(td.namedShared, namedNet{}) // placeholder, replaced below
			case mnet.NetworkIsolated:
				td.namedIsolated = append(td.namedIsolated, namedNet{})
			}
		}
	}
	// Re-collect named networks from the map so we keep pointers stable
	// while attaching VMs, then materialise them ordered alphabetically.
	td.namedShared = td.namedShared[:0]
	td.namedIsolated = td.namedIsolated[:0]

	// Walk VMs and classify each.
	vmsDir, err := vm.VMsDir()
	if err != nil {
		return td, err
	}
	if entries, err := os.ReadDir(vmsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			cfg, err := vm.ReadConfig(name)
			if err != nil {
				continue
			}
			st, err := vm.ReadState(name)
			if err != nil || st == nil {
				st = &vm.VMStatus{State: vm.StatusStopped}
			}
			row := vmRow{
				name:      name,
				state:     st.State,
				ip:        st.IP,
				mode:      cfg.NetworkMode,
				hostIface: cfg.HostInterface,
				netName:   cfg.NetworkName,
			}

			switch cfg.NetworkMode {
			case mnet.NetworkHost:
				iface := cfg.HostInterface
				if iface == "" {
					iface = "(none)"
				}
				td.bridgedByIface[iface] = append(td.bridgedByIface[iface], row)
			case mnet.NetworkNAT:
				td.natVMs = append(td.natVMs, row)
			case mnet.NetworkShared:
				if cfg.NetworkName == "" {
					td.anonShared = append(td.anonShared, row)
				} else if n, ok := netByName[cfg.NetworkName]; ok {
					n.vms = append(n.vms, row)
				} else {
					// Referenced network is missing; surface it as a
					// pseudo-network so the VM is visible.
					missing := &namedNet{
						name:   "(missing: " + cfg.NetworkName + ")",
						mode:   mnet.NetworkShared,
						subnet: "",
						state:  "missing",
					}
					missing.vms = append(missing.vms, row)
					netByName["__missing_shared_"+cfg.NetworkName] = missing
				}
			case mnet.NetworkIsolated:
				if cfg.NetworkName == "" {
					td.anonIsolated = append(td.anonIsolated, row)
				} else if n, ok := netByName[cfg.NetworkName]; ok {
					n.vms = append(n.vms, row)
				} else {
					missing := &namedNet{
						name:   "(missing: " + cfg.NetworkName + ")",
						mode:   mnet.NetworkIsolated,
						subnet: "",
						state:  "missing",
					}
					missing.vms = append(missing.vms, row)
					netByName["__missing_isolated_"+cfg.NetworkName] = missing
				}
			case mnet.NetworkNone, "":
				td.detached = append(td.detached, row)
			}
		}
	}

	// Materialise named networks into the topology, sorted alphabetically.
	// Real networks first, then "(missing: ...)" pseudo-networks.
	var sharedReal, sharedMissing, isoReal, isoMissing []*namedNet
	for k, n := range netByName {
		isMissing := strings.HasPrefix(k, "__missing_")
		switch n.mode {
		case mnet.NetworkShared:
			if isMissing {
				sharedMissing = append(sharedMissing, n)
			} else {
				sharedReal = append(sharedReal, n)
			}
		case mnet.NetworkIsolated:
			if isMissing {
				isoMissing = append(isoMissing, n)
			} else {
				isoReal = append(isoReal, n)
			}
		}
	}
	sortByName := func(s []*namedNet) {
		sort.Slice(s, func(i, j int) bool { return s[i].name < s[j].name })
	}
	sortByName(sharedReal)
	sortByName(sharedMissing)
	sortByName(isoReal)
	sortByName(isoMissing)
	for _, n := range sharedReal {
		sortVMs(n.vms)
		td.namedShared = append(td.namedShared, *n)
	}
	for _, n := range sharedMissing {
		sortVMs(n.vms)
		td.namedShared = append(td.namedShared, *n)
	}
	for _, n := range isoReal {
		sortVMs(n.vms)
		td.namedIsolated = append(td.namedIsolated, *n)
	}
	for _, n := range isoMissing {
		sortVMs(n.vms)
		td.namedIsolated = append(td.namedIsolated, *n)
	}

	sortVMs(td.natVMs)
	sortVMs(td.anonShared)
	sortVMs(td.anonIsolated)
	sortVMs(td.detached)
	for k := range td.bridgedByIface {
		sortVMs(td.bridgedByIface[k])
	}

	return td, nil
}

func sortVMs(rows []vmRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
}

// topoRenderer holds the writer and styles used while rendering.
type topoRenderer struct {
	w io.Writer
	s styles
}

// printTopology renders the topology data to w as a Unicode tree.
func printTopology(w io.Writer, td topologyData) error {
	r := topoRenderer{w: w, s: newStyles(w)}
	return r.render(td)
}

func (r topoRenderer) render(td topologyData) error {
	hasInternet := len(td.bridgedByIface) > 0 || len(td.natVMs) > 0 ||
		len(td.namedShared) > 0 || len(td.anonShared) > 0
	hasHostOnly := len(td.namedIsolated) > 0 || len(td.anonIsolated) > 0
	hasAirgapped := len(td.detached) > 0

	if !hasInternet && !hasHostOnly && !hasAirgapped {
		fmt.Fprintln(r.w, "No VMs or networks found.")
		return nil
	}

	sections := 0
	if hasInternet {
		sections++
	}
	if hasHostOnly {
		sections++
	}
	if hasAirgapped {
		sections++
	}

	written := 0
	if hasInternet {
		r.printInternetSection(td)
		written++
		if written < sections {
			fmt.Fprintln(r.w)
		}
	}
	if hasHostOnly {
		r.printHostOnlySection(td)
		written++
		if written < sections {
			fmt.Fprintln(r.w)
		}
	}
	if hasAirgapped {
		r.printAirgappedSection(td)
	}
	return nil
}

// Box-drawing characters used for tree connectors.
const (
	branch    = "├── "
	lastBr    = "└── "
	indentBar = "│   "
	indent    = "    "
)

// padRight returns s padded with spaces to width w.
func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// dashIfEmpty returns "-" when s is empty.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatVMLine returns a styled "name<pad>  state<pad>  ip" given column widths.
// Padding is computed against raw lengths, then each cell is colorised.
func (r topoRenderer) formatVMLine(row vmRow, nameW, stateW int) string {
	name := r.s.vmName.Render(padRight(row.name, nameW))
	state := r.s.stateStyle(row.state).Render(padRight(row.state, stateW))
	ip := r.s.addr.Render(dashIfEmpty(row.ip))
	return fmt.Sprintf("%s   %s   %s", name, state, ip)
}

// formatNetLine returns a styled "name<pad>  state<pad>  subnet".
func (r topoRenderer) formatNetLine(n namedNet, nameW, stateW int) string {
	nameStyle := r.s.netName
	if n.state == "missing" {
		nameStyle = r.s.pseudo
	}
	name := nameStyle.Render(padRight(n.name, nameW))
	state := r.s.stateStyle(n.state).Render(padRight(n.state, stateW))
	subnet := r.s.subnet.Render(dashIfEmpty(n.subnet))
	return fmt.Sprintf("%s   %s   %s", name, state, subnet)
}

// vmColumnWidths returns the max name and state widths over a slice of VMs.
func vmColumnWidths(rows []vmRow) (nameW, stateW int) {
	for _, r := range rows {
		if len(r.name) > nameW {
			nameW = len(r.name)
		}
		if len(r.state) > stateW {
			stateW = len(r.state)
		}
	}
	return
}

// netColumnWidths returns the max name and state widths over a slice of nets.
func netColumnWidths(nets []namedNet) (nameW, stateW int) {
	for _, n := range nets {
		if len(n.name) > nameW {
			nameW = len(n.name)
		}
		if len(n.state) > stateW {
			stateW = len(n.state)
		}
	}
	return
}

// printInternetSection writes the Internet bucket: Bridged + NAT + Shared.
func (r topoRenderer) printInternetSection(t topologyData) {
	fmt.Fprintln(r.w, r.s.internet.Render("Internet"))

	// Determine which sub-sections are present, in fixed order.
	type sub struct {
		name string
		do   func(prefix string)
	}
	var subs []sub
	if len(t.bridgedByIface) > 0 {
		subs = append(subs, sub{"Bridged (physical NICs)", func(prefix string) {
			ifaces := make([]string, 0, len(t.bridgedByIface))
			for k := range t.bridgedByIface {
				ifaces = append(ifaces, k)
			}
			sort.Strings(ifaces)
			for i, iface := range ifaces {
				rows := t.bridgedByIface[iface]
				ifaceLast := i == len(ifaces)-1
				conn := branch
				if ifaceLast {
					conn = lastBr
				}
				fmt.Fprintf(r.w, "%s%s%s\n",
					r.s.tree.Render(prefix),
					r.s.tree.Render(conn),
					r.s.iface.Render(iface),
				)
				childPrefix := prefix
				if ifaceLast {
					childPrefix += indent
				} else {
					childPrefix += indentBar
				}
				r.printVMRows(rows, childPrefix)
			}
		}})
	}
	if len(t.natVMs) > 0 {
		subs = append(subs, sub{"NAT", func(prefix string) {
			r.printVMRows(t.natVMs, prefix)
		}})
	}
	if len(t.namedShared) > 0 || len(t.anonShared) > 0 {
		subs = append(subs, sub{"Shared networks", func(prefix string) {
			r.printSharedOrIsolated(t.namedShared, t.anonShared, prefix)
		}})
	}

	for i, s := range subs {
		isLast := i == len(subs)-1
		conn := branch
		childPrefix := indentBar
		if isLast {
			conn = lastBr
			childPrefix = indent
		}
		fmt.Fprintf(r.w, "%s%s\n",
			r.s.tree.Render(conn),
			r.s.subSection.Render(s.name),
		)
		s.do(childPrefix)
	}
}

func (r topoRenderer) printHostOnlySection(t topologyData) {
	fmt.Fprintln(r.w, r.s.hostOnly.Render("Host-only"))
	// Only one sub-section: Isolated networks.
	fmt.Fprintf(r.w, "%s%s\n",
		r.s.tree.Render(lastBr),
		r.s.subSection.Render("Isolated networks"),
	)
	r.printSharedOrIsolated(t.namedIsolated, t.anonIsolated, indent)
}

func (r topoRenderer) printAirgappedSection(t topologyData) {
	fmt.Fprintln(r.w, r.s.airgapped.Render("Airgapped"))
	r.printVMRows(t.detached, "")
}

// printVMRows renders a flat list of VMs as siblings under prefix.
// Column widths are computed across this list so columns line up.
func (r topoRenderer) printVMRows(rows []vmRow, prefix string) {
	if len(rows) == 0 {
		return
	}
	nameW, stateW := vmColumnWidths(rows)
	for i, row := range rows {
		conn := branch
		if i == len(rows)-1 {
			conn = lastBr
		}
		fmt.Fprintf(r.w, "%s%s%s\n",
			r.s.tree.Render(prefix),
			r.s.tree.Render(conn),
			r.formatVMLine(row, nameW, stateW),
		)
	}
}

// printSharedOrIsolated renders named networks (each with their VM children)
// followed by a "(anonymous)" pseudo-group containing standalone VMs.
func (r topoRenderer) printSharedOrIsolated(nets []namedNet, anon []vmRow, prefix string) {
	// Compute combined name/state widths across all networks (so the
	// network rows line up), and separately for the (anonymous) group.
	netNameW, netStateW := netColumnWidths(nets)
	if len("(anonymous)") > netNameW && len(anon) > 0 {
		netNameW = len("(anonymous)")
	}

	type entry struct {
		isAnon bool
		net    namedNet
	}
	var entries []entry
	for _, n := range nets {
		entries = append(entries, entry{net: n})
	}
	if len(anon) > 0 {
		entries = append(entries, entry{isAnon: true})
	}

	for i, e := range entries {
		isLast := i == len(entries)-1
		conn := branch
		childPrefix := prefix + indentBar
		if isLast {
			conn = lastBr
			childPrefix = prefix + indent
		}
		if e.isAnon {
			fmt.Fprintf(r.w, "%s%s%s\n",
				r.s.tree.Render(prefix),
				r.s.tree.Render(conn),
				r.s.pseudo.Render("(anonymous)"),
			)
			r.printVMRows(anon, childPrefix)
			continue
		}
		fmt.Fprintf(r.w, "%s%s%s\n",
			r.s.tree.Render(prefix),
			r.s.tree.Render(conn),
			r.formatNetLine(e.net, netNameW, netStateW),
		)
		r.printVMRows(e.net.vms, childPrefix)
	}
}

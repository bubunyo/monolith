package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
)

var flagWide bool

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all VMs",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return listVMs(flagWide)
	},
}

// listVMs lists all VMs with their current status, config, and resource usage.
func listVMs(wide bool) error {
	vmsPath, err := vm.VMsDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(vmsPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No VMs found.")
			return nil
		}
		return err
	}

	// Collect every VM directory (see vm.Exists).
	type vmInfo struct {
		name        string
		logicalSize int64
		actualSize  int64
		created     time.Time
		state       string
		ip          string
		cpus        uint
		memory      uint64 // bytes
		network     string
		image       string
		networkName string
		uptime      time.Duration
		cpuPercent  float64
		memoryRSS   int64 // bytes
	}
	var vms []vmInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !vm.Exists(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}

		// Alpine VMs have a per-VM disk; Kata VMs share a read-only base
		// image and have none. Actual usage via syscall (sparse-aware).
		var logical, actual int64
		diskPath := filepath.Join(vmsPath, e.Name(), "disk.img")
		if di, err := os.Stat(diskPath); err == nil {
			logical = di.Size()
			var st syscall.Stat_t
			if err := syscall.Stat(diskPath, &st); err == nil {
				actual = st.Blocks * 512
			}
		}

		// Read runtime state (running/stopped + IP + stats).
		status, err := vm.ReadState(e.Name())
		if err != nil {
			status = &vm.VMStatus{State: vm.StatusStopped}
		}

		// Read per-VM hardware config.
		cfg, _ := vm.ReadConfig(e.Name())

		var uptime time.Duration
		if (status.State == vm.StatusRunning || status.State == vm.StatusPaused) && !status.StartedAt.IsZero() {
			uptime = time.Since(status.StartedAt)
		}

		vms = append(vms, vmInfo{
			name:        e.Name(),
			logicalSize: logical,
			actualSize:  actual,
			created:     info.ModTime(),
			state:       status.State,
			ip:          status.IP,
			cpus:        cfg.CPUs,
			memory:      cfg.Memory,
			network:     cfg.NetworkMode,
			networkName: cfg.NetworkName,
			image:       imageLabel(cfg),
			uptime:      uptime,
			cpuPercent:  status.CPUPercent,
			memoryRSS:   status.MemoryRSS,
		})
	}

	if len(vms) == 0 {
		fmt.Println("No VMs found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

	if wide {
		fmt.Fprintln(w, "NAME\tSTATE\tIMAGE\tIP\tNET\tCPUS\tMEM\tDISK\tUPTIME\tCPU%\tRSS")
	} else {
		fmt.Fprintln(w, "NAME\tSTATE\tIMAGE\tIP\tNET\tCPUS\tMEM\tDISK\tUPTIME")
	}

	for _, row := range vms {
		ip := dash(row.ip)
		uptime := dash("")
		if (row.state == vm.StatusRunning || row.state == vm.StatusPaused) && row.uptime > 0 {
			uptime = formatUptime(row.uptime)
		}

		disk := dash("")
		if row.logicalSize > 0 {
			disk = formatDisk(row.actualSize, row.logicalSize)
		}
		net := dash(row.network)
		if row.networkName != "" {
			net = fmt.Sprintf("%s:%s", row.network, row.networkName)
		}

		if wide {
			cpuPct := dash("")
			rss := dash("")
			if row.state == vm.StatusRunning || row.state == vm.StatusPaused {
				cpuPct = fmt.Sprintf("%.1f%%", row.cpuPercent)
				rss = formatSize(row.memoryRSS)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				row.name,
				row.state,
				row.image,
				ip,
				net,
				row.cpus,
				formatSize(int64(row.memory)),
				disk,
				uptime,
				cpuPct,
				rss,
			)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				row.name,
				row.state,
				row.image,
				ip,
				net,
				row.cpus,
				formatSize(int64(row.memory)),
				disk,
				uptime,
			)
		}
	}
	return w.Flush()
}

func init() {
	listCmd.Flags().BoolVarP(&flagWide, "wide", "w", false, "show additional columns (CPU%, RSS)")
}

// dash returns "-" if s is empty, otherwise returns s.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func formatSize(bytes int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case bytes >= gib:
		if bytes%gib == 0 {
			return fmt.Sprintf("%d GiB", bytes/gib)
		}
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		if bytes%mib == 0 {
			return fmt.Sprintf("%d MiB", bytes/mib)
		}
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	case bytes >= kib:
		if bytes%kib == 0 {
			return fmt.Sprintf("%d KiB", bytes/kib)
		}
		return fmt.Sprintf("%.1f KiB", float64(bytes)/float64(kib))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatDisk shows actual / logical disk usage for sparse files.
func formatDisk(actual, logical int64) string {
	return fmt.Sprintf("%s / %s", formatSize(actual), formatSize(logical))
}

// formatUptime converts a duration to a compact human-readable string.
func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	if h == 0 {
		return fmt.Sprintf("%dd", days)
	}
	return fmt.Sprintf("%dd%dh", days, h)
}

// imageLabel is the IMAGE column: the base image, with the container engine
// in brackets for docker image VMs, e.g. "docker (containerd)".
func imageLabel(cfg vm.VMConfig) string {
	if cfg.Image == vm.ImageDocker {
		return fmt.Sprintf("%s (%s)", cfg.Image, vm.EngineOf(cfg))
	}
	return cfg.Image
}

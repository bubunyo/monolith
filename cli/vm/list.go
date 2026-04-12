package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bubunyo/monolith"
	"github.com/spf13/cobra"
)

var flagWide bool

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all VMs",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		vmsDir, err := vmsDir()
		if err != nil {
			return err
		}

		entries, err := os.ReadDir(vmsDir)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("No VMs found.")
				return nil
			}
			return err
		}

		// Filter to directories that contain a disk.img.
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
			uptime      time.Duration
			cpuPercent  float64
			memoryRSS   int64 // bytes
		}
		var vms []vmInfo
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			diskPath := filepath.Join(vmsDir, e.Name(), "disk.img")
			info, err := os.Stat(diskPath)
			if err != nil {
				continue // no disk.img, skip
			}

			// Get actual disk usage via syscall (sparse file awareness).
			var actual int64
			var st syscall.Stat_t
			if err := syscall.Stat(diskPath, &st); err == nil {
				actual = st.Blocks * 512
			}

			// Read runtime state (running/stopped + IP + stats).
			status, err := monolith.ReadState(e.Name())
			if err != nil {
				status = &monolith.VMStatus{State: "stopped"}
			}

			// Read per-VM hardware config.
			cfg, _ := monolith.ReadConfig(e.Name())

			var uptime time.Duration
			if status.State == "running" && !status.StartedAt.IsZero() {
				uptime = time.Since(status.StartedAt)
			}

			vms = append(vms, vmInfo{
				name:        e.Name(),
				logicalSize: info.Size(),
				actualSize:  actual,
				created:     info.ModTime(),
				state:       status.State,
				ip:          status.IP,
				cpus:        cfg.CPUs,
				memory:      cfg.Memory,
				network:     cfg.Network,
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

		if flagWide {
			fmt.Fprintln(w, "NAME\tSTATE\tIP\tNET\tCPUS\tMEM\tDISK\tUPTIME\tCPU%\tRSS\tOS")
		} else {
			fmt.Fprintln(w, "NAME\tSTATE\tIP\tNET\tCPUS\tMEM\tDISK\tUPTIME")
		}

		for _, vm := range vms {
			ip := dash(vm.ip)
			uptime := dash("")
			if vm.state == "running" && vm.uptime > 0 {
				uptime = formatUptime(vm.uptime)
			}

			disk := formatDisk(vm.actualSize, vm.logicalSize)
			net := dash(vm.network)

			if flagWide {
				cpuPct := dash("")
				rss := dash("")
				if vm.state == "running" {
					cpuPct = fmt.Sprintf("%.1f%%", vm.cpuPercent)
					rss = formatSize(vm.memoryRSS)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					vm.name,
					vm.state,
					ip,
					net,
					vm.cpus,
					formatSize(int64(vm.memory)),
					disk,
					uptime,
					cpuPct,
					rss,
					"alpine",
				)
			} else {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
					vm.name,
					vm.state,
					ip,
					net,
					vm.cpus,
					formatSize(int64(vm.memory)),
					disk,
					uptime,
				)
			}
		}
		return w.Flush()
	},
}

func init() {
	listCmd.Flags().BoolVarP(&flagWide, "wide", "w", false, "show additional columns (CPU%, RSS, OS)")
}

// vmsDir returns the path to the VMs directory.
func vmsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "monlith", "vms"), nil
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

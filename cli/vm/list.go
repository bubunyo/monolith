package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

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
			name    string
			size    int64
			created time.Time
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
			vms = append(vms, vmInfo{
				name:    e.Name(),
				size:    info.Size(),
				created: info.ModTime(),
			})
		}

		if len(vms) == 0 {
			fmt.Println("No VMs found.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tDISK\tCREATED\tOS")
		for _, vm := range vms {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
				vm.name,
				formatSize(vm.size),
				vm.created.Format("2006-01-02 15:04"),
				"alpine",
			)
		}
		return w.Flush()
	},
}

// vmsDir returns the path to the VMs directory.
func vmsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "monlith", "vms"), nil
}

func formatSize(bytes int64) string {
	const (
		mib = 1024 * 1024
		gib = 1024 * mib
	)
	switch {
	case bytes >= gib:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(gib))
	case bytes >= mib:
		return fmt.Sprintf("%.1f MiB", float64(bytes)/float64(mib))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

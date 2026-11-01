package container

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bubunyo/monolith"
	"github.com/bubunyo/monolith/oci"
	"github.com/bubunyo/monolith/vm"
	"github.com/spf13/cobra"
)

var (
	flagPruneForce bool
	flagPruneAll   bool
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove all stopped containers",
	Long: `Remove every container that isn't running (exited, or created and never
started). Running and paused containers are left alone. The size shown is what
each container wrote on top of its image: the space removing it gives back.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		containers, err := list()
		if err != nil {
			return err
		}
		stale := stoppedContainers(containers)
		if len(stale) == 0 {
			fmt.Println("Nothing to prune.")
			return nil
		}
		var total int64
		sizes := make([]int64, len(stale))
		fmt.Println("The following stopped containers will be removed:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		for i, c := range stale {
			sizes[i] = containerSize(c.name)
			total += sizes[i]
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.name, c.cfg.Container.Image, c.statusText(), oci.HumanSize(sizes[i]))
		}
		w.Flush()
		if !flagPruneForce && !confirm(os.Stdin, fmt.Sprintf("Remove %d %s (%s)?", len(stale), plural(len(stale), "container"), oci.HumanSize(total))) {
			fmt.Println("Cancelled.")
			return nil
		}
		var reclaimed int64
		var failed int
		for i, c := range stale {
			if err := removeDir(c.name); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing %s: %v\n", c.name, err)
				failed++
				continue
			}
			reclaimed += sizes[i]
			fmt.Printf("Deleted %s\n", c.name)
		}
		fmt.Printf("Reclaimed %s\n", oci.HumanSize(reclaimed))
		if failed > 0 {
			return fmt.Errorf("%d of %d failed", failed, len(stale))
		}
		return nil
	},
}

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Manage images",
}

var imagePruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove unused images",
	Long: `Remove images that have no tags left (for example an older version whose tag
moved to a newer pull). With --all, remove every image no container was created
from, stopped containers included. Cached layers no remaining image needs go
too. The size shown is the space removing them gives back.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := oci.Open()
		if err != nil {
			return err
		}
		images, err := store.List()
		if err != nil {
			return err
		}
		containers, err := list()
		if err != nil {
			return err
		}
		unused := unusedImages(images, containers, flagPruneAll)
		if len(unused) == 0 {
			fmt.Println("Nothing to prune.")
			return nil
		}
		ids := make([]string, len(unused))
		fmt.Println("The following images will be removed:")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		for i, img := range unused {
			ids[i] = img.ID
			fmt.Fprintf(w, "  %s\t%s\t%s\n", imageName(img), img.ShortID(), oci.HumanSize(img.DiskBytes))
		}
		w.Flush()
		estimate := store.Reclaimable(ids)
		if !flagPruneForce && !confirm(os.Stdin, fmt.Sprintf("Remove %d %s (%s)?", len(unused), plural(len(unused), "image"), oci.HumanSize(estimate))) {
			fmt.Println("Cancelled.")
			return nil
		}
		var reclaimed int64
		var failed int
		for _, img := range unused {
			r, err := store.Remove(img.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error removing %s: %v\n", imageName(img), err)
				failed++
				continue
			}
			reclaimed += r.Freed
			fmt.Printf("Deleted %s (%s)\n", imageName(img), img.ShortID())
		}
		fmt.Printf("Reclaimed %s\n", oci.HumanSize(reclaimed))
		if failed > 0 {
			return fmt.Errorf("%d of %d failed", failed, len(unused))
		}
		return nil
	},
}

// stoppedContainers are the containers prune removes: not running or
// paused, and not suspended to disk.
func stoppedContainers(all []info) []info {
	var out []info
	for _, c := range all {
		if !c.running() && c.status.State != vm.StatusSuspended {
			out = append(out, c)
		}
	}
	return out
}

// unusedImages are the images image prune removes: untagged ones, or with
// all, every image no container was created from.
func unusedImages(images []oci.Image, containers []info, all bool) []oci.Image {
	used := map[string]bool{}
	for _, c := range containers {
		used[c.cfg.Container.ImageID] = true
	}
	var out []oci.Image
	for _, img := range images {
		if used[img.ID] {
			continue
		}
		if all || len(img.Refs) == 0 {
			out = append(out, img)
		}
	}
	return out
}

// containerSize is what removing a container gives back: what its disk
// holds beyond the image it was cloned from, plus its output log.
func containerSize(name string) int64 {
	var n int64
	if disk, err := vm.VMContainerDiskPath(name); err == nil {
		if p, err := monolith.PrivateSize(disk); err == nil {
			n += p
		}
	}
	if log, err := vm.VMConsoleLogPath(name); err == nil {
		if fi, err := os.Stat(log); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func imageName(img oci.Image) string {
	if len(img.Refs) > 0 {
		return oci.Display(img.Refs[0])
	}
	return "<none>"
}

// confirm asks a yes/no question on stdout and reads the answer from in.
func confirm(in io.Reader, question string) bool {
	fmt.Printf("%s [y/N] ", question)
	answer, _ := bufio.NewReader(in).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func init() {
	pruneCmd.Flags().BoolVarP(&flagPruneForce, "force", "f", false, "don't ask for confirmation")
	imagePruneCmd.Flags().BoolVarP(&flagPruneForce, "force", "f", false, "don't ask for confirmation")
	imagePruneCmd.Flags().BoolVarP(&flagPruneAll, "all", "a", false, "remove every image no container uses, not just untagged ones")
	imageCmd.AddCommand(imagePruneCmd)
}

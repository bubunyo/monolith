package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/bubunyo/monolith/oci"
	"github.com/spf13/cobra"
)

var pullCmd = &cobra.Command{
	Use:   "pull IMAGE",
	Short: "Pull an image and convert it for container VMs",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := oci.Open()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		_, err = store.Pull(ctx, args[0], oci.PullOptions{Log: os.Stdout, TTY: isTerminal(os.Stdout)})
		return err
	},
}

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "List local images",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := oci.Open()
		if err != nil {
			return err
		}
		images, err := store.List()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "REPOSITORY\tTAG\tIMAGE ID\tPULLED\tSIZE\tON DISK")
		for _, img := range images {
			refs := img.Refs
			if len(refs) == 0 {
				refs = []string{"<none>"}
			}
			for _, ref := range refs {
				repo, tag := splitRef(ref)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\t%s\t%s\n", repo, tag, img.ShortID(), ago(img.Created), oci.HumanSize(img.Size), oci.HumanSize(img.DiskBytes))
			}
		}
		return w.Flush()
	},
}

var rmiCmd = &cobra.Command{
	Use:   "rmi IMAGE...",
	Short: "Remove images (not while a container uses them, unless -f)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := oci.Open()
		if err != nil {
			return err
		}
		containers, err := list()
		if err != nil {
			return err
		}
		var failed int
		for _, ref := range args {
			if err := removeImage(store, containers, ref); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d failed", failed, len(args))
		}
		return nil
	},
}

func removeImage(store *oci.Store, containers []info, ref string) error {
	img, err := store.Lookup(ref)
	if err != nil {
		return err
	}
	// Containers have their own clone of the disk, so removing the image
	// doesn't break them; Docker refuses anyway unless forced.
	if !flagForce {
		var users []string
		for _, c := range containers {
			if c.cfg.Container.ImageID == img.ID {
				users = append(users, c.name)
			}
		}
		if len(users) > 0 && len(img.Refs) <= 1 {
			return fmt.Errorf("image %s is used by %s; remove them or use -f", ref, strings.Join(users, ", "))
		}
	}
	r, err := store.Remove(ref)
	if err != nil {
		if errors.Is(err, oci.ErrNotFound) {
			return fmt.Errorf("no such image: %s", ref)
		}
		return err
	}
	if !r.Deleted {
		fmt.Printf("Untagged: %s\n", oci.Display(mustNormalize(ref)))
		return nil
	}
	for _, ref := range r.Image.Refs {
		fmt.Printf("Untagged: %s\n", oci.Display(ref))
	}
	if r.KeptLayers > 0 {
		layers := "layers"
		if r.KeptLayers == 1 {
			layers = "layer"
		}
		fmt.Printf("Deleted: %s (kept %d %s used by %s)\n", r.Image.ShortID(), r.KeptLayers, layers, strings.Join(r.SharedWith, ", "))
		return nil
	}
	fmt.Printf("Deleted: %s\n", r.Image.ShortID())
	return nil
}

// splitRef splits a normalized reference into the REPOSITORY and TAG
// columns ("nginx", "1.29"); a digest reference has TAG <none>.
func splitRef(ref string) (string, string) {
	ref = oci.Display(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], "<none>"
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

func init() {
	rmiCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "remove even if containers were created from it")
}

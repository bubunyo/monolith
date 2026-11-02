package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bubunyo/monolith/oci"
	"github.com/spf13/cobra"
)

// The image commands below read images on the Mac, read-only: nothing from
// the image runs, so they work on images without a shell (distroless,
// scratch). Images that aren't local are pulled first.

var (
	flagInspectJSON bool
	flagNoTrunc     bool
	flagRecursive   bool
	flagCpForce     bool
)

var imageInspectCmd = &cobra.Command{
	Use:   "inspect IMAGE",
	Short: "Show what an image runs, its settings and layers",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, img, err := localImage(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if flagInspectJSON {
			data, err := json.MarshalIndent(img, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}
		printInspect(os.Stdout, img)
		return nil
	},
}

var imageHistoryCmd = &cobra.Command{
	Use:   "history IMAGE",
	Short: "Show the build steps that made an image, with layer sizes",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		_, img, err := localImage(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if len(img.History) == 0 {
			return fmt.Errorf("%s was pulled before monolith recorded build history; pull it again (rmi, then pull)", args[0])
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "CREATED\tCREATED BY\tSIZE")
		for _, row := range historyRows(img) {
			step := row.step
			if !flagNoTrunc && len([]rune(step)) > 60 {
				step = string([]rune(step)[:59]) + "…"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", row.created, step, row.size)
		}
		return w.Flush()
	},
}

var imageLsCmd = &cobra.Command{
	Use:   "ls IMAGE [PATH]",
	Short: "List files in an image (default /), like ls -l",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ifs, err := openImageFS(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		defer ifs.Close()
		p := "/"
		if len(args) == 2 {
			p = args[1]
		}
		users, groups := ifs.Names()
		l := lister{fs: ifs, users: users, groups: groups, w: os.Stdout}
		return l.list(p, flagRecursive)
	},
}

var imageCatCmd = &cobra.Command{
	Use:   "cat IMAGE PATH",
	Short: "Print a file from an image",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ifs, err := openImageFS(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		defer ifs.Close()
		rc, _, err := ifs.Open(args[1])
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(os.Stdout, rc)
		return err
	},
}

var imageCpCmd = &cobra.Command{
	Use:   "cp IMAGE:PATH DEST",
	Short: "Copy a file or directory out of an image",
	Long: `Copy a file or directory out of an image to the Mac, like cp -R: into DEST if
it's an existing directory, else as DEST. PATH must be absolute
(nginx:1.29:/etc/nginx). Symlinks in the image are followed for PATH itself
and kept as links inside a copied directory. Setuid, setgid and sticky bits are
dropped, and existing files are only replaced with -f.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ref, src, err := splitImagePath(args[0])
		if err != nil {
			return err
		}
		ifs, err := openImageFS(cmd.Context(), ref)
		if err != nil {
			return err
		}
		defer ifs.Close()
		n := 0
		if err := ifs.CopyOut(src, args[1], flagCpForce, func(string) { n++ }); err != nil {
			if errors.Is(err, oci.ErrExists) {
				return err
			}
			return fmt.Errorf("copying %s: %w", src, err)
		}
		fmt.Printf("Copied %s from %s to %s (%d %s)\n", src, ref, args[1], n, plural(n, "item"))
		return nil
	},
}

// localImage finds ref in the store, pulling it if it isn't there.
func localImage(ctx context.Context, ref string) (*oci.Store, oci.Image, error) {
	store, err := oci.Open()
	if err != nil {
		return nil, oci.Image{}, err
	}
	img, err := resolveImage(ctx, ref, "missing")
	return store, img, err
}

func openImageFS(ctx context.Context, ref string) (*oci.ImageFS, error) {
	store, img, err := localImage(ctx, ref)
	if err != nil {
		return nil, err
	}
	return store.OpenFS(img.ID)
}

// splitImagePath splits "IMAGE:/PATH" at the first ":/"; image references
// contain colons themselves (tags, registry ports).
func splitImagePath(s string) (ref, p string, err error) {
	i := strings.Index(s, ":/")
	if i <= 0 {
		return "", "", fmt.Errorf("%q: want IMAGE:/PATH, e.g. nginx:1.29:/etc/nginx", s)
	}
	return s[:i], s[i+1:], nil
}

func printInspect(w io.Writer, img oci.Image) {
	c := img.Config
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", k, v) }
	row("Image", fmt.Sprintf("%s (%s)", imageName(img), img.ShortID()))
	if len(img.Refs) > 1 {
		var others []string
		for _, r := range img.Refs[1:] {
			others = append(others, oci.Display(r))
		}
		row("Also tagged", strings.Join(others, ", "))
	}
	row("Platform", img.Platform)
	if !img.Built.IsZero() {
		row("Built", img.Built.Local().Format("2006-01-02 15:04"))
	}
	row("Pulled", ago(img.Created)+" ago")
	row("Size", fmt.Sprintf("%s compressed, %s on disk", oci.HumanSize(img.Size), oci.HumanSize(img.DiskBytes)))
	row("Entrypoint", jsonList(c.Entrypoint))
	row("Cmd", jsonList(c.Cmd))
	row("Workdir", orDefault(c.WorkingDir, "/"))
	row("User", orDefault(c.User, "root"))
	row("Stop signal", orDefault(c.StopSignal, "SIGTERM"))
	if len(c.ExposedPorts) > 0 {
		var ports []string
		for p := range c.ExposedPorts {
			ports = append(ports, p)
		}
		sort.Strings(ports)
		row("Ports", strings.Join(ports, ", "))
	}
	if len(c.Volumes) > 0 {
		var vols []string
		for v := range c.Volumes {
			vols = append(vols, v)
		}
		sort.Strings(vols)
		row("Volumes", strings.Join(vols, ", "))
	}
	tw.Flush()
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(w, "%s:\n", title)
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	section("Env", c.Env)
	var labels []string
	for k, v := range c.Labels {
		labels = append(labels, k+"="+v)
	}
	sort.Strings(labels)
	section("Labels", labels)
	var layers []string
	for i, d := range img.Layers {
		l := strings.TrimPrefix(d, "sha256:")
		if len(l) > 12 {
			l = l[:12]
		}
		if i < len(img.LayerSizes) {
			l += "  " + oci.HumanSize(img.LayerSizes[i])
		}
		layers = append(layers, l)
	}
	section("Layers", layers)
}

type historyRow struct{ created, step, size string }

// historyRows pairs the build steps with the layers they made, newest
// first like docker history. Steps that made no layer (ENV, CMD...) show
// size 0B.
func historyRows(img oci.Image) []historyRow {
	var rows []historyRow
	layer := 0
	for _, h := range img.History {
		size := "0B"
		if !h.EmptyLayer {
			if layer < len(img.LayerSizes) {
				size = oci.HumanSize(img.LayerSizes[layer])
			}
			layer++
		}
		created := "?"
		if !h.Created.IsZero() {
			created = ago(h.Created.Time) + " ago"
		}
		rows = append([]historyRow{{created: created, step: buildStep(h.CreatedBy), size: size}}, rows...)
	}
	return rows
}

// buildStep tidies a history entry: "/bin/sh -c #(nop)  CMD [...]" becomes
// "CMD [...]", and BuildKit's " # buildkit" suffix goes.
func buildStep(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "#(nop)"); i >= 0 {
		s = strings.TrimSpace(s[i+len("#(nop)"):])
	}
	return strings.TrimSpace(strings.TrimSuffix(s, "# buildkit"))
}

// lister prints entries like ls -l.
type lister struct {
	fs            *oci.ImageFS
	users, groups map[uint32]string
	w             io.Writer
}

func (l lister) list(p string, recursive bool) error {
	e, err := l.fs.Stat(p)
	if err != nil {
		return err
	}
	if !e.IsDir() {
		lt, _ := l.fs.Lstat(p)
		l.print([]oci.Entry{lt})
		return nil
	}
	return l.listDir(e.Path, recursive, false)
}

func (l lister) listDir(p string, recursive, header bool) error {
	entries, err := l.fs.ReadDir(p)
	if err != nil {
		return err
	}
	if header {
		fmt.Fprintf(l.w, "\n%s:\n", p)
	}
	l.print(entries)
	if !recursive {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() { // symlinked directories aren't followed
			if err := l.listDir(e.Path, true, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l lister) print(entries []oci.Entry) {
	tw := tabwriter.NewWriter(l.w, 0, 0, 0, ' ', tabwriter.AlignRight)
	for _, e := range entries {
		name := e.Name
		if e.IsSymlink() {
			name += " -> " + e.Target
		}
		size := oci.HumanSize(e.Size)
		if e.IsDir() {
			size = "-"
		}
		fmt.Fprintf(tw, "%s\t %s\t %s\t %s\t %s\t %s\n", lsMode(e.Mode), owner(l.users, e.UID), owner(l.groups, e.GID), size, lsTime(e.ModTime), name)
	}
	tw.Flush()
}

// lsMode formats a mode the way ls does: -rwsr-xr-x, drwxrwxrwt.
func lsMode(m fs.FileMode) string {
	b := []byte("----------")
	switch {
	case m.IsDir():
		b[0] = 'd'
	case m&fs.ModeSymlink != 0:
		b[0] = 'l'
	}
	for i, c := range "rwxrwxrwx" {
		if m&(1<<uint(8-i)) != 0 {
			b[i+1] = byte(c)
		}
	}
	special := func(set bool, pos int, lower, upper byte) {
		if !set {
			return
		}
		if b[pos] == 'x' {
			b[pos] = lower
		} else {
			b[pos] = upper
		}
	}
	special(m&fs.ModeSetuid != 0, 3, 's', 'S')
	special(m&fs.ModeSetgid != 0, 6, 's', 'S')
	special(m&fs.ModeSticky != 0, 9, 't', 'T')
	return string(b)
}

func owner(names map[uint32]string, id uint32) string {
	if n, ok := names[id]; ok {
		return n
	}
	return fmt.Sprint(id)
}

// lsTime shows month, day and time, or the year for files over six months old.
func lsTime(t time.Time) string {
	if time.Since(t) > 180*24*time.Hour || t.After(time.Now().Add(time.Hour)) {
		return t.Local().Format("Jan _2  2006")
	}
	return t.Local().Format("Jan _2 15:04")
}

func jsonList(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	data, _ := json.Marshal(list)
	return string(data)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func init() {
	imageInspectCmd.Flags().BoolVar(&flagInspectJSON, "json", false, "print the full image record as JSON")
	imageHistoryCmd.Flags().BoolVar(&flagNoTrunc, "no-trunc", false, "don't shorten build steps")
	imageLsCmd.Flags().BoolVarP(&flagRecursive, "recursive", "R", false, "list subdirectories too")
	imageCpCmd.Flags().BoolVarP(&flagCpForce, "force", "f", false, "replace existing files")
	imageCmd.AddCommand(imageInspectCmd, imageHistoryCmd, imageLsCmd, imageCatCmd, imageCpCmd)
}

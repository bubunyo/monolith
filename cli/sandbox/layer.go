package sandbox

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// The writable layer: what the code changed inside the sandbox, relative to
// the repo on the Mac. Reading it runs commands in the VM through the
// helper; files are compared on the Mac.

const lowerDir = "/mnt/repo-lower" // the Mac's copy, read-only (sandbox-mount)

// change is one path that differs from the Mac's copy.
type change struct {
	kind  byte   // 'A' added, 'M' modified, 'D' deleted
	path  string // relative to the repo, slash-separated
	group int    // for a collapsed new directory: how many files it holds
}

var (
	flagPatch bool
	flagAll   bool
)

var diffCmd = &cobra.Command{
	Use:   "diff [PATH...]",
	Short: "Show what the code changed in the sandbox, compared to your Mac's copy",
	Long: `List the files the sandbox added (A), modified (M) or deleted (D), relative to the
repo on your Mac. New directories are summarised with their file count. --patch
shows unified diffs for text files. PATHs limit the output.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		return s.withEngine(func() error {
			changes, err := s.changes(filterFor(s.repo, args))
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Println("No changes.")
				return nil
			}
			for _, c := range collapse(changes, s.repo) {
				if c.group > 0 {
					fmt.Printf("%c %s/ (%d files)\n", c.kind, c.path, c.group)
				} else {
					fmt.Printf("%c %s\n", c.kind, c.path)
				}
			}
			if flagPatch {
				return s.patch(changes)
			}
			return nil
		})
	},
}

var applyCmd = &cobra.Command{
	Use:   "apply [PATH...]",
	Short: "Copy changes from the sandbox to your Mac's copy of the repo",
	Long: `Copy the sandbox's changes to the given paths (or every change with --all) into the
repo on your Mac: new and modified files are written, deleted ones removed. It
lists what will change and asks first (--yes to skip). Review with 'diff' before
applying anything from code you don't trust: you are bringing its output onto
your Mac.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		if len(args) == 0 && !flagAll {
			return fmt.Errorf("name the paths to apply, or pass --all")
		}
		return s.withEngine(func() error {
			changes, err := s.changes(filterFor(s.repo, args))
			if err != nil {
				return err
			}
			if len(changes) == 0 {
				fmt.Println("Nothing to apply.")
				return nil
			}
			for _, c := range collapse(changes, s.repo) {
				if c.group > 0 {
					fmt.Printf("%c %s/ (%d files)\n", c.kind, c.path, c.group)
				} else {
					fmt.Printf("%c %s\n", c.kind, c.path)
				}
			}
			if !flagYes && !confirm(fmt.Sprintf("Apply these %d change(s) to %s?", len(changes), s.repo)) {
				fmt.Println("Cancelled.")
				return nil
			}
			return s.apply(changes)
		})
	},
}

var resetCmd = &cobra.Command{
	Use:   "reset",
	Short: "Discard everything the code wrote in the sandbox",
	Long: `Throw away the sandbox's writable layer, so the repo inside matches your Mac's copy
again (installed dependencies and build output included). The shell container is
recreated on the next activate.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := resolve()
		if err != nil {
			return err
		}
		if !flagYes && !confirm("Discard every change made in the sandbox?") {
			fmt.Println("Cancelled.")
			return nil
		}
		if n := s.openSessions(); n > 0 {
			return fmt.Errorf("the sandbox is in use (%d session(s)); deactivate them first", n)
		}
		return s.withEngine(func() error {
			if _, err := s.dockerOutput("rm", "--force", shellName); err != nil && !strings.Contains(err.Error(), "No such container") {
				return err
			}
			if err := s.helper("/usr/local/bin/sandbox-mount reset"); err != nil {
				return err
			}
			fmt.Println("Sandbox reset.")
			return nil
		})
	},
}

// withEngine runs fn with the sandbox's VM up. Afterwards the VM is paused
// again if it wasn't running before and no other session is open.
func (s *sandbox) withEngine(fn func() error) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("the docker CLI is required")
	}
	wasRunning, err := s.ensureRunning()
	if err != nil {
		return err
	}
	end, err := s.beginSession()
	if err != nil {
		return err
	}
	fnErr := fn()
	if others := end(); others == 0 && !wasRunning {
		if err := s.pause(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: pausing the sandbox: %v\n", err)
		}
	}
	return fnErr
}

// changes lists what differs between the sandbox and the Mac, limited by
// keep (nil keeps everything), sorted by path.
func (s *sandbox) changes(keep func(string) bool) ([]change, error) {
	var out []change

	// Deleted: entries of the Mac's copy missing from the merged view,
	// checked only in directories the layer touched (only those can hide
	// anything: whiteouts and directories replaced wholesale).
	script := fmt.Sprintf(`upper=%s lower=%s merged=%s
cd "$upper" && find . -type d | while IFS= read -r d; do
	[ -d "$lower/$d" ] || continue
	ls -A "$lower/$d" | while IFS= read -r e; do
		[ -e "$merged/$d/$e" ] || [ -L "$merged/$d/$e" ] || printf '%%s\n' "$d/$e"
	done
done`, shq(upperDir), shq(lowerDir), shq(s.repo))
	deleted, err := s.helperOutput(script)
	if err != nil {
		return nil, err
	}
	for _, p := range strings.Split(deleted, "\n") {
		p, ok := relName(p)
		if ok && (keep == nil || keep(p)) {
			out = append(out, change{kind: 'D', path: p})
		}
	}

	// Added and modified: files and symlinks in the layer that differ.
	err = s.walkLayer(func(hdr *tar.Header, r io.Reader) error {
		rel, ok := relName(hdr.Name)
		if !ok || (keep != nil && !keep(rel)) {
			return nil
		}
		mac := filepath.Join(s.repo, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeReg:
			info, err := os.Lstat(mac)
			if err != nil {
				out = append(out, change{kind: 'A', path: rel})
				return nil
			}
			if !info.Mode().IsRegular() || info.Size() != hdr.Size || info.Mode().Perm()&0111 != os.FileMode(hdr.Mode).Perm()&0111 {
				out = append(out, change{kind: 'M', path: rel})
				return nil
			}
			same, err := sameContent(mac, r)
			if err != nil {
				return err
			}
			if !same {
				out = append(out, change{kind: 'M', path: rel})
			}
		case tar.TypeSymlink:
			target, err := os.Readlink(mac)
			switch {
			case os.IsNotExist(err):
				out = append(out, change{kind: 'A', path: rel})
			case err != nil || target != hdr.Linkname:
				out = append(out, change{kind: 'M', path: rel})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// walkLayer streams the writable layer as a tar from the VM. Whiteouts
// (character devices) and directories are skipped by callers.
func (s *sandbox) walkLayer(fn func(*tar.Header, io.Reader) error) error {
	if err := s.ensureImage(helperImage); err != nil {
		return err
	}
	cmd := s.docker("run", "--rm", "--privileged", "--pid=host", "--net=host", "--cgroupns=host", helperImage,
		"nsenter", "-t", "1", "-m", "--", "tar", "-C", upperDir, "-cf", "-", ".")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	tr := tar.NewReader(stdout)
	var walkErr error
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			walkErr = fmt.Errorf("reading the sandbox layer: %w", err)
			break
		}
		if err := fn(hdr, tr); err != nil {
			walkErr = err
			break
		}
	}
	io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil && walkErr == nil {
		walkErr = fmt.Errorf("reading the sandbox layer: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return walkErr
}

// helperOutput runs a script in the VM and returns its stdout.
func (s *sandbox) helperOutput(script string) (string, error) {
	if err := s.ensureImage(helperImage); err != nil {
		return "", err
	}
	return s.dockerOutput("run", "--rm", "--privileged", "--pid=host", "--net=host", "--cgroupns=host", helperImage,
		"nsenter", "-t", "1", "-m", "--", "/bin/sh", "-c", script)
}

// patch prints unified diffs for added and modified files.
func (s *sandbox) patch(changes []change) error {
	want := map[string]byte{}
	for _, c := range changes {
		if c.kind != 'D' {
			want[c.path] = c.kind
		}
	}
	if len(want) == 0 {
		return nil
	}
	tmp, err := os.MkdirTemp("", "monolith-sandbox-diff-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fmt.Println()
	return s.walkLayer(func(hdr *tar.Header, r io.Reader) error {
		rel, ok := relName(hdr.Name)
		if !ok || hdr.Typeflag != tar.TypeReg {
			return nil
		}
		kind, ok := want[rel]
		if !ok {
			return nil
		}
		f, err := os.CreateTemp(tmp, "new-")
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, r); err != nil {
			f.Close()
			return err
		}
		f.Close()
		old := filepath.Join(s.repo, filepath.FromSlash(rel))
		if kind == 'A' {
			old = "/dev/null"
		}
		cmd := exec.Command("diff", "-u", "--label", "mac/"+rel, "--label", "sandbox/"+rel, old, f.Name())
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		cmd.Run() // exit 1 means "differs"
		return nil
	})
}

// apply writes the given changes into the Mac's repo.
func (s *sandbox) apply(changes []change) error {
	writes := map[string]bool{}
	for _, c := range changes {
		if c.kind == 'D' {
			continue
		}
		writes[c.path] = true
	}
	applied := 0
	err := s.walkLayer(func(hdr *tar.Header, r io.Reader) error {
		rel, ok := relName(hdr.Name)
		if !ok || !writes[rel] {
			return nil
		}
		dst, err := s.safePath(rel)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			tmp := dst + ".monolith-apply"
			f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, r); err != nil {
				f.Close()
				os.Remove(tmp)
				return err
			}
			f.Close()
			if err := os.Rename(tmp, dst); err != nil {
				os.Remove(tmp)
				return err
			}
		case tar.TypeSymlink:
			target := hdr.Linkname
			if filepath.IsAbs(target) || !within(filepath.Join(filepath.Dir(dst), target), s.repo) {
				fmt.Fprintf(os.Stderr, "Skipped symlink %s -> %s (points outside the repo)\n", rel, target)
				return nil
			}
			os.Remove(dst)
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
		default:
			return nil
		}
		applied++
		return nil
	})
	if err != nil {
		return err
	}
	for _, c := range changes {
		// Never delete something just written, or a directory holding it.
		if c.kind != 'D' || writes[c.path] || holdsWrite(c.path, writes) {
			continue
		}
		dst, err := s.safePath(c.path)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		applied++
	}
	fmt.Printf("Applied %d change(s) to %s.\n", applied, s.repo)
	return nil
}

func holdsWrite(dir string, writes map[string]bool) bool {
	for w := range writes {
		if strings.HasPrefix(w, dir+"/") {
			return true
		}
	}
	return false
}

// safePath maps a layer path to the Mac, refusing anything that would
// leave the repo, including through a symlinked directory on the Mac.
func (s *sandbox) safePath(rel string) (string, error) {
	clean := path.Clean("/" + rel)[1:]
	if clean == "" || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("refusing path %q", rel)
	}
	dst := s.repo
	parts := strings.Split(clean, "/")
	for i, p := range parts {
		dst = filepath.Join(dst, p)
		if i == len(parts)-1 {
			break
		}
		info, err := os.Lstat(dst)
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing %s: %s is a symlink on your Mac", rel, dst)
		}
	}
	return dst, nil
}

// collapse groups added files under directories that don't exist on the
// Mac, so a new node_modules is one line.
func collapse(changes []change, repo string) []change {
	var out []change
	groups := map[string]int{}
	var order []string
	for _, c := range changes {
		top := ""
		if c.kind == 'A' {
			parts := strings.Split(c.path, "/")
			for i := 1; i < len(parts); i++ {
				dir := strings.Join(parts[:i], "/")
				if _, err := os.Lstat(filepath.Join(repo, filepath.FromSlash(dir))); os.IsNotExist(err) {
					top = dir
					break
				}
			}
		}
		if top == "" {
			out = append(out, c)
			continue
		}
		if _, ok := groups[top]; !ok {
			order = append(order, top)
		}
		groups[top]++
	}
	for _, dir := range order {
		out = append(out, change{kind: 'A', path: dir, group: groups[dir]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// filterFor keeps changes under the given paths (relative to the current
// directory or absolute, inside the repo).
func filterFor(repo string, args []string) func(string) bool {
	if len(args) == 0 {
		return nil
	}
	var prefixes []string
	for _, a := range args {
		abs, err := filepath.Abs(a)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
			abs = filepath.Join(real, filepath.Base(abs))
		}
		rel, err := filepath.Rel(repo, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			fmt.Fprintf(os.Stderr, "Ignoring %s: not in %s\n", a, repo)
			continue
		}
		prefixes = append(prefixes, filepath.ToSlash(rel))
	}
	return func(p string) bool {
		for _, pre := range prefixes {
			if pre == "." || p == pre || strings.HasPrefix(p, pre+"/") {
				return true
			}
		}
		return false
	}
}

func relName(name string) (string, bool) {
	rel := strings.TrimPrefix(path.Clean("/"+name), "/")
	return rel, rel != "" && rel != "."
}

func sameContent(path string, r io.Reader) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	a, b := sha256.New(), sha256.New()
	if _, err := io.Copy(a, f); err != nil {
		return false, err
	}
	if _, err := io.Copy(b, r); err != nil {
		return false, err
	}
	return bytes.Equal(a.Sum(nil), b.Sum(nil)), nil
}

// shq single-quotes s for sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func init() {
	diffCmd.Flags().BoolVarP(&flagPatch, "patch", "p", false, "show unified diffs")
	applyCmd.Flags().BoolVar(&flagAll, "all", false, "apply every change")
	applyCmd.Flags().BoolVarP(&flagYes, "yes", "y", false, "don't ask")
	resetCmd.Flags().BoolVarP(&flagYes, "yes", "y", false, "don't ask")
}

package container

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/bubunyo/monolith/oci"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

var flagShCommands string

var imageShCmd = &cobra.Command{
	Use:   "sh IMAGE",
	Short: "Explore an image and its layers in a shell (nothing from the image runs)",
	Long: `Open a shell for browsing an image and each of its layers. It's monolith's own
shell, running on the Mac over the image's disk and cached layers: no VM starts
and nothing from the image runs, so it works on images without a shell.

  ls, cd, pwd, tree, find, cat, head, less, stat, du   browse the image
  layers                 each layer: size, files added/changed/deleted, build step
  layer N / merged       browse only what layer N contains / the whole image again
  changes [N] [PATH]     what a layer added (A), changed (M) and deleted (D)
  blame PATH             which layers added, changed or deleted a path
  cp PATH DEST           copy out to the Mac
  help, exit             (Ctrl+D also exits)

Commands can be joined with ; and &&. -c runs commands without a prompt:
  monolith container image sh nginx -c "find /etc -name '*.conf'"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		store, img, err := localImage(ctx, args[0])
		if err != nil {
			return err
		}
		full, err := store.OpenFS(img.ID)
		if err != nil {
			return err
		}
		defer full.Close()
		sh := &imageShell{ctx: ctx, store: store, img: img, name: args[0], full: full, layer: -1, cwd: "/", out: os.Stdout, width: 100}
		if out := int(os.Stdout.Fd()); term.IsTerminal(out) {
			sh.color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
			if w, _, err := term.GetSize(out); err == nil && w > 20 {
				sh.width = w
			}
		}
		if cmd.Flags().Changed("command") {
			if !sh.exec(flagShCommands) {
				os.Exit(1)
			}
			return nil
		}
		return sh.interact()
	},
}

func init() {
	imageShCmd.Flags().StringVarP(&flagShCommands, "command", "c", "", "run these commands and exit")
	imageCmd.AddCommand(imageShCmd)
}

// imageShell is the state of `image sh`.
type imageShell struct {
	ctx   context.Context
	store *oci.Store
	img   oci.Image
	name  string
	full  imageView
	index *oci.LayerIndex // built on first use
	layer int             // -1: the whole image
	cwd   string
	out   io.Writer
	color bool
	width int
	term  *term.Terminal // nil off a terminal
	state *term.State
	done  bool // exit was run
}

// imageView is the whole image (an *oci.ImageFS; tests use a fake).
type imageView interface {
	oci.View
	Names() (users, groups map[uint32]string)
}

// view is what the shell browses: the whole image or the current layer.
func (sh *imageShell) view() oci.View {
	if sh.layer >= 0 {
		return sh.index.Layers[sh.layer].FS()
	}
	return sh.full
}

func (sh *imageShell) abs(p string) string {
	if p == "" {
		return sh.cwd
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(sh.cwd, p)
	}
	return path.Clean(p)
}

func (sh *imageShell) printf(format string, args ...any) { fmt.Fprintf(sh.out, format, args...) }

func (sh *imageShell) paint(style, s string) string {
	if !sh.color || s == "" {
		return s
	}
	return "\033[" + style + "m" + s + "\033[0m"
}

func (sh *imageShell) prompt() string {
	where := sh.paint("1;36", sh.name)
	if sh.layer >= 0 {
		where += sh.paint("33", fmt.Sprintf("[layer %d]", sh.layer))
	}
	return where + ":" + sh.paint("34", sh.cwd) + "$ "
}

// interact runs the prompt: line editing, history and Tab completion on a
// terminal, plain lines otherwise.
func (sh *imageShell) interact() error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		sc := bufio.NewScanner(os.Stdin)
		for !sh.done && sc.Scan() {
			sh.exec(sc.Text())
		}
		return sc.Err()
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	sh.state = state
	defer term.Restore(fd, state)
	sh.term = term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{os.Stdin, os.Stdout}, "")
	sh.out = sh.term
	sh.term.AutoCompleteCallback = sh.complete
	sh.printf("%s — %d layers, %s %s\n", sh.paint("1", oci.Display(imageRef(sh.img))), len(sh.img.Layers), oci.HumanSize(sh.img.Size), sh.paint("2", "(type help; Ctrl+D or exit to leave)"))
	for !sh.done {
		sh.term.SetPrompt(sh.prompt())
		line, err := sh.term.ReadLine()
		if err == io.EOF {
			sh.printf("\n")
			return nil
		}
		if err != nil {
			return err
		}
		sh.exec(line)
	}
	return nil
}

func imageRef(img oci.Image) string {
	if len(img.Refs) > 0 {
		return img.Refs[0]
	}
	return img.ShortID()
}

var errExit = errors.New("exit")

// exec runs a command line and reports whether every command it ran
// succeeded. Errors are printed.
func (sh *imageShell) exec(line string) bool {
	cmds, err := parseLine(line)
	if err != nil {
		sh.printf("sh: %v\n", err)
		return false
	}
	ok := true
	for _, c := range cmds {
		if c.andPrev && !ok {
			continue
		}
		if len(c.args) == 0 {
			continue
		}
		err := sh.run(c.args)
		if errors.Is(err, errExit) {
			sh.done = true
			return ok
		}
		if errors.Is(err, errSilent) {
			ok = false
			continue
		}
		if err != nil {
			sh.printf("%s: %v\n", c.args[0], err)
			ok = false
		} else {
			ok = true
		}
	}
	return ok
}

type shellCommand struct {
	args    []string
	andPrev bool // joined with && to the previous command
}

// parseLine splits a line into commands at ; and &&, and each command into
// words, with '…' and "…" quoting and backslash escapes.
func parseLine(line string) ([]shellCommand, error) {
	var cmds []shellCommand
	cur := shellCommand{}
	var word strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			cur.args = append(cur.args, word.String())
			word.Reset()
			inWord = false
		}
	}
	end := func(and bool) {
		flush()
		cmds = append(cmds, cur)
		cur = shellCommand{andPrev: and}
	}
	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\' && i+1 < len(runes):
			i++
			word.WriteRune(runes[i])
			inWord = true
		case c == '\'' || c == '"':
			j := i + 1
			for ; j < len(runes) && runes[j] != c; j++ {
				if c == '"' && runes[j] == '\\' && j+1 < len(runes) {
					j++
				}
				word.WriteRune(runes[j])
			}
			if j >= len(runes) {
				return nil, fmt.Errorf("unterminated %c quote", c)
			}
			i = j
			inWord = true
		case c == ';':
			end(false)
		case c == '&' && i+1 < len(runes) && runes[i+1] == '&':
			i++
			end(true)
		case c == '#' && !inWord:
			i = len(runes)
		case c == '|' || c == '>' || c == '<':
			return nil, fmt.Errorf("pipes and redirection (%c) aren't supported; use -c and pipe its output on the Mac", c)
		case c == ' ' || c == '\t':
			flush()
		default:
			word.WriteRune(c)
			inWord = true
		}
	}
	flush()
	cmds = append(cmds, cur)
	return cmds, nil
}

type shellFunc func(sh *imageShell, args []string) error

var shellCommands map[string]shellFunc

func init() {
	shellCommands = map[string]shellFunc{
		"ls": shLs, "cd": shCd, "pwd": shPwd, "tree": shTree, "find": shFind,
		"cat": shCat, "head": shHead, "less": shLess, "more": shLess, "stat": shStat, "du": shDu,
		"cp": shCp, "layers": shLayers, "layer": shLayer, "merged": shMerged,
		"changes": shChanges, "blame": shBlame, "help": shHelp,
		"exit": func(*imageShell, []string) error { return errExit },
		"quit": func(*imageShell, []string) error { return errExit },
	}
}

func (sh *imageShell) run(args []string) error {
	f, ok := shellCommands[args[0]]
	if !ok {
		return fmt.Errorf("unknown command (type help)")
	}
	return f(sh, args[1:])
}

// flags parses a command's flags; -h prints its usage.
func flags(name string, args []string, define func(*pflag.FlagSet)) (*pflag.FlagSet, error) {
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return fs, nil
}

func shLs(sh *imageShell, args []string) error {
	var long, all, recursive bool
	f, err := flags("ls", args, func(f *pflag.FlagSet) {
		f.BoolVarP(&long, "long", "l", false, "")
		f.BoolVarP(&all, "all", "a", false, "")
		f.BoolVarP(&recursive, "recursive", "R", false, "")
	})
	if err != nil {
		return err
	}
	targets := f.Args()
	if len(targets) == 0 {
		targets = []string{"."}
	}
	v := sh.view()
	var users, groups map[uint32]string
	if long {
		users, groups = sh.full.Names()
	}
	var failed error
	for i, t := range targets {
		p := sh.abs(t)
		e, err := v.Stat(p)
		if err != nil {
			sh.printf("ls: %v\n", err)
			failed = errSilent
			continue
		}
		if len(targets) > 1 && e.IsDir() {
			if i > 0 {
				sh.printf("\n")
			}
			sh.printf("%s:\n", t)
		}
		if !e.IsDir() {
			le, _ := v.Lstat(p)
			sh.printEntries([]oci.Entry{le}, long, users, groups)
			continue
		}
		if err := sh.lsDir(v, e.Path, long, all, recursive, false, users, groups); err != nil {
			return err
		}
	}
	return failed
}

// errSilent fails a command whose errors were already printed.
var errSilent = errors.New("")

func (sh *imageShell) lsDir(v oci.View, dir string, long, all, recursive, header bool, users, groups map[uint32]string) error {
	entries, err := v.ReadDir(dir)
	if err != nil {
		return err
	}
	if !all {
		entries = withoutHidden(entries)
	}
	if header {
		sh.printf("\n%s:\n", dir)
	}
	sh.printEntries(entries, long, users, groups)
	if recursive {
		for _, e := range entries {
			if e.IsDir() {
				if err := sh.lsDir(v, e.Path, long, all, true, true, users, groups); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func withoutHidden(entries []oci.Entry) []oci.Entry {
	var out []oci.Entry
	for _, e := range entries {
		if !strings.HasPrefix(e.Name, ".") {
			out = append(out, e)
		}
	}
	return out
}

func (sh *imageShell) printEntries(entries []oci.Entry, long bool, users, groups map[uint32]string) {
	if long {
		var buf bytes.Buffer
		lister{fs: sh.view(), users: users, groups: groups, w: &buf}.print(entries)
		sh.out.Write(buf.Bytes())
		return
	}
	names := make([]string, len(entries))
	plain := make([]string, len(entries))
	for i, e := range entries {
		plain[i] = e.Name
		switch {
		case e.IsDir():
			names[i] = sh.paint("1;34", e.Name)
		case e.IsSymlink():
			names[i] = sh.paint("36", e.Name)
		case e.Mode&0111 != 0:
			names[i] = sh.paint("32", e.Name)
		default:
			names[i] = e.Name
		}
	}
	sh.printf("%s", columns(names, plain, sh.width))
}

// columns lays names out in columns, top to bottom like ls, to fit width.
// plain are the names without colour, for measuring.
func columns(names, plain []string, width int) string {
	if len(names) == 0 {
		return ""
	}
	widest := 0
	for _, p := range plain {
		if n := len([]rune(p)); n > widest {
			widest = n
		}
	}
	colWidth := widest + 2
	cols := width / colWidth
	if cols < 1 {
		cols = 1
	}
	rows := (len(names) + cols - 1) / cols
	var b strings.Builder
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			i := c*rows + r
			if i >= len(names) {
				continue
			}
			b.WriteString(names[i])
			if c < cols-1 && (c+1)*rows+r < len(names) {
				b.WriteString(strings.Repeat(" ", colWidth-len([]rune(plain[i]))))
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func shCd(sh *imageShell, args []string) error {
	target := "/"
	if len(args) > 0 {
		target = args[0]
	}
	e, err := sh.view().Stat(sh.abs(target))
	if err != nil {
		return err
	}
	if !e.IsDir() {
		return fmt.Errorf("%s: not a directory", target)
	}
	sh.cwd = e.Path
	return nil
}

func shPwd(sh *imageShell, _ []string) error {
	sh.printf("%s\n", sh.cwd)
	return nil
}

func shTree(sh *imageShell, args []string) error {
	var depth int
	f, err := flags("tree", args, func(f *pflag.FlagSet) { f.IntVarP(&depth, "level", "L", 0, "") })
	if err != nil {
		return err
	}
	root := sh.cwd
	if f.NArg() > 0 {
		root = sh.abs(f.Arg(0))
	}
	v := sh.view()
	e, err := v.Stat(root)
	if err != nil {
		return err
	}
	sh.printf("%s\n", sh.paint("1;34", e.Path))
	dirs, files := 0, 0
	var walk func(dir, indent string, level int) error
	walk = func(dir, indent string, level int) error {
		entries, err := v.ReadDir(dir)
		if err != nil {
			return err
		}
		for i, c := range entries {
			branch, next := "├── ", "│   "
			if i == len(entries)-1 {
				branch, next = "└── ", "    "
			}
			name := c.Name
			switch {
			case c.IsDir():
				name = sh.paint("1;34", name)
				dirs++
			case c.IsSymlink():
				name = sh.paint("36", name) + " -> " + c.Target
				files++
			default:
				files++
			}
			sh.printf("%s%s%s\n", indent, branch, name)
			if c.IsDir() && (depth == 0 || level < depth) {
				if err := walk(c.Path, indent+next, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if !e.IsDir() {
		return nil
	}
	if err := walk(e.Path, "", 1); err != nil {
		return err
	}
	dirWord := "directories"
	if dirs == 1 {
		dirWord = "directory"
	}
	sh.printf("\n%d %s, %d %s\n", dirs, dirWord, files, plural(files, "file"))
	return nil
}

// walk visits everything under root (root included) without following
// symlinked directories.
func walk(v oci.View, root string, fn func(e oci.Entry) error) error {
	e, err := v.Lstat(root)
	if err != nil {
		return err
	}
	if err := fn(e); err != nil {
		return err
	}
	if !e.IsDir() {
		return nil
	}
	entries, err := v.ReadDir(root)
	if err != nil {
		return err
	}
	for _, c := range entries {
		if err := walk(v, c.Path, fn); err != nil {
			return err
		}
	}
	return nil
}

func shFind(sh *imageShell, args []string) error {
	var glob, typ string
	// find puts the path first and uses single-dash long options; rewrite
	// them for pflag.
	var paths, rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-name", "-type":
			rest = append(rest, "-"+args[i])
			if i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
		default:
			if strings.HasPrefix(args[i], "-") {
				rest = append(rest, args[i])
			} else {
				paths = append(paths, args[i])
			}
		}
	}
	if _, err := flags("find", rest, func(f *pflag.FlagSet) {
		f.StringVar(&glob, "name", "", "")
		f.StringVar(&typ, "type", "", "")
	}); err != nil {
		return err
	}
	if typ != "" && typ != "f" && typ != "d" && typ != "l" {
		return fmt.Errorf("-type must be f, d or l")
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	v := sh.view()
	for _, p := range paths {
		root, err := v.Resolve(sh.abs(p))
		if err != nil {
			return err
		}
		err = walk(v, root, func(e oci.Entry) error {
			if glob != "" {
				if ok, _ := path.Match(glob, e.Name); !ok {
					return nil
				}
			}
			switch typ {
			case "f":
				if !e.Mode.IsRegular() {
					return nil
				}
			case "d":
				if !e.IsDir() {
					return nil
				}
			case "l":
				if !e.IsSymlink() {
					return nil
				}
			}
			sh.printf("%s\n", e.Path)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// openText opens a file for printing, refusing binary files unless force.
func (sh *imageShell) openText(p string, force bool) (io.Reader, io.Closer, error) {
	rc, _, err := sh.view().Open(sh.abs(p))
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(rc, 8192)
	head, _ := br.Peek(8000)
	if !force && bytes.IndexByte(head, 0) >= 0 {
		rc.Close()
		return nil, nil, fmt.Errorf("%s: binary file (use --force to print it)", p)
	}
	return br, rc, nil
}

func shCat(sh *imageShell, args []string) error {
	var force bool
	f, err := flags("cat", args, func(f *pflag.FlagSet) { f.BoolVar(&force, "force", false, "") })
	if err != nil {
		return err
	}
	if f.NArg() == 0 {
		return fmt.Errorf("usage: cat [--force] FILE...")
	}
	for _, p := range f.Args() {
		r, c, err := sh.openText(p, force)
		if err != nil {
			return err
		}
		_, err = io.Copy(sh.out, r)
		c.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func shHead(sh *imageShell, args []string) error {
	var n int
	var force bool
	f, err := flags("head", args, func(f *pflag.FlagSet) {
		f.IntVarP(&n, "lines", "n", 10, "")
		f.BoolVar(&force, "force", false, "")
	})
	if err != nil {
		return err
	}
	if f.NArg() != 1 {
		return fmt.Errorf("usage: head [-n N] FILE")
	}
	r, c, err := sh.openText(f.Arg(0), force)
	if err != nil {
		return err
	}
	defer c.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for i := 0; i < n && sc.Scan(); i++ {
		sh.printf("%s\n", sc.Text())
	}
	return sc.Err()
}

// shLess shows a file in $PAGER (default less), with the terminal back in
// normal mode while it runs.
func shLess(sh *imageShell, args []string) error {
	var force bool
	f, err := flags("less", args, func(f *pflag.FlagSet) { f.BoolVar(&force, "force", false, "") })
	if err != nil {
		return err
	}
	if f.NArg() != 1 {
		return fmt.Errorf("usage: less FILE")
	}
	r, c, err := sh.openText(f.Arg(0), force)
	if err != nil {
		return err
	}
	defer c.Close()
	if sh.term == nil {
		_, err := io.Copy(sh.out, r)
		return err
	}
	pager := strings.Fields(os.Getenv("PAGER"))
	if len(pager) == 0 {
		pager = []string{"less", "-R"}
	}
	fd := int(os.Stdin.Fd())
	term.Restore(fd, sh.state)
	defer func() {
		if st, err := term.MakeRaw(fd); err == nil {
			sh.state = st
		}
	}()
	cmd := exec.Command(pager[0], pager[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r, os.Stdout, os.Stderr
	return cmd.Run()
}

func shStat(sh *imageShell, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: stat PATH")
	}
	v := sh.view()
	e, err := v.Lstat(sh.abs(args[0]))
	if err != nil {
		// The path may go through a symlinked directory.
		r, rerr := v.Resolve(path.Dir(sh.abs(args[0])))
		if rerr != nil {
			return err
		}
		if e, err = v.Lstat(path.Join(r, path.Base(args[0]))); err != nil {
			return err
		}
	}
	users, groups := sh.full.Names()
	kind := "regular file"
	switch {
	case e.IsDir():
		kind = "directory"
	case e.IsSymlink():
		kind = "symbolic link -> " + e.Target
	}
	sh.printf("  Path: %s\n  Type: %s\n  Mode: %s (%04o)\n Owner: %s (%d) / %s (%d)\n  Size: %s (%d bytes)\nModify: %s\n",
		e.Path, kind, lsMode(e.Mode), modeOctal(e.Mode), owner(users, e.UID), e.UID, owner(groups, e.GID), e.GID, oci.HumanSize(e.Size), e.Size, e.ModTime.Local().Format("2006-01-02 15:04:05"))
	if sh.index != nil {
		for _, b := range sh.index.Blame(e.Path) {
			sh.printf(" Layer: %d %s\n", b.Layer.Index, changeVerb(b.Kind))
		}
	}
	return nil
}

func modeOctal(m fs.FileMode) uint32 {
	o := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		o |= 04000
	}
	if m&fs.ModeSetgid != 0 {
		o |= 02000
	}
	if m&fs.ModeSticky != 0 {
		o |= 01000
	}
	return o
}

func shDu(sh *imageShell, args []string) error {
	root := sh.cwd
	if len(args) > 0 {
		root = sh.abs(args[0])
	}
	v := sh.view()
	r, err := v.Resolve(root)
	if err != nil {
		return err
	}
	e, err := v.Lstat(r)
	if err != nil {
		return err
	}
	if !e.IsDir() {
		sh.printf("%8s  %s\n", oci.HumanSize(e.Size), e.Path)
		return nil
	}
	entries, err := v.ReadDir(r)
	if err != nil {
		return err
	}
	type usage struct {
		name string
		size int64
	}
	var rows []usage
	var total int64
	for _, c := range entries {
		var n int64
		if err := walk(v, c.Path, func(x oci.Entry) error {
			if x.Mode.IsRegular() {
				n += x.Size
			}
			return nil
		}); err != nil {
			return err
		}
		name := c.Name
		if c.IsDir() {
			name += "/"
		}
		rows = append(rows, usage{name, n})
		total += n
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].size > rows[j].size })
	for _, u := range rows {
		sh.printf("%8s  %s\n", oci.HumanSize(u.size), u.name)
	}
	sh.printf("%8s  %s\n", oci.HumanSize(total), sh.paint("1", "total"))
	return nil
}

func shCp(sh *imageShell, args []string) error {
	var force bool
	f, err := flags("cp", args, func(f *pflag.FlagSet) { f.BoolVarP(&force, "force", "f", false, "") })
	if err != nil {
		return err
	}
	if f.NArg() != 2 {
		return fmt.Errorf("usage: cp [-f] PATH DEST (DEST is on the Mac)")
	}
	dest := f.Arg(1)
	if strings.HasPrefix(dest, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dest = filepath.Join(home, dest[2:])
		}
	}
	n := 0
	if err := oci.CopyOut(sh.view(), sh.abs(f.Arg(0)), dest, force, func(string) { n++ }); err != nil {
		return err
	}
	sh.printf("Copied %d %s to %s\n", n, plural(n, "item"), dest)
	return nil
}

// ensureIndex reads the layers, fetching any that aren't cached.
func (sh *imageShell) ensureIndex() error {
	if sh.index != nil {
		return nil
	}
	if sh.store.MissingLayers(sh.img) {
		sh.printf("The layers of %s aren't cached; fetching them.\n", oci.Display(imageRef(sh.img)))
		img, err := sh.store.EnsureLayers(sh.ctx, sh.img, oci.PullOptions{Log: sh.out})
		if err != nil {
			return err
		}
		sh.img = img
	}
	x, err := sh.store.IndexLayers(sh.img)
	if err != nil {
		return err
	}
	sh.index = x
	return nil
}

func shLayers(sh *imageShell, _ []string) error {
	if err := sh.ensureIndex(); err != nil {
		return err
	}
	var buf bytes.Buffer
	w := newTable(&buf)
	fmt.Fprintln(w, "#\tLAYER\tSIZE\tADDED\tCHANGED\tDELETED\tSTEP")
	stepWidth := sh.width - 60
	if stepWidth < 20 {
		stepWidth = 20
	}
	for _, l := range sh.index.Layers {
		a, c, d := l.Counts()
		step := buildStep(l.Step)
		if r := []rune(step); len(r) > stepWidth {
			step = string(r[:stepWidth-1]) + "…"
		}
		marker := strconv.Itoa(l.Index)
		if l.Index == sh.layer {
			marker = "*" + marker
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", marker, shortHex(l.Digest), oci.HumanSize(l.Size), a, c, d, step)
	}
	w.Flush()
	sh.out.Write(buf.Bytes())
	return nil
}

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func shortHex(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func shLayer(sh *imageShell, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: layer N (see layers)")
	}
	if err := sh.ensureIndex(); err != nil {
		return err
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 || n >= len(sh.index.Layers) {
		return fmt.Errorf("no layer %q: there are %d (0-%d)", args[0], len(sh.index.Layers), len(sh.index.Layers)-1)
	}
	sh.layer = n
	if _, err := sh.view().Stat(sh.cwd); err != nil {
		sh.cwd = "/"
	}
	l := sh.index.Layers[n]
	a, c, d := l.Counts()
	sh.printf("Layer %d (%s, %s): %d added, %d changed, %d deleted. Showing only its files; merged goes back.\n", n, shortHex(l.Digest), oci.HumanSize(l.Size), a, c, d)
	return nil
}

func shMerged(sh *imageShell, _ []string) error {
	sh.layer = -1
	if _, err := sh.view().Stat(sh.cwd); err != nil {
		sh.cwd = "/"
	}
	return nil
}

func shChanges(sh *imageShell, args []string) error {
	if err := sh.ensureIndex(); err != nil {
		return err
	}
	n := sh.layer
	prefix := "/"
	for _, a := range args {
		if i, err := strconv.Atoi(a); err == nil {
			n = i
		} else {
			prefix = sh.abs(a)
		}
	}
	if n < 0 {
		return fmt.Errorf("which layer? changes N [PATH] (or go into one with layer N)")
	}
	if n >= len(sh.index.Layers) {
		return fmt.Errorf("no layer %d: there are %d", n, len(sh.index.Layers))
	}
	changes := sh.index.Layers[n].Changes(prefix)
	for _, c := range changes {
		kind := string(c.Kind)
		switch c.Kind {
		case oci.Added:
			kind = sh.paint("32", kind)
		case oci.Changed:
			kind = sh.paint("33", kind)
		case oci.Deleted:
			kind = sh.paint("31", kind)
		}
		sh.printf("%s %s\n", kind, c.Path)
	}
	if len(changes) == 0 {
		sh.printf("Layer %d changes nothing under %s.\n", n, prefix)
	}
	return nil
}

func changeVerb(kind byte) string {
	switch kind {
	case oci.Added:
		return "added it"
	case oci.Changed:
		return "changed it"
	default:
		return "deleted it"
	}
}

func shBlame(sh *imageShell, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: blame PATH")
	}
	if err := sh.ensureIndex(); err != nil {
		return err
	}
	p := sh.abs(args[0])
	// Blame the path as the image has it, through symlinked directories.
	if dir, err := sh.full.Resolve(path.Dir(p)); err == nil {
		p = path.Join(dir, path.Base(p))
	}
	blame := sh.index.Blame(p)
	if len(blame) == 0 {
		return fmt.Errorf("%s: no layer touches it", p)
	}
	for _, b := range blame {
		verb := changeVerb(b.Kind)
		if b.Via != "" {
			verb += " (deleted " + b.Via + ")"
		}
		step := buildStep(b.Layer.Step)
		if r := []rune(step); len(r) > 70 {
			step = string(r[:69]) + "…"
		}
		line := fmt.Sprintf("layer %d (%s) %s", b.Layer.Index, shortHex(b.Layer.Digest), verb)
		if step != "" {
			line += ": " + sh.paint("2", step)
		}
		sh.printf("%s\n", line)
	}
	return nil
}

func shHelp(sh *imageShell, _ []string) error {
	sh.printf(`Browse:
  ls [-l] [-a] [-R] [PATH...]   list files
  cd [PATH], pwd                move around
  tree [-L N] [PATH]            directory tree
  find [PATH] [-name GLOB] [-type f|d|l]
  cat [--force] FILE...         print files (--force for binary ones)
  head [-n N] FILE              first lines
  less FILE                     page through a file ($PAGER)
  stat PATH                     details, and which layers touched it
  du [PATH]                     sizes by entry, largest first
Layers:
  layers                        each layer: size, added/changed/deleted, build step
  layer N                       browse only what layer N contains
  merged                        browse the whole image again
  changes [N] [PATH]            what layer N added (A), changed (M), deleted (D)
  blame PATH                    which layers added, changed or deleted PATH
Out:
  cp [-f] PATH DEST             copy to the Mac (DEST is a Mac path)
  help, exit                    (Ctrl+D also exits)
Join commands with ; or &&. Nothing from the image runs.
`)
	return nil
}

// complete is the Tab completion: command names first, then paths.
func (sh *imageShell) complete(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' {
		return "", 0, false
	}
	start := strings.LastIndexAny(line[:pos], " \t;&") + 1
	word := line[start:pos]
	firstWord := strings.TrimSpace(strings.TrimRight(line[:start], " \t")) == "" ||
		strings.HasSuffix(strings.TrimRight(line[:start], " \t"), ";") ||
		strings.HasSuffix(strings.TrimRight(line[:start], " \t"), "&&")
	var candidates []string
	if firstWord {
		for name := range shellCommands {
			if strings.HasPrefix(name, word) {
				candidates = append(candidates, name+" ")
			}
		}
	} else {
		dir, base := path.Split(word)
		entries, err := sh.view().ReadDir(sh.abs(dir))
		if err != nil {
			return "", 0, false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name, base) && (base != "" || !strings.HasPrefix(e.Name, ".")) {
				suffix := " "
				if e.IsDir() {
					suffix = "/"
				}
				candidates = append(candidates, dir+e.Name+suffix)
			}
		}
	}
	if len(candidates) == 0 {
		return "", 0, false
	}
	sort.Strings(candidates)
	completion := candidates[0]
	if len(candidates) > 1 {
		completion = commonPrefix(candidates)
		if completion == word {
			return "", 0, false
		}
	}
	return line[:start] + completion + line[pos:], start + len(completion), true
}

func commonPrefix(list []string) string {
	p := list[0]
	for _, s := range list[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}

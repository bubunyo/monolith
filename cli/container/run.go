package container

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	clivm "github.com/bubunyo/monolith/cli/vm"
	ci "github.com/bubunyo/monolith/containerinit"
	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/oci"
	"github.com/bubunyo/monolith/vm"
	"github.com/pkg/term/termios"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

var (
	flagDetach      bool
	flagInteractive bool
	flagTTY         bool
	flagRm          bool
	flagName        string
	flagEnv         []string
	flagEnvFile     []string
	flagWorkdir     string
	flagUser        string
	flagEntrypoint  string
	flagCPUs        uint
	flagMemory      string
	flagDisk        string
	flagPull        string
)

var runCmd = &cobra.Command{
	Use:   "run [flags] IMAGE [COMMAND] [ARG...]",
	Short: "Run a command in a new container",
	Long: `Create a container VM from IMAGE and run its command (the image's entrypoint and
cmd, or COMMAND).

In the foreground, the container's output goes to the terminal and run exits
with the container's exit code. With -it you get an interactive terminal:
every key, Ctrl+C included, goes to the container; Ctrl+P Ctrl+Q detaches.
With -d the container runs in the background ('container logs' shows its
output).

The image is pulled if it isn't local (--pull always pulls first).`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(cmd, args[0], args[1:])
	},
}

func init() {
	f := runCmd.Flags()
	f.SetInterspersed(false) // flags after IMAGE belong to the command, as with docker run
	f.BoolVarP(&flagDetach, "detach", "d", false, "run in the background and print the container name")
	f.BoolVarP(&flagInteractive, "interactive", "i", false, "keep stdin open (needs -t for now)")
	f.BoolVarP(&flagTTY, "tty", "t", false, "allocate a terminal")
	f.BoolVar(&flagRm, "rm", false, "remove the container when it exits")
	f.StringVar(&flagName, "name", "", "container name (default: <image>-<random>)")
	f.StringArrayVarP(&flagEnv, "env", "e", nil, "set an environment variable (KEY=VALUE, or KEY to copy it from here)")
	f.StringArrayVar(&flagEnvFile, "env-file", nil, "read environment variables from a file")
	f.StringVarP(&flagWorkdir, "workdir", "w", "", "working directory in the container")
	f.StringVarP(&flagUser, "user", "u", "", "user to run as (name|uid[:group|gid])")
	f.StringVar(&flagEntrypoint, "entrypoint", "", "override the image's entrypoint")
	f.UintVar(&flagCPUs, "cpus", 1, "number of vCPUs")
	f.StringVarP(&flagMemory, "memory", "m", "512M", "memory limit of the container VM")
	f.StringVar(&flagDisk, "disk", "8G", "size of the container's disk, image included (sparse: only what's written takes space)")
	f.StringVar(&flagPull, "pull", "missing", "pull the image: missing, always or never")
}

func run(cmd *cobra.Command, ref string, cmdArgs []string) error {
	if flagInteractive && !flagTTY {
		return errors.New("-i without -t isn't supported yet; use -it")
	}
	if flagTTY && !flagDetach && !isTerminal(os.Stdin) {
		return errors.New("the input device is not a TTY; drop -t or run from a terminal")
	}
	memory, err := clivm.ParseMemory(flagMemory)
	if err != nil {
		return err
	}
	if flagCPUs == 0 {
		return errors.New("--cpus must be at least 1")
	}
	disk, err := clivm.ParseMemory(flagDisk)
	if err != nil {
		return fmt.Errorf("--disk: %w", err)
	}
	env, err := userEnv()
	if err != nil {
		return err
	}

	img, err := resolveImage(cmd.Context(), ref, flagPull)
	if err != nil {
		return err
	}

	name := flagName
	if name == "" {
		name = generateName(ref)
	}
	if err := clivm.ValidateName(name); err != nil {
		return err
	}

	var entrypoint *string
	if cmd.Flags().Changed("entrypoint") {
		entrypoint = &flagEntrypoint
	}
	spec := ci.Spec{
		Args:     ci.Command(img.Config.Entrypoint, img.Config.Cmd, entrypoint, cmdArgs),
		Env:      ci.MergeEnv(img.Config.Env, env...),
		Workdir:  firstNonEmpty(flagWorkdir, img.Config.WorkingDir),
		User:     firstNonEmpty(flagUser, img.Config.User),
		Hostname: name,
		TTY:      flagTTY,
	}
	if len(spec.Args) == 0 {
		return fmt.Errorf("no command specified: %s has no entrypoint or cmd", ref)
	}

	cfg := vm.VMConfig{
		CPUs:        flagCPUs,
		Memory:      memory,
		NetworkMode: mnet.NetworkUser,
		Image:       vm.ImageContainer,
		Container: &vm.ContainerConfig{
			Image:      oci.Display(mustNormalize(ref)),
			ImageID:    img.ID,
			Spec:       spec,
			StopSignal: img.Config.StopSignal,
			Created:    time.Now(),
			Attach:     !flagDetach,
			AutoRemove: flagRm && flagDetach,
		},
	}
	store, err := oci.Open()
	if err != nil {
		return err
	}
	if minimum, err := store.FilesystemSize(img.ID); err == nil && int64(disk) < minimum {
		return fmt.Errorf("--disk must be at least %s for %s", clivm.FormatMemory(uint64(minimum)), ref)
	}
	cfg.Container.DiskSize = int64(disk)
	if err := vm.CreateContainer(name, cfg, store.DiskPath(img.ID)); err != nil {
		return err
	}
	if err := boot(name); err != nil {
		if flagRm {
			removeDir(name)
		}
		return err
	}
	if flagDetach {
		fmt.Println(name)
		return nil
	}
	return attachForeground(name, flagTTY, flagRm)
}

// boot prepares (downloading Kata's kernel on first use) and starts the
// container's VM daemon.
func boot(name string) error {
	image, err := vm.LookupImage(vm.ImageContainer)
	if err != nil {
		return err
	}
	if _, err := image.Prepare(name); err != nil {
		return err
	}
	logPath, err := vm.VMConsoleLogPath(name)
	if err != nil {
		return err
	}
	return vm.SpawnVMDaemon(name, false, logPath)
}

// attachForeground attaches until the container exits, then exits with its
// code (removing the container first with rm).
func attachForeground(name string, tty, rm bool) error {
	detached, err := vm.AttachContainer(name, tty)
	if err != nil {
		return err
	}
	if detached {
		fmt.Fprintf(os.Stderr, "\nDetached from %s; it keeps running.\n", name)
		return nil
	}
	st, err := vm.WaitContainerExit(name, 30*time.Second)
	if err != nil {
		return err
	}
	if rm {
		removeDir(name)
	}
	if st.Error != "" {
		fmt.Fprintf(os.Stderr, "monolith: %s\n", st.Error)
	}
	if st.Code != 0 {
		os.Exit(st.Code)
	}
	return nil
}

// resolveImage finds ref locally or pulls it, per the --pull policy.
func resolveImage(ctx context.Context, ref, policy string) (oci.Image, error) {
	store, err := oci.Open()
	if err != nil {
		return oci.Image{}, err
	}
	switch policy {
	case "missing", "always", "never":
	default:
		return oci.Image{}, fmt.Errorf("--pull must be missing, always or never")
	}
	if policy != "always" {
		img, err := store.Lookup(ref)
		if err == nil {
			return img, nil
		}
		if !errors.Is(err, oci.ErrNotFound) {
			return oci.Image{}, err
		}
		if policy == "never" {
			return oci.Image{}, fmt.Errorf("image %s isn't local (and --pull never)", ref)
		}
		fmt.Fprintf(os.Stderr, "Unable to find image %s locally\n", ref)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return store.Pull(ctx, ref, oci.PullOptions{Log: os.Stderr})
}

// userEnv collects -e and --env-file values; a bare KEY copies the Mac's
// value (and is dropped if unset), as docker run does.
func userEnv() ([]string, error) {
	var out []string
	add := func(kv string) {
		if strings.Contains(kv, "=") {
			out = append(out, kv)
		} else if v, ok := os.LookupEnv(kv); ok {
			out = append(out, kv+"="+v)
		}
	}
	for _, file := range flagEnvFile {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				add(line)
			}
		}
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	for _, kv := range flagEnv {
		add(kv)
	}
	return out, nil
}

// generateName makes "<image>-<4 hex>", e.g. "nginx-3f9a".
func generateName(ref string) string {
	base := path.Base(ref)
	if i := strings.IndexAny(base, ":@"); i >= 0 {
		base = base[:i]
	}
	var b strings.Builder
	for _, c := range strings.ToLower(base) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case b.Len() > 0:
			b.WriteByte('-')
		}
	}
	prefix := strings.Trim(b.String(), "-")
	if prefix == "" || len(prefix) > 40 {
		prefix = "c"
	}
	for {
		suffix := make([]byte, 2)
		rand.Read(suffix)
		name := prefix + "-" + hex.EncodeToString(suffix)
		if dir, err := vm.VMDir(name); err == nil {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return name
			}
		}
	}
}

func mustNormalize(ref string) string {
	if n, err := oci.Normalize(ref); err == nil {
		return n
	}
	return ref
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func isTerminal(f *os.File) bool {
	var t unix.Termios
	return termios.Tcgetattr(f.Fd(), &t) == nil
}

// removeDir deletes a stopped container's directory.
func removeDir(name string) error {
	dir, err := vm.VMDir(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

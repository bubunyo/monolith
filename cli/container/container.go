// Package container implements `monolith container`: each container runs in
// its own small VM (Kata's kernel, monolith-init as PID 1, the image's
// filesystem as root), in the style of Apple's container tool.
package container

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bubunyo/monolith/vm"
	"github.com/spf13/cobra"
)

// Cmd is the parent command for containers.
var Cmd = &cobra.Command{
	Use:     "container",
	Aliases: []string{"c"},
	Short:   "Run containers, each in its own VM",
	Long: `Run OCI container images, each in its own lightweight VM.

A container VM boots Kata's Linux kernel with monolith's own init as PID 1 and
the image's filesystem as its root disk (an APFS clone of the converted image,
so it's instant and only takes space for what the container writes). There's
no Docker engine involved, and containers share nothing but the Mac.

Images are pulled from registries (Docker Hub by default; logins come from
~/.docker/config.json) and converted once into ~/.cache/monolith/images.

Containers are VMs too: 'monolith vm list' shows them with IMAGE
"container (<image>)".`,
}

func init() {
	Cmd.AddCommand(runCmd, psCmd, logsCmd, stopCmd, startCmd, rmCmd, attachCmd, pruneCmd, pullCmd, imagesCmd, rmiCmd, imageCmd)
}

// info is a container and its current state.
type info struct {
	name   string
	cfg    vm.VMConfig
	status *vm.VMStatus
	exit   vm.ExitStatus
	exited bool
}

func (c info) running() bool {
	return c.status.State == vm.StatusRunning || c.status.State == vm.StatusPaused
}

// statusText is the STATUS column, like Docker's.
func (c info) statusText() string {
	switch c.status.State {
	case vm.StatusRunning:
		return "Up " + ago(c.status.StartedAt)
	case vm.StatusPaused:
		return "Up " + ago(c.status.StartedAt) + " (Paused)"
	}
	if c.exited {
		return fmt.Sprintf("Exited (%d) %s ago", c.exit.Code, ago(c.exit.FinishedAt))
	}
	return "Created"
}

// lookup returns container name, or an error if there's no such container.
func lookup(name string) (info, error) {
	cfg, err := vm.ReadConfig(name)
	dir, _ := vm.VMDir(name)
	if _, statErr := os.Stat(dir); err != nil || statErr != nil {
		return info{}, fmt.Errorf("no such container: %s", name)
	}
	if cfg.Container == nil {
		return info{}, fmt.Errorf("%s is a VM, not a container (use 'monolith vm')", name)
	}
	status, err := vm.ReadState(name)
	if err != nil {
		return info{}, err
	}
	c := info{name: name, cfg: cfg, status: status}
	c.exit, c.exited = vm.ReadExit(name)
	return c, nil
}

// list returns all containers, newest first.
func list() ([]info, error) {
	dir, err := vm.VMsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if c, err := lookup(e.Name()); err == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cfg.Container.Created.After(out[j].cfg.Container.Created) })
	return out, nil
}

// ago formats the time since t like Docker: "3 seconds", "5 minutes".
func ago(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Minute:
		return plural(int(d.Seconds()), "second")
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

// quoteCommand is the COMMAND column: the process's arguments, truncated.
func quoteCommand(args []string) string {
	s := strings.Join(args, " ")
	if len(s) > 30 {
		s = s[:29] + "…"
	}
	return `"` + s + `"`
}

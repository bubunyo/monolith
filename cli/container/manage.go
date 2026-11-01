package container

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/bubunyo/monolith"
	"github.com/bubunyo/monolith/vm"
	"github.com/spf13/cobra"
)

var (
	flagAll     bool
	flagFollow  bool
	flagTail    int
	flagTimeout int
	flagAttach  bool
	flagForce   bool
	flagQuiet   bool
)

var psCmd = &cobra.Command{
	Use:     "ps",
	Aliases: []string{"ls", "list"},
	Short:   "List containers",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		containers, err := list()
		if err != nil {
			return err
		}
		if flagQuiet {
			for _, c := range containers {
				if flagAll || c.running() {
					fmt.Println(c.name)
				}
			}
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "NAME\tIMAGE\tCOMMAND\tCREATED\tSTATUS")
		for _, c := range containers {
			if !flagAll && !c.running() {
				continue
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\t%s\n", c.name, c.cfg.Container.Image,
				quoteCommand(c.cfg.Container.Spec.Args), ago(c.cfg.Container.Created), c.statusText())
		}
		return w.Flush()
	},
}

var logsCmd = &cobra.Command{
	Use:   "logs NAME",
	Short: "Show a container's output",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := lookup(args[0]); err != nil {
			return err
		}
		path, err := vm.VMConsoleLogPath(args[0])
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil // never started
		}
		return monolith.StreamLogFile(path, os.Stdout, monolith.TailOptions{Lines: flagTail, Follow: flagFollow})
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop NAME...",
	Short: "Stop containers (SIGTERM, then SIGKILL after --time)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return each(args, true, func(c info) error {
			if !c.running() {
				return nil
			}
			return stop(c.name, flagTimeout)
		})
	},
}

var startCmd = &cobra.Command{
	Use:   "start NAME...",
	Short: "Start stopped containers (their filesystem is kept)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagAttach && len(args) > 1 {
			return errors.New("--attach takes one container")
		}
		return each(args, false, func(c info) error {
			if c.running() {
				return nil
			}
			c.cfg.Container.Attach = flagAttach
			if err := vm.WriteConfig(c.name, c.cfg); err != nil {
				return err
			}
			if err := boot(c.name); err != nil {
				return err
			}
			if flagAttach {
				return attachForeground(c.name, c.cfg.Container.Spec.TTY, false)
			}
			fmt.Println(c.name)
			return nil
		})
	},
}

var rmCmd = &cobra.Command{
	Use:   "rm NAME...",
	Short: "Remove containers (-f stops running ones first)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return each(args, true, func(c info) error {
			if c.running() || c.status.State == vm.StatusSuspended {
				if !flagForce {
					return fmt.Errorf("container %s is running; stop it first or use -f", c.name)
				}
				if c.running() {
					// Docker's rm -f kills without a grace period.
					if _, err := vm.SendControlCommand(c.name, vm.ControlRequest{Cmd: vm.CmdStop, Force: true}); err != nil {
						return err
					}
					if err := vm.WaitDaemonExit(c.name, 30*time.Second); err != nil {
						return err
					}
				}
			}
			return removeDir(c.name)
		})
	},
}

var attachCmd = &cobra.Command{
	Use:   "attach NAME",
	Short: "Attach to a running container's console",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := lookup(args[0])
		if err != nil {
			return err
		}
		if !c.running() {
			return fmt.Errorf("container %s isn't running", c.name)
		}
		return attachForeground(c.name, c.cfg.Container.Spec.TTY, false)
	},
}

// stop stops a running container and waits for it to exit.
func stop(name string, timeout int) error {
	resp, err := vm.SendControlCommand(name, vm.ControlRequest{Cmd: vm.CmdStop, Timeout: timeout})
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	_, err = vm.WaitContainerExit(name, 30*time.Second)
	return err
}

// each runs fn for every named container, printing the names it handled
// if print (as docker stop and rm do), and reports all failures.
func each(names []string, print bool, fn func(info) error) error {
	var errs []error
	for _, name := range names {
		c, err := lookup(name)
		if err == nil {
			err = fn(c)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			errs = append(errs, err)
			continue
		}
		if print {
			fmt.Println(name)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d failed", len(errs), len(names))
	}
	return nil
}

func init() {
	psCmd.Flags().BoolVarP(&flagAll, "all", "a", false, "show stopped containers too")
	psCmd.Flags().BoolVarP(&flagQuiet, "quiet", "q", false, "only print names")
	logsCmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "keep following new output")
	logsCmd.Flags().IntVarP(&flagTail, "tail", "n", 0, "number of lines from the end (0: all)")
	stopCmd.Flags().IntVarP(&flagTimeout, "time", "t", 10, "seconds to wait before killing")
	startCmd.Flags().BoolVarP(&flagAttach, "attach", "a", false, "attach to its output and exit with its code")
	rmCmd.Flags().BoolVarP(&flagForce, "force", "f", false, "kill and remove running containers")
}

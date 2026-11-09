package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	mnet "github.com/bubunyo/monolith/net"
	"github.com/bubunyo/monolith/update"
	"github.com/bubunyo/monolith/vm"
	"github.com/spf13/cobra"
)

var (
	flagUpdateCheck   bool
	flagUpdateVersion string
	flagUpdateForce   bool
)

// Tests replace these: the signature check of a downloaded binary, and the
// binary to replace.
var (
	verifySignature = update.VerifySignature
	updateTarget    = update.Target
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update monolith to the latest release",
	Long: `Download the latest monolith release (or the one --version names) and
replace this binary with it. The archive is checked against the release's
checksums, and the binary's code signature and virtualization entitlement
are verified before anything is replaced. sudo is used when the binary's
directory isn't writable.

Running VMs and network daemons keep the old binary until restarted.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runUpdate()
	},
}

func init() {
	updateCmd.Flags().BoolVar(&flagUpdateCheck, "check", false, "only report whether an update is available")
	updateCmd.Flags().StringVar(&flagUpdateVersion, "version", "", "release to install, e.g. v0.1.0 (default: latest)")
	updateCmd.Flags().BoolVar(&flagUpdateForce, "force", false, "replace a development build, or reinstall the same version")
}

func runUpdate() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := &http.Client{}

	target := update.Canonical(flagUpdateVersion)
	latest := target == ""
	if latest {
		var err error
		if target, err = update.Latest(ctx, client); err != nil {
			return err
		}
	} else if !update.IsRelease(target) {
		return fmt.Errorf("%q isn't a release version (like v0.1.0)", flagUpdateVersion)
	}

	release := update.IsRelease(version)
	if flagUpdateCheck {
		switch {
		case !release:
			fmt.Printf("monolith %s is a development build; the latest release is %s\n", version, target)
		case update.Compare(version, target) < 0:
			fmt.Printf("Update available: %s → %s (run 'monolith update')\n", version, target)
		default:
			fmt.Printf("monolith %s is up to date\n", version)
		}
		return nil
	}

	if !flagUpdateForce {
		if !release {
			return fmt.Errorf("monolith %s is a development build; use --force to replace it with %s", version, target)
		}
		if c := update.Compare(version, target); c == 0 || (latest && c > 0) {
			fmt.Printf("monolith %s is up to date\n", version)
			return nil
		}
	}

	dst, err := updateTarget()
	if err != nil {
		return fmt.Errorf("finding the monolith binary: %w", err)
	}
	dir, err := os.MkdirTemp("", "monolith-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	bin, err := update.Download(ctx, client, target, dir, os.Stdout)
	if err != nil {
		return err
	}
	if err := verifySignature(bin); err != nil {
		return err
	}
	fmt.Println("Verified checksum and signature")

	if _, err := update.Replace(bin, dst); err != nil {
		return err
	}
	fmt.Printf("Updated %s: %s → %s\n", dst, version, installedVersion(dst, target))
	warnRunning(target)
	return nil
}

// installedVersion asks the new binary its version, falling back to want.
func installedVersion(path, want string) string {
	cmd := exec.Command(path, "--version")
	cmd.Env = append(os.Environ(), update.EnvNoCheck+"=1")
	out, err := cmd.Output()
	if err != nil {
		return want
	}
	f := strings.Fields(string(out))
	if len(f) >= 3 {
		return f[2] // "monolith version vX"
	}
	return want
}

// warnRunning lists the VMs and networks whose daemons still run the
// previous binary.
func warnRunning(target string) {
	vms, nets := runningDaemons()
	if len(vms) == 0 && len(nets) == 0 {
		return
	}
	var parts []string
	if len(vms) > 0 {
		parts = append(parts, plural(len(vms), "VM", "VMs")+" ("+strings.Join(vms, ", ")+")")
	}
	if len(nets) > 0 {
		parts = append(parts, plural(len(nets), "network", "networks")+" ("+strings.Join(nets, ", ")+")")
	}
	fmt.Printf("Still running: %s. They keep the binary they started with; restart them to use %s.\n", strings.Join(parts, " and "), target)
	if len(nets) > 0 {
		fmt.Println("A VM can only join a named network started by the same build, so restart the networks first.")
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// runningDaemons returns the VMs that are running or paused and the named
// networks whose daemon is up.
func runningDaemons() (vms, nets []string) {
	if dir, err := vm.VMsDir(); err == nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if st, err := vm.ReadState(e.Name()); err == nil && (st.State == vm.StatusRunning || st.State == vm.StatusPaused) {
				vms = append(vms, e.Name())
			}
		}
	}
	if dir, err := mnet.NetworksDir(); err == nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && mnet.IsNetworkDaemonAlive(e.Name()) {
				nets = append(nets, e.Name())
			}
		}
	}
	return vms, nets
}

// versionNote is what 'monolith --version' adds after the version: whether
// it's the latest release. It's empty when the check is off or fails.
func versionNote(current string, latest func() (string, error)) string {
	if os.Getenv(update.EnvNoCheck) != "" {
		return "\n"
	}
	tag, err := latest()
	if err != nil {
		return "\n"
	}
	switch c := update.Compare(current, tag); {
	case !update.IsRelease(current):
		return fmt.Sprintf(" (development build; latest release is %s)\n", tag)
	case c < 0:
		return fmt.Sprintf("\nUpdate available: %s (run 'monolith update')\n", tag)
	case c > 0:
		return fmt.Sprintf(" (newer than the latest release, %s)\n", tag)
	default:
		return " (latest)\n"
	}
}

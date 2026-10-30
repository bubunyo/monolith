package net

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Named networks are shared between processes through a launchd Mach
// service: vmnet.ShareNetwork only answers on a name that launchd has
// registered for the publishing process. Each network daemon therefore runs
// as a per-user launchd job (gui/<uid> domain) whose plist lives in the
// network's directory. The job is loaded with `launchctl bootstrap` and
// unloaded with `launchctl bootout`; nothing is installed in
// ~/Library/LaunchAgents, so jobs do not survive logout.

const (
	networkPlistFileName  = "launchd.plist"
	networkReceiveTimeout = 5 * time.Second
	networkStopTimeout    = 5 * time.Second
)

// NetworkServiceName returns the launchd label and Mach service name used by
// the daemon for the named network.
func NetworkServiceName(name string) string {
	return "dev.monolith.network." + name
}

func launchdDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// StartNetworkDaemon loads the launchd job for the named network, which
// starts `monolith __netd <name> <mode>` from the current executable. Any
// stale job with the same label is unloaded first. It returns once launchd
// accepts the job; callers should wait for IsNetworkDaemonAlive.
func StartNetworkDaemon(name, mode string, verbose bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	dir, err := NetworkDir(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	logPath, err := NetworkLogPath(name)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	args := []string{exe, "__netd", name, mode}
	if verbose {
		args = append(args, "--verbose")
	}

	plistPath := filepath.Join(dir, networkPlistFileName)
	if err := os.WriteFile(plistPath, networkPlist(NetworkServiceName(name), args, logPath, home), 0644); err != nil {
		return fmt.Errorf("writing launchd plist: %w", err)
	}

	// A previous daemon may have exited without being unloaded.
	exec.Command("launchctl", "bootout", launchdDomain()+"/"+NetworkServiceName(name)).Run()

	if out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %v: %s", plistPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StopNetworkDaemon unloads the launchd job for the named network, which
// sends the daemon SIGTERM, and waits for it to exit. It reports whether a
// running daemon was stopped.
func StopNetworkDaemon(name string) (bool, error) {
	wasAlive := IsNetworkDaemonAlive(name)

	// bootout fails when the job is not loaded; that is not an error here.
	exec.Command("launchctl", "bootout", launchdDomain()+"/"+NetworkServiceName(name)).Run()

	deadline := time.Now().Add(networkStopTimeout)
	for IsNetworkDaemonAlive(name) {
		if time.Now().After(deadline) {
			return wasAlive, fmt.Errorf("network daemon %q did not exit within %s", name, networkStopTimeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	CleanNetworkPid(name)
	return wasAlive, nil
}

// networkPlist renders the launchd job definition for a network daemon.
func networkPlist(label string, args []string, logPath, home string) []byte {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}

	var argXML strings.Builder
	for _, a := range args {
		fmt.Fprintf(&argXML, "\t\t<string>%s</string>\n", esc(a))
	}

	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%[1]s</string>
	<key>ProgramArguments</key>
	<array>
%[2]s	</array>
	<key>MachServices</key>
	<dict>
		<key>%[1]s</key>
		<true/>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>%[4]s</string>
	</dict>
	<key>StandardOutPath</key>
	<string>%[3]s</string>
	<key>StandardErrorPath</key>
	<string>%[3]s</string>
</dict>
</plist>
`, esc(label), argXML.String(), esc(logPath), esc(home)))
}

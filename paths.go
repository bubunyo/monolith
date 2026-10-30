package monolith

import (
	"os"
	"path/filepath"
)

// cachePath returns a path under the monolith cache root, ~/.cache/monolith.
// It does not create anything.
func cachePath(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home, ".cache", "monolith"}, elem...)...), nil
}

// VMsDir returns the parent directory containing all per-VM directories.
func VMsDir() (string, error) {
	return cachePath("vms")
}

// VMDir returns the per-VM cache directory path.
func VMDir(name string) (string, error) {
	return cachePath("vms", name)
}

// VMSockPath returns the path to the VM daemon's Unix domain socket.
func VMSockPath(name string) (string, error) {
	return cachePath("vms", name, "daemon.sock")
}

// VMSuspendPath returns the path where a VM's state is saved on suspend.
func VMSuspendPath(name string) (string, error) {
	return cachePath("vms", name, "suspend.vmstate")
}

// VMLogPath returns the path to the VM daemon's log file.
func VMLogPath(name string) (string, error) {
	return cachePath("vms", name, "daemon.log")
}

// NetworksDir returns the parent directory for all named networks.
func NetworksDir() (string, error) {
	return cachePath("networks")
}

// NetworkDir returns the path to the named network's directory.
func NetworkDir(name string) (string, error) {
	return cachePath("networks", name)
}

// NetworkLogPath returns the path to the network daemon's log file.
func NetworkLogPath(name string) (string, error) {
	return cachePath("networks", name, networkLogFileName)
}

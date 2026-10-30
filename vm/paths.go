package vm

import "github.com/bubunyo/monolith"

// VMsDir returns the parent directory containing all per-VM directories.
func VMsDir() (string, error) {
	return monolith.CachePath("vms")
}

// VMDir returns the per-VM cache directory path.
func VMDir(name string) (string, error) {
	return monolith.CachePath("vms", name)
}

// VMSockPath returns the path to the VM daemon's Unix domain socket.
func VMSockPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "daemon.sock")
}

// VMSuspendPath returns the path where a VM's state is saved on suspend.
func VMSuspendPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "suspend.vmstate")
}

// VMLogPath returns the path to the VM daemon's log file.
func VMLogPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "daemon.log")
}

// VMMachineIDPath returns the path of the VM's persisted machine identifier.
func VMMachineIDPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "machine-identifier")
}

// VMDockerSockPath returns the host socket relayed to dockerd in a docker
// image VM; point DOCKER_HOST at it.
func VMDockerSockPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "docker.sock")
}

// VMSSHKeyPath returns the private key for a containerd engine VM's
// internal sshd; the public half is at the same path plus ".pub".
func VMSSHKeyPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "ssh_key")
}

// VMSSHSockPath returns the host socket relayed to a containerd engine VM's
// internal sshd (use with ssh -o ProxyCommand="nc -U <path>").
func VMSSHSockPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "ssh.sock")
}

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

// VMKubeDir returns the directory shared into a cluster VM (read-write);
// k3s writes its kubeconfig there.
func VMKubeDir(name string) (string, error) {
	return monolith.CachePath("vms", name, "kube")
}

// VMKubeconfigPath returns the kubeconfig k3s writes in a cluster VM, with
// the server at https://127.0.0.1:6443 (as seen from the guest).
func VMKubeconfigPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "kube", "kubeconfig")
}

// VMSSHKeyPath returns the private key for a containerd engine VM's
// internal sshd; the public half is at the same path plus ".pub".
func VMSSHKeyPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "ssh_key")
}

// VMLoadSockPath returns the host socket relayed to a cluster VM's image
// load service (`monolith cluster load` writes an image tarball to it).
func VMLoadSockPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "load.sock")
}

// VMSSHSockPath returns the host socket relayed to a containerd engine VM's
// internal sshd (use with ssh -o ProxyCommand="nc -U <path>").
func VMSSHSockPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "ssh.sock")
}

package vm

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DockerAPIPort is the guest TCP port relayed to dockerd's socket. The
	// guest only accepts connections to it from the user-mode gateway, so
	// containers can't reach it.
	DockerAPIPort = 2375

	// NerdctlPortsPort is the guest TCP port that answers each connection
	// with `nerdctl ps --format json` (containerd engine), restricted to the
	// gateway like DockerAPIPort. The daemon reads published ports from it.
	NerdctlPortsPort = 2377

	// SSHPort is the guest's internal sshd (containerd engine), used by
	// `monolith docker nerdctl` through the daemon's ssh.sock.
	SSHPort = 22

	dockerDataDiskName  = "data.img"
	dockerDataDiskSize  = 64 << 30 // sparse; grows as images are pulled
	dockerOverlay       = "docker/manifest.yaml"
	containerdOverlay   = "containerd/manifest.yaml"
	sshPubKeyCmdlineArg = "monolith.ssh_pubkey"
)

// Container engines for the docker image.
const (
	EngineDocker     = "docker"
	EngineContainerd = "containerd"
)

// ValidEngines is the set of accepted --engine values.
var ValidEngines = []string{EngineDocker, EngineContainerd}

// EngineOf returns the VM's container engine; configs from before engines
// existed are docker.
func EngineOf(cfg VMConfig) string {
	if cfg.Engine == "" {
		return EngineDocker
	}
	return cfg.Engine
}

// dockerImage is a container host: the Alpine image plus the files in
// embeddings/docker, which install the engine on first boot and run it.
// Images and containers go on a separate sparse data disk (/dev/vdb) and the
// Mac's /Users is shared over virtio-fs at the same path so bind mounts
// resolve. The daemon forwards published ports to localhost and, depending
// on the engine, exposes dockerd's API as docker.sock (docker) or the
// guest's internal sshd as ssh.sock for nerdctl (containerd, which adds the
// embeddings/containerd overlay and a per-VM SSH key). All of it goes
// through the user-mode network, which the image therefore requires.
type dockerImage struct{}

func (dockerImage) Name() string { return ImageDocker }

func (dockerImage) Prepare(name string) (*BootSpec, error) {
	kernel, err := DefaultKernel()
	if err != nil {
		return nil, err
	}
	rootfs, err := DefaultRootFS()
	if err != nil {
		return nil, err
	}
	cfg, err := ReadConfig(name)
	if err != nil {
		return nil, err
	}
	overlays := []string{dockerOverlay}
	commandLine := alpineCommandLine
	if EngineOf(cfg) == EngineContainerd {
		overlays = append(overlays, containerdOverlay)
		pub, err := ensureSSHKey(name)
		if err != nil {
			return nil, fmt.Errorf("SSH key: %w", err)
		}
		// The guest installs it as root's authorized key on every boot.
		commandLine += " " + sshPubKeyCmdlineArg + "=" + pub
	}
	disk, err := DefaultDisk(rootfs, kernel.ModloopPath, name, overlays...)
	if err != nil {
		return nil, err
	}

	vmDir, err := VMDir(name)
	if err != nil {
		return nil, err
	}
	dataDisk := filepath.Join(vmDir, dockerDataDiskName)
	if !exists(dataDisk) {
		// Left unformatted; docker-setup runs mkfs.ext4 on first boot.
		if err := createSparseFile(dataDisk, dockerDataDiskSize); err != nil {
			return nil, fmt.Errorf("creating docker data disk: %w", err)
		}
	}

	return &BootSpec{
		KernelPath:   kernel.KernelPath,
		InitrdPath:   kernel.InitrdPath,
		CommandLine:  commandLine,
		DiskPath:     disk,
		DataDiskPath: dataDisk,
		Shares:       []Share{{Tag: "users", HostPath: "/Users"}},
	}, nil
}

func createSparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

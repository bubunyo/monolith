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

	dockerDataDiskName = "data.img"
	dockerDataDiskSize = 64 << 30 // sparse; grows as images are pulled
	dockerOverlay      = "docker/manifest.yaml"
)

// dockerImage is a Docker host: the Alpine image plus the files in
// embeddings/docker, which install Docker on first boot and run dockerd.
// Images and containers go on a separate sparse data disk (/dev/vdb, on
// /var/lib/docker) and the Mac's /Users is shared over virtio-fs at the same
// path so bind mounts resolve. The daemon exposes dockerd's API on the host
// as docker.sock and forwards published ports to localhost, both through the
// user-mode network, which it therefore requires.
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
	disk, err := DefaultDisk(rootfs, kernel.ModloopPath, name, dockerOverlay)
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
		CommandLine:  alpineCommandLine,
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

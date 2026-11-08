package vm

import (
	"io"
	"path/filepath"
)

// alpineCommandLine boots the stock Alpine initramfs into the per-VM ext4
// disk; modules come from the modloop squashfs copied onto the disk.
const alpineCommandLine = "console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop"

// alpineImage is the original monolith guest: Alpine's virt kernel and
// initramfs from the release ISO, plus a persistent, writable 1 GiB ext4
// disk per VM populated from the minirootfs (user mono, autologin, sudo).
type alpineImage struct{}

func (alpineImage) Name() string { return ImageAlpine }

func (alpineImage) Prepare(name string, log io.Writer) (*BootSpec, error) {
	kernel, err := DefaultKernel(log)
	if err != nil {
		return nil, err
	}
	rootfs, err := DefaultRootFS(log)
	if err != nil {
		return nil, err
	}
	vmDir, err := VMDir(name)
	if err != nil {
		return nil, err
	}
	newDisk := !exists(filepath.Join(vmDir, alpineDiskName))
	disk, err := DefaultDisk(rootfs, kernel.ModloopPath, name, log)
	if err != nil {
		return nil, err
	}
	// Record which guest files the disk got, as the docker image does.
	if newDisk {
		cfg, err := ReadConfig(name)
		if err != nil {
			return nil, err
		}
		cfg.GuestLayout = GuestLayout
		if err := WriteConfig(name, cfg); err != nil {
			return nil, err
		}
	}
	return &BootSpec{
		KernelPath:  kernel.KernelPath,
		InitrdPath:  kernel.InitrdPath,
		CommandLine: alpineCommandLine,
		DiskPath:    disk,
	}, nil
}

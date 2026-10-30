package vm

// alpineCommandLine boots the stock Alpine initramfs into the per-VM ext4
// disk; modules come from the modloop squashfs copied onto the disk.
const alpineCommandLine = "console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop"

// alpineImage is the original monolith guest: Alpine's virt kernel and
// initramfs from the release ISO, plus a persistent, writable 1 GiB ext4
// disk per VM populated from the minirootfs (user mono, autologin, sudo).
type alpineImage struct{}

func (alpineImage) Name() string { return ImageAlpine }

func (alpineImage) Prepare(name string) (*BootSpec, error) {
	kernel, err := DefaultKernel()
	if err != nil {
		return nil, err
	}
	rootfs, err := DefaultRootFS()
	if err != nil {
		return nil, err
	}
	disk, err := DefaultDisk(rootfs, kernel.ModloopPath, name)
	if err != nil {
		return nil, err
	}
	return &BootSpec{
		KernelPath:  kernel.KernelPath,
		InitrdPath:  kernel.InitrdPath,
		CommandLine: alpineCommandLine,
		DiskPath:    disk,
	}, nil
}

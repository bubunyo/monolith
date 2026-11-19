package vm

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultDataDiskSize is a new docker host's data disk (images,
	// containers, volumes). The file is sparse, so this is a ceiling, not
	// space taken up front. VMs created before data_disk_size existed got
	// 64 GiB.
	DefaultDataDiskSize = 20 << 30
	// MinDataDiskSize is the smallest --disk accepted.
	MinDataDiskSize = 4 << 30

	// GuestLayoutDiskResize is the first guest layout whose docker-setup
	// grows the data disk's filesystem to the disk's size on boot. (Layout 5
	// is the /var/folders share, GuestLayoutTempShare, which doesn't resize.)
	GuestLayoutDiskResize = 6
)

// DataDiskSize returns the size of VM name's docker data disk, or 0 if it
// hasn't been created yet.
func DataDiskSize(name string) (int64, error) {
	dir, err := VMDir(name)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(filepath.Join(dir, dockerDataDiskName))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// CheckDataDiskSize reports whether VM name's data disk can be set to size:
// at least MinDataDiskSize, never smaller than the disk already is (ext4
// can't shrink online and the space may be in use), and growing an existing
// disk needs a guest that resizes its filesystem.
func CheckDataDiskSize(name string, cfg VMConfig, size int64) error {
	if size < MinDataDiskSize {
		return fmt.Errorf("--disk must be at least %d GiB", MinDataDiskSize>>30)
	}
	current, err := DataDiskSize(name)
	if err != nil {
		return err
	}
	return checkGrow(name, cfg, current, size)
}

func checkGrow(name string, cfg VMConfig, current, size int64) error {
	switch {
	case current == 0 || size == current:
		return nil
	case size < current:
		return fmt.Errorf("%q's data disk is %s; it can grow but not shrink (remove the VM to start smaller)", name, formatGiB(current))
	case cfg.GuestLayout < GuestLayoutDiskResize:
		return fmt.Errorf("%q was created before data disks could grow; recreate it ('monolith docker rm --name %s') to change --disk", name, name)
	}
	return nil
}

// ensureDataDisk creates the data disk at path, or grows it to the size in
// cfg. It returns the disk's size. A config without a size (VMs from before
// data_disk_size) leaves an existing disk alone and creates a new one at
// DefaultDataDiskSize.
func ensureDataDisk(name, path string, cfg VMConfig) (int64, error) {
	want := cfg.DataDiskSize
	if want == 0 {
		want = DefaultDataDiskSize
	}
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		// Left unformatted; docker-setup runs mkfs.ext4 on first boot.
		if err := createSparseFile(path, want); err != nil {
			return 0, fmt.Errorf("creating docker data disk: %w", err)
		}
		return want, nil
	}
	if err != nil {
		return 0, err
	}
	if cfg.DataDiskSize == 0 || cfg.DataDiskSize == fi.Size() {
		return fi.Size(), nil
	}
	if err := checkGrow(name, cfg, fi.Size(), cfg.DataDiskSize); err != nil {
		return 0, err
	}
	// Sparse growth; docker-setup runs resize2fs on the next boot.
	if err := os.Truncate(path, cfg.DataDiskSize); err != nil {
		return 0, fmt.Errorf("growing docker data disk: %w", err)
	}
	return cfg.DataDiskSize, nil
}

func formatGiB(n int64) string {
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", n>>30)
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

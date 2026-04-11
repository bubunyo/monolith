package monolith

import (
	"os"
	"path/filepath"
)

const defaultDiskSize = 1 << 30 // 1 GiB

func DefaultDisk() (string, error) {
	cacheDir, err := cacheDir()
	if err != nil {
		return "", err
	}

	diskPath := filepath.Join(cacheDir, "disk.img")
	if exists(diskPath) {
		return diskPath, nil
	}

	return diskPath, createDiskImage(diskPath, defaultDiskSize)
}

func createDiskImage(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

package monolith

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

const defaultDiskSize = "1g"

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

func createDiskImage(path, size string) error {
	cmd := exec.Command("diskutil", "image", "create", "blank",
		"--fs", "none",
		"--format", "RAW",
		"--size", size,
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("creating disk image: %s: %w", out, err)
	}
	return nil
}

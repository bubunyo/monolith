package monolith

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
)

const (
	alpineVersion = "3.21.0"
	kernelURL     = "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/aarch64/alpine-virt-" + alpineVersion + "-aarch64.iso"
	isoName       = "alpine-virt.iso"
	kernelName    = "vmlinuz-virt"
	initrdName    = "initramfs-virt"
)

type KernelAssets struct {
	KernelPath string
	InitrdPath string
}

func DefaultKernel() (*KernelAssets, error) {
	cacheDir, err := cacheDir()
	if err != nil {
		return nil, err
	}

	kernelPath := filepath.Join(cacheDir, kernelName)
	initrdPath := filepath.Join(cacheDir, initrdName)

	if exists(kernelPath) && exists(initrdPath) {
		return &KernelAssets{KernelPath: kernelPath, InitrdPath: initrdPath}, nil
	}

	isoPath := filepath.Join(cacheDir, isoName)
	if err := downloadISO(isoPath); err != nil {
		return nil, fmt.Errorf("downloading iso: %w", err)
	}
	defer os.Remove(isoPath)

	if err := extractFromISO(isoPath, cacheDir); err != nil {
		return nil, fmt.Errorf("extracting kernel: %w", err)
	}

	return &KernelAssets{KernelPath: kernelPath, InitrdPath: initrdPath}, nil
}

func cacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".cache", "myruntime")
	return dir, os.MkdirAll(dir, 0755)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func downloadISO(dest string) error {
	fmt.Println("Downloading Alpine virt ISO...")
	resp, err := http.Get(kernelURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}
func extractFromISO(isoPath, destDir string) error {
	isoBackend, err := file.OpenFromPath(isoPath, true)
	if err != nil {
		return fmt.Errorf("opening iso: %w", err)
	}
	defer isoBackend.Close()
	disk, err := diskfs.OpenBackend(isoBackend, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return fmt.Errorf("opening iso: %w", err)
	}
	defer disk.Close()

	fs, err := disk.GetFilesystem(0)
	if err != nil {
		return fmt.Errorf("getting filesystem: %w", err)
	}

	for _, name := range []string{kernelName, initrdName} {
		if err := extractFile(fs, "/boot/"+name, filepath.Join(destDir, name)); err != nil {
			return fmt.Errorf("extracting %s: %w", name, err)
		}
	}
	return nil
}

func extractFile(fs filesystem.FileSystem, srcPath, destPath string) error {
	src, err := fs.OpenFile(srcPath, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}

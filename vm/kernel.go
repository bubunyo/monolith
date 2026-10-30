package vm

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bubunyo/monolith"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
)

const (
	alpineVersion    = "3.21.0"
	alpineMinirootfs = "3.21.6"
	kernelURL        = "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/aarch64/alpine-virt-" + alpineVersion + "-aarch64.iso"
	minirootfsURL    = "https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/aarch64/alpine-minirootfs-" + alpineMinirootfs + "-aarch64.tar.gz"
	isoName          = "alpine-virt.iso"
	kernelName       = "vmlinuz-virt"
	initrdName       = "initramfs-virt"
	modloopName      = "modloop-virt"
	minirootfsName   = "minirootfs.tar.gz"
)

type KernelAssets struct {
	KernelPath  string
	InitrdPath  string
	ModloopPath string
}

func DefaultKernel() (*KernelAssets, error) {
	alpineDir, err := alpineCacheDir()
	if err != nil {
		return nil, err
	}

	kernelPath := filepath.Join(alpineDir, kernelName)
	initrdPath := filepath.Join(alpineDir, initrdName)
	modloopPath := filepath.Join(alpineDir, modloopName)

	if exists(kernelPath) && exists(initrdPath) && exists(modloopPath) {
		return &KernelAssets{
			KernelPath:  kernelPath,
			InitrdPath:  initrdPath,
			ModloopPath: modloopPath,
		}, nil
	}

	isoPath := filepath.Join(alpineDir, isoName)
	if err := downloadISO(isoPath); err != nil {
		return nil, fmt.Errorf("downloading iso: %w", err)
	}
	defer os.Remove(isoPath)

	if err := extractFromISO(isoPath, alpineDir); err != nil {
		return nil, fmt.Errorf("extracting kernel: %w", err)
	}

	return &KernelAssets{
		KernelPath:  kernelPath,
		InitrdPath:  initrdPath,
		ModloopPath: modloopPath,
	}, nil
}

// DefaultRootFS returns the path to the cached Alpine minirootfs tarball,
// downloading it on first use.
func DefaultRootFS() (string, error) {
	alpineDir, err := alpineCacheDir()
	if err != nil {
		return "", err
	}

	rootfsPath := filepath.Join(alpineDir, minirootfsName)
	if exists(rootfsPath) {
		return rootfsPath, nil
	}

	fmt.Println("Downloading Alpine minirootfs...")
	resp, err := http.Get(minirootfsURL)
	if err != nil {
		return "", fmt.Errorf("downloading minirootfs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading minirootfs: HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(rootfsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(rootfsPath)
		return "", fmt.Errorf("writing minirootfs: %w", err)
	}

	return rootfsPath, nil
}

// alpineCacheDir returns the path for shared Alpine assets (kernel, initrd,
// minirootfs) scoped under the "alpine" subdirectory of the cache.
func alpineCacheDir() (string, error) {
	dir, err := monolith.CachePath("alpine")
	if err != nil {
		return "", err
	}
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

	for _, name := range []string{kernelName, initrdName, modloopName} {
		if err := extractFile(fs, "/boot/"+name, filepath.Join(destDir, name)); err != nil {
			return fmt.Errorf("extracting %s: %w", name, err)
		}
	}

	// The Alpine aarch64 vmlinuz-virt is an EFI stub with a gzip-compressed
	// ARM64 Image payload. VZLinuxBootLoader needs the raw Image, so we
	// decompress it in place.
	if err := decompressKernel(filepath.Join(destDir, kernelName)); err != nil {
		return fmt.Errorf("decompressing kernel: %w", err)
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

// decompressKernel extracts the raw ARM64 Image from an EFI-stub vmlinuz.
// The EFI stub wraps a gzip stream (magic 0x1f 0x8b) containing the
// uncompressed kernel that VZLinuxBootLoader requires.
func decompressKernel(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	gzipMagic := []byte{0x1f, 0x8b}
	idx := bytes.Index(data, gzipMagic)
	if idx < 0 {
		// Not an EFI stub; assume already a raw Image.
		return nil
	}

	gz, err := gzip.NewReader(bytes.NewReader(data[idx:]))
	if err != nil {
		return fmt.Errorf("opening gzip stream at offset %d: %w", idx, err)
	}
	defer gz.Close()

	// Disable multistream so the reader stops after the first gzip member
	// instead of trying to parse trailing EFI stub data as another member.
	gz.Multistream(false)

	image, err := io.ReadAll(gz)
	if err != nil {
		return fmt.Errorf("decompressing kernel: %w", err)
	}

	return os.WriteFile(path, image, 0644)
}

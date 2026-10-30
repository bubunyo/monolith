package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Base image names accepted by `vm run --image` and stored in config.json.
const (
	ImageKata   = "kata"
	ImageAlpine = "alpine"
	ImageDocker = "docker"
)

// DefaultImage is used for new VMs when --image is not given.
const DefaultImage = ImageKata

// ValidImages is the set of accepted --image values.
var ValidImages = []string{ImageKata, ImageAlpine, ImageDocker}

// BootSpec is everything the daemon needs from a base image to boot a VM.
type BootSpec struct {
	KernelPath   string // raw ARM64 Image, as VZLinuxBootLoader requires
	InitrdPath   string // optional
	CommandLine  string
	DiskPath     string // root disk attached as /dev/vda
	DiskReadOnly bool   // true when the disk is a base image shared by all VMs
	DataDiskPath string // optional second writable disk, attached as /dev/vdb
	Shares       []Share
}

// Share is a host directory exposed to the guest over virtio-fs; the guest
// mounts it with `mount -t virtiofs <Tag> <dir>`.
type Share struct {
	Tag      string
	HostPath string
	ReadOnly bool
}

// Image is a guest base image: where the kernel comes from, how the root
// disk is produced, and which kernel command line boots it.
type Image interface {
	Name() string
	// Prepare downloads and caches any shared assets, creates per-VM files
	// if the image needs them, and returns the boot spec for VM name. It is
	// idempotent; `vm run` calls it before spawning the daemon so first-run
	// downloads happen in the foreground, and the daemon calls it again.
	Prepare(name string) (*BootSpec, error)
}

// LookupImage returns the base image with the given name.
func LookupImage(name string) (Image, error) {
	switch name {
	case ImageKata:
		return kataImage{}, nil
	case ImageAlpine:
		return alpineImage{}, nil
	case ImageDocker:
		return dockerImage{}, nil
	default:
		return nil, fmt.Errorf("unknown image %q; must be one of: %s", name, strings.Join(ValidImages, ", "))
	}
}

// Exists reports whether a VM directory exists for name. A VM is a directory
// under VMsDir holding a config.json (written by `vm run`) or a disk.img
// (older Alpine VMs and VMs started directly with __vmd).
func Exists(name string) bool {
	dir, err := VMDir(name)
	if err != nil {
		return false
	}
	for _, f := range []string{configFileName, alpineDiskName} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

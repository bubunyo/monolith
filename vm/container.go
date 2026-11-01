package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bubunyo/monolith"
	ci "github.com/bubunyo/monolith/containerinit"
	"github.com/bubunyo/monolith/vm/guestinit"
	"golang.org/x/sys/unix"
)

// ImageContainer is the base image of `monolith container` VMs: one VM per
// container, booting Kata's kernel with monolith-init as PID 1 and an APFS
// clone of the container image's disk as root. It isn't a `vm run --image`
// choice; `container run` creates these VMs.
const ImageContainer = "container"

// ContainerConfig makes a VM a container (`monolith container`).
type ContainerConfig struct {
	Image   string  `json:"image"`    // reference as the user gave it, for display
	ImageID string  `json:"image_id"` // image store ID the disk was cloned from
	Spec    ci.Spec `json:"spec"`     // the process, handed to monolith-init
	// StopSignal is what `container stop` sends first (the image's
	// STOPSIGNAL, e.g. "SIGQUIT" for nginx); default SIGTERM.
	StopSignal string    `json:"stop_signal,omitempty"`
	Created    time.Time `json:"created"`
	DiskSize   int64     `json:"disk_size"` // bytes (--disk)

	// Attach makes the daemon hold the process until a console client
	// attaches, so a foreground run sees all output. `run`/`start -a` set
	// it; `run -d`/`start` clear it.
	Attach bool `json:"attach,omitempty"`
	// AutoRemove deletes the container when it exits (`run -d --rm`; a
	// foreground --rm is removed by the CLI after reading the exit code).
	AutoRemove bool `json:"auto_remove,omitempty"`
}

// containerCmdline boots quietly (only the process's output on the
// console), configures eth0 by DHCP in the kernel, and runs the initrd's
// /init. panic=-1 ends the VM if PID 1 ever dies instead of hanging.
const containerCmdline = "console=hvc0 quiet loglevel=1 ip=dhcp rdinit=/init panic=-1"

// VMContainerDiskPath is a container's root disk (an APFS clone of its image).
func VMContainerDiskPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "rootfs.img")
}

// VMConsoleLogPath is a container's output (`container logs`).
func VMConsoleLogPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "console.log")
}

// VMExitPath records how a container's last run ended.
func VMExitPath(name string) (string, error) {
	return monolith.CachePath("vms", name, "exit.json")
}

// ExitStatus is how a container's process ended.
type ExitStatus struct {
	Code       int       `json:"code"`
	Error      string    `json:"error,omitempty"` // why the process couldn't run
	FinishedAt time.Time `json:"finished_at"`
}

// ReadExit returns how the container last exited; ok is false if it never
// has (or is running again).
func ReadExit(name string) (ExitStatus, bool) {
	var st ExitStatus
	path, err := VMExitPath(name)
	if err != nil {
		return st, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return st, false
	}
	return st, json.Unmarshal(data, &st) == nil
}

func writeExit(name string, st ExitStatus) error {
	path, err := VMExitPath(name)
	if err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// CreateContainer creates container VM name from an image disk: the VM
// directory, an APFS clone of the disk (instant, copy-on-write) extended
// to cfg.Container.DiskSize (sparse; monolith-init grows the filesystem at
// boot) and the config.
func CreateContainer(name string, cfg VMConfig, imageDisk string) error {
	if cfg.Container == nil {
		return errors.New("not a container config")
	}
	img, err := os.Stat(imageDisk)
	if err != nil {
		return err
	}
	if cfg.Container.DiskSize < img.Size() {
		return fmt.Errorf("the disk must be at least %d MiB for this image", img.Size()>>20)
	}
	dir, err := VMDir(name)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("a VM or container named %q already exists", name)
		}
		return err
	}
	disk, err := VMContainerDiskPath(name)
	if err != nil {
		return err
	}
	if err := unix.Clonefile(imageDisk, disk, unix.CLONE_NOFOLLOW); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("cloning the image disk: %w", err)
	}
	if err := os.Truncate(disk, cfg.Container.DiskSize); err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("sizing the container disk: %w", err)
	}
	if err := WriteConfig(name, cfg); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

type containerImage struct{}

func (containerImage) Name() string { return ImageContainer }

func (containerImage) Prepare(name string) (*BootSpec, error) {
	cfg, err := ReadConfig(name)
	if err != nil {
		return nil, err
	}
	if cfg.Container == nil {
		return nil, fmt.Errorf("%q is not a container", name)
	}
	kernel, err := kataKernel()
	if err != nil {
		return nil, err
	}
	disk, err := VMContainerDiskPath(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(disk); err != nil {
		return nil, fmt.Errorf("container disk: %w", err)
	}
	initrd, err := guestinit.Initrd()
	if err != nil {
		return nil, err
	}
	return &BootSpec{
		KernelPath:  kernel,
		InitrdPath:  initrd,
		CommandLine: containerCmdline,
		DiskPath:    disk,
		HandlesStop: true,
	}, nil
}

// removeContainer deletes a container's directory (`run -d --rm`).
func removeContainer(name string) {
	if dir, err := VMDir(name); err == nil && filepath.Base(dir) == name {
		os.RemoveAll(dir)
	}
}

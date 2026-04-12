package monolith

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Code-Hex/vz/v3"
	"github.com/pkg/errors"
	"github.com/pkg/term/termios"
	"golang.org/x/sys/unix"
)

func setRawMode(f *os.File) {
	var attr unix.Termios

	// Get settings for terminal
	termios.Tcgetattr(f.Fd(), &attr)

	// Put stdin into raw mode, disabling local echo, input canonicalization,
	// and CR-NL mapping.
	attr.Iflag &^= syscall.ICRNL
	attr.Lflag &^= syscall.ICANON | syscall.ECHO

	// Set minimum characters when reading = 1 char
	attr.Cc[syscall.VMIN] = 1

	// set timeout when reading as non-canonical mode
	attr.Cc[syscall.VTIME] = 0

	// reflects the changed settings
	termios.Tcsetattr(f.Fd(), termios.TCSANOW, &attr)
}

func Run(name string) error {
	// Ensure state is cleaned up when we exit, regardless of how.
	defer CleanState(name)

	// Read per-VM hardware configuration (written by the CLI before Run).
	cfg, err := ReadConfig(name)
	if err != nil {
		return errors.Wrap(err, "ReadConfig")
	}

	kernelConfig, err := DefaultKernel()
	if err != nil {
		return errors.Wrap(err, "DefaultKernel")
	}

	rootfs, err := DefaultRootFS()
	if err != nil {
		return errors.Wrap(err, "DefaultRootFS")
	}

	disk, err := DefaultDisk(rootfs, kernelConfig.ModloopPath, name)
	if err != nil {
		return errors.Wrap(err, "DefaultDisk")
	}

	// 1. Define the Boot Loader
	bootLoader, err := vz.NewLinuxBootLoader(
		kernelConfig.KernelPath,
		vz.WithCommandLine("console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop"),
		vz.WithInitrd(kernelConfig.InitrdPath),
	)
	if err != nil {
		return errors.Wrap(err, "vz.NewLinuxBootLoader")
	}

	// 2. Configure the VM with per-VM CPU and memory settings.
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, cfg.CPUs, cfg.Memory)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtualMachineConfiguration")
	}

	// 3. Add Storage
	attachment, err := vz.NewDiskImageStorageDeviceAttachment(disk, false)
	if err != nil {
		return errors.Wrap(err, "vz.NewDiskImageStorageDeviceAttachment")
	}
	storageConfig, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioBlockDeviceConfiguration")
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{storageConfig})

	// 4. Add Network
	macStr, err := configureNetwork(config, cfg)
	if err != nil {
		return errors.Wrap(err, "configureNetwork")
	}

	// 5. Setup Console (Required for interaction)
	setRawMode(os.Stdin)

	portAttachment, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, os.Stdout)
	if err != nil {
		return errors.Wrap(err, "vz.NewFileHandleSerialPortAttachment")
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(portAttachment)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioConsoleDeviceSerialPortConfiguration")
	}
	config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{serialConfig})

	// Add entropy device for virtio random
	entropyConfig, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioEntropyDeviceConfiguration")
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropyConfig})

	// 6. Validate and Create the VM
	validated, err := config.Validate()
	if !validated || err != nil {
		return fmt.Errorf("invalid VM configuration: %w", err)
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtualMachine")
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM)

	// 7. Start the VM
	err = vm.Start()
	if err != nil {
		return errors.Wrap(err, "vz.Start")
	}

	// Write state file so `vm list` can report this VM as running.
	// Store our PID and the MAC address (used to look up the DHCP-assigned IP).
	if err := WriteState(name, os.Getpid(), macStr); err != nil {
		log.Printf("warning: failed to write state file: %v", err)
	}

	for {
		select {
		case <-signalCh:
			result, err := vm.RequestStop()
			if err != nil {
				return errors.Wrap(err, "vm.RequestStop")
			}
			log.Println("received signal", result)
		case newState := <-vm.StateChangedNotify():
			if newState == vz.VirtualMachineStateRunning {
				log.Println("VM is running")
			}
			if newState == vz.VirtualMachineStateStopped {
				log.Println("stopped successfully")
				return nil
			}
			if newState == vz.VirtualMachineStateError {
				return fmt.Errorf("VM entered error state")
			}
		}
	}
}

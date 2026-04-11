package monolith

import (
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

func Run(path string) error {
	kernelConfig, err := DefaultKernel()
	if err != nil {
		return errors.Wrap(err, "DefaultKernel")
	}

	// 1. Define the Boot Loader (Linux example)
	bootLoader, err := vz.NewLinuxBootLoader(
		kernelConfig.KernelPath,
		vz.WithCommandLine("console=hvc0 root=/dev/vda"),
		vz.WithInitrd(kernelConfig.InitrdPath),
	)
	if err != nil {
		return errors.Wrap(err, "vx.NewLinuxBootLoader")
	}

	// 2. Configure the VM
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, 2, 2*1024*1024*1024) // 2 vCPUs, 2GB RAM
	if err != nil {
		return errors.Wrap(err, "vx.VirtualMachineConfiguration")
	}

	//disk, err := DefaultDisk()
	//if err != nil {
	//	return errors.Wrap(err, "DefaultDisk")
	//}

	// 3. Add Storage
	//attachment, err := vz.NewDiskImageStorageDeviceAttachment(disk, false)
	//if err != nil {
	//	return errors.Wrap(err, "vz.NewDiskImageStorageDeviceAttachment")
	//}
	//storageConfig, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	//if err != nil {
	//	return errors.Wrap(err, "vz.NewVirtioBlockDeviceConfiguration")
	//}
	//config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{storageConfig})

	// 4. Add Network (NAT)
	networkConfig, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return errors.Wrap(err, "vz.NewNATNetworkDeviceAttachment")
	}
	netConfig, err := vz.NewVirtioNetworkDeviceConfiguration(networkConfig)
	if err != nil {
		return errors.Wrap(err, "vz.NewVirtioNetworkDeviceConfiguration")
	}
	mac, err := vz.NewRandomLocallyAdministeredMACAddress()
	if err != nil {
		return errors.Wrap(err, "vz.NewRandomLocallyAdministeredMACAddress")
	}
	netConfig.SetMACAddress(mac)
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netConfig})

	// 5. Setup Console (Required for interaction)
	setRawMode(os.Stdin)

	portAttachement, err := vz.NewFileHandleSerialPortAttachment(os.Stdin, os.Stdout)
	if err != nil {
		return errors.Wrap(err, "vz.NewFileHandleSerialPortAttachment")
	}
	serialConfig, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(portAttachement)
	if err != nil {
		return errors.Wrap(err, "vz.NewFileHandleSerialPortAttachment")
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
		log.Fatal("Invalid configuration: ", err)
	}

	vm, err := vz.NewVirtualMachine(config)
	if err != nil {
		return errors.Wrap(err, "vz.NewFileHandleSerialPortAttachment")
	}

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGTERM)

	// 7. Start the VM
	err = vm.Start()
	if err != nil {
		return errors.Wrap(err, "vz.Start")
	}

	errCh := make(chan error, 1)

	for {
		select {
		case <-signalCh:
			result, err := vm.RequestStop()
			if err != nil {
				return errors.Wrap(err, "vm.RequestStop")
			}
			log.Println("recieved signal", result)
		case newState := <-vm.StateChangedNotify():
			if newState == vz.VirtualMachineStateRunning {
				log.Println("start VM is running")
			}
			if newState == vz.VirtualMachineStateStopped {
				log.Println("stopped successfully")
				return nil
			}
		case err := <-errCh:
			log.Println("in start:", err)
		}
	}
}

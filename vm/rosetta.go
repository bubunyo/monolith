package vm

import (
	"fmt"

	"github.com/Code-Hex/vz/v4"
)

const (
	rosettaShareTag   = "rosetta"
	rosettaCmdlineArg = "monolith.rosetta"
)

// CheckRosetta explains why Rosetta can't be shared into a VM, if it can't.
func CheckRosetta() error {
	switch vz.LinuxRosettaDirectoryShareAvailability() {
	case vz.LinuxRosettaAvailabilityInstalled:
		return nil
	case vz.LinuxRosettaAvailabilityNotInstalled:
		return fmt.Errorf("Rosetta is not installed; install it with 'softwareupdate --install-rosetta' or start with --rosetta=false")
	default:
		return fmt.Errorf("Rosetta for Linux is not supported on this Mac; start with --rosetta=false")
	}
}

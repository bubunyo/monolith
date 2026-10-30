package vm

import (
	"fmt"

	"github.com/bubunyo/monolith/vm"
)

// executeEscapeCommand handles a control command issued from the console
// escape prompt (Ctrl+]). It sends the command to the daemon and prints
// the result.
func executeEscapeCommand(name string, req vm.ControlRequest) error {
	resp, err := vm.SendControlCommand(name, req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	switch req.Cmd {
	case vm.CmdPause:
		fmt.Printf("VM %q paused.\n", name)
	case vm.CmdResume:
		fmt.Printf("VM %q resumed.\n", name)
	case vm.CmdSuspend:
		fmt.Printf("VM %q suspended to disk. Resume with 'monolith vm resume %s'.\n", name, name)
	case vm.CmdStop:
		fmt.Printf("VM %q stopped.\n", name)
	}
	return nil
}

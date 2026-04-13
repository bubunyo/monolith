package vm

import (
	"fmt"

	"github.com/bubunyo/monolith"
)

// executeEscapeCommand handles a control command issued from the console
// escape prompt (Ctrl+]). It sends the command to the daemon and prints
// the result.
func executeEscapeCommand(name string, req monolith.ControlRequest) error {
	resp, err := monolith.SendControlCommand(name, req)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}

	switch req.Cmd {
	case monolith.CmdPause:
		fmt.Printf("VM %q paused.\n", name)
	case monolith.CmdResume:
		fmt.Printf("VM %q resumed.\n", name)
	case monolith.CmdSuspend:
		fmt.Printf("VM %q suspended to disk. Resume with 'monolith vm resume %s'.\n", name, name)
	case monolith.CmdStop:
		fmt.Printf("VM %q stopped.\n", name)
	}
	return nil
}

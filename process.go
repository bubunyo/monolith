package monolith

import (
	"os"
	"syscall"
)

// ProcessAlive checks if a process with the given PID is still running.
// On Unix, sending signal 0 to a process checks for existence without
// actually sending a signal.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 tests for process existence.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

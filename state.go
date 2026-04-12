package monolith

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const stateFileName = "state.json"

// VMState represents the persisted runtime state of a VM.
type VMState struct {
	PID int    `json:"pid"`
	MAC string `json:"mac"`
}

// VMStatus is the computed status of a VM for display purposes.
type VMStatus struct {
	State string // "running" or "stopped"
	IP    string // e.g. "192.168.64.3" or "" if unknown
}

// WriteState writes the VM state file into the VM's cache directory.
func WriteState(name string, pid int, mac string) error {
	vmDir, err := vmDir(name)
	if err != nil {
		return err
	}

	state := VMState{
		PID: pid,
		MAC: mac,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	return os.WriteFile(filepath.Join(vmDir, stateFileName), data, 0644)
}

// ReadState reads and validates the VM state file. If the state file exists
// but the process is no longer alive, the stale file is removed and a
// "stopped" status is returned.
func ReadState(name string) (*VMStatus, error) {
	vmDir, err := vmDir(name)
	if err != nil {
		return &VMStatus{State: "stopped"}, nil
	}

	statePath := filepath.Join(vmDir, stateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &VMStatus{State: "stopped"}, nil
		}
		return nil, fmt.Errorf("reading state: %w", err)
	}

	var state VMState
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt state file — remove and report stopped.
		os.Remove(statePath)
		return &VMStatus{State: "stopped"}, nil
	}

	// Verify the process is still alive.
	if !processAlive(state.PID) {
		// Stale state file from a crashed/killed process.
		os.Remove(statePath)
		return &VMStatus{State: "stopped"}, nil
	}

	// Process is alive — look up IP from ARP table using the stored MAC.
	ip := lookupIPByMAC(state.MAC)

	return &VMStatus{
		State: "running",
		IP:    ip,
	}, nil
}

// CleanState removes the state file for the given VM.
func CleanState(name string) {
	vmDir, err := vmDir(name)
	if err != nil {
		return
	}
	os.Remove(filepath.Join(vmDir, stateFileName))
}

// vmDir returns the per-VM cache directory path.
func vmDir(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "monlith", "vms", name), nil
}

// processAlive checks if a process with the given PID is still running.
// On Unix, sending signal 0 to a process checks for existence without
// actually sending a signal.
func processAlive(pid int) bool {
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

// lookupIPByMAC reads the macOS DHCP server leases file to find the IP
// address assigned to the given MAC address. This is more reliable than
// the ARP table, which only contains entries for recent IP traffic.
//
// The leases file at /var/db/dhcpd_leases has entries like:
//
//	{
//		ip_address=192.168.64.14
//		hw_address=1,ce:b7:d1:c3:89:1d
//		identifier=1,ce:b7:d1:c3:89:1d
//		lease=0x69db5aec
//	}
//
// The hw_address is prefixed with "1," (ethernet type) and uses shortened
// octets (e.g. "e:3d:be" instead of "0e:3d:be"). We normalize both sides
// for comparison.
func lookupIPByMAC(mac string) string {
	if mac == "" {
		return ""
	}

	targetNorm := normalizeMAC(mac)
	if targetNorm == "" {
		return ""
	}

	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return ""
	}

	// Simple state-machine parser for the leases file.
	var currentIP string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)

		if strings.HasPrefix(line, "ip_address=") {
			currentIP = strings.TrimPrefix(line, "ip_address=")
		}

		if strings.HasPrefix(line, "hw_address=") {
			hwAddr := strings.TrimPrefix(line, "hw_address=")
			// Strip the ethernet type prefix "1," if present.
			if idx := strings.Index(hwAddr, ","); idx >= 0 {
				hwAddr = hwAddr[idx+1:]
			}
			if normalizeMAC(hwAddr) == targetNorm {
				return currentIP
			}
		}

		if line == "}" {
			currentIP = ""
		}
	}

	return ""
}

// normalizeMAC converts a MAC address to a canonical lowercase form with
// zero-padded octets (e.g. "8:b4:b1:82:87:2b" -> "08:b4:b1:82:87:2b").
// This handles the shortened format that macOS uses in ARP output and
// DHCP leases.
func normalizeMAC(mac string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(mac)), ":")
	if len(parts) != 6 {
		return ""
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

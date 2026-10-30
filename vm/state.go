package vm

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

const stateFileName = "state.json"

// VM status constants — persisted in state.json and reported by `vm list`.
const (
	StatusStopped   = "stopped"   // No process, no resources. Initial and final state.
	StatusRunning   = "running"   // Daemon alive, VM executing, guest clock ticking.
	StatusPaused    = "paused"    // Daemon alive, VM frozen in memory, guest clock stopped.
	StatusSuspended = "suspended" // Daemon exited, VM state saved to disk file.
)

// VMState represents the persisted runtime state of a VM.
type VMState struct {
	PID         int    `json:"pid"`
	MAC         string `json:"mac"`
	StartedAt   int64  `json:"started_at"`             // unix timestamp
	Status      string `json:"status,omitempty"`       // "running", "paused", "suspended"
	SuspendFile string `json:"suspend_file,omitempty"` // path to .vmstate file when suspended
}

// VMStatus is the computed status of a VM for display purposes.
type VMStatus struct {
	State      string    // "running", "paused", "suspended", or "stopped"
	IP         string    // e.g. "192.168.64.3" or "" if unknown
	StartedAt  time.Time // zero if stopped
	CPUPercent float64   // host-side CPU% of the VM process
	MemoryRSS  int64     // host-side RSS in bytes
}

// WriteState writes the VM state file into the VM's cache directory.
func WriteState(name string, pid int, mac, status string) error {
	dir, err := VMDir(name)
	if err != nil {
		return err
	}

	state := VMState{
		PID:       pid,
		MAC:       mac,
		StartedAt: time.Now().Unix(),
		Status:    status,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	return os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)
}

// UpdateStateStatus updates only the status field of the state file.
// Used by the daemon to transition between running/paused without rewriting everything.
func UpdateStateStatus(name, status string) error {
	dir, err := VMDir(name)
	if err != nil {
		return err
	}

	statePath := filepath.Join(dir, stateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		return err
	}

	var state VMState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	state.Status = status
	if status == StatusSuspended || status == StatusStopped {
		state.PID = 0
	}

	out, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, out, 0644)
}

// WriteSuspendState writes a suspended state (daemon has exited, state file on disk).
func WriteSuspendState(name, mac, suspendFile string) error {
	dir, err := VMDir(name)
	if err != nil {
		return err
	}

	state := VMState{
		PID:         0,
		MAC:         mac,
		Status:      StatusSuspended,
		SuspendFile: suspendFile,
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	return os.WriteFile(filepath.Join(dir, stateFileName), data, 0644)
}

// ReadState reads and validates the VM state file. If the state file exists
// but the process is no longer alive, the status is checked:
// - suspended: kept as-is (daemon intentionally exited)
// - running/paused: stale — cleaned up and reported as stopped
func ReadState(name string) (*VMStatus, error) {
	dir, err := VMDir(name)
	if err != nil {
		return &VMStatus{State: StatusStopped}, nil
	}

	statePath := filepath.Join(dir, stateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return &VMStatus{State: StatusStopped}, nil
		}
		return nil, fmt.Errorf("reading state: %w", err)
	}

	var state VMState
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt state file — remove and report stopped.
		os.Remove(statePath)
		return &VMStatus{State: StatusStopped}, nil
	}

	// Suspended VMs have no daemon process — that's expected.
	if state.Status == StatusSuspended {
		return &VMStatus{State: StatusSuspended}, nil
	}

	// For running/paused, verify the daemon process is still alive.
	if !monolith.ProcessAlive(state.PID) {
		// Stale state file from a crashed/killed daemon.
		os.Remove(statePath)
		return &VMStatus{State: StatusStopped}, nil
	}

	// Determine display state from persisted status.
	displayState := StatusRunning
	if state.Status == StatusPaused {
		displayState = StatusPaused
	}

	// Process is alive — look up IP and gather host-side stats.
	cfg, _ := ReadConfig(name)
	ip := mnet.LookupIPForMode(cfg.NetworkMode, state.MAC)
	cpuPct, rssBytes := processStats(state.PID)

	var startedAt time.Time
	if state.StartedAt > 0 {
		startedAt = time.Unix(state.StartedAt, 0)
	}

	return &VMStatus{
		State:      displayState,
		IP:         ip,
		StartedAt:  startedAt,
		CPUPercent: cpuPct,
		MemoryRSS:  rssBytes,
	}, nil
}

// ReadRawState reads the raw VM state file without computing derived status.
// Used by the top command to access PID/MAC for per-tick stat gathering.
// Returns nil if the VM is not running or state file is missing.
func ReadRawState(name string) (*VMState, error) {
	dir, err := VMDir(name)
	if err != nil {
		return nil, err
	}

	statePath := filepath.Join(dir, stateFileName)
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}

	var state VMState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	if !monolith.ProcessAlive(state.PID) {
		os.Remove(statePath)
		return nil, fmt.Errorf("VM %q is not running (stale state)", name)
	}

	return &state, nil
}

// CleanState removes the state file for the given VM.
func CleanState(name string) {
	dir, err := VMDir(name)
	if err != nil {
		return
	}
	os.Remove(filepath.Join(dir, stateFileName))
}

// processStats returns the CPU% and RSS (in bytes) for the given PID by
// shelling out to `ps`. Returns zeros if the process is not found or ps fails.
func processStats(pid int) (cpuPct float64, rssBytes int64) {
	cpuPct, rssBytes, _ = ProcessStatsEx(pid)
	return
}

// ProcessStatsEx returns CPU%, RSS (bytes), and cumulative CPU time for the
// given PID. The cpuTime string is in the format "H:MM:SS" or "M:SS".
func ProcessStatsEx(pid int) (cpuPct float64, rssBytes int64, cpuTime string) {
	// ps -o pcpu=,rss=,time= -p <pid>
	// Output example: "  0.3 19056 0:02:15\n"
	out, err := exec.Command("ps", "-o", "pcpu=,rss=,time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, 0, ""
	}

	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 3 {
		return 0, 0, ""
	}

	cpuPct, _ = strconv.ParseFloat(fields[0], 64)
	rssKiB, _ := strconv.ParseInt(fields[1], 10, 64)
	return cpuPct, rssKiB * 1024, fields[2]
}

package monolith

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// NetStats holds per-VM network counters from the host's vmenet interface.
// RX/TX are from the guest's perspective (RX = bytes received by guest).
type NetStats struct {
	RxBytes int64
	TxBytes int64
	RxPkts  int64
	TxPkts  int64
}

// WriteState writes the VM state file into the VM's cache directory.
func WriteState(name string, pid int, mac, status string) error {
	dir, err := vmDir(name)
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
	dir, err := vmDir(name)
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
	dir, err := vmDir(name)
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
	dir, err := vmDir(name)
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
	if !processAlive(state.PID) {
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
	ip := lookupIPForMode(cfg.NetworkMode, state.MAC)
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
	dir, err := vmDir(name)
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

	if !processAlive(state.PID) {
		os.Remove(statePath)
		return nil, fmt.Errorf("VM %q is not running (stale state)", name)
	}

	return &state, nil
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
	return filepath.Join(home, ".cache", "monolith", "vms", name), nil
}

// VMDir is the exported version of vmDir.
func VMDir(name string) (string, error) {
	return vmDir(name)
}

// VMsDir returns the parent directory containing all per-VM directories.
func VMsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "monolith", "vms"), nil
}

// VMSockPath returns the path to the VM daemon's Unix domain socket.
func VMSockPath(name string) (string, error) {
	dir, err := vmDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// VMSuspendPath returns the path where a VM's state is saved on suspend.
func VMSuspendPath(name string) (string, error) {
	dir, err := vmDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "suspend.vmstate"), nil
}

// VMLogPath returns the path to the VM daemon's log file.
func VMLogPath(name string) (string, error) {
	dir, err := vmDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
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

// VmenetForMAC finds the vmenet<N> interface associated with the given guest
// MAC address by parsing the bridge100 address cache from `ifconfig bridge100`.
//
// The address cache section looks like:
//
//	Address cache:
//	    ce:b7:d1:c3:89:1d Vlan1 vmenet0 1116 flags=0<>
//
// Returns "" if no matching interface is found.
func VmenetForMAC(mac string) string {
	if mac == "" {
		return ""
	}
	targetNorm := normalizeMAC(mac)
	if targetNorm == "" {
		return ""
	}

	out, err := exec.Command("ifconfig", "bridge100").Output()
	if err != nil {
		return ""
	}

	// Scan for lines in the address cache section.
	inCache := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "Address cache:") {
			inCache = true
			continue
		}

		if !inCache {
			continue
		}

		// End of address cache section — lines that aren't indented entries.
		if !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "    ") {
			if trimmed != "" {
				break
			}
			continue
		}

		// Parse: "ce:b7:d1:c3:89:1d Vlan1 vmenet0 1116 flags=0<>"
		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}
		entryMAC := normalizeMAC(fields[0])
		if entryMAC == targetNorm {
			return fields[2] // e.g. "vmenet0"
		}
	}

	return ""
}

// NetworkStats returns per-VM network counters by querying the given
// vmenet interface via `netstat -I <iface> -b`.
//
// Note: from the host's perspective, Ibytes = bytes received from guest
// (guest TX) and Obytes = bytes sent to guest (guest RX). We swap them
// so the returned values are from the guest's perspective.
//
// netstat output:
//
//	Name  Mtu  Network  Address   Ipkts Ierrs  Ibytes   Opkts Oerrs  Obytes  Coll
//	vmenet0 1500 <Link#27> ba:23:...  9     0     1286    156     0     24693     0
func NetworkStats(iface string) NetStats {
	if iface == "" {
		return NetStats{}
	}

	out, err := exec.Command("netstat", "-I", iface, "-b").Output()
	if err != nil {
		return NetStats{}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return NetStats{}
	}

	// Parse the data line (skip header). There may be multiple lines
	// (one per address family); we want the first one (link-level).
	fields := strings.Fields(lines[1])
	// Expected fields: Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll
	//                  0    1   2       3       4     5     6      7     8     9      10
	if len(fields) < 11 {
		return NetStats{}
	}

	iPkts, _ := strconv.ParseInt(fields[4], 10, 64)
	iBytes, _ := strconv.ParseInt(fields[6], 10, 64)
	oPkts, _ := strconv.ParseInt(fields[7], 10, 64)
	oBytes, _ := strconv.ParseInt(fields[9], 10, 64)

	// Swap: host Ibytes = guest TX, host Obytes = guest RX
	return NetStats{
		RxBytes: oBytes,
		TxBytes: iBytes,
		RxPkts:  oPkts,
		TxPkts:  iPkts,
	}
}

// lookupIPForMode picks the right IP discovery strategy based on network mode.
// - nat/shared: DHCP leases file (macOS DHCP server assigns IPs)
// - host: DHCP leases first (router assigns), ARP table fallback
// - isolated/none: no IP expected
func lookupIPForMode(mode, mac string) string {
	switch mode {
	case NetworkNAT, NetworkShared, "":
		return LookupIPByMAC(mac)
	case NetworkHost:
		// Try DHCP leases first (in case the router's DHCP is visible),
		// then fall back to ARP table.
		if ip := LookupIPByMAC(mac); ip != "" {
			return ip
		}
		return lookupIPByARP(mac)
	case NetworkIsolated, NetworkNone:
		return ""
	default:
		return LookupIPByMAC(mac)
	}
}

// lookupIPByARP scans the ARP table for the given MAC address.
// Used as a fallback for host mode where the router assigns IPs
// and they may not appear in macOS DHCP leases.
func lookupIPByARP(mac string) string {
	if mac == "" {
		return ""
	}
	targetNorm := normalizeMAC(mac)
	if targetNorm == "" {
		return ""
	}

	out, err := exec.Command("arp", "-an").Output()
	if err != nil {
		return ""
	}

	// Lines look like: ? (192.168.1.42) at 8:b4:b1:82:87:2b on en0 ifscope [ethernet]
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] == "(incomplete)" {
			continue
		}
		if fields[2] != "at" {
			continue
		}
		arpMAC := normalizeMAC(fields[3])
		if arpMAC == targetNorm {
			// Extract IP from parentheses: "(192.168.1.42)"
			ip := strings.Trim(fields[1], "()")
			return ip
		}
	}
	return ""
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
// LookupIPByMAC reads the macOS DHCP server leases file to find the IP
// address assigned to the given MAC address.
func LookupIPByMAC(mac string) string {
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

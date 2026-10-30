package net

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// NetStats holds per-VM network counters from the host's vmenet interface.
// RX/TX are from the guest's perspective (RX = bytes received by guest).
type NetStats struct {
	RxBytes int64
	TxBytes int64
	RxPkts  int64
	TxPkts  int64
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

// LookupIPForMode picks the right IP discovery strategy based on network mode.
// - nat/shared: DHCP leases file (macOS DHCP server assigns IPs)
// - host: DHCP leases first (router assigns), ARP table fallback
// - isolated/none: no IP expected
func LookupIPForMode(mode, mac string) string {
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

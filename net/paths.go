package net

import "github.com/bubunyo/monolith"

// NetworksDir returns the parent directory for all named networks.
func NetworksDir() (string, error) {
	return monolith.CachePath("networks")
}

// NetworkDir returns the path to the named network's directory.
func NetworkDir(name string) (string, error) {
	return monolith.CachePath("networks", name)
}

// NetworkLogPath returns the path to the network daemon's log file.
func NetworkLogPath(name string) (string, error) {
	return monolith.CachePath("networks", name, networkLogFileName)
}

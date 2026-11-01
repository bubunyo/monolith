//go:build !linux

// monolith-init only runs inside container VMs; `make build` cross-compiles
// it for linux/arm64 and embeds it in monolith.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "monolith-init runs as PID 1 of a Linux container VM; build it with GOOS=linux")
	os.Exit(1)
}

package monolith

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// HasEntitlement reports whether the currently running binary has been
// codesigned with the given entitlement key set to true.
//
// The result is cached for the lifetime of the process. On any error
// reading the signature (binary not signed, codesign missing, etc.) it
// returns false. This is intended for best-effort pre-flight checks; the
// authoritative enforcement happens inside Virtualization.framework at
// VM startup.
func HasEntitlement(key string) bool {
	ents := loadOwnEntitlements()
	return ents[key]
}

var (
	entitlementsOnce sync.Once
	entitlementsMap  map[string]bool
)

func loadOwnEntitlements() map[string]bool {
	entitlementsOnce.Do(func() {
		entitlementsMap = make(map[string]bool)
		exe, err := os.Executable()
		if err != nil {
			return
		}
		// codesign -d --entitlements - <path>  prints the entitlements XML
		// (with an 8-byte magic+length prefix on macOS <13, plain XML on >=13)
		// to stdout.
		cmd := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", exe)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return
		}
		out := stdout.Bytes()
		// Strip leading binary magic if present (older macOS).
		if i := bytes.Index(out, []byte("<?xml")); i > 0 {
			out = out[i:]
		}
		// Cheap parse: scan for "<key>NAME</key>" followed by "<true/>".
		// Avoids pulling in an XML parser for a one-shot check.
		s := string(out)
		for {
			ki := strings.Index(s, "<key>")
			if ki < 0 {
				break
			}
			ke := strings.Index(s[ki:], "</key>")
			if ke < 0 {
				break
			}
			key := s[ki+len("<key>") : ki+ke]
			rest := s[ki+ke+len("</key>"):]
			rest = strings.TrimLeft(rest, " \t\r\n")
			if strings.HasPrefix(rest, "<true/>") {
				entitlementsMap[key] = true
			}
			s = rest
		}
	})
	return entitlementsMap
}

// EntitlementHostNetworking is the entitlement Apple requires for using
// VZBridgedNetworkDeviceAttachment (i.e. host/bridged network mode).
const EntitlementHostNetworking = "com.apple.vm.networking"

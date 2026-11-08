package vm

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/bubunyo/monolith/internal/ansi"
)

// Boot step markers: the guest's inittab prints OSC 7777 "monolith;NAME"
// to the console as it reaches each step. Terminals ignore unknown OSC
// sequences, so the markers are invisible in `vm attach`.
const (
	bootMarkerPrefix = "\x1b]7777;monolith;"
	bootMarkerEnd    = '\x07'
)

// BootStep is a stage of the guest's boot as seen on its console. It ends
// when the guest prints marker End, or a line containing Match.
type BootStep struct {
	Title string
	End   string
	Match string
}

// BootSteps returns the stages `vm run` shows for cfg's guest. Alpine and
// docker disks created before the markers (GuestLayoutBootSteps) only get
// Kernel; their boot carries on in the console.
func BootSteps(cfg VMConfig) []BootStep {
	kernel := BootStep{Title: "Kernel", Match: " as init process"}
	switch cfg.Image {
	case ImageKata:
		// Read-only rootfs, so no markers; the kernel itself runs DHCP.
		return []BootStep{kernel, {Title: "System", Match: "Reached target kata-containers.target"}}
	case ImageAlpine, ImageDocker:
		if cfg.GuestLayout < GuestLayoutBootSteps {
			return []BootStep{kernel}
		}
		steps := []BootStep{kernel, {Title: "System", End: "network"}}
		if cfg.Image == ImageAlpine {
			return append(steps, BootStep{Title: "Network", End: "ready"})
		}
		return append(steps, BootStep{Title: "Network", End: "engine"}, BootStep{Title: "Engine", End: "ready"})
	}
	return nil
}

// WatchBoot reads console output from r and follows steps through it.
// Each console line goes to line with the index of the step it belongs to,
// and ended is called as each step ends. A marker for a later step ends
// the steps before it too. WatchBoot returns once the last step has ended,
// with the output read past that point, which belongs to the console.
func WatchBoot(r io.Reader, steps []BootStep, line func(step int, text string), ended func(step int)) (rest []byte, err error) {
	if len(steps) == 0 {
		return nil, nil
	}
	cur := 0
	// advance ends steps up to and including last; true once all have.
	advance := func(last int) bool {
		for ; cur <= last; cur++ {
			ended(cur)
		}
		return cur == len(steps)
	}
	var buf []byte
	chunk := make([]byte, 4096)
	for {
		for {
			nl := bytes.IndexByte(buf, '\n')
			m := bytes.Index(buf, []byte(bootMarkerPrefix))
			if m >= 0 && (nl < 0 || m < nl) {
				end := bytes.IndexByte(buf[m:], bootMarkerEnd)
				if end < 0 {
					break // the rest of the marker hasn't arrived
				}
				name := string(buf[m+len(bootMarkerPrefix) : m+end])
				if pre := buf[:m]; len(bytes.TrimSpace(pre)) > 0 {
					line(cur, string(pre))
				}
				buf = buf[m+end+1:]
				for i := cur; i < len(steps); i++ {
					if steps[i].End == name {
						if advance(i) {
							return buf, nil
						}
						break
					}
				}
				continue
			}
			if nl < 0 {
				break
			}
			text := string(buf[:nl])
			buf = buf[nl+1:]
			line(cur, text)
			if match := steps[cur].Match; match != "" && strings.Contains(ansi.Plain(text), match) {
				if advance(cur) {
					return buf, nil
				}
			}
		}
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if err == io.EOF {
				err = fmt.Errorf("the console closed during %s", strings.ToLower(steps[cur].Title))
			}
			return buf, err
		}
	}
}

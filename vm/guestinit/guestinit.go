// Package guestinit embeds monolith-init (cmd/monolith-init, built for
// linux/arm64 by `make build`) and packs it with a container's spec into
// the initrd a container VM boots from.
package guestinit

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	ci "github.com/bubunyo/monolith/containerinit"
)

//go:embed files
var files embed.FS

// Binary returns the embedded init, or an error for a binary built without
// it (a bare `go build` rather than `make build`).
func Binary() ([]byte, error) {
	data, err := files.ReadFile("files/monolith-init")
	if errors.Is(err, os.ErrNotExist) || len(data) == 0 {
		return nil, fmt.Errorf("this monolith was built without monolith-init; build it with 'make build'")
	}
	return data, err
}

// WriteInitrd writes an initrd (cpio, newc format) holding the init as
// /init and spec as /spec.json.
func WriteInitrd(path string, init []byte, spec ci.Spec) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	data := Cpio([]CpioEntry{
		{Name: "dev", Mode: 040755},
		{Name: "proc", Mode: 040755},
		{Name: "newroot", Mode: 040755},
		{Name: "init", Mode: 0100755, Data: init},
		{Name: ci.SpecPath[1:], Mode: 0100644, Data: specJSON},
	})
	return os.WriteFile(path, data, 0644)
}

// CpioEntry is a file or directory in a cpio archive. Mode includes the
// file type bits (040000 directory, 0100000 regular file).
type CpioEntry struct {
	Name string
	Mode uint32
	Data []byte
}

// Cpio builds a newc ("070701") cpio archive, the format the kernel
// unpacks as an initramfs.
func Cpio(entries []CpioEntry) []byte {
	var b bytes.Buffer
	pad := func() {
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
	}
	write := func(ino int, name string, mode uint32, data []byte) {
		nlink := 1
		if mode&040000 != 0 {
			nlink = 2
		}
		// magic, ino, mode, uid, gid, nlink, mtime, filesize, devmajor,
		// devminor, rdevmajor, rdevminor, namesize, check
		fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		b.WriteString(name)
		b.WriteByte(0)
		pad()
		b.Write(data)
		pad()
	}
	for i, e := range entries {
		write(i+1, e.Name, e.Mode, e.Data)
	}
	write(0, "TRAILER!!!", 0, nil)
	return b.Bytes()
}

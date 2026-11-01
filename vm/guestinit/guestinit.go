// Package guestinit embeds monolith-init (cmd/monolith-init, built for
// linux/arm64 by `make build`) and packs it into the initrd container VMs
// boot from. The container's spec isn't in it: the daemon sends it over
// the Agent API, so one initrd serves every container.
package guestinit

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bubunyo/monolith"
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

// Initrd returns the initrd (cpio, newc format) holding the embedded init
// as /init, writing it on first use to the cache, named by the init's hash
// so a new monolith brings its own.
func Initrd() (string, error) {
	init, err := Binary()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(init)
	path, err := monolith.CachePath("container", "initrd-"+hex.EncodeToString(sum[:6])+".img")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	data := Cpio([]CpioEntry{
		{Name: "dev", Mode: 040755},
		{Name: "proc", Mode: 040755},
		{Name: "newroot", Mode: 040755},
		{Name: "init", Mode: 0100755, Data: init},
	})
	// A unique temporary file: containers may start at the same time.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".initrd-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
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

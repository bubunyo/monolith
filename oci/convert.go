package oci

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"golang.org/x/sys/unix"
)

// An image's filesystem is sized to fit its contents with some room
// (EstimateDiskSize); a container's clone of it is then made as big as
// its --disk and monolith-init grows the filesystem to match at boot.
const (
	minDiskSize  = 1 << 30
	diskSizeStep = 256 << 20
)

// errFull means the filesystem ran out of blocks or inodes while
// converting; Pull retries with a bigger one.
var errFull = errors.New("image filesystem too small")

// EstimateDiskSize sizes an image's filesystem from its compressed layers:
// four times their size (layers rarely compress better than that) plus
// 512 MiB, at least 1 GiB, in 256 MiB steps.
func EstimateDiskSize(compressed int64) int64 {
	size := 4*compressed + 512<<20
	if size < minDiskSize {
		size = minDiskSize
	}
	return (size + diskSizeStep - 1) / diskSizeStep * diskSizeStep
}

// ConvertStats describes a conversion.
type ConvertStats struct {
	Files, Dirs, Symlinks, Hardlinks int
	Skipped                          int // device nodes and FIFOs: devtmpfs provides /dev
	XattrsDropped                    int // files whose extended attributes were lost
	DiskBytes                        int64
}

type inodeMeta struct {
	mode     os.FileMode
	uid, gid int
	mtime    time.Time
}

// Convert flattens img's layers (whiteouts applied) into a new ext4 disk
// image of size bytes at diskPath. It fails with errFull if they don't fit.
//
// go-diskfs has gaps this works around: it has no hard links (the target's
// content is copied), no device nodes (skipped) and no extended attributes
// (dropped, which loses file capabilities). Writing to a directory also
// resets the mode and owner of inodes written before, so all metadata is
// applied in a final pass. Its mkfs writes out every inode table, so the
// zero runs are punched out of the file at the end. Its reserved GDT
// blocks (resize_inode) are misplaced, which makes the kernel refuse to
// grow the filesystem, so they're left out: without them the kernel grows
// it by switching to meta_bg.
func Convert(img v1.Image, diskPath string, size int64) (ConvertStats, error) {
	var st ConvertStats
	f, err := os.Create(diskPath)
	if err != nil {
		return st, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return st, err
	}
	f.Close()

	backend, err := file.OpenFromPath(diskPath, false)
	if err != nil {
		return st, err
	}
	// 4 KiB blocks: go-diskfs picks 1 KiB by default, whose journal
	// allocation fails on large filesystems.
	fs, err := ext4.Create(backend, size, 0, 512, &ext4.Params{
		SectorsPerBlock: 8,
		VolumeName:      "rootfs",
		Features:        []ext4.FeatureOpt{ext4.WithFeatureReservedGDTBlocksForExpansion(false)},
	})
	if err != nil {
		backend.Close()
		return st, fmt.Errorf("creating ext4 filesystem: %w", err)
	}

	rc := mutate.Extract(img)
	err = extract(fs, tar.NewReader(rc), &st)
	rc.Close()
	if err != nil && isFull(err) {
		err = fmt.Errorf("%w (%s): %v", errFull, HumanSize(size), err)
	}
	if cerr := backend.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return st, err
	}
	if err := punchZeros(diskPath); err != nil {
		return st, fmt.Errorf("making %s sparse: %w", diskPath, err)
	}
	var fi syscall.Stat_t
	if err := syscall.Stat(diskPath, &fi); err == nil {
		st.DiskBytes = fi.Blocks * 512
	}
	return st, nil
}

func extract(fs *ext4.FileSystem, tr *tar.Reader, st *ConvertStats) error {
	made := map[string]bool{}
	meta := map[string]inodeMeta{}
	var mkdirAll func(p string) error
	mkdirAll = func(p string) error {
		if p == "" || p == "." || made[p] {
			return nil
		}
		if err := mkdirAll(path.Dir(p)); err != nil {
			return err
		}
		made[p] = true
		if _, ok := meta[p]; !ok {
			// Directories with no entry of their own, as Docker creates them.
			meta[p] = inodeMeta{mode: os.ModeDir | 0755}
		}
		// Mkdir on an existing directory would reset its metadata.
		return fs.Mkdir(p)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading layers: %w", err)
		}
		p := cleanPath(hdr.Name)
		if p == "" {
			continue
		}
		for k := range hdr.PAXRecords {
			if strings.HasPrefix(k, "SCHILY.xattr.") {
				st.XattrsDropped++
				break
			}
		}
		if err := mkdirAll(path.Dir(p)); err != nil {
			return fmt.Errorf("creating parents of /%s: %w", p, err)
		}
		m := inodeMeta{mode: fileMode(hdr.Mode), uid: hdr.Uid, gid: hdr.Gid, mtime: hdr.ModTime}
		switch hdr.Typeflag {
		case tar.TypeDir:
			st.Dirs++
			if err := mkdirAll(p); err != nil {
				return fmt.Errorf("mkdir /%s: %w", p, err)
			}
			meta[p] = m
		case tar.TypeReg:
			st.Files++
			if err := writeFile(fs, p, tr); err != nil {
				return fmt.Errorf("writing /%s: %w", p, err)
			}
			meta[p] = m
		case tar.TypeSymlink:
			st.Symlinks++
			if err := fs.Symlink(hdr.Linkname, p); err != nil {
				return fmt.Errorf("symlink /%s: %w", p, err)
			}
		case tar.TypeLink:
			st.Hardlinks++
			target := cleanPath(hdr.Linkname)
			src, err := fs.OpenFile(target, os.O_RDONLY)
			if err != nil {
				return fmt.Errorf("hard link /%s -> /%s: %w", p, target, err)
			}
			err = writeFile(fs, p, src)
			src.Close()
			if err != nil {
				return fmt.Errorf("hard link /%s: %w", p, err)
			}
			if tm, ok := meta[target]; ok {
				meta[p] = tm
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			st.Skipped++
		default:
			st.Skipped++
		}
	}

	for p, m := range meta {
		if err := fs.Chmod(p, m.mode&^os.ModeDir); err != nil {
			return fmt.Errorf("chmod /%s: %w", p, err)
		}
		if err := fs.Chown(p, m.uid, m.gid); err != nil {
			return fmt.Errorf("chown /%s: %w", p, err)
		}
		if !m.mtime.IsZero() {
			if err := fs.Chtimes(p, m.mtime, m.mtime, m.mtime); err != nil {
				return fmt.Errorf("chtimes /%s: %w", p, err)
			}
		}
	}
	return nil
}

// isFull recognizes go-diskfs's out-of-space errors.
func isFull(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "blocks free, requires") || strings.Contains(msg, "no free inodes")
}

// cleanPath makes a tar name a go-diskfs path: relative, no "./" or "..".
func cleanPath(name string) string {
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

// fileMode converts tar mode bits, including setuid, setgid and sticky.
func fileMode(m int64) os.FileMode {
	mode := os.FileMode(m) & os.ModePerm
	if m&04000 != 0 {
		mode |= os.ModeSetuid
	}
	if m&02000 != 0 {
		mode |= os.ModeSetgid
	}
	if m&01000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

func writeFile(fs *ext4.FileSystem, p string, r io.Reader) error {
	f, err := fs.OpenFile(p, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// punchZeros returns the space of all-zero 64 KiB runs in path to the
// filesystem (macOS F_PUNCHHOLE), leaving them as holes that read as zeros.
func punchZeros(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	zero := make([]byte, chunk)
	for off := int64(0); ; off += chunk {
		n, err := io.ReadFull(f, buf)
		if n == chunk && bytes.Equal(buf, zero) {
			hole := struct {
				flags, reserved uint32
				offset, length  int64
			}{offset: off, length: chunk}
			// No libSystem wrapper exists for F_PUNCHHOLE.
			//nolint:staticcheck
			if _, _, e := unix.Syscall(unix.SYS_FCNTL, f.Fd(), unix.F_PUNCHHOLE, uintptr(unsafe.Pointer(&hole))); e != 0 {
				return e
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

package oci

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

// ImageFS reads an image's filesystem on the Mac, read-only, without
// running anything from it (`container image ls|cat|cp`). Paths are
// absolute paths in the image. Symlinks are resolved here, never by the
// Mac, and can't lead out of the image: ".." stops at its root and
// absolute targets start from it.
type ImageFS struct {
	b  backend.Storage
	fs *ext4.FileSystem
}

// Entry describes a file in the image.
type Entry struct {
	Name     string // base name
	Path     string // absolute path in the image
	Mode     fs.FileMode
	UID, GID uint32
	Size     int64
	ModTime  time.Time
	Target   string // symlink target, as stored
}

func (e Entry) IsDir() bool     { return e.Mode.IsDir() }
func (e Entry) IsSymlink() bool { return e.Mode&fs.ModeSymlink != 0 }

// maxSymlinks bounds symlink resolution (Linux's limit).
const maxSymlinks = 40

// OpenFS opens image id's filesystem read-only.
func (s *Store) OpenFS(id string) (*ImageFS, error) {
	p := s.DiskPath(id)
	fi, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	b, err := file.OpenFromPath(p, true)
	if err != nil {
		return nil, err
	}
	efs, err := ext4.Read(b, fi.Size(), 0, 512)
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("reading the image filesystem: %w", err)
	}
	return &ImageFS{b: b, fs: efs}, nil
}

// Close releases the image's disk.
func (f *ImageFS) Close() error { return f.b.Close() }

// clean makes p an absolute, clean image path.
func clean(p string) string { return path.Clean("/" + p) }

// diskPath turns an image path into a go-diskfs path (relative; the root
// is ".").
func diskPath(p string) string {
	if p = strings.TrimPrefix(clean(p), "/"); p == "" {
		return "."
	}
	return p
}

// Lstat describes p itself (a symlink is not followed). Its parent
// directories are taken as they are; use Resolve first for user paths.
func (f *ImageFS) Lstat(p string) (Entry, error) {
	p = clean(p)
	if p == "/" {
		return Entry{Name: "/", Path: "/", Mode: fs.ModeDir | 0755}, nil
	}
	fi, err := f.fs.Stat(diskPath(p))
	if err != nil {
		return Entry{}, fmt.Errorf("%s: no such file or directory", p)
	}
	return f.entry(p, fi)
}

func (f *ImageFS) entry(p string, fi fs.FileInfo) (Entry, error) {
	e := Entry{Name: path.Base(p), Path: p, Mode: fi.Mode(), Size: fi.Size(), ModTime: fi.ModTime()}
	if fi.IsDir() {
		e.Mode |= fs.ModeDir
	}
	if st, ok := fi.Sys().(*ext4.StatT); ok {
		e.UID, e.GID = st.UID, st.GID
	}
	if e.IsSymlink() {
		t, err := f.fs.ReadLink(diskPath(p))
		if err != nil {
			return Entry{}, fmt.Errorf("%s: %w", p, err)
		}
		e.Target = t
	}
	return e, nil
}

// Resolve follows symlinks in every component of p, inside the image, and
// returns the path they lead to.
func (f *ImageFS) Resolve(p string) (string, error) { return resolve(f.Lstat, p) }

// Stat describes what p leads to, following symlinks inside the image.
func (f *ImageFS) Stat(p string) (Entry, error) {
	r, err := f.Resolve(p)
	if err != nil {
		return Entry{}, err
	}
	return f.Lstat(r)
}

// ReadDir lists the directory p leads to, sorted by name. Entries are
// described themselves (symlinks not followed).
func (f *ImageFS) ReadDir(p string) ([]Entry, error) {
	r, err := f.Resolve(p)
	if err != nil {
		return nil, err
	}
	dir, err := f.Lstat(r)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", clean(p))
	}
	des, err := f.fs.ReadDir(diskPath(r))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", r, err)
	}
	var out []Entry
	for _, de := range des {
		name := de.Name()
		if name == "." || name == ".." || name == "lost+found" && r == "/" || strings.Contains(name, "/") {
			continue
		}
		// go-diskfs's DirEntry.Info has no mode, owner or size: stat it.
		e, err := f.Lstat(path.Join(r, name))
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Open opens the regular file p leads to.
func (f *ImageFS) Open(p string) (io.ReadCloser, Entry, error) {
	e, err := f.Stat(p)
	if err != nil {
		return nil, Entry{}, err
	}
	if e.IsDir() {
		return nil, Entry{}, fmt.Errorf("%s: is a directory", clean(p))
	}
	if !e.Mode.IsRegular() {
		return nil, Entry{}, fmt.Errorf("%s: not a regular file", clean(p))
	}
	rc, err := f.fs.OpenFile(diskPath(e.Path), os.O_RDONLY)
	if err != nil {
		return nil, Entry{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	return rc, e, nil
}

// ErrExists is returned by CopyOut when a destination exists and
// overwriting wasn't asked for.
var ErrExists = errors.New("already exists")

// CopyOut copies what src leads to (a file, or a directory with
// everything under it) to dest on the Mac, like cp: into dest if it's an
// existing directory, else as dest. Symlinks inside a copied directory are
// recreated as symlinks, never followed. Files keep their permissions
// without setuid, setgid or sticky bits. Nothing is written through an
// existing symlink on the Mac, and an existing file is only replaced with
// overwrite. copied is called with each path written.
func (f *ImageFS) CopyOut(src, dest string, overwrite bool, copied func(string)) error {
	return CopyOut(f, src, dest, overwrite, copied)
}

// CopyOut copies from any view (the whole image or one layer); see
// ImageFS.CopyOut.
func CopyOut(v View, src, dest string, overwrite bool, copied func(string)) error {
	e, err := v.Stat(src)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		dest = filepath.Join(dest, e.Name)
	}
	return copyEntry(v, e, dest, overwrite, copied)
}

func copyEntry(v View, e Entry, dest string, overwrite bool, copied func(string)) error {
	existing, err := os.Lstat(dest)
	exists := err == nil
	switch {
	case e.IsDir():
		if exists && !existing.IsDir() {
			return fmt.Errorf("%s: %w and isn't a directory", dest, ErrExists)
		}
		if !exists {
			if err := os.Mkdir(dest, e.Mode.Perm()|0700); err != nil {
				return err
			}
			copied(dest)
		}
		children, err := v.ReadDir(e.Path)
		if err != nil {
			return err
		}
		for _, c := range children {
			if err := copyEntry(v, c, filepath.Join(dest, c.Name), overwrite, copied); err != nil {
				return err
			}
		}
		return nil
	case e.IsSymlink():
		if exists {
			if !overwrite {
				return fmt.Errorf("%s: %w (use -f)", dest, ErrExists)
			}
			if existing.IsDir() {
				return fmt.Errorf("%s: %w and is a directory", dest, ErrExists)
			}
			os.Remove(dest)
		}
		if err := os.Symlink(e.Target, dest); err != nil {
			return err
		}
		copied(dest)
		return nil
	case e.Mode.IsRegular():
		if exists {
			if !overwrite {
				return fmt.Errorf("%s: %w (use -f)", dest, ErrExists)
			}
			if existing.IsDir() {
				return fmt.Errorf("%s: %w and is a directory", dest, ErrExists)
			}
			os.Remove(dest) // a symlink there goes, its target stays untouched
		}
		rc, _, err := v.Open(e.Path)
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, e.Mode.Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		os.Chtimes(dest, e.ModTime, e.ModTime)
		copied(dest)
		return nil
	default:
		return nil // devices and FIFOs aren't in converted images
	}
}

// Names maps uids and gids to names using the image's /etc/passwd and
// /etc/group, for listings.
func (f *ImageFS) Names() (users, groups map[uint32]string) {
	read := func(p string) map[uint32]string {
		out := map[uint32]string{}
		rc, _, err := f.Open(p)
		if err != nil {
			return out
		}
		defer rc.Close()
		data, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) < 3 {
				continue
			}
			var id uint32
			if _, err := fmt.Sscan(fields[2], &id); err == nil {
				if _, seen := out[id]; !seen {
					out[id] = fields[0]
				}
			}
		}
		return out
	}
	return read("/etc/passwd"), read("/etc/group")
}

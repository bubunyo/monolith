package oci

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/stretchr/testify/require"
)

// testFS converts a small image with tricky symlinks and opens it.
func testFS(t *testing.T) *ImageFS {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layer(t,
		entry{name: "etc/", typ: tar.TypeDir, mode: 0755},
		entry{name: "etc/os-release", typ: tar.TypeSymlink, body: "../usr/lib/os-release"},
		entry{name: "etc/passwd", typ: tar.TypeReg, mode: 0644, body: "root:x:0:0::/root:/bin/sh\napp:x:1000:1000::/home/app:/bin/sh\n"},
		entry{name: "etc/group", typ: tar.TypeReg, mode: 0644, body: "root:x:0:\nstaff:x:50:\n"},
		entry{name: "usr/lib/os-release", typ: tar.TypeReg, mode: 0644, body: "NAME=Test\n"},
		entry{name: "usr/bin/tool", typ: tar.TypeReg, mode: 04755, body: "#!/bin/true\n"},
		entry{name: "bin", typ: tar.TypeSymlink, body: "usr/bin"},
		// go-containerregistry drops relative links that climb out of the
		// root when flattening, so this one never reaches the image.
		entry{name: "escape", typ: tar.TypeSymlink, body: "../../../../etc/passwd"},
		entry{name: "usr/lib/up", typ: tar.TypeSymlink, body: "../../etc/passwd"},
		entry{name: "absolute", typ: tar.TypeSymlink, body: "/etc/passwd"},
		entry{name: "loop", typ: tar.TypeSymlink, body: "loop"},
		entry{name: "home/app/notes.txt", typ: tar.TypeReg, mode: 0600, uid: 1000, gid: 50, body: "hello"},
	))
	require.NoError(t, err)
	s, err := OpenAt(t.TempDir())
	require.NoError(t, err)
	disk := filepath.Join(t.TempDir(), "rootfs.img")
	_, err = Convert(img, disk, minDiskSize)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(s.DiskPath("x")), 0755))
	require.NoError(t, os.Rename(disk, s.DiskPath("x")))
	f, err := s.OpenFS("x")
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return f
}

func readAll(t *testing.T, f *ImageFS, p string) string {
	t.Helper()
	rc, _, err := f.Open(p)
	require.NoError(t, err, p)
	defer rc.Close()
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	return string(data)
}

func TestImageFSResolve(t *testing.T) {
	f := testFS(t)

	e, err := f.Lstat("/etc/os-release")
	require.NoError(t, err)
	require.True(t, e.IsSymlink())
	require.Equal(t, "../usr/lib/os-release", e.Target)

	require.Equal(t, "NAME=Test\n", readAll(t, f, "/etc/os-release"), "relative symlink")
	require.Equal(t, "#!/bin/true\n", readAll(t, f, "/bin/tool"), "a symlinked directory in the path")

	// Symlinks can't leave the image: absolute targets start from its root
	// and ".." stops there, so these all reach the image's /etc/passwd.
	require.Contains(t, readAll(t, f, "/absolute"), "app:x:1000")
	require.Contains(t, readAll(t, f, "/usr/lib/up"), "app:x:1000")
	r, err := f.Resolve("/../../usr/../../absolute")
	require.NoError(t, err)
	require.Equal(t, "/etc/passwd", r)
	_, err = f.Lstat("/escape")
	require.Error(t, err, "dropped when the image was flattened")

	_, err = f.Resolve("/loop")
	require.ErrorContains(t, err, "too many levels of symbolic links")
	_, err = f.Resolve("/etc/passwd/x")
	require.ErrorContains(t, err, "not a directory")
	_, _, err = f.Open("/nope")
	require.ErrorContains(t, err, "no such file or directory")
	_, _, err = f.Open("/etc")
	require.ErrorContains(t, err, "is a directory")
}

func TestImageFSReadDir(t *testing.T) {
	f := testFS(t)
	root, err := f.ReadDir("/")
	require.NoError(t, err)
	var names []string
	for _, e := range root {
		names = append(names, e.Name)
	}
	require.ElementsMatch(t, []string{"etc", "usr", "bin", "absolute", "loop", "home"}, names)

	bin, err := f.ReadDir("/bin") // follows the directory symlink
	require.NoError(t, err)
	require.Len(t, bin, 1)
	require.Equal(t, "tool", bin[0].Name)
	require.Equal(t, "/usr/bin/tool", bin[0].Path)
	require.NotZero(t, bin[0].Mode&os.ModeSetuid)

	notes, err := f.Stat("/home/app/notes.txt")
	require.NoError(t, err)
	require.Equal(t, [2]uint32{1000, 50}, [2]uint32{notes.UID, notes.GID})
	users, groups := f.Names()
	require.Equal(t, "app", users[1000])
	require.Equal(t, "staff", groups[50])
}

func TestImageFSCopyOut(t *testing.T) {
	f := testFS(t)
	dest := t.TempDir()
	var copied []string
	record := func(p string) { copied = append(copied, p) }

	// A file into an existing directory, following the symlink to it.
	require.NoError(t, f.CopyOut("/etc/os-release", dest, false, record))
	data, err := os.ReadFile(filepath.Join(dest, "os-release"))
	require.NoError(t, err)
	require.Equal(t, "NAME=Test\n", string(data))

	// No overwriting without asking.
	err = f.CopyOut("/etc/os-release", dest, false, record)
	require.ErrorIs(t, err, ErrExists)

	// A directory: symlinks inside it are kept as links, setuid is dropped.
	require.NoError(t, f.CopyOut("/usr", filepath.Join(dest, "u"), false, record))
	fi, err := os.Stat(filepath.Join(dest, "u", "bin", "tool"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), fi.Mode())
	require.NoError(t, f.CopyOut("/", filepath.Join(dest, "all"), false, record))
	link, err := os.Readlink(filepath.Join(dest, "all", "absolute"))
	require.NoError(t, err)
	require.Equal(t, "/etc/passwd", link, "recreated as is, not followed")

	// A symlink on the Mac is replaced with -f, never written through.
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("mac file"), 0644))
	trap := filepath.Join(dest, "trap")
	require.NoError(t, os.Symlink(victim, trap))
	require.NoError(t, f.CopyOut("/etc/passwd", trap, true, record))
	data, err = os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "mac file", string(data))
	fi, err = os.Lstat(trap)
	require.NoError(t, err)
	require.True(t, fi.Mode().IsRegular(), "the link was replaced by the file")
}

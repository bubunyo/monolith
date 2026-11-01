package oci

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

type entry struct {
	name     string
	typ      byte
	mode     int64
	uid, gid int
	body     string // content, or link target
}

func layer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Uid: e.uid, Gid: e.gid, ModTime: time.Unix(1700000000, 0)}
		switch e.typ {
		case tar.TypeReg:
			hdr.Size = int64(len(e.body))
		case tar.TypeSymlink, tar.TypeLink:
			hdr.Linkname = e.body
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if e.typ == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	data := buf.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	require.NoError(t, err)
	return l
}

func TestConvert(t *testing.T) {
	img, err := mutate.AppendLayers(empty.Image,
		layer(t,
			entry{name: "etc/", typ: tar.TypeDir, mode: 0755},
			entry{name: "etc/a", typ: tar.TypeReg, mode: 0644, body: "old\n"},
			entry{name: "etc/shadow", typ: tar.TypeReg, mode: 0640, gid: 42, body: "root:*:\n"},
			entry{name: "var/mail/", typ: tar.TypeDir, mode: 02775, gid: 8},
			entry{name: "tmp/", typ: tar.TypeDir, mode: 01777},
			entry{name: "bin/tool", typ: tar.TypeReg, mode: 04755, body: "#!/bin/true\n"},
			entry{name: "bin/tool2", typ: tar.TypeLink, body: "bin/tool"},
			entry{name: "bin/sh", typ: tar.TypeSymlink, body: "tool"},
			entry{name: "gone", typ: tar.TypeReg, mode: 0644, body: "x"},
			entry{name: "dev/null", typ: tar.TypeChar, mode: 0666},
			entry{name: "implicit/child/file", typ: tar.TypeReg, mode: 0600, uid: 1000, gid: 1000, body: "deep"},
		),
		layer(t,
			entry{name: "etc/a", typ: tar.TypeReg, mode: 0600, body: "new\n"},
			entry{name: ".wh.gone", typ: tar.TypeReg},
			// Writing into var/mail after it got its metadata must not reset it.
			entry{name: "var/mail/root", typ: tar.TypeReg, mode: 0660, gid: 8, body: ""},
		),
	)
	require.NoError(t, err)

	disk := filepath.Join(t.TempDir(), "rootfs.img")
	st, err := Convert(img, disk, minDiskSize)
	require.NoError(t, err)
	require.Equal(t, 1, st.Hardlinks)
	require.Equal(t, 1, st.Skipped, "the device node")
	require.Less(t, st.DiskBytes, int64(64<<20), "zero runs are punched out")
	var fi syscall.Stat_t
	require.NoError(t, syscall.Stat(disk, &fi))
	require.EqualValues(t, minDiskSize, fi.Size)

	b, err := file.OpenFromPath(disk, true)
	require.NoError(t, err)
	defer b.Close()
	fs, err := ext4.Read(b, minDiskSize, 0, 512)
	require.NoError(t, err)

	read := func(p string) string {
		f, err := fs.OpenFile(p, os.O_RDONLY)
		require.NoError(t, err, p)
		defer f.Close()
		data, err := io.ReadAll(f)
		require.NoError(t, err, p)
		return string(data)
	}
	check := func(p string, mode os.FileMode, uid, gid uint32) {
		t.Helper()
		info, err := fs.Stat(p)
		require.NoError(t, err, p)
		require.Equal(t, mode, info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky), p)
		sys := info.Sys().(*ext4.StatT)
		require.Equal(t, [2]uint32{uid, gid}, [2]uint32{sys.UID, sys.GID}, p)
	}

	require.Equal(t, "new\n", read("etc/a"), "upper layer wins")
	check("etc/a", 0600, 0, 0)
	check("etc/shadow", 0640, 0, 42)
	check("var/mail", 0775|os.ModeSetgid, 0, 8)
	check("tmp", 0777|os.ModeSticky, 0, 0)
	check("bin/tool", 0755|os.ModeSetuid, 0, 0)
	require.Equal(t, "#!/bin/true\n", read("bin/tool2"), "hard link copies the target")
	check("bin/tool2", 0755|os.ModeSetuid, 0, 0)
	target, err := fs.ReadLink("bin/sh")
	require.NoError(t, err)
	require.Equal(t, "tool", target)
	check("implicit", 0755, 0, 0)
	check("implicit/child/file", 0600, 1000, 1000)
	require.Equal(t, "deep", read("implicit/child/file"))
	_, err = fs.Stat("gone")
	require.Error(t, err, "whiteout removes the file")
	_, err = fs.Stat("dev/null")
	require.Error(t, err, "device nodes are skipped")
}

func TestConvertFull(t *testing.T) {
	big := strings.Repeat("x", 40<<20)
	img, err := mutate.AppendLayers(empty.Image, layer(t, entry{name: "big", typ: tar.TypeReg, mode: 0644, body: big}))
	require.NoError(t, err)
	_, err = Convert(img, filepath.Join(t.TempDir(), "tiny.img"), 16<<20)
	require.ErrorIs(t, err, errFull)
}

func TestEstimateDiskSize(t *testing.T) {
	require.EqualValues(t, 1<<30, EstimateDiskSize(3<<20), "minimum")
	require.EqualValues(t, 1536<<20, EstimateDiskSize(250<<20), "4x + 512 MiB, in 256 MiB steps")
}

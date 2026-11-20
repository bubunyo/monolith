package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnsureDataDisk(t *testing.T) {
	size := func(path string) int64 {
		fi, err := os.Stat(path)
		require.NoError(t, err)
		return fi.Size()
	}
	current := VMConfig{GuestLayout: GuestLayout}

	// New disk: the default, or the configured size.
	path := filepath.Join(t.TempDir(), "data.img")
	got, err := ensureDataDisk("d", path, current)
	require.NoError(t, err)
	require.EqualValues(t, DefaultDataDiskSize, got)
	require.EqualValues(t, DefaultDataDiskSize, size(path))

	sized := current
	sized.DataDiskSize = 8 << 30
	path2 := filepath.Join(t.TempDir(), "data.img")
	got, err = ensureDataDisk("d", path2, sized)
	require.NoError(t, err)
	require.EqualValues(t, 8<<30, got)

	// Growing.
	sized.DataDiskSize = 40 << 30
	got, err = ensureDataDisk("d", path, sized)
	require.NoError(t, err)
	require.EqualValues(t, 40<<30, got)
	require.EqualValues(t, 40<<30, size(path))

	// Shrinking is refused and leaves the disk alone.
	sized.DataDiskSize = 30 << 30
	_, err = ensureDataDisk("d", path, sized)
	require.ErrorContains(t, err, "not shrink")
	require.EqualValues(t, 40<<30, size(path))

	// A config without a size (older VMs) keeps the existing disk.
	legacy := filepath.Join(t.TempDir(), "data.img")
	require.NoError(t, createSparseFile(legacy, 64<<30))
	got, err = ensureDataDisk("d", legacy, VMConfig{GuestLayout: 4})
	require.NoError(t, err)
	require.EqualValues(t, 64<<30, got)

	// ...and can't grow it: its guest doesn't run resize2fs.
	_, err = ensureDataDisk("d", legacy, VMConfig{GuestLayout: 4, DataDiskSize: 100 << 30})
	require.ErrorContains(t, err, "recreate")
	require.EqualValues(t, 64<<30, size(legacy))
}

func TestCheckGrow(t *testing.T) {
	cur := VMConfig{GuestLayout: GuestLayoutDiskResize}
	old := VMConfig{GuestLayout: GuestLayoutDiskResize - 1}
	require.NoError(t, checkGrow("d", cur, 0, 4<<30))       // no disk yet
	require.NoError(t, checkGrow("d", old, 0, 4<<30))       // no disk yet, any layout
	require.NoError(t, checkGrow("d", old, 20<<30, 20<<30)) // unchanged
	require.NoError(t, checkGrow("d", cur, 20<<30, 40<<30))
	require.Error(t, checkGrow("d", old, 20<<30, 40<<30))
	require.Error(t, checkGrow("d", cur, 40<<30, 20<<30))
}

// Each guest-file feature needs its own layout: a disk created with the
// /var/folders share (layout 5) has no resize2fs in docker-setup.
func TestGuestLayoutOrder(t *testing.T) {
	require.Greater(t, GuestLayoutDiskResize, GuestLayoutTempShare)
	require.LessOrEqual(t, GuestLayoutDiskResize, GuestLayout, "new disks get the resize step")
	require.Error(t, checkGrow("d", VMConfig{GuestLayout: GuestLayoutTempShare}, 20<<30, 40<<30))
}

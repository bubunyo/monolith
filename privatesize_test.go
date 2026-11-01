package monolith

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPrivateSize(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "orig")
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i)
	}
	require.NoError(t, os.WriteFile(orig, data, 0644))
	n, err := PrivateSize(orig)
	require.NoError(t, err)
	require.InDelta(t, 4<<20, n, 64<<10, "a plain file is all private")

	// A clone shares everything until it's written to.
	clone := filepath.Join(dir, "clone")
	require.NoError(t, unix.Clonefile(orig, clone, 0))
	n, err = PrivateSize(clone)
	require.NoError(t, err)
	require.Zero(t, n)

	f, err := os.OpenFile(clone, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(make([]byte, 1<<20), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	n, err = PrivateSize(clone)
	require.NoError(t, err)
	require.InDelta(t, 1<<20, n, 64<<10, "only what was written is private")
}

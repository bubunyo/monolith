package oci

import (
	"archive/tar"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/stretchr/testify/require"
)

// layeredImage pulls a three-layer image into a fresh store:
//
//	0: adds /etc/a, /etc/b, /dir/x/y, /usr/bin/tool and a hard link to it
//	1: changes /etc/a, deletes /etc/b, adds /etc/c
//	2: makes /dir opaque (hiding /dir/x) and adds /dir/z
func layeredImage(t *testing.T) (*Store, Image) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	pushLayers(t, host, "layered:1",
		layer(t,
			entry{name: "etc/", typ: tar.TypeDir, mode: 0755},
			entry{name: "etc/a", typ: tar.TypeReg, mode: 0644, body: "a1"},
			entry{name: "etc/b", typ: tar.TypeReg, mode: 0644, body: "b"},
			entry{name: "dir/x/y", typ: tar.TypeReg, mode: 0644, body: "y"},
			entry{name: "usr/bin/tool", typ: tar.TypeReg, mode: 0755, body: "tool"},
			entry{name: "usr/bin/alias", typ: tar.TypeLink, body: "usr/bin/tool"},
		),
		layer(t,
			entry{name: "etc/a", typ: tar.TypeReg, mode: 0600, body: "a2"},
			entry{name: "etc/.wh.b", typ: tar.TypeReg},
			entry{name: "etc/c", typ: tar.TypeReg, mode: 0644, body: "c"},
		),
		layer(t,
			entry{name: "dir/.wh..wh..opq", typ: tar.TypeReg},
			entry{name: "dir/z", typ: tar.TypeReg, mode: 0644, body: "z"},
		),
	)
	s, err := OpenAt(t.TempDir())
	require.NoError(t, err)
	img, err := s.Pull(context.Background(), host+"/layered:1", PullOptions{})
	require.NoError(t, err)
	return s, img
}

func TestLayerIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("converts an image")
	}
	s, img := layeredImage(t)
	x, err := s.IndexLayers(img)
	require.NoError(t, err)
	require.Len(t, x.Layers, 3)

	kinds := func(l *Layer, prefix string) map[string]byte {
		out := map[string]byte{}
		for _, c := range l.Changes(prefix) {
			out[c.Path] = c.Kind
		}
		return out
	}
	require.Equal(t, map[string]byte{"/etc/a": Changed, "/etc/b": Deleted, "/etc/c": Added}, kinds(x.Layers[1], "/"))
	require.Equal(t, map[string]byte{"/dir/x": Deleted, "/dir/x/y": Deleted, "/dir/z": Added}, kinds(x.Layers[2], "/"))
	require.Equal(t, map[string]byte{"/etc": Added, "/etc/a": Added, "/etc/b": Added}, kinds(x.Layers[0], "/etc"), "the folder itself is listed in layer 0")
	a, c, d := x.Layers[1].Counts()
	require.Equal(t, [3]int{1, 1, 1}, [3]int{a, c, d})

	blame := x.Blame("/etc/a")
	require.Len(t, blame, 2)
	require.Equal(t, [2]byte{Added, Changed}, [2]byte{blame[0].Kind, blame[1].Kind})
	require.Equal(t, 1, blame[1].Layer.Index)
	blame = x.Blame("/dir/x/y")
	require.Len(t, blame, 2)
	require.Equal(t, byte(Deleted), blame[1].Kind)

	// Each layer's own files.
	l1 := x.Layers[1].FS()
	etc, err := l1.ReadDir("/etc")
	require.NoError(t, err)
	var names []string
	for _, e := range etc {
		names = append(names, e.Name)
	}
	require.Equal(t, []string{"a", "c"}, names, "whiteout markers aren't files")
	read := func(v View, p string) string {
		rc, _, err := v.Open(p)
		require.NoError(t, err, p)
		defer rc.Close()
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		return string(data)
	}
	require.Equal(t, "a2", read(l1, "/etc/a"))
	require.Equal(t, "a1", read(x.Layers[0].FS(), "/etc/a"))
	require.Equal(t, "tool", read(x.Layers[0].FS(), "/usr/bin/alias"), "a hard link reads its target")
	_, err = l1.Lstat("/usr")
	require.ErrorContains(t, err, "not in layer 1")
	root, err := x.Layers[0].FS().ReadDir("/")
	require.NoError(t, err)
	require.Len(t, root, 3, "etc, dir, usr; parents the tar didn't list are implied")
}

func TestEnsureLayers(t *testing.T) {
	if testing.Short() {
		t.Skip("converts an image")
	}
	s, img := layeredImage(t)
	require.False(t, s.MissingLayers(img))

	// An image pulled before the layer cache: no layers recorded, none cached.
	old := img
	old.Layers, old.LayerSizes, old.History = nil, nil, nil
	require.NoError(t, writeJSON(s.imagePath(img.ID), old))
	pruned, err := s.pruneBlobs()
	require.NoError(t, err)
	require.Positive(t, pruned)
	require.True(t, s.MissingLayers(old))

	var out strings.Builder
	got, err := s.EnsureLayers(context.Background(), old, PullOptions{Log: &out})
	require.NoError(t, err)
	require.Equal(t, img.Layers, got.Layers)
	require.Equal(t, img.LayerSizes, got.LayerSizes)
	require.Contains(t, out.String(), "Fetching the layers of")
	require.False(t, s.MissingLayers(got))
	saved, err := s.Lookup(img.ID)
	require.NoError(t, err)
	require.Equal(t, img.Layers, saved.Layers, "recorded in image.json")
}

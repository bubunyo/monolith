package oci

import (
	"archive/tar"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"
)

// pushImage pushes a one-file arm64 image with cmd to the test registry.
func pushImage(t *testing.T, host, ref, content string, cmd ...string) {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layer(t, entry{name: "hello", typ: tar.TypeReg, mode: 0644, body: content}))
	require.NoError(t, err)
	img, err = mutate.Config(img, v1.Config{Cmd: cmd, Env: []string{"PATH=/bin"}})
	require.NoError(t, err)
	cf, err := img.ConfigFile()
	require.NoError(t, err)
	cf.OS, cf.Architecture = "linux", "arm64"
	img, err = mutate.ConfigFile(img, cf)
	require.NoError(t, err)
	r, err := name.ParseReference(host + "/" + ref)
	require.NoError(t, err)
	require.NoError(t, remote.Write(r, img))
}

func TestStore(t *testing.T) {
	if testing.Short() {
		t.Skip("converts images (a few seconds each)")
	}
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	pushImage(t, host, "app:1", "one", "/bin/one")
	pushImage(t, host, "app:2", "two", "/bin/two")

	s, err := OpenAt(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()
	ref1, ref2 := host+"/app:1", host+"/app:2"

	img1, err := s.Pull(ctx, ref1, PullOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"/bin/one"}, img1.Config.Cmd)
	require.Equal(t, "linux/arm64", img1.Platform)
	require.FileExists(t, s.DiskPath(img1.ID))

	// Pulling again doesn't convert again; a second tag of the same image
	// shares it.
	again, err := s.Pull(ctx, ref1, PullOptions{})
	require.NoError(t, err)
	require.Equal(t, img1.ID, again.ID)

	img2, err := s.Pull(ctx, ref2, PullOptions{})
	require.NoError(t, err)
	require.NotEqual(t, img1.ID, img2.ID)

	got, err := s.Lookup(ref2)
	require.NoError(t, err)
	require.Equal(t, img2.ID, got.ID)
	got, err = s.Lookup(img1.ID[:8])
	require.NoError(t, err)
	require.Equal(t, img1.ID, got.ID)
	_, err = s.Lookup(host + "/app:3")
	require.ErrorIs(t, err, ErrNotFound)

	images, err := s.List()
	require.NoError(t, err)
	require.Len(t, images, 2)

	// Removing by reference drops the image with its last reference.
	removed, deleted, err := s.Remove(ref1)
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, img1.ID, removed.ID)
	require.NoFileExists(t, s.DiskPath(img1.ID))
	_, err = s.Lookup(ref1)
	require.ErrorIs(t, err, ErrNotFound)

	// Pushing a new image under a tag moves the tag on the next pull.
	pushImage(t, host, "app:2", "two again", "/bin/new")
	img3, err := s.Pull(ctx, ref2, PullOptions{})
	require.NoError(t, err)
	require.NotEqual(t, img2.ID, img3.ID)
	old, err := s.read(img2.ID)
	require.NoError(t, err)
	require.Empty(t, old.Refs, "the old image lost the tag")
}

func TestDisplay(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":          "alpine:latest",
		"nginx:1.29":      "nginx:1.29",
		"bitnami/redis:7": "bitnami/redis:7",
		"ghcr.io/o/r:t":   "ghcr.io/o/r:t",
		"localhost:5000/app@sha256:" + strings.Repeat("a", 64): "localhost:5000/app@sha256:" + strings.Repeat("a", 64),
	} {
		norm, err := Normalize(in)
		require.NoError(t, err, in)
		require.Equal(t, want, Display(norm), in)
	}
	require.Equal(t, "1.5kB", HumanSize(1500))
	require.Equal(t, "61.3MB", HumanSize(61_300_000))
}

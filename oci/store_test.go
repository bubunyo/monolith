package oci

import (
	"archive/tar"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
	removal, err := s.Remove(ref1)
	require.NoError(t, err)
	require.True(t, removal.Deleted)
	require.Equal(t, img1.ID, removal.Image.ID)
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

// pushLayers pushes an arm64 image made of the given layers.
func pushLayers(t *testing.T, host, ref string, layers ...v1.Layer) {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layers...)
	require.NoError(t, err)
	cf, err := img.ConfigFile()
	require.NoError(t, err)
	cf.OS, cf.Architecture = "linux", "arm64"
	cf.Config.Cmd = []string{"/bin/true"}
	img, err = mutate.ConfigFile(img, cf)
	require.NoError(t, err)
	r, err := name.ParseReference(host + "/" + ref)
	require.NoError(t, err)
	require.NoError(t, remote.Write(r, img))
}

func TestPullProgressAndSharedLayers(t *testing.T) {
	if testing.Short() {
		t.Skip("converts images (a few seconds each)")
	}
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	base := layer(t, entry{name: "base", typ: tar.TypeReg, mode: 0644, body: "base"})
	pushLayers(t, host, "a:1", base, layer(t, entry{name: "a", typ: tar.TypeReg, mode: 0644, body: "a"}))
	pushLayers(t, host, "b:1", base, layer(t, entry{name: "b", typ: tar.TypeReg, mode: 0644, body: "b"}))
	baseDigest, err := base.Digest()
	require.NoError(t, err)

	dir := t.TempDir()
	s, err := OpenAt(dir)
	require.NoError(t, err)
	ctx := context.Background()

	// Off a terminal: one line per status change, no escape codes.
	var out strings.Builder
	_, err = s.Pull(ctx, host+"/a:1", PullOptions{Log: &out})
	require.NoError(t, err)
	got := out.String()
	require.Contains(t, got, "Pulling "+host+"/a:1 (2 layers")
	require.Contains(t, got, baseDigest.Hex[:12]+": Downloading\n")
	require.Contains(t, got, baseDigest.Hex[:12]+": Download complete\n")
	require.Contains(t, got, baseDigest.Hex[:12]+": Pull complete\n")
	require.Contains(t, got, "conversion: Converted to ext4\n")
	require.Contains(t, got, "Pulled "+host+"/a:1 in ")
	require.NotContains(t, got, "\033")

	// The shared base layer comes from the cache, naming the image it's from.
	out.Reset()
	_, err = s.Pull(ctx, host+"/b:1", PullOptions{Log: &out})
	require.NoError(t, err)
	require.Contains(t, out.String(), baseDigest.Hex[:12]+": Already exists ("+host+"/a:1)\n")
	blobs, err := os.ReadDir(filepath.Join(dir, blobsDir, "sha256"))
	require.NoError(t, err)
	require.Len(t, blobs, 3, "base, a and b")

	// Removing a keeps the base for b and drops a's own layer.
	removal, err := s.Remove(host + "/a:1")
	require.NoError(t, err)
	require.Equal(t, 1, removal.KeptLayers)
	require.Equal(t, []string{host + "/b:1"}, removal.SharedWith)
	blobs, err = os.ReadDir(filepath.Join(dir, blobsDir, "sha256"))
	require.NoError(t, err)
	require.Len(t, blobs, 2)
	require.FileExists(t, s.blobPath(baseDigest))

	removal, err = s.Remove(host + "/b:1")
	require.NoError(t, err)
	require.Zero(t, removal.KeptLayers)
	blobs, err = os.ReadDir(filepath.Join(dir, blobsDir, "sha256"))
	require.NoError(t, err)
	require.Empty(t, blobs)
}

func TestPullViewTerminal(t *testing.T) {
	render := func() string {
		var out strings.Builder
		v := newPullView(&out, true, []string{"0123456789ab"})
		v.set(0, "Downloading", 50, 100)
		v.redraw()
		v.set(0, "Pull complete", 0, 0)
		v.close()
		return out.String()
	}

	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	got := render()
	require.Contains(t, got, "\r\033[2K", "lines are cleared and redrawn")
	require.Contains(t, got, "\033[1A", "the cursor goes back up for the next redraw")
	require.Contains(t, got, "\033[36mDownloading")
	require.Contains(t, got, "[====================>")
	require.Contains(t, got, "\033[32mPull complete")

	t.Setenv("NO_COLOR", "1")
	got = render()
	require.Contains(t, got, "\r\033[2K0123456789ab: Downloading")
	require.NotContains(t, got, "\033[36m")
	require.NotContains(t, got, "\033[32m")
}

func TestViewLineWidth(t *testing.T) {
	l := viewLine{id: "0123456789ab", status: "Downloading", done: 1, total: 2}
	require.Len(t, []rune(l.render(30, false)), 30, "cut to the terminal's width")
	colored := l.render(30, true)
	require.Contains(t, colored, "\033[0m")
	require.Len(t, []rune(regexp.MustCompile("\033\\[[0-9;]*m").ReplaceAllString(colored, "")), 30, "escape codes don't count")
}

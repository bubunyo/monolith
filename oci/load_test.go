package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

func platformImage(t *testing.T, arch, content string) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, layer(t, entry{name: "hello", typ: tar.TypeReg, mode: 0644, body: content}))
	require.NoError(t, err)
	cf, err := img.ConfigFile()
	require.NoError(t, err)
	cf.OS, cf.Architecture = "linux", arch
	img, err = mutate.ConfigFile(img, cf)
	require.NoError(t, err)
	return img
}

func TestChoosePlatform(t *testing.T) {
	arm := v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	amd := PlatformAMD64
	arm7 := v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}

	p, err := choosePlatform([]v1.Platform{amd, arm}, nil)
	require.NoError(t, err)
	require.Equal(t, arm, p, "arm64 preferred, any variant")

	p, err = choosePlatform([]v1.Platform{arm7, amd}, nil)
	require.NoError(t, err)
	require.Equal(t, amd, p, "amd64 when there's no arm64")

	p, err = choosePlatform([]v1.Platform{amd, arm}, &PlatformAMD64)
	require.NoError(t, err)
	require.Equal(t, amd, p, "--platform wins")

	_, err = choosePlatform([]v1.Platform{arm}, &PlatformAMD64)
	require.ErrorContains(t, err, "no linux/amd64 variant (has linux/arm64/v8)")

	_, err = choosePlatform([]v1.Platform{arm7}, nil)
	require.ErrorContains(t, err, "has linux/arm/v7")
}

func TestParseLoadPlatform(t *testing.T) {
	p, err := ParseLoadPlatform("")
	require.NoError(t, err)
	require.Nil(t, p)
	p, err = ParseLoadPlatform("linux/amd64")
	require.NoError(t, err)
	require.Equal(t, PlatformAMD64, *p)
	_, err = ParseLoadPlatform("linux/386")
	require.Error(t, err)
}

func TestLoadRef(t *testing.T) {
	tag, err := LoadRef("nginx")
	require.NoError(t, err)
	require.Equal(t, "latest", tag.TagStr())
	_, err = LoadRef("nginx@sha256:" + strings.Repeat("a", 64))
	require.ErrorContains(t, err, "by digest")
}

func TestRelabelKeepsLayers(t *testing.T) {
	img := platformImage(t, "amd64", "x86")
	out, err := Relabel(img)
	require.NoError(t, err)
	cf, err := out.ConfigFile()
	require.NoError(t, err)
	require.Equal(t, "arm64", cf.Architecture)
	before, _ := img.Layers()
	after, _ := out.Layers()
	require.Len(t, after, len(before))
	for i := range before {
		d1, _ := before[i].Digest()
		d2, _ := after[i].Digest()
		require.Equal(t, d1, d2)
	}
}

// Resolve picks the variant, Fetch caches its layers and WriteTarball
// writes a docker-save archive under the given name, relabelled when amd64.
func TestResolveFetchWriteTarball(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()

	amdOnly, err := name.NewTag(host + "/vendor/app:1")
	require.NoError(t, err)
	require.NoError(t, remote.Write(amdOnly, platformImage(t, "amd64", "x86 only")))

	multi, err := name.NewTag(host + "/multi:1")
	require.NoError(t, err)
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: platformImage(t, "amd64", "x86"), Descriptor: v1.Descriptor{Platform: &PlatformAMD64}},
		mutate.IndexAddendum{Add: platformImage(t, "arm64", "arm"), Descriptor: v1.Descriptor{Platform: &PlatformARM64}})
	require.NoError(t, remote.WriteIndex(multi, idx))

	s, err := OpenAt(t.TempDir())
	require.NoError(t, err)

	r, err := Resolve(ctx, multi, nil)
	require.NoError(t, err)
	require.False(t, r.Relabel)
	require.Equal(t, "arm64", r.Platform.Architecture)
	require.Len(t, r.Available, 2)

	r, err = Resolve(ctx, amdOnly, nil)
	require.NoError(t, err)
	require.True(t, r.Relabel)

	rep := &recordingReporter{}
	require.NoError(t, s.Fetch(ctx, r, rep))
	require.Contains(t, rep.statuses, "Download complete")

	var buf bytes.Buffer
	var progress int64
	require.NoError(t, s.WriteTarball(r, amdOnly, &buf, func(n int64) { progress = n }))
	require.Equal(t, r.Size, progress, "every layer byte read from the cache")

	data := buf.Bytes()
	img, err := tarball.Image(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, &amdOnly)
	require.NoError(t, err, "the archive carries the image under its name")
	cf, err := img.ConfigFile()
	require.NoError(t, err)
	require.Equal(t, "arm64", cf.Architecture, "relabelled")

	// A second fetch finds the layers cached.
	rep = &recordingReporter{}
	require.NoError(t, s.Fetch(ctx, r, rep))
	require.Contains(t, rep.statuses, "Already exists")
}

type recordingReporter struct{ statuses []string }

func (r *recordingReporter) SetLayer(_ int, status string, _, _ int64) {
	r.statuses = append(r.statuses, status)
}

func TestLoadViewOffTerminal(t *testing.T) {
	var buf bytes.Buffer
	v := NewLoadView(&buf, false)
	g := v.Add("nginx:1.27")
	failed := v.Add("bad:1")
	g.Start("arm64", []string{"aaaaaaaaaaaa"})
	g.SetLayer(0, "Downloading", 10, 100) // not printed off a terminal
	g.Stage("Loading into cluster-x", 1, 10)
	g.Stage("Loading into cluster-x", 5, 10) // same stage: printed once
	g.Done()
	failed.Fail(io.ErrUnexpectedEOF)
	v.Close()
	out := buf.String()
	require.Equal(t, 1, strings.Count(out, "Loading into cluster-x"))
	require.Contains(t, out, "nginx:1.27 (arm64): Loaded in")
	require.Contains(t, out, "bad:1: Error: unexpected EOF")
	require.NotContains(t, out, "Downloading")
}

func TestLoadViewTerminal(t *testing.T) {
	var buf bytes.Buffer
	v := &LoadView{w: &buf, tty: true, width: 80, stop: make(chan struct{}), done: make(chan struct{})}
	close(v.done) // no redraw loop: redraw by hand
	a := v.Add("a:1")
	v.Add("b:1")
	a.Start("arm64", []string{"aaaaaaaaaaaa"})
	a.SetLayer(0, "Downloading", 50, 100)
	v.redraw()
	first := buf.String()
	require.Contains(t, first, "a:1 (arm64)")
	require.Contains(t, first, "Downloading")
	require.Contains(t, first, "Waiting: b:1")

	a.Done()
	buf.Reset()
	v.redraw()
	v.redraw()
	out := buf.String()
	require.Equal(t, 1, strings.Count(out, "✔ a:1 (arm64)"), "a finished image is printed once")
	require.NotContains(t, out, "Downloading")
}

func TestResolveMissingImage(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	ref, err := name.NewTag(strings.TrimPrefix(srv.URL, "http://") + "/nope:1")
	require.NoError(t, err)
	_, err = Resolve(context.Background(), ref, nil)
	require.ErrorContains(t, err, "not found in its registry")
}

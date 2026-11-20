package cluster

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/bubunyo/monolith/oci"
)

func TestReadImageList(t *testing.T) {
	refs, err := readImageList(strings.NewReader("nginx:1.27\n\n# base images\n  redis:7  # cache\nvendor/app:1.2\n"))
	require.NoError(t, err)
	require.Equal(t, []string{"nginx:1.27", "redis:7", "vendor/app:1.2"}, refs)

	_, err = readImageList(strings.NewReader("nginx:1.27 redis:7\n"))
	require.ErrorContains(t, err, "line 1: one image per line")
}

func TestParseFrom(t *testing.T) {
	for in, want := range map[string]string{"": "", "registry": "", "docker": defaultDockerHost, "docker:builder": "builder"} {
		got, err := parseFrom(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"docker:", "podman", "registry:x"} {
		_, err := parseFrom(bad)
		require.Error(t, err, bad)
	}
}

func TestLoadJobsFromStdinWithoutDuplicates(t *testing.T) {
	oldFile, oldInputs := flagLoadFile, flagLoadInputs
	t.Cleanup(func() { flagLoadFile, flagLoadInputs = oldFile, oldInputs })
	flagLoadFile, flagLoadInputs = "", nil

	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("nginx\nnginx:latest\nredis:7\n"))
	jobs, err := loadJobs(cmd, nil)
	require.NoError(t, err)
	require.Len(t, jobs, 2, "nginx and nginx:latest are the same image")
	require.Equal(t, "nginx", jobs[0].ref)
	require.Equal(t, "redis:7", jobs[1].ref)

	jobs, err = loadJobs(cmd, []string{"alpine:3.21"})
	require.NoError(t, err)
	require.Len(t, jobs, 1, "arguments, not stdin, when given")

	_, err = loadJobs(cmd, []string{"alpine@sha256:" + strings.Repeat("a", 64)})
	require.ErrorContains(t, err, "by digest")

	flagLoadInputs = []string{"-"}
	flagLoadFile = "-"
	_, err = loadJobs(cmd, nil)
	require.ErrorContains(t, err, "stdin can't hold both")
}

func TestSummarize(t *testing.T) {
	jobs := []loadJob{{ref: "a:1"}, {ref: "b:1"}}
	var buf bytes.Buffer
	require.NoError(t, summarize(&buf, false, "c", jobs, []error{nil, nil}, nil))
	require.Contains(t, buf.String(), "Loaded 2 of 2 images into cluster-c.")

	buf.Reset()
	err := summarize(&buf, false, "c", jobs, []error{nil, errors.New("boom")}, []string{"a:1 is a :latest tag"})
	require.EqualError(t, err, "1 image failed to load")
	require.Contains(t, buf.String(), "Loaded 1 of 2 images")
	require.Contains(t, buf.String(), "b:1: boom")
	require.Contains(t, buf.String(), "Note: a:1 is a :latest tag")
}

// An amd64-only image is refused before anything downloads when the
// cluster has no Rosetta.
func TestLoadRefusesAmd64WithoutRosetta(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/vendor/app:1"
	tag, err := name.NewTag(ref)
	require.NoError(t, err)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "x", Mode: 0644, Size: 1}))
	tw.Write([]byte("x"))
	tw.Close()
	data := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	require.NoError(t, err)
	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)
	cf, err := img.ConfigFile()
	require.NoError(t, err)
	cf.OS, cf.Architecture = "linux", "amd64"
	img, err = mutate.ConfigFile(img, cf)
	require.NoError(t, err)
	require.NoError(t, remote.Write(tag, img))

	store, err := oci.OpenAt(t.TempDir())
	require.NoError(t, err)
	l := &loader{cluster: "c", rosetta: false, store: store}
	g := oci.NewLoadView(io.Discard, false).Add(ref)
	err = l.loadFromRegistry(context.Background(), loadJob{ref: ref, tag: tag}, g)
	require.ErrorContains(t, err, "needs Rosetta")
	require.ErrorContains(t, err, "cluster start --name c --rosetta")
}

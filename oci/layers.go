package oci

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/bubunyo/monolith"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
)

// Layers are downloaded into blobs/sha256/<hex> (compressed, as the
// registry serves them) before converting, so their progress can be shown,
// several download at once, and images that share base layers download
// them once. rmi deletes the blobs no image uses any more.

const (
	blobsDir          = "blobs"
	parallelDownloads = 8
)

func (s *Store) blobPath(d v1.Hash) string {
	return filepath.Join(s.dir, blobsDir, d.Algorithm, d.Hex)
}

// download fetches the layers that aren't cached yet, up to
// parallelDownloads at a time, reporting each on line i of the view. A
// cached layer is reported with the image it came from.
func (s *Store) download(ctx context.Context, layers []v1.Layer, view *pullView) error {
	owners := s.layerOwners()
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(parallelDownloads)
	for i, l := range layers {
		g.Go(func() error {
			return s.downloadLayer(ctx, i, l, view, owners)
		})
	}
	return g.Wait()
}

func (s *Store) downloadLayer(ctx context.Context, i int, l v1.Layer, view *pullView, owners map[string][]string) error {
	d, err := l.Digest()
	if err != nil {
		return err
	}
	path := s.blobPath(d)
	if _, err := os.Stat(path); err == nil {
		status := "Already exists"
		if names := owners[d.String()]; len(names) > 0 {
			status += " (" + names[0] + ")"
		}
		view.set(i, status, 0, 0)
		return nil
	}
	size, _ := l.Size()
	view.set(i, "Downloading", 0, size)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	// The reader verifies the digest at the end, so a bad download fails
	// before the blob is kept.
	rc, err := l.Compressed()
	if err != nil {
		return fmt.Errorf("layer %s: %w", d.Hex[:12], err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	src := &countingReader{r: rc, fn: func(n int64) { view.set(i, "Downloading", n, size) }}
	_, err = io.Copy(tmp, contextReader{ctx, src})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("layer %s: %w", d.Hex[:12], err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	view.set(i, "Download complete", 0, 0)
	return nil
}

// contextReader stops a download when ctx is cancelled (Ctrl+C).
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// cachedImage is an image whose layers read from the blob cache.
type cachedImage struct {
	v1.Image
	layers []v1.Layer
}

func (c cachedImage) Layers() ([]v1.Layer, error) { return c.layers, nil }

// cachedLayer reads a cached blob, reporting progress as it's extracted.
// mutate.Extract only calls Uncompressed.
type cachedLayer struct {
	v1.Layer
	path     string
	size     int64
	progress func(done int64, finished bool)
}

func (c *cachedLayer) Uncompressed() (io.ReadCloser, error) {
	return openBlob(c.path, func(n int64) { c.progress(n, n >= c.size) })
}

// openBlob opens a cached layer as its uncompressed tar (gzip, zstd or
// plain, by magic), reporting compressed bytes read through progress.
func openBlob(path string, progress func(int64)) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if progress == nil {
		progress = func(int64) {}
	}
	counted := &countingReader{r: f, fn: progress}
	br := bufio.NewReader(counted)
	magic, _ := br.Peek(4)
	var r io.Reader = br
	closeFn := f.Close
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			f.Close()
			return nil, err
		}
		r = gz
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		zr, err := zstd.NewReader(br)
		if err != nil {
			f.Close()
			return nil, err
		}
		r = zr
		closeFn = func() error { zr.Close(); return f.Close() }
	}
	return readCloser{r, closeFn}, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// fromCache returns img with its layers read from the blob cache, each
// reporting extraction on its line of the view and all of them on the
// summary line.
func (s *Store) fromCache(img v1.Image, layers []v1.Layer, view *pullView, summary int) (v1.Image, error) {
	var total int64
	sizes := make([]int64, len(layers))
	for i, l := range layers {
		sizes[i], _ = l.Size()
		total += sizes[i]
	}
	var mu sync.Mutex
	done := make([]int64, len(layers))
	cached := make([]v1.Layer, len(layers))
	for i, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return nil, err
		}
		cached[i] = &cachedLayer{Layer: l, path: s.blobPath(d), size: sizes[i], progress: func(n int64, finished bool) {
			if finished {
				view.set(i, "Pull complete", 0, 0)
			} else {
				view.set(i, "Extracting", n, sizes[i])
			}
			mu.Lock()
			done[i] = n
			var sum int64
			for _, v := range done {
				sum += v
			}
			mu.Unlock()
			view.set(summary, "Converting", sum, total)
		}}
	}
	return cachedImage{Image: img, layers: cached}, nil
}

// layerOwners maps each cached layer digest to the images (display names,
// oldest first) that use it.
func (s *Store) layerOwners() map[string][]string {
	owners := map[string][]string{}
	images, _ := s.List() // newest first
	for i := len(images) - 1; i >= 0; i-- {
		name := images[i].ShortID()
		if len(images[i].Refs) > 0 {
			name = Display(images[i].Refs[0])
		}
		for _, d := range images[i].Layers {
			owners[d] = append(owners[d], name)
		}
	}
	return owners
}

// pruneBlobs deletes cached layers no image uses and returns the bytes it
// freed.
func (s *Store) pruneBlobs() (int64, error) {
	used := s.layerOwners()
	dir := filepath.Join(s.dir, blobsDir, "sha256")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, e := range entries {
		if len(used["sha256:"+e.Name()]) == 0 {
			p := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(p); err == nil && os.Remove(p) == nil {
				freed += fi.Size()
			}
		}
	}
	return freed, nil
}

// Reclaimable estimates the space removing images ids would free: the
// part of each image's disk not shared with containers' clones, plus the
// cached layers only those images use.
func (s *Store) Reclaimable(ids []string) int64 {
	removing := map[string]bool{}
	for _, id := range ids {
		removing[id] = true
	}
	images, _ := s.List()
	keep := map[string]bool{} // layers images that stay still use
	var total int64
	for _, img := range images {
		if !removing[img.ID] {
			for _, d := range img.Layers {
				keep[d] = true
			}
			continue
		}
		if n, err := monolith.PrivateSize(s.DiskPath(img.ID)); err == nil {
			total += n
		}
	}
	counted := map[string]bool{}
	for _, img := range images {
		if !removing[img.ID] {
			continue
		}
		for _, d := range img.Layers {
			if keep[d] || counted[d] {
				continue
			}
			counted[d] = true
			if h, err := v1.NewHash(d); err == nil {
				if fi, err := os.Stat(s.blobPath(h)); err == nil {
					total += fi.Size()
				}
			}
		}
	}
	return total
}

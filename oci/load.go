package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// `monolith cluster load` support: pick the platform to load, relabel amd64
// images as arm64 (the node only accepts its own platform; Rosetta runs the
// x86-64 binaries), download layers into the blob cache and stream the image
// as a `docker save` tarball for containerd to import.

var (
	PlatformARM64 = v1.Platform{OS: "linux", Architecture: "arm64"}
	PlatformAMD64 = v1.Platform{OS: "linux", Architecture: "amd64"}
)

// ParseLoadPlatform accepts the platforms a cluster can run: linux/arm64
// natively, linux/amd64 through Rosetta.
func ParseLoadPlatform(s string) (*v1.Platform, error) {
	switch strings.TrimSpace(s) {
	case "":
		return nil, nil
	case "linux/arm64", "linux/arm64/v8", "arm64":
		p := PlatformARM64
		return &p, nil
	case "linux/amd64", "amd64":
		p := PlatformAMD64
		return &p, nil
	}
	return nil, fmt.Errorf("--platform must be linux/arm64 or linux/amd64, not %q", s)
}

// LoadRef parses a reference for loading. The image is written under this
// name, so it must have a tag (none means latest); digests are refused.
func LoadRef(ref string) (name.Tag, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return name.Tag{}, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	t, ok := r.(name.Tag)
	if !ok {
		return name.Tag{}, fmt.Errorf("%s: loading by digest isn't supported; use a tag", ref)
	}
	return t, nil
}

// Resolved is an image picked for loading.
type Resolved struct {
	Image     v1.Image
	Platform  v1.Platform   // the variant picked
	Relabel   bool          // not arm64: relabel it, Rosetta runs it
	Available []v1.Platform // what the registry has
	Size      int64         // compressed layers, as the registry reports
	Layers    []v1.Layer
}

// Resolve looks ref up in its registry and picks the variant to load:
// want if given, else arm64, else amd64.
func Resolve(ctx context.Context, ref name.Tag, want *v1.Platform) (*Resolved, error) {
	opts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain)}
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, friendlyRegistryError(err)
	}
	var img v1.Image
	var avail []v1.Platform
	var picked v1.Platform
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return nil, err
		}
		im, err := idx.IndexManifest()
		if err != nil {
			return nil, err
		}
		digests := map[string]v1.Hash{}
		for _, m := range im.Manifests {
			if m.Platform == nil || m.Platform.OS == "unknown" {
				continue // attestations
			}
			avail = append(avail, *m.Platform)
			if _, ok := digests[platformKey(*m.Platform)]; !ok {
				digests[platformKey(*m.Platform)] = m.Digest
			}
		}
		if picked, err = choosePlatform(avail, want); err != nil {
			return nil, err
		}
		if img, err = idx.Image(digests[platformKey(picked)]); err != nil {
			return nil, err
		}
	} else {
		if img, err = desc.Image(); err != nil {
			return nil, err
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return nil, err
		}
		avail = []v1.Platform{{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant}}
		if picked, err = choosePlatform(avail, want); err != nil {
			return nil, err
		}
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	var size int64
	for _, l := range layers {
		if n, err := l.Size(); err == nil {
			size += n
		}
	}
	return &Resolved{Image: img, Platform: picked, Relabel: picked.Architecture != "arm64",
		Available: avail, Size: size, Layers: layers}, nil
}

// platformKey ignores the arm64 variant (v8 or none are the same here).
func platformKey(p v1.Platform) string {
	if p.Architecture == "arm64" {
		return p.OS + "/arm64"
	}
	return p.OS + "/" + p.Architecture + "/" + p.Variant
}

// choosePlatform picks want if it's available, else linux/arm64, else
// linux/amd64.
func choosePlatform(avail []v1.Platform, want *v1.Platform) (v1.Platform, error) {
	has := func(p v1.Platform) (v1.Platform, bool) {
		for _, a := range avail {
			if a.OS == p.OS && a.Architecture == p.Architecture && (p.Architecture == "arm64" || a.Variant == p.Variant) {
				return a, true
			}
		}
		return v1.Platform{}, false
	}
	if want != nil {
		if p, ok := has(*want); ok {
			return p, nil
		}
		return v1.Platform{}, fmt.Errorf("no %s variant (has %s)", platformString(*want), platformList(avail))
	}
	for _, p := range []v1.Platform{PlatformARM64, PlatformAMD64} {
		if a, ok := has(p); ok {
			return a, nil
		}
	}
	return v1.Platform{}, fmt.Errorf("no linux/arm64 or linux/amd64 variant (has %s)", platformList(avail))
}

func platformString(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func platformList(ps []v1.Platform) string {
	if len(ps) == 0 {
		return "none"
	}
	var s []string
	for _, p := range ps {
		s = append(s, platformString(p))
	}
	return strings.Join(s, ", ")
}

// Relabel marks img as linux/arm64 without touching its layers, so the
// node accepts it; Rosetta then runs its x86-64 binaries.
func Relabel(img v1.Image) (v1.Image, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	cf.OS, cf.Architecture, cf.Variant = "linux", "arm64", ""
	return mutate.ConfigFile(img, cf)
}

// Fetch downloads r's layers into the blob cache, reporting each on line i
// of view. Hold SharedLock while fetching and writing.
func (s *Store) Fetch(ctx context.Context, r *Resolved, view LayerReporter) error {
	return s.download(ctx, r.Layers, reporterView{view})
}

// LayerReporter receives per-layer progress (LoadGroup implements it).
type LayerReporter interface {
	SetLayer(i int, status string, done, total int64)
}

type reporterView struct{ r LayerReporter }

func (v reporterView) set(i int, status string, done, total int64) {
	v.r.SetLayer(i, status, done, total)
}

// WriteTarball writes r (relabelled if needed) as a `docker save` tarball
// named tag, reading the layers Fetch put in the blob cache. progress gets
// the layer bytes read so far.
func (s *Store) WriteTarball(r *Resolved, tag name.Tag, w io.Writer, progress func(int64)) error {
	img := r.Image
	if r.Relabel {
		var err error
		if img, err = Relabel(img); err != nil {
			return err
		}
	}
	return tarball.Write(tag, s.cachedForWrite(img, r.Layers, progress), w)
}

// cachedForWrite is img with each layer's compressed bytes read from the
// blob cache.
func (s *Store) cachedForWrite(img v1.Image, layers []v1.Layer, progress func(int64)) v1.Image {
	var read int64
	count := func(n int64) {
		read += n
		if progress != nil {
			progress(read)
		}
	}
	cached := make([]v1.Layer, len(layers))
	for i, l := range layers {
		cached[i] = &blobLayer{Layer: l, store: s, count: count}
	}
	return &cachedImage{Image: img, layers: cached}
}

// blobLayer reads a layer's compressed bytes from the blob cache.
type blobLayer struct {
	v1.Layer
	store *Store
	count func(int64)
}

func (b *blobLayer) Compressed() (io.ReadCloser, error) {
	d, err := b.Digest()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(b.store.blobPath(d))
	if err != nil {
		return nil, fmt.Errorf("layer %s isn't in the cache: %w", d.Hex[:12], err)
	}
	var last int64
	return readCloser{&countingReader{r: f, fn: func(n int64) { b.count(n - last); last = n }}, f.Close}, nil
}

// RelabelArchive reads the image tag from a `docker save` archive at path
// and writes it to w relabelled as linux/arm64 (images exported from an
// amd64 docker host). progress gets the archive bytes read so far.
func RelabelArchive(path string, tag name.Tag, w io.Writer, progress func(int64)) error {
	img, err := tarball.ImageFromPath(path, &tag)
	if err != nil {
		return err
	}
	if img, err = Relabel(img); err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	var read int64
	counted := make([]v1.Layer, len(layers))
	for i, l := range layers {
		counted[i] = &countedLayer{Layer: l, count: func(n int64) {
			read += n
			if progress != nil {
				progress(read)
			}
		}}
	}
	return tarball.Write(tag, &cachedImage{Image: img, layers: counted}, w)
}

// countedLayer reports the compressed bytes read from a layer.
type countedLayer struct {
	v1.Layer
	count func(int64)
}

func (c *countedLayer) Compressed() (io.ReadCloser, error) {
	rc, err := c.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	var last int64
	return readCloser{&countingReader{r: rc, fn: func(n int64) { c.count(n - last); last = n }}, rc.Close}, nil
}

// friendlyRegistryError shortens the registry's answer for a missing image:
// registries say "unauthorized" for repositories that don't exist, too.
func friendlyRegistryError(err error) error {
	var te *transport.Error
	if errors.As(err, &te) {
		for _, d := range te.Errors {
			switch d.Code {
			case transport.UnauthorizedErrorCode, transport.DeniedErrorCode,
				transport.NameUnknownErrorCode, transport.ManifestUnknownErrorCode:
				return fmt.Errorf("not found in its registry, or you have no access ('docker login' for private images)")
			}
		}
	}
	return err
}

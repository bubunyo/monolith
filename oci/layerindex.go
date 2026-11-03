package oci

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// View is a read-only filesystem the image shell can browse: the whole
// image (ImageFS) or one layer (LayerFS). Paths are absolute.
type View interface {
	Lstat(p string) (Entry, error)
	Stat(p string) (Entry, error)
	ReadDir(p string) ([]Entry, error)
	Open(p string) (io.ReadCloser, Entry, error)
	Resolve(p string) (string, error)
}

// Change kinds in a layer, relative to the layers below it.
const (
	Added   = 'A'
	Changed = 'M'
	Deleted = 'D'
)

// LayerIndex is what each layer of an image contains and changes,
// built by reading the cached layers bottom to top and applying Docker's
// whiteouts (".wh.NAME" deletes NAME; ".wh..wh..opq" hides everything
// lower layers had in that directory).
type LayerIndex struct {
	Layers []*Layer
}

// Layer is one layer's contents and its changes.
type Layer struct {
	Index   int
	Digest  string
	Size    int64  // compressed
	Step    string // the build step that made it, if recorded
	blob    string
	entries map[string]Entry // its files (no whiteout markers)
	links   map[string]string
	kids    map[string][]string // directory -> child names, implicit parents included
	changes map[string]byte     // path -> Added, Changed or Deleted
}

// Counts returns how many paths the layer added, changed and deleted.
func (l *Layer) Counts() (added, changed, deleted int) {
	for _, c := range l.changes {
		switch c {
		case Added:
			added++
		case Changed:
			changed++
		case Deleted:
			deleted++
		}
	}
	return
}

// ChangeList is one line of `changes`.
type ChangeList struct {
	Kind byte
	Path string
}

// Changes lists what the layer added, changed and deleted under prefix
// (or everywhere), sorted by path.
func (l *Layer) Changes(prefix string) []ChangeList {
	prefix = clean(prefix)
	var out []ChangeList
	for p, c := range l.changes {
		if prefix == "/" || p == prefix || strings.HasPrefix(p, prefix+"/") {
			out = append(out, ChangeList{Kind: c, Path: p})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Blame is what one layer did to a path.
type Blame struct {
	Layer *Layer
	Kind  byte
	Via   string // for deletions: the deleted ancestor, if not the path itself
}

// Blame lists the layers that added, changed or deleted p (or deleted a
// directory above it), bottom first.
func (x *LayerIndex) Blame(p string) []Blame {
	p = clean(p)
	var out []Blame
	for _, l := range x.Layers {
		if c, ok := l.changes[p]; ok {
			out = append(out, Blame{Layer: l, Kind: c})
			continue
		}
		for dir := path.Dir(p); dir != "/"; dir = path.Dir(dir) {
			if l.changes[dir] == Deleted {
				out = append(out, Blame{Layer: l, Kind: Deleted, Via: dir})
				break
			}
		}
	}
	return out
}

// IndexLayers reads img's cached layers. All of them must be in the blob
// cache (EnsureLayers fetches missing ones).
func (s *Store) IndexLayers(img Image) (*LayerIndex, error) {
	steps := layerSteps(img)
	x := &LayerIndex{}
	merged := map[string]bool{} // paths present after the layers so far
	for i, d := range img.Layers {
		h, err := v1.NewHash(d)
		if err != nil {
			return nil, err
		}
		l := &Layer{
			Index:   i,
			Digest:  d,
			Step:    steps[i],
			blob:    s.blobPath(h),
			entries: map[string]Entry{},
			links:   map[string]string{},
			kids:    map[string][]string{},
			changes: map[string]byte{},
		}
		if i < len(img.LayerSizes) {
			l.Size = img.LayerSizes[i]
		}
		if err := l.read(merged); err != nil {
			return nil, fmt.Errorf("layer %d (%s): %w", i, shortDigest(d), err)
		}
		x.Layers = append(x.Layers, l)
	}
	return x, nil
}

// read indexes the layer's tar and records its changes against merged
// (the lower layers), then applies them to merged.
func (l *Layer) read(merged map[string]bool) error {
	rc, err := openBlob(l.blob, nil)
	if err != nil {
		return err
	}
	defer rc.Close()
	var whiteouts, opaques []string
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p := clean(hdr.Name)
		if p == "/" {
			continue
		}
		dir, base := path.Dir(p), path.Base(p)
		switch {
		case base == ".wh..wh..opq":
			opaques = append(opaques, dir)
			continue
		case strings.HasPrefix(base, ".wh."):
			whiteouts = append(whiteouts, path.Join(dir, strings.TrimPrefix(base, ".wh.")))
			continue
		}
		e := Entry{Name: base, Path: p, Mode: fileMode(hdr.Mode), UID: uint32(hdr.Uid), GID: uint32(hdr.Gid), Size: hdr.Size, ModTime: hdr.ModTime}
		switch hdr.Typeflag {
		case tar.TypeDir:
			e.Mode |= fs.ModeDir
			e.Size = 0
		case tar.TypeSymlink:
			e.Mode |= fs.ModeSymlink
			e.Target = hdr.Linkname
		case tar.TypeLink:
			l.links[p] = clean(hdr.Linkname)
		case tar.TypeReg:
		default:
			continue // devices and FIFOs
		}
		l.entries[p] = e
	}

	// Deletions first: they act on the lower layers only.
	deleteTree := func(p string) {
		if merged[p] {
			l.changes[p] = Deleted
		}
		delete(merged, p)
		for q := range merged {
			if strings.HasPrefix(q, p+"/") {
				delete(merged, q)
			}
		}
	}
	for _, dir := range opaques {
		for q := range merged {
			if strings.HasPrefix(q, dir+"/") {
				l.changes[q] = Deleted
				delete(merged, q)
			}
		}
	}
	for _, p := range whiteouts {
		deleteTree(p)
	}
	// In path order, so a directory is seen before what's in it.
	paths := make([]string, 0, len(l.entries))
	for p := range l.entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		e := l.entries[p]
		switch {
		case merged[p]:
			if !e.IsDir() { // re-listed directories aren't changes
				l.changes[p] = Changed
			}
		default:
			l.changes[p] = Added
		}
		merged[p] = true
		// Parents the tar didn't list exist too (not counted as changes).
		for dir := path.Dir(p); dir != "/" && !merged[dir]; dir = path.Dir(dir) {
			merged[dir] = true
		}
	}

	// The directory tree, with parents the tar didn't list.
	seen := map[string]bool{}
	for p := range l.entries {
		for child := p; child != "/"; child = path.Dir(child) {
			parent := path.Dir(child)
			key := parent + "\x00" + path.Base(child)
			if seen[key] {
				break
			}
			seen[key] = true
			l.kids[parent] = append(l.kids[parent], path.Base(child))
		}
	}
	return nil
}

// layerSteps maps each layer to the build step that made it.
func layerSteps(img Image) []string {
	steps := make([]string, len(img.Layers))
	i := 0
	for _, h := range img.History {
		if h.EmptyLayer {
			continue
		}
		if i < len(steps) {
			steps[i] = h.CreatedBy
		}
		i++
	}
	return steps
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// FS is a view of the layer's own files, as its tar has them.
func (l *Layer) FS() *LayerFS { return &LayerFS{l: l} }

// LayerFS browses one layer. Symlinks are resolved within the layer.
type LayerFS struct{ l *Layer }

func (f *LayerFS) Lstat(p string) (Entry, error) {
	p = clean(p)
	if e, ok := f.l.entries[p]; ok {
		return e, nil
	}
	if p == "/" || len(f.l.kids[p]) > 0 {
		return Entry{Name: path.Base(p), Path: p, Mode: fs.ModeDir | 0755}, nil
	}
	return Entry{}, fmt.Errorf("%s: not in layer %d", p, f.l.Index)
}

func (f *LayerFS) Resolve(p string) (string, error) { return resolve(f.Lstat, p) }

func (f *LayerFS) Stat(p string) (Entry, error) {
	r, err := f.Resolve(p)
	if err != nil {
		return Entry{}, err
	}
	return f.Lstat(r)
}

func (f *LayerFS) ReadDir(p string) ([]Entry, error) {
	r, err := f.Resolve(p)
	if err != nil {
		return nil, err
	}
	dir, err := f.Lstat(r)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", clean(p))
	}
	var out []Entry
	for _, name := range f.l.kids[r] {
		e, err := f.Lstat(path.Join(r, name))
		if err == nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Open streams the file from the layer's tar (hard links read their
// target's content).
func (f *LayerFS) Open(p string) (io.ReadCloser, Entry, error) {
	e, err := f.Stat(p)
	if err != nil {
		return nil, Entry{}, err
	}
	if e.IsDir() {
		return nil, Entry{}, fmt.Errorf("%s: is a directory", clean(p))
	}
	if !e.Mode.IsRegular() {
		return nil, Entry{}, fmt.Errorf("%s: not a regular file", clean(p))
	}
	want := e.Path
	if target, ok := f.l.links[want]; ok {
		want = target
	}
	rc, err := openBlob(f.l.blob, nil)
	if err != nil {
		return nil, Entry{}, err
	}
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err != nil {
			rc.Close()
			return nil, Entry{}, fmt.Errorf("%s: not found in the layer's data", e.Path)
		}
		if clean(hdr.Name) == want && hdr.Typeflag == tar.TypeReg {
			return readCloser{tr, rc.Close}, e, nil
		}
	}
}

// resolve follows symlinks in every component of p using lstat, keeping
// ".." at the root and starting absolute targets from it.
func resolve(lstat func(string) (Entry, error), p string) (string, error) {
	todo := strings.Split(strings.TrimPrefix(clean(p), "/"), "/")
	var done []string
	hops := 0
	for len(todo) > 0 {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(done) > 0 {
				done = done[:len(done)-1]
			}
			continue
		}
		cur := "/" + path.Join(append(done, c)...)
		e, err := lstat(cur)
		if err != nil {
			return "", fmt.Errorf("%s: no such file or directory", clean(p))
		}
		if !e.IsSymlink() {
			if len(todo) > 0 && !e.IsDir() {
				return "", fmt.Errorf("%s: not a directory", cur)
			}
			done = append(done, c)
			continue
		}
		if hops++; hops > maxSymlinks {
			return "", fmt.Errorf("%s: too many levels of symbolic links", clean(p))
		}
		if strings.HasPrefix(e.Target, "/") {
			done = nil
		}
		todo = append(strings.Split(e.Target, "/"), todo...)
	}
	return "/" + path.Join(done...), nil
}

// MissingLayers reports whether some of img's layers aren't cached (or,
// for images pulled before the cache, aren't even recorded).
func (s *Store) MissingLayers(img Image) bool {
	if len(img.Layers) == 0 {
		return true
	}
	for _, d := range img.Layers {
		h, err := v1.NewHash(d)
		if err != nil {
			return true
		}
		if _, err := os.Stat(s.blobPath(h)); err != nil {
			return true
		}
	}
	return false
}

// EnsureLayers downloads img's missing layers into the cache (and records
// its layers, sizes and history if it was pulled before monolith did),
// without converting anything again. The registry must still serve the
// same image for its first reference.
func (s *Store) EnsureLayers(ctx context.Context, img Image, opts PullOptions) (Image, error) {
	if !s.MissingLayers(img) {
		return img, nil
	}
	if len(img.Refs) == 0 {
		return img, errors.New("the image has no reference to fetch its layers from; pull it again")
	}
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}
	r, err := name.ParseReference(img.Refs[0])
	if err != nil {
		return img, err
	}
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	if osArch := strings.SplitN(img.Platform, "/", 2); len(osArch) == 2 {
		platform = v1.Platform{OS: osArch[0], Architecture: osArch[1]}
	}
	remoteImg, err := remote.Image(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(platform))
	if err != nil {
		return img, fmt.Errorf("fetching layers of %s: %w", Display(r.Name()), err)
	}
	digest, err := remoteImg.ConfigName()
	if err != nil {
		return img, err
	}
	if digest.Hex != img.ID {
		return img, fmt.Errorf("%s has changed in the registry since it was pulled; pull it again", Display(r.Name()))
	}
	layers, err := remoteImg.Layers()
	if err != nil {
		return img, err
	}
	ids := make([]string, len(layers))
	digests := make([]string, len(layers))
	sizes := make([]int64, len(layers))
	for i, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return img, err
		}
		ids[i], digests[i] = d.Hex[:12], d.String()
		sizes[i], _ = l.Size()
	}
	fmt.Fprintf(logw, "%s\n", paint(useColor(opts.TTY), styleBold, "Fetching the layers of "+Display(r.Name())))
	view := newPullView(logw, opts.TTY, ids)
	err = s.download(ctx, layers, view)
	view.close()
	if err != nil {
		return img, err
	}

	unlock, err := s.lock()
	if err != nil {
		return img, err
	}
	defer unlock()
	img.Layers, img.LayerSizes = digests, sizes
	if len(img.History) == 0 {
		if cf, err := remoteImg.ConfigFile(); err == nil {
			img.History, img.Built = cf.History, cf.Created.Time
		}
	}
	return img, writeJSON(s.imagePath(img.ID), img)
}

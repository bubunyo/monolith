// Package oci keeps container images for `monolith container`: it pulls
// them from registries and converts each into an ext4 disk image that a
// container VM boots as its root filesystem.
//
// Layout under ~/.cache/monolith/images/:
//
//	refs.json             image references -> image ID
//	<id>/rootfs.img       the image's filesystem (sparse ext4)
//	<id>/image.json       config (entrypoint, env, ...), references, sizes
//
// An image ID is the hex digest of the image's config, like Docker's.
package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bubunyo/monolith"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ErrNotFound is returned when no local image matches a reference.
var ErrNotFound = errors.New("image not found")

const (
	refsFile  = "refs.json"
	imageFile = "image.json"
	diskFile  = "rootfs.img"
	lockFile  = ".lock"
)

// Image is a converted image in the store.
type Image struct {
	ID        string    `json:"id"`         // config digest, hex
	Refs      []string  `json:"refs"`       // normalized references pointing at it
	Config    v1.Config `json:"config"`     // entrypoint, cmd, env, user, workdir, ...
	Platform  string    `json:"platform"`   // e.g. "linux/arm64"
	Created   time.Time `json:"created"`    // when it was pulled
	Size      int64     `json:"size"`       // compressed layer bytes, as registries report it
	DiskBytes int64     `json:"disk_bytes"` // space rootfs.img takes on the Mac
}

// ShortID is the first 12 hex digits of the ID.
func (i Image) ShortID() string {
	if len(i.ID) > 12 {
		return i.ID[:12]
	}
	return i.ID
}

// Store is the local image store.
type Store struct {
	dir string
}

// Open returns the store at ~/.cache/monolith/images.
func Open() (*Store, error) {
	dir, err := monolith.CachePath("images")
	if err != nil {
		return nil, err
	}
	return OpenAt(dir)
}

// OpenAt returns a store rooted at dir (tests use a temporary directory).
func OpenAt(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// DiskPath is the root filesystem image of image id.
func (s *Store) DiskPath(id string) string {
	return filepath.Join(s.dir, id, diskFile)
}

// FilesystemSize is the size of image id's filesystem: the smallest disk
// a container of it can have.
func (s *Store) FilesystemSize(id string) (int64, error) {
	fi, err := os.Stat(s.DiskPath(id))
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Normalize turns a user's reference ("alpine", "nginx:1.29",
// "ghcr.io/o/r@sha256:...") into its full form
// ("index.docker.io/library/alpine:latest").
func Normalize(ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	return r.Name(), nil
}

// Display shortens a normalized reference the way Docker shows it:
// "index.docker.io/library/alpine:3.21" becomes "alpine:3.21".
func Display(ref string) string {
	ref = strings.TrimPrefix(ref, "index.docker.io/")
	return strings.TrimPrefix(ref, "library/")
}

// Lookup finds a local image by reference, full ID or unique ID prefix.
func (s *Store) Lookup(ref string) (Image, error) {
	refs, err := s.readRefs()
	if err != nil {
		return Image{}, err
	}
	if norm, err := Normalize(ref); err == nil {
		if id, ok := refs[norm]; ok {
			return s.read(id)
		}
	}
	id := strings.TrimPrefix(ref, "sha256:")
	if len(id) >= 4 && isHex(id) {
		images, err := s.List()
		if err != nil {
			return Image{}, err
		}
		var match []Image
		for _, img := range images {
			if strings.HasPrefix(img.ID, id) {
				match = append(match, img)
			}
		}
		if len(match) == 1 {
			return match[0], nil
		}
		if len(match) > 1 {
			return Image{}, fmt.Errorf("image ID prefix %q is ambiguous", ref)
		}
	}
	return Image{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
}

// List returns all images, newest first.
func (s *Store) List() ([]Image, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var images []Image
	for _, e := range entries {
		if !e.IsDir() || !isHex(e.Name()) {
			continue
		}
		img, err := s.read(e.Name())
		if err != nil {
			continue // a pull that never finished
		}
		images = append(images, img)
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Created.After(images[j].Created) })
	return images, nil
}

// PullOptions configures Pull.
type PullOptions struct {
	Platform v1.Platform // default linux/arm64
	Log      io.Writer   // progress lines; nil discards them
}

// Pull fetches ref from its registry and converts it, unless the same image
// is already in the store, in which case only the reference is updated.
// Credentials come from the Docker config (~/.docker/config.json and its
// credential helpers).
func (s *Store) Pull(ctx context.Context, ref string, opts PullOptions) (Image, error) {
	logw := opts.Log
	if logw == nil {
		logw = io.Discard
	}
	if opts.Platform.OS == "" {
		opts.Platform = v1.Platform{OS: "linux", Architecture: "arm64"}
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return Image{}, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	remoteImg, err := remote.Image(r,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
		remote.WithPlatform(opts.Platform))
	if err != nil {
		return Image{}, fmt.Errorf("pulling %s: %w", Display(r.Name()), err)
	}
	digest, err := remoteImg.ConfigName()
	if err != nil {
		return Image{}, err
	}
	id := digest.Hex

	unlock, err := s.lock()
	if err != nil {
		return Image{}, err
	}
	defer unlock()

	if img, err := s.read(id); err == nil {
		fmt.Fprintf(logw, "%s is up to date (%s)\n", Display(r.Name()), img.ShortID())
		return s.addRef(img, r.Name())
	}

	cf, err := remoteImg.ConfigFile()
	if err != nil {
		return Image{}, err
	}
	if cf.OS != "" && (cf.OS != opts.Platform.OS || cf.Architecture != opts.Platform.Architecture) {
		return Image{}, fmt.Errorf("%s has no %s/%s variant (got %s/%s)", Display(r.Name()), opts.Platform.OS, opts.Platform.Architecture, cf.OS, cf.Architecture)
	}
	layers, err := remoteImg.Layers()
	if err != nil {
		return Image{}, err
	}
	var size int64
	for _, l := range layers {
		if n, err := l.Size(); err == nil {
			size += n
		}
	}
	fmt.Fprintf(logw, "Pulling %s (%d layers, %s)...\n", Display(r.Name()), len(layers), HumanSize(size))

	// Convert into a temporary directory and rename, so an interrupted pull
	// never leaves a half-written image behind.
	tmp, err := os.MkdirTemp(s.dir, ".pull-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(tmp)
	start := time.Now()
	var stats ConvertStats
	for fsSize := EstimateDiskSize(size); ; fsSize *= 2 {
		stats, err = Convert(remoteImg, filepath.Join(tmp, diskFile), fsSize)
		if !errors.Is(err, errFull) || fsSize >= 1<<40 {
			break
		}
		fmt.Fprintf(logw, "%s doesn't fit in %s; retrying with %s\n", Display(r.Name()), HumanSize(fsSize), HumanSize(2*fsSize))
	}
	if err != nil {
		return Image{}, fmt.Errorf("converting %s: %w", Display(r.Name()), err)
	}
	img := Image{
		ID:        id,
		Config:    cf.Config,
		Platform:  opts.Platform.OS + "/" + opts.Platform.Architecture,
		Created:   time.Now(),
		Size:      size,
		DiskBytes: stats.DiskBytes,
	}
	if err := writeJSON(filepath.Join(tmp, imageFile), img); err != nil {
		return Image{}, err
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, id)); err != nil {
		return Image{}, err
	}
	fmt.Fprintf(logw, "Converted %s in %s (%s on disk)\n", Display(r.Name()), time.Since(start).Round(100*time.Millisecond), HumanSize(stats.DiskBytes))
	return s.addRef(img, r.Name())
}

// Remove deletes an image by reference or ID. Removing by reference only
// drops that reference while others remain; the image itself goes with its
// last reference, or at once when removed by ID.
func (s *Store) Remove(ref string) (removed Image, deleted bool, err error) {
	unlock, err := s.lock()
	if err != nil {
		return Image{}, false, err
	}
	defer unlock()
	img, err := s.Lookup(ref)
	if err != nil {
		return Image{}, false, err
	}
	refs, err := s.readRefs()
	if err != nil {
		return Image{}, false, err
	}
	byRef := false
	if norm, err := Normalize(ref); err == nil {
		if id, ok := refs[norm]; ok && id == img.ID {
			byRef = true
			delete(refs, norm)
			img.Refs = without(img.Refs, norm)
		}
	}
	if byRef && len(img.Refs) > 0 {
		if err := s.writeRefs(refs); err != nil {
			return Image{}, false, err
		}
		return img, false, writeJSON(filepath.Join(s.dir, img.ID, imageFile), img)
	}
	for r, id := range refs {
		if id == img.ID {
			delete(refs, r)
		}
	}
	if err := s.writeRefs(refs); err != nil {
		return Image{}, false, err
	}
	return img, true, os.RemoveAll(filepath.Join(s.dir, img.ID))
}

func (s *Store) addRef(img Image, ref string) (Image, error) {
	refs, err := s.readRefs()
	if err != nil {
		return Image{}, err
	}
	// A reference moves to the new image; drop it from the old one.
	if old, ok := refs[ref]; ok && old != img.ID {
		if prev, err := s.read(old); err == nil {
			prev.Refs = without(prev.Refs, ref)
			writeJSON(filepath.Join(s.dir, old, imageFile), prev)
		}
	}
	refs[ref] = img.ID
	if err := s.writeRefs(refs); err != nil {
		return Image{}, err
	}
	if !contains(img.Refs, ref) {
		img.Refs = append(img.Refs, ref)
		sort.Strings(img.Refs)
	}
	return img, writeJSON(filepath.Join(s.dir, img.ID, imageFile), img)
}

func (s *Store) read(id string) (Image, error) {
	var img Image
	data, err := os.ReadFile(filepath.Join(s.dir, id, imageFile))
	if err != nil {
		return img, err
	}
	if err := json.Unmarshal(data, &img); err != nil {
		return img, err
	}
	if _, err := os.Stat(s.DiskPath(id)); err != nil {
		return img, err
	}
	return img, nil
}

func (s *Store) readRefs() (map[string]string, error) {
	refs := map[string]string{}
	data, err := os.ReadFile(filepath.Join(s.dir, refsFile))
	if errors.Is(err, os.ErrNotExist) {
		return refs, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, fmt.Errorf("reading %s: %w", refsFile, err)
	}
	return refs, nil
}

func (s *Store) writeRefs(refs map[string]string) error {
	return writeJSON(filepath.Join(s.dir, refsFile), refs)
}

// lock serializes pulls and removals across monolith processes.
func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.dir, lockFile), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// writeJSON writes v atomically (temporary file and rename).
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// HumanSize formats bytes like Docker: "3.4MB", "1.2GB".
func HumanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func without(list []string, s string) []string {
	var out []string
	for _, v := range list {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

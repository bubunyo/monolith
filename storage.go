package monolith

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"gopkg.in/yaml.v3"
)

//go:embed embeddings
var embeddings embed.FS

// manifest represents the embeddings/manifest.yaml structure.
type manifest struct {
	Files []manifestEntry `yaml:"files"`
}

// manifestEntry describes a single file to install in the VM.
type manifestEntry struct {
	Src  string `yaml:"src"`
	Dst  string `yaml:"dst"`
	Mode string `yaml:"mode"`
	UID  int    `yaml:"uid,omitempty"`
	GID  int    `yaml:"gid,omitempty"`
}

// templateData is passed to Go templates for VM-specific substitution.
type templateData struct {
	Name string
}

// truncater is the subset of ext4.FileSystem we need for Truncate.
// The filesystem.FileSystem interface does not include Truncate, but the
// concrete ext4 implementation does.
type truncater interface {
	Truncate(p string, size int64) error
}

const defaultDiskSize = 1 << 30 // 1 GiB

// DefaultDisk returns the path to the VM's disk image, creating and
// populating it with the Alpine minirootfs on first use.
// Each VM gets its own disk under ~/.cache/monlith/vms/<name>/disk.img.
func DefaultDisk(rootfsTarball, modloopPath, name string) (string, error) {
	cache, err := cacheDir()
	if err != nil {
		return "", err
	}

	vmDir := filepath.Join(cache, "vms", name)
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return "", fmt.Errorf("creating VM directory: %w", err)
	}

	diskPath := filepath.Join(vmDir, "disk.img")
	if exists(diskPath) {
		return diskPath, nil
	}

	if err := createDiskImage(diskPath, defaultDiskSize, rootfsTarball, modloopPath, name); err != nil {
		os.Remove(diskPath) // clean up partial image on failure
		return "", err
	}
	return diskPath, nil
}

func createDiskImage(path string, size int64, rootfsTarball, modloopPath, name string) error {
	// Create a sparse file for the disk image.
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating disk file: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return fmt.Errorf("truncating disk file: %w", err)
	}
	f.Close()

	// Open as a go-diskfs backend so ext4.Create can use it.
	backend, err := file.OpenFromPath(path, false)
	if err != nil {
		return fmt.Errorf("opening disk backend: %w", err)
	}
	defer backend.Close()

	// Call ext4.Create directly (bypassing disk.CreateFilesystem) so we can
	// set SectorsPerBlock = 8 → 4096-byte blocks. The default
	// recalculateBlocksize in go-diskfs always picks 1024-byte blocks, which
	// causes the journal allocator to fragment beyond 4 extents and fail.
	ext4fs, err := ext4.Create(backend, size, 0, 512, &ext4.Params{
		SectorsPerBlock: 8, // 8 × 512 = 4096-byte blocks
		VolumeName:      "rootfs",
	})
	if err != nil {
		return fmt.Errorf("creating ext4 filesystem: %w", err)
	}

	if err := populateRootFS(ext4fs, rootfsTarball, modloopPath, name); err != nil {
		return fmt.Errorf("populating rootfs: %w", err)
	}

	return nil
}

// populateRootFS extracts an Alpine minirootfs tarball into the ext4
// filesystem and writes a custom /etc/inittab for hvc0 console access.
func populateRootFS(ext4fs filesystem.FileSystem, tarball, modloopPath, name string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("opening gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	// Track hardlink targets so we can copy their content for subsequent
	// hardlinks (go-diskfs ext4 does not support Link).
	hardlinkContent := make(map[string][]byte)

	// Track directories we've already created so we don't Mkdir twice
	// (Mkdir is idempotent but this saves syscalls).
	createdDirs := make(map[string]bool)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}

		// go-diskfs ext4 paths must not have leading "/" or "./"
		name := cleanTarPath(hdr.Name)
		if name == "." || name == "" {
			continue
		}

		// Ensure parent directory exists
		if dir := filepath.Dir(name); dir != "." && !createdDirs[dir] {
			if err := ext4fs.Mkdir(dir); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
			createdDirs[dir] = true
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if !createdDirs[name] {
				if err := ext4fs.Mkdir(name); err != nil {
					return fmt.Errorf("mkdir %s: %w", name, err)
				}
				createdDirs[name] = true
			}
			if err := ext4fs.Chmod(name, hdr.FileInfo().Mode()); err != nil {
				return fmt.Errorf("chmod %s: %w", name, err)
			}
			if err := ext4fs.Chown(name, hdr.Uid, hdr.Gid); err != nil {
				return fmt.Errorf("chown %s: %w", name, err)
			}

		case tar.TypeReg, tar.TypeRegA:
			data, err := writeFile(ext4fs, name, tr, hdr)
			if err != nil {
				return err
			}
			// Cache content for potential hardlinks pointing to this file.
			hardlinkContent["/"+name] = data

		case tar.TypeSymlink:
			if err := ext4fs.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", name, hdr.Linkname, err)
			}

		case tar.TypeLink:
			// go-diskfs ext4 doesn't support hardlinks. Create a copy of the
			// target file's content instead.
			target := hdr.Linkname
			if !strings.HasPrefix(target, "/") {
				target = "/" + target
			}
			content, ok := hardlinkContent[target]
			if !ok {
				// Target not seen yet — read zero bytes (empty file).
				// This can happen if tar ordering is unusual.
				content = nil
			}
			if _, err := writeFileBytes(ext4fs, name, content, hdr); err != nil {
				return err
			}

		case tar.TypeBlock, tar.TypeChar, tar.TypeFifo:
			// Skip device nodes and FIFOs — the kernel creates /dev entries.
			continue

		default:
			// Skip unknown types
			continue
		}
	}

	// Copy the modloop squashfs into the rootfs so the kernel modules
	// (including af_packet for networking) are available after boot.
	if err := installModloop(ext4fs, modloopPath); err != nil {
		return fmt.Errorf("installing modloop: %w", err)
	}

	// Set SUID bit on /bin/busybox so that busybox applets like su can
	// switch users. Without this, "su" prints "must be suid to work properly".
	if err := ext4fs.Chmod("bin/busybox", 0755|os.ModeSetuid); err != nil {
		return fmt.Errorf("chmod suid busybox: %w", err)
	}

	// Apply VM-specific configuration: custom inittab, user accounts,
	// autologin, and sudo support.
	if err := configureVM(ext4fs, name); err != nil {
		return err
	}

	return nil
}

// writeFile creates a file in the ext4 filesystem from a tar reader.
// Returns the file content (for hardlink caching).
func writeFile(ext4fs filesystem.FileSystem, name string, r io.Reader, hdr *tar.Header) ([]byte, error) {
	ef, err := ext4fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		ef.Close()
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}

	if _, err := ef.Write(data); err != nil {
		ef.Close()
		return nil, fmt.Errorf("writing %s: %w", name, err)
	}
	ef.Close()

	if err := ext4fs.Chmod(name, hdr.FileInfo().Mode()); err != nil {
		return data, fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := ext4fs.Chown(name, hdr.Uid, hdr.Gid); err != nil {
		return data, fmt.Errorf("chown %s: %w", name, err)
	}

	return data, nil
}

// writeFileBytes creates a file in the ext4 filesystem from a byte slice.
func writeFileBytes(ext4fs filesystem.FileSystem, name string, data []byte, hdr *tar.Header) ([]byte, error) {
	ef, err := ext4fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}

	if len(data) > 0 {
		if _, err := ef.Write(data); err != nil {
			ef.Close()
			return nil, fmt.Errorf("writing %s: %w", name, err)
		}
	}
	ef.Close()

	if err := ext4fs.Chmod(name, hdr.FileInfo().Mode()); err != nil {
		return data, fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := ext4fs.Chown(name, hdr.Uid, hdr.Gid); err != nil {
		return data, fmt.Errorf("chown %s: %w", name, err)
	}

	return data, nil
}

// installModloop copies the modloop squashfs image into the rootfs so that
// kernel modules (notably af_packet, needed for DHCP) are available at boot.
// The squashfs is stored at /lib/modloop-virt and mounted by inittab at
// /lib/modules during sysinit.
func installModloop(ext4fs filesystem.FileSystem, modloopPath string) error {
	data, err := os.ReadFile(modloopPath)
	if err != nil {
		return fmt.Errorf("reading modloop: %w", err)
	}

	if err := ext4fs.Mkdir("lib"); err != nil {
		return fmt.Errorf("mkdir lib: %w", err)
	}

	destPath := "lib/modloop-virt"
	ef, err := ext4fs.OpenFile(destPath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	if _, err := ef.Write(data); err != nil {
		ef.Close()
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	ef.Close()

	if err := ext4fs.Chmod(destPath, 0644); err != nil {
		return fmt.Errorf("chmod %s: %w", destPath, err)
	}

	return nil
}

// configureVM installs all VM-specific configuration files into the ext4
// filesystem using embedded assets from embeddings/ and a YAML manifest.
// The name parameter is used as the VM's hostname and appears in templates.
func configureVM(ext4fs filesystem.FileSystem, name string) error {
	const monoUID = 1000
	const monoGID = 1000

	// We need Truncate to overwrite files without calling Remove (which has
	// a go-diskfs bug that corrupts the block group descriptor free count).
	trunc, ok := ext4fs.(truncater)
	if !ok {
		return fmt.Errorf("filesystem does not support Truncate")
	}

	// Parse the manifest.
	manifestData, err := embeddings.ReadFile("embeddings/manifest.yaml")
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	var m manifest
	if err := yaml.Unmarshal(manifestData, &m); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	data := templateData{Name: name}

	for _, entry := range m.Files {
		// Read source from embedded FS.
		src, err := embeddings.ReadFile("embeddings/" + entry.Src)
		if err != nil {
			return fmt.Errorf("reading embedded %s: %w", entry.Src, err)
		}

		// If the source is a template, execute it.
		var content []byte
		if strings.HasSuffix(entry.Src, ".tmpl") {
			tmpl, err := template.New(entry.Src).Parse(string(src))
			if err != nil {
				return fmt.Errorf("parsing template %s: %w", entry.Src, err)
			}
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				return fmt.Errorf("executing template %s: %w", entry.Src, err)
			}
			content = buf.Bytes()
		} else {
			content = src
		}

		// Parse octal mode string.
		mode64, err := strconv.ParseUint(entry.Mode, 8, 32)
		if err != nil {
			return fmt.Errorf("parsing mode %q for %s: %w", entry.Mode, entry.Dst, err)
		}
		mode := os.FileMode(mode64)

		// Ensure parent directory exists.
		if dir := filepath.Dir(entry.Dst); dir != "." {
			if err := ext4fs.Mkdir(dir); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}

		// Overwrite the file in place. We avoid Remove() because go-diskfs
		// ext4 Remove has a bug that double-counts freed blocks in the block
		// group descriptor, causing "block bitmap and bg descriptor
		// inconsistent" errors at mount time. Instead we open, write, then
		// truncate the inode size to the new length (handles the case where
		// the new content is shorter than the existing file).
		ef, err := ext4fs.OpenFile(entry.Dst, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return fmt.Errorf("create %s: %w", entry.Dst, err)
		}
		n, err := ef.Write(content)
		if err != nil {
			ef.Close()
			return fmt.Errorf("write %s: %w", entry.Dst, err)
		}
		ef.Close()
		if err := trunc.Truncate(entry.Dst, int64(n)); err != nil {
			return fmt.Errorf("truncate %s: %w", entry.Dst, err)
		}

		if err := ext4fs.Chmod(entry.Dst, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", entry.Dst, err)
		}
		if entry.UID != 0 || entry.GID != 0 {
			if err := ext4fs.Chown(entry.Dst, entry.UID, entry.GID); err != nil {
				return fmt.Errorf("chown %s: %w", entry.Dst, err)
			}
		}
	}

	// .profile stays inline — embed.FS ignores dotfiles by default.
	profilePath := "home/mono/.profile"
	profileData := fmt.Sprintf(`[ -f /etc/motd ] && cat /etc/motd
export HOME=/home/mono
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PS1='mono@%s:\w\$ '
`, name)

	if dir := filepath.Dir(profilePath); dir != "." {
		if err := ext4fs.Mkdir(dir); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	// Overwrite .profile in place (no Remove — see go-diskfs bug above).
	ef, err := ext4fs.OpenFile(profilePath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("create %s: %w", profilePath, err)
	}
	n, err := ef.Write([]byte(profileData))
	if err != nil {
		ef.Close()
		return fmt.Errorf("write %s: %w", profilePath, err)
	}
	ef.Close()
	if err := trunc.Truncate(profilePath, int64(n)); err != nil {
		return fmt.Errorf("truncate %s: %w", profilePath, err)
	}
	if err := ext4fs.Chmod(profilePath, 0644); err != nil {
		return fmt.Errorf("chmod %s: %w", profilePath, err)
	}
	if err := ext4fs.Chown(profilePath, monoUID, monoGID); err != nil {
		return fmt.Errorf("chown %s: %w", profilePath, err)
	}

	// Ensure /home/mono is owned by mono.
	if err := ext4fs.Chown("home/mono", monoUID, monoGID); err != nil {
		return fmt.Errorf("chown home/mono: %w", err)
	}

	return nil
}

// cleanTarPath normalizes a tar entry name for go-diskfs ext4 paths
// (no leading "/" or "./", no trailing "/").
func cleanTarPath(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "."
	}
	return name
}

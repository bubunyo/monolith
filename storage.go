package monolith

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

const defaultDiskSize = 1 << 30 // 1 GiB

// DefaultDisk returns the path to the cached disk image, creating and
// populating it with the Alpine minirootfs on first use.
func DefaultDisk(rootfsTarball string) (string, error) {
	cache, err := cacheDir()
	if err != nil {
		return "", err
	}

	diskPath := filepath.Join(cache, "disk.img")
	if exists(diskPath) {
		return diskPath, nil
	}

	if err := createDiskImage(diskPath, defaultDiskSize, rootfsTarball); err != nil {
		os.Remove(diskPath) // clean up partial image on failure
		return "", err
	}
	return diskPath, nil
}

func createDiskImage(path string, size int64, rootfsTarball string) error {
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

	if err := populateRootFS(ext4fs, rootfsTarball); err != nil {
		return fmt.Errorf("populating rootfs: %w", err)
	}

	return nil
}

// populateRootFS extracts an Alpine minirootfs tarball into the ext4
// filesystem and writes a custom /etc/inittab for hvc0 console access.
func populateRootFS(ext4fs filesystem.FileSystem, tarball string) error {
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

	// Apply VM-specific configuration: custom inittab, user accounts,
	// autologin, and sudo support.
	if err := configureVM(ext4fs); err != nil {
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

// configureVM writes all VM-specific configuration files into the ext4
// filesystem: inittab, user accounts, autologin, and sudo wrapper.
func configureVM(ext4fs filesystem.FileSystem) error {
	type configFile struct {
		path string
		data string
		mode os.FileMode
		uid  int
		gid  int
	}

	const monoUID = 1000
	const monoGID = 1000

	files := []configFile{
		{
			path: "etc/inittab",
			mode: 0644,
			data: `# /etc/inittab — busybox init for monolith VM
# Alpine initramfs already mounts proc, sysfs, devtmpfs and root (ro).
::sysinit:/bin/mount -o remount,rw /
::sysinit:/bin/mkdir -p /dev/pts /dev/shm /run /tmp
::sysinit:/bin/mount -t devpts devpts /dev/pts
::sysinit:/bin/mount -t tmpfs tmpfs /dev/shm
::sysinit:/bin/mount -t tmpfs tmpfs /run
::sysinit:/bin/mount -t tmpfs tmpfs /tmp
::sysinit:/sbin/mdev -s
::sysinit:/bin/hostname monolith
::sysinit:/sbin/ifconfig lo 127.0.0.1 up
::sysinit:/sbin/ifconfig eth0 up
::sysinit:/sbin/udhcpc -i eth0 -q -n -f

# Auto-login as mono (no login prompt)
hvc0::respawn:/sbin/getty -n -l /usr/local/bin/autologin 0 hvc0

::ctrlaltdel:/sbin/reboot
::shutdown:/bin/umount -a -r
`,
		},
		{
			// Overwrite /etc/passwd: add mono user (uid 1000, gid 1000).
			path: "etc/passwd",
			mode: 0644,
			data: `root:x:0:0:root:/root:/bin/sh
bin:x:1:1:bin:/bin:/sbin/nologin
daemon:x:2:2:daemon:/sbin:/sbin/nologin
lp:x:4:7:lp:/var/spool/lpd:/sbin/nologin
sync:x:5:0:sync:/sbin:/bin/sync
shutdown:x:6:0:shutdown:/sbin:/sbin/shutdown
halt:x:7:0:halt:/sbin:/sbin/halt
mail:x:8:12:mail:/var/mail:/sbin/nologin
news:x:9:13:news:/usr/lib/news:/sbin/nologin
uucp:x:10:14:uucp:/var/spool/uucppublic:/sbin/nologin
cron:x:16:16:cron:/var/spool/cron:/sbin/nologin
ftp:x:21:21::/var/lib/ftp:/sbin/nologin
sshd:x:22:22:sshd:/dev/null:/sbin/nologin
games:x:35:35:games:/usr/games:/sbin/nologin
ntp:x:123:123:NTP:/var/empty:/sbin/nologin
guest:x:405:100:guest:/dev/null:/sbin/nologin
nobody:x:65534:65534:nobody:/:/sbin/nologin
mono:x:1000:1000::/home/mono:/bin/sh
`,
		},
		{
			// Overwrite /etc/shadow: root has empty password (for su),
			// mono has empty password.
			path: "etc/shadow",
			mode: 0600,
			data: `root:::0:::::
bin:!::0:::::
daemon:!::0:::::
lp:!::0:::::
sync:!::0:::::
shutdown:!::0:::::
halt:!::0:::::
mail:!::0:::::
news:!::0:::::
uucp:!::0:::::
cron:!::0:::::
ftp:!::0:::::
sshd:!::0:::::
games:!::0:::::
ntp:!::0:::::
guest:!::0:::::
nobody:!::0:::::
mono:::0:::::
`,
		},
		{
			// Overwrite /etc/group: add mono group and put mono in wheel.
			path: "etc/group",
			mode: 0644,
			data: `root:x:0:root
bin:x:1:root,bin,daemon
daemon:x:2:root,bin,daemon
sys:x:3:root,bin
adm:x:4:root,daemon
tty:x:5:
disk:x:6:root
lp:x:7:lp
mem:x:8:
kmem:x:9:
wheel:x:10:root,mono
floppy:x:11:root
mail:x:12:mail
news:x:13:news
uucp:x:14:uucp
cron:x:16:cron
ftp:x:21:
sshd:x:22:
games:x:35:
ntp:x:123:
guest:x:100:
nobody:x:65534:
mono:x:1000:
`,
		},
		{
			// Autologin script: getty -n -l calls this instead of /bin/login.
			// It execs a login shell as mono.
			path: "usr/local/bin/autologin",
			mode: 0755,
			data: `#!/bin/sh
exec /bin/su -l mono
`,
		},
		{
			// sudo wrapper: runs commands as root via su.
			// Supports "sudo cmd args..." and "sudo -i" for root shell.
			path: "usr/local/bin/sudo",
			mode: 0755,
			data: `#!/bin/sh
if [ "$1" = "-i" ]; then
    exec /bin/su -l root
fi
exec /bin/su -c "$*" root
`,
		},
		{
			// mono's .profile
			path: "home/mono/.profile",
			mode: 0644, uid: monoUID, gid: monoGID,
			data: `export HOME=/home/mono
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PS1='mono@monolith:\w\$ '
`,
		},
	}

	for _, cf := range files {
		// Ensure parent directory exists.
		if dir := filepath.Dir(cf.path); dir != "." {
			if err := ext4fs.Mkdir(dir); err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
		}

		ef, err := ext4fs.OpenFile(cf.path, os.O_CREATE|os.O_RDWR)
		if err != nil {
			return fmt.Errorf("create %s: %w", cf.path, err)
		}
		if _, err := ef.Write([]byte(cf.data)); err != nil {
			ef.Close()
			return fmt.Errorf("write %s: %w", cf.path, err)
		}
		ef.Close()

		if err := ext4fs.Chmod(cf.path, cf.mode); err != nil {
			return fmt.Errorf("chmod %s: %w", cf.path, err)
		}
		if cf.uid != 0 || cf.gid != 0 {
			if err := ext4fs.Chown(cf.path, cf.uid, cf.gid); err != nil {
				return fmt.Errorf("chown %s: %w", cf.path, err)
			}
		}
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

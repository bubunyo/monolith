# Turning OCI images into ext4 disks on a Mac

Draft for a follow-up to [One VM per container](one-vm-per-container.md). Links
are pinned to the same commit.

A container starts as a directory holding its config, its logs and its disk.
[The disk is an APFS clone](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/container.go#L132-L139)
of the image's ext4 disk, so creating one is instant, and it only takes space
for what the container writes.

## Turning images into disks

A container image is a stack of tar files, one per layer, each adding,
changing or deleting files on top of the ones below. A VM can't boot from
that; it wants a block device with a filesystem on it. So when monolith pulls
an image, it
[flattens the layers into an ext4 disk image](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/convert.go#L60-L72),
once per image. Doing that on a Mac, without a Linux machine to run
`mkfs.ext4` on, means writing ext4 from Go, with
[go-diskfs](https://github.com/diskfs/go-diskfs).

go-diskfs made this possible, and it also gave me most of my bugs:

- **No hard links, device nodes or extended attributes.** Hard links become
  copies, device nodes are skipped (the init's `devtmpfs` provides them
  anyway), and extended attributes are dropped. That last one loses file
  capabilities, which some images rely on.
- **Writing a directory resets what was written before.** Adding a file to a
  directory reset the mode and owner of inodes written earlier. Files came out
  owned by root with the wrong permissions. The fix was to write all the
  contents first, then
  [apply every file's metadata in a final pass](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/convert.go#L204-L216).
- **Empty space that isn't empty.** go-diskfs's mkfs writes out every inode
  table in full, zeros and all, so a small image took far more space than its
  files. Afterwards,
  [every run of zeros is punched out](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/convert.go#L258-L260)
  of the file, leaving holes that read as zeros and take no space.
- **A filesystem the kernel refused to grow.** An image's filesystem is sized
  to its contents, and a container's disk is bigger, so the init
  [grows the filesystem at boot](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/grow_linux.go#L14-L17),
  with the filesystem mounted. The kernel refused. The blocks go-diskfs
  reserves for future growth (the `resize_inode` feature) were in the wrong
  place. The fix was to
  [leave them out](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/convert.go#L93):
  without them, the kernel grows the filesystem a different way (`meta_bg`),
  and that works.

The result is cheap to run. The image's disk is created once, and each
container gets an APFS clone of it, extended to its `--disk` size (8 GB by
default). For a running nginx, the numbers look like this:

| | Size |
|---|---|
| nginx image, compressed layers | 62 MB |
| nginx image, as an ext4 disk | 201 MB |
| a container's disk, as `ls` reports it | 8 GB |
| a container's disk, space it actually uses | 1.3 MB |

`du` can't tell you that last number, since it counts the blocks a clone
shares with the original. monolith asks APFS directly, with
[`getattrlist`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/privatesize.go#L10-L13),
which is how `prune` knows how much space it will give back.

## Making it feel like Docker

A container engine you don't trust to feel familiar is one you don't use. A
couple of things went a long way.

**Pull progress.** The first version of `pull` was a long silence. Now it
looks like Docker's: a line per layer, updated in place, with
[eight downloads at a time](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/layers.go#L37-L47).
Layers are kept in a cache shared by all images, so pulling a second image on
the same base says
[`Already exists (alpine:latest)`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/layers.go#L55-L60)
and skips the download.

**Looking inside an image without running it.** Some images have no shell at
all, and sometimes you just want to see what's in one before trusting it.
`monolith container image sh nginx` opens a small shell on the Mac, over the
image's files, with `ls`, `cat` and `find`. It can also
[take the image apart by layer](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/layerindex.go#L121-L123):
`layers` lists them with the Dockerfile step that made each one, and
`blame /etc/passwd` shows which layers touched that file. Nothing from the
image runs, and no VM starts.


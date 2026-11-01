package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// growRoot grows the mounted root filesystem online to fill /dev/vda: an
// image's filesystem is sized to its contents, and a container's disk (its
// clone) is made as big as --disk.
func growRoot(mountpoint string) error {
	raw, err := os.ReadFile(mountpoint + "/sys/block/vda/size")
	if err != nil {
		return err
	}
	sectors, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return err
	}
	fsBlocks, blockSize, err := ext4Size("/dev/vda")
	if err != nil {
		return err
	}
	want := sectors * 512 / blockSize
	if want <= fsBlocks {
		return nil
	}
	f, err := os.Open(mountpoint)
	if err != nil {
		return err
	}
	defer f.Close()
	const ext4IocResizeFs = 0x40086610 // _IOW('f', 16, __u64)
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), ext4IocResizeFs, uintptr(unsafe.Pointer(&want))); e != 0 {
		return fmt.Errorf("resizing from %d to %d blocks: %w", fsBlocks, want, e)
	}
	return nil
}

// ext4Size reads the block count and block size from an ext4 superblock.
func ext4Size(dev string) (blocks, blockSize uint64, err error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sb := make([]byte, 1024)
	if _, err := f.ReadAt(sb, 1024); err != nil {
		return 0, 0, err
	}
	if binary.LittleEndian.Uint16(sb[0x38:]) != 0xEF53 {
		return 0, 0, fmt.Errorf("%s is not ext4", dev)
	}
	blocks = uint64(binary.LittleEndian.Uint32(sb[0x04:]))
	if binary.LittleEndian.Uint32(sb[0x60:])&0x80 != 0 { // INCOMPAT_64BIT
		blocks |= uint64(binary.LittleEndian.Uint32(sb[0x150:])) << 32
	}
	blockSize = 1024 << binary.LittleEndian.Uint32(sb[0x18:])
	return blocks, blockSize, nil
}

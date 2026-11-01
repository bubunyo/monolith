package monolith

import (
	"encoding/binary"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PrivateSize is the space a file takes that isn't shared with APFS clones
// of it: for a container's disk, what the container wrote on top of its
// image (what removing it gives back). du and stat count shared blocks too.
func PrivateSize(path string) (int64, error) {
	const (
		attrBitMapCount       = 5
		attrCmnextPrivateSize = 0x00000008 // ATTR_CMNEXT_PRIVATESIZE (in forkattr with the option below)
		fsoptAttrCmnExtended  = 0x00000020 // FSOPT_ATTR_CMN_EXTENDED
	)
	attrs := struct {
		bitmapCount uint16
		reserved    uint16
		commonAttr  uint32
		volAttr     uint32
		dirAttr     uint32
		fileAttr    uint32
		forkAttr    uint32
	}{bitmapCount: attrBitMapCount, forkAttr: attrCmnextPrivateSize}
	// The reply is packed: a uint32 length, then the off_t at offset 4.
	var buf [12]byte
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	// No x/sys/unix wrapper exists for getattrlist.
	//nolint:staticcheck
	_, _, e := unix.Syscall6(unix.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), fsoptAttrCmnExtended, 0)
	if e != 0 {
		return 0, e
	}
	return int64(binary.LittleEndian.Uint64(buf[4:])), nil
}

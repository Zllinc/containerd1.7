//go:build linux

package lvm

import (
	"fmt"
	"math"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FS_IOC_FIEMAP ioctl number for linux/fiemap.h (_IOWR('f', 11, struct fiemap)).
// This value is stable for Linux amd64.
const fsIoctlFiemap = 0xC020660B

type fileExtent struct {
	Start  uint64
	Length uint64
}

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type fiemapExtent struct {
	Logical   uint64
	Physical  uint64
	Length    uint64
	Reserved1 uint64
	Reserved2 uint64
	Flags     uint32
	Reserved3 uint32
	Reserved4 uint32
	Reserved5 uint32
}

func fiemapExtents(path string) ([]fileExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	const extentCount = 256
	hdrSize := unsafe.Sizeof(fiemapHeader{})
	extentSize := unsafe.Sizeof(fiemapExtent{})
	bufSize := int(hdrSize + extentSize*extentCount)

	var (
		start   uint64
		extents []fileExtent
	)
	for {
		buf := make([]byte, bufSize)
		hdr := (*fiemapHeader)(unsafe.Pointer(&buf[0]))
		hdr.Start = start
		hdr.Length = math.MaxUint64
		hdr.ExtentCount = extentCount

		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIoctlFiemap, uintptr(unsafe.Pointer(&buf[0])))
		if errno != 0 {
			return nil, fmt.Errorf("fiemap ioctl failed for %s: %v", path, errno)
		}

		if hdr.MappedExtents == 0 {
			break
		}

		for i := 0; i < int(hdr.MappedExtents); i++ {
			extPtr := unsafe.Add(unsafe.Pointer(&buf[0]), int(hdrSize)+i*int(extentSize))
			ext := (*fiemapExtent)(extPtr)
			if ext.Length == 0 {
				continue
			}
			extents = append(extents, fileExtent{
				Start:  ext.Physical,
				Length: ext.Length,
			})
			next := ext.Logical + ext.Length
			if next > start {
				start = next
			}
		}

		if hdr.MappedExtents < extentCount {
			break
		}
	}

	return extents, nil
}


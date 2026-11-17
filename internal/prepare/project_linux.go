package prepare

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// fsxattr is struct fsxattr of <linux/fs.h>; Lustre implements the ioctls below for project IDs.
type fsxattr struct {
	xflags     uint32
	extsize    uint32
	nextents   uint32
	projid     uint32
	cowextsize uint32
	pad        [8]byte
}

const (
	fsIocFsGetXattr    = 0x801c581f // _IOR('X', 31, struct fsxattr)
	fsIocFsSetXattr    = 0x401c5820 // _IOW('X', 32, struct fsxattr)
	fsXflagProjInherit = 0x00000200
)

// ProjectOf reads a directory's project ID and whether new entries inherit it.
func ProjectOf(fd int) (uint32, bool, error) {
	var a fsxattr
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fsIocFsGetXattr, uintptr(unsafe.Pointer(&a))); e != 0 {
		return 0, false, e
	}
	return a.projid, a.xflags&fsXflagProjInherit != 0, nil
}

// setProject gives a directory a project ID inherited by everything created below it. The
// caller must own the directory.
func setProject(fd int, id uint32) error {
	var a fsxattr
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fsIocFsGetXattr, uintptr(unsafe.Pointer(&a))); e != 0 {
		return e
	}
	a.projid = id
	a.xflags |= fsXflagProjInherit
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fsIocFsSetXattr, uintptr(unsafe.Pointer(&a))); e != 0 {
		return e
	}
	return nil
}

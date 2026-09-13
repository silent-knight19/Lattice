//go:build linux

package engine

import (
	"os"
	"syscall"
	"unsafe"
)

// unlinkAt unlinks a directory entry anchored to an already opened and pinned
// directory file descriptor using the unlinkat(2) system call with flag 0 on Linux.
// Flag 0 guarantees that directories cannot be removed (preventing rmdir recursion races).
func unlinkAt(dirFile *os.File, name string) error {
	if dirFile == nil {
		return os.ErrInvalid
	}
	nameBytes, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}

	_, _, sysErr := syscall.Syscall(syscall.SYS_UNLINKAT, dirFile.Fd(), uintptr(unsafe.Pointer(nameBytes)), 0)
	if sysErr != 0 {
		return sysErr
	}
	return nil
}

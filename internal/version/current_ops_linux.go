//go:build linux

package version

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// createTempAt atomically creates and opens a temporary file within the pinned
// directory descriptor using openat(2) with O_WRONLY|O_CREAT|O_EXCL|O_NOFOLLOW.
func createTempAt(dirFile *os.File, name string, perm os.FileMode) (*os.File, error) {
	if dirFile == nil {
		return nil, os.ErrInvalid
	}
	nameBytes, err := syscall.BytePtrFromString(name)
	if err != nil {
		return nil, err
	}
	flags := syscall.O_WRONLY | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW
	r1, _, errno := syscall.Syscall6(syscall.SYS_OPENAT, dirFile.Fd(), uintptr(unsafe.Pointer(nameBytes)), uintptr(flags), uintptr(perm), 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return os.NewFile(r1, filepath.Join(dirFile.Name(), name)), nil
}

// renameAt atomically renames a file within the pinned directory descriptor using renameat(2).
// Both oldName and newName are resolved relative to the verified directory descriptor.
func renameAt(dirFile *os.File, oldName, newName string) error {
	if dirFile == nil {
		return os.ErrInvalid
	}
	oldBytes, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newBytes, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_RENAMEAT, dirFile.Fd(), uintptr(unsafe.Pointer(oldBytes)), dirFile.Fd(), uintptr(unsafe.Pointer(newBytes)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// removeAt unlinks a directory entry anchored to the pinned directory descriptor using unlinkat(2).
func removeAt(dirFile *os.File, name string) error {
	if dirFile == nil {
		return os.ErrInvalid
	}
	nameBytes, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, dirFile.Fd(), uintptr(unsafe.Pointer(nameBytes)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

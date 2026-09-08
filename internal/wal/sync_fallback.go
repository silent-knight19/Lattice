//go:build !linux

package wal

import "os"

// fdatasync flushes modified in-core data of f to stable storage.
//
// Platform Limitation Note:
// On non-Linux operating systems (such as Darwin/macOS and Windows), the POSIX
// fdatasync(2) system call is not provided by the operating system kernel.
// On these platforms, fdatasync falls back to f.Sync() (which invokes fsync(2)
// on Unix/Darwin or FlushFileBuffers on Windows).
// Consequently, on these platforms both data blocks and inode metadata are synchronized.
func fdatasync(f *os.File) error {
	return f.Sync()
}

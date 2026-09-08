//go:build linux

package wal

import (
	"os"
	"syscall"
)

// fdatasync flushes modified in-core data pages of f to stable storage media
// using the Linux fdatasync(2) system call.
//
// Unlike fsync, fdatasync flushes only modified data blocks and does not force
// synchronization of unchanged inode metadata (such as file access/modification timestamps),
// reducing physical disk I/O operations and latency.
func fdatasync(f *os.File) error {
	return syscall.Fdatasync(int(f.Fd()))
}

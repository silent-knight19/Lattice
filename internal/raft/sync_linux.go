//go:build linux

package raft

import (
	"os"
	"syscall"
)

// fdatasync flushes modified in-core data pages of f to stable storage media
// using the Linux fdatasync(2) system call.
func fdatasync(f *os.File) error {
	return syscall.Fdatasync(int(f.Fd()))
}

//go:build !linux

package raft

import "os"

// fdatasync flushes modified in-core data of f to stable storage.
// Falls back to f.Sync() on non-Linux platforms (macOS/Darwin, Windows).
func fdatasync(f *os.File) error {
	return f.Sync()
}

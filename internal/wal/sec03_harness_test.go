package wal_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// SecurityHarness provides isolated sandboxes, deterministic filesystem mutation helpers,
// symlink capability detection, and adversarial payload generators for SEC-03 dynamic auditing.
type SecurityHarness struct {
	t        *testing.T
	rootDir  string
	walDir   string
	symlinks bool
}

// NewSecurityHarness initializes an isolated test environment under t.TempDir().
// Guarantees that zero test operations escape into real system directories.
func NewSecurityHarness(t *testing.T) *SecurityHarness {
	t.Helper()
	root := t.TempDir()
	walDir := wal.Dir(root)

	h := &SecurityHarness{
		t:       t,
		rootDir: root,
		walDir:  walDir,
	}
	h.detectSymlinkSupport()
	return h
}

// RootDir returns the isolated database root directory.
func (h *SecurityHarness) RootDir() string {
	return h.rootDir
}

// WALDir returns the path to <root>/wal.
func (h *SecurityHarness) WALDir() string {
	return h.walDir
}

// SupportsSymlinks reports whether the host operating system and filesystem permit symlink creation.
func (h *SecurityHarness) SupportsSymlinks() bool {
	return h.symlinks
}

// RequireSymlinks skips the test if symlink creation is not permitted on this platform.
func (h *SecurityHarness) RequireSymlinks() {
	h.t.Helper()
	if !h.symlinks {
		h.t.Skip("symlinks not supported or permitted in this environment")
	}
}

func (h *SecurityHarness) detectSymlinkSupport() {
	testTarget := filepath.Join(h.rootDir, ".symlink_test_target")
	testLink := filepath.Join(h.rootDir, ".symlink_test_link")

	if err := os.WriteFile(testTarget, []byte("probe"), 0600); err != nil {
		h.symlinks = false
		return
	}
	defer func() { _ = os.Remove(testTarget) }()

	if err := os.Symlink(testTarget, testLink); err != nil {
		h.symlinks = false
		return
	}
	_ = os.Remove(testLink)
	h.symlinks = true
}

// CreateSymlink safely creates a symbolic link inside the temporary sandbox.
func (h *SecurityHarness) CreateSymlink(target, link string) error {
	h.t.Helper()
	h.RequireSymlinks()
	return os.Symlink(target, link)
}

// ReplaceWithDifferentInode replaces the file at path with a newly created file,
// changing its inode while preserving the path name.
func (h *SecurityHarness) ReplaceWithDifferentInode(path string, content []byte) error {
	h.t.Helper()
	tmp := path + ".swap_tmp"
	if err := os.Rename(path, tmp); err != nil {
		return fmt.Errorf("failed to move original file: %w", err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		return fmt.Errorf("failed to write replacement file: %w", err)
	}
	_ = os.Remove(tmp)
	return nil
}

// MakeRecord synthesizes a valid WAL record with the given sequence number, key, and value.
func (h *SecurityHarness) MakeRecord(seq uint64, key, val string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000 + seq,
		Key:       []byte(key),
		Value:     []byte(val),
	}
}

// CreateSegmentWithRecords creates a valid segment file with N sequential records.
func (h *SecurityHarness) CreateSegmentWithRecords(id uint64, startSeq uint64, count int) string {
	h.t.Helper()
	if _, err := wal.InitDir(h.rootDir); err != nil {
		h.t.Fatalf("InitDir failed: %v", err)
	}

	segPath := wal.SegmentPath(h.rootDir, id)
	w, err := wal.CreateWriter(segPath)
	if err != nil {
		h.t.Fatalf("CreateWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	for i := 0; i < count; i++ {
		rec := h.MakeRecord(startSeq+uint64(i), fmt.Sprintf("k-%d-%d", id, i), fmt.Sprintf("v-%d-%d", id, i))
		if err := w.Append(rec); err != nil {
			h.t.Fatalf("Append failed: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		h.t.Fatalf("Sync failed: %v", err)
	}

	return segPath
}

// AppendRawBytes appends arbitrary raw bytes to a file.
func (h *SecurityHarness) AppendRawBytes(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(data)
	return err
}

// CorruptByteAt flips bits at a specific offset in a file.
func (h *SecurityHarness) CorruptByteAt(path string, offset int64, bitMask byte) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	b := make([]byte, 1)
	if _, err := f.ReadAt(b, offset); err != nil {
		return err
	}
	b[0] ^= bitMask
	_, err = f.WriteAt(b, offset)
	return err
}

// IsWindows reports whether the test is executing under Windows.
func (h *SecurityHarness) IsWindows() bool {
	return runtime.GOOS == "windows"
}

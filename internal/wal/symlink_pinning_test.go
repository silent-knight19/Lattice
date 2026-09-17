package wal_test

import (
	stdErrors "errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestCreateWriter_ParentSymlink_Rejected asserts that CreateWriter rejects
// creating a segment file when its parent directory is a symbolic link (SEC-P02-001).
func TestCreateWriter_ParentSymlink_Rejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows without elevated privileges")
	}

	baseDir := t.TempDir()
	outsideTarget := t.TempDir()

	// Create symlink parent: baseDir/symlink_wal -> outsideTarget
	symlinkWal := filepath.Join(baseDir, "symlink_wal")
	if err := os.Symlink(outsideTarget, symlinkWal); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	segmentPath := filepath.Join(symlinkWal, "wal_000000000001.log")
	w, err := wal.CreateWriter(segmentPath)
	if err == nil {
		_ = w.Close()
		t.Fatal("expected CreateWriter to reject parent symlink, got nil")
	}

	if !stdErrors.Is(err, errors.ErrParentDirectorySymlink) {
		t.Fatalf("expected error wrapping ErrParentDirectorySymlink, got: %v", err)
	}

	// Verify no file was created in outside target
	entries, err := os.ReadDir(outsideTarget)
	if err != nil {
		t.Fatalf("failed to read outside target: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected outside target to remain empty, found %d entries", len(entries))
	}
}

// TestOpenWriter_ParentSymlink_Rejected asserts that OpenWriter rejects
// opening or creating a segment file when its parent directory is a symbolic link (SEC-P02-001).
func TestOpenWriter_ParentSymlink_Rejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows without elevated privileges")
	}

	baseDir := t.TempDir()
	outsideTarget := t.TempDir()

	symlinkWal := filepath.Join(baseDir, "symlink_wal")
	if err := os.Symlink(outsideTarget, symlinkWal); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	segmentPath := filepath.Join(symlinkWal, "wal_000000000001.log")
	w, err := wal.OpenWriter(segmentPath)
	if err == nil {
		_ = w.Close()
		t.Fatal("expected OpenWriter to reject parent symlink, got nil")
	}

	if !stdErrors.Is(err, errors.ErrParentDirectorySymlink) {
		t.Fatalf("expected error wrapping ErrParentDirectorySymlink, got: %v", err)
	}
}

// TestRotatingWriter_ParentDirectorySwapped_FailsClosed asserts that if the
// WAL directory is displaced or replaced by a symlink during runtime,
// RotatingWriter fails closed and refuses to create subsequent segments (SEC-P02-001).
func TestRotatingWriter_ParentDirectorySwapped_FailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows without elevated privileges")
	}

	dbPath := t.TempDir()
	outsideTarget := t.TempDir()

	opts := wal.Options{
		SegmentSize:      100, // Trigger rotation quickly
		InitialSegmentID: 1,
	}

	rw, err := wal.OpenRotatingWriter(dbPath, opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Write record 1 into segment 1
	rec1 := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 1000,
		Key:       []byte("key-1"),
		Value:     []byte("val-1"),
	}
	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("AppendSync failed: %v", err)
	}

	// Adversarial Action: Displace dbPath/wal with a symlink pointing to outsideTarget
	walDir := wal.Dir(dbPath)
	backupDir := filepath.Join(dbPath, "wal_backup")
	if err := os.Rename(walDir, backupDir); err != nil {
		t.Fatalf("failed to move original wal directory: %v", err)
	}
	if err := os.Symlink(outsideTarget, walDir); err != nil {
		t.Fatalf("failed to create symlink at wal directory path: %v", err)
	}

	rotErr := rw.Rotate()
	if rotErr == nil {
		t.Fatal("expected Rotate to fail when wal directory is replaced with symlink, got nil")
	}

	if !stdErrors.Is(rotErr, errors.ErrParentDirectorySymlink) && !stdErrors.Is(rotErr, errors.ErrParentDirectorySwapped) {
		t.Fatalf("expected ErrParentDirectorySymlink or ErrParentDirectorySwapped, got: %v", rotErr)
	}

	// Ensure writer is failed closed: active is nil, subsequent operations fail
	if rw.ActiveWriter() != nil {
		t.Fatal("expected ActiveWriter to be nil after rotation failure")
	}

	rec3 := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    3,
		Timestamp: 3000,
		Key:       []byte("key-3"),
		Value:     []byte("val-3"),
	}
	if err := rw.Append(rec3); err == nil {
		t.Fatal("expected Append to fail on poisoned/closed rotating writer, got nil")
	}

	// Verify outside target was not contaminated with segment 2
	entries, err := os.ReadDir(outsideTarget)
	if err != nil {
		t.Fatalf("failed to read outside target: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected outside target to have 0 entries, found %d", len(entries))
	}
}

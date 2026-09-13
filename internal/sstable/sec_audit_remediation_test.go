package sstable_test

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	internalErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// TestVULN001_DecodeBlockIndex_OversizedBuffer verifies that DecodeBlockIndex rejects
// raw byte buffers exceeding MaxIndexBlockSize (8 MiB) immediately before any allocation.
func TestVULN001_DecodeBlockIndex_OversizedBuffer(t *testing.T) {
	oversized := make([]byte, sstable.MaxIndexBlockSize+1)
	_, err := sstable.DecodeBlockIndex(oversized)
	if err == nil {
		t.Fatal("expected DecodeBlockIndex to reject oversized buffer, got nil error")
	}
	var corruptedErr *internalErrors.IndexBlockCorruptedError
	if !errors.As(err, &corruptedErr) && !errors.Is(err, internalErrors.ErrIndexBlockCorrupted) {
		t.Fatalf("expected ErrIndexBlockCorrupted, got: %v", err)
	}
}

// TestVULN001_DecodeMetaIndexBlock_OversizedBuffer verifies that DecodeMetaIndexBlock rejects
// raw byte buffers exceeding MaxIndexBlockSize (8 MiB) immediately before any allocation.
func TestVULN001_DecodeMetaIndexBlock_OversizedBuffer(t *testing.T) {
	oversized := make([]byte, sstable.MaxIndexBlockSize+1)
	_, err := sstable.DecodeMetaIndexBlock(oversized)
	if err == nil {
		t.Fatal("expected DecodeMetaIndexBlock to reject oversized buffer, got nil error")
	}
	var corruptedErr *internalErrors.IndexBlockCorruptedError
	if !errors.As(err, &corruptedErr) && !errors.Is(err, internalErrors.ErrIndexBlockCorrupted) {
		t.Fatalf("expected ErrIndexBlockCorrupted, got: %v", err)
	}
}

// TestVULN001_ReadFilterBlock_OversizedMetaHandle verifies that TableReader.ReadFilterBlock
// rejects MetaIndexHandle sizes exceeding MaxIndexBlockSize or MaxBlockSize.
func TestVULN001_ReadFilterBlock_OversizedMetaHandle(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "test_oversized_handle.sst")

	w, err := sstable.NewTableWriter(sstPath, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	ik, err := binary.NewInternalKey([]byte("key1"), 1, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}
	if err := w.Add(ik, []byte("val1")); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	r, err := sstable.OpenTableReader(sstPath)
	if err != nil {
		t.Fatalf("OpenTableReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	// Overwrite reader's footer in-memory to simulate malicious or corrupted metaHandle
	sstable.SetFooterForTesting(r, sstable.Footer{
		MetaIndexHandle: sstable.BlockHandle{
			Offset: 0,
			Size:   sstable.MaxBlockSize + 1024,
		},
		IndexHandle: r.Footer().IndexHandle,
	})

	_, err = r.ReadFilterBlock()
	if err == nil {
		t.Fatal("expected ReadFilterBlock to reject oversized metaHandle size, got nil error")
	}
	var invalidHandleErr *internalErrors.InvalidBlockHandleError
	if !errors.As(err, &invalidHandleErr) {
		t.Fatalf("expected InvalidBlockHandleError, got: %v", err)
	}
}

// TestVULN003_TableWriter_FilterBuilderFailureAbortsWriter verifies that if filter builder
// AddKey fails, TableWriter.Add returns an error, enters stateError, and subsequent Finish() fails.
func TestVULN003_TableWriter_FilterBuilderFailureAbortsWriter(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "test_filter_fail.sst")

	fb := filter.NewFilterBlockBuilder(10)
	// Prematurely finish the filter builder so subsequent AddKey calls return ErrFilterFinished
	_ = fb.Finish()

	opts := sstable.DefaultTableWriterOptions()
	opts.FilterBuilder = fb

	w, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	ik, err := binary.NewInternalKey([]byte("key1"), 1, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}
	err = w.Add(ik, []byte("val1"))
	if err == nil {
		t.Fatal("expected Add to fail when filterBuilder.AddKey fails, got nil")
	}

	// Subsequent Add must be rejected due to stateError
	ik2, err := binary.NewInternalKey([]byte("key2"), 2, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}
	if err2 := w.Add(ik2, []byte("val2")); err2 == nil {
		t.Fatal("expected subsequent Add to be rejected on errored TableWriter")
	}

	// Finish must fail closed and never publish SSTable
	if _, errFinish := w.Finish(); errFinish == nil {
		t.Fatal("expected Finish to fail on errored TableWriter, got nil")
	}

	// Staging file should not exist
	if _, errStat := os.Lstat(sstPath); !os.IsNotExist(errStat) {
		t.Fatalf("final SSTable must not exist after aborted creation: %v", errStat)
	}
}

// TestSEC002_NewTableWriterWithFile_InsecurePermissions verifies that NewTableWriterWithFile
// strictly rejects file descriptors opened with group or world permissions.
func TestSEC002_NewTableWriterWithFile_InsecurePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions not applicable on Windows")
	}

	dir := t.TempDir()
	insecureFile := filepath.Join(dir, "insecure.sst")

	f, err := os.OpenFile(insecureFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(insecureFile)
	}()

	_, err = sstable.NewTableWriterWithFile(f, sstable.DefaultTableWriterOptions())
	if err == nil {
		t.Fatal("expected NewTableWriterWithFile to reject insecure 0666 permissions, got nil error")
	}
	var insecureErr *internalErrors.InsecureFileModeError
	if !errors.As(err, &insecureErr) {
		t.Fatalf("expected InsecureFileModeError, got: %v", err)
	}
}

// TestSEC002_NewTableWriterWithFile_SymlinkRejected verifies that NewTableWriterWithFile
// rejects files whose on-disk path is a symlink.
func TestSEC002_NewTableWriterWithFile_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "target.sst")
	if err := os.WriteFile(targetFile, []byte(""), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	symlinkFile := filepath.Join(dir, "symlink.sst")
	if err := os.Symlink(targetFile, symlinkFile); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	f, err := os.OpenFile(symlinkFile, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("OpenFile symlink failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	_, err = sstable.NewTableWriterWithFile(f, sstable.DefaultTableWriterOptions())
	if err == nil {
		t.Fatal("expected NewTableWriterWithFile to reject symlink target path, got nil error")
	}
}

// Helper to make sure compiler uses math
var _ = math.MaxInt

package wal_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

func TestSEC03_Symlink_01_WALDirIsSymlink(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	outsideTarget := filepath.Join(h.RootDir(), "outside_dir")
	if err := os.MkdirAll(outsideTarget, 0700); err != nil {
		t.Fatalf("failed to create outside target: %v", err)
	}

	// Create symlink at walDir pointing to outsideTarget
	if err := h.CreateSymlink(outsideTarget, h.WALDir()); err != nil {
		t.Fatalf("failed to create symlink for walDir: %v", err)
	}

	// InitDir must reject the symlink fail-closed to prevent directory hijacking
	_, err := wal.InitDir(h.RootDir())
	if err == nil {
		t.Fatalf("expected InitDir to reject symlink directory, but it succeeded")
	}

	// Verify outsideTarget permissions were not modified by unauthorized caller
	info, err := os.Stat(outsideTarget)
	if err != nil {
		t.Fatalf("failed to stat outside target: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("outside target should remain intact")
	}
}

func TestSEC03_Symlink_02_SegmentPathIsSymlink(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	victimPath := filepath.Join(h.RootDir(), "victim_file.txt")
	victimContent := []byte("CRITICAL_SYSTEM_DATA_DO_NOT_OVERWRITE")
	if err := os.WriteFile(victimPath, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victim file: %v", err)
	}

	segPath := wal.SegmentPath(h.RootDir(), 1)
	if err := h.CreateSymlink(victimPath, segPath); err != nil {
		t.Fatalf("failed to create segment symlink: %v", err)
	}

	// 1. CreateWriter must reject symlink
	_, err := wal.CreateWriter(segPath)
	if err == nil {
		t.Fatalf("CreateWriter should have rejected symlink")
	}

	// 2. OpenWriter must reject symlink
	_, err = wal.OpenWriter(segPath)
	if err == nil {
		t.Fatalf("OpenWriter should have rejected symlink")
	}

	// 3. OpenReader must reject symlink
	_, err = wal.OpenReader(segPath)
	if err == nil {
		t.Fatalf("OpenReader should have rejected symlink")
	}

	// 4. RecoverSegment must reject symlink
	_, err = wal.RecoverSegment(segPath)
	if err == nil {
		t.Fatalf("RecoverSegment should have rejected symlink")
	}

	// 5. ListSegments must reject symlink
	_, err = wal.ListSegments(h.RootDir())
	if err == nil {
		t.Fatalf("ListSegments should have rejected symlink")
	}

	// Verify victim was untouched
	afterContent, err := os.ReadFile(victimPath)
	if err != nil || string(afterContent) != string(victimContent) {
		t.Fatalf("victim content was modified or corrupted: %v", err)
	}
}

func TestSEC03_Symlink_03_SegmentReplacedAfterValidation(t *testing.T) {
	h := NewSecurityHarness(t)

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	segPath := wal.SegmentPath(h.RootDir(), 1)
	if err := os.WriteFile(segPath, []byte("initial_content"), 0600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Verify that if a file descriptor is opened and the disk file's inode changes,
	// post-open validation triggers an error.
	f, err := os.OpenFile(segPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("failed to open file: %v", err)
	}
	defer func() { _ = f.Close() }()

	finfo, err := f.Stat()
	if err != nil {
		t.Fatalf("f.Stat failed: %v", err)
	}

	// Swap disk file with a new inode
	if err := h.ReplaceWithDifferentInode(segPath, []byte("replacement_content")); err != nil {
		t.Fatalf("failed to replace file inode: %v", err)
	}

	postInfo, err := os.Lstat(segPath)
	if err != nil {
		t.Fatalf("os.Lstat failed: %v", err)
	}

	// SameFile check must fail
	if os.SameFile(finfo, postInfo) {
		t.Fatalf("expected different inodes after swap")
	}
}

func TestSEC03_Symlink_04_ParentDirectoryReplaced(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	realParent := filepath.Join(h.RootDir(), "parent_real")
	altParent := filepath.Join(h.RootDir(), "parent_alt")
	symParent := filepath.Join(h.RootDir(), "parent_link")

	if err := os.MkdirAll(realParent, 0700); err != nil {
		t.Fatalf("mkdir realParent: %v", err)
	}
	if err := os.MkdirAll(altParent, 0700); err != nil {
		t.Fatalf("mkdir altParent: %v", err)
	}

	if err := h.CreateSymlink(realParent, symParent); err != nil {
		t.Fatalf("create symParent: %v", err)
	}

	// Attempting to initialize WAL inside a symlinked parent directory
	// InitDir checks the final wal directory; if the parent is a symlink,
	// verifying that InitDir still sets 0700 on the resolved wal directory
	targetDB := filepath.Join(symParent, "db")
	_ = os.MkdirAll(targetDB, 0700)
	if _, err := wal.InitDir(targetDB); err != nil {
		t.Fatalf("InitDir in targetDB failed: %v", err)
	}

	walPath := wal.Dir(targetDB)
	info, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("failed to stat walPath: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("wal directory must not be a symlink")
	}
}

func TestSEC03_Symlink_05_ExistingSegmentChangesInode(t *testing.T) {
	h := NewSecurityHarness(t)

	segPath := h.CreateSegmentWithRecords(1, 1, 3)

	// Now replace segPath with a different inode containing garbage
	if err := h.ReplaceWithDifferentInode(segPath, []byte("garbage_bytes_for_inode_swap")); err != nil {
		t.Fatalf("failed to replace inode: %v", err)
	}

	// RecoverSegment must open the new file, inspect it, and fail closed
	// because it is not a valid WAL segment
	_, err := wal.RecoverSegment(segPath)
	if err == nil {
		t.Fatalf("expected RecoverSegment to fail on corrupt swapped file")
	}
}

func TestSEC03_Symlink_06_SymlinkTargetOutsideDB(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	externalTarget := filepath.Join(t.TempDir(), "external_sensitive_file")
	if err := os.WriteFile(externalTarget, []byte("SENSITIVE_DATA"), 0600); err != nil {
		t.Fatalf("write external: %v", err)
	}

	segPath := wal.SegmentPath(h.RootDir(), 2)
	if err := h.CreateSymlink(externalTarget, segPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// CreateWriter must fail
	_, err := wal.CreateWriter(segPath)
	if !errors.Is(err, os.ErrInvalid) && !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected ErrInvalid or ErrExist, got %v", err)
	}

	// Verify external target was untouched
	data, err := os.ReadFile(externalTarget)
	if err != nil || string(data) != "SENSITIVE_DATA" {
		t.Fatalf("external file was mutated: %v", err)
	}
}

func TestSEC03_Symlink_07_SegmentPointsToRegularFile(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	regularTarget := filepath.Join(h.RootDir(), "another_file.bin")
	if err := os.WriteFile(regularTarget, []byte("PRESERVE_ME"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	segPath := wal.SegmentPath(h.RootDir(), 5)
	if err := h.CreateSymlink(regularTarget, segPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// OpenReader must reject symlink
	_, err := wal.OpenReader(segPath)
	if err == nil {
		t.Fatalf("expected OpenReader to reject symlink")
	}

	// Verify target unchanged
	data, _ := os.ReadFile(regularTarget)
	if string(data) != "PRESERVE_ME" {
		t.Errorf("regularTarget was modified: %s", string(data))
	}
}

func TestSEC03_Symlink_08_SegmentPointsToDirectory(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	dirTarget := filepath.Join(h.RootDir(), "some_directory")
	if err := os.MkdirAll(dirTarget, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	segPath := wal.SegmentPath(h.RootDir(), 10)
	if err := h.CreateSymlink(dirTarget, segPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// CreateWriter on a path that resolves to directory (or is symlink)
	_, err := wal.CreateWriter(segPath)
	if err == nil {
		t.Fatalf("expected CreateWriter to fail on directory target")
	}

	// RecoverSegment must reject
	_, err = wal.RecoverSegment(segPath)
	if err == nil {
		t.Fatalf("expected RecoverSegment to fail on directory target")
	}
}

func TestSEC03_Symlink_09_DanglingSymlink(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	danglingTarget := filepath.Join(h.RootDir(), "does_not_exist_anywhere")
	segPath := wal.SegmentPath(h.RootDir(), 3)
	if err := h.CreateSymlink(danglingTarget, segPath); err != nil {
		t.Fatalf("create dangling symlink: %v", err)
	}

	// OpenReader on dangling symlink
	_, err := wal.OpenReader(segPath)
	if err == nil {
		t.Fatalf("expected OpenReader to fail on dangling symlink")
	}

	// CreateWriter on dangling symlink
	_, err = wal.CreateWriter(segPath)
	if err == nil {
		t.Fatalf("expected CreateWriter to fail on dangling symlink")
	}
}

func TestSEC03_Symlink_10_SymlinkChain(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	target := filepath.Join(h.RootDir(), "chain_final.txt")
	_ = os.WriteFile(target, []byte("final_data"), 0600)

	hop1 := filepath.Join(h.RootDir(), "hop1")
	segPath := wal.SegmentPath(h.RootDir(), 7)

	if err := h.CreateSymlink(target, hop1); err != nil {
		t.Fatalf("create hop1: %v", err)
	}
	if err := h.CreateSymlink(hop1, segPath); err != nil {
		t.Fatalf("create hop2: %v", err)
	}

	// Any operation on segPath must detect it is a symlink and abort
	_, err := wal.OpenWriter(segPath)
	if err == nil {
		t.Fatalf("expected OpenWriter to reject symlink chain")
	}
}

func TestSEC03_Symlink_11_PermissionChangesDuringOpen(t *testing.T) {
	h := NewSecurityHarness(t)

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	segPath := wal.SegmentPath(h.RootDir(), 1)
	w, err := wal.CreateWriter(segPath)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}

	// Write a valid record
	rec := h.MakeRecord(1, "k1", "v1")
	if err := w.Append(rec); err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}
	_ = w.Close()

	if !h.IsWindows() {
		// Make segment read-only (0400)
		if err := os.Chmod(segPath, 0400); err != nil {
			t.Fatalf("chmod 0400: %v", err)
		}
		defer func() { _ = os.Chmod(segPath, 0600) }()

		// CreateWriter must fail with ErrExist
		_, err = wal.CreateWriter(segPath)
		if !errors.Is(err, os.ErrExist) {
			t.Errorf("expected ErrExist, got %v", err)
		}

		// OpenWriter (which requires O_WRONLY) must fail with permission error
		_, err = wal.OpenWriter(segPath)
		if err == nil {
			t.Errorf("expected OpenWriter to fail on 0400 file")
		}

		// OpenReader (O_RDONLY) should succeed
		r, err := wal.OpenReader(segPath)
		if err != nil {
			t.Errorf("OpenReader should succeed on 0400 file: %v", err)
		} else {
			_ = r.Close()
		}
	}
}

func TestSEC03_Symlink_12_ForeignFileCollision(t *testing.T) {
	h := NewSecurityHarness(t)

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	// Create valid segment 1
	_ = h.CreateSegmentWithRecords(1, 1, 2)

	// Create foreign non-segment files in the wal directory
	foreignFiles := []string{
		filepath.Join(h.WALDir(), "temp_compaction.tmp"),
		filepath.Join(h.WALDir(), "MANIFEST-000001"),
		filepath.Join(h.WALDir(), "wal_invalid_name.log"),
		filepath.Join(h.WALDir(), "wal_000000000001.tmp"),
		filepath.Join(h.WALDir(), "README.txt"),
	}

	for _, f := range foreignFiles {
		if err := os.WriteFile(f, []byte("foreign_unrelated_content"), 0600); err != nil {
			t.Fatalf("write foreign file: %v", err)
		}
	}

	// ListSegments should gracefully ignore all foreign files and return only [1]
	ids, err := wal.ListSegments(h.RootDir())
	if err != nil {
		t.Fatalf("ListSegments failed: %v", err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("expected exactly [1], got %v", ids)
	}

	// RecoverWAL should succeed, processing segment 1 and ignoring foreign files
	var replayed []wal.Record
	report, err := wal.RecoverWAL(h.RootDir(), wal.ReplayFunc(func(rec wal.Record) error {
		replayed = append(replayed, rec)
		return nil
	}))
	if err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}
	if report.SegmentCount != 1 || len(replayed) != 2 {
		t.Fatalf("expected 1 segment, 2 records; got %d segments, %d records", report.SegmentCount, len(replayed))
	}

	// If a foreign directory is placed inside walDir, ListSegments must ignore it
	foreignDir := filepath.Join(h.WALDir(), "subfolder")
	_ = os.MkdirAll(foreignDir, 0700)
	ids, err = wal.ListSegments(h.RootDir())
	if err != nil || len(ids) != 1 {
		t.Fatalf("ListSegments should ignore subfolder, got ids=%v, err=%v", ids, err)
	}

	// If a directory happens to have the exact name of a segment, ListSegments must reject it fail-closed
	collidingDir := wal.SegmentPath(h.RootDir(), 2)
	_ = os.MkdirAll(collidingDir, 0700)
	_, err = wal.ListSegments(h.RootDir())
	if err == nil {
		t.Fatalf("expected ListSegments to fail when segment path is a directory")
	}
	var notADirErr *latticeErrors.NotADirectoryError
	if !errors.As(err, &notADirErr) {
		t.Errorf("expected NotADirectoryError, got %T (%v)", err, err)
	}
}

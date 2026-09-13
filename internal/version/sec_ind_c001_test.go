package version_test

import (
	stdErrors "errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// TestINDC001_DirSyncAfterRename_OrderingAndOnce proves the IND-C-001 remediation:
// SetCurrentManifest must invoke the parent-directory sync barrier exactly once,
// strictly after a successful rename, targeting the database directory, and must
// leave a complete CURRENT pointer with no staging residue.
func TestINDC001_DirSyncAfterRename_OrderingAndOnce(t *testing.T) {
	dir := t.TempDir()

	var order atomic.Int64 // 1=fileSync 2=rename 3=dirSync; 0=none yet
	var renameCalls atomic.Int64
	var dirSyncCalls atomic.Int64
	var dirSyncArg string
	var fileSyncCalls atomic.Int64

	restoreSync := version.SetCurrentSyncFnForTesting(func(f *os.File) error {
		fileSyncCalls.Add(1)
		if order.Load() != 0 {
			t.Errorf("file sync out of order: order=%d, want 0 (first)", order.Load())
		}
		order.Store(1)
		return nil // skip actual fdatasync; ordering is what this test proves
	})
	defer restoreSync()

	restoreRename := version.SetCurrentRenameFnForTesting(func(oldpath, newpath string) error {
		renameCalls.Add(1)
		if order.Load() != 1 {
			t.Errorf("rename out of order: order=%d, want 1 (after file sync)", order.Load())
		}
		order.Store(2)
		return os.Rename(oldpath, newpath)
	})
	defer restoreRename()

	restoreDirSync := version.SetCurrentSyncDirFnForTesting(func(dirPath string) error {
		dirSyncCalls.Add(1)
		dirSyncArg = dirPath
		if order.Load() != 2 {
			t.Errorf("dir sync out of order: order=%d, want 2 (after rename)", order.Load())
		}
		order.Store(3)
		return version.SyncDirForTesting(dirPath)
	})
	defer restoreDirSync()

	if err := version.SetCurrentManifest(dir, 7); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	if got := fileSyncCalls.Load(); got != 1 {
		t.Errorf("file sync calls = %d, want exactly 1", got)
	}
	if got := renameCalls.Load(); got != 1 {
		t.Errorf("rename calls = %d, want exactly 1", got)
	}
	if got := dirSyncCalls.Load(); got != 1 {
		t.Fatalf("dir sync calls = %d, want exactly 1 (IND-C-001)", got)
	}
	if order.Load() != 3 {
		t.Errorf("final order = %d, want 3 (fileSync->rename->dirSync)", order.Load())
	}
	// Directory argument must be the cleaned DB dir, not the tmp path.
	if dirSyncArg != filepath.Clean(dir) {
		t.Errorf("dir sync arg = %q, want %q", dirSyncArg, filepath.Clean(dir))
	}

	content, err := os.ReadFile(filepath.Join(dir, version.CurrentFilename))
	if err != nil {
		t.Fatalf("ReadFile CURRENT failed: %v", err)
	}
	if string(content) != "MANIFEST-000007\n" {
		t.Errorf("CURRENT content = %q, want %q", content, "MANIFEST-000007\n")
	}
	if _, err := os.Lstat(filepath.Join(dir, version.CurrentTempFilename)); !os.IsNotExist(err) {
		t.Errorf("staging file CURRENT.tmp still exists after success")
	}
}

// TestINDC001_DirSyncFailurePreservesPointer proves the failure contract: if the
// post-rename directory barrier fails, SetCurrentManifest must surface
// ErrCurrentDirectorySync (wrapping the root cause) while leaving the renamed
// CURRENT intact — never deleting it or leaving a torn pointer.
func TestINDC001_DirSyncFailurePreservesPointer(t *testing.T) {
	dir := t.TempDir()

	if err := version.SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	injectedErr := stdErrors.New("simulated IND-C-001 parent directory sync failure")
	restore := version.SetCurrentSyncDirFnForTesting(func(dirPath string) error {
		return injectedErr
	})
	defer restore()

	err := version.SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected failure on directory sync error")
	}
	if !stdErrors.Is(err, errors.ErrCurrentDirectorySync) {
		t.Errorf("expected ErrCurrentDirectorySync, got %v", err)
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected root cause %v wrapped, got %v", injectedErr, err)
	}

	// Rename already succeeded, so CURRENT must hold the new pointer intact.
	content, readErr := os.ReadFile(filepath.Join(dir, version.CurrentFilename))
	if readErr != nil {
		t.Fatalf("CURRENT missing after dir-sync failure: %v", readErr)
	}
	if string(content) != "MANIFEST-000002\n" {
		t.Errorf("CURRENT = %q, want %q (renamed pointer must survive)", content, "MANIFEST-000002\n")
	}
	if _, statErr := os.Lstat(filepath.Join(dir, version.CurrentTempFilename)); !os.IsNotExist(statErr) {
		t.Errorf("staging file must not remain after rename succeeded")
	}
}

// TestINDC001_StrictSyncOptIn verifies the user-approved durability option:
// default mode uses the fast fdatasync path; SetCurrentStrictSync(true) opts
// into a full fsync file barrier. Both modes must succeed and the flag must
// round-trip; the directory barrier runs in both modes.
func TestINDC001_StrictSyncOptIn(t *testing.T) {
	// Preserve global flag across test (process-wide setting).
	prev := version.CurrentStrictSync()
	defer version.SetCurrentStrictSync(prev)

	version.SetCurrentStrictSync(false)
	if version.CurrentStrictSync() {
		t.Fatalf("strict flag should be false after disable")
	}
	dir1 := t.TempDir()
	if err := version.SetCurrentManifest(dir1, 3); err != nil {
		t.Fatalf("default-mode SetCurrentManifest failed: %v", err)
	}

	version.SetCurrentStrictSync(true)
	if !version.CurrentStrictSync() {
		t.Fatalf("strict flag should be true after enable")
	}
	dir2 := t.TempDir()
	var dirSyncCalls atomic.Int64
	restore := version.SetCurrentSyncDirFnForTesting(func(dirPath string) error {
		dirSyncCalls.Add(1)
		return version.SyncDirForTesting(dirPath)
	})
	defer restore()
	if err := version.SetCurrentManifest(dir2, 4); err != nil {
		t.Fatalf("strict-mode SetCurrentManifest failed: %v", err)
	}
	if got := dirSyncCalls.Load(); got != 1 {
		t.Errorf("strict mode must still dir-sync exactly once, got %d", got)
	}
	content, err := os.ReadFile(filepath.Join(dir2, version.CurrentFilename))
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(content) != "MANIFEST-000004\n" {
		t.Errorf("CURRENT = %q, want %q", content, "MANIFEST-000004\n")
	}
}

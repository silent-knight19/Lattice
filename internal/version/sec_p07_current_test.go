package version_test

import (
	stdErrors "errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// TestSEC_P07_03_ParentSwapBeforeTempCreation verifies that swapping the parent directory path
// after initial resolution cannot redirect temporary file creation into an attacker directory.
// Descriptor-anchored operations remain bound to the opened directory object.
func TestSEC_P07_03_ParentSwapBeforeTempCreation(t *testing.T) {
	baseDir := t.TempDir()
	dbDir := filepath.Join(baseDir, "real_db")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatalf("failed to create real_db: %v", err)
	}

	attackerDir := filepath.Join(baseDir, "attacker_dir")
	if err := os.Mkdir(attackerDir, 0700); err != nil {
		t.Fatalf("failed to create attacker_dir: %v", err)
	}

	// Hook into createTempAt seam to swap parent directory before creating CURRENT.tmp
	restoreHook := version.SetCurrentCreateTempAtFnForTesting(func(dirFile *os.File, name string, perm os.FileMode) (*os.File, error) {
		// Attempt directory path swap
		_ = os.Rename(dbDir, dbDir+"_moved")
		_ = os.Symlink(attackerDir, dbDir)
		// Perform creation on original pinned descriptor
		return version.CreateTempAt(dirFile, name, perm)
	})
	defer restoreHook()

	_ = version.SetCurrentManifest(dbDir, 1)

	// Attacker directory must have NO files created in it
	entries, err := os.ReadDir(attackerDir)
	if err != nil {
		t.Fatalf("failed to read attackerDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("attacker directory was polluted! entries: %v", entries)
	}
}

// TestSEC_P07_03_ParentSwapBeforeRename verifies that replacing the parent pathname
// between temp file creation and rename does not redirect the atomic rename into an attacker directory.
// Descriptor-anchored renameat guarantees destination is in the originally pinned directory descriptor.
func TestSEC_P07_03_ParentSwapBeforeRename(t *testing.T) {
	baseDir := t.TempDir()
	dbDir := filepath.Join(baseDir, "real_db")
	if err := os.Mkdir(dbDir, 0700); err != nil {
		t.Fatalf("failed to create real_db: %v", err)
	}

	attackerDir := filepath.Join(baseDir, "attacker_dir")
	if err := os.Mkdir(attackerDir, 0700); err != nil {
		t.Fatalf("failed to create attacker_dir: %v", err)
	}

	restoreRename := version.SetCurrentRenameAtFnForTesting(func(dirFile *os.File, oldName, newName string) error {
		// Swap path on disk right before rename
		_ = os.Rename(dbDir, dbDir+"_swapped")
		_ = os.Symlink(attackerDir, dbDir)
		return version.RenameAt(dirFile, oldName, newName)
	})
	defer restoreRename()

	_ = version.SetCurrentManifest(dbDir, 10)

	// Attacker directory must NOT contain CURRENT
	attackerCurrent := filepath.Join(attackerDir, version.CurrentFilename)
	if _, err := os.Lstat(attackerCurrent); !os.IsNotExist(err) {
		t.Fatalf("CURRENT was renamed into attacker directory!")
	}
}

// TestSEC_P07_03_SymlinkCurrentTargetProtection verifies that if CURRENT is replaced
// by a symbolic link pointing to a critical victim file, SetCurrentManifest refuses to
// overwrite the victim file.
func TestSEC_P07_03_SymlinkCurrentTargetProtection(t *testing.T) {
	dir := t.TempDir()
	victimPath := filepath.Join(dir, "victim_file.txt")
	victimContent := []byte("confidential-critical-data")
	if err := os.WriteFile(victimPath, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victim: %v", err)
	}

	// CURRENT is a symlink pointing to victim
	currentPath := filepath.Join(dir, version.CurrentFilename)
	if err := os.Symlink(victimPath, currentPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	err := version.SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatal("expected error when CURRENT is a symlink, got nil")
	}
	if !stdErrors.Is(err, errors.ErrCurrentSymlink) {
		t.Fatalf("expected ErrCurrentSymlink, got: %v", err)
	}

	// Victim content must remain completely unchanged
	content, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("failed to read victim: %v", err)
	}
	if string(content) != string(victimContent) {
		t.Fatalf("victim file was overwritten or modified! got %q, want %q", string(content), string(victimContent))
	}
}

// TestSEC_P07_03_SymlinkTempTargetProtection verifies that if CURRENT.tmp is a symlink
// pointing to a victim file, SetCurrentManifest refuses to write through the symlink.
func TestSEC_P07_03_SymlinkTempTargetProtection(t *testing.T) {
	dir := t.TempDir()
	victimPath := filepath.Join(dir, "victim_temp.txt")
	victimContent := []byte("important-user-state")
	if err := os.WriteFile(victimPath, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victim: %v", err)
	}

	// CURRENT.tmp is a symlink pointing to victim
	tmpPath := filepath.Join(dir, version.CurrentTempFilename)
	if err := os.Symlink(victimPath, tmpPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	err := version.SetCurrentManifest(dir, 3)
	if err == nil {
		t.Fatal("expected error when CURRENT.tmp is a symlink, got nil")
	}
	if !stdErrors.Is(err, errors.ErrCurrentSymlink) {
		t.Fatalf("expected ErrCurrentSymlink, got: %v", err)
	}

	// Victim content must remain intact
	content, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("failed to read victim: %v", err)
	}
	if string(content) != string(victimContent) {
		t.Fatalf("victim file modified! got %q, want %q", string(content), string(victimContent))
	}
}

// TestSEC_P07_03_ConcurrentWritersStress verifies that multiple concurrent SetCurrentManifest
// invocations remain serialized, crash-safe, and result in a valid canonical CURRENT file.
func TestSEC_P07_03_ConcurrentWritersStress(t *testing.T) {
	dir := t.TempDir()
	const numWriters = 20

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	for i := 1; i <= numWriters; i++ {
		manifestNum := uint64(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier
			_ = version.SetCurrentManifest(dir, manifestNum)
		}()
	}

	close(startBarrier)
	wg.Wait()

	// Read and strictly validate final CURRENT pointer
	finalNum, err := version.ReadCurrentManifest(dir)
	if err != nil {
		t.Fatalf("ReadCurrentManifest failed on final state: %v", err)
	}
	if finalNum < 1 || finalNum > numWriters {
		t.Fatalf("invalid final manifest number: %d (expected 1..%d)", finalNum, numWriters)
	}

	// Verify no stray CURRENT.tmp left behind
	tmpPath := filepath.Join(dir, version.CurrentTempFilename)
	if _, err := os.Lstat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("stray CURRENT.tmp was left in directory after concurrent writes")
	}
}

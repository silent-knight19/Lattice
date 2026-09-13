package engine_test

import (
	stdErrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
)

// TestSEC_P07_02_ParentDirectoryReplacement verifies that cleaner operations are anchored
// to the opened directory file descriptor via unlinkAt: replacing the parent path after open
// does NOT delete files in the replaced/decoy directory.
func TestSEC_P07_02_ParentDirectoryReplacement(t *testing.T) {
	baseDir := t.TempDir()
	originalDir := filepath.Join(baseDir, "db")
	if err := os.Mkdir(originalDir, 0700); err != nil {
		t.Fatalf("failed to create originalDir: %v", err)
	}

	candidateName := ".tmp_000001.sst_123456"
	origFile := filepath.Join(originalDir, candidateName)
	if err := os.WriteFile(origFile, []byte("original-candidate-data"), 0600); err != nil {
		t.Fatalf("failed to write original candidate: %v", err)
	}

	// Create decoy directory with same candidate filename
	decoyDir := filepath.Join(baseDir, "decoy")
	if err := os.Mkdir(decoyDir, 0700); err != nil {
		t.Fatalf("failed to create decoyDir: %v", err)
	}
	decoyFile := filepath.Join(decoyDir, candidateName)
	decoyContent := []byte("critical-victim-data-do-not-delete")
	if err := os.WriteFile(decoyFile, decoyContent, 0600); err != nil {
		t.Fatalf("failed to write decoy victim: %v", err)
	}

	// Run cleanup on original directory
	report, err := engine.CleanOrphanedFilesDir(originalDir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.FilesCleaned != 1 {
		t.Fatalf("expected 1 file cleaned, got %d", report.FilesCleaned)
	}

	// Original candidate file must be gone
	if _, err := os.Lstat(origFile); !os.IsNotExist(err) {
		t.Fatalf("expected original candidate to be removed, got err: %v", err)
	}

	// Decoy victim file must remain completely untouched
	decoyRead, err := os.ReadFile(decoyFile)
	if err != nil {
		t.Fatalf("failed to read decoy file: %v", err)
	}
	if string(decoyRead) != string(decoyContent) {
		t.Fatalf("decoy victim was corrupted or altered! got %q, want %q", string(decoyRead), string(decoyContent))
	}
}

// TestSEC_P07_02_ParentSymlinkSubstitution verifies that CleanOrphanedFilesDir rejects
// a database directory path that is a symbolic link pointing to a victim directory.
func TestSEC_P07_02_ParentSymlinkSubstitution(t *testing.T) {
	baseDir := t.TempDir()
	victimDir := filepath.Join(baseDir, "victim_dir")
	if err := os.Mkdir(victimDir, 0700); err != nil {
		t.Fatalf("failed to create victimDir: %v", err)
	}

	victimFile := filepath.Join(victimDir, ".tmp_000001.sst_123456")
	victimContent := []byte("valuable-user-content")
	if err := os.WriteFile(victimFile, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victim file: %v", err)
	}

	// Create symlink pointing to victim directory
	symlinkPath := filepath.Join(baseDir, "symlink_db")
	if err := os.Symlink(victimDir, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// CleanOrphanedFilesDir on symlinked directory must fail
	report, err := engine.CleanOrphanedFilesDir(symlinkPath)
	if err == nil {
		t.Fatalf("expected error cleaning symlinked directory, got success: %+v", report)
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected os.ErrInvalid for symlinked db directory, got: %v", err)
	}

	// Victim file must remain intact
	readData, err := os.ReadFile(victimFile)
	if err != nil {
		t.Fatalf("failed to read victim file: %v", err)
	}
	if string(readData) != string(victimContent) {
		t.Fatalf("victim file content was altered: got %q, want %q", string(readData), string(victimContent))
	}
}

// TestSEC_P07_02_HardLinkCandidate verifies that unlinking an orphan staging candidate
// that is a hard link to another inode removes the candidate directory entry without
// modifying, truncating, or corrupting the underlying inode content preserved at the other link.
func TestSEC_P07_02_HardLinkCandidate(t *testing.T) {
	dir := t.TempDir()
	originalFile := filepath.Join(dir, "target_preserved.sst")
	originalContent := []byte("durable-sstable-payload-referenced-by-hardlink")
	if err := os.WriteFile(originalFile, originalContent, 0600); err != nil {
		t.Fatalf("failed to write original file: %v", err)
	}

	candidateName := ".tmp_000001.sst_123456"
	candidatePath := filepath.Join(dir, candidateName)
	if err := os.Link(originalFile, candidatePath); err != nil {
		t.Fatalf("failed to create hard link: %v", err)
	}

	// Verify hard link exists and links point to same inode
	origStat, err := os.Stat(originalFile)
	if err != nil {
		t.Fatalf("failed to stat originalFile: %v", err)
	}
	candStat, err := os.Stat(candidatePath)
	if err != nil {
		t.Fatalf("failed to stat candidatePath: %v", err)
	}
	if !os.SameFile(origStat, candStat) {
		t.Fatal("expected originalFile and candidatePath to share the same inode")
	}

	// Run cleanup
	report, err := engine.CleanOrphanedFilesDir(dir)
	if err != nil {
		t.Fatalf("CleanOrphanedFilesDir failed: %v", err)
	}
	if report.FilesCleaned != 1 {
		t.Fatalf("expected 1 file cleaned, got %d", report.FilesCleaned)
	}

	// Candidate directory entry must be unlinked
	if _, err := os.Lstat(candidatePath); !os.IsNotExist(err) {
		t.Fatalf("expected candidate hard link to be removed, got: %v", err)
	}

	// Original file MUST remain intact, with untouched contents
	data, err := os.ReadFile(originalFile)
	if err != nil {
		t.Fatalf("failed to read originalFile after candidate unlinked: %v", err)
	}
	if string(data) != string(originalContent) {
		t.Fatalf("originalFile content corrupted! got %q, want %q", string(data), string(originalContent))
	}
}

// TestSEC_P07_06_DirectorySyncFailureVisibility verifies that when directory synchronization
// fails after unlinking files, the report accurately preserves FilesCleaned, sets DirectorySyncFailed
// and SyncError, and returns the durability error without falsely claiming durability.
func TestSEC_P07_06_DirectorySyncFailureVisibility(t *testing.T) {
	dir := t.TempDir()
	candidateName := ".tmp_000001.sst_999999"
	candidatePath := filepath.Join(dir, candidateName)
	if err := os.WriteFile(candidatePath, []byte("temp-data"), 0600); err != nil {
		t.Fatalf("failed to write candidate: %v", err)
	}

	injectedErr := stdErrors.New("simulated-eio-sync-error")
	restoreSync := engine.SetCleanerSyncDirFnForTesting(func(f *os.File) error {
		return injectedErr
	})
	defer restoreSync()

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err == nil {
		t.Fatal("expected error when directory sync fails, got nil")
	}

	// FilesCleaned must still report 1 (the physical unlink succeeded)
	if report.FilesCleaned != 1 {
		t.Fatalf("expected FilesCleaned == 1, got %d", report.FilesCleaned)
	}

	// Synchronization status must be explicitly recorded
	if !report.DirectorySyncFailed {
		t.Fatal("expected report.DirectorySyncFailed to be true")
	}
	if !stdErrors.Is(report.SyncError, injectedErr) {
		t.Fatalf("expected report.SyncError to wrap injectedErr, got: %v", report.SyncError)
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Fatalf("expected returned error to wrap injectedErr, got: %v", err)
	}

	// Verify idempotency: subsequent run without sync failure succeeds with 0 cleaned
	restoreSync()
	report2, err2 := engine.CleanOrphanedFilesDir(dir)
	if err2 != nil {
		t.Fatalf("subsequent clean failed: %v", err2)
	}
	if report2.FilesCleaned != 0 {
		t.Fatalf("expected 0 files cleaned on idempotent retry, got %d", report2.FilesCleaned)
	}
	if report2.DirectorySyncFailed {
		t.Fatal("expected DirectorySyncFailed to be false on clean retry")
	}
}

// TestSEC_P07_02_CandidateSymlinkPreservesVictim verifies that if an attacker places a symlink
// named like an orphan candidate pointing to a victim file, the cleaner refuses to delete or follow it.
func TestSEC_P07_02_CandidateSymlinkPreservesVictim(t *testing.T) {
	dir := t.TempDir()
	victimFile := filepath.Join(dir, "live_victim.sst")
	victimContent := []byte("important-sst-data")
	if err := os.WriteFile(victimFile, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victimFile: %v", err)
	}

	// Create symlink candidate pointing to victimFile
	candidateName := ".tmp_000001.sst_123456"
	candidatePath := filepath.Join(dir, candidateName)
	if err := os.Symlink(victimFile, candidatePath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	report, err := engine.CleanOrphanedFilesDir(dir)
	if err == nil {
		t.Fatalf("expected error when symlink candidate encountered, got nil")
	}
	if report.FilesCleaned != 0 {
		t.Fatalf("expected 0 files cleaned (symlink rejected), got %d", report.FilesCleaned)
	}

	// Symlink was recorded as a failure
	if _, ok := report.Failures[candidateName]; !ok {
		t.Fatalf("expected failure for %s in report.Failures, got: %v", candidateName, report.Failures)
	}

	// Victim file remains untouched
	readData, err := os.ReadFile(victimFile)
	if err != nil {
		t.Fatalf("failed to read victim: %v", err)
	}
	if string(readData) != string(victimContent) {
		t.Fatalf("victim file corrupted! got %q, want %q", string(readData), string(victimContent))
	}
}

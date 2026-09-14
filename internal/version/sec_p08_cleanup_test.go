package version

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestP08_SEC_001_DirectoryDescriptorPinning verifies that deletePhysicalFiles unlinks
// obsolete files strictly anchored to the opened directory file descriptor via removeAt.
func TestP08_SEC_001_DirectoryDescriptorPinning(t *testing.T) {
	dir := t.TempDir()

	// Create real mock SSTable file
	fileNum := uint64(101)
	sstPath := filepath.Join(dir, TableFilename(fileNum))
	if err := os.WriteFile(sstPath, []byte("sstable-data"), 0600); err != nil {
		t.Fatalf("failed to create mock SSTable: %v", err)
	}

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:      dir,
		NextFileNum: 200,
	})

	var removeAtCalled atomic.Bool
	var unlinkedName string

	vs.SetRemoveAtHook(func(dirFile *os.File, name string) error {
		removeAtCalled.Store(true)
		unlinkedName = name

		// Verify dirFile is the actual directory
		fi, err := dirFile.Stat()
		if err != nil {
			t.Fatalf("dirFile.Stat failed: %v", err)
		}
		if !fi.IsDir() {
			t.Fatalf("expected dirFile to be directory, got mode: %s", fi.Mode())
		}

		// Perform real unlink via removeAt
		return removeAt(dirFile, name)
	})

	cleaned, err := vs.deletePhysicalFiles(dir, []uint64{fileNum})
	if err != nil {
		t.Fatalf("deletePhysicalFiles failed: %v", err)
	}

	if !removeAtCalled.Load() {
		t.Fatalf("expected removeAt hook to be called for descriptor-pinned deletion")
	}
	if unlinkedName != TableFilename(fileNum) {
		t.Fatalf("expected unlinked name %s, got %s", TableFilename(fileNum), unlinkedName)
	}
	if len(cleaned) != 1 || cleaned[0] != fileNum {
		t.Fatalf("expected cleaned file [%d], got %v", fileNum, cleaned)
	}

	// Verify file was physically removed from disk
	if _, err := os.Lstat(sstPath); !os.IsNotExist(err) {
		t.Fatalf("expected file to be unlinked from disk, got err: %v", err)
	}
}

// TestP08_SEC_001_DirectorySubstitutionDefense verifies that if the database directory
// descriptor does not match the observed directory via SameFile, cleanup fails closed.
func TestP08_SEC_001_DirectorySubstitutionDefense(t *testing.T) {
	dir := t.TempDir()
	otherDir := t.TempDir()

	fileNum := uint64(202)
	sstPath := filepath.Join(dir, TableFilename(fileNum))
	if err := os.WriteFile(sstPath, []byte("data"), 0600); err != nil {
		t.Fatalf("failed to write sstable: %v", err)
	}

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:      dir,
		NextFileNum: 300,
	})

	// Simulate directory substitution by having lstat return stats from otherDir
	vs.SetTestHooks(func(name string) (os.FileInfo, error) {
		if filepath.Clean(name) == filepath.Clean(dir) {
			return os.Lstat(otherDir)
		}
		return os.Lstat(name)
	}, nil, nil)

	cleaned, err := vs.deletePhysicalFiles(dir, []uint64{fileNum})
	if err == nil {
		t.Fatalf("expected error on directory substitution, got nil")
	}
	if !strings.Contains(err.Error(), "substituted") {
		t.Fatalf("expected substitution error, got: %v", err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("expected 0 cleaned files on substitution, got: %v", cleaned)
	}

	// Target file must remain untouched
	if _, err := os.Lstat(sstPath); err != nil {
		t.Fatalf("expected sstPath to still exist, got err: %v", err)
	}
}

// TestP08_SEC_001_DirectorySymlinkDefense verifies that if the base directory is a symlink,
// deletePhysicalFiles rejects cleanup with a security violation.
func TestP08_SEC_001_DirectorySymlinkDefense(t *testing.T) {
	realDir := t.TempDir()
	parentDir := t.TempDir()
	symlinkDir := filepath.Join(parentDir, "symlink_db")

	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:      symlinkDir,
		NextFileNum: 100,
	})

	cleaned, err := vs.deletePhysicalFiles(symlinkDir, []uint64{10})
	if err == nil {
		t.Fatalf("expected error when DBPath is a symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink security violation, got: %v", err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("expected 0 cleaned files, got: %v", cleaned)
	}
}

// TestP08_SEC_001_SymlinkVictimProtection verifies that if a candidate SSTable file is replaced
// by a symlink to an external sensitive file, deletePhysicalFiles refuses to unlink it.
func TestP08_SEC_001_SymlinkVictimProtection(t *testing.T) {
	dbDir := t.TempDir()
	externalDir := t.TempDir()

	// External sensitive file
	victimFile := filepath.Join(externalDir, "sensitive_db.conf")
	if err := os.WriteFile(victimFile, []byte("SUPER_SECRET_KEY"), 0600); err != nil {
		t.Fatalf("failed to create victim file: %v", err)
	}

	fileNum := uint64(555)
	sstSymlink := filepath.Join(dbDir, TableFilename(fileNum))
	if err := os.Symlink(victimFile, sstSymlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:      dbDir,
		NextFileNum: 600,
	})

	cleaned, err := vs.deletePhysicalFiles(dbDir, []uint64{fileNum})
	if err == nil {
		t.Fatalf("expected error refusing to unlink symlink, got nil")
	}
	if !strings.Contains(err.Error(), "refusing to unlink symlink") {
		t.Fatalf("expected symlink refusal message, got: %v", err)
	}
	if len(cleaned) != 0 {
		t.Fatalf("expected 0 cleaned files, got: %v", cleaned)
	}

	// Invariant: Victim file must not be modified or unlinked!
	content, err := os.ReadFile(victimFile)
	if err != nil {
		t.Fatalf("failed to read victim file: %v", err)
	}
	if string(content) != "SUPER_SECRET_KEY" {
		t.Fatalf("victim file content mutated! got: %s", string(content))
	}
}

// TestP08_SEC_008_PathConfinementAndFilenameSanity verifies that invalid or path-escaping
// filenames are strictly rejected.
func TestP08_SEC_008_PathConfinementAndFilenameSanity(t *testing.T) {
	dir := t.TempDir()

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:      dir,
		NextFileNum: 100,
	})

	// FileNum 0 is invalid
	cleaned, err := vs.deletePhysicalFiles(dir, []uint64{0})
	if err == nil {
		t.Fatalf("expected error on FileNum 0, got nil")
	}
	if len(cleaned) != 0 {
		t.Fatalf("expected 0 cleaned files, got: %v", cleaned)
	}
}

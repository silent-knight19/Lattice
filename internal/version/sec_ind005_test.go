package version_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/version"
)

// TestIND005_PreRenameSymlinkDetected verifies that if a symlink appears at CURRENT
// immediately before rename, SetCurrentManifest aborts and rejects the symlink.
func TestIND005_PreRenameSymlinkDetected(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, version.CurrentFilename)

	// Create a dummy target file and a symlink at CURRENT
	targetFile := filepath.Join(dir, "external_config.txt")
	if err := os.WriteFile(targetFile, []byte("sensitive-config"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := os.Symlink(targetFile, currentPath); err != nil {
		t.Fatalf("Symlink creation failed: %v", err)
	}

	err := version.SetCurrentManifest(dir, 1)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail when CURRENT is a symlink")
	}

	// Verify target file was NOT overwritten
	content, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(content) != "sensitive-config" {
		t.Fatalf("target file was overwritten: %q", string(content))
	}
}

// TestIND005_PostRenameMismatchRejected verifies that if the post-rename stat
// does not match the temporary file inode, SetCurrentManifest fails closed.
func TestIND005_PostRenameMismatchRejected(t *testing.T) {
	dir := t.TempDir()

	// Intercept rename to replace destination with a different file
	otherFile := filepath.Join(dir, "other_file.txt")
	if err := os.WriteFile(otherFile, []byte("other-content"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	restore := version.SetCurrentRenameFnForTesting(func(oldpath, newpath string) error {
		// Instead of renaming oldpath -> newpath, copy otherFile -> newpath
		data, _ := os.ReadFile(otherFile)
		return os.WriteFile(newpath, data, 0600)
	})
	defer restore()

	err := version.SetCurrentManifest(dir, 1)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to detect inode mismatch post-rename")
	}
}

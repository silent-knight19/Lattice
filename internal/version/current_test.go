package version

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

// readCurrentForTesting is a test-only reader that reads and returns the raw bytes
// and trimmed string content of the CURRENT pointer file.
func readCurrentForTesting(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	p := filepath.Join(dir, CurrentFilename)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("failed to read CURRENT file at %s: %v", p, err)
	}
	return string(bytes.TrimSpace(b)), b
}

// -----------------------------------------------------------------------------
// EXACT-BYTE TEST FIXTURES (§29)
// -----------------------------------------------------------------------------

func TestSetCurrentManifest_ExactBytes_Manifest1(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	// Exact canonical bytes: 16 bytes ASCII ("MANIFEST-000001\n")
	expectedBytes := []byte{
		0x4d, 0x41, 0x4e, 0x49, 0x46, 0x45, 0x53, 0x54, // "MANIFEST"
		0x2d,                               // "-"
		0x30, 0x30, 0x30, 0x30, 0x30, 0x31, // "000001"
		0x0a, // "\n"
	}

	rawBytes, err := os.ReadFile(filepath.Join(dir, CurrentFilename))
	if err != nil {
		t.Fatalf("failed to read CURRENT file: %v", err)
	}

	if !bytes.Equal(rawBytes, expectedBytes) {
		t.Fatalf("exact bytes mismatch for manifest 1:\ngot:  %x (%q)\nwant: %x (%q)",
			rawBytes, string(rawBytes), expectedBytes, string(expectedBytes))
	}
}

func TestSetCurrentManifest_ExactBytes_Manifest42(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 42); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	// Exact canonical bytes: 16 bytes ASCII ("MANIFEST-000042\n")
	expectedBytes := []byte{
		0x4d, 0x41, 0x4e, 0x49, 0x46, 0x45, 0x53, 0x54, // "MANIFEST"
		0x2d,                               // "-"
		0x30, 0x30, 0x30, 0x30, 0x34, 0x32, // "000042"
		0x0a, // "\n"
	}

	rawBytes, err := os.ReadFile(filepath.Join(dir, CurrentFilename))
	if err != nil {
		t.Fatalf("failed to read CURRENT file: %v", err)
	}

	if !bytes.Equal(rawBytes, expectedBytes) {
		t.Fatalf("exact bytes mismatch for manifest 42:\ngot:  %x (%q)\nwant: %x (%q)",
			rawBytes, string(rawBytes), expectedBytes, string(expectedBytes))
	}
}

// -----------------------------------------------------------------------------
// CREATION, REPLACEMENT & REPEATED UPDATES (§27)
// -----------------------------------------------------------------------------

func TestSetCurrentManifest_FirstCreation(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, CurrentFilename)
	tmpPath := filepath.Join(dir, CurrentTempFilename)

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	// Verify CURRENT exists
	info, err := os.Stat(currentPath)
	if err != nil {
		t.Fatalf("CURRENT file missing after creation: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("CURRENT is not a regular file (mode: %s)", info.Mode())
	}

	// Verify permissions (0600 subject to umask)
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("insecure permissions: CURRENT must not grant group/other access, got %04o", perm)
	}

	// Verify content
	name, raw := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000001" {
		t.Errorf("content mismatch: got %q, want %q", name, "MANIFEST-000001")
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Errorf("expected trailing newline in raw bytes")
	}

	// Verify CURRENT.tmp does not linger
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp must not exist after successful creation")
	}
}

func TestSetCurrentManifest_AtomicReplacement(t *testing.T) {
	dir := t.TempDir()

	// Initial pointer: MANIFEST-000001
	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("initial SetCurrentManifest failed: %v", err)
	}
	name1, _ := readCurrentForTesting(t, dir)
	if name1 != "MANIFEST-000001" {
		t.Fatalf("initial mismatch: got %q", name1)
	}

	// Atomic replacement: MANIFEST-000002
	if err := SetCurrentManifest(dir, 2); err != nil {
		t.Fatalf("second SetCurrentManifest failed: %v", err)
	}
	name2, raw2 := readCurrentForTesting(t, dir)
	if name2 != "MANIFEST-000002" {
		t.Fatalf("updated mismatch: got %q, want %q", name2, "MANIFEST-000002")
	}
	if len(raw2) != 16 {
		t.Errorf("expected exactly 16 bytes for MANIFEST-000002\\n, got %d", len(raw2))
	}

	// Verify CURRENT.tmp does not exist
	if _, err := os.Stat(filepath.Join(dir, CurrentTempFilename)); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp must not exist after successful replacement")
	}
}

func TestSetCurrentManifest_RepeatedUpdates_50Times(t *testing.T) {
	dir := t.TempDir()

	const numUpdates = 50
	for i := uint64(1); i <= numUpdates; i++ {
		if err := SetCurrentManifest(dir, i); err != nil {
			t.Fatalf("SetCurrentManifest failed at iteration %d: %v", i, err)
		}

		expected := fmt.Sprintf("MANIFEST-%06d", i)
		name, raw := readCurrentForTesting(t, dir)
		if name != expected {
			t.Fatalf("iteration %d content mismatch: got %q, want %q", i, name, expected)
		}
		if string(raw) != expected+"\n" {
			t.Fatalf("iteration %d raw mismatch: got %q, want %q", i, string(raw), expected+"\n")
		}
	}

	// Final verification of 50th pointer
	finalName, _ := readCurrentForTesting(t, dir)
	if finalName != "MANIFEST-000050" {
		t.Errorf("final pointer mismatch: got %q, want MANIFEST-000050", finalName)
	}
}

// -----------------------------------------------------------------------------
// FAULT INJECTION & CRASH SAFETY TESTS (§20 & §28)
// -----------------------------------------------------------------------------

func TestSetCurrentManifest_FaultInjection_WriteError(t *testing.T) {
	dir := t.TempDir()

	// 1. Establish initial valid CURRENT pointer
	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	// 2. Inject write failure
	injectedErr := stdErrors.New("simulated disk full during write")
	restore := SetCurrentWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, injectedErr
	})
	defer restore()

	// 3. Attempt update to manifest 2
	err := SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail under write error")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping %v, got %v", injectedErr, err)
	}

	// 4. CRASH-SAFETY INVARIANT: Prior CURRENT must remain completely untouched!
	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000001" {
		t.Fatalf("CRITICAL: CURRENT was modified after write failure! got %q, want MANIFEST-000001", name)
	}

	// 5. Temporary file must be unlinked
	if _, err := os.Stat(filepath.Join(dir, CurrentTempFilename)); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp must be removed on write failure")
	}
}

func TestSetCurrentManifest_FaultInjection_ShortWrite(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	// Inject 0-byte write
	restore := SetCurrentWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, nil
	})
	defer restore()

	err := SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail on short write")
	}
	if !stdErrors.Is(err, io.ErrShortWrite) {
		t.Errorf("expected error wrapping io.ErrShortWrite, got %v", err)
	}

	// Verify old CURRENT intact
	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000001" {
		t.Fatalf("CURRENT was modified after short write! got %q", name)
	}
	if _, err := os.Stat(filepath.Join(dir, CurrentTempFilename)); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp must be cleaned up on short write")
	}
}

func TestSetCurrentManifest_FaultInjection_SyncError(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	// Inject fdatasync failure
	injectedErr := stdErrors.New("simulated I/O sync failure")
	restore := SetCurrentSyncFnForTesting(func(f *os.File) error {
		return injectedErr
	})
	defer restore()

	err := SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail on sync failure")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping %v, got %v", injectedErr, err)
	}

	// Verify old CURRENT intact
	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000001" {
		t.Fatalf("CURRENT was modified after sync failure! got %q", name)
	}
	if _, err := os.Stat(filepath.Join(dir, CurrentTempFilename)); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp must be cleaned up on sync failure")
	}
}

func TestSetCurrentManifest_FaultInjection_CloseError(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	// Inject close failure
	injectedErr := stdErrors.New("simulated descriptor close failure")
	restore := SetCurrentCloseFnForTesting(func(f *os.File) error {
		_ = f.Close() // prevent fd leak
		return injectedErr
	})
	defer restore()

	err := SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail on close failure")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping %v, got %v", injectedErr, err)
	}

	// Verify old CURRENT intact
	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000001" {
		t.Fatalf("CURRENT was modified after close failure! got %q", name)
	}
}

func TestSetCurrentManifest_FaultInjection_RenameError(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	// Inject rename failure
	injectedErr := stdErrors.New("simulated rename I/O error")
	restore := SetCurrentRenameFnForTesting(func(oldpath, newpath string) error {
		return injectedErr
	})
	defer restore()

	err := SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail on rename error")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping %v, got %v", injectedErr, err)
	}

	// Verify old CURRENT intact
	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000001" {
		t.Fatalf("CURRENT was modified after rename failure! got %q", name)
	}
	if _, err := os.Stat(filepath.Join(dir, CurrentTempFilename)); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp must be removed on rename failure")
	}
}

func TestSetCurrentManifest_FaultInjection_DirectorySyncError(t *testing.T) {
	dir := t.TempDir()

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("setup SetCurrentManifest failed: %v", err)
	}

	// Inject parent directory sync failure
	injectedErr := stdErrors.New("simulated parent directory sync failure")
	restore := SetCurrentSyncDirFnForTesting(func(dirPath string) error {
		return injectedErr
	})
	defer restore()

	err := SetCurrentManifest(dir, 2)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to fail on directory sync error")
	}
	if !stdErrors.Is(err, errors.ErrCurrentDirectorySync) {
		t.Errorf("expected error wrapping ErrCurrentDirectorySync, got %v", err)
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping root cause %v, got %v", injectedErr, err)
	}

	// Note on Post-Rename State:
	// Because rename already succeeded, CURRENT was placed at the destination.
	// The directory sync error indicates durability is uncertain, but the pointer
	// must NOT be deleted or left in a partial state.
	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000002" {
		t.Errorf("expected CURRENT to contain MANIFEST-000002 after rename succeeded, got %q", name)
	}
}

// -----------------------------------------------------------------------------
// STALE TEMPORARY FILE CLEANUP (§22)
// -----------------------------------------------------------------------------

func TestSetCurrentManifest_StaleTempFile_Replaced(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, CurrentTempFilename)

	// Simulate stale temporary file left by an earlier crashed process
	staleContent := []byte("CORRUPTED_STALE_TMP_FILE_DATA")
	if err := os.WriteFile(tmpPath, staleContent, 0600); err != nil {
		t.Fatalf("failed to create stale tmp file: %v", err)
	}

	// SetCurrentManifest must safely purge the stale regular file and succeed
	if err := SetCurrentManifest(dir, 5); err != nil {
		t.Fatalf("SetCurrentManifest failed with stale tmp file: %v", err)
	}

	name, _ := readCurrentForTesting(t, dir)
	if name != "MANIFEST-000005" {
		t.Errorf("content mismatch: got %q, want MANIFEST-000005", name)
	}

	// Verify temporary file is gone
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp still exists after replacement")
	}
}

// -----------------------------------------------------------------------------
// SYMLINK & DIRECTORY DEFENSE (§12 & §38)
// -----------------------------------------------------------------------------

func TestSetCurrentManifest_SymlinkDefense_Target(t *testing.T) {
	dir := t.TempDir()
	victimFile := filepath.Join(dir, "victim.txt")
	victimContent := []byte("CRITICAL_FILE_CONTENT")
	if err := os.WriteFile(victimFile, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victim file: %v", err)
	}

	currentPath := filepath.Join(dir, CurrentFilename)
	if err := os.Symlink(victimFile, currentPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// Attempt SetCurrentManifest when CURRENT is a symlink
	err := SetCurrentManifest(dir, 1)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to reject symlink target")
	}
	if !stdErrors.Is(err, errors.ErrCurrentSymlink) {
		t.Errorf("expected ErrCurrentSymlink, got %v", err)
	}

	// Victim file must NOT be overwritten!
	content, _ := os.ReadFile(victimFile)
	if !bytes.Equal(content, victimContent) {
		t.Fatalf("SECURITY VIOLATION: victim file was overwritten through symlink!")
	}
}

func TestSetCurrentManifest_SymlinkDefense_TempFile(t *testing.T) {
	dir := t.TempDir()
	victimFile := filepath.Join(dir, "victim.txt")
	victimContent := []byte("CRITICAL_FILE_CONTENT")
	if err := os.WriteFile(victimFile, victimContent, 0600); err != nil {
		t.Fatalf("failed to write victim file: %v", err)
	}

	tmpPath := filepath.Join(dir, CurrentTempFilename)
	if err := os.Symlink(victimFile, tmpPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// Attempt SetCurrentManifest when CURRENT.tmp is a symlink
	err := SetCurrentManifest(dir, 1)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to reject symlink tmp file")
	}
	if !stdErrors.Is(err, errors.ErrCurrentSymlink) {
		t.Errorf("expected ErrCurrentSymlink, got %v", err)
	}

	// Victim file must NOT be modified!
	content, _ := os.ReadFile(victimFile)
	if !bytes.Equal(content, victimContent) {
		t.Fatalf("SECURITY VIOLATION: victim file was overwritten through tmp symlink!")
	}
}

func TestSetCurrentManifest_SymlinkDefense_Directory(t *testing.T) {
	realDir := t.TempDir()
	parentDir := t.TempDir()
	symlinkedDir := filepath.Join(parentDir, "symlinked_db")

	if err := os.Symlink(realDir, symlinkedDir); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// Passing a symlinked directory must be rejected
	err := SetCurrentManifest(symlinkedDir, 1)
	if err == nil {
		t.Fatalf("expected SetCurrentManifest to reject symlinked directory path")
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid for symlinked directory, got %v", err)
	}
}

func TestSetCurrentManifest_DirectoryDefense(t *testing.T) {
	dir := t.TempDir()

	// 1. Target CURRENT is a directory
	currentPath := filepath.Join(dir, CurrentFilename)
	if err := os.Mkdir(currentPath, 0700); err != nil {
		t.Fatalf("failed to create directory at CURRENT: %v", err)
	}
	err := SetCurrentManifest(dir, 1)
	if err == nil {
		t.Fatalf("expected error when CURRENT is a directory")
	}
	var notADirErr *errors.NotADirectoryError
	if !stdErrors.As(err, &notADirErr) {
		t.Errorf("expected *errors.NotADirectoryError, got %v", err)
	}
	_ = os.Remove(currentPath)

	// 2. Staging CURRENT.tmp is a directory
	tmpPath := filepath.Join(dir, CurrentTempFilename)
	if err := os.Mkdir(tmpPath, 0700); err != nil {
		t.Fatalf("failed to create directory at CURRENT.tmp: %v", err)
	}
	err = SetCurrentManifest(dir, 1)
	if err == nil {
		t.Fatalf("expected error when CURRENT.tmp is a directory")
	}
	if !stdErrors.As(err, &notADirErr) {
		t.Errorf("expected *errors.NotADirectoryError for CURRENT.tmp directory, got %v", err)
	}
	_ = os.Remove(tmpPath)

	// 3. dir is a regular file, not a directory
	filePath := filepath.Join(dir, "some_file")
	if err := os.WriteFile(filePath, []byte("data"), 0600); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	err = SetCurrentManifest(filePath, 1)
	if err == nil {
		t.Fatalf("expected error when dir is a regular file")
	}
	if !stdErrors.As(err, &notADirErr) {
		t.Errorf("expected *errors.NotADirectoryError when dir is a file, got %v", err)
	}
}

func TestSetCurrentManifest_InvalidArguments(t *testing.T) {
	dir := t.TempDir()

	// Empty directory path
	if err := SetCurrentManifest("", 1); err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid for empty dir, got %v", err)
	}

	// Manifest number = 0
	if err := SetCurrentManifest(dir, 0); err == nil || !stdErrors.Is(err, errors.ErrInvalidManifestNum) {
		t.Errorf("expected ErrInvalidManifestNum for manifest 0, got %v", err)
	}

	// Non-existent directory
	nonExistent := filepath.Join(dir, "does_not_exist")
	if err := SetCurrentManifest(nonExistent, 1); err == nil {
		t.Errorf("expected error for non-existent directory")
	}
}

func TestSetCurrentManifest_LargeManifestNumbers(t *testing.T) {
	dir := t.TempDir()

	// 7-digit manifest number (1,000,000)
	if err := SetCurrentManifest(dir, 1000000); err != nil {
		t.Fatalf("SetCurrentManifest(1000000) failed: %v", err)
	}
	name, raw := readCurrentForTesting(t, dir)
	if name != "MANIFEST-1000000" {
		t.Errorf("expected MANIFEST-1000000, got %q", name)
	}
	if string(raw) != "MANIFEST-1000000\n" {
		t.Errorf("raw mismatch: got %q, want MANIFEST-1000000\\n", string(raw))
	}

	// Max uint64
	if err := SetCurrentManifest(dir, math.MaxUint64); err != nil {
		t.Fatalf("SetCurrentManifest(MaxUint64) failed: %v", err)
	}
	expectedMax := fmt.Sprintf("MANIFEST-%06d", uint64(math.MaxUint64))
	nameMax, rawMax := readCurrentForTesting(t, dir)
	if nameMax != expectedMax {
		t.Errorf("expected %s, got %q", expectedMax, nameMax)
	}
	if string(rawMax) != expectedMax+"\n" {
		t.Errorf("raw mismatch: got %q, want %s\\n", string(rawMax), expectedMax)
	}
}

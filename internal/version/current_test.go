package version

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
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

// -----------------------------------------------------------------------------
// P06-S02-M02: CURRENT POINTER READER & VALIDATION TESTS
// -----------------------------------------------------------------------------

func TestParseCurrentManifest_ExactByteFixtures(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		expected uint64
	}{
		{
			name: "Manifest-000001",
			raw: []byte{
				'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', '-',
				'0', '0', '0', '0', '0', '1', '\n',
			},
			expected: 1,
		},
		{
			name: "Manifest-000002",
			raw: []byte{
				'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', '-',
				'0', '0', '0', '0', '0', '2', '\n',
			},
			expected: 2,
		},
		{
			name: "Manifest-000042",
			raw: []byte{
				'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', '-',
				'0', '0', '0', '0', '4', '2', '\n',
			},
			expected: 42,
		},
		{
			name: "Manifest-999999",
			raw: []byte{
				'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', '-',
				'9', '9', '9', '9', '9', '9', '\n',
			},
			expected: 999999,
		},
		{
			name: "Manifest-1000000",
			raw: []byte{
				'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', '-',
				'1', '0', '0', '0', '0', '0', '0', '\n',
			},
			expected: 1000000,
		},
		{
			name:     "Manifest-MaxUint64",
			raw:      []byte("MANIFEST-18446744073709551615\n"),
			expected: math.MaxUint64,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCurrentManifest(tc.raw)
			if err != nil {
				t.Fatalf("ParseCurrentManifest failed unexpectedly: %v", err)
			}
			if got != tc.expected {
				t.Errorf("manifest sequence mismatch: got %d, want %d", got, tc.expected)
			}
		})
	}
}

func TestParseCurrentManifest_CorruptionMatrix(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		// Prefix corruption
		{"Prefix-X", "XMANIFEST-000001\n"},
		{"Prefix-Short", "MANIFES-000001\n"},
		{"Prefix-Lower", "manifest-000001\n"},
		{"Prefix-Underscore", "MANIFEST_000001\n"},
		{"Prefix-LeadingSpace", " MANIFEST-000001\n"},
		{"Prefix-Empty", ""},
		{"Prefix-Only", "MANIFEST-"},
		{"Prefix-OnlyWithNewline", "MANIFEST-\n"},

		// Number corruption
		{"Number-Alpha", "MANIFEST-ABCDEF\n"},
		{"Number-Hex", "MANIFEST-00000A\n"},
		{"Number-Negative", "MANIFEST--000001\n"},
		{"Number-PlusSign", "MANIFEST-+000001\n"},
		{"Number-InternalSpace", "MANIFEST-0000 1\n"},
		{"Number-Period", "MANIFEST-0000.1\n"},

		// Whitespace corruption
		{"Whitespace-TrailingSpaceBeforeNewline", "MANIFEST-000001 \n"},
		{"Whitespace-TrailingSpace", "MANIFEST-000001 "},
		{"Whitespace-Tab", "MANIFEST-000001\t\n"},
		{"Whitespace-TabNoNewline", "MANIFEST-000001\t"},
		{"Whitespace-InternalSpace", "MANIFEST- 000001\n"},
		{"Whitespace-DoubleNewline", "MANIFEST-000001\n\n"},

		// Newline corruption
		{"Newline-Missing", "MANIFEST-000001"},
		{"Newline-CRLF", "MANIFEST-000001\r\n"},
		{"Newline-CR", "MANIFEST-000001\r"},
		{"Newline-NullByte", "MANIFEST-000001\x00"},
		{"Newline-TrailingNull", "MANIFEST-000001\n\x00"},

		// Numeric boundary & non-canonical cases
		{"Boundary-Zero", "MANIFEST-000000\n"},
		{"Boundary-TooFewDigits1", "MANIFEST-1\n"},
		{"Boundary-TooFewDigits4", "MANIFEST-0001\n"},
		{"Boundary-TooFewDigits5", "MANIFEST-00001\n"},
		{"Boundary-SuperfluousZero1", "MANIFEST-0000001\n"},
		{"Boundary-SuperfluousZeroMillion", "MANIFEST-01000000\n"},
		{"Boundary-Uint64Overflow", "MANIFEST-18446744073709551616\n"},
		{"Boundary-21DigitsOverflow", "MANIFEST-999999999999999999999\n"},
		{"Boundary-21DigitsPadded", "MANIFEST-184467440737095516150\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCurrentManifest([]byte(tc.raw))
			if err == nil {
				t.Fatalf("expected error for malformed input %q, got manifest %d", tc.raw, got)
			}
			if !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
				t.Errorf("expected ErrCurrentCorrupted in chain for %q, got: %v", tc.raw, err)
			}
		})
	}
}

func TestParseCurrentManifest_OneByteMutations(t *testing.T) {
	canonical := []byte("MANIFEST-000042\n")

	for i := 0; i < len(canonical); i++ {
		orig := canonical[i]

		// Mutate to an invalid character
		mutated := byte('X')
		if orig == 'X' {
			mutated = 'Y'
		}
		if orig == '\n' {
			mutated = ' '
		}

		copied := append([]byte(nil), canonical...)
		copied[i] = mutated

		t.Run(fmt.Sprintf("offset_%d_byte_%c", i, mutated), func(t *testing.T) {
			got, err := ParseCurrentManifest(copied)
			if err == nil {
				t.Fatalf("expected mutation at offset %d to fail, got %d", i, got)
			}
			if !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
				t.Errorf("expected ErrCurrentCorrupted for mutated byte at offset %d, got: %v", i, err)
			}
		})
	}
}

func TestReadCurrentManifest_RoundTrip(t *testing.T) {
	testNumbers := []uint64{
		1,
		2,
		42,
		999999,
		1000000,
		math.MaxUint64,
	}

	for _, num := range testNumbers {
		t.Run(fmt.Sprintf("ManifestNum_%d", num), func(t *testing.T) {
			dir := t.TempDir()

			if err := SetCurrentManifest(dir, num); err != nil {
				t.Fatalf("SetCurrentManifest failed: %v", err)
			}

			got, err := ReadCurrentManifest(dir)
			if err != nil {
				t.Fatalf("ReadCurrentManifest failed: %v", err)
			}
			if got != num {
				t.Errorf("round-trip mismatch: got %d, want %d", got, num)
			}
		})
	}
}

func TestReadCurrentManifest_RepeatedUpdates(t *testing.T) {
	dir := t.TempDir()

	for i := uint64(1); i <= 50; i++ {
		if err := SetCurrentManifest(dir, i); err != nil {
			t.Fatalf("SetCurrentManifest(%d) failed: %v", i, err)
		}

		got, err := ReadCurrentManifest(dir)
		if err != nil {
			t.Fatalf("ReadCurrentManifest(%d) failed: %v", i, err)
		}
		if got != i {
			t.Fatalf("read after update %d mismatch: got %d", i, got)
		}
	}
}

func TestReadCurrentManifest_Missing(t *testing.T) {
	dir := t.TempDir()

	got, err := ReadCurrentManifest(dir)
	if err == nil {
		t.Fatalf("expected error reading non-existent CURRENT, got manifest %d", got)
	}
	if !stdErrors.Is(err, errors.ErrCurrentNotFound) {
		t.Errorf("expected ErrCurrentNotFound, got: %v", err)
	}
	if !stdErrors.Is(err, os.ErrNotExist) {
		t.Errorf("expected stdErrors.Is(err, os.ErrNotExist) to be true, got: %v", err)
	}
}

func TestReadCurrentManifest_SymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "some_target")
	if err := os.WriteFile(targetFile, []byte("MANIFEST-000001\n"), 0600); err != nil {
		t.Fatalf("failed to write target file: %v", err)
	}

	currentPath := filepath.Join(dir, CurrentFilename)
	if err := os.Symlink(targetFile, currentPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	got, err := ReadCurrentManifest(dir)
	if err == nil {
		t.Fatalf("expected error reading symlinked CURRENT, got manifest %d", got)
	}
	if !stdErrors.Is(err, errors.ErrCurrentSymlink) {
		t.Errorf("expected ErrCurrentSymlink, got: %v", err)
	}
}

func TestReadCurrentManifest_SymlinkDir(t *testing.T) {
	realDir := t.TempDir()
	if err := SetCurrentManifest(realDir, 1); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	symlinkDir := filepath.Join(t.TempDir(), "symlink_dir")
	if err := os.Symlink(realDir, symlinkDir); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	got, err := ReadCurrentManifest(symlinkDir)
	if err == nil {
		t.Fatalf("expected error for symlinked directory, got manifest %d", got)
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid for symlinked directory, got: %v", err)
	}
}

func TestReadCurrentManifest_DirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, CurrentFilename)
	if err := os.Mkdir(currentPath, 0700); err != nil {
		t.Fatalf("failed to create directory: %v", err)
	}

	got, err := ReadCurrentManifest(dir)
	if err == nil {
		t.Fatalf("expected error when CURRENT is a directory, got manifest %d", got)
	}
	var notDirErr *errors.NotADirectoryError
	if !stdErrors.As(err, &notDirErr) {
		t.Errorf("expected NotADirectoryError, got: %v", err)
	}
}

func TestReadCurrentManifest_SizeCeiling(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, CurrentFilename)

	// Too small (15 bytes)
	smallData := []byte("MANIFEST-00001\n")
	if err := os.WriteFile(currentPath, smallData, 0600); err != nil {
		t.Fatalf("failed to write small CURRENT: %v", err)
	}
	if _, err := ReadCurrentManifest(dir); !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
		t.Errorf("expected ErrCurrentCorrupted for 15-byte file, got: %v", err)
	}

	// Too large (31 bytes)
	largeData := []byte("MANIFEST-184467440737095516150\n")
	if err := os.WriteFile(currentPath, largeData, 0600); err != nil {
		t.Fatalf("failed to write large CURRENT: %v", err)
	}
	if _, err := ReadCurrentManifest(dir); !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
		t.Errorf("expected ErrCurrentCorrupted for 31-byte file, got: %v", err)
	}

	// 1 MB oversized file
	megaData := make([]byte, 1024*1024)
	copy(megaData, []byte("MANIFEST-000001\n"))
	if err := os.WriteFile(currentPath, megaData, 0600); err != nil {
		t.Fatalf("failed to write 1MB CURRENT: %v", err)
	}
	if _, err := ReadCurrentManifest(dir); !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
		t.Errorf("expected ErrCurrentCorrupted for 1MB file, got: %v", err)
	}
}

func TestReadCurrentManifest_TornWrite(t *testing.T) {
	tornCases := []struct {
		name string
		data []byte
	}{
		{"Truncated-14", []byte("MANIFEST-00000")},
		{"Truncated-12", []byte("MANIFEST-000")},
		{"NoNewline-15", []byte("MANIFEST-000001")},
		{"PartialExtra-23", []byte("MANIFEST-000001\npartial")},
	}

	for _, tc := range tornCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			currentPath := filepath.Join(dir, CurrentFilename)
			if err := os.WriteFile(currentPath, tc.data, 0600); err != nil {
				t.Fatalf("failed to write torn file: %v", err)
			}

			_, err := ReadCurrentManifest(dir)
			if err == nil {
				t.Fatalf("expected error for torn write %q, got success", tc.data)
			}
			if !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
				t.Errorf("expected ErrCurrentCorrupted for torn write, got: %v", err)
			}
		})
	}
}

func TestReadCurrentManifest_StaleTmpIgnored(t *testing.T) {
	dir := t.TempDir()

	// Write valid CURRENT pointing to 5
	if err := SetCurrentManifest(dir, 5); err != nil {
		t.Fatalf("SetCurrentManifest(5) failed: %v", err)
	}

	// Create stale / corrupt CURRENT.tmp
	tmpPath := filepath.Join(dir, CurrentTempFilename)
	if err := os.WriteFile(tmpPath, []byte("CORRUPTED_TMP_DATA"), 0600); err != nil {
		t.Fatalf("failed to write stale CURRENT.tmp: %v", err)
	}

	got, err := ReadCurrentManifest(dir)
	if err != nil {
		t.Fatalf("ReadCurrentManifest failed: %v", err)
	}
	if got != 5 {
		t.Errorf("expected valid manifest 5 despite stale tmp, got %d", got)
	}
}

func TestReadCurrentManifest_FaultInjection(t *testing.T) {
	dir := t.TempDir()
	if err := SetCurrentManifest(dir, 42); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	t.Run("OpenFailure", func(t *testing.T) {
		injectedErr := stdErrors.New("disk permission denied")
		restore := SetCurrentOpenFnForTesting(func(name string) (*os.File, error) {
			return nil, injectedErr
		})
		defer restore()

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatalf("expected open failure")
		}
		if !stdErrors.Is(err, injectedErr) {
			t.Errorf("expected injected error, got: %v", err)
		}
	})

	t.Run("ReadFailure", func(t *testing.T) {
		injectedErr := stdErrors.New("simulated I/O read failure")
		restore := SetCurrentReadFnForTesting(func(f *os.File, p []byte) (int, error) {
			return 0, injectedErr
		})
		defer restore()

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatalf("expected read failure")
		}
		if !stdErrors.Is(err, injectedErr) {
			t.Errorf("expected injected error, got: %v", err)
		}
	})

	t.Run("CloseFailure", func(t *testing.T) {
		injectedErr := stdErrors.New("simulated close failure")
		restore := SetCurrentCloseFnForTesting(func(f *os.File) error {
			_ = f.Close()
			return injectedErr
		})
		defer restore()

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatalf("expected close failure")
		}
		if !stdErrors.Is(err, injectedErr) {
			t.Errorf("expected injected error, got: %v", err)
		}
	})

	t.Run("LstatFailure", func(t *testing.T) {
		injectedErr := stdErrors.New("simulated lstat failure")
		restore := SetCurrentLstatFnForTesting(func(name string) (os.FileInfo, error) {
			if filepath.Base(name) == CurrentFilename {
				return nil, injectedErr
			}
			return os.Lstat(name)
		})
		defer restore()

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatalf("expected lstat failure")
		}
		if !stdErrors.Is(err, injectedErr) {
			t.Errorf("expected injected error, got: %v", err)
		}
	})
}

func TestReadCurrentManifest_ConcurrentReadWrite(t *testing.T) {
	dir := t.TempDir()

	// Initial pointer
	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("initial SetCurrentManifest failed: %v", err)
	}

	const updateCount = 100
	const numReaders = 4

	var wg sync.WaitGroup
	stopCh := make(chan struct{})

	// Writer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(2); i <= updateCount; i++ {
			if err := SetCurrentManifest(dir, i); err != nil {
				t.Errorf("concurrent SetCurrentManifest(%d) failed: %v", i, err)
				break
			}
		}
		close(stopCh)
	}()

	// Reader goroutines
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					manifestNum, err := ReadCurrentManifest(dir)
					if err != nil {
						t.Errorf("reader %d failed: %v", readerID, err)
						return
					}
					if manifestNum < 1 || manifestNum > updateCount {
						t.Errorf("reader %d observed out-of-range manifest number: %d", readerID, manifestNum)
						return
					}
				}
			}
		}(r)
	}

	wg.Wait()

	// Final verification: pointer must be at updateCount
	finalNum, err := ReadCurrentManifest(dir)
	if err != nil {
		t.Fatalf("final ReadCurrentManifest failed: %v", err)
	}
	if finalNum != updateCount {
		t.Errorf("expected final manifest number %d, got %d", updateCount, finalNum)
	}
}

func FuzzParseCurrentManifest(f *testing.F) {
	// Seed valid canonical examples
	f.Add([]byte("MANIFEST-000001\n"))
	f.Add([]byte("MANIFEST-000042\n"))
	f.Add([]byte("MANIFEST-999999\n"))
	f.Add([]byte("MANIFEST-1000000\n"))
	f.Add([]byte("MANIFEST-18446744073709551615\n"))

	// Seed invalid examples
	f.Add([]byte("MANIFEST-000000\n"))
	f.Add([]byte("MANIFEST-1\n"))
	f.Add([]byte("MANIFEST-0001\n"))
	f.Add([]byte("MANIFEST-000001"))
	f.Add([]byte("MANIFEST-000001\r\n"))
	f.Add([]byte("MANIFEST-18446744073709551616\n"))
	f.Add([]byte("MANIFEST-ABCDEF\n"))
	f.Add([]byte("manifest-000001\n"))
	f.Add([]byte("MANIFEST-0000001\n"))
	f.Add([]byte(""))
	f.Add([]byte("MANIFEST-"))

	f.Fuzz(func(t *testing.T, data []byte) {
		manifestNum, err := ParseCurrentManifest(data)
		if err == nil {
			// Invariant 1: manifest number must be non-zero
			if manifestNum == 0 {
				t.Fatalf("ParseCurrentManifest returned 0 without error for input: %q", data)
			}

			// Invariant 2: successfully parsed data must match exact canonical representation byte-for-byte
			expected := []byte(ManifestFilename(manifestNum) + "\n")
			if !bytes.Equal(data, expected) {
				t.Fatalf("ParseCurrentManifest accepted non-canonical input: got num %d, expected %q, was %q",
					manifestNum, expected, data)
			}
		}
	})
}

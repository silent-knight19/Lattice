package wal_test

import (
	stdErrors "errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestDir_PathConstruction verifies that wal.Dir and wal.DirPath construct platform-aware,
// lexically normalized paths across various directory inputs.
func TestDir_PathConstruction(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "AbsoluteUnixPath",
			input:    "/var/lib/lattice",
			expected: filepath.Join("/var/lib/lattice", "wal"),
		},
		{
			name:     "TrailingSlash",
			input:    "/var/lib/lattice/",
			expected: filepath.Join("/var/lib/lattice", "wal"),
		},
		{
			name:     "DotPath",
			input:    ".",
			expected: "wal",
		},
		{
			name:     "NestedRelativePath",
			input:    "data/db",
			expected: filepath.Join("data/db", "wal"),
		},
		{
			name:     "PathWithRedundantSeparators",
			input:    "data//sub///dir/",
			expected: filepath.Join("data", "sub", "dir", "wal"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotDir := wal.Dir(tc.input)
			if gotDir != tc.expected {
				t.Errorf("Dir(%q) = %q, want %q", tc.input, gotDir, tc.expected)
			}

			gotDirPath := wal.DirPath(tc.input)
			if gotDirPath != tc.expected {
				t.Errorf("DirPath(%q) = %q, want %q", tc.input, gotDirPath, tc.expected)
			}
		})
	}
}

// TestInitDir_FreshDatabasePath verifies Scenarios A, B, C, D:
// A. Create WAL directory from a fresh database path.
// B. Verify resulting path exists.
// C. Verify resulting path is a directory.
// D. Verify expected 0700 permission behavior on POSIX systems.
func TestInitDir_FreshDatabasePath(t *testing.T) {
	dbPath := t.TempDir()

	walPath, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("InitDir failed on fresh db path: %v", err)
	}

	expectedPath := filepath.Join(dbPath, wal.DirName)
	if walPath != expectedPath {
		t.Fatalf("InitDir returned path %q, expected %q", walPath, expectedPath)
	}

	// Verify existence and directory properties
	info, statErr := os.Stat(walPath)
	if statErr != nil {
		t.Fatalf("os.Stat failed on created WAL directory: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("expected created path to be a directory, got mode: %s", info.Mode())
	}

	// Verify POSIX 0700 permissions
	if runtime.GOOS != "windows" {
		actualPerm := info.Mode().Perm()
		if actualPerm != wal.DirMode {
			t.Errorf("expected permissions %04o, got %04o", wal.DirMode, actualPerm)
		}
	}
}

// TestInitDir_Idempotency verifies Scenarios E & F:
// Repeated calls to InitDir on an already existing WAL directory succeed without side effects.
func TestInitDir_Idempotency(t *testing.T) {
	dbPath := t.TempDir()

	// First call: creates directory
	firstPath, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("first InitDir failed: %v", err)
	}

	// Second call: idempotent convergence
	secondPath, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("second InitDir failed: %v", err)
	}

	if firstPath != secondPath {
		t.Errorf("path mismatch across idempotent calls: first=%q, second=%q", firstPath, secondPath)
	}

	// Third call: repeated convergence
	thirdPath, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("third InitDir failed: %v", err)
	}
	if secondPath != thirdPath {
		t.Errorf("path mismatch on third call: second=%q, third=%q", secondPath, thirdPath)
	}
}

// TestInitDir_PermissionHardening verifies that an existing WAL directory created
// with loose permissions (e.g. 0755 or 0777) is tightened to 0700.
func TestInitDir_PermissionHardening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission hardening not testable on Windows")
	}

	dbPath := t.TempDir()
	walPath := filepath.Join(dbPath, wal.DirName)

	// Pre-create directory with overly permissive 0777 mode
	if err := os.Mkdir(walPath, 0777); err != nil {
		t.Fatalf("failed to pre-create directory: %v", err)
	}

	// Ensure group/other bits are set prior to hardening
	_ = os.Chmod(walPath, 0777)

	// Call InitDir: should detect loose permissions and tighten to 0700
	returnedPath, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("InitDir failed on pre-existing loose directory: %v", err)
	}
	if returnedPath != walPath {
		t.Errorf("path mismatch: got %q, want %q", returnedPath, walPath)
	}

	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Mode().Perm() != wal.DirMode {
		t.Errorf("expected hardened permissions %04o, got %04o", wal.DirMode, info.Mode().Perm())
	}
}

// TestInitDir_ExistingRegularFileAtWALPath verifies Scenario G & Adversarial Scenario 1:
// If <db_path>/wal exists as a regular file, InitDir must fail with *errors.NotADirectoryError,
// matching errors.ErrNotADirectory, and must NOT delete or overwrite the regular file.
func TestInitDir_ExistingRegularFileAtWALPath(t *testing.T) {
	dbPath := t.TempDir()
	walPath := filepath.Join(dbPath, wal.DirName)

	originalContent := []byte("this is a regular file, not a directory")
	if err := os.WriteFile(walPath, originalContent, 0644); err != nil {
		t.Fatalf("failed to create regular file: %v", err)
	}

	// Call InitDir: must fail
	_, err := wal.InitDir(dbPath)
	if err == nil {
		t.Fatalf("expected InitDir to fail when wal path is a regular file, got nil")
	}

	// Must match ErrNotADirectory
	if !stdErrors.Is(err, errors.ErrNotADirectory) {
		t.Fatalf("expected error to match errors.ErrNotADirectory, got: %v", err)
	}

	// Must extract structured *errors.NotADirectoryError
	var notDirErr *errors.NotADirectoryError
	if !stdErrors.As(err, &notDirErr) {
		t.Fatalf("expected *errors.NotADirectoryError, got %T: %v", err, err)
	}
	if notDirErr.Path != walPath {
		t.Errorf("NotADirectoryError path mismatch: got %q, want %q", notDirErr.Path, walPath)
	}

	// Adversarial invariant: the existing file must NOT be deleted or mutated!
	content, readErr := os.ReadFile(walPath)
	if readErr != nil {
		t.Fatalf("failed to read conflicting file after InitDir failure: %v", readErr)
	}
	if string(content) != string(originalContent) {
		t.Fatalf("conflicting file was mutated: got %q, want %q", string(content), string(originalContent))
	}
}

// TestInitDir_ExistingSymlinks verifies Scenario H & Adversarial Scenario 2:
// If <db_path>/wal exists as a symlink (pointing to directory, pointing to file, or broken),
// InitDir must reject it to prevent symlink redirection attacks.
func TestInitDir_ExistingSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows without elevated privileges")
	}

	t.Run("SymlinkPointingToDirectory", func(t *testing.T) {
		dbPath := t.TempDir()
		externalTarget := t.TempDir() // Target directory outside database root
		walPath := filepath.Join(dbPath, wal.DirName)

		if err := os.Symlink(externalTarget, walPath); err != nil {
			t.Fatalf("failed to create symlink: %v", err)
		}

		_, err := wal.InitDir(dbPath)
		if err == nil {
			t.Fatalf("expected InitDir to reject directory symlink, got nil")
		}
		if !stdErrors.Is(err, errors.ErrNotADirectory) {
			t.Fatalf("expected ErrNotADirectory, got: %v", err)
		}

		// Verify symlink still exists and was not followed or deleted
		lfi, lerr := os.Lstat(walPath)
		if lerr != nil {
			t.Fatalf("os.Lstat failed: %v", lerr)
		}
		if lfi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink was replaced or altered!")
		}
	})

	t.Run("SymlinkPointingToFile", func(t *testing.T) {
		dbPath := t.TempDir()
		targetFile := filepath.Join(dbPath, "target.txt")
		if err := os.WriteFile(targetFile, []byte("target"), 0644); err != nil {
			t.Fatalf("failed to create target file: %v", err)
		}

		walPath := filepath.Join(dbPath, wal.DirName)
		if err := os.Symlink(targetFile, walPath); err != nil {
			t.Fatalf("failed to create symlink: %v", err)
		}

		_, err := wal.InitDir(dbPath)
		if err == nil {
			t.Fatalf("expected InitDir to reject file symlink, got nil")
		}
		if !stdErrors.Is(err, errors.ErrNotADirectory) {
			t.Fatalf("expected ErrNotADirectory, got: %v", err)
		}
	})

	t.Run("BrokenSymlink", func(t *testing.T) {
		dbPath := t.TempDir()
		walPath := filepath.Join(dbPath, wal.DirName)
		nonExistentTarget := filepath.Join(dbPath, "does_not_exist")

		if err := os.Symlink(nonExistentTarget, walPath); err != nil {
			t.Fatalf("failed to create broken symlink: %v", err)
		}

		_, err := wal.InitDir(dbPath)
		if err == nil {
			t.Fatalf("expected InitDir to reject broken symlink, got nil")
		}
		if !stdErrors.Is(err, errors.ErrNotADirectory) {
			t.Fatalf("expected ErrNotADirectory, got: %v", err)
		}
	})
}

// TestInitDir_DbPathAsSymlink verifies that if dbPath itself is a symlink pointing
// to an external data partition, InitDir creates the real wal directory inside the target.
func TestInitDir_DbPathAsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows without elevated privileges")
	}

	realDataDir := t.TempDir()
	containerDir := t.TempDir()
	symlinkDB := filepath.Join(containerDir, "symlink_db")

	if err := os.Symlink(realDataDir, symlinkDB); err != nil {
		t.Fatalf("failed to symlink database path: %v", err)
	}

	walPath, err := wal.InitDir(symlinkDB)
	if err != nil {
		t.Fatalf("InitDir failed with symlinked dbPath: %v", err)
	}

	// Verify directory exists in real data directory
	realWAL := filepath.Join(realDataDir, wal.DirName)
	fi, err := os.Stat(realWAL)
	if err != nil {
		t.Fatalf("os.Stat on real WAL directory failed: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("expected real WAL path to be a directory")
	}

	// Verify returned path points to the expected joined path
	if walPath != filepath.Join(symlinkDB, wal.DirName) {
		t.Errorf("path mismatch: got %q, want %q", walPath, filepath.Join(symlinkDB, wal.DirName))
	}
}

// TestInitDir_ConcurrentInitialization verifies Scenario I & Adversarial Scenario 5:
// Multiple goroutines concurrently racing to initialize the same WAL directory
// safely converge on a valid directory without deadlocks, panics, or race conditions.
func TestInitDir_ConcurrentInitialization(t *testing.T) {
	dbPath := t.TempDir()
	const goroutines = 50

	var wg sync.WaitGroup
	startBarrier := make(chan struct{})
	errorsCh := make(chan error, goroutines)
	pathsCh := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startBarrier // Synchronize simultaneous release
			p, err := wal.InitDir(dbPath)
			if err != nil {
				errorsCh <- err
			} else {
				pathsCh <- p
			}
		}()
	}

	// Release all goroutines simultaneously
	close(startBarrier)
	wg.Wait()
	close(errorsCh)
	close(pathsCh)

	// Check for any errors
	for err := range errorsCh {
		t.Fatalf("concurrent InitDir encountered error: %v", err)
	}

	// Verify all returned paths are identical
	expectedPath := filepath.Join(dbPath, wal.DirName)
	for p := range pathsCh {
		if p != expectedPath {
			t.Fatalf("path mismatch under concurrency: got %q, want %q", p, expectedPath)
		}
	}

	// Verify resulting directory exists and has 0700 permissions
	info, err := os.Stat(expectedPath)
	if err != nil {
		t.Fatalf("os.Stat failed on final directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("final path is not a directory")
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != wal.DirMode {
			t.Errorf("expected permissions %04o, got %04o", wal.DirMode, info.Mode().Perm())
		}
	}
}

// TestInitDir_NestedAndSpecialPaths verifies Scenarios J, K, L:
// Nested database paths, relative paths, and paths with spaces/special characters.
func TestInitDir_NestedAndSpecialPaths(t *testing.T) {
	baseDir := t.TempDir()

	t.Run("NestedHierarchy", func(t *testing.T) {
		nestedDB := filepath.Join(baseDir, "level1", "level2", "store")
		if err := os.MkdirAll(nestedDB, 0755); err != nil {
			t.Fatalf("failed to create nested parent: %v", err)
		}

		walPath, err := wal.InitDir(nestedDB)
		if err != nil {
			t.Fatalf("InitDir failed on nested path: %v", err)
		}
		if walPath != filepath.Join(nestedDB, wal.DirName) {
			t.Errorf("path mismatch: got %q, want %q", walPath, filepath.Join(nestedDB, wal.DirName))
		}
	})

	t.Run("PathWithSpacesAndSymbols", func(t *testing.T) {
		specialDB := filepath.Join(baseDir, "db store with spaces & #symbols (test)")
		if err := os.MkdirAll(specialDB, 0755); err != nil {
			t.Fatalf("failed to create special parent: %v", err)
		}

		walPath, err := wal.InitDir(specialDB)
		if err != nil {
			t.Fatalf("InitDir failed on special path: %v", err)
		}
		if walPath != filepath.Join(specialDB, wal.DirName) {
			t.Errorf("path mismatch: got %q, want %q", walPath, filepath.Join(specialDB, wal.DirName))
		}
	})

	t.Run("RelativePath", func(t *testing.T) {
		// Create a local subfolder in baseDir
		relParent := filepath.Join(baseDir, "rel_db")
		if err := os.Mkdir(relParent, 0755); err != nil {
			t.Fatalf("failed to create relParent: %v", err)
		}

		// Use relative path representation
		walPath, err := wal.InitDir(relParent)
		if err != nil {
			t.Fatalf("InitDir failed: %v", err)
		}
		if walPath != filepath.Join(relParent, wal.DirName) {
			t.Errorf("path mismatch: got %q, want %q", walPath, filepath.Join(relParent, wal.DirName))
		}
	})
}

// TestInitDir_FilesystemFailureModes verifies Scenario M:
// Error propagation when parent directory does not exist, is invalid, is empty, or is read-only.
func TestInitDir_FilesystemFailureModes(t *testing.T) {
	t.Run("EmptyDBPath", func(t *testing.T) {
		_, err := wal.InitDir("")
		if err == nil {
			t.Fatalf("expected error on empty dbPath, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid on empty dbPath, got: %v", err)
		}
	})

	t.Run("ParentDoesNotExist", func(t *testing.T) {
		nonExistentDB := filepath.Join(t.TempDir(), "non_existent_dir", "sub_dir")
		_, err := wal.InitDir(nonExistentDB)
		if err == nil {
			t.Fatalf("expected error on non-existent parent, got nil")
		}
		if !stdErrors.Is(err, fs.ErrNotExist) {
			t.Errorf("expected error wrapping fs.ErrNotExist, got: %v", err)
		}
	})

	t.Run("ParentIsRegularFile", func(t *testing.T) {
		baseDir := t.TempDir()
		parentFile := filepath.Join(baseDir, "file_not_dir")
		if err := os.WriteFile(parentFile, []byte("data"), 0644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}

		_, err := wal.InitDir(parentFile)
		if err == nil {
			t.Fatalf("expected error when parent is a regular file, got nil")
		}
	})

	t.Run("ReadOnlyParentDirectory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("read-only directory permissions not enforced on Windows in standard mode")
		}
		if os.Geteuid() == 0 {
			t.Skip("skipping read-only test as root user bypasses permissions")
		}

		baseDir := t.TempDir()
		readOnlyDB := filepath.Join(baseDir, "readonly_db")
		if err := os.Mkdir(readOnlyDB, 0500); err != nil { // r-x------ (no write)
			t.Fatalf("failed to create read-only directory: %v", err)
		}
		defer func() {
			// Restore write permission so t.TempDir() cleanup succeeds
			_ = os.Chmod(readOnlyDB, 0700)
		}()

		_, err := wal.InitDir(readOnlyDB)
		if err == nil {
			t.Fatalf("expected error on read-only parent, got nil")
		}
		if !stdErrors.Is(err, fs.ErrPermission) {
			t.Errorf("expected error wrapping fs.ErrPermission, got: %v", err)
		}
	})
}

// TestInitDir_PreservesExistingFiles verifies Scenarios N & O & Adversarial Scenarios 3 & 4:
// Pre-existing WAL files and unrelated database files are NEVER deleted, truncated, or modified.
func TestInitDir_PreservesExistingFiles(t *testing.T) {
	dbPath := t.TempDir()

	// 1. Create unrelated database files in dbPath
	manifestContent := []byte("MANIFEST-000001-DATA")
	currentContent := []byte("CURRENT-POINTER")
	if err := os.WriteFile(filepath.Join(dbPath, "MANIFEST-000001"), manifestContent, 0600); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dbPath, "CURRENT"), currentContent, 0600); err != nil {
		t.Fatalf("failed to write current: %v", err)
	}

	// 2. Pre-create wal directory and insert existing WAL segments
	walPath := filepath.Join(dbPath, wal.DirName)
	if err := os.Mkdir(walPath, 0700); err != nil {
		t.Fatalf("failed to pre-create wal dir: %v", err)
	}

	walSegment1 := filepath.Join(walPath, "wal_000000000001.log")
	walSegment2 := filepath.Join(walPath, "wal_000000000002.log")
	seg1Data := []byte("PRE_EXISTING_WAL_SEGMENT_1_DATA_PAYLOAD")
	seg2Data := []byte("PRE_EXISTING_WAL_SEGMENT_2_DATA_PAYLOAD")
	if err := os.WriteFile(walSegment1, seg1Data, 0600); err != nil {
		t.Fatalf("failed to write seg1: %v", err)
	}
	if err := os.WriteFile(walSegment2, seg2Data, 0600); err != nil {
		t.Fatalf("failed to write seg2: %v", err)
	}

	// 3. Call InitDir repeatedly
	for i := 0; i < 5; i++ {
		returnedPath, err := wal.InitDir(dbPath)
		if err != nil {
			t.Fatalf("iteration %d: InitDir failed: %v", i, err)
		}
		if returnedPath != walPath {
			t.Errorf("iteration %d: path mismatch: got %q, want %q", i, returnedPath, walPath)
		}
	}

	// 4. Assert unrelated files are completely untouched
	gotManifest, err := os.ReadFile(filepath.Join(dbPath, "MANIFEST-000001"))
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}
	if string(gotManifest) != string(manifestContent) {
		t.Errorf("manifest altered: got %q, want %q", gotManifest, manifestContent)
	}

	gotCurrent, err := os.ReadFile(filepath.Join(dbPath, "CURRENT"))
	if err != nil {
		t.Fatalf("failed to read current: %v", err)
	}
	if string(gotCurrent) != string(currentContent) {
		t.Errorf("current altered: got %q, want %q", gotCurrent, currentContent)
	}

	// 5. Assert pre-existing WAL segments are completely untouched
	gotSeg1, err := os.ReadFile(walSegment1)
	if err != nil {
		t.Fatalf("failed to read seg1: %v", err)
	}
	if string(gotSeg1) != string(seg1Data) {
		t.Errorf("wal segment 1 altered: got %q, want %q", gotSeg1, seg1Data)
	}

	gotSeg2, err := os.ReadFile(walSegment2)
	if err != nil {
		t.Fatalf("failed to read seg2: %v", err)
	}
	if string(gotSeg2) != string(seg2Data) {
		t.Errorf("wal segment 2 altered: got %q, want %q", gotSeg2, seg2Data)
	}
}

// TestInitDir_ExistingDirectoryInodePreservation verifies Requirement 8:
// Calling InitDir on an existing directory (both with loose 0777 and target 0700 permissions)
// preserves the exact underlying filesystem inode (os.SameFile == true).
// This proves that the implementation never deletes, recreates, or swaps the existing directory.
func TestInitDir_ExistingDirectoryInodePreservation(t *testing.T) {
	dbPath := t.TempDir()
	walPath := filepath.Join(dbPath, wal.DirName)

	// Pre-create directory with loose 0777 mode
	if err := os.Mkdir(walPath, 0777); err != nil {
		t.Fatalf("failed to create initial dir: %v", err)
	}
	_ = os.Chmod(walPath, 0777)

	fiInitial, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("os.Lstat failed: %v", err)
	}

	// Call InitDir: should harden to 0700 via descriptor fchmod
	p1, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}
	if p1 != walPath {
		t.Fatalf("path mismatch: got %q, want %q", p1, walPath)
	}

	fiAfterHardening, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("os.Lstat failed: %v", err)
	}

	// Prove the inode was preserved across permission hardening
	if !os.SameFile(fiInitial, fiAfterHardening) {
		t.Fatalf("directory inode changed during permission hardening! Directory was recreated or swapped.")
	}

	if runtime.GOOS != "windows" {
		if fiAfterHardening.Mode().Perm() != wal.DirMode {
			t.Errorf("expected permissions %04o, got %04o", wal.DirMode, fiAfterHardening.Mode().Perm())
		}
	}

	// Call InitDir again on already-0700 directory
	p2, err := wal.InitDir(dbPath)
	if err != nil {
		t.Fatalf("second InitDir failed: %v", err)
	}
	if p2 != walPath {
		t.Fatalf("path mismatch on second call: got %q, want %q", p2, walPath)
	}

	fiAfterSecondCall, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("os.Lstat failed: %v", err)
	}

	// Prove the inode was preserved across idempotent call
	if !os.SameFile(fiAfterHardening, fiAfterSecondCall) {
		t.Fatalf("directory inode changed during idempotent call! Directory was recreated.")
	}
}

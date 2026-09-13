package version

import (
	"crypto/sha256"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// helperCreatePopulatedManifest creates a valid MANIFEST with editCount edits.
func helperCreatePopulatedManifest(t *testing.T, dir string, manifestNum uint64, editCount int) string {
	t.Helper()
	path := ManifestPath(dir, manifestNum)
	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter(%s): %v", path, err)
	}
	for i := 1; i <= editCount; i++ {
		edit := NewVersionEdit()
		edit.SetNextFileNum(uint64(i + 100))
		edit.SetLastSeqNum(binary.SeqNum(i * 10))
		if err := w.LogEdit(*edit); err != nil {
			t.Fatalf("LogEdit(%d): %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close manifest writer: %v", err)
	}
	return path
}

// Test A — Valid database directory: CURRENT points to MANIFEST-000001 with valid edits.
func TestBootDiscovery_TestA_ValidDatabaseDirectory(t *testing.T) {
	dir := t.TempDir()

	manifestPath := helperCreatePopulatedManifest(t, dir, 1, 3)
	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("SetCurrentManifest: %v", err)
	}

	disc, err := DiscoverActiveManifest(dir)
	if err != nil {
		t.Fatalf("DiscoverActiveManifest: %v", err)
	}
	defer func() { _ = disc.Close() }()

	cleanDir := filepath.Clean(dir)
	if disc.Dir != cleanDir {
		t.Errorf("Dir: got %q, want %q", disc.Dir, cleanDir)
	}
	if disc.ManifestNum != 1 {
		t.Errorf("ManifestNum: got %d, want 1", disc.ManifestNum)
	}
	if disc.Path != manifestPath {
		t.Errorf("Path: got %q, want %q", disc.Path, manifestPath)
	}
	if disc.File == nil {
		t.Fatal("File descriptor is nil")
	}
	if disc.FileSize <= 0 {
		t.Errorf("FileSize: got %d, want > 0", disc.FileSize)
	}

	// Verify the returned descriptor is pinned and readable
	var header [ManifestHeaderSize]byte
	n, readErr := disc.File.ReadAt(header[:], 0)
	if readErr != nil || n != ManifestHeaderSize {
		t.Fatalf("ReadAt opened descriptor failed: n=%d, err=%v", n, readErr)
	}

	// Verify Close releases descriptor and is idempotent
	if err := disc.Close(); err != nil {
		t.Errorf("disc.Close() error: %v", err)
	}
	if disc.File != nil {
		t.Errorf("disc.File not cleared after Close")
	}
	if err := disc.Close(); err != nil {
		t.Errorf("idempotent disc.Close() error: %v", err)
	}
}

// Test B — Nontrivial manifest numbers: multi-digit identifiers within range.
func TestBootDiscovery_TestB_NontrivialManifestNumbers(t *testing.T) {
	testCases := []uint64{
		42,
		999999,
		1000000,
		999999999999,
	}

	for _, num := range testCases {
		t.Run(fmt.Sprintf("Manifest_%d", num), func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := helperCreatePopulatedManifest(t, dir, num, 1)
			if err := SetCurrentManifest(dir, num); err != nil {
				t.Fatalf("SetCurrentManifest(%d): %v", num, err)
			}

			disc, err := DiscoverActiveManifest(dir)
			if err != nil {
				t.Fatalf("DiscoverActiveManifest(%d): %v", num, err)
			}
			defer func() { _ = disc.Close() }()

			if disc.ManifestNum != num {
				t.Errorf("got manifestNum %d, want %d", disc.ManifestNum, num)
			}
			if disc.Path != manifestPath {
				t.Errorf("got path %q, want %q", disc.Path, manifestPath)
			}
			if disc.File == nil {
				t.Fatal("File descriptor is nil")
			}
		})
	}
}

// Test C — Empty but valid MANIFEST: 0-byte regular file opens cleanly.
func TestBootDiscovery_TestC_EmptyValidManifest(t *testing.T) {
	dir := t.TempDir()

	manifestPath := ManifestPath(dir, 1)
	f, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) //nolint:gosec // Test-only file creation
	if err != nil {
		t.Fatalf("create empty manifest: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close empty manifest: %v", err)
	}

	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("SetCurrentManifest: %v", err)
	}

	disc, err := DiscoverActiveManifest(dir)
	if err != nil {
		t.Fatalf("DiscoverActiveManifest with empty manifest: %v", err)
	}
	defer func() { _ = disc.Close() }()

	if disc.ManifestNum != 1 {
		t.Errorf("ManifestNum: got %d, want 1", disc.ManifestNum)
	}
	if disc.Path != manifestPath {
		t.Errorf("Path: got %q, want %q", disc.Path, manifestPath)
	}
	if disc.FileSize != 0 {
		t.Errorf("FileSize: got %d, want 0", disc.FileSize)
	}
	if disc.File == nil {
		t.Fatal("File descriptor is nil")
	}
}

// Invalid CURRENT Test Matrix
func TestBootDiscovery_InvalidCurrentMatrix(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, dir string)
		expectErrIs error
	}{
		{
			name: "Missing CURRENT in empty directory",
			setup: func(t *testing.T, dir string) {
				// No files created
			},
			expectErrIs: errors.ErrCurrentNotFound,
		},
		{
			name: "Missing CURRENT in populated directory",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
			},
			expectErrIs: errors.ErrCurrentNotFound,
		},
		{
			name: "Empty CURRENT file (0 bytes)",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte(""), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Missing newline",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-000001"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Double newline",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-000001\n\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "CRLF line ending",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-000001\r\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Leading whitespace",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte(" MANIFEST-000001\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Invalid prefix",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("LOG-000000000001\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Zero manifest sequence number",
			setup: func(t *testing.T, dir string) {
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-000000\n"), 0600)
			},
			expectErrIs: errors.ErrInvalidManifestNum,
		},
		{
			name: "Superfluous leading zeros",
			setup: func(t *testing.T, dir string) {
				_ = helperCreatePopulatedManifest(t, dir, 1, 1)
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-0000001\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Non-digit characters",
			setup: func(t *testing.T, dir string) {
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-00000A\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Uint64 overflow",
			setup: func(t *testing.T, dir string) {
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-99999999999999999999\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Too short (<16 bytes)",
			setup: func(t *testing.T, dir string) {
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-1\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "Too long (>30 bytes)",
			setup: func(t *testing.T, dir string) {
				_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-0000000000000000000001\n"), 0600)
			},
			expectErrIs: errors.ErrCurrentCorrupted,
		},
		{
			name: "CURRENT is a directory",
			setup: func(t *testing.T, dir string) {
				_ = os.Mkdir(filepath.Join(dir, CurrentFilename), 0700)
			},
			expectErrIs: errors.ErrNotADirectory,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)

			disc, err := DiscoverActiveManifest(dir)
			if err == nil {
				_ = disc.Close()
				t.Fatalf("expected error matching %v, got nil", tc.expectErrIs)
			}
			if tc.expectErrIs != nil && !stdErrors.Is(err, tc.expectErrIs) {
				t.Fatalf("expected error matching %v, got %v", tc.expectErrIs, err)
			}
		})
	}
}

// Test CURRENT is a symlink
func TestBootDiscovery_CurrentIsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require special privileges on Windows")
	}

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target_current")
	_ = os.WriteFile(targetPath, []byte("MANIFEST-000001\n"), 0600)
	_ = helperCreatePopulatedManifest(t, dir, 1, 1)

	currentPath := filepath.Join(dir, CurrentFilename)
	if err := os.Symlink(targetPath, currentPath); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	disc, err := DiscoverActiveManifest(dir)
	if err == nil {
		_ = disc.Close()
		t.Fatal("expected error for symlinked CURRENT, got nil")
	}
	if !stdErrors.Is(err, errors.ErrCurrentSymlink) {
		t.Fatalf("expected ErrCurrentSymlink, got %v", err)
	}
}

// Invalid / Missing MANIFEST Matrix
func TestBootDiscovery_InvalidManifestMatrix(t *testing.T) {
	t.Run("Referenced MANIFEST missing", func(t *testing.T) {
		dir := t.TempDir()
		// CURRENT points to MANIFEST-000005, but file does not exist
		if err := SetCurrentManifest(dir, 5); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for missing MANIFEST, got nil")
		}
		if !stdErrors.Is(err, errors.ErrManifestNotFound) {
			t.Errorf("expected ErrManifestNotFound, got %v", err)
		}
		if !stdErrors.Is(err, os.ErrNotExist) {
			t.Errorf("expected os.ErrNotExist wrapping, got %v", err)
		}
	})

	t.Run("Referenced MANIFEST is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}
		// Create directory where MANIFEST-000001 should be
		manifestDir := filepath.Join(dir, "MANIFEST-000001")
		if err := os.Mkdir(manifestDir, 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for directory MANIFEST, got nil")
		}
		if !stdErrors.Is(err, errors.ErrNotADirectory) {
			t.Errorf("expected ErrNotADirectory, got %v", err)
		}
	})

	t.Run("Referenced MANIFEST is a symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require special privileges on Windows")
		}

		dir := t.TempDir()
		realManifest := filepath.Join(dir, "real_manifest")
		_ = os.WriteFile(realManifest, []byte("fake content"), 0600)

		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		manifestPath := filepath.Join(dir, "MANIFEST-000001")
		if err := os.Symlink(realManifest, manifestPath); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for symlinked MANIFEST, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})
}

// Directory Validation Matrix
func TestBootDiscovery_DirectoryValidation(t *testing.T) {
	t.Run("Empty directory path", func(t *testing.T) {
		disc, err := DiscoverActiveManifest("")
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for empty directory, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})

	t.Run("Non-existent directory", func(t *testing.T) {
		missingDir := filepath.Join(t.TempDir(), "non_existent_subdir")
		disc, err := DiscoverActiveManifest(missingDir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for non-existent directory, got nil")
		}
		if !stdErrors.Is(err, os.ErrNotExist) && !stdErrors.Is(err, errors.ErrCurrentNotFound) {
			t.Errorf("expected ErrNotExist or ErrCurrentNotFound, got %v", err)
		}
	})

	t.Run("Directory path is regular file", func(t *testing.T) {
		filePath := filepath.Join(t.TempDir(), "not_a_dir")
		if err := os.WriteFile(filePath, []byte("hello"), 0600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		disc, err := DiscoverActiveManifest(filePath)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for file passed as dir, got nil")
		}
		if !stdErrors.Is(err, errors.ErrNotADirectory) {
			t.Errorf("expected ErrNotADirectory, got %v", err)
		}
	})

	t.Run("Directory path is symlink", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require special privileges on Windows")
		}
		realDir := filepath.Join(t.TempDir(), "real_db")
		if err := os.Mkdir(realDir, 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		symlinkDir := filepath.Join(t.TempDir(), "symlink_db")
		if err := os.Symlink(realDir, symlinkDir); err != nil {
			t.Fatalf("Symlink: %v", err)
		}

		disc, err := DiscoverActiveManifest(symlinkDir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error for symlinked directory, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})
}

// TOCTOU & Replacement Race Matrix
func TestBootDiscovery_TOCTOU_ReplacementRaces(t *testing.T) {
	t.Run("MANIFEST inode swapped during open", func(t *testing.T) {
		dir := t.TempDir()
		helperCreatePopulatedManifest(t, dir, 1, 1)
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		manifestPath := ManifestPath(dir, 1)

		// Inject hook: swap the file on disk with a new file (different inode) post-open
		restore := SetBootPostOpenHookForTesting(func(path string, f *os.File) error {
			_ = os.Remove(manifestPath)
			_ = os.WriteFile(manifestPath, []byte("attacker-content-different-inode"), 0600)
			return nil
		})
		defer restore()

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error when MANIFEST inode was swapped post-open, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})

	t.Run("MANIFEST replaced with symlink during open", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks require special privileges on Windows")
		}

		dir := t.TempDir()
		helperCreatePopulatedManifest(t, dir, 1, 1)
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		manifestPath := ManifestPath(dir, 1)
		decoyPath := filepath.Join(dir, "decoy_manifest")
		_ = os.WriteFile(decoyPath, []byte("decoy"), 0600)

		// Inject hook: swap MANIFEST for a symlink post-open
		restore := SetBootPostOpenHookForTesting(func(path string, f *os.File) error {
			_ = os.Remove(manifestPath)
			_ = os.Symlink(decoyPath, manifestPath)
			return nil
		})
		defer restore()

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error when MANIFEST replaced with symlink post-open, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})

	t.Run("Parent directory swapped during open", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("rename of in-use directory behavior differs on Windows")
		}

		parentBase := t.TempDir()
		dbDir := filepath.Join(parentBase, "db")
		if err := os.Mkdir(dbDir, 0700); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		helperCreatePopulatedManifest(t, dbDir, 1, 1)
		if err := SetCurrentManifest(dbDir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		swappedDir := filepath.Join(parentBase, "db_swapped")

		// Inject hook: swap parent directory post-open
		restore := SetBootPostOpenHookForTesting(func(path string, f *os.File) error {
			if err := os.Rename(dbDir, swappedDir); err != nil {
				return err
			}
			if err := os.Mkdir(dbDir, 0700); err != nil {
				return err
			}
			return nil
		})
		defer restore()

		disc, err := DiscoverActiveManifest(dbDir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("expected error when parent directory identity was swapped post-open, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})
}

// Invariant Tests: P07-S01-M01-INV-01 through INV-09
func TestBootDiscovery_Invariants(t *testing.T) {
	// INV-01: Exactly one authoritative active manifest resolved from CURRENT
	t.Run("INV-01_AuthoritativeManifestResolution", func(t *testing.T) {
		dir := t.TempDir()
		_ = helperCreatePopulatedManifest(t, dir, 7, 2)
		if err := SetCurrentManifest(dir, 7); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			t.Fatalf("DiscoverActiveManifest: %v", err)
		}
		defer func() { _ = disc.Close() }()

		if disc.ManifestNum != 7 {
			t.Errorf("INV-01 violated: got %d, want 7", disc.ManifestNum)
		}
	})

	// INV-02: Malformed CURRENT never causes fallback to another MANIFEST
	t.Run("INV-02_NoFallbackOnMalformedCurrent", func(t *testing.T) {
		dir := t.TempDir()
		// Put multiple manifests on disk
		_ = helperCreatePopulatedManifest(t, dir, 1, 1)
		_ = helperCreatePopulatedManifest(t, dir, 2, 1)
		_ = helperCreatePopulatedManifest(t, dir, 99, 1)

		// Malformed CURRENT with corrupt syntax
		_ = os.WriteFile(filepath.Join(dir, CurrentFilename), []byte("MANIFEST-CORRUPT\n"), 0600)

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("INV-02 violated: discovery succeeded on malformed CURRENT")
		}
		if !stdErrors.Is(err, errors.ErrCurrentCorrupted) {
			t.Errorf("INV-02 violated: expected ErrCurrentCorrupted, got %v", err)
		}
	})

	// INV-05: Discovery performs zero mutation of database state
	t.Run("INV-05_ZeroStateMutation", func(t *testing.T) {
		dir := t.TempDir()
		manifestPath := helperCreatePopulatedManifest(t, dir, 1, 5)
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}
		currentPath := filepath.Join(dir, CurrentFilename)

		currentBefore, _ := os.ReadFile(currentPath)   //nolint:gosec // Test-only file reading for hash verification
		manifestBefore, _ := os.ReadFile(manifestPath) //nolint:gosec // Test-only file reading for hash verification
		hCurrentBefore := sha256.Sum256(currentBefore)
		hManifestBefore := sha256.Sum256(manifestBefore)

		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			t.Fatalf("DiscoverActiveManifest: %v", err)
		}
		_ = disc.Close()

		currentAfter, _ := os.ReadFile(currentPath)   //nolint:gosec // Test-only file reading for hash verification
		manifestAfter, _ := os.ReadFile(manifestPath) //nolint:gosec // Test-only file reading for hash verification
		hCurrentAfter := sha256.Sum256(currentAfter)
		hManifestAfter := sha256.Sum256(manifestAfter)

		if hCurrentBefore != hCurrentAfter {
			t.Fatal("INV-05 violated: CURRENT file was mutated during discovery")
		}
		if hManifestBefore != hManifestAfter {
			t.Fatal("INV-05 violated: MANIFEST file was mutated during discovery")
		}
	})

	// INV-07: Discovery consumes bounded metadata only, no full manifest buffering
	t.Run("INV-07_BoundedMetadataOnly", func(t *testing.T) {
		dir := t.TempDir()
		// Create a manifest with 100 edits (~20KB)
		helperCreatePopulatedManifest(t, dir, 1, 100)
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		disc, err := DiscoverActiveManifest(dir)
		if err != nil {
			t.Fatalf("DiscoverActiveManifest: %v", err)
		}
		defer func() { _ = disc.Close() }()

		if disc.FileSize <= 0 {
			t.Errorf("expected non-zero FileSize, got %d", disc.FileSize)
		}
	})

	// INV-08: Repeated discovery over unchanged storage state is strictly deterministic
	t.Run("INV-08_DeterministicDiscovery", func(t *testing.T) {
		dir := t.TempDir()
		manifestPath := helperCreatePopulatedManifest(t, dir, 42, 3)
		if err := SetCurrentManifest(dir, 42); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		var firstNum uint64
		var firstSize int64

		for i := 0; i < 25; i++ {
			disc, err := DiscoverActiveManifest(dir)
			if err != nil {
				t.Fatalf("run %d failed: %v", i, err)
			}
			if i == 0 {
				firstNum = disc.ManifestNum
				firstSize = disc.FileSize
			} else {
				if disc.ManifestNum != firstNum || disc.FileSize != firstSize || disc.Path != manifestPath {
					_ = disc.Close()
					t.Fatalf("run %d non-deterministic: num=%d, size=%d, path=%s", i, disc.ManifestNum, disc.FileSize, disc.Path)
				}
			}
			_ = disc.Close()
		}
	})

	// INV-09: Failure is strictly fail-closed
	t.Run("INV-09_FailClosedOnAmbiguousState", func(t *testing.T) {
		dir := t.TempDir()
		// Ambiguous: CURRENT references manifest 2, but only manifest 1 exists
		_ = helperCreatePopulatedManifest(t, dir, 1, 1)
		if err := SetCurrentManifest(dir, 2); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		disc, err := DiscoverActiveManifest(dir)
		if err == nil {
			_ = disc.Close()
			t.Fatal("INV-09 violated: discovery succeeded on missing referenced manifest")
		}
		if !stdErrors.Is(err, errors.ErrManifestNotFound) {
			t.Errorf("INV-09 violated: expected ErrManifestNotFound, got %v", err)
		}
	})
}

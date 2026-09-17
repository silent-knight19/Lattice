package version

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// DiscoveredManifest encapsulates the verified active MANIFEST discovered at boot time.
// It conveys exclusive ownership of the opened active MANIFEST file descriptor to the caller.
type DiscoveredManifest struct {
	// Dir is the cleaned database directory path.
	Dir string

	// ManifestNum is the authoritative active manifest number parsed from CURRENT.
	ManifestNum uint64

	// Path is the cleaned canonical filesystem path to the active MANIFEST file.
	Path string

	// File is the open regular file descriptor pinned to the MANIFEST inode.
	// The caller assumes ownership and is responsible for calling Close.
	File *os.File

	// FileSize is the physical byte size of the MANIFEST at discovery time.
	FileSize int64
}

// Close releases the underlying MANIFEST file descriptor if open.
// Subsequent calls to Close are idempotent no-ops.
func (d *DiscoveredManifest) Close() error {
	if d == nil || d.File == nil {
		return nil
	}
	err := d.File.Close()
	d.File = nil
	return err
}

// manifestNotFoundError wraps errors when the referenced MANIFEST file does not exist on disk.
// It matches both errors.ErrManifestNotFound and os.ErrNotExist with errors.Is().
type manifestNotFoundError struct {
	path       string
	underlying error
}

func (e *manifestNotFoundError) Error() string {
	return fmt.Sprintf("%s: %s: %v", errors.ErrManifestNotFound, e.path, e.underlying)
}

func (e *manifestNotFoundError) Is(target error) bool {
	return target == errors.ErrManifestNotFound || target == os.ErrNotExist
}

func (e *manifestNotFoundError) Unwrap() error {
	return e.underlying
}

// Test seams for deterministic fault injection and race simulation.
//
// Architectural Note on Subsystem-Scoped Test Seams (SEC-P07-004):
// Lattice maintains independent, package-private test seams for filesystem operations
// (`bootLstatFn`, `currentLstatFn`, `replayLstatFn`, and `vs.lstatFn`) rather than a single
// global hook. This design enforces subsystem isolation during fault injection: tests
// targeting boot discovery, atomic pointer publication, or log replay can inject simulated
// filesystem mutations without unintended cross-talk or side-effects on concurrent operations
// across other subsystems.
var (
	bootLstatFn      = os.Lstat
	bootOpenFn       = openFileNoFollow
	bootPostOpenHook func(path string, f *os.File) error
	bootHookMu       sync.Mutex
)

// DiscoverActiveManifest executes the boot-time discovery and CURRENT validation boundary
// according to the P07-S01-M01 specification.
//
// Invariants & Operational Semantics:
//  1. Validates the target database directory, rejecting empty paths, non-existent directories,
//     symlinks, and regular files (P07-S01-M01-INV-03).
//  2. Discovers and strictly parses CURRENT using the canonical ReadCurrentManifest reader,
//     enforcing strict syntax, single newline, bounded size [16, 30] bytes, non-zero number,
//     and zero fallback on corruption (P07-S01-M01-INV-01, INV-02).
//  3. Resolves the active MANIFEST path via ManifestPath(dir, manifestNum) and asserts directory
//     confinement (preventing path traversal).
//  4. Performs pre-open inspection via os.Lstat (without following symlinks), rejecting symlinks,
//     directories, and non-regular files.
//  5. Opens the MANIFEST descriptor with O_RDONLY and O_NOFOLLOW via bootOpenFn.
//  6. Guarantees descriptor cleanup: if any subsequent verification fails, the descriptor is
//     closed immediately without leaking (P07-S01-M01-INV-06).
//  7. Validates descriptor properties via fstat: verifies regular file, non-directory.
//  8. Re-inspects the pathname post-open via os.Lstat and enforces double inode pinning
//     via os.SameFile(fstat, lstatBefore) and os.SameFile(fstat, lstatAfter) to eliminate
//     TOCTOU substitution attacks (P07-S01-M01-INV-04).
//  9. Re-verifies parent directory identity to detect parent substitution races.
//  10. Transfers ownership of the opened file descriptor to DiscoveredManifest without reading
//     or buffering the MANIFEST content (P07-S01-M01-INV-05, INV-07).
//  11. Repeated discovery calls over unchanged filesystem state are strictly deterministic (INV-08).
//  12. Fail-closed on any ambiguous or corrupted filesystem state (INV-09).
func DiscoverActiveManifest(dir string) (*DiscoveredManifest, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: directory path cannot be empty", os.ErrInvalid)
	}

	cleanDir := filepath.Clean(dir)

	// 1. Directory validation: must exist, be a directory, and not be a symlink
	dirInfo, err := bootLstatFn(cleanDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &currentNotFoundError{path: cleanDir, underlying: err}
		}
		return nil, fmt.Errorf("boot: failed to inspect directory %s: %w", cleanDir, err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: directory %s cannot be a symbolic link", os.ErrInvalid, cleanDir)
	}
	if !dirInfo.IsDir() {
		return nil, &errors.NotADirectoryError{Path: cleanDir, Mode: dirInfo.Mode()}
	}

	// 2. Discover and parse authoritative CURRENT
	manifestNum, err := ReadCurrentManifest(cleanDir)
	if err != nil {
		return nil, err
	}

	// 3. Resolve active MANIFEST path and assert containment
	manifestPath := ManifestPath(cleanDir, manifestNum)
	cleanManifestPath := filepath.Clean(manifestPath)
	if filepath.Dir(cleanManifestPath) != cleanDir {
		return nil, fmt.Errorf("%w: resolved manifest path %q escapes directory %q", os.ErrInvalid, cleanManifestPath, cleanDir)
	}

	// 4. Pre-open inspection of the referenced MANIFEST
	lstatBefore, err := bootLstatFn(cleanManifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &manifestNotFoundError{path: cleanManifestPath, underlying: err}
		}
		return nil, fmt.Errorf("boot: failed to inspect manifest %s: %w", cleanManifestPath, err)
	}
	if lstatBefore.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: manifest path %q is a symlink", os.ErrInvalid, cleanManifestPath)
	}
	if lstatBefore.IsDir() {
		return nil, &errors.NotADirectoryError{Path: cleanManifestPath, Mode: lstatBefore.Mode()}
	}
	if !lstatBefore.Mode().IsRegular() {
		return nil, fmt.Errorf("boot: manifest %q is not a regular file (mode: %s): %w", cleanManifestPath, lstatBefore.Mode(), os.ErrInvalid)
	}

	// 5. Open file descriptor with O_RDONLY and O_NOFOLLOW
	file, err := bootOpenFn(cleanManifestPath, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &manifestNotFoundError{path: cleanManifestPath, underlying: err}
		}
		return nil, fmt.Errorf("boot: failed to open manifest %s: %w", cleanManifestPath, err)
	}

	// Guaranteed descriptor cleanup if subsequent validation fails
	var success bool
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()

	// 6. Test seam hook for race / substitution injection
	bootHookMu.Lock()
	hook := bootPostOpenHook
	bootHookMu.Unlock()
	if hook != nil {
		if hookErr := hook(cleanManifestPath, file); hookErr != nil {
			return nil, hookErr
		}
	}

	// 7. Inspect descriptor directly via fstat
	fstat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("boot: failed to stat open manifest %s: %w", cleanManifestPath, err)
	}
	if fstat.IsDir() {
		return nil, &errors.NotADirectoryError{Path: cleanManifestPath, Mode: fstat.Mode()}
	}
	if !fstat.Mode().IsRegular() {
		return nil, fmt.Errorf("boot: manifest descriptor %s is not a regular file (mode: %s): %w", cleanManifestPath, fstat.Mode(), os.ErrInvalid)
	}

	// 8. Post-open pathname re-inspection without following symlinks
	lstatAfter, err := bootLstatFn(cleanManifestPath)
	if err != nil {
		return nil, fmt.Errorf("%w: manifest path %s could not be statted post-open: %w", os.ErrInvalid, cleanManifestPath, err)
	}
	if lstatAfter.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: manifest file %q was replaced with a symlink post-open", os.ErrInvalid, cleanManifestPath)
	}

	// 9. Inode pinning & object identity invariance (TOCTOU defense)
	if !os.SameFile(fstat, lstatBefore) || !os.SameFile(fstat, lstatAfter) {
		return nil, fmt.Errorf("%w: manifest file %s was replaced during open", os.ErrInvalid, cleanManifestPath)
	}

	// 10. Parent directory re-inspection invariance
	parentLstatAfter, err := bootLstatFn(cleanDir)
	if err != nil {
		return nil, fmt.Errorf("%w: parent directory %s could not be statted post-open: %w", os.ErrInvalid, cleanDir, err)
	}
	if !os.SameFile(dirInfo, parentLstatAfter) {
		return nil, fmt.Errorf("%w: parent directory %s was replaced during open", os.ErrInvalid, cleanDir)
	}

	// Successfully validated! Transfer descriptor ownership to caller
	success = true
	return &DiscoveredManifest{
		Dir:         cleanDir,
		ManifestNum: manifestNum,
		Path:        cleanManifestPath,
		File:        file,
		FileSize:    fstat.Size(),
	}, nil
}

package sstable_test

import (
	"bytes"
	stdErrors "errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// helperBuildTable creates a valid, sealed SSTable file at path with the given key-value pairs.
func helperBuildTable(t *testing.T, path string, kvs map[string]string) {
	t.Helper()
	opts := sstable.DefaultTableWriterOptions()
	w, err := sstable.NewTableWriter(path, opts)
	if err != nil {
		t.Fatalf("failed to create table writer at %q: %v", path, err)
	}

	// Sort keys to guarantee strictly increasing canonical key order
	keys := make([]string, 0, len(kvs))
	for k := range kvs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := kvs[k]
		ik, err := binary.NewInternalKey([]byte(k), 10, binary.OpTypePut)
		if err != nil {
			t.Fatalf("failed to create internal key: %v", err)
		}
		if err := w.Add(ik, []byte(v)); err != nil {
			t.Fatalf("failed to add entry: %v", err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatalf("failed to finish table writer at %q: %v", path, err)
	}
}

// 1. Normal regular SSTable: correctly opens and reads data and filter blocks.
func TestSEC007_NormalRegularSSTable_Accepted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "normal.sst")
	helperBuildTable(t, path, map[string]string{
		"apple":  "fruit_red",
		"banana": "fruit_yellow",
	})

	r, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("expected NewTableReader to succeed, got %v", err)
	}
	defer func() { _ = r.Close() }()

	val, err := r.Seek([]byte("apple"))
	if err != nil {
		t.Fatalf("Seek(apple) failed: %v", err)
	}
	if string(val) != "fruit_red" {
		t.Fatalf("expected fruit_red, got %s", string(val))
	}

	fb, err := r.ReadFilterBlock()
	if err != nil {
		t.Fatalf("ReadFilterBlock failed: %v", err)
	}
	if fb != nil {
		// If filter block was written, verify key membership
		if !fb.MayContain([]byte("apple")) {
			t.Errorf("expected BloomFilter to contain 'apple'")
		}
	}
}

// 2. Destination path is symlink: rejected with ErrSSTableSymlink. Target remains untouched.
func TestSEC007_PathIsSymlink_Rejected(t *testing.T) {
	requireSymlinks(t)

	dir := t.TempDir()
	realPath := filepath.Join(dir, "real.sst")
	helperBuildTable(t, realPath, map[string]string{"k": "v"})

	symlinkPath := filepath.Join(dir, "symlink.sst")
	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	r, err := sstable.NewTableReader(symlinkPath)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader to reject symlink path, but succeeded")
	}
	if !stdErrors.Is(err, errors.ErrSSTableSymlink) {
		t.Fatalf("expected ErrSSTableSymlink, got: %v", err)
	}

	// Verify real file is untouched and can still be opened directly
	r2, err2 := sstable.NewTableReader(realPath)
	if err2 != nil {
		t.Fatalf("expected direct open of real file to succeed, got %v", err2)
	}
	_ = r2.Close()
}

// 3. Intermediate component is symlink: rejected with ErrParentDirectorySymlink.
func TestSEC007_IntermediateComponentSymlink_Rejected(t *testing.T) {
	requireSymlinks(t)

	dir := t.TempDir()
	realDir := filepath.Join(dir, "real_dir")
	if err := os.MkdirAll(realDir, 0700); err != nil {
		t.Fatalf("failed to create real dir: %v", err)
	}
	realPath := filepath.Join(realDir, "table.sst")
	helperBuildTable(t, realPath, map[string]string{"k": "v"})

	// Create symlink pointing to realDir
	symDir := filepath.Join(dir, "sym_dir")
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Fatalf("failed to create dir symlink: %v", err)
	}

	// Access table through intermediate symlink
	indirectPath := filepath.Join(symDir, "table.sst")
	r, err := sstable.NewTableReader(indirectPath)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader to reject path with intermediate symlink, but succeeded")
	}
	if !stdErrors.Is(err, errors.ErrParentDirectorySymlink) {
		t.Fatalf("expected ErrParentDirectorySymlink, got: %v", err)
	}
}

// 4. File replaced before open: detected and fails closed.
func TestSEC007_FileReplacedBeforeOpen_Detected(t *testing.T) {
	requireSymlinks(t)

	dir := t.TempDir()
	legitPath := filepath.Join(dir, "legit.sst")
	helperBuildTable(t, legitPath, map[string]string{"key": "legit_value"})

	evilPath := filepath.Join(dir, "evil.sst")
	helperBuildTable(t, evilPath, map[string]string{"key": "evil_value"})

	// Before open, replace legit.sst with a symlink to evil.sst
	_ = os.Remove(legitPath)
	if err := os.Symlink(evilPath, legitPath); err != nil {
		t.Fatalf("failed to symlink: %v", err)
	}

	r, err := sstable.NewTableReader(legitPath)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader to fail on replaced file, got nil")
	}
	if !stdErrors.Is(err, errors.ErrSSTableSymlink) && !stdErrors.Is(err, errors.ErrSSTableObjectChanged) {
		t.Fatalf("expected ErrSSTableSymlink or ErrSSTableObjectChanged, got: %v", err)
	}
}

// 5. File replaced after open: opened descriptor remains authoritative.
// Subsequent modifications to pathname MUST NOT redirect reads from the open TableReader.
func TestSEC007_FileReplacedAfterOpen_DescriptorAuthoritative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authoritative.sst")
	helperBuildTable(t, path, map[string]string{
		"target_key": "LEGITIMATE_PAYLOAD",
	})

	// Open the legitimate table
	r, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	// While reader is active, replace the file on disk with an attacker table
	_ = os.Remove(path)
	helperBuildTable(t, path, map[string]string{
		"target_key": "ATTACKER_PAYLOAD",
	})

	// Seek on the open reader MUST return the legitimate payload from the pinned descriptor
	val, err := r.Seek([]byte("target_key"))
	if err != nil {
		t.Fatalf("Seek failed: %v", err)
	}
	if string(val) != "LEGITIMATE_PAYLOAD" {
		t.Fatalf("CRITICAL SECURITY FAILURE: reader read from replaced file on disk! expected LEGITIMATE_PAYLOAD, got %s", string(val))
	}
}

// 6. Same path, different inode: post-open substitution is detected via os.SameFile.
func TestSEC007_SamePathDifferentInode_Detected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.sst")
	helperBuildTable(t, path, map[string]string{"k": "v1"})

	altPath := filepath.Join(dir, "alt.sst")
	helperBuildTable(t, altPath, map[string]string{"k": "v2"})

	// Inject hook: immediately after os.Open succeeds, replace target.sst with alt.sst (different inode)
	cleanup := sstable.SetPostOpenHookForTesting(func() error {
		_ = os.Remove(path)
		return os.Rename(altPath, path)
	})
	defer cleanup()

	r, err := sstable.NewTableReader(path)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader to detect inode mismatch post-open, got nil")
	}
	if !stdErrors.Is(err, errors.ErrSSTableObjectChanged) {
		t.Fatalf("expected ErrSSTableObjectChanged, got: %v", err)
	}
}

// 7. Post-open symlink swap: detected and rejected with ErrSSTableSymlink.
func TestSEC007_PostOpenSymlinkSwap_Detected(t *testing.T) {
	requireSymlinks(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "swap_symlink.sst")
	helperBuildTable(t, path, map[string]string{"k": "v"})

	victimFile := filepath.Join(dir, "victim.sst")
	helperBuildTable(t, victimFile, map[string]string{"k": "victim"})

	// Inject hook: immediately after os.Open, replace path with a symlink to victimFile
	cleanup := sstable.SetPostOpenHookForTesting(func() error {
		_ = os.Remove(path)
		return os.Symlink(victimFile, path)
	})
	defer cleanup()

	r, err := sstable.NewTableReader(path)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader to detect post-open symlink swap, got nil")
	}
	if !stdErrors.Is(err, errors.ErrSSTableSymlink) && !stdErrors.Is(err, errors.ErrSSTableObjectChanged) {
		t.Fatalf("expected ErrSSTableSymlink or ErrSSTableObjectChanged, got: %v", err)
	}
}

// 8. Opened object is directory: rejected with ErrNotADirectory and mentions "not a regular file".
func TestSEC007_OpenedObjectIsDirectory_Rejected(t *testing.T) {
	dir := t.TempDir()

	// 1. Via pathname
	r, err := sstable.NewTableReader(dir)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader on directory to fail, but succeeded")
	}
	if !stdErrors.Is(err, errors.ErrNotADirectory) {
		t.Errorf("expected ErrNotADirectory, got: %v", err)
	}

	// 2. Via open descriptor
	df, err := os.Open(dir)
	if err != nil {
		t.Fatalf("failed to open dir: %v", err)
	}
	r2, err2 := sstable.NewTableReaderWithFile(df)
	if err2 == nil {
		_ = r2.Close()
		t.Fatalf("expected NewTableReaderWithFile on directory to fail, but succeeded")
	}
	if !stdErrors.Is(err2, errors.ErrNotADirectory) {
		t.Errorf("expected ErrNotADirectory for NewTableReaderWithFile, got: %v", err2)
	}
}

// 9. Opened object is non-regular: rejected before open (FIFO / named pipe).
func TestSEC007_OpenedObjectIsNonRegular_Rejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named pipes via mkfifo not supported on Windows")
	}

	dir := t.TempDir()
	fifoPath := filepath.Join(dir, "test.fifo")
	if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
		t.Skipf("mkfifo failed: %v", err)
	}

	// NewTableReader must reject FIFO before os.Open, avoiding blocking on open
	r, err := sstable.NewTableReader(fifoPath)
	if err == nil {
		_ = r.Close()
		t.Fatalf("expected NewTableReader on FIFO to fail, but succeeded")
	}
}

// 10. Missing file: returns ordinary not-found error.
func TestSEC007_MissingFile_OrdinaryNotFound(t *testing.T) {
	dir := t.TempDir()
	nonexistent := filepath.Join(dir, "does_not_exist.sst")

	_, err := sstable.NewTableReader(nonexistent)
	if err == nil {
		t.Fatalf("expected error for nonexistent file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist(err) to be true, got %v", err)
	}
}

// 11. Permission failure: preserves OS permission error.
func TestSEC007_PermissionFailure_Preserved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping permission test when running as root")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "no_access.sst")
	helperBuildTable(t, p, map[string]string{"k": "v"})

	if err := os.Chmod(p, 0000); err != nil {
		t.Skipf("chmod failed: %v", err)
	}
	defer func() { _ = os.Chmod(p, 0600) }()

	_, err := sstable.NewTableReader(p)
	if err == nil {
		t.Fatalf("expected error for unreadable file, got nil")
	}
	if !os.IsPermission(err) {
		t.Fatalf("expected os.IsPermission(err) to be true, got %v", err)
	}
}

// 12. Malformed SSTable: existing corruption errors are preserved.
func TestSEC007_MalformedSSTable_CorruptionPreserved(t *testing.T) {
	dir := t.TempDir()

	t.Run("truncated_file", func(t *testing.T) {
		p := filepath.Join(dir, "truncated.sst")
		if err := os.WriteFile(p, []byte("short"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := sstable.NewTableReader(p)
		if err == nil {
			t.Fatalf("expected error on truncated sstable, got nil")
		}
		var target *errors.InvalidFooterSizeError
		if !stdErrors.As(err, &target) {
			t.Fatalf("expected InvalidFooterSizeError, got: %v", err)
		}
	})

	t.Run("corrupt_magic", func(t *testing.T) {
		p := filepath.Join(dir, "bad_magic.sst")
		badFooter := bytes.Repeat([]byte{0x77}, 48)
		if err := os.WriteFile(p, badFooter, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := sstable.NewTableReader(p)
		if err == nil {
			t.Fatalf("expected error on bad magic, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidFooter) && !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
			t.Fatalf("expected footer magic error, got: %v", err)
		}
	})
}

// 13. Descriptor leak prevention: no descriptors leaked across all failure modes.
func TestSEC007_DescriptorLeakPrevention(t *testing.T) {
	dir := t.TempDir()

	// Helper to check if a file descriptor is closed
	isFileClosed := func(f *os.File) bool {
		var b [1]byte
		_, err := f.Read(b[:])
		return stdErrors.Is(err, os.ErrClosed)
	}

	// 1. Truncated file
	pTrunc := filepath.Join(dir, "leak_trunc.sst")
	if err := os.WriteFile(pTrunc, []byte("too_short"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		f, err := os.Open(pTrunc)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !isFileClosed(f) {
			t.Fatalf("descriptor leaked on truncated file iteration %d", i)
		}
	}

	// 2. Corrupted magic
	pMagic := filepath.Join(dir, "leak_magic.sst")
	if err := os.WriteFile(pMagic, bytes.Repeat([]byte{0xFF}, 48), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		f, err := os.Open(pMagic)
		if err != nil {
			t.Fatal(err)
		}
		_, err = sstable.NewTableReaderWithFile(f)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !isFileClosed(f) {
			t.Fatalf("descriptor leaked on bad magic iteration %d", i)
		}
	}

	// 3. NewTableReader on directory
	for i := 0; i < 30; i++ {
		_, err := sstable.NewTableReader(dir)
		if err == nil {
			t.Fatal("expected error on directory, got nil")
		}
	}
}

// 14. Concurrent path mutation: goroutines swap path between valid SSTable, symlink,
// and replacement file while multiple readers call NewTableReader concurrently.
// Invariant: No panics, no data races, every reader either succeeds on legitimate data or fails closed.
func TestSEC007_ConcurrentPathMutation(t *testing.T) {
	requireSymlinks(t)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.sst")
	legitA := filepath.Join(dir, "legit_a.sst")
	legitB := filepath.Join(dir, "legit_b.sst")
	evilFile := filepath.Join(dir, "evil.sst")

	helperBuildTable(t, legitA, map[string]string{"k": "val_A"})
	helperBuildTable(t, legitB, map[string]string{"k": "val_B"})
	helperBuildTable(t, evilFile, map[string]string{"k": "EVIL"})

	// Initially target.sst is legitA
	initialBytes, _ := os.ReadFile(legitA)
	if err := os.WriteFile(targetPath, initialBytes, 0600); err != nil {
		t.Fatal(err)
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup

	// Mutator goroutine: continuously rotates targetPath
	wg.Add(1)
	go func() {
		defer wg.Done()
		flip := false
		for {
			select {
			case <-stopCh:
				return
			default:
				if flip {
					_ = os.Remove(targetPath)
					_ = os.Symlink(evilFile, targetPath)
				} else {
					_ = os.Remove(targetPath)
					b, _ := os.ReadFile(legitB)
					_ = os.WriteFile(targetPath, b, 0600)
				}
				flip = !flip
			}
		}
	}()

	// Reader goroutines
	const numReaders = 8
	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r, err := sstable.NewTableReader(targetPath)
				if err != nil {
					// Safe fail-closed (symlink, replaced, or not found during swap)
					continue
				}
				// If opened successfully, verify seek never returns EVIL
				val, err := r.Seek([]byte("k"))
				if err == nil {
					if string(val) == "EVIL" {
						t.Errorf("CRITICAL SECURITY BREACH: reader returned EVIL payload!")
					}
				}
				_ = r.Close()
			}
		}()
	}

	// Let readers run
	for i := 0; i < 15; i++ {
		r, err := sstable.NewTableReader(legitA)
		if err == nil {
			_ = r.Close()
		}
	}

	close(stopCh)
	wg.Wait()
}

// 15. Object Identity Matrix: comprehensive evaluation of path resolution scenarios.
func TestSEC007_ObjectIdentityMatrix(t *testing.T) {
	requireSymlinks(t)
	baseDir := t.TempDir()

	t.Run("01_CanonicalAbsolutePath_Accept", func(t *testing.T) {
		p := filepath.Join(baseDir, "case01.sst")
		helperBuildTable(t, p, map[string]string{"k": "v"})
		r, err := sstable.NewTableReader(p)
		if err != nil {
			t.Fatalf("expected accept, got %v", err)
		}
		_ = r.Close()
	})

	t.Run("02_RedundantDotsAndSeparators_Accept", func(t *testing.T) {
		p := filepath.Join(baseDir, "case02.sst")
		helperBuildTable(t, p, map[string]string{"k": "v"})
		redundant := filepath.Join(baseDir, ".", ".", "case02.sst")
		r, err := sstable.NewTableReader(redundant)
		if err != nil {
			t.Fatalf("expected accept for normalized path, got %v", err)
		}
		_ = r.Close()
	})

	t.Run("03_LeafSymlink_Reject", func(t *testing.T) {
		p := filepath.Join(baseDir, "case03.sst")
		helperBuildTable(t, p, map[string]string{"k": "v"})
		sym := filepath.Join(baseDir, "case03_sym.sst")
		if err := os.Symlink(p, sym); err != nil {
			t.Fatal(err)
		}
		r, err := sstable.NewTableReader(sym)
		if err == nil {
			_ = r.Close()
			t.Fatal("expected reject for leaf symlink")
		}
		if !stdErrors.Is(err, errors.ErrSSTableSymlink) {
			t.Fatalf("expected ErrSSTableSymlink, got %v", err)
		}
	})

	t.Run("04_IntermediateAncestorSymlink_Reject", func(t *testing.T) {
		subDir := filepath.Join(baseDir, "case04_sub")
		_ = os.MkdirAll(subDir, 0700)
		p := filepath.Join(subDir, "case04.sst")
		helperBuildTable(t, p, map[string]string{"k": "v"})

		symDir := filepath.Join(baseDir, "case04_symdir")
		if err := os.Symlink(subDir, symDir); err != nil {
			t.Fatal(err)
		}
		indirectPath := filepath.Join(symDir, "case04.sst")
		r, err := sstable.NewTableReader(indirectPath)
		if err == nil {
			_ = r.Close()
			t.Fatal("expected reject for intermediate symlink")
		}
		if !stdErrors.Is(err, errors.ErrParentDirectorySymlink) {
			t.Fatalf("expected ErrParentDirectorySymlink, got %v", err)
		}
	})

	t.Run("05_SamePathReplacedInodePostOpen_Reject", func(t *testing.T) {
		p := filepath.Join(baseDir, "case05.sst")
		helperBuildTable(t, p, map[string]string{"k": "v"})
		alt := filepath.Join(baseDir, "case05_alt.sst")
		helperBuildTable(t, alt, map[string]string{"k": "v_alt"})

		cleanup := sstable.SetPostOpenHookForTesting(func() error {
			_ = os.Remove(p)
			return os.Rename(alt, p)
		})
		defer cleanup()

		r, err := sstable.NewTableReader(p)
		if err == nil {
			_ = r.Close()
			t.Fatal("expected reject on inode replacement")
		}
		if !stdErrors.Is(err, errors.ErrSSTableObjectChanged) {
			t.Fatalf("expected ErrSSTableObjectChanged, got %v", err)
		}
	})
}

// 16. Fuzz-style path safety: arbitrary path inputs must never panic and must fail cleanly.
func TestSEC007_PathRobustness(t *testing.T) {
	adversarialPaths := []string{
		"",
		"/",
		".",
		"..",
		"...",
		"/dev/null",
		"///nonexistent///",
		"\x00invalid\x00path",
		"long/" + string(bytes.Repeat([]byte("a"), 1000)),
		filepath.Join(t.TempDir(), "nonexistent", "sub", "table.sst"),
	}

	for _, p := range adversarialPaths {
		r, err := sstable.NewTableReader(p)
		if err == nil {
			_ = r.Close()
			t.Errorf("expected error for adversarial path %q, got nil", p)
		}
	}
}

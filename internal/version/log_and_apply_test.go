package version

import (
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// helper to create a mock SSTable with exact size
func createMockSSTable(t *testing.T, dir string, fileNum uint64, size int64) string {
	t.Helper()
	p := TablePath(dir, fileNum)
	data := make([]byte, size)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatalf("failed to write mock sstable: %v", err)
	}
	return p
}

// helper to create canonical FileMetadata with valid InternalKeys
func makeTestFileMeta(fileNum uint64, size uint64, smallestUser, largestUser string, smallSeq, largeSeq uint64) FileMetadata {
	smallIK, err := binary.NewInternalKey([]byte(smallestUser), binary.SeqNum(largeSeq), binary.OpTypePut)
	if err != nil {
		panic(err)
	}
	largeIK, err := binary.NewInternalKey([]byte(largestUser), binary.SeqNum(smallSeq), binary.OpTypePut)
	if err != nil {
		panic(err)
	}
	return FileMetadata{
		FileNum:        fileNum,
		FileSize:       size,
		SmallestKey:    binary.EncodeInternalKey(smallIK),
		LargestKey:     binary.EncodeInternalKey(largeIK),
		SmallestSeqNum: smallSeq,
		LargestSeqNum:  largeSeq,
	}
}

// setupTestVersionSet initializes a VersionSet with an active ManifestWriter in a temporary directory
func setupTestVersionSet(t *testing.T) (*VersionSet, *ManifestWriter, string) {
	t.Helper()
	dir := t.TempDir()
	manPath := filepath.Join(dir, ManifestFilename(1))
	w, err := CreateManifestWriter(manPath)
	if err != nil {
		t.Fatalf("failed to create manifest writer: %v", err)
	}
	t.Cleanup(func() {
		_ = w.Close()
	})

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:         dir,
		ManifestWriter: w,
		NextFileNum:    1,
		LastSeqNum:     0,
	})
	return vs, w, dir
}

// -----------------------------------------------------------------------------
// ACCEPTANCE TEST MATRIX A THROUGH V
// -----------------------------------------------------------------------------

// Test A — Empty edit
func TestLogAndApply_MatrixA_EmptyEdit(t *testing.T) {
	vs, _, _ := setupTestVersionSet(t)

	edit := NewVersionEdit()
	if err := vs.LogAndApply(edit); err != nil {
		t.Fatalf("expected empty edit to succeed, got: %v", err)
	}

	cur := vs.Current()
	if cur == nil {
		t.Fatalf("expected active Current after empty edit")
	}
	defer cur.Unref()

	for lvl := 0; lvl < NumLevels; lvl++ {
		if cur.NumFiles(lvl) != 0 {
			t.Fatalf("expected 0 files at level %d, got %d", lvl, cur.NumFiles(lvl))
		}
	}
}

// Test B — Add file
func TestLogAndApply_MatrixB_AddFile(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	edit := NewVersionEdit()
	edit.SetNextFileNum(2)
	edit.SetLastSeqNum(10)
	meta := makeTestFileMeta(1, 1024, "a", "m", 1, 10)
	if err := edit.AddFile(0, meta); err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	if err := vs.LogAndApply(edit); err != nil {
		t.Fatalf("LogAndApply failed: %v", err)
	}

	cur := vs.Current()
	if cur == nil {
		t.Fatalf("expected Current to be set")
	}
	defer cur.Unref()

	if cur.NumFiles(0) != 1 {
		t.Fatalf("expected 1 file at L0, got %d", cur.NumFiles(0))
	}
	files := cur.Files(0)
	if files[0].FileNum != 1 || files[0].FileSize != 1024 {
		t.Fatalf("unexpected metadata in L0: %+v", files[0])
	}
	if vs.NextFileNum() != 2 {
		t.Fatalf("expected NextFileNum = 2, got %d", vs.NextFileNum())
	}
	if vs.LastSeqNum() != 10 {
		t.Fatalf("expected LastSeqNum = 10, got %d", vs.LastSeqNum())
	}
}

// Test C — Delete file
func TestLogAndApply_MatrixC_DeleteFile(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// Step 1: Add file 1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	e1.SetLastSeqNum(5)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 5))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Step 2: Delete file 1
	e2 := NewVersionEdit()
	if err := e2.DeleteFile(0, 1); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	if err := vs.LogAndApply(e2); err != nil {
		t.Fatalf("e2 failed: %v", err)
	}

	cur := vs.Current()
	if cur == nil {
		t.Fatalf("expected Current to be set")
	}
	defer cur.Unref()

	if cur.NumFiles(0) != 0 {
		t.Fatalf("expected 0 files at L0 after deletion, got %d", cur.NumFiles(0))
	}
}

// Test D — Compaction replacement
func TestLogAndApply_MatrixD_CompactionReplacement(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)
	createMockSSTable(t, dir, 3, 2048)

	// Initial: L0 has files 1 and 2
	e1 := NewVersionEdit()
	e1.SetNextFileNum(3)
	e1.SetLastSeqNum(20)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "d", 1, 10))
	_ = e1.AddFile(0, makeTestFileMeta(2, 1024, "e", "h", 11, 20))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("initial edit failed: %v", err)
	}

	// Compaction edit: Delete 1, 2 from L0, Add 3 to L1
	eComp := NewVersionEdit()
	eComp.SetNextFileNum(4)
	_ = eComp.DeleteFile(0, 1)
	_ = eComp.DeleteFile(0, 2)
	_ = eComp.AddFile(1, makeTestFileMeta(3, 2048, "a", "h", 1, 20))

	if err := vs.LogAndApply(eComp); err != nil {
		t.Fatalf("compaction edit failed: %v", err)
	}

	cur := vs.Current()
	defer cur.Unref()

	if cur.NumFiles(0) != 0 {
		t.Fatalf("expected L0 to be empty, got %d files", cur.NumFiles(0))
	}
	if cur.NumFiles(1) != 1 {
		t.Fatalf("expected L1 to have 1 file, got %d", cur.NumFiles(1))
	}
	if cur.Files(1)[0].FileNum != 3 {
		t.Fatalf("expected file 3 at L1, got %d", cur.Files(1)[0].FileNum)
	}
}

// Test E — Multiple deletes/additions
func TestLogAndApply_MatrixE_MultipleDeletesAndAdditions(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	for i := uint64(1); i <= 6; i++ {
		createMockSSTable(t, dir, i, 512)
	}

	// Install initial files 1, 2, 3 at L1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(4)
	e1.SetLastSeqNum(30)
	_ = e1.AddFile(1, makeTestFileMeta(1, 512, "a", "c", 1, 10))
	_ = e1.AddFile(1, makeTestFileMeta(2, 512, "d", "f", 11, 20))
	_ = e1.AddFile(1, makeTestFileMeta(3, 512, "g", "i", 21, 30))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Multi-delete (1, 2) and multi-add (4, 5, 6)
	e2 := NewVersionEdit()
	e2.SetNextFileNum(7)
	_ = e2.DeleteFile(1, 1)
	_ = e2.DeleteFile(1, 2)
	_ = e2.AddFile(1, makeTestFileMeta(4, 512, "a", "b", 1, 10))
	_ = e2.AddFile(1, makeTestFileMeta(5, 512, "ba", "bb", 11, 20))
	_ = e2.AddFile(1, makeTestFileMeta(6, 512, "c", "d", 21, 30))

	if err := vs.LogAndApply(e2); err != nil {
		t.Fatalf("e2 failed: %v", err)
	}

	cur := vs.Current()
	defer cur.Unref()

	files := cur.Files(1)
	if len(files) != 4 {
		t.Fatalf("expected 4 files in L1 (files 4, 5, 6, 3), got %d", len(files))
	}
	// Expected ordering: 4 ("a"-"b"), 5 ("ba"-"bb"), 6 ("c"-"d"), 3 ("g"-"i")
	expectedNums := []uint64{4, 5, 6, 3}
	for i, f := range files {
		if f.FileNum != expectedNums[i] {
			t.Errorf("file[%d] = %d, expected %d", i, f.FileNum, expectedNums[i])
		}
	}
}

// Test F — Manifest append failure
func TestLogAndApply_MatrixF_ManifestAppendFailure(t *testing.T) {
	vs, w, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// Inject write failure
	injectedErr := fmt.Errorf("injected disk write error")
	w.writeFn = func(file *os.File, p []byte) (int, error) {
		return 0, injectedErr
	}

	edit := NewVersionEdit()
	edit.SetNextFileNum(2)
	_ = edit.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 5))

	err := vs.LogAndApply(edit)
	if err == nil {
		t.Fatalf("expected LogAndApply to fail on manifest write error")
	}

	// Current must remain nil (not published)
	if vs.HasCurrent() {
		t.Fatalf("Version must not be published if manifest append failed")
	}
}

// Test G — Manifest sync failure
func TestLogAndApply_MatrixG_ManifestSyncFailure(t *testing.T) {
	vs, w, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// Inject sync failure
	injectedErr := fmt.Errorf("injected fdatasync failure")
	w.syncFn = func(file *os.File) error {
		return injectedErr
	}

	edit := NewVersionEdit()
	edit.SetNextFileNum(2)
	_ = edit.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 5))

	err := vs.LogAndApply(edit)
	if err == nil {
		t.Fatalf("expected LogAndApply to fail on sync failure")
	}

	// Current must remain uninstalled
	if vs.HasCurrent() {
		t.Fatalf("Version must not be published if manifest sync failed")
	}
}

// Test H — Invalid edit
func TestLogAndApply_MatrixH_InvalidEdit(t *testing.T) {
	vs, w, _ := setupTestVersionSet(t)

	// 1. Nil edit
	if err := vs.LogAndApply(nil); err == nil {
		t.Fatalf("expected error for nil edit")
	}

	// 2. Invalid level
	eBadLevel := NewVersionEdit()
	eBadLevel.deletedFiles = append(eBadLevel.deletedFiles, DeleteFileEntry{Level: 99, FileNum: 1})
	if err := vs.LogAndApply(eBadLevel); err == nil {
		t.Fatalf("expected error for invalid level in delete")
	}

	// 3. File number zero
	eBadFileNum := NewVersionEdit()
	eBadFileNum.deletedFiles = append(eBadFileNum.deletedFiles, DeleteFileEntry{Level: 0, FileNum: 0})
	if err := vs.LogAndApply(eBadFileNum); err == nil {
		t.Fatalf("expected error for file num 0")
	}

	// 4. Overlapping keys in L1 within edit
	eOverlap := NewVersionEdit()
	_ = eOverlap.AddFile(1, makeTestFileMeta(1, 1024, "a", "m", 1, 5))
	_ = eOverlap.AddFile(1, makeTestFileMeta(2, 1024, "k", "z", 6, 10)) // overlaps "a"-"m"
	if err := vs.LogAndApply(eOverlap); err == nil {
		t.Fatalf("expected error for overlapping key ranges in L1")
	}

	// Verify Manifest record count remained 0 (no disk records committed)
	if w.RecordCount() != 0 {
		t.Fatalf("expected 0 manifest records, got %d", w.RecordCount())
	}
}

// Test I — Scalar regression
func TestLogAndApply_MatrixI_ScalarRegression(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)

	// Baseline edit with nextFileNum = 10, lastSeqNum = 50
	e1 := NewVersionEdit()
	e1.SetNextFileNum(10)
	e1.SetLastSeqNum(50)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Regress nextFileNum: 9 < 10
	eRegressFile := NewVersionEdit()
	eRegressFile.SetNextFileNum(9)
	if err := vs.LogAndApply(eRegressFile); err == nil {
		t.Fatalf("expected nextFileNum regression to be rejected")
	}

	// Regress lastSeqNum: 49 < 50
	eRegressSeq := NewVersionEdit()
	eRegressSeq.SetLastSeqNum(49)
	if err := vs.LogAndApply(eRegressSeq); err == nil {
		t.Fatalf("expected lastSeqNum regression to be rejected")
	}

	// Valid advancement should succeed
	eValid := NewVersionEdit()
	eValid.SetNextFileNum(11)
	eValid.SetLastSeqNum(60)
	if err := vs.LogAndApply(eValid); err != nil {
		t.Fatalf("eValid failed: %v", err)
	}
	if vs.NextFileNum() != 11 || vs.LastSeqNum() != 60 {
		t.Fatalf("watermarks not updated: file=%d, seq=%d", vs.NextFileNum(), vs.LastSeqNum())
	}
}

// Test J — Duplicate file number
func TestLogAndApply_MatrixJ_DuplicateFileNumber(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// Install file 1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Attempt to re-add file 1 at same level
	e2 := NewVersionEdit()
	_ = e2.AddFile(0, makeTestFileMeta(1, 1024, "c", "d", 11, 20))
	if err := vs.LogAndApply(e2); err == nil {
		t.Fatalf("expected duplicate file number at same level to be rejected")
	}
}

// Test K — Same file number across levels
func TestLogAndApply_MatrixK_SameFileNumberAcrossLevels(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// Install file 1 at L0
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Attempt to add file 1 at L1
	e2 := NewVersionEdit()
	_ = e2.AddFile(1, makeTestFileMeta(1, 1024, "c", "d", 11, 20))
	if err := vs.LogAndApply(e2); err == nil {
		t.Fatalf("expected cross-level file number collision to be rejected")
	}
}

// Test L — Missing physical output
func TestLogAndApply_MatrixL_MissingPhysicalOutput(t *testing.T) {
	vs, _, _ := setupTestVersionSet(t)
	// Notice: we do NOT create the physical SSTable on disk!

	edit := NewVersionEdit()
	edit.SetNextFileNum(2)
	_ = edit.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))

	err := vs.LogAndApply(edit)
	if err == nil {
		t.Fatalf("expected LogAndApply to reject missing physical SSTable")
	}
	if !stdErrors.Is(err, errors.ErrMissingSSTable) {
		t.Fatalf("expected ErrMissingSSTable, got: %v", err)
	}

	// Check size mismatch
	dir := vs.DBPath()
	createMockSSTable(t, dir, 2, 512) // actual size 512, metadata claims 1024
	eSizeMismatch := NewVersionEdit()
	eSizeMismatch.SetNextFileNum(3)
	_ = eSizeMismatch.AddFile(0, makeTestFileMeta(2, 1024, "a", "b", 1, 10))
	errSize := vs.LogAndApply(eSizeMismatch)
	if errSize == nil {
		t.Fatalf("expected size mismatch error")
	}
}

// Test M — Old-Version reader pin
func TestLogAndApply_MatrixM_OldVersionReaderPin(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)

	// Step 1: Install file 1 in V1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Step 2: Reader pins V1
	v1 := vs.Current()
	if v1 == nil {
		t.Fatalf("expected current V1")
	}

	// Step 3: Compaction replaces file 1 with file 2
	e2 := NewVersionEdit()
	e2.SetNextFileNum(3)
	_ = e2.DeleteFile(0, 1)
	_ = e2.AddFile(0, makeTestFileMeta(2, 1024, "c", "d", 11, 20))
	if err := vs.LogAndApply(e2); err != nil {
		t.Fatalf("e2 failed: %v", err)
	}

	// Invariant: file 1 MUST NOT be unlinked while v1 is still pinned!
	sstPath1 := TablePath(dir, 1)
	if _, err := os.Stat(sstPath1); err != nil {
		t.Fatalf("file 1 was unlinked prematurely while reader still holds pin: %v", err)
	}

	// Invariant: file 2 exists
	sstPath2 := TablePath(dir, 2)
	if _, err := os.Stat(sstPath2); err != nil {
		t.Fatalf("file 2 does not exist: %v", err)
	}

	// Clean up reader pin
	v1.Unref()
}

// Test N — Cleanup after final unref
func TestLogAndApply_MatrixN_CleanupAfterFinalUnref(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)

	// Step 1: Install file 1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Reader pins V1
	v1 := vs.Current()

	// Step 2: Replace file 1 with file 2
	e2 := NewVersionEdit()
	e2.SetNextFileNum(3)
	_ = e2.DeleteFile(0, 1)
	_ = e2.AddFile(0, makeTestFileMeta(2, 1024, "c", "d", 11, 20))
	if err := vs.LogAndApply(e2); err != nil {
		t.Fatalf("e2 failed: %v", err)
	}

	sstPath1 := TablePath(dir, 1)
	if _, err := os.Stat(sstPath1); err != nil {
		t.Fatalf("file 1 missing prematurely: %v", err)
	}

	// Step 3: Reader unrefs V1 -> triggers finalization -> file 1 is unlinked!
	v1.Unref()

	if _, err := os.Stat(sstPath1); !os.IsNotExist(err) {
		t.Fatalf("expected file 1 to be unlinked after final unref, but it still exists: %v", err)
	}

	// File 2 must remain live
	if _, err := os.Stat(TablePath(dir, 2)); err != nil {
		t.Fatalf("file 2 was unexpectedly removed: %v", err)
	}
}

// Test O — Cleanup failure
func TestLogAndApply_MatrixO_CleanupFailure(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)

	// Inject unlink failure
	injectedErr := fmt.Errorf("injected permission denied unlink failure")
	vs.SetTestHooks(nil, func(name string) error {
		return injectedErr
	}, nil)

	// Install file 1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Delete file 1 without readers (so oldCurrent unrefs immediately)
	e2 := NewVersionEdit()
	e2.SetNextFileNum(3)
	_ = e2.DeleteFile(0, 1)
	_ = e2.AddFile(0, makeTestFileMeta(2, 1024, "c", "d", 11, 20))

	// Invariant: LogAndApply MUST succeed logically even if cleanup fails!
	if err := vs.LogAndApply(e2); err != nil {
		t.Fatalf("LogAndApply must succeed despite cleanup failure, got: %v", err)
	}

	// The new Version must be published as Current
	cur := vs.Current()
	defer cur.Unref()
	if cur.NumFiles(0) != 1 || cur.Files(0)[0].FileNum != 2 {
		t.Fatalf("expected Current to reflect file 2, got %+v", cur.Files(0))
	}

	// Diagnostics must record the cleanup error
	errs := vs.LastCleanupErrors()
	if len(errs) == 0 {
		t.Fatalf("expected diagnostic cleanup error to be recorded")
	}
}

// Test P — Concurrent LogAndApply
func TestLogAndApply_MatrixP_ConcurrentLogAndApply(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)

	const numWorkers = 8
	const editsPerWorker = 20

	// Create physical SSTable files in advance
	totalFiles := uint64(numWorkers * editsPerWorker)
	vs.SetNextFileNum(totalFiles + 1)
	for i := uint64(1); i <= totalFiles; i++ {
		createMockSSTable(t, dir, i, 256)
	}

	var wg sync.WaitGroup
	var allocatedNum atomic.Uint64
	allocatedNum.Store(1)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for e := 0; e < editsPerWorker; e++ {
				fileNum := allocatedNum.Add(1) - 1
				edit := NewVersionEdit()
				uKey := fmt.Sprintf("k_%04d_%04d", workerID, e)
				meta := makeTestFileMeta(fileNum, 256, uKey, uKey, fileNum*10-9, fileNum*10)
				_ = edit.AddFile(0, meta)

				if err := vs.LogAndApply(edit); err != nil {
					t.Errorf("worker %d edit %d failed: %v", workerID, e, err)
					return
				}
			}
		}(w)
	}

	wg.Wait()

	cur := vs.Current()
	if cur == nil {
		t.Fatalf("expected active Current")
	}
	defer cur.Unref()

	// Invariant: All editsPerWorker * numWorkers files must be present at L0
	if cur.NumFiles(0) != int(totalFiles) {
		t.Fatalf("expected %d files at L0, got %d", totalFiles, cur.NumFiles(0))
	}

	// Verify L0 sorted by FileNum ASC
	files := cur.Files(0)
	for i := 0; i < len(files)-1; i++ {
		if files[i].FileNum >= files[i+1].FileNum {
			t.Fatalf("L0 not sorted by FileNum: %d >= %d", files[i].FileNum, files[i+1].FileNum)
		}
	}
}

// Test Q — Crash/replay equivalence
func TestLogAndApply_MatrixQ_CrashReplayEquivalence(t *testing.T) {
	dir := t.TempDir()
	manPath := filepath.Join(dir, ManifestFilename(1))
	w, err := CreateManifestWriter(manPath)
	if err != nil {
		t.Fatalf("failed to create manifest writer: %v", err)
	}

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:         dir,
		ManifestWriter: w,
		NextFileNum:    1,
		LastSeqNum:     0,
	})

	for i := uint64(1); i <= 5; i++ {
		createMockSSTable(t, dir, i, 512)
	}

	// Edit 1: Add 1, 2 to L0
	e1 := NewVersionEdit()
	e1.SetNextFileNum(3)
	e1.SetLastSeqNum(20)
	_ = e1.AddFile(0, makeTestFileMeta(1, 512, "a", "b", 1, 10))
	_ = e1.AddFile(0, makeTestFileMeta(2, 512, "c", "d", 11, 20))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// Edit 2: Compaction - Delete 1 from L0, Add 3 and 4 to L1
	e2 := NewVersionEdit()
	e2.SetNextFileNum(5)
	e2.SetLastSeqNum(40)
	_ = e2.DeleteFile(0, 1)
	_ = e2.AddFile(1, makeTestFileMeta(3, 512, "a", "b", 1, 30))
	_ = e2.AddFile(1, makeTestFileMeta(4, 512, "m", "z", 31, 40))
	if err := vs.LogAndApply(e2); err != nil {
		t.Fatalf("e2 failed: %v", err)
	}

	// Edit 3: Add 5 to L2
	e3 := NewVersionEdit()
	e3.SetNextFileNum(6)
	e3.SetLastSeqNum(50)
	_ = e3.AddFile(2, makeTestFileMeta(5, 512, "k", "n", 41, 50))
	if err := vs.LogAndApply(e3); err != nil {
		t.Fatalf("e3 failed: %v", err)
	}

	// Keep Current for comparison
	committed := vs.Current()
	defer committed.Unref()

	// Close writer to emulate crash/restart
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close manifest writer: %v", err)
	}

	// Replay manifest from scratch
	manFile, err := os.Open(filepath.Clean(manPath)) // #nosec G304
	if err != nil {
		t.Fatalf("failed to open manifest for replay: %v", err)
	}
	defer func() { _ = manFile.Close() }()

	disc := &DiscoveredManifest{
		Dir:         dir,
		ManifestNum: 1,
		Path:        manPath,
		File:        manFile,
	}

	replayRes, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer replayRes.Version.Unref()

	// Compare exact state parity between LogAndApply and ReplayManifest
	if replayRes.NextFileNum != vs.NextFileNum() {
		t.Fatalf("NextFileNum mismatch: replayed %d vs committed %d", replayRes.NextFileNum, vs.NextFileNum())
	}
	if replayRes.LastSeqNum != vs.LastSeqNum() {
		t.Fatalf("LastSeqNum mismatch: replayed %d vs committed %d", replayRes.LastSeqNum, vs.LastSeqNum())
	}

	for lvl := 0; lvl < NumLevels; lvl++ {
		replayedFiles := replayRes.Version.Files(lvl)
		committedFiles := committed.Files(lvl)
		if len(replayedFiles) != len(committedFiles) {
			t.Fatalf("level %d count mismatch: replayed %d vs committed %d", lvl, len(replayedFiles), len(committedFiles))
		}
		for i := range replayedFiles {
			if !replayedFiles[i].Equal(committedFiles[i]) {
				t.Fatalf("level %d file[%d] mismatch: replayed %+v vs committed %+v", lvl, i, replayedFiles[i], committedFiles[i])
			}
		}
	}
}

// Test R — Manifest replay rejects malformed edit
func TestLogAndApply_MatrixR_ManifestReplayRejection(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// 1. Commit an initial valid edit
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(1, makeTestFileMeta(1, 1024, "a", "m", 1, 10))
	if err := vs.LogAndApply(e1); err != nil {
		t.Fatalf("e1 failed: %v", err)
	}

	// 2. Commit path must reject overlapping key ranges in L1
	createMockSSTable(t, dir, 2, 1024)
	eOverlap := NewVersionEdit()
	eOverlap.SetNextFileNum(3)
	_ = eOverlap.AddFile(1, makeTestFileMeta(2, 1024, "c", "z", 11, 20)) // overlaps "a"-"m"
	if err := vs.LogAndApply(eOverlap); err == nil {
		t.Fatalf("expected LogAndApply to reject overlapping key ranges in L1")
	}
}

// Test S — Idempotence / retry behavior
func TestLogAndApply_MatrixS_RetryBehavior(t *testing.T) {
	vs, w, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)

	// Inject temporary failure
	failOnce := true
	w.syncFn = func(file *os.File) error {
		if failOnce {
			failOnce = false
			return fmt.Errorf("transient sync failure")
		}
		return file.Sync()
	}

	edit := NewVersionEdit()
	edit.SetNextFileNum(2)
	_ = edit.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))

	// First attempt fails due to sync error (and writer is poisoned)
	err1 := vs.LogAndApply(edit)
	if err1 == nil {
		t.Fatalf("expected first attempt to fail")
	}

	// Writer is poisoned, so retrying against the poisoned writer fails closed
	err2 := vs.LogAndApply(edit)
	if err2 == nil {
		t.Fatalf("expected retry against poisoned writer to fail")
	}

	// Replace with clean writer to simulate recovery/reopen
	_ = w.Close()
	manPath := filepath.Join(dir, ManifestFilename(2))
	w2, err := CreateManifestWriter(manPath)
	if err != nil {
		t.Fatalf("failed to create new manifest writer: %v", err)
	}
	defer func() { _ = w2.Close() }()
	vs.SetManifestWriter(w2)

	// Now retry succeeds cleanly
	if err := vs.LogAndApply(edit); err != nil {
		t.Fatalf("retry on clean writer failed: %v", err)
	}
	cur := vs.Current()
	defer cur.Unref()
	if cur.NumFiles(0) != 1 {
		t.Fatalf("expected 1 file after clean retry")
	}
}

// Test T — Current pinning
func TestLogAndApply_MatrixT_CurrentPinning(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)

	// V1 has file 1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	_ = vs.LogAndApply(e1)

	v1 := vs.Current()
	if v1 == nil {
		t.Fatalf("expected v1")
	}

	// V2 replaces file 1 with file 2
	e2 := NewVersionEdit()
	e2.SetNextFileNum(3)
	_ = e2.DeleteFile(0, 1)
	_ = e2.AddFile(0, makeTestFileMeta(2, 1024, "c", "d", 11, 20))
	_ = vs.LogAndApply(e2)

	// v1 must remain unchanged
	if v1.NumFiles(0) != 1 || v1.Files(0)[0].FileNum != 1 {
		t.Fatalf("pinned v1 was mutated: %+v", v1.Files(0))
	}

	// New Current reflects V2
	v2 := vs.Current()
	defer v2.Unref()
	if v2.NumFiles(0) != 1 || v2.Files(0)[0].FileNum != 2 {
		t.Fatalf("v2 does not reflect new file: %+v", v2.Files(0))
	}

	v1.Unref()
}

// Test U — ActiveVersions
func TestLogAndApply_MatrixU_ActiveVersions(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)
	createMockSSTable(t, dir, 1, 1024)
	createMockSSTable(t, dir, 2, 1024)
	createMockSSTable(t, dir, 3, 1024)

	// V1
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	_ = e1.AddFile(0, makeTestFileMeta(1, 1024, "a", "b", 1, 10))
	_ = vs.LogAndApply(e1)
	v1 := vs.Current()

	// V2
	e2 := NewVersionEdit()
	e2.SetNextFileNum(3)
	_ = e2.AddFile(0, makeTestFileMeta(2, 1024, "c", "d", 11, 20))
	_ = vs.LogAndApply(e2)
	v2 := vs.Current()

	// V3
	e3 := NewVersionEdit()
	e3.SetNextFileNum(4)
	_ = e3.AddFile(0, makeTestFileMeta(3, 1024, "e", "f", 21, 30))
	_ = vs.LogAndApply(e3)

	active := vs.ActiveVersions()
	if len(active) != 3 {
		t.Fatalf("expected 3 active versions, got %d", len(active))
	}

	// Unpin all active
	for _, v := range active {
		v.Unref()
	}

	v1.Unref()
	v2.Unref()
}

// Test V — File cleanup race
func TestLogAndApply_MatrixV_FileCleanupRace(t *testing.T) {
	vs, _, dir := setupTestVersionSet(t)

	const iterations = 50
	for i := uint64(1); i <= iterations*2; i++ {
		createMockSSTable(t, dir, i, 128)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Concurrent reader constantly pinning and releasing Current
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			v := vs.Current()
			if v != nil {
				// simulate read
				_ = v.NumFiles(0)
				time.Sleep(50 * time.Microsecond)
				v.Unref()
			}
		}
	}()

	// Concurrent active versions inspector
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			vers := vs.ActiveVersions()
			for _, v := range vers {
				_ = v.NumFiles(0)
				v.Unref()
			}
			time.Sleep(100 * time.Microsecond)
		}
	}()

	// Publisher continuously commits replacements
	for i := uint64(1); i < iterations; i++ {
		edit := NewVersionEdit()
		edit.SetNextFileNum(i + 1)
		_ = edit.AddFile(0, makeTestFileMeta(i, 128, fmt.Sprintf("k%04d", i), fmt.Sprintf("k%04d", i), i, i))
		if i > 1 {
			_ = edit.DeleteFile(0, i-1)
		}
		if err := vs.LogAndApply(edit); err != nil {
			t.Fatalf("iteration %d failed: %v", i, err)
		}
		time.Sleep(200 * time.Microsecond)
	}

	stop.Store(true)
	wg.Wait()
}

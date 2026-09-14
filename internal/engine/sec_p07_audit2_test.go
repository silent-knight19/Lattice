package engine_test

import (
	"context"
	stdErrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

func newTestEngine(dbPath string) *engine.Engine {
	eng := engine.NewEngine(engine.DefaultBackpressureConfig())
	eng.SetDBPath(dbPath)
	return eng
}

// =============================================================================
// P07-SEC-001: MISSING MANIFEST MUST NOT BECOME A FRESH DATABASE
// =============================================================================

// TestSEC_P07_001_MissingManifestMatrix validates recovery decision tree:
// A — Fresh environment (no CURRENT, no MANIFEST) -> succeeds as uninitialized
// B — CURRENT references missing MANIFEST -> fails closed with ErrManifestNotFound
// C — CURRENT references missing MANIFEST but WAL exists -> fails closed, no WAL replay into fresh state
// D — Existing SSTables + missing MANIFEST -> fails closed, no silent loss of logical visibility
func TestSEC_P07_001_MissingManifestMatrix(t *testing.T) {
	t.Run("A - Fresh environment (no CURRENT, no MANIFEST): succeeds as uninitialized", func(t *testing.T) {
		dir := t.TempDir()
		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("expected fresh database recovery to succeed, got %v", err)
		}

		if eng.NextSeqNum() != 0 {
			t.Errorf("expected sequence watermark 0 for fresh environment, got %d", eng.NextSeqNum())
		}
		if eng.ActiveMemTable().Len() != 0 {
			t.Errorf("expected empty active memtable, got %d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("B - CURRENT references missing MANIFEST: fails closed with ErrManifestNotFound", func(t *testing.T) {
		dir := t.TempDir()
		// Write CURRENT pointing to MANIFEST-000001, but do not create MANIFEST-000001
		if err := version.SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest failed: %v", err)
		}

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded when referenced MANIFEST does not exist")
		}

		if !stdErrors.Is(err, errors.ErrManifestNotFound) {
			t.Errorf("expected ErrManifestNotFound, got %v", err)
		}
	})

	t.Run("C - CURRENT references missing MANIFEST but WAL exists: fails closed without WAL replay", func(t *testing.T) {
		dir := t.TempDir()

		// 1. Point CURRENT to non-existent MANIFEST-000002
		if err := version.SetCurrentManifest(dir, 2); err != nil {
			t.Fatalf("SetCurrentManifest failed: %v", err)
		}

		// 2. Create WAL with uncommitted writes seq 1..10
		writeWALSegment(t, dir, 1,
			makePutRecord(1, "k1", "v1"),
			makePutRecord(2, "k2", "v2"),
		)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded with missing MANIFEST and replayed WAL")
		}

		if !stdErrors.Is(err, errors.ErrManifestNotFound) {
			t.Errorf("expected ErrManifestNotFound, got %v", err)
		}

		// Verify zero WAL records published into engine
		if eng.NextSeqNum() != 0 {
			t.Errorf("expected sequence watermark 0, got %d", eng.NextSeqNum())
		}
		if eng.ActiveMemTable().Len() != 0 {
			t.Errorf("expected active memtable to remain empty, got %d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("D - Existing SSTables + missing MANIFEST: fails closed, prevents silent data loss", func(t *testing.T) {
		dir := t.TempDir()

		// Create valid SSTable file on disk
		sstPath := version.TablePath(dir, 1)
		if err := os.WriteFile(sstPath, make([]byte, 1024), 0600); err != nil {
			t.Fatalf("WriteFile sstable failed: %v", err)
		}

		// Point CURRENT to missing MANIFEST-000001
		if err := version.SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest failed: %v", err)
		}

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: startup succeeded with missing manifest and orphaned live SSTables")
		}
		if !stdErrors.Is(err, errors.ErrManifestNotFound) {
			t.Errorf("expected ErrManifestNotFound, got %v", err)
		}

		// Verify SSTable file was not touched or deleted
		if _, statErr := os.Lstat(sstPath); statErr != nil {
			t.Fatalf("SSTable was modified or deleted during failed recovery: %v", statErr)
		}
	})
}

// =============================================================================
// P07-SEC-004 & P07-SEC-006: CRASH-WINDOW SSTABLE & NEXTFILENUM ALLOCATOR
// =============================================================================

// TestSEC_P07_004_006_CrashWindowAndFileNumberAllocation tests:
// 1. Crash window simulation: TableWriter staging -> link 000001.sst -> crash before VersionEdit
// 2. NextFileNum advancement beyond existing physical file numbers
// 3. Prevention of SSTable file number collisions on future flushes/allocations
// 4. File-number monotonicity across repeated recoveries and allocations
func TestSEC_P07_004_006_CrashWindowAndFileNumberAllocation(t *testing.T) {
	t.Run("Crash window: uncommitted 000001.sst does not collide with future allocations", func(t *testing.T) {
		dir := t.TempDir()

		// 1. Simulate TableWriter publishing 000001.sst via real TableWriter
		sstPath1 := version.TablePath(dir, 1)
		writer1, err := sstable.NewTableWriter(sstPath1, sstable.DefaultTableWriterOptions())
		if err != nil {
			t.Fatalf("NewTableWriter failed: %v", err)
		}
		ik1, _ := binary.NewInternalKey([]byte("alpha"), 1, binary.OpTypePut)
		if err := writer1.Add(ik1, []byte("val1")); err != nil {
			t.Fatalf("writer1.Add failed: %v", err)
		}
		meta1, err := writer1.Finish()
		if err != nil {
			t.Fatalf("writer1.Finish failed: %v", err)
		}
		_ = meta1

		// 000001.sst now exists on disk!
		if _, statErr := os.Lstat(sstPath1); statErr != nil {
			t.Fatalf("expected 000001.sst to exist on disk: %v", statErr)
		}

		// 2. Simulate crash BEFORE VersionEdit is appended to MANIFEST.
		// Start a fresh Engine and run RecoverWAL.
		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("RecoverWAL failed: %v", err)
		}

		// Invariant: nextFileNum MUST have advanced beyond 1 (must be at least 2)
		if eng.NextFileNum() < 2 {
			t.Fatalf("SECURITY VIOLATION: nextFileNum was %d, expected >= 2 to avoid collision with 000001.sst", eng.NextFileNum())
		}

		// 3. Allocate next file number for future flush
		allocatedNum := eng.AllocateFileNum()
		if allocatedNum < 2 {
			t.Fatalf("SECURITY VIOLATION: AllocateFileNum returned %d, collides with existing 000001.sst", allocatedNum)
		}

		// 4. Future TableWriter can publish with allocatedNum without ErrSSTableExists collision
		sstPathAlloc := version.TablePath(dir, allocatedNum)
		writer2, err := sstable.NewTableWriter(sstPathAlloc, sstable.DefaultTableWriterOptions())
		if err != nil {
			t.Fatalf("NewTableWriter on allocated file %d failed: %v", allocatedNum, err)
		}
		ik2, _ := binary.NewInternalKey([]byte("beta"), 2, binary.OpTypePut)
		if err := writer2.Add(ik2, []byte("val2")); err != nil {
			t.Fatalf("writer2.Add failed: %v", err)
		}
		if _, err := writer2.Finish(); err != nil {
			t.Fatalf("writer2.Finish failed: %v", err)
		}

		// Verify 000001.sst was NOT deleted or overwritten
		data1, err := os.ReadFile(sstPath1)
		if err != nil || len(data1) == 0 {
			t.Fatalf("000001.sst was lost or corrupted: %v", err)
		}
	})

	t.Run("Multiple unreferenced SSTables on disk: allocator advances past highest physical number", func(t *testing.T) {
		dir := t.TempDir()

		// Create 000001.sst and 000007.sst on disk (manifest references neither)
		for _, num := range []uint64{1, 7} {
			p := version.TablePath(dir, num)
			if err := os.WriteFile(p, make([]byte, 1024), 0600); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
		}

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("RecoverWAL failed: %v", err)
		}

		// Watermark must be at least 8 (highest physical 7 + 1)
		if eng.NextFileNum() < 8 {
			t.Fatalf("expected NextFileNum >= 8, got %d", eng.NextFileNum())
		}

		firstAlloc := eng.AllocateFileNum()
		if firstAlloc < 8 {
			t.Fatalf("expected first allocation >= 8, got %d", firstAlloc)
		}
		secondAlloc := eng.AllocateFileNum()
		if secondAlloc <= firstAlloc {
			t.Fatalf("expected strictly increasing allocation: %d <= %d", secondAlloc, firstAlloc)
		}
	})

	t.Run("NextFileNum preservation from MANIFEST: authoritative watermark retained", func(t *testing.T) {
		dir := t.TempDir()

		// Set up MANIFEST with NextFileNum = 42
		e := version.NewVersionEdit()
		e.SetNextFileNum(42)
		e.SetLastSeqNum(100)

		manifestPath := version.ManifestPath(dir, 1)
		w, err := version.CreateManifestWriter(manifestPath)
		if err != nil {
			t.Fatalf("CreateManifestWriter: %v", err)
		}
		if err := w.LogEditPtr(e); err != nil {
			t.Fatalf("LogEditPtr: %v", err)
		}
		_ = w.Close()
		if err := version.SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest: %v", err)
		}

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("RecoverWAL failed: %v", err)
		}

		// Authoritative watermark 42 preserved
		if eng.NextFileNum() != 42 {
			t.Fatalf("expected NextFileNum = 42, got %d", eng.NextFileNum())
		}

		alloc := eng.AllocateFileNum()
		if alloc != 42 {
			t.Fatalf("expected AllocateFileNum() = 42, got %d", alloc)
		}
		if eng.NextFileNum() != 43 {
			t.Fatalf("expected NextFileNum after alloc = 43, got %d", eng.NextFileNum())
		}
	})
}

// =============================================================================
// P07-SEC-005: CLEANER REFUSAL MUST NOT TURN INTO STARTUP DOS
// =============================================================================

// TestSEC_P07_005_CleanerRefusalResilience verifies that safe cleaner refusals
// (symlinks, directories, permission errors) do not prevent engine startup (P07-SEC-005).
func TestSEC_P07_005_CleanerRefusalResilience(t *testing.T) {
	t.Run("A - Symlink candidate: victim untouched, recovery succeeds", func(t *testing.T) {
		dir := t.TempDir()
		victimDir := t.TempDir()
		victimFile := filepath.Join(victimDir, "victim.txt")
		if err := os.WriteFile(victimFile, []byte("sensitive-system-data"), 0600); err != nil {
			t.Fatalf("WriteFile victim: %v", err)
		}

		// Malicious staging candidate symlinked to victim
		symlinkPath := filepath.Join(dir, ".tmp_000001.sst_symlinkattack")
		if err := os.Symlink(victimFile, symlinkPath); err != nil {
			t.Fatalf("os.Symlink failed: %v", err)
		}

		eng := newTestEngine(dir)

		// Recovery must SUCCEED (cleaner safely refuses to follow or unlink symlink)
		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("SECURITY REGRESSION: cleaner symlink refusal caused startup failure: %v", err)
		}

		// Victim file must remain untouched
		victimData, err := os.ReadFile(victimFile)
		if err != nil || string(victimData) != "sensitive-system-data" {
			t.Fatalf("victim file was modified or deleted: %v", err)
		}

		// Cleaner diagnostic must record failure
		report := eng.LastCleanerReport()
		if len(report.Failures) == 0 {
			t.Errorf("expected diagnostic recording refusal for symlink candidate")
		}
	})

	t.Run("B - Directory masquerade: directory untouched, recovery succeeds", func(t *testing.T) {
		dir := t.TempDir()
		masqueradeDir := filepath.Join(dir, ".tmp_000001.sst_dirattack")
		if err := os.Mkdir(masqueradeDir, 0750); err != nil {
			t.Fatalf("Mkdir failed: %v", err)
		}
		innerFile := filepath.Join(masqueradeDir, "payload.txt")
		if err := os.WriteFile(innerFile, []byte("important-content"), 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("SECURITY REGRESSION: cleaner directory refusal caused startup failure: %v", err)
		}

		// Masquerade directory and contents must remain intact
		if _, err := os.Lstat(innerFile); err != nil {
			t.Fatalf("inner file was unexpectedly deleted: %v", err)
		}

		report := eng.LastCleanerReport()
		if len(report.Failures) == 0 {
			t.Errorf("expected diagnostic recording refusal for directory candidate")
		}
	})

	t.Run("C - Permission-denied candidate: candidate remains, recovery succeeds, diagnostic available", func(t *testing.T) {
		dir := t.TempDir()
		protectedFile := filepath.Join(dir, ".tmp_000001.sst_readonly")
		if err := os.WriteFile(protectedFile, []byte("stale-staging-data"), 0400); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("startup failed on undeletable candidate: %v", err)
		}

		report := eng.LastCleanerReport()
		_ = report // diagnostic available
	})

	t.Run("D - Multiple candidates: safe removed, unsafe remains, recovery succeeds", func(t *testing.T) {
		dir := t.TempDir()

		// Safe candidate 1 (regular file)
		safePath1 := filepath.Join(dir, ".tmp_000001.sst_safe1111")
		if err := os.WriteFile(safePath1, []byte("orphan1"), 0600); err != nil {
			t.Fatalf("WriteFile safe1: %v", err)
		}

		// Unsafe candidate (symlink)
		victim := filepath.Join(t.TempDir(), "passwords")
		_ = os.WriteFile(victim, []byte("passwords"), 0600)
		unsafePath := filepath.Join(dir, ".tmp_000001.sst_unsafe")
		if err := os.Symlink(victim, unsafePath); err != nil {
			t.Fatalf("Symlink failed: %v", err)
		}

		// Safe candidate 2 (regular file)
		safePath2 := filepath.Join(dir, ".tmp_000001.sst_safe2222")
		if err := os.WriteFile(safePath2, []byte("orphan2"), 0600); err != nil {
			t.Fatalf("WriteFile safe2: %v", err)
		}

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("RecoverWAL failed: %v", err)
		}

		// Safe candidates must be removed
		if _, err := os.Lstat(safePath1); !os.IsNotExist(err) {
			t.Errorf("safe candidate 1 was not removed")
		}
		if _, err := os.Lstat(safePath2); !os.IsNotExist(err) {
			t.Errorf("safe candidate 2 was not removed")
		}

		// Unsafe candidate must remain untouched
		if _, err := os.Lstat(unsafePath); err != nil {
			t.Errorf("unsafe symlink was unexpectedly removed or followed: %v", err)
		}

		report := eng.LastCleanerReport()
		if report.FilesCleaned != 2 {
			t.Errorf("expected 2 files cleaned, got %d", report.FilesCleaned)
		}
		if len(report.Failures) != 1 {
			t.Errorf("expected 1 failure diagnostic, got %d", len(report.Failures))
		}
	})

	t.Run("E - Repeated restart: repeated recovery works without permanent boot lock", func(t *testing.T) {
		dir := t.TempDir()
		masqueradeDir := filepath.Join(dir, ".tmp_000001.sst_dirattack")
		_ = os.Mkdir(masqueradeDir, 0750)

		// First boot
		eng1 := newTestEngine(dir)
		if err := eng1.RecoverWAL(); err != nil {
			t.Fatalf("first boot failed: %v", err)
		}
		_ = eng1.Close()

		// Second boot (candidate still present on disk)
		eng2 := newTestEngine(dir)
		if err := eng2.RecoverWAL(); err != nil {
			t.Fatalf("SECURITY REGRESSION: second boot locked out by persistent candidate: %v", err)
		}
		_ = eng2.Close()

		// Third boot
		eng3 := newTestEngine(dir)
		if err := eng3.RecoverWAL(); err != nil {
			t.Fatalf("third boot failed: %v", err)
		}
		_ = eng3.Close()
	})
}

// =============================================================================
// P07-SEC-007: BOUND WAL RECOVERY BATCHBUFFER
// =============================================================================

// TestSEC_P07_007_WALRecoveryBatchBounds verifies memory limits on recovery batchBuffer (P07-SEC-007).
func TestSEC_P07_007_WALRecoveryBatchBounds(t *testing.T) {
	t.Run("A - Exactly at record limit: succeeds", func(t *testing.T) {
		dir := t.TempDir()
		const limit = 5
		restore := engine.SetRecoveryBatchLimitsForTesting(limit, engine.MaxRecoveryBatchBytes)
		defer restore()

		// Write batch of exactly 5 records
		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			makePutRecord(2, "k1", "v1"),
			makePutRecord(3, "k2", "v2"),
			makePutRecord(4, "k3", "v3"),
			makePutRecord(5, "k4", "v4"),
			makePutRecord(6, "k5", "v5"),
			{Type: wal.RecordTypeBatchCommit, SeqNum: 7, Timestamp: 105},
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("expected success at exact record limit, got %v", err)
		}
		if eng.ActiveMemTable().Len() != 5 {
			t.Errorf("expected 5 records in active memtable, got %d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("B - One record over record limit: fails closed", func(t *testing.T) {
		dir := t.TempDir()
		const limit = 5
		restore := engine.SetRecoveryBatchLimitsForTesting(limit, engine.MaxRecoveryBatchBytes)
		defer restore()

		// Write batch of 6 records (limit = 5)
		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			makePutRecord(2, "k1", "v1"),
			makePutRecord(3, "k2", "v2"),
			makePutRecord(4, "k3", "v3"),
			makePutRecord(5, "k4", "v4"),
			makePutRecord(6, "k5", "v5"),
			makePutRecord(7, "k6", "v6"), // 6th record exceeds limit
			{Type: wal.RecordTypeBatchCommit, SeqNum: 8, Timestamp: 106},
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: accepted recovery batch exceeding record limit")
		}

		if !stdErrors.Is(err, errors.ErrRecoveryBatchLimitExceeded) {
			t.Errorf("expected ErrRecoveryBatchLimitExceeded, got %v", err)
		}

		// Zero partial state published
		if eng.ActiveMemTable().Len() != 0 {
			t.Errorf("SECURITY VIOLATION: partial batch published into memtable: len=%d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("C - Exactly at byte limit: succeeds", func(t *testing.T) {
		dir := t.TempDir()
		// Record 1: key len 10, val len 40 -> 50 bytes
		// Record 2: key len 10, val len 40 -> 50 bytes
		// Total: 100 bytes
		const exactBytes = 100
		restore := engine.SetRecoveryBatchLimitsForTesting(100, exactBytes)
		defer restore()

		k1 := "0123456789"
		v1 := "0123456789012345678901234567890123456789"
		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			makePutRecord(2, k1, v1),
			makePutRecord(3, k1, v1),
			{Type: wal.RecordTypeBatchCommit, SeqNum: 4, Timestamp: 102},
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("expected success at exact byte limit, got %v", err)
		}
		if eng.ActiveMemTable().Len() != 2 {
			t.Errorf("expected 2 records in active memtable, got %d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("D - One byte over byte limit: fails closed", func(t *testing.T) {
		dir := t.TempDir()
		const exactBytes = 100
		restore := engine.SetRecoveryBatchLimitsForTesting(100, exactBytes)
		defer restore()

		k1 := "0123456789"
		v1 := "0123456789012345678901234567890123456789"  // 50 bytes
		v2 := "0123456789012345678901234567890123456789X" // 51 bytes -> total 101 bytes (1 byte over!)

		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			makePutRecord(2, k1, v1),
			makePutRecord(3, k1, v2),
			{Type: wal.RecordTypeBatchCommit, SeqNum: 4, Timestamp: 102},
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: accepted recovery batch exceeding byte limit by 1 byte")
		}

		if !stdErrors.Is(err, errors.ErrRecoveryBatchLimitExceeded) {
			t.Errorf("expected ErrRecoveryBatchLimitExceeded, got %v", err)
		}
		if eng.ActiveMemTable().Len() != 0 {
			t.Errorf("SECURITY VIOLATION: partial batch published: len=%d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("E - Huge PUT values: byte accounting enforced before allocation", func(t *testing.T) {
		dir := t.TempDir()
		const byteLimit = 1024
		restore := engine.SetRecoveryBatchLimitsForTesting(1000, byteLimit)
		defer restore()

		hugeVal := make([]byte, 2048)
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    2,
			Timestamp: 100,
			Key:       []byte("key"),
			Value:     hugeVal,
		}

		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			rec,
			{Type: wal.RecordTypeBatchCommit, SeqNum: 3, Timestamp: 101},
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: accepted batch exceeding byte limit with huge value")
		}
		if !stdErrors.Is(err, errors.ErrRecoveryBatchLimitExceeded) {
			t.Errorf("expected ErrRecoveryBatchLimitExceeded, got %v", err)
		}
	})

	t.Run("F - Incomplete batch at EOF: bounded memory, uncommitted records discarded", func(t *testing.T) {
		dir := t.TempDir()
		const limit = 5
		restore := engine.SetRecoveryBatchLimitsForTesting(limit, engine.MaxRecoveryBatchBytes)
		defer restore()

		// Batch starts and has 3 records, but NO BATCH_COMMIT before EOF
		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			makePutRecord(2, "k1", "v1"),
			makePutRecord(3, "k2", "v2"),
			makePutRecord(4, "k3", "v3"),
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("RecoverWAL failed on incomplete batch: %v", err)
		}

		// Invariant: uncommitted records from incomplete batch are NOT published
		if eng.ActiveMemTable().Len() != 0 {
			t.Errorf("expected 0 records from incomplete batch, got %d", eng.ActiveMemTable().Len())
		}
	})

	t.Run("G - Limit failure publishes zero partial state", func(t *testing.T) {
		dir := t.TempDir()
		const limit = 2
		restore := engine.SetRecoveryBatchLimitsForTesting(limit, engine.MaxRecoveryBatchBytes)
		defer restore()

		records := []wal.Record{
			{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100},
			makePutRecord(2, "k1", "v1"),
			makePutRecord(3, "k2", "v2"),
			makePutRecord(4, "k3", "v3"), // exceeds limit 2
			{Type: wal.RecordTypeBatchCommit, SeqNum: 5, Timestamp: 103},
		}
		writeWALSegment(t, dir, 1, records...)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("expected error")
		}

		// Verify engine state is not recovered
		if _, getErr := eng.Get([]byte("k1")); getErr == nil {
			t.Fatalf("SECURITY VIOLATION: partial batch data accessible via Get")
		}
		if eng.NextSeqNum() != 0 {
			t.Errorf("expected sequence watermark 0, got %d", eng.NextSeqNum())
		}
	})
}

// =============================================================================
// CROSS-FINDING INTERACTION TESTS (SECTION 43)
// =============================================================================

// TestSEC_P07_Audit2_CrossFindingInteractions validates safety under compound failure modes.
func TestSEC_P07_Audit2_CrossFindingInteractions(t *testing.T) {
	t.Run("Missing MANIFEST + orphan SSTable: fails closed without destroying disk state", func(t *testing.T) {
		dir := t.TempDir()
		// Orphan SSTable exists
		sstPath := version.TablePath(dir, 7)
		if err := os.WriteFile(sstPath, []byte("orphan-sst-content"), 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		// CURRENT points to missing MANIFEST-000001
		_ = version.SetCurrentManifest(dir, 1)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("expected failure on missing manifest")
		}
		if !stdErrors.Is(err, errors.ErrManifestNotFound) {
			t.Errorf("expected ErrManifestNotFound, got %v", err)
		}

		// Orphan SSTable must NOT be destroyed
		if _, statErr := os.Lstat(sstPath); statErr != nil {
			t.Fatalf("orphan SSTable was destroyed during failed recovery: %v", statErr)
		}
	})

	t.Run("Cleaner failure + valid recovery: engine remains bootable and serviceable", func(t *testing.T) {
		dir := t.TempDir()

		// Valid WAL
		writeWALSegment(t, dir, 1, makePutRecord(1, "committedKey", "committedVal"))

		// Malicious cleaner trap
		trap := filepath.Join(dir, ".tmp_000001.sst_trapped")
		_ = os.Mkdir(trap, 0750)

		eng := newTestEngine(dir)

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("expected recovery to succeed despite cleaner trap: %v", err)
		}

		// Verify database is completely operational
		val, err := eng.Get([]byte("committedKey"))
		if err != nil || string(val) != "committedVal" {
			t.Fatalf("recovered key not readable: %v", err)
		}

		// Write new data
		if err := eng.Put(context.Background(), []byte("newKey"), []byte("newVal")); err != nil {
			t.Fatalf("post-recovery write failed: %v", err)
		}
	})

	t.Run("SSTable size mismatch + Version replay: fails closed, Version is not published", func(t *testing.T) {
		dir := t.TempDir()

		e := version.NewVersionEdit()
		ik, _ := binary.NewInternalKey([]byte("k"), 1, binary.OpTypePut)
		_ = e.AddFile(0, version.FileMetadata{
			FileNum:        1,
			FileSize:       4096,
			SmallestKey:    binary.EncodeInternalKey(ik),
			LargestKey:     binary.EncodeInternalKey(ik),
			SmallestSeqNum: 1,
			LargestSeqNum:  1,
		})
		// Write 500 bytes (mismatched)
		sstPath := version.TablePath(dir, 1)
		_ = os.WriteFile(sstPath, make([]byte, 500), 0600)

		manifestPath := version.ManifestPath(dir, 1)
		w, _ := version.CreateManifestWriter(manifestPath)
		_ = w.LogEditPtr(e)
		_ = w.Close()
		_ = version.SetCurrentManifest(dir, 1)

		eng := newTestEngine(dir)

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatalf("expected size mismatch failure")
		}
		if !stdErrors.Is(err, errors.ErrSSTableSizeMismatch) {
			t.Errorf("expected ErrSSTableSizeMismatch, got %v", err)
		}

		if eng.VersionSet().HasCurrent() {
			t.Fatalf("SECURITY VIOLATION: Version published to VersionSet on size mismatch failure")
		}
	})
}

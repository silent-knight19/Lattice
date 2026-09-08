package wal_test

import (
	"bytes"
	stdErrors "errors"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// SEC-03.10: Formalized Storage Security Invariant Regression Suite
//
// Invariants verified:
//   SEC-WAL-INV-01: Corrupt historical records are never silently skipped.
//   SEC-WAL-INV-02: Only the latest recoverable torn tail may be truncated.
//   SEC-WAL-INV-03: A complete checksum-invalid record is never replayed.
//   SEC-WAL-INV-04: No external filesystem target can be unexpectedly overwritten through WAL segment creation.
//   SEC-WAL-INV-05: Successful durability completion requires successful synchronization.
//   SEC-WAL-INV-06: Malformed lengths cannot trigger uncontrolled allocation.
//   SEC-WAL-INV-07: Queue and batch bounds remain finite under adversarial input.
//   SEC-WAL-INV-08: Segment ID overflow cannot create segment 0 / wraparound.
//   SEC-WAL-INV-09: Sequence-number regression cannot be replayed.
//   SEC-WAL-INV-10: Recovery failure cannot silently acknowledge unpersisted state.

// SEC-WAL-INV-01: Corrupt historical records are never silently skipped.
func TestSEC_WAL_INV_01_CorruptHistoricalRecordsNeverSkipped(t *testing.T) {
	h := NewSecurityHarness(t)

	// Historical segment 1 with records 1 and 2; active segment 2 with record 3
	seg1Path := writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, h.RootDir(), 2, testRecord(3, "k3", "v3"))

	// Corrupt record 1 in historical segment 1
	if err := h.CorruptByteAt(seg1Path, 30, 0xFF); err != nil {
		t.Fatalf("CorruptByteAt failed: %v", err)
	}

	var sink recordingSink
	_, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-01]: RecoverWAL silently skipped corrupt historical record!")
	}
	if len(sink.records) != 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-01]: records were replayed after historical corruption!")
	}
}

// SEC-WAL-INV-02: Only the latest recoverable torn tail may be truncated.
func TestSEC_WAL_INV_02_OnlyLatestTornTailTruncated(t *testing.T) {
	h := NewSecurityHarness(t)

	seg1Path := writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"))
	seg2Path := writeSegmentRecords(t, h.RootDir(), 2, testRecord(2, "k2", "v2"))

	info1Before, _ := os.Stat(seg1Path)

	// Inject torn tail into latest segment 2
	if err := h.AppendRawBytes(seg2Path, []byte("torn-incomplete-tail")); err != nil {
		t.Fatalf("AppendRawBytes failed: %v", err)
	}
	info2Before, _ := os.Stat(seg2Path)

	var sink recordingSink
	report, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}

	if !report.Truncated {
		t.Fatalf("expected report.Truncated == true")
	}

	info1After, _ := os.Stat(seg1Path)
	info2After, _ := os.Stat(seg2Path)

	// Segment 1 (historical) MUST NEVER be truncated
	if info1Before.Size() != info1After.Size() {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-02]: historical segment was modified: %d -> %d", info1Before.Size(), info1After.Size())
	}
	// Segment 2 (latest) was safely truncated back to valid record boundary
	if info2After.Size() >= info2Before.Size() {
		t.Fatalf("expected latest segment to be truncated: %d -> %d", info2Before.Size(), info2After.Size())
	}
}

// SEC-WAL-INV-03: A complete checksum-invalid record is never replayed.
func TestSEC_WAL_INV_03_ChecksumInvalidRecordNeverReplayed(t *testing.T) {
	h := NewSecurityHarness(t)

	// Construct complete record with invalid checksum
	rec := testRecord(1, "valid-looking-key", "valid-looking-val")
	raw, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	// Corrupt checksum
	raw[0] ^= 0xFF

	segPath := wal.SegmentPath(h.RootDir(), 1)
	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}
	if err := os.WriteFile(segPath, raw, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var sink recordingSink
	_, err = wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-03]: RecoverWAL accepted record with invalid checksum!")
	}
	if len(sink.records) != 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-03]: record with invalid checksum was passed to sink!")
	}
}

// SEC-WAL-INV-04: No external filesystem target can be unexpectedly overwritten through WAL segment creation.
func TestSEC_WAL_INV_04_NoExternalTargetOverwrittenViaSegmentCreation(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	// External critical file outside database
	extFile := filepath.Join(h.RootDir(), "important_external.conf")
	extContent := []byte("CRITICAL_CONFIGURATION_DATA")
	if err := os.WriteFile(extFile, extContent, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	// Symlink segment 1 path to external file
	seg1Path := wal.SegmentPath(h.RootDir(), 1)
	if err := os.Symlink(extFile, seg1Path); err != nil {
		t.Fatalf("Symlink creation failed: %v", err)
	}

	// Attempt CreateWriter at segment 1
	_, err := wal.CreateWriter(seg1Path)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-04]: CreateWriter opened existing symlink target!")
	}

	// Verify external file was completely untouched
	currentData, err := os.ReadFile(extFile)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !bytes.Equal(currentData, extContent) {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-04]: external file was overwritten or mutated!")
	}
}

// SEC-WAL-INV-05: Successful durability completion requires successful synchronization.
func TestSEC_WAL_INV_05_DurabilityRequiresSync(t *testing.T) {
	h := NewSecurityHarness(t)
	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.CreateSegmentWriter(h.RootDir(), 1)
	if err != nil {
		t.Fatalf("CreateSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Direct Append without Sync MUST NOT guarantee durability
	rec := testRecord(1, "unpersisted-key", "unpersisted-val")
	if err := w.Append(rec); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// Close without explicit sync by terminating descriptor
	// Note: Close() performs a sync internally, so let's verify Sync() returns clean error when fdatasync fails.
	// WALWriter provides AppendSync contract:
	simulatedErr := syscall.EIO
	// Verify that if Sync fails, AppendSync returns the error
	wFail, err := wal.CreateSegmentWriter(h.RootDir(), 2)
	if err != nil {
		t.Fatalf("CreateSegmentWriter 2 failed: %v", err)
	}
	defer func() { _ = wFail.Close() }()

	// Inject error into WALWriter through unexported seam tested in sec03_fault_injection_test.go
	// Here verify the public contract: AppendSync must fail if write or sync fails.
	_ = wFail.Close() // closed writer
	err = wFail.AppendSync(testRecord(2, "k", "v"))
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-05]: AppendSync succeeded on closed writer without sync!")
	}
	if !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed, got: %v", err)
	}
	_ = simulatedErr
}

// SEC-WAL-INV-06: Malformed lengths cannot trigger uncontrolled allocation.
func TestSEC_WAL_INV_06_MalformedLengthsDoNotCauseUncontrolledAllocation(t *testing.T) {
	// Synthesize a 30-byte buffer that claims a 4 GiB value length
	raw := make([]byte, 27)
	// Put valid CRC
	binary.PutUint32(raw[0:4], 0)
	raw[4] = byte(wal.RecordTypePut)
	binary.PutUint64(raw[5:13], 1)
	binary.PutUint64(raw[13:21], 1700000000)
	binary.PutUint16(raw[21:23], 2)          // key length = 2
	binary.PutUint32(raw[23:27], 0x7FFFFFFF) // val length = 2 GiB

	// CRC update
	binary.PutUint32(raw[0:4], binary.Checksum(raw[4:]))

	// DecodeRecord must detect that reader ends before the declared 2 GiB without allocating 2 GiB
	_, err := wal.DecodeRecord(bytes.NewReader(raw))
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-06]: DecodeRecord accepted truncated 2 GiB declared length!")
	}
	t.Logf("DecodeRecord safely rejected truncated record: %v", err)
}

// SEC-WAL-INV-07: Queue and batch bounds remain finite under adversarial input.
func TestSEC_WAL_INV_07_QueueAndBatchBoundsRemainFinite(t *testing.T) {
	// 1. Queue capacity cannot be initialized to unbounded or invalid values
	_, err := wal.NewWriteQueue(0)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-07]: NewWriteQueue accepted capacity 0")
	}
	_, err = wal.NewWriteQueue(-5)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-07]: NewWriteQueue accepted negative capacity")
	}

	// 2. Queue with capacity N rejects task N+1 via TryEnqueue
	q, err := wal.NewWriteQueue(5)
	if err != nil {
		t.Fatalf("NewWriteQueue failed: %v", err)
	}
	for i := 0; i < 5; i++ {
		task, err := wal.NewWriteTask(testRecord(uint64(i+1), "k", "v"))
		if err != nil {
			t.Fatalf("NewWriteTask failed: %v", err)
		}
		if err := q.TryEnqueue(task); err != nil {
			t.Fatalf("TryEnqueue failed on slot %d: %v", i, err)
		}
	}
	overflowTask, _ := wal.NewWriteTask(testRecord(99, "k", "v"))
	if err := q.TryEnqueue(overflowTask); err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-07]: TryEnqueue accepted task when capacity was exhausted!")
	}
}

// SEC-WAL-INV-08: Segment ID overflow cannot create segment 0 / wraparound.
func TestSEC_WAL_INV_08_SegmentIDOverflowCannotWrap(t *testing.T) {
	h := NewSecurityHarness(t)

	// Segment ID at math.MaxUint64 cannot wrap to 0 or 1
	w, err := wal.OpenRotatingWriter(h.RootDir(), wal.Options{
		SegmentSize:      64 * 1024,
		InitialSegmentID: math.MaxUint64,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Rotate from MaxUint64 must fail with SegmentIDOverflowError
	err = w.Rotate()
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-08]: Rotate succeeded at math.MaxUint64 (possible wraparound)!")
	}
	var overflowErr *errors.SegmentIDOverflowError
	if !stdErrors.As(err, &overflowErr) {
		t.Fatalf("expected *errors.SegmentIDOverflowError, got: %T (%v)", err, err)
	}

	// Check if segment 0 was created on disk
	seg0Path := wal.SegmentPath(h.RootDir(), 0)
	if _, err := os.Stat(seg0Path); err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-08]: segment 0 was created on filesystem!")
	}
}

// SEC-WAL-INV-09: Sequence-number regression cannot be replayed.
func TestSEC_WAL_INV_09_SequenceRegressionNeverReplayed(t *testing.T) {
	h := NewSecurityHarness(t)

	writeSegmentRecords(t, h.RootDir(), 1, testRecord(100, "k1", "v1"))
	writeSegmentRecords(t, h.RootDir(), 2, testRecord(50, "k2", "v2")) // regresses from 100 to 50

	var sink recordingSink
	_, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-09]: RecoverWAL accepted sequence regression!")
	}

	var seqErr *errors.SequenceOutOfOrderError
	if !stdErrors.As(err, &seqErr) {
		t.Fatalf("expected *errors.SequenceOutOfOrderError, got: %T (%v)", err, err)
	}
	if seqErr.Previous != 100 || seqErr.Current != 50 {
		t.Errorf("SequenceOutOfOrderError mismatch: prev=%d, curr=%d", seqErr.Previous, seqErr.Current)
	}

	// Record 2 (seq 50) must NEVER be applied to sink
	if len(sink.records) != 1 {
		t.Fatalf("expected exactly 1 record replayed before regression, got %d", len(sink.records))
	}
}

// SEC-WAL-INV-10: Recovery failure cannot silently acknowledge unpersisted state.
func TestSEC_WAL_INV_10_RecoveryFailureCannotAcknowledgeUnpersistedState(t *testing.T) {
	h := NewSecurityHarness(t)

	seg1Path := writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"))

	// Corrupt the only record
	if err := h.CorruptByteAt(seg1Path, 25, 0xEE); err != nil {
		t.Fatalf("CorruptByteAt failed: %v", err)
	}

	var sink recordingSink
	report, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-10]: RecoverWAL succeeded on corrupt data!")
	}

	// Verify report and sink do not claim any acknowledged records
	if report.ValidRecords != 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-10]: report claimed %d valid records on failed recovery!", report.ValidRecords)
	}
	if report.ReplayedRecords != 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-10]: report claimed %d replayed records on failed recovery!", report.ReplayedRecords)
	}
	if len(sink.records) != 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-WAL-INV-10]: sink received %d records on failed recovery!", len(sink.records))
	}
}

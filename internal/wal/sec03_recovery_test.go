package wal_test

import (
	"bytes"
	"crypto/sha256"
	stdErrors "errors"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// SEC-03.9: Recovery Security Audit
//
// Invariants under test:
//   1. Segment ID gaps abort recovery immediately (*errors.SegmentGapError).
//   2. Duplicate segment IDs abort recovery immediately (*errors.DuplicateSegmentError).
//   3. Duplicate sequence numbers abort recovery immediately (*errors.SequenceOutOfOrderError).
//   4. Sequence regressions abort recovery immediately (*errors.SequenceOutOfOrderError).
//   5. Invalid record types abort recovery; invalid operations are never dispatched to sink.
//   6. Corrupt historical segments fail closed; historical segments are NEVER truncated.
//   7. Middle corruption in latest segment fails closed without truncation.
//   8. Only torn tail at the EOF of the latest segment is safely truncated.
//   9. Replay sink errors halt recovery immediately without applying subsequent records.
//  10. Idempotence: recover -> inspect -> recover again produces deterministic, identical state.
//  11. Failed recovery never mutates or masks corruption into seemingly valid state.

// TestSEC03_Recovery_01_SegmentGapsFailsClosed verifies that missing segments in the sequence
// (e.g. 1, 2, 4) cause recovery to fail closed immediately without replaying records.
func TestSEC03_Recovery_01_SegmentGapsFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	// Create segment 1, 2, and 4 (missing 3)
	writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"))
	writeSegmentRecords(t, h.RootDir(), 2, testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, h.RootDir(), 4, testRecord(3, "k4", "v4"))

	var sink recordingSink
	_, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded despite segment gap (1, 2, 4)")
	}

	var gapErr *errors.SegmentGapError
	if !stdErrors.As(err, &gapErr) {
		t.Fatalf("expected *errors.SegmentGapError, got: %T (%v)", err, err)
	}
	if gapErr.Expected != 3 || gapErr.Actual != 4 {
		t.Errorf("gapErr mismatch: expected (3, 4), got (%d, %d)", gapErr.Expected, gapErr.Actual)
	}

	// Invariant: zero records replayed when gap detected
	if len(sink.records) != 0 {
		t.Fatalf("expected 0 replayed records, got %d", len(sink.records))
	}
}

// TestSEC03_Recovery_02_DuplicateSegmentsFailsClosed verifies that duplicate segment IDs
// fail closed with *errors.DuplicateSegmentError.
func TestSEC03_Recovery_02_DuplicateSegmentsFailsClosed(t *testing.T) {
	err := wal.ValidateSegmentContinuity([]uint64{1, 2, 2, 3})
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: ValidateSegmentContinuity accepted duplicate segment ID")
	}
	var dupErr *errors.DuplicateSegmentError
	if !stdErrors.As(err, &dupErr) {
		t.Fatalf("expected *errors.DuplicateSegmentError, got: %T (%v)", err, err)
	}
	if dupErr.SegmentID != 2 {
		t.Errorf("expected DuplicateSegmentError for ID 2, got: %d", dupErr.SegmentID)
	}
}

// TestSEC03_Recovery_03_DuplicateSequenceNumbersFailsClosed verifies that duplicate sequence
// numbers (e.g. seq 2 in segment 1 and seq 2 in segment 2) abort recovery immediately.
func TestSEC03_Recovery_03_DuplicateSequenceNumbersFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, h.RootDir(), 2, testRecord(2, "k2-dup", "v2-dup"), testRecord(3, "k3", "v3"))

	var sink recordingSink
	_, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL accepted duplicate sequence number 2")
	}

	var seqErr *errors.SequenceOutOfOrderError
	if !stdErrors.As(err, &seqErr) {
		t.Fatalf("expected *errors.SequenceOutOfOrderError, got: %T (%v)", err, err)
	}
	if seqErr.Previous != 2 || seqErr.Current != 2 {
		t.Errorf("SequenceOutOfOrderError mismatch: prev=%d, curr=%d", seqErr.Previous, seqErr.Current)
	}

	// Only records before the duplicate should have reached the sink (2 records from segment 1)
	if len(sink.records) != 2 {
		t.Fatalf("expected 2 records replayed before failure, got %d", len(sink.records))
	}
}

// TestSEC03_Recovery_04_SequenceRegressionFailsClosed verifies that decreasing sequence
// numbers across or within segments abort recovery immediately.
func TestSEC03_Recovery_04_SequenceRegressionFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	writeSegmentRecords(t, h.RootDir(), 1, testRecord(10, "k1", "v1"))
	writeSegmentRecords(t, h.RootDir(), 2, testRecord(5, "k2", "v2")) // regresses from 10 to 5

	var sink recordingSink
	_, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL accepted sequence regression (10 -> 5)")
	}

	var seqErr *errors.SequenceOutOfOrderError
	if !stdErrors.As(err, &seqErr) {
		t.Fatalf("expected *errors.SequenceOutOfOrderError, got: %T (%v)", err, err)
	}
	if seqErr.Previous != 10 || seqErr.Current != 5 {
		t.Errorf("SequenceOutOfOrderError mismatch: prev=%d, curr=%d", seqErr.Previous, seqErr.Current)
	}
}

// TestSEC03_Recovery_05_InvalidRecordTypesFailsClosed verifies that unknown record types
// on disk fail recovery and are never executed or passed to the replay sink.
func TestSEC03_Recovery_05_InvalidRecordTypesFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	// Synthesize record with invalid type 0x7F
	raw, err := wal.EncodeRecord(testRecord(1, "key", "val"))
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	corrupted := make([]byte, len(raw))
	copy(corrupted, raw)
	corrupted[4] = 0x7F
	binary.PutUint32(corrupted[0:4], binary.Checksum(corrupted[4:]))

	segPath := wal.SegmentPath(h.RootDir(), 1)
	if _, err := wal.InitDir(h.RootDir()); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}
	if err := os.WriteFile(segPath, corrupted, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var sink recordingSink
	_, err = wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded on invalid record type 0x7F")
	}

	if len(sink.records) != 0 {
		t.Fatalf("SECURITY VIOLATION: invalid record was dispatched to sink!")
	}
}

// TestSEC03_Recovery_06_CorruptHistoricalSegmentFailsClosedWithoutTruncation verifies
// that corruption in a sealed historical segment causes recovery to fail closed immediately,
// and that historical segments are NEVER physically truncated or modified.
func TestSEC03_Recovery_06_CorruptHistoricalSegmentFailsClosedWithoutTruncation(t *testing.T) {
	h := NewSecurityHarness(t)

	seg1Path := writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, h.RootDir(), 2, testRecord(3, "k3", "v3"))

	// Corrupt historical segment 1: flip a byte in the payload of record 2
	data, err := os.ReadFile(seg1Path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	data[len(data)-2] ^= 0xFF
	if err := os.WriteFile(seg1Path, data, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Hash the corrupt historical file
	hBefore := sha256.Sum256(data)

	var sink recordingSink
	_, err = wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded despite corrupted historical segment")
	}

	// Verify historical segment was NOT truncated or modified
	dataAfter, err := os.ReadFile(seg1Path)
	if err != nil {
		t.Fatalf("ReadFile after failed: %v", err)
	}
	hAfter := sha256.Sum256(dataAfter)

	if !bytes.Equal(hBefore[:], hAfter[:]) {
		t.Fatalf("SECURITY VIOLATION: historical segment was mutated during failed recovery!")
	}
}

// TestSEC03_Recovery_07_CorruptLatestSegmentMiddleFailsClosed verifies that non-tail
// corruption in the latest segment fails closed and is NOT truncated.
func TestSEC03_Recovery_07_CorruptLatestSegmentMiddleFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	// Write 3 records to segment 1 (which is the latest segment)
	seg1Path := writeSegmentRecords(t, h.RootDir(), 1,
		testRecord(1, "k1", "v1"),
		testRecord(2, "k2", "v2"),
		testRecord(3, "k3", "v3"),
	)

	// Corrupt record 2's CRC
	data, err := os.ReadFile(seg1Path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	// Record 1 wire size
	rec1WireSize := int(wal.RecordWireSize(testRecord(1, "k1", "v1")))
	// Flip CRC byte in record 2
	data[rec1WireSize] ^= 0xFF
	if err := os.WriteFile(seg1Path, data, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	lenBefore := len(data)

	var sink recordingSink
	_, err = wal.RecoverWAL(h.RootDir(), &sink)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION: RecoverWAL succeeded on middle CRC corruption")
	}

	// Ensure file was NOT truncated (middle corruption cannot be safely truncated)
	info, err := os.Stat(seg1Path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != int64(lenBefore) {
		t.Fatalf("SECURITY VIOLATION: latest segment was truncated on middle corruption (size %d -> %d)", lenBefore, info.Size())
	}
}

// TestSEC03_Recovery_08_TornLatestTailRepairedSafely verifies that only an incomplete
// torn tail at the end of the latest segment is safely truncated and the clean prefix is replayed.
func TestSEC03_Recovery_08_TornLatestTailRepairedSafely(t *testing.T) {
	h := NewSecurityHarness(t)

	writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"))
	seg2Path := writeSegmentRecords(t, h.RootDir(), 2, testRecord(2, "k2", "v2"), testRecord(3, "k3", "v3"))

	// Append 12 bytes of torn tail garbage to segment 2
	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	tornGarbage := []byte("torn-bytes!!")
	if _, err := f.Write(tornGarbage); err != nil {
		t.Fatalf("Write garbage failed: %v", err)
	}
	_ = f.Close()

	var sink recordingSink
	report, err := wal.RecoverWAL(h.RootDir(), &sink)
	if err != nil {
		t.Fatalf("RecoverWAL failed on torn tail: %v", err)
	}

	if !report.Truncated {
		t.Fatalf("expected report.Truncated == true")
	}
	if report.TruncatedBytes != int64(len(tornGarbage)) {
		t.Fatalf("expected TruncatedBytes == %d, got %d", len(tornGarbage), report.TruncatedBytes)
	}
	if report.ValidRecords != 3 || len(sink.records) != 3 {
		t.Fatalf("expected 3 valid records replayed, got %d", len(sink.records))
	}
	if report.LastSeqNum != 3 {
		t.Fatalf("expected LastSeqNum == 3, got %d", report.LastSeqNum)
	}
}

// TestSEC03_Recovery_09_ReplaySinkErrorAbortsImmediately verifies that if the replay
// sink returns an error, recovery stops immediately and propagates the error without further replays.
func TestSEC03_Recovery_09_ReplaySinkErrorAbortsImmediately(t *testing.T) {
	h := NewSecurityHarness(t)

	writeSegmentRecords(t, h.RootDir(), 1,
		testRecord(1, "k1", "v1"),
		testRecord(2, "k2", "v2"),
		testRecord(3, "k3", "v3"),
		testRecord(4, "k4", "v4"),
	)

	simulatedSinkErr := stdErrors.New("sink database storage engine full")
	sink := &failingSink{failAfter: 2, err: simulatedSinkErr}

	report, err := wal.RecoverWAL(h.RootDir(), sink)
	if err == nil {
		t.Fatalf("expected RecoverWAL to return error from failing sink")
	}
	if !stdErrors.Is(err, simulatedSinkErr) {
		t.Fatalf("expected error wrapping simulatedSinkErr, got: %v", err)
	}

	if report.ReplayedRecords != 2 {
		t.Fatalf("expected ReplayedRecords == 2, got %d", report.ReplayedRecords)
	}
	if sink.calls != 3 {
		t.Fatalf("expected sink to be called exactly 3 times (2 successes + 1 failure), got %d", sink.calls)
	}
}

// TestSEC03_Recovery_10_RecoveryIdempotence verifies the core security invariant:
// recover -> inspect -> recover again produces deterministic, identical state.
func TestSEC03_Recovery_10_RecoveryIdempotence(t *testing.T) {
	h := NewSecurityHarness(t)

	writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"))
	seg2Path := writeSegmentRecords(t, h.RootDir(), 2, testRecord(2, "k2", "v2"))

	// Append torn tail to segment 2
	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	_, _ = f.Write([]byte("torn-tail-12345"))
	_ = f.Close()

	// First recovery: truncates torn tail
	var sink1 recordingSink
	report1, err := wal.RecoverWAL(h.RootDir(), &sink1)
	if err != nil {
		t.Fatalf("First RecoverWAL failed: %v", err)
	}
	if !report1.Truncated {
		t.Fatalf("expected first recovery to truncate")
	}

	// Second recovery: must be clean and idempotent (0 truncations, identical records)
	var sink2 recordingSink
	report2, err := wal.RecoverWAL(h.RootDir(), &sink2)
	if err != nil {
		t.Fatalf("Second RecoverWAL failed: %v", err)
	}

	if report2.Truncated {
		t.Fatalf("expected second recovery to have report2.Truncated == false")
	}
	if report2.TruncatedBytes != 0 {
		t.Fatalf("expected 0 truncated bytes in second recovery, got %d", report2.TruncatedBytes)
	}
	if report1.ValidRecords != report2.ValidRecords {
		t.Fatalf("ValidRecords mismatch: %d vs %d", report1.ValidRecords, report2.ValidRecords)
	}
	if report1.LastSeqNum != report2.LastSeqNum {
		t.Fatalf("LastSeqNum mismatch: %d vs %d", report1.LastSeqNum, report2.LastSeqNum)
	}
	if len(sink1.records) != len(sink2.records) {
		t.Fatalf("sink records count mismatch: %d vs %d", len(sink1.records), len(sink2.records))
	}
	for i := range sink1.records {
		if sink1.records[i].SeqNum != sink2.records[i].SeqNum {
			t.Errorf("record %d seqnum mismatch: %d vs %d", i, sink1.records[i].SeqNum, sink2.records[i].SeqNum)
		}
	}
}

// TestSEC03_Recovery_11_FailedRecoveryDoesNotTransformCorruptionIntoValidState verifies
// that a failed recovery attempt leaves the corruption intact so that subsequent recovery
// attempts continue to fail closed and never falsely succeed.
func TestSEC03_Recovery_11_FailedRecoveryDoesNotTransformCorruptionIntoValidState(t *testing.T) {
	h := NewSecurityHarness(t)

	seg1Path := writeSegmentRecords(t, h.RootDir(), 1, testRecord(1, "k1", "v1"))

	// Corrupt the record on disk
	data, err := os.ReadFile(seg1Path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	data[len(data)-1] ^= 0xAA
	if err := os.WriteFile(seg1Path, data, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Attempt 1: must fail
	var sink1 recordingSink
	_, err1 := wal.RecoverWAL(h.RootDir(), &sink1)
	if err1 == nil {
		t.Fatalf("Attempt 1: expected failure on corrupt WAL")
	}

	// Attempt 2: must STILL fail
	var sink2 recordingSink
	_, err2 := wal.RecoverWAL(h.RootDir(), &sink2)
	if err2 == nil {
		t.Fatalf("SECURITY VIOLATION: Attempt 2 succeeded after failed recovery attempt!")
	}

	if len(sink2.records) != 0 {
		t.Fatalf("SECURITY VIOLATION: Attempt 2 replayed %d corrupt records to sink", len(sink2.records))
	}
}

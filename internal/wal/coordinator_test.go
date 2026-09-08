package wal_test

import (
	"crypto/sha256"
	"encoding/hex"
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// testRecord creates a valid PUT record for testing.
func testRecord(seq uint64, key, val string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000000 + seq,
		Key:       []byte(key),
		Value:     []byte(val),
	}
}

// testDeleteRecord creates a valid DELETE tombstone record for testing.
func testDeleteRecord(seq uint64, key string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypeDelete,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000000 + seq,
		Key:       []byte(key),
		Value:     nil,
	}
}

// testBatchMarker creates a BATCH_START or BATCH_COMMIT record for testing.
func testBatchMarker(seq uint64, rType wal.RecordType) wal.Record {
	return wal.Record{
		Type:      rType,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000000000 + seq,
	}
}

// fileSHA256 returns the hex-encoded SHA-256 hash of the file contents.
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file %s for sha256: %v", path, err)
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// recordingSink accumulates replayed records in order.
type recordingSink struct {
	records []wal.Record
}

func (s *recordingSink) Apply(rec wal.Record) error {
	s.records = append(s.records, rec)
	return nil
}

// failingSink fails after a configured number of successful Apply calls.
type failingSink struct {
	failAfter int
	calls     int
	err       error
}

func (s *failingSink) Apply(rec wal.Record) error {
	s.calls++
	if s.calls > s.failAfter {
		return s.err
	}
	return nil
}

// writeSegmentRecords opens a segment writer, appends the given records, and closes it.
func writeSegmentRecords(t *testing.T, dbPath string, segID uint64, records ...wal.Record) string {
	t.Helper()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("failed to init wal dir: %v", err)
	}
	w, err := wal.CreateSegmentWriter(dbPath, segID)
	if err != nil {
		t.Fatalf("failed to create segment %d: %v", segID, err)
	}
	for _, rec := range records {
		if err := w.AppendSync(rec); err != nil {
			_ = w.Close()
			t.Fatalf("failed to append record to segment %d: %v", segID, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close segment %d: %v", segID, err)
	}
	return wal.SegmentPath(dbPath, segID)
}

// 1. Empty WAL directory
func TestCoordinator_EmptyWALDirectory(t *testing.T) {
	dbPath := t.TempDir()
	sink := &recordingSink{}

	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected nil error on empty WAL, got: %v", err)
	}

	if report.SegmentCount != 0 {
		t.Errorf("expected SegmentCount 0, got %d", report.SegmentCount)
	}
	if report.HighestSegmentID != 0 {
		t.Errorf("expected HighestSegmentID 0, got %d", report.HighestSegmentID)
	}
	if report.ValidRecords != 0 {
		t.Errorf("expected ValidRecords 0, got %d", report.ValidRecords)
	}
	if report.ReplayedRecords != 0 {
		t.Errorf("expected ReplayedRecords 0, got %d", report.ReplayedRecords)
	}
	if report.Truncated {
		t.Errorf("expected Truncated false, got true")
	}
	if report.TruncatedBytes != 0 {
		t.Errorf("expected TruncatedBytes 0, got %d", report.TruncatedBytes)
	}
	if report.LastSeqNum != 0 {
		t.Errorf("expected LastSeqNum 0, got %d", report.LastSeqNum)
	}
	if len(sink.records) != 0 {
		t.Errorf("expected 0 sink records, got %d", len(sink.records))
	}
}

// 2. One clean segment
func TestCoordinator_OneCleanSegment(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "key1", "val1")
	r2 := testRecord(2, "key2", "val2")
	r3 := testRecord(3, "key3", "val3")
	writeSegmentRecords(t, dbPath, 1, r1, r2, r3)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if report.SegmentCount != 1 || report.HighestSegmentID != 1 {
		t.Errorf("unexpected segments: count=%d, highest=%d", report.SegmentCount, report.HighestSegmentID)
	}
	if report.ValidRecords != 3 || report.ReplayedRecords != 3 {
		t.Errorf("unexpected record counts: valid=%d, replayed=%d", report.ValidRecords, report.ReplayedRecords)
	}
	if report.Truncated || report.TruncatedBytes != 0 {
		t.Errorf("expected no truncation, got truncated=%v, bytes=%d", report.Truncated, report.TruncatedBytes)
	}
	if report.LastSeqNum != 3 {
		t.Errorf("expected LastSeqNum 3, got %d", report.LastSeqNum)
	}
	if len(sink.records) != 3 {
		t.Fatalf("expected 3 sink records, got %d", len(sink.records))
	}
	if !sink.records[0].Equal(r1) || !sink.records[1].Equal(r2) || !sink.records[2].Equal(r3) {
		t.Errorf("replayed records do not match original records")
	}
}

// 3. Three clean segments
func TestCoordinator_ThreeCleanSegments(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(10, "k1", "v1")
	r2 := testRecord(20, "k2", "v2")
	r3 := testRecord(30, "k3", "v3")
	r4 := testRecord(40, "k4", "v4")
	r5 := testRecord(50, "k5", "v5")
	r6 := testRecord(60, "k6", "v6")

	writeSegmentRecords(t, dbPath, 1, r1, r2)
	writeSegmentRecords(t, dbPath, 2, r3, r4)
	writeSegmentRecords(t, dbPath, 3, r5, r6)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if report.SegmentCount != 3 || report.HighestSegmentID != 3 {
		t.Errorf("unexpected segments: count=%d, highest=%d", report.SegmentCount, report.HighestSegmentID)
	}
	if report.ValidRecords != 6 || report.ReplayedRecords != 6 {
		t.Errorf("unexpected record counts: valid=%d, replayed=%d", report.ValidRecords, report.ReplayedRecords)
	}
	if report.LastSeqNum != 60 {
		t.Errorf("expected LastSeqNum 60, got %d", report.LastSeqNum)
	}
	if len(sink.records) != 6 {
		t.Fatalf("expected 6 sink records, got %d", len(sink.records))
	}
	expected := []wal.Record{r1, r2, r3, r4, r5, r6}
	for i, exp := range expected {
		if !sink.records[i].Equal(exp) {
			t.Errorf("record %d mismatch: got %v, want %v", i, sink.records[i], exp)
		}
	}
}

// 4. Numeric ordering with IDs such as: 1, 2, 9, 10
func TestCoordinator_NumericOrdering(t *testing.T) {
	dbPath := t.TempDir()
	r7 := testRecord(7, "k7", "v7")
	r8 := testRecord(8, "k8", "v8")
	r9 := testRecord(9, "k9", "v9")
	r10 := testRecord(10, "k10", "v10")

	// Create segments 7, 8, 9, 10
	writeSegmentRecords(t, dbPath, 7, r7)
	writeSegmentRecords(t, dbPath, 8, r8)
	writeSegmentRecords(t, dbPath, 9, r9)
	writeSegmentRecords(t, dbPath, 10, r10)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if report.SegmentCount != 4 || report.HighestSegmentID != 10 {
		t.Errorf("unexpected segments: count=%d, highest=%d", report.SegmentCount, report.HighestSegmentID)
	}
	if len(sink.records) != 4 {
		t.Fatalf("expected 4 records, got %d", len(sink.records))
	}
	if sink.records[0].SeqNum != 7 || sink.records[1].SeqNum != 8 || sink.records[2].SeqNum != 9 || sink.records[3].SeqNum != 10 {
		t.Errorf("records were not replayed in numeric segment order: %v", sink.records)
	}
}

// 5. Missing segment ID: 1, 2, 4
func TestCoordinator_MissingSegmentID_GapsRejected(t *testing.T) {
	dbPath := t.TempDir()
	writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"))
	writeSegmentRecords(t, dbPath, 2, testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, dbPath, 4, testRecord(4, "k4", "v4"))

	sink := &recordingSink{}
	_, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on missing segment ID, got nil")
	}

	if !stdErrors.Is(err, errors.ErrSegmentGap) {
		t.Errorf("expected ErrSegmentGap, got: %v", err)
	}

	var gapErr *errors.SegmentGapError
	if !stdErrors.As(err, &gapErr) {
		t.Fatalf("expected SegmentGapError type, got: %T", err)
	}
	if gapErr.Expected != 3 || gapErr.Actual != 4 {
		t.Errorf("unexpected gap info: Expected=%d, Actual=%d", gapErr.Expected, gapErr.Actual)
	}
}

// 6. Latest segment torn tail
func TestCoordinator_LatestSegmentTornTail(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2, r3)

	// Append a 7-byte incomplete torn tail to latest segment
	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("failed to open segment 2: %v", err)
	}
	if _, err := f.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x00, 0x00}); err != nil {
		_ = f.Close()
		t.Fatalf("failed to write torn tail: %v", err)
	}
	_ = f.Close()

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success with truncation, got: %v", err)
	}

	if !report.Truncated {
		t.Errorf("expected Truncated true")
	}
	if report.TruncatedBytes != 7 {
		t.Errorf("expected TruncatedBytes 7, got %d", report.TruncatedBytes)
	}
	if report.ValidRecords != 3 || report.ReplayedRecords != 3 {
		t.Errorf("unexpected records: valid=%d, replayed=%d", report.ValidRecords, report.ReplayedRecords)
	}
	if report.LastSeqNum != 3 {
		t.Errorf("expected LastSeqNum 3, got %d", report.LastSeqNum)
	}
	if len(sink.records) != 3 {
		t.Fatalf("expected 3 sink records, got %d", len(sink.records))
	}
}

// 7. Earlier segment torn tail (must fail closed; historical segments are never truncated)
func TestCoordinator_EarlierSegmentTornTail(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	seg1Path := writeSegmentRecords(t, dbPath, 1, r1, r2)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r3)

	// Append 5 torn bytes to historical segment 1
	f, err := os.OpenFile(seg1Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("failed to open segment 1: %v", err)
	}
	if _, err := f.Write([]byte{0x01, 0x02, 0x03, 0x04, 0x05}); err != nil {
		_ = f.Close()
		t.Fatalf("failed to write torn tail: %v", err)
	}
	_ = f.Close()

	seg1HashBefore := fileSHA256(t, seg1Path)
	seg2HashBefore := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error for torn tail in earlier segment, got nil")
	}

	// Must preserve underlying error identity
	if !stdErrors.Is(err, errors.ErrHeaderTruncated) && !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected ErrHeaderTruncated or io.ErrUnexpectedEOF, got: %v", err)
	}

	if report.Truncated {
		t.Errorf("report.Truncated must be false on fail-closed")
	}

	// Invariant: historical and latest segments must remain untouched (no mutation)
	if fileSHA256(t, seg1Path) != seg1HashBefore {
		t.Errorf("historical segment was modified during fail-closed recovery")
	}
	if fileSHA256(t, seg2Path) != seg2HashBefore {
		t.Errorf("latest segment was modified during fail-closed recovery")
	}
}

// 8. Latest segment complete CRC corruption (must fail closed without truncation)
func TestCoordinator_LatestSegmentCompleteCRCCorruption(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")

	writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2)

	// Append a complete record with corrupted CRC to segment 2
	corruptRec := testRecord(3, "k3", "v3")
	buf, err := wal.EncodeRecord(corruptRec)
	if err != nil {
		t.Fatalf("failed to encode corrupt record: %v", err)
	}
	// Flip bits in CRC bytes (offsets 0..3)
	buf[0] ^= 0xFF

	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("failed to open segment 2: %v", err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		t.Fatalf("failed to write corrupted record: %v", err)
	}
	_ = f.Close()

	seg2HashBefore := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on complete CRC corruption in latest segment, got nil")
	}

	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}
	if report.Truncated {
		t.Errorf("report.Truncated must be false on complete corruption")
	}

	// Invariant: file must NOT be truncated merely because corruption is in latest segment
	if fileSHA256(t, seg2Path) != seg2HashBefore {
		t.Errorf("latest segment was mutated on complete corruption")
	}
}

// 9. Earlier segment complete CRC corruption
func TestCoordinator_EarlierSegmentCompleteCRCCorruption(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	seg1Path := writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r3)

	// Append a complete CRC-corrupted record to earlier segment 1
	buf, err := wal.EncodeRecord(r2)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	buf[0] ^= 0xFF

	f, err := os.OpenFile(seg1Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	seg1HashBefore := fileSHA256(t, seg1Path)
	seg2HashBefore := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	_, err = wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}

	if fileSHA256(t, seg1Path) != seg1HashBefore || fileSHA256(t, seg2Path) != seg2HashBefore {
		t.Errorf("files were modified on corruption")
	}
}

// 10. Middle corruption in historical segment
func TestCoordinator_MiddleCorruptionHistoricalSegment(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")
	r4 := testRecord(4, "k4", "v4")

	// Segment 1 has r1, r2, r3; Segment 2 has r4
	seg1Path := writeSegmentRecords(t, dbPath, 1, r1, r2, r3)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r4)

	// Corrupt middle record r2 in segment 1
	f, err := os.OpenFile(seg1Path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	// r1 is 27 + 2 + 2 = 31 bytes
	rec1Len := wal.RecordWireSize(r1)
	if _, err := f.Seek(rec1Len, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek failed: %v", err)
	}
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("read failed: %v", err)
	}
	b[0] ^= 0x01
	if _, err := f.Seek(rec1Len, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek back failed: %v", err)
	}
	if _, err := f.Write(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("write corrupt byte failed: %v", err)
	}
	_ = f.Close()

	seg1HashBefore := fileSHA256(t, seg1Path)
	seg2HashBefore := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	_, err = wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on middle corruption, got nil")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}

	// No file mutation
	if fileSHA256(t, seg1Path) != seg1HashBefore || fileSHA256(t, seg2Path) != seg2HashBefore {
		t.Errorf("files were modified on middle corruption")
	}
}

// 11. Middle corruption in latest segment
func TestCoordinator_MiddleCorruptionLatestSegment(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2, r3)

	// Corrupt r2 (offset 0 in segment 2)
	f, err := os.OpenFile(seg2Path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("read failed: %v", err)
	}
	b[0] ^= 0xFF
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek failed: %v", err)
	}
	if _, err := f.Write(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	seg2HashBefore := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on middle corruption in latest segment, got nil")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}
	if report.Truncated {
		t.Errorf("report.Truncated must be false on middle corruption")
	}
	if fileSHA256(t, seg2Path) != seg2HashBefore {
		t.Errorf("latest segment was mutated on middle corruption")
	}
}

// 12. Invalid record type
func TestCoordinator_InvalidRecordType(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	seg1Path := writeSegmentRecords(t, dbPath, 1, r1)

	// Append record with invalid type byte 0x99
	f, err := os.OpenFile(seg1Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	var invalidHeader [21]byte
	invalidHeader[4] = 0x99 // Invalid type
	if _, err := f.Write(invalidHeader[:]); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	sink := &recordingSink{}
	_, err = wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on invalid record type, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
		t.Errorf("expected ErrInvalidRecordType, got: %v", err)
	}
}

// 13. Sequence monotonicity success
func TestCoordinator_SequenceMonotonicitySuccess(t *testing.T) {
	dbPath := t.TempDir()
	// Gaps in sequence numbers are allowed by architecture, as long as strictly increasing
	r1 := testRecord(5, "k1", "v1")
	r2 := testRecord(15, "k2", "v2")
	r3 := testRecord(100, "k3", "v3")

	writeSegmentRecords(t, dbPath, 1, r1, r2)
	writeSegmentRecords(t, dbPath, 2, r3)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if report.ValidRecords != 3 || report.LastSeqNum != 100 {
		t.Errorf("unexpected report: valid=%d, lastSeq=%d", report.ValidRecords, report.LastSeqNum)
	}
}

// 14. Sequence regression failure
func TestCoordinator_SequenceRegressionFailure(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(10, "k1", "v1")
	r2 := testRecord(20, "k2", "v2")
	r3 := testRecord(15, "k3", "v3") // Regression! 15 < 20

	writeSegmentRecords(t, dbPath, 1, r1, r2)
	writeSegmentRecords(t, dbPath, 2, r3)

	sink := &recordingSink{}
	_, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on sequence regression, got nil")
	}
	if !stdErrors.Is(err, errors.ErrSequenceOutOfOrder) {
		t.Errorf("expected ErrSequenceOutOfOrder, got: %v", err)
	}
	var seqErr *errors.SequenceOutOfOrderError
	if !stdErrors.As(err, &seqErr) {
		t.Fatalf("expected SequenceOutOfOrderError, got: %T", err)
	}
	if seqErr.Previous != 20 || seqErr.Current != 15 {
		t.Errorf("unexpected seq error details: Previous=%d, Current=%d", seqErr.Previous, seqErr.Current)
	}
}

// 15. Duplicate sequence failure if architecture requires uniqueness
func TestCoordinator_DuplicateSequenceFailure(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(10, "k1", "v1")
	r2 := testRecord(10, "k2", "v2") // Duplicate sequence number!

	writeSegmentRecords(t, dbPath, 1, r1, r2)

	sink := &recordingSink{}
	_, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on duplicate sequence number, got nil")
	}
	if !stdErrors.Is(err, errors.ErrSequenceOutOfOrder) {
		t.Errorf("expected ErrSequenceOutOfOrder, got: %v", err)
	}
	var seqErr *errors.SequenceOutOfOrderError
	if !stdErrors.As(err, &seqErr) {
		t.Fatalf("expected SequenceOutOfOrderError, got: %T", err)
	}
	if seqErr.Previous != 10 || seqErr.Current != 10 {
		t.Errorf("unexpected seq error details: Previous=%d, Current=%d", seqErr.Previous, seqErr.Current)
	}
}

// 16. Valid DELETE replay
func TestCoordinator_ValidDeleteReplay(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	del2 := testDeleteRecord(2, "k1")
	r3 := testRecord(3, "k2", "v2")

	writeSegmentRecords(t, dbPath, 1, r1, del2)
	writeSegmentRecords(t, dbPath, 2, r3)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if report.ValidRecords != 3 {
		t.Errorf("expected 3 valid records, got %d", report.ValidRecords)
	}
	if len(sink.records) != 3 {
		t.Fatalf("expected 3 sink records, got %d", len(sink.records))
	}
	if sink.records[1].Type != wal.RecordTypeDelete {
		t.Errorf("expected record 1 to be DELETE, got %v", sink.records[1].Type)
	}
	if string(sink.records[1].Key) != "k1" || len(sink.records[1].Value) != 0 {
		t.Errorf("DELETE record corrupted: key=%s, valLen=%d", sink.records[1].Key, len(sink.records[1].Value))
	}
}

// 17. Valid BATCH_START / BATCH_COMMIT preservation according to architecture
func TestCoordinator_ValidBatchStartCommitPreserved(t *testing.T) {
	dbPath := t.TempDir()
	bStart := testBatchMarker(1, wal.RecordTypeBatchStart)
	r1 := testRecord(2, "k1", "v1")
	r2 := testRecord(3, "k2", "v2")
	bCommit := testBatchMarker(4, wal.RecordTypeBatchCommit)

	writeSegmentRecords(t, dbPath, 1, bStart, r1, r2, bCommit)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if report.ValidRecords != 4 {
		t.Errorf("expected 4 valid records, got %d", report.ValidRecords)
	}
	if len(sink.records) != 4 {
		t.Fatalf("expected 4 sink records, got %d", len(sink.records))
	}
	if sink.records[0].Type != wal.RecordTypeBatchStart {
		t.Errorf("expected BATCH_START, got %v", sink.records[0].Type)
	}
	if sink.records[3].Type != wal.RecordTypeBatchCommit {
		t.Errorf("expected BATCH_COMMIT, got %v", sink.records[3].Type)
	}
}

// 18. Replay sink failure propagation
func TestCoordinator_ReplaySinkFailurePropagation(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	writeSegmentRecords(t, dbPath, 1, r1, r2, r3)

	expectedSinkErr := stdErrors.New("disk write failure in memtable")
	sink := &failingSink{
		failAfter: 2, // Fails on record 3
		err:       expectedSinkErr,
	}

	report, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected sink error, got nil")
	}
	if !stdErrors.Is(err, expectedSinkErr) {
		t.Errorf("expected sink error %v, got: %v", expectedSinkErr, err)
	}
	if report.ReplayedRecords != 2 {
		t.Errorf("expected 2 replayed records before failure, got %d", report.ReplayedRecords)
	}
	if report.ValidRecords != 3 {
		t.Errorf("expected 3 valid records counted, got %d", report.ValidRecords)
	}
}

// 19. Replay order across multiple segments
func TestCoordinator_ReplayOrderAcrossMultipleSegments(t *testing.T) {
	dbPath := t.TempDir()
	for seg := uint64(1); seg <= 5; seg++ {
		rA := testRecord(seg*10, fmt.Sprintf("k%d_A", seg), "valA")
		rB := testRecord(seg*10+1, fmt.Sprintf("k%d_B", seg), "valB")
		writeSegmentRecords(t, dbPath, seg, rA, rB)
	}

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if report.SegmentCount != 5 || report.ValidRecords != 10 {
		t.Errorf("unexpected report: segs=%d, records=%d", report.SegmentCount, report.ValidRecords)
	}
	if len(sink.records) != 10 {
		t.Fatalf("expected 10 sink records, got %d", len(sink.records))
	}
	for i := 0; i < len(sink.records)-1; i++ {
		if sink.records[i].SeqNum >= sink.records[i+1].SeqNum {
			t.Errorf("records out of order at index %d: %d >= %d", i, sink.records[i].SeqNum, sink.records[i+1].SeqNum)
		}
	}
}

// 20. No replay of truncated tail bytes
func TestCoordinator_NoReplayOfTruncatedTailBytes(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	segPath := writeSegmentRecords(t, dbPath, 1, r1, r2)

	// Append corrupt garbage
	f, err := os.OpenFile(segPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF}); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !report.Truncated || report.TruncatedBytes != 4 {
		t.Errorf("expected 4 bytes truncated, got %d (truncated=%v)", report.TruncatedBytes, report.Truncated)
	}
	if len(sink.records) != 2 {
		t.Fatalf("expected exactly 2 records, got %d", len(sink.records))
	}
}

// 21. Recovery result accuracy
func TestCoordinator_RecoveryResultAccuracy(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(100, "key100", "val100")
	r2 := testRecord(200, "key200", "val200")
	r3 := testRecord(300, "key300", "val300")

	writeSegmentRecords(t, dbPath, 1, r1)
	writeSegmentRecords(t, dbPath, 2, r2)
	seg3Path := writeSegmentRecords(t, dbPath, 3, r3)

	// Append 10 torn tail bytes to segment 3
	f, err := os.OpenFile(seg3Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte("1234567890")); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	if report.SegmentCount != 3 {
		t.Errorf("expected SegmentCount 3, got %d", report.SegmentCount)
	}
	if report.HighestSegmentID != 3 {
		t.Errorf("expected HighestSegmentID 3, got %d", report.HighestSegmentID)
	}
	if report.ValidRecords != 3 {
		t.Errorf("expected ValidRecords 3, got %d", report.ValidRecords)
	}
	if report.ReplayedRecords != 3 {
		t.Errorf("expected ReplayedRecords 3, got %d", report.ReplayedRecords)
	}
	if !report.Truncated {
		t.Errorf("expected Truncated true")
	}
	if report.TruncatedBytes != 10 {
		t.Errorf("expected TruncatedBytes 10, got %d", report.TruncatedBytes)
	}
	if report.LastSeqNum != 300 {
		t.Errorf("expected LastSeqNum 300, got %d", report.LastSeqNum)
	}
}

// 22. Idempotent recovery after latest-tail truncation
func TestCoordinator_IdempotentRecoveryAfterLatestTailTruncation(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	segPath := writeSegmentRecords(t, dbPath, 1, r1)

	// Append torn tail
	f, err := os.OpenFile(segPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte{0x01, 0x02, 0x03}); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	// First recovery: truncates
	sink1 := &recordingSink{}
	report1, err := wal.RecoverWAL(dbPath, sink1)
	if err != nil {
		t.Fatalf("first recovery failed: %v", err)
	}
	if !report1.Truncated || report1.TruncatedBytes != 3 {
		t.Errorf("expected first recovery to truncate 3 bytes")
	}

	// Second recovery: clean
	sink2 := &recordingSink{}
	report2, err := wal.RecoverWAL(dbPath, sink2)
	if err != nil {
		t.Fatalf("second recovery failed: %v", err)
	}
	if report2.Truncated || report2.TruncatedBytes != 0 {
		t.Errorf("expected second recovery to not truncate: %v, %d", report2.Truncated, report2.TruncatedBytes)
	}
	if report2.ValidRecords != report1.ValidRecords {
		t.Errorf("valid records mismatch across recoveries: %d vs %d", report1.ValidRecords, report2.ValidRecords)
	}
}

// 23. Second recovery after first successful recovery returns clean state
func TestCoordinator_SecondRecoveryReturnsCleanState(t *testing.T) {
	dbPath := t.TempDir()
	writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, dbPath, 2, testRecord(3, "k3", "v3"))

	sink1 := &recordingSink{}
	report1, err := wal.RecoverWAL(dbPath, sink1)
	if err != nil {
		t.Fatalf("recovery 1 failed: %v", err)
	}

	sink2 := &recordingSink{}
	report2, err := wal.RecoverWAL(dbPath, sink2)
	if err != nil {
		t.Fatalf("recovery 2 failed: %v", err)
	}

	if report1 != report2 {
		t.Errorf("consecutive recoveries produced different reports: %+v vs %+v", report1, report2)
	}
	if len(sink1.records) != len(sink2.records) {
		t.Errorf("consecutive recoveries produced different record counts: %d vs %d", len(sink1.records), len(sink2.records))
	}
}

// 24. Foreign/non-segment files in WAL directory
func TestCoordinator_ForeignNonSegmentFilesInWALDirectory(t *testing.T) {
	dbPath := t.TempDir()
	writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"))

	walDir := wal.Dir(dbPath)
	// Write harmless unrelated files
	if err := os.WriteFile(filepath.Join(walDir, "README.txt"), []byte("wal info"), 0644); err != nil {
		t.Fatalf("failed to write README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(walDir, "wal_temp.tmp"), []byte("temp"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(walDir, "manifest_backups"), 0700); err != nil {
		t.Fatalf("failed to create subfolder: %v", err)
	}

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("expected success with harmless foreign files, got: %v", err)
	}
	if report.SegmentCount != 1 || report.ValidRecords != 1 {
		t.Errorf("unexpected report: segs=%d, records=%d", report.SegmentCount, report.ValidRecords)
	}
}

// 25. Symlink segment rejection
func TestCoordinator_SymlinkSegmentRejection(t *testing.T) {
	dbPath := t.TempDir()
	seg1Path := writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"))

	walDir := wal.Dir(dbPath)
	symlinkPath := filepath.Join(walDir, wal.SegmentName(2))
	if err := os.Symlink(seg1Path, symlinkPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	sink := &recordingSink{}
	_, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error on symlink segment, got nil")
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid, got: %v", err)
	}
}

// 26. Directory at expected segment path
func TestCoordinator_DirectoryAtExpectedSegmentPath(t *testing.T) {
	dbPath := t.TempDir()
	walDir := wal.Dir(dbPath)
	if err := os.MkdirAll(walDir, 0700); err != nil {
		t.Fatalf("failed to create wal dir: %v", err)
	}

	// Create directory named wal_000000000001.log
	dirSegment := filepath.Join(walDir, wal.SegmentName(1))
	if err := os.Mkdir(dirSegment, 0700); err != nil {
		t.Fatalf("failed to create dir at segment path: %v", err)
	}

	sink := &recordingSink{}
	_, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected error when directory exists at segment path, got nil")
	}
	if !stdErrors.Is(err, errors.ErrNotADirectory) {
		t.Errorf("expected ErrNotADirectory, got: %v", err)
	}
}

// 27. Missing segment file
func TestCoordinator_MissingSegmentFile(t *testing.T) {
	dbPath := t.TempDir()
	writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"))

	// Non-existent DB path
	sink := &recordingSink{}
	_, err := wal.RecoverWAL(filepath.Join(dbPath, "nonexistent"), sink)
	// Nonexistent directory returns empty segment list (clean empty state)
	if err != nil {
		t.Errorf("non-existent directory should return empty clean report, got: %v", err)
	}
}

// 28. Large multi-segment WAL without full-file buffering
func TestCoordinator_LargeMultiSegmentStreaming(t *testing.T) {
	dbPath := t.TempDir()
	const numSegs = 10
	const recsPerSeg = 50

	var globalSeq uint64
	for s := uint64(1); s <= numSegs; s++ {
		var recs []wal.Record
		for r := 0; r < recsPerSeg; r++ {
			globalSeq++
			recs = append(recs, testRecord(globalSeq, fmt.Sprintf("k_%d_%d", s, r), fmt.Sprintf("val_%d", globalSeq)))
		}
		writeSegmentRecords(t, dbPath, s, recs...)
	}

	var streamCount int
	sink := wal.ReplayFunc(func(rec wal.Record) error {
		streamCount++
		return nil
	})

	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}

	if report.SegmentCount != numSegs {
		t.Errorf("expected %d segments, got %d", numSegs, report.SegmentCount)
	}
	if report.ValidRecords != numSegs*recsPerSeg {
		t.Errorf("expected %d valid records, got %d", numSegs*recsPerSeg, report.ValidRecords)
	}
	if streamCount != numSegs*recsPerSeg {
		t.Errorf("expected %d replayed records, got %d", numSegs*recsPerSeg, streamCount)
	}
	if report.LastSeqNum != binary.SeqNum(numSegs*recsPerSeg) {
		t.Errorf("expected LastSeqNum %d, got %d", numSegs*recsPerSeg, report.LastSeqNum)
	}
}

// 29. Deterministic repeated recovery of identical WAL contents
func TestCoordinator_DeterministicRepeatedRecovery(t *testing.T) {
	dbPath := t.TempDir()
	writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, dbPath, 2, testRecord(3, "k3", "v3"), testRecord(4, "k4", "v4"))

	sinkA := &recordingSink{}
	reportA, errA := wal.RecoverWAL(dbPath, sinkA)
	if errA != nil {
		t.Fatalf("run A failed: %v", errA)
	}

	sinkB := &recordingSink{}
	reportB, errB := wal.RecoverWAL(dbPath, sinkB)
	if errB != nil {
		t.Fatalf("run B failed: %v", errB)
	}

	if reportA != reportB {
		t.Errorf("reports differ: %+v vs %+v", reportA, reportB)
	}
	if len(sinkA.records) != len(sinkB.records) {
		t.Fatalf("record lengths differ: %d vs %d", len(sinkA.records), len(sinkB.records))
	}
	for i := range sinkA.records {
		if !sinkA.records[i].Equal(sinkB.records[i]) {
			t.Errorf("record %d differs between runs", i)
		}
	}
}

// 30. File immutability of historical clean segments
func TestCoordinator_FileImmutabilityHistoricalCleanSegments(t *testing.T) {
	dbPath := t.TempDir()
	seg1Path := writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"))
	seg2Path := writeSegmentRecords(t, dbPath, 2, testRecord(2, "k2", "v2"))
	seg3Path := writeSegmentRecords(t, dbPath, 3, testRecord(3, "k3", "v3"))

	// Append torn tail only to segment 3
	f, err := os.OpenFile(seg3Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte{0xDE, 0xAD}); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	hash1Before := fileSHA256(t, seg1Path)
	hash2Before := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if !report.Truncated {
		t.Errorf("expected segment 3 truncated")
	}

	// Historical segments 1 and 2 must have identical byte hashes
	if fileSHA256(t, seg1Path) != hash1Before {
		t.Errorf("historical segment 1 was mutated during recovery")
	}
	if fileSHA256(t, seg2Path) != hash2Before {
		t.Errorf("historical segment 2 was mutated during recovery")
	}
}

// 31. Latest-segment mutation only when torn tail exists
func TestCoordinator_LatestSegmentMutationOnlyWhenTornTailExists(t *testing.T) {
	dbPath := t.TempDir()
	seg1Path := writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"))

	hashBefore := fileSHA256(t, seg1Path)

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}
	if report.Truncated {
		t.Errorf("expected Truncated false for clean latest segment")
	}
	if fileSHA256(t, seg1Path) != hashBefore {
		t.Errorf("clean latest segment was modified during recovery")
	}
}

// 32. No mutation anywhere on middle corruption
func TestCoordinator_NoMutationAnywhereOnMiddleCorruption(t *testing.T) {
	dbPath := t.TempDir()
	seg1Path := writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	seg2Path := writeSegmentRecords(t, dbPath, 2, testRecord(3, "k3", "v3"))

	// Corrupt middle of segment 1
	f, err := os.OpenFile(seg1Path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("read failed: %v", err)
	}
	b[0] ^= 0xAA
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek failed: %v", err)
	}
	if _, err := f.Write(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	hash1Before := fileSHA256(t, seg1Path)
	hash2Before := fileSHA256(t, seg2Path)

	sink := &recordingSink{}
	_, err = wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected failure on middle corruption, got nil")
	}

	if fileSHA256(t, seg1Path) != hash1Before {
		t.Errorf("segment 1 modified after corruption failure")
	}
	if fileSHA256(t, seg2Path) != hash2Before {
		t.Errorf("segment 2 modified after corruption failure")
	}
}

// 33. Sequence validation after a repaired latest tail
func TestCoordinator_SequenceValidationAfterRepairedLatestTail(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(10, "k1", "v1")
	r2 := testRecord(20, "k2", "v2")
	r3 := testRecord(30, "k3", "v3")

	writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2, r3)

	// Append torn tail
	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte{0x01, 0x02}); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}

	if !report.Truncated {
		t.Errorf("expected Truncated true")
	}
	if report.LastSeqNum != 30 {
		t.Errorf("expected LastSeqNum 30, got %d", report.LastSeqNum)
	}
	if len(sink.records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(sink.records))
	}
}

// 34. Multiple segment rotations followed by recovery
func TestCoordinator_MultipleRotationsFollowedByRecovery(t *testing.T) {
	dbPath := t.TempDir()

	// Use small SegmentSize to force frequent rotations
	opts := wal.Options{
		SegmentSize: 120, // ~3 records per segment
	}
	writer, err := wal.OpenRotatingWriter(dbPath, opts)
	if err != nil {
		t.Fatalf("failed to open rotating writer: %v", err)
	}

	const totalRecords = 25
	for i := uint64(1); i <= totalRecords; i++ {
		rec := testRecord(i, fmt.Sprintf("k%d", i), fmt.Sprintf("val%d", i))
		if err := writer.AppendSync(rec); err != nil {
			_ = writer.Close()
			t.Fatalf("append failed at %d: %v", i, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close writer: %v", err)
	}

	// Verify multiple segments were created
	segs, err := wal.ListSegments(dbPath)
	if err != nil {
		t.Fatalf("ListSegments failed: %v", err)
	}
	if len(segs) < 3 {
		t.Fatalf("expected at least 3 segments from rotations, got %d", len(segs))
	}

	// Recover WAL
	sink := &recordingSink{}
	report, err := wal.RecoverWAL(dbPath, sink)
	if err != nil {
		t.Fatalf("recovery failed: %v", err)
	}

	if report.SegmentCount != len(segs) {
		t.Errorf("SegmentCount mismatch: got %d, expected %d", report.SegmentCount, len(segs))
	}
	if report.ValidRecords != totalRecords || report.ReplayedRecords != totalRecords {
		t.Errorf("record count mismatch: valid=%d, replayed=%d, want=%d", report.ValidRecords, report.ReplayedRecords, totalRecords)
	}
	if report.LastSeqNum != binary.SeqNum(totalRecords) {
		t.Errorf("expected LastSeqNum %d, got %d", totalRecords, report.LastSeqNum)
	}
	if len(sink.records) != totalRecords {
		t.Fatalf("expected %d sink records, got %d", totalRecords, len(sink.records))
	}

	for i, rec := range sink.records {
		expectedSeq := uint64(i + 1)
		if rec.SeqNum != binary.SeqNum(expectedSeq) {
			t.Errorf("record %d seq mismatch: got %d, want %d", i, rec.SeqNum, expectedSeq)
		}
	}
}

// ADVERSARIAL TEST 1: segment 1 clean, segment 2 torn, segment 3 valid -> fails closed, no mutation
func TestCoordinator_Adversarial_HistoricalTornTail_WithValidLatest(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	seg1Path := writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2)
	seg3Path := writeSegmentRecords(t, dbPath, 3, r3)

	// Append torn tail to middle segment 2
	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte{0xCA, 0xFE}); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	hash1 := fileSHA256(t, seg1Path)
	hash2 := fileSHA256(t, seg2Path)
	hash3 := fileSHA256(t, seg3Path)

	sink := &recordingSink{}
	_, err = wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected failure on torn historical segment, got nil")
	}

	// Must fail closed without mutating ANY segment
	if fileSHA256(t, seg1Path) != hash1 {
		t.Errorf("segment 1 was mutated")
	}
	if fileSHA256(t, seg2Path) != hash2 {
		t.Errorf("segment 2 was mutated (historical torn tail must NOT be truncated)")
	}
	if fileSHA256(t, seg3Path) != hash3 {
		t.Errorf("segment 3 was mutated")
	}
}

// ADVERSARIAL TEST 2: segment 1 clean, segment 2 complete CRC corruption, segment 3 valid -> fails closed, no mutation
func TestCoordinator_Adversarial_HistoricalCRC_WithValidLatest(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	seg1Path := writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2)
	seg3Path := writeSegmentRecords(t, dbPath, 3, r3)

	// Corrupt CRC of record in segment 2
	f, err := os.OpenFile(seg2Path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	var b [1]byte
	if _, err := f.Read(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("read failed: %v", err)
	}
	b[0] ^= 0x55
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek failed: %v", err)
	}
	if _, err := f.Write(b[:]); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	hash1 := fileSHA256(t, seg1Path)
	hash2 := fileSHA256(t, seg2Path)
	hash3 := fileSHA256(t, seg3Path)

	sink := &recordingSink{}
	_, err = wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected failure on corrupted historical segment, got nil")
	}

	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}

	// No mutation on any file
	if fileSHA256(t, seg1Path) != hash1 || fileSHA256(t, seg2Path) != hash2 || fileSHA256(t, seg3Path) != hash3 {
		t.Errorf("files were mutated on corruption")
	}
}

// ADVERSARIAL TEST 3: latest segment has torn tail, replay sink fails halfway -> returns sink error, report.Truncated is true
func TestCoordinator_Adversarial_SinkFailureAfterTruncation(t *testing.T) {
	dbPath := t.TempDir()
	r1 := testRecord(1, "k1", "v1")
	r2 := testRecord(2, "k2", "v2")
	r3 := testRecord(3, "k3", "v3")

	writeSegmentRecords(t, dbPath, 1, r1)
	seg2Path := writeSegmentRecords(t, dbPath, 2, r2, r3)

	// Append torn tail to latest segment
	f, err := os.OpenFile(seg2Path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := f.Write([]byte{0x99, 0x88, 0x77}); err != nil {
		_ = f.Close()
		t.Fatalf("write failed: %v", err)
	}
	_ = f.Close()

	sinkErr := stdErrors.New("sink out of memory")
	sink := &failingSink{
		failAfter: 1, // Succeeds on r1, fails on r2
		err:       sinkErr,
	}

	report, err := wal.RecoverWAL(dbPath, sink)
	if err == nil {
		t.Fatalf("expected sink failure, got nil")
	}
	if !stdErrors.Is(err, sinkErr) {
		t.Errorf("expected sink error %v, got: %v", sinkErr, err)
	}

	// Crucial contract: Truncation DID happen physically on disk before replay failed.
	// The report must reflect this state honestly!
	if !report.Truncated {
		t.Errorf("report.Truncated must be true because physical file was truncated")
	}
	if report.TruncatedBytes != 3 {
		t.Errorf("expected TruncatedBytes 3, got %d", report.TruncatedBytes)
	}
	if report.ReplayedRecords != 1 {
		t.Errorf("expected ReplayedRecords 1, got %d", report.ReplayedRecords)
	}
}

// Validation-only mode (sink == nil)
func TestCoordinator_ValidationOnlyMode(t *testing.T) {
	dbPath := t.TempDir()
	writeSegmentRecords(t, dbPath, 1, testRecord(1, "k1", "v1"), testRecord(2, "k2", "v2"))
	writeSegmentRecords(t, dbPath, 2, testRecord(3, "k3", "v3"))

	// Pass nil sink -> should validate all records without panicking or erroring
	report, err := wal.RecoverWAL(dbPath, nil)
	if err != nil {
		t.Fatalf("validation-only mode failed: %v", err)
	}
	if report.ValidRecords != 3 || report.ReplayedRecords != 3 {
		t.Errorf("unexpected counts: valid=%d, replayed=%d", report.ValidRecords, report.ReplayedRecords)
	}
	if report.LastSeqNum != 3 {
		t.Errorf("expected LastSeqNum 3, got %d", report.LastSeqNum)
	}
}

// Empty DB Path validation
func TestCoordinator_EmptyDBPath(t *testing.T) {
	_, err := wal.RecoverWAL("", nil)
	if err == nil {
		t.Fatalf("expected error on empty dbPath, got nil")
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid, got: %v", err)
	}
}

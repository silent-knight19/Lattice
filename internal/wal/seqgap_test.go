package wal_test

import (
	stderrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// =============================================================================
// Sequence-gap observability.
//
// WAL recovery enforces monotonicity (a regression fails closed with
// SequenceOutOfOrderError) but deliberately does NOT enforce contiguity. Gaps are
// legitimately produced by three code paths, so treating one as corruption would
// refuse to open healthy databases:
//
//  1. Engine.Put / Delete / Batch allocate a SeqNum and then fail to persist a
//     record; nextSeqNum is never rolled back.
//  2. Batch reserves N+2 sequence numbers up front and can fail mid-loop.
//  3. Post-recovery the engine seeds nextSeqNum from
//     max(manifest checkpoint, WAL last SeqNum), so the next write can legitimately
//     jump. This is the case that makes strict detection a permanent boot failure
//     the moment WAL segment GC lands.
//
// Recovery therefore observes gaps and reports them, but never rejects on one.
// =============================================================================

// writeRawRecords appends records with arbitrary (possibly gapped) sequence
// numbers directly into a segment, bypassing the Engine allocator.
func writeRawRecords(t *testing.T, dbPath string, seqs []uint64) {
	t.Helper()
	rw, err := wal.OpenRotatingWriter(dbPath, wal.Options{})
	if err != nil {
		t.Fatalf("OpenRotatingWriter: %v", err)
	}
	for _, seq := range seqs {
		rec := wal.Record{
			Type:   wal.RecordTypePut,
			SeqNum: binary.SeqNum(seq),
			Key:    []byte("k"),
			Value:  []byte("v"),
		}
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync(%d): %v", seq, err)
		}
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSeqGap_ObservedButNotFatal is the core behavior: a gapped WAL recovers
// successfully and reports the gap.
func TestSeqGap_ObservedButNotFatal(t *testing.T) {
	dbPath := t.TempDir()
	writeRawRecords(t, dbPath, []uint64{1, 2, 7, 8, 20})

	var sink countingSink
	rep, err := wal.RecoverWAL(dbPath, &sink)
	if err != nil {
		t.Fatalf("a gapped WAL must still recover; got %v. Strict contiguity here would "+
			"refuse to open databases whose gaps are legitimate", err)
	}
	if rep.ValidRecords != 5 {
		t.Errorf("ValidRecords = %d; want 5 (every record must still be replayed)", rep.ValidRecords)
	}
	if len(sink.seqs) != 5 {
		t.Errorf("replayed %d records; want 5", len(sink.seqs))
	}
	if rep.SequenceGaps != 2 {
		t.Errorf("SequenceGaps = %d; want 2 (1->7 and 8->20)", rep.SequenceGaps)
	}
	// skipped = (7-2-1) + (20-8-1) = 4 + 11 = 15
	if rep.SkippedSequenceNumbers != 15 {
		t.Errorf("SkippedSequenceNumbers = %d; want 15", rep.SkippedSequenceNumbers)
	}
	if rep.LastSeqNum != 20 {
		t.Errorf("LastSeqNum = %d; want 20", rep.LastSeqNum)
	}
}

// TestSeqGap_ContiguousWALReportsNoGaps guards against false positives in the
// observability signal itself.
func TestSeqGap_ContiguousWALReportsNoGaps(t *testing.T) {
	dbPath := t.TempDir()
	writeRawRecords(t, dbPath, []uint64{1, 2, 3, 4, 5})

	rep, err := wal.RecoverWAL(dbPath, nil)
	if err != nil {
		t.Fatalf("RecoverWAL: %v", err)
	}
	if rep.SequenceGaps != 0 {
		t.Errorf("SequenceGaps = %d on a contiguous WAL; want 0", rep.SequenceGaps)
	}
	if rep.SkippedSequenceNumbers != 0 {
		t.Errorf("SkippedSequenceNumbers = %d on a contiguous WAL; want 0", rep.SkippedSequenceNumbers)
	}
}

// TestSeqGap_SingleRecordWALReportsNoGaps: a lone record has no predecessor.
func TestSeqGap_SingleRecordWALReportsNoGaps(t *testing.T) {
	dbPath := t.TempDir()
	writeRawRecords(t, dbPath, []uint64{99})

	rep, err := wal.RecoverWAL(dbPath, nil)
	if err != nil {
		t.Fatalf("RecoverWAL: %v", err)
	}
	if rep.SequenceGaps != 0 {
		t.Errorf("SequenceGaps = %d for a single-record WAL; want 0", rep.SequenceGaps)
	}
}

// TestSeqGap_RegressionStillFailsClosed is the important counterpart: tolerating
// gaps must NOT weaken the monotonicity guarantee. A regression remains fatal.
func TestSeqGap_RegressionStillFailsClosed(t *testing.T) {
	dbPath := t.TempDir()
	writeRawRecords(t, dbPath, []uint64{1, 5, 3})

	_, err := wal.RecoverWAL(dbPath, nil)
	if err == nil {
		t.Fatal("a sequence regression must still fail closed; tolerating gaps must not " +
			"weaken the monotonicity invariant")
	}
	var seqErr *latticeerrors.SequenceOutOfOrderError
	if !stderrors.As(err, &seqErr) {
		t.Errorf("regression returned %T (%v); want *errors.SequenceOutOfOrderError", err, err)
	}
}

// TestSeqGap_AcrossSegmentBoundary covers gaps spanning the historical/replay
// phase boundary and the transition into the active segment.
func TestSeqGap_AcrossSegmentBoundary(t *testing.T) {
	dbPath := t.TempDir()

	rw, err := wal.OpenRotatingWriter(dbPath, wal.Options{})
	if err != nil {
		t.Fatalf("OpenRotatingWriter: %v", err)
	}
	// Segment 1: contiguous.
	for _, seq := range []uint64{1, 2, 3} {
		if err := rw.AppendSync(wal.Record{
			Type: wal.RecordTypePut, SeqNum: binary.SeqNum(seq),
			Key: []byte("k"), Value: []byte("v"),
		}); err != nil {
			t.Fatalf("AppendSync(%d): %v", seq, err)
		}
	}
	if err := rw.Rotate(); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	// Segment 2: a gap across the segment boundary.
	for _, seq := range []uint64{10, 11} {
		if err := rw.AppendSync(wal.Record{
			Type: wal.RecordTypePut, SeqNum: binary.SeqNum(seq),
			Key: []byte("k"), Value: []byte("v"),
		}); err != nil {
			t.Fatalf("AppendSync(%d): %v", seq, err)
		}
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var sink countingSink
	rep, err := wal.RecoverWAL(dbPath, &sink)
	if err != nil {
		t.Fatalf("cross-segment gap must recover: %v", err)
	}
	if rep.ValidRecords != 5 {
		t.Errorf("ValidRecords = %d; want 5", rep.ValidRecords)
	}
	if rep.SequenceGaps != 1 {
		t.Errorf("SequenceGaps = %d; want 1 (the 3 -> 10 boundary jump)", rep.SequenceGaps)
	}
	if rep.SkippedSequenceNumbers != 6 {
		t.Errorf("SkippedSequenceNumbers = %d; want 6 (4..9)", rep.SkippedSequenceNumbers)
	}
}

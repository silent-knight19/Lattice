package wal_test

import (
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/wal"
)

func TestSEC03_TornWrite_01_CrashBeforeWrite(t *testing.T) {
	h := NewSecurityHarness(t)

	// Segment 1 has 2 clean records. Crash occurs before Record 3 write begins.
	segPath := h.CreateSegmentWithRecords(1, 1, 2)

	res, err := wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if res.Truncated {
		t.Errorf("clean segment before write should not be truncated")
	}
	if res.ValidRecords != 2 {
		t.Errorf("expected 2 valid records, got %d", res.ValidRecords)
	}
}

func TestSEC03_TornWrite_02_CrashDuringHeaderWrite(t *testing.T) {
	h := NewSecurityHarness(t)
	segPath := h.CreateSegmentWithRecords(1, 1, 2)
	rec1Size := wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0"))
	rec2Size := wal.RecordWireSize(h.MakeRecord(2, "k-1-1", "v-1-1"))
	cleanPrefixSize := rec1Size + rec2Size

	// Simulate crash mid-header: append 7 bytes of a new 21-byte header
	partialHeader := []byte{0xDE, 0xAD, 0xBE, 0xEF, byte(wal.RecordTypePut), 0x00, 0x03}
	if err := h.AppendRawBytes(segPath, partialHeader); err != nil {
		t.Fatalf("AppendRawBytes failed: %v", err)
	}

	res, err := wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment on partial header failed: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected physical truncation of partial header")
	}
	if res.RecoveredOffset != cleanPrefixSize {
		t.Errorf("expected recovered offset %d, got %d", cleanPrefixSize, res.RecoveredOffset)
	}
	if res.ValidRecords != 2 {
		t.Errorf("expected 2 valid records preserved, got %d", res.ValidRecords)
	}

	// Verify physical file size matches clean prefix
	info, _ := os.Stat(segPath)
	if info.Size() != cleanPrefixSize {
		t.Errorf("file size on disk %d does not match clean prefix %d", info.Size(), cleanPrefixSize)
	}
}

func TestSEC03_TornWrite_03_CrashDuringKeyPayload(t *testing.T) {
	h := NewSecurityHarness(t)
	segPath := h.CreateSegmentWithRecords(1, 1, 1)
	cleanPrefixSize := wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0"))

	// Encode a complete Record 2, but write only header + 2 bytes of key
	rec2 := h.MakeRecord(2, "long-key-for-test", "val-for-test")
	rec2Bytes, _ := wal.EncodeRecord(rec2)
	partialBytes := rec2Bytes[:wal.MinRecordSize+2]

	if err := h.AppendRawBytes(segPath, partialBytes); err != nil {
		t.Fatalf("AppendRawBytes: %v", err)
	}

	res, err := wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated || res.RecoveredOffset != cleanPrefixSize || res.ValidRecords != 1 {
		t.Errorf("expected clean recovery to prefix %d with 1 record; got %+v", cleanPrefixSize, res)
	}
}

func TestSEC03_TornWrite_04_CrashDuringValuePayload(t *testing.T) {
	h := NewSecurityHarness(t)
	segPath := h.CreateSegmentWithRecords(1, 1, 1)
	cleanPrefixSize := wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0"))

	rec2 := h.MakeRecord(2, "key2", "value-payload-that-gets-cut-off")
	rec2Bytes, _ := wal.EncodeRecord(rec2)
	// Cut off 5 bytes before the end of the value payload
	partialBytes := rec2Bytes[:len(rec2Bytes)-5]

	if err := h.AppendRawBytes(segPath, partialBytes); err != nil {
		t.Fatalf("AppendRawBytes: %v", err)
	}

	res, err := wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated || res.RecoveredOffset != cleanPrefixSize || res.ValidRecords != 1 {
		t.Errorf("expected clean recovery to prefix %d with 1 record; got %+v", cleanPrefixSize, res)
	}
}

func TestSEC03_TornWrite_05_HistoricalSealedSegmentTornTailFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	// Segment 1 (Historical) has a torn tail at EOF
	seg1Path := h.CreateSegmentWithRecords(1, 1, 2)
	_ = h.AppendRawBytes(seg1Path, []byte{0x01, 0x02, 0x03}) // torn 3 bytes in historical segment 1

	// Segment 2 (Latest Active) has valid record 3
	seg2Path := wal.SegmentPath(h.RootDir(), 2)
	w2, err := wal.CreateWriter(seg2Path)
	if err != nil {
		t.Fatalf("CreateWriter seg2: %v", err)
	}
	_ = w2.Append(h.MakeRecord(3, "k3", "v3"))
	_ = w2.Sync()
	_ = w2.Close()

	// RecoverWAL MUST fail closed on historical segment 1!
	// Invariant: Historical segments are NEVER truncated.
	_, err = wal.RecoverWAL(h.RootDir(), nil)
	if err == nil {
		t.Fatalf("expected RecoverWAL to fail closed on historical segment torn tail")
	}

	// Verify segment 1 was NOT physically modified
	info1, _ := os.Stat(seg1Path)
	expectedSize := wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0"))*2 + 3
	if info1.Size() != expectedSize {
		t.Fatalf("historical segment was illegally truncated! size=%d, expected=%d", info1.Size(), expectedSize)
	}
}

func TestSEC03_TornWrite_06_BatchesCrossingRotations(t *testing.T) {
	h := NewSecurityHarness(t)

	// Configure small segment size so batches span across rotation boundaries
	opts := wal.Options{
		SegmentSize: 120, // force rotation after ~2 records
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}

	// Append 6 sequential records with Sync
	for i := 1; i <= 6; i++ {
		rec := h.MakeRecord(uint64(i), "k-rot", "v-rot-batch-payload")
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync record %d failed: %v", i, err)
		}
	}
	_ = rw.Close()

	// Simulate crash on latest segment: append 4 torn bytes to the newest segment
	ids, err := wal.ListSegments(h.RootDir())
	if err != nil || len(ids) < 2 {
		t.Fatalf("expected multiple segments across rotation, got ids=%v, err=%v", ids, err)
	}
	latestSeg := wal.SegmentPath(h.RootDir(), ids[len(ids)-1])
	_ = h.AppendRawBytes(latestSeg, []byte{0xAA, 0xBB, 0xCC, 0xDD})

	// RecoverWAL should successfully process historical segments untouched,
	// truncate the torn 4 bytes from the latest segment, and recover all 6 committed records.
	var replayed []wal.Record
	report, err := wal.RecoverWAL(h.RootDir(), wal.ReplayFunc(func(rec wal.Record) error {
		replayed = append(replayed, rec)
		return nil
	}))
	if err != nil {
		t.Fatalf("RecoverWAL failed on rotated segments with torn tail: %v", err)
	}

	if report.SegmentCount != len(ids) {
		t.Errorf("expected %d segments processed, got %d", len(ids), report.SegmentCount)
	}
	if !report.Truncated {
		t.Errorf("expected latest segment to be marked truncated")
	}
	if len(replayed) != 6 {
		t.Errorf("expected 6 records replayed, got %d", len(replayed))
	}
	for i, r := range replayed {
		expectedSeq := uint64(i + 1)
		if uint64(r.SeqNum) != expectedSeq {
			t.Errorf("record %d seq mismatch: got %d, want %d", i, r.SeqNum, expectedSeq)
		}
	}
}

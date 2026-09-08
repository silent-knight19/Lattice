package wal_test

import (
	"bytes"
	"crypto/sha256"
	stdErrors "errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// helper to make valid Record with specified key and val
func makeRotRecord(seq uint64, key, val string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: uint64(time.Now().UnixNano()),
		Key:       []byte(key),
		Value:     []byte(val),
	}
}

// 1. Initial segment creation.
func TestRotation_InitialSegmentCreation(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{
		SegmentSize: 1024,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	if rw.ActiveSegmentID() != 1 {
		t.Errorf("expected active segment ID 1, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != 0 {
		t.Errorf("expected active segment size 0, got %d", rw.ActiveSegmentSize())
	}

	expectedPath := wal.SegmentPath(dir, 1)
	if rw.ActivePath() != expectedPath {
		t.Errorf("expected active path %s, got %s", expectedPath, rw.ActivePath())
	}

	info, statErr := os.Stat(expectedPath)
	if statErr != nil {
		t.Fatalf("Stat initial segment failed: %v", statErr)
	}
	if info.Mode().Perm() != wal.FileMode {
		t.Errorf("expected file mode %v, got %v", wal.FileMode, info.Mode().Perm())
	}
	if info.Size() != 0 {
		t.Errorf("expected file size 0, got %d", info.Size())
	}
}

// 2. Append multiple records without rotation.
func TestRotation_AppendMultipleRecordsWithoutRotation(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{
		SegmentSize: 10 * 1024,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	var expectedTotalSize int64
	for i := uint64(1); i <= 5; i++ {
		rec := makeRotRecord(i, fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i))
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync record %d failed: %v", i, err)
		}
		expectedTotalSize += wal.RecordWireSize(rec)
	}

	if rw.ActiveSegmentID() != 1 {
		t.Errorf("expected segment ID 1 without rotation, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != expectedTotalSize {
		t.Errorf("expected active segment size %d, got %d", expectedTotalSize, rw.ActiveSegmentSize())
	}

	segs, err := rw.Segments()
	if err != nil {
		t.Fatalf("Segments() failed: %v", err)
	}
	if len(segs) != 1 || segs[0] != 1 {
		t.Errorf("expected segments [1], got %v", segs)
	}
}

// 3. Rotation at exact threshold.
func TestRotation_AtExactThreshold(t *testing.T) {
	dir := t.TempDir()
	// Create two records each of known wire size
	rec1 := makeRotRecord(1, "k1", "v1") // Wire size: 27 + 2 + 2 = 31 bytes
	rec1Size := wal.RecordWireSize(rec1)

	// Configure segment size to exactly 2 * rec1Size (62 bytes)
	segSize := 2 * rec1Size
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{
		SegmentSize: segSize,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Append rec1: size becomes 31 <= 62 -> stays in segment 1
	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("Append rec1 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 1 {
		t.Fatalf("expected segment 1, got %d", rw.ActiveSegmentID())
	}

	// Append rec2 (same size 31): 31 + 31 = 62 == segSize -> EXACT FIT -> stays in segment 1
	rec2 := makeRotRecord(2, "k2", "v2")
	if err := rw.AppendSync(rec2); err != nil {
		t.Fatalf("Append rec2 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 1 {
		t.Fatalf("expected segment 1 on exact fit, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != segSize {
		t.Fatalf("expected segment size %d, got %d", segSize, rw.ActiveSegmentSize())
	}

	// Append rec3: current size 62 > 0, 62 + 31 = 93 > 62 -> MUST ROTATE to segment 2 before append
	rec3 := makeRotRecord(3, "k3", "v3")
	if err := rw.AppendSync(rec3); err != nil {
		t.Fatalf("Append rec3 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Fatalf("expected segment 2 after exact threshold exceeded, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != rec1Size {
		t.Fatalf("expected segment 2 size %d, got %d", rec1Size, rw.ActiveSegmentSize())
	}

	// Verify segment 1 physical size on disk is exactly segSize
	info1, err := os.Stat(wal.SegmentPath(dir, 1))
	if err != nil {
		t.Fatalf("Stat segment 1 failed: %v", err)
	}
	if info1.Size() != segSize {
		t.Errorf("expected segment 1 size %d, got %d", segSize, info1.Size())
	}
}

// 4. Rotation just before threshold.
func TestRotation_JustBeforeThreshold(t *testing.T) {
	dir := t.TempDir()
	rec1 := makeRotRecord(1, "k1", "v1") // 31 bytes
	rec1Size := wal.RecordWireSize(rec1)

	// Threshold is rec1Size + 1 (32 bytes)
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{
		SegmentSize: rec1Size + 1,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("Append rec1 failed: %v", err)
	}
	// Active size is 31, threshold is 32. 31 < 32 (just before threshold)
	if rw.ActiveSegmentID() != 1 {
		t.Fatalf("expected segment 1, got %d", rw.ActiveSegmentID())
	}

	// Next record is 31 bytes: 31 + 31 = 62 > 32 -> triggers rotation
	rec2 := makeRotRecord(2, "k2", "v2")
	if err := rw.AppendSync(rec2); err != nil {
		t.Fatalf("Append rec2 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Fatalf("expected segment 2, got %d", rw.ActiveSegmentID())
	}
}

// 5. Rotation crossing threshold.
func TestRotation_CrossingThreshold(t *testing.T) {
	dir := t.TempDir()
	rec1 := makeRotRecord(1, "key-short", "val-short") // 27 + 9 + 9 = 45 bytes
	rec1Size := wal.RecordWireSize(rec1)

	// Segment size = 50
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{
		SegmentSize: 50,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("Append rec1 failed: %v", err)
	}

	// Next record is 45 bytes: 45 + 45 = 90 > 50 -> crosses threshold, rotates
	rec2 := makeRotRecord(2, "key-cross", "val-cross")
	if err := rw.AppendSync(rec2); err != nil {
		t.Fatalf("Append rec2 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Fatalf("expected segment 2, got %d", rw.ActiveSegmentID())
	}

	info1, _ := os.Stat(wal.SegmentPath(dir, 1))
	info2, _ := os.Stat(wal.SegmentPath(dir, 2))
	if info1.Size() != rec1Size {
		t.Errorf("expected segment 1 size %d, got %d", rec1Size, info1.Size())
	}
	if info2.Size() != wal.RecordWireSize(rec2) {
		t.Errorf("expected segment 2 size %d, got %d", wal.RecordWireSize(rec2), info2.Size())
	}
}

// 6. Old segment remains intact (byte-for-byte SHA256 match).
func TestRotation_OldSegmentRemainsIntact(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{
		SegmentSize: 50,
	})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	rec1 := makeRotRecord(1, "key1", "val1")
	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("Append rec1 failed: %v", err)
	}

	// Read segment 1 bytes and compute SHA256 before rotation
	path1 := wal.SegmentPath(dir, 1)
	bytesBefore, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("ReadFile segment 1 failed: %v", err)
	}
	hashBefore := sha256.Sum256(bytesBefore)

	// Trigger rotation and append to segment 2
	rec2 := makeRotRecord(2, "key2", "val2")
	if err := rw.AppendSync(rec2); err != nil {
		t.Fatalf("Append rec2 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Fatalf("expected segment 2, got %d", rw.ActiveSegmentID())
	}

	// Verify segment 1 bytes are 100% identical after rotation
	bytesAfter, err := os.ReadFile(path1)
	if err != nil {
		t.Fatalf("ReadFile segment 1 after rotation failed: %v", err)
	}
	hashAfter := sha256.Sum256(bytesAfter)

	if !bytes.Equal(hashBefore[:], hashAfter[:]) {
		t.Fatalf("segment 1 bytes were mutated during or after rotation!")
	}
}

// 7. New segment starts empty.
func TestRotation_NewSegmentStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	_ = rw.AppendSync(makeRotRecord(1, "k", "v"))

	// Explicit Rotate()
	if err := rw.Rotate(); err != nil {
		t.Fatalf("Rotate failed: %v", err)
	}

	if rw.ActiveSegmentID() != 2 {
		t.Fatalf("expected segment 2, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != 0 {
		t.Fatalf("expected active size 0 for new segment, got %d", rw.ActiveSegmentSize())
	}

	info, err := os.Stat(wal.SegmentPath(dir, 2))
	if err != nil {
		t.Fatalf("Stat segment 2 failed: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected physical file size 0 for new segment, got %d", info.Size())
	}
}

// 8. Subsequent append goes only to new segment.
func TestRotation_SubsequentAppendGoesOnlyToNewSegment(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 50})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	rec1 := makeRotRecord(1, "k1", "v1")
	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("Append rec1 failed: %v", err)
	}

	rec2 := makeRotRecord(2, "k2", "v2")
	if err := rw.AppendSync(rec2); err != nil {
		t.Fatalf("Append rec2 failed: %v", err)
	}

	r1, err := wal.OpenSegmentReader(dir, 1)
	if err != nil {
		t.Fatalf("OpenSegmentReader 1 failed: %v", err)
	}
	defer func() { _ = r1.Close() }()

	readRec1, err := r1.Next()
	if err != nil {
		t.Fatalf("r1.Next failed: %v", err)
	}
	if string(readRec1.Key) != "k1" {
		t.Errorf("expected k1 in segment 1, got %s", string(readRec1.Key))
	}
	if _, err := r1.Next(); !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF in segment 1 after rec1, got %v", err)
	}

	r2, err := wal.OpenSegmentReader(dir, 2)
	if err != nil {
		t.Fatalf("OpenSegmentReader 2 failed: %v", err)
	}
	defer func() { _ = r2.Close() }()

	readRec2, err := r2.Next()
	if err != nil {
		t.Fatalf("r2.Next failed: %v", err)
	}
	if string(readRec2.Key) != "k2" {
		t.Errorf("expected k2 in segment 2, got %s", string(readRec2.Key))
	}
	if _, err := r2.Next(); !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF in segment 2 after rec2, got %v", err)
	}
}

// 9. Segment IDs increment correctly.
func TestRotation_SegmentIDsIncrementCorrectly(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	for expectedID := uint64(2); expectedID <= 10; expectedID++ {
		if err := rw.Rotate(); err != nil {
			t.Fatalf("Rotate to %d failed: %v", expectedID, err)
		}
		if rw.ActiveSegmentID() != expectedID {
			t.Fatalf("expected active segment ID %d, got %d", expectedID, rw.ActiveSegmentID())
		}
	}
}

// 10. No ID reuse.
func TestRotation_NoIDReuse(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	for i := 0; i < 5; i++ {
		if err := rw.Rotate(); err != nil {
			t.Fatalf("Rotate failed: %v", err)
		}
	}

	segs, err := rw.Segments()
	if err != nil {
		t.Fatalf("Segments failed: %v", err)
	}
	if len(segs) != 6 {
		t.Fatalf("expected 6 segments, got %d: %v", len(segs), segs)
	}

	seen := make(map[uint64]bool)
	for _, id := range segs {
		if seen[id] {
			t.Fatalf("duplicate segment ID %d detected!", id)
		}
		seen[id] = true
	}
}

// 11. Existing next-segment file collision rejected.
func TestRotation_ExistingNextSegmentFileCollisionRejected(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Pre-create segment 2 file with sensitive foreign content
	seg2Path := wal.SegmentPath(dir, 2)
	foreignData := []byte("CRITICAL UNRELATED DATA")
	if err := os.WriteFile(seg2Path, foreignData, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Attempt rotation from 1 to 2 -> must fail with os.ErrExist
	err = rw.Rotate()
	if err == nil {
		t.Fatalf("expected rotation to fail on existing file collision, got nil")
	}
	if !stdErrors.Is(err, os.ErrExist) {
		t.Errorf("expected error wrapping os.ErrExist, got: %v", err)
	}

	// Verify foreign file was not overwritten or truncated
	content, readErr := os.ReadFile(seg2Path)
	if readErr != nil {
		t.Fatalf("ReadFile failed: %v", readErr)
	}
	if !bytes.Equal(content, foreignData) {
		t.Fatalf("foreign file was mutated by collision! content: %s", string(content))
	}

	// Verify active writer state is marked inactive (no active segment)
	if rw.ActiveSegmentID() != 0 {
		t.Errorf("expected ActiveSegmentID == 0 after failed rotation, got %d", rw.ActiveSegmentID())
	}
}

// 12. Creation failure propagated.
func TestRotation_CreationFailurePropagated(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	injectedErr := stdErrors.New("injected disk failure")
	rw.SetCreateWriterFnForTesting(func(path string) (*wal.WALWriter, error) {
		return nil, injectedErr
	})

	err = rw.Rotate()
	if err == nil {
		t.Fatalf("expected injected error from Rotate, got nil")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected injected error, got: %v", err)
	}
}

// 13. Permission failure propagated.
func TestRotation_PermissionFailurePropagated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping permission test when running as root")
	}

	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	walDir := wal.Dir(dir)
	// Make WAL directory read-only so segment 2 creation fails
	if err := os.Chmod(walDir, 0500); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(walDir, 0700)
	})

	err = rw.Rotate()
	if err == nil {
		t.Fatalf("expected permission error, got nil")
	}
	if !stdErrors.Is(err, fs.ErrPermission) {
		t.Errorf("expected error wrapping fs.ErrPermission, got: %v", err)
	}
}

// 14. Multiple sequential rotations.
func TestRotation_MultipleSequentialRotations(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 50})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Write 10 records, causing multiple automatic rotations
	for i := uint64(1); i <= 10; i++ {
		rec := makeRotRecord(i, fmt.Sprintf("k-%02d", i), "v")
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync %d failed: %v", i, err)
		}
	}

	segs, err := rw.Segments()
	if err != nil {
		t.Fatalf("Segments failed: %v", err)
	}
	if len(segs) < 5 {
		t.Errorf("expected at least 5 segments, got %d: %v", len(segs), segs)
	}

	// Verify all segments exist and are strictly ordered
	for i := 0; i < len(segs); i++ {
		if segs[i] != uint64(i+1) {
			t.Errorf("expected segment %d at index %d, got %d", i+1, i, segs[i])
		}
	}
}

// 15. Both old and new segments readable independently.
func TestRotation_BothOldAndNewSegmentsReadableIndependently(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	rec1 := makeRotRecord(1, "key1", "val1")
	rec2 := makeRotRecord(2, "key2", "val2")
	_ = rw.AppendSync(rec1)
	_ = rw.AppendSync(rec2)

	_ = rw.Rotate()

	rec3 := makeRotRecord(3, "key3", "val3")
	rec4 := makeRotRecord(4, "key4", "val4")
	_ = rw.AppendSync(rec3)
	_ = rw.AppendSync(rec4)

	// Read segment 1 independently
	r1, err := wal.OpenSegmentReader(dir, 1)
	if err != nil {
		t.Fatalf("OpenSegmentReader 1 failed: %v", err)
	}
	defer func() { _ = r1.Close() }()

	r1Rec1, err := r1.Next()
	if err != nil || string(r1Rec1.Key) != "key1" {
		t.Fatalf("unexpected rec 1: %v, err: %v", r1Rec1, err)
	}
	r1Rec2, err := r1.Next()
	if err != nil || string(r1Rec2.Key) != "key2" {
		t.Fatalf("unexpected rec 2: %v, err: %v", r1Rec2, err)
	}
	if _, err := r1.Next(); !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected EOF in segment 1, got %v", err)
	}

	// Read segment 2 independently
	r2, err := wal.OpenSegmentReader(dir, 2)
	if err != nil {
		t.Fatalf("OpenSegmentReader 2 failed: %v", err)
	}
	defer func() { _ = r2.Close() }()

	r2Rec1, err := r2.Next()
	if err != nil || string(r2Rec1.Key) != "key3" {
		t.Fatalf("unexpected rec 3: %v, err: %v", r2Rec1, err)
	}
	r2Rec2, err := r2.Next()
	if err != nil || string(r2Rec2.Key) != "key4" {
		t.Fatalf("unexpected rec 4: %v, err: %v", r2Rec2, err)
	}
	if _, err := r2.Next(); !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected EOF in segment 2, got %v", err)
	}
}

// 16. Record never split across segments.
func TestRotation_RecordNeverSplitAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 64})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	for i := uint64(1); i <= 8; i++ {
		rec := makeRotRecord(i, fmt.Sprintf("k%d", i), "v")
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync %d failed: %v", i, err)
		}
	}

	segs, _ := rw.Segments()
	for _, id := range segs {
		r, err := wal.OpenSegmentReader(dir, id)
		if err != nil {
			t.Fatalf("OpenSegmentReader %d failed: %v", id, err)
		}
		for {
			_, err := r.Next()
			if err != nil {
				if stdErrors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("segment %d contained split/corrupted record: %v", id, err)
			}
		}
		_ = r.Close()
	}
}

// 17. Oversized record behavior.
func TestRotation_OversizedRecordBehavior(t *testing.T) {
	dir := t.TempDir()
	// Segment size = 50 bytes
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 50})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Big record: 27 + 10 + 100 = 137 bytes (exceeds segment size 50)
	bigVal := string(bytes.Repeat([]byte("v"), 100))
	bigRec := makeRotRecord(1, "key-large1", bigVal)
	bigSize := wal.RecordWireSize(bigRec)

	// Since segment 1 is empty, oversized record MUST be accepted into segment 1
	if err := rw.AppendSync(bigRec); err != nil {
		t.Fatalf("AppendSync oversized record to empty segment failed: %v", err)
	}
	if rw.ActiveSegmentID() != 1 {
		t.Errorf("expected segment 1, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != bigSize {
		t.Errorf("expected active size %d, got %d", bigSize, rw.ActiveSegmentSize())
	}

	// Next record (size 31 bytes): since segment 1 size is 137 > 50, MUST rotate to segment 2!
	smallRec := makeRotRecord(2, "k2", "v2")
	if err := rw.AppendSync(smallRec); err != nil {
		t.Fatalf("AppendSync next record failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Errorf("expected segment 2, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != wal.RecordWireSize(smallRec) {
		t.Errorf("expected segment 2 size %d, got %d", wal.RecordWireSize(smallRec), rw.ActiveSegmentSize())
	}
}

// 18. Rotation with empty active segment.
func TestRotation_WithEmptyActiveSegment(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	if rw.ActiveSegmentID() != 1 {
		t.Fatalf("expected segment 1, got %d", rw.ActiveSegmentID())
	}

	// Rotate empty segment
	if err := rw.Rotate(); err != nil {
		t.Fatalf("Rotate on empty segment failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Errorf("expected segment 2, got %d", rw.ActiveSegmentID())
	}

	// Segment 1 should be a 0-byte file
	info1, err := os.Stat(wal.SegmentPath(dir, 1))
	if err != nil {
		t.Fatalf("Stat segment 1 failed: %v", err)
	}
	if info1.Size() != 0 {
		t.Errorf("expected segment 1 size 0, got %d", info1.Size())
	}
}

// 19. Rotation state after writer close.
func TestRotation_StateAfterWriterClose(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}

	if err := rw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Operations on closed writer
	if err := rw.AppendSync(makeRotRecord(1, "k", "v")); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("expected ErrWriterClosed from AppendSync, got: %v", err)
	}
	if err := rw.Rotate(); !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("expected ErrWriterClosed from Rotate, got: %v", err)
	}
	if rw.ActiveSegmentID() != 0 {
		t.Errorf("expected ActiveSegmentID == 0 after close, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != 0 {
		t.Errorf("expected ActiveSegmentSize == 0 after close, got %d", rw.ActiveSegmentSize())
	}
	if rw.ActivePath() != "" {
		t.Errorf("expected empty ActivePath after close, got %s", rw.ActivePath())
	}

	// Idempotent Close()
	if err := rw.Close(); err != nil {
		t.Errorf("expected idempotent Close to return nil, got: %v", err)
	}
}

// 20. Repeated rotation attempts behave deterministically.
func TestRotation_RepeatedRotationAttemptsBehaveDeterministically(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	for i := 1; i <= 5; i++ {
		if err := rw.Rotate(); err != nil {
			t.Fatalf("Rotate iteration %d failed: %v", i, err)
		}
		expectedID := uint64(i + 1)
		if rw.ActiveSegmentID() != expectedID {
			t.Errorf("expected active segment ID %d, got %d", expectedID, rw.ActiveSegmentID())
		}
	}
}

// 21. Concurrent append/rotation stress under -race.
func TestRotation_ConcurrentAppendRotationStressRace(t *testing.T) {
	dir := t.TempDir()
	// Segment size = 200 bytes (forces very frequent rotations)
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 200})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	const numGoroutines = 10
	const recordsPerGoroutine = 25
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for r := 0; r < recordsPerGoroutine; r++ {
				seq := uint64(gid*1000 + r + 1)
				rec := makeRotRecord(seq, fmt.Sprintf("k-%d-%d", gid, r), "v")
				if err := rw.AppendSync(rec); err != nil {
					t.Errorf("goroutine %d append %d failed: %v", gid, r, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()

	// Verify all segments can be read back cleanly
	segs, err := rw.Segments()
	if err != nil {
		t.Fatalf("Segments failed: %v", err)
	}

	totalRecordsRead := 0
	for _, id := range segs {
		reader, err := wal.OpenSegmentReader(dir, id)
		if err != nil {
			t.Fatalf("OpenSegmentReader %d failed: %v", id, err)
		}
		for {
			_, err := reader.Next()
			if err != nil {
				if stdErrors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("segment %d read error: %v", id, err)
			}
			totalRecordsRead++
		}
		_ = reader.Close()
	}

	expectedTotal := numGoroutines * recordsPerGoroutine
	if totalRecordsRead != expectedTotal {
		t.Fatalf("expected %d total records across segments, got %d", expectedTotal, totalRecordsRead)
	}
}

// 22. Two concurrent callers cannot both create the same next segment.
func TestRotation_ConcurrentCallersCannotBothCreateSameNextSegment(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	const numCallers = 10
	var wg sync.WaitGroup
	wg.Add(numCallers)

	for i := 0; i < numCallers; i++ {
		go func() {
			defer wg.Done()
			_ = rw.Rotate()
		}()
	}

	wg.Wait()

	segs, err := rw.Segments()
	if err != nil {
		t.Fatalf("Segments failed: %v", err)
	}

	// Verify IDs are strictly unique and contiguous
	for i := 0; i < len(segs); i++ {
		expected := uint64(i + 1)
		if segs[i] != expected {
			t.Fatalf("expected segment ID %d at position %d, got %d (segs: %v)", expected, i, segs[i], segs)
		}
	}
}

// 23. Old segment receives no writes after rotation.
func TestRotation_OldSegmentReceivesNoWritesAfterRotation(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	_ = rw.AppendSync(makeRotRecord(1, "k1", "v1"))
	_ = rw.Rotate()

	path1 := wal.SegmentPath(dir, 1)
	infoBefore, _ := os.Stat(path1)

	// Append 10 records to segment 2
	for i := uint64(2); i <= 11; i++ {
		_ = rw.AppendSync(makeRotRecord(i, fmt.Sprintf("k%d", i), "v"))
	}

	infoAfter, _ := os.Stat(path1)
	if infoAfter.Size() != infoBefore.Size() {
		t.Errorf("segment 1 size changed from %d to %d after rotation!", infoBefore.Size(), infoAfter.Size())
	}
}

// 24. File permissions remain correct for every created segment.
func TestRotation_FilePermissionsCorrect(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	for i := 0; i < 5; i++ {
		_ = rw.Rotate()
	}

	segs, _ := rw.Segments()
	for _, id := range segs {
		info, err := os.Stat(wal.SegmentPath(dir, id))
		if err != nil {
			t.Fatalf("Stat segment %d failed: %v", id, err)
		}
		if info.Mode().Perm() != wal.FileMode {
			t.Errorf("segment %d has mode %v, expected %v", id, info.Mode().Perm(), wal.FileMode)
		}
	}
}

// 25. Symlink target rejection for next-segment creation.
func TestRotation_SymlinkTargetRejection(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Create target file and symlink pointing to it at segment 2 path
	targetFile := filepath.Join(dir, "target.txt")
	targetContent := []byte("DO NOT OVERWRITE VIA SYMLINK")
	if err := os.WriteFile(targetFile, targetContent, 0600); err != nil {
		t.Fatalf("WriteFile target failed: %v", err)
	}

	seg2Path := wal.SegmentPath(dir, 2)
	if err := os.Symlink(targetFile, seg2Path); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	// Rotation to 2 must be rejected
	err = rw.Rotate()
	if err == nil {
		t.Fatalf("expected rotation to fail over symlink, got nil")
	}

	// Verify target file was untouched
	content, _ := os.ReadFile(targetFile)
	if !bytes.Equal(content, targetContent) {
		t.Fatalf("target file was overwritten through symlink!")
	}
}

// 26. Existing unrelated file is never truncated.
func TestRotation_ExistingUnrelatedFileNeverTruncated(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	seg2Path := wal.SegmentPath(dir, 2)
	sensitiveData := []byte("CONFIDENTIAL UNRELATED CONTENT")
	if err := os.WriteFile(seg2Path, sensitiveData, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = rw.Rotate()
	if err == nil {
		t.Fatalf("expected Rotate to fail on existing file, got nil")
	}

	content, _ := os.ReadFile(seg2Path)
	if !bytes.Equal(content, sensitiveData) {
		t.Fatalf("existing file was truncated or modified!")
	}
}

// 27. Failure after old-segment close leaves deterministic state.
func TestRotation_FailureAfterOldSegmentCloseLeavesDeterministicState(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	_ = rw.AppendSync(makeRotRecord(1, "k1", "v1"))

	injectedErr := stdErrors.New("disk full during segment creation")
	rw.SetCreateWriterFnForTesting(func(path string) (*wal.WALWriter, error) {
		return nil, injectedErr
	})

	// Rotate fails during creation of segment 2
	err = rw.Rotate()
	if !stdErrors.Is(err, injectedErr) {
		t.Fatalf("expected injected error, got: %v", err)
	}

	// Active segment is marked inactive
	if rw.ActiveSegmentID() != 0 {
		t.Errorf("expected ActiveSegmentID == 0, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveWriter() != nil {
		t.Errorf("expected ActiveWriter == nil, got %v", rw.ActiveWriter())
	}

	// AppendSync fails deterministically
	err = rw.AppendSync(makeRotRecord(2, "k2", "v2"))
	if err == nil {
		t.Fatalf("expected AppendSync to fail when active segment is nil, got nil")
	}

	// Recovery: restore normal creation function and retry Rotate
	rw.SetCreateWriterFnForTesting(wal.CreateWriter)
	if err := rw.Rotate(); err != nil {
		t.Fatalf("retry Rotate failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Errorf("expected recovered active segment ID 2, got %d", rw.ActiveSegmentID())
	}

	// Appends succeed to segment 2 now
	if err := rw.AppendSync(makeRotRecord(2, "k2", "v2")); err != nil {
		t.Fatalf("AppendSync after recovery failed: %v", err)
	}
}

// 28. Large number of sequential segment rotations.
func TestRotation_LargeNumberOfSequentialRotations(t *testing.T) {
	dir := t.TempDir()
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	const totalRotations = 50
	for i := 1; i <= totalRotations; i++ {
		rec := makeRotRecord(uint64(i), fmt.Sprintf("k%d", i), "v")
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync %d failed: %v", i, err)
		}
		if err := rw.Rotate(); err != nil {
			t.Fatalf("Rotate %d failed: %v", i, err)
		}
	}

	segs, err := rw.Segments()
	if err != nil {
		t.Fatalf("Segments failed: %v", err)
	}
	if len(segs) != totalRotations+1 {
		t.Fatalf("expected %d segments, got %d", totalRotations+1, len(segs))
	}

	for i := 0; i < len(segs); i++ {
		expected := uint64(i + 1)
		if segs[i] != expected {
			t.Errorf("expected segment %d at index %d, got %d", expected, i, segs[i])
		}
	}
}

// 29. ParseSegmentID tests.
func TestRotation_ParseSegmentID(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantID  uint64
		wantErr bool
	}{
		{"valid 1", "wal_000000000001.log", 1, false},
		{"valid 42", "wal_000000000042.log", 42, false},
		{"valid large", "wal_000012345678.log", 12345678, false},
		{"valid max", "wal_999999999999.log", 999999999999, false},
		{"zero ID rejected", "wal_000000000000.log", 0, true},
		{"short name", "wal_0001.log", 0, true},
		{"long name", "wal_0000000000001.log", 0, true},
		{"bad prefix", "log_000000000001.log", 0, true},
		{"bad suffix", "wal_000000000001.txt", 0, true},
		{"non digit", "wal_00000000001a.log", 0, true},
		{"empty", "", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := wal.ParseSegmentID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got ID %d", tc.input, id)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error for %q: %v", tc.input, err)
				}
				if id != tc.wantID {
					t.Errorf("expected ID %d, got %d", tc.wantID, id)
				}
			}
		})
	}
}

// 30. ListSegments numeric ordering (not lexicographic).
func TestRotation_ListSegments_NumericOrdering(t *testing.T) {
	dir := t.TempDir()
	walDir, _ := wal.InitDir(dir)

	// Create files out of order: 9, 10, 2, 1
	// Lexicographically: 1, 10, 2, 9. Numerically: 1, 2, 9, 10.
	files := []string{
		"wal_000000000009.log",
		"wal_000000000010.log",
		"wal_000000000002.log",
		"wal_000000000001.log",
		"other_file.tmp",
		"README.md",
	}
	for _, f := range files {
		_ = os.WriteFile(filepath.Join(walDir, f), []byte("test"), 0600)
	}

	ids, err := wal.ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments failed: %v", err)
	}

	expected := []uint64{1, 2, 9, 10}
	if len(ids) != len(expected) {
		t.Fatalf("expected %d IDs, got %d: %v", len(expected), len(ids), ids)
	}
	for i := range expected {
		if ids[i] != expected[i] {
			t.Errorf("position %d: expected %d, got %d", i, expected[i], ids[i])
		}
	}
}

// 31. Resume existing segments.
func TestRotation_ResumeExistingSegments(t *testing.T) {
	dir := t.TempDir()
	// Create segment 1 and 2 with writer 1
	rw1, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("rw1 open failed: %v", err)
	}
	_ = rw1.AppendSync(makeRotRecord(1, "k1", "v1"))
	_ = rw1.Rotate()
	rec2 := makeRotRecord(2, "k2", "v2")
	_ = rw1.AppendSync(rec2)
	rec2Size := wal.RecordWireSize(rec2)
	_ = rw1.Close()

	// Reopen with rw2: should resume segment 2
	rw2, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 1000})
	if err != nil {
		t.Fatalf("rw2 open failed: %v", err)
	}
	defer func() { _ = rw2.Close() }()

	if rw2.ActiveSegmentID() != 2 {
		t.Errorf("expected rw2 to resume active segment 2, got %d", rw2.ActiveSegmentID())
	}
	if rw2.ActiveSegmentSize() != rec2Size {
		t.Errorf("expected rw2 to track active size %d, got %d", rec2Size, rw2.ActiveSegmentSize())
	}

	// Appending to resumed writer stays in segment 2
	rec3 := makeRotRecord(3, "k3", "v3")
	if err := rw2.AppendSync(rec3); err != nil {
		t.Fatalf("AppendSync rec3 failed: %v", err)
	}
	if rw2.ActiveSegmentID() != 2 {
		t.Errorf("expected still segment 2, got %d", rw2.ActiveSegmentID())
	}
}

// 32. RecordWireSize helper accuracy.
func TestRotation_RecordWireSize(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(100),
		Timestamp: 200,
		Key:       []byte("hello"),
		Value:     []byte("world!"),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	computed := wal.RecordWireSize(rec)
	if computed != int64(len(encoded)) {
		t.Errorf("RecordWireSize mismatch: computed %d, encoded %d", computed, len(encoded))
	}
}

// 33. Byte-level verification: continuous multi-segment logical sequence.
// Verifies:
//   - Exact record ordering across multiple segment files
//   - Exact record contents (SeqNum, Key, Value, Type)
//   - No duplication, no loss, no partial record
//   - All segments terminate at clean io.EOF
func TestRotation_ByteLevelContinuousSequence(t *testing.T) {
	dir := t.TempDir()
	// Segment size = 150 bytes (~3-4 records per segment)
	rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 150})
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	const totalRecords = 30
	var writtenRecords []wal.Record

	for i := uint64(1); i <= totalRecords; i++ {
		rec := makeRotRecord(i, fmt.Sprintf("metric-key-%04d", i), fmt.Sprintf("metric-val-%06d", i*10))
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync %d failed: %v", i, err)
		}
		writtenRecords = append(writtenRecords, rec)
	}

	// Close writer to ensure all buffers synced
	if err := rw.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Discover all segments
	segIDs, err := wal.ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments failed: %v", err)
	}
	if len(segIDs) < 3 {
		t.Fatalf("expected at least 3 segments, got %d: %v", len(segIDs), segIDs)
	}

	// Stream and decode all records across segments in sequential order
	var readRecords []wal.Record
	for _, segID := range segIDs {
		reader, err := wal.OpenSegmentReader(dir, segID)
		if err != nil {
			t.Fatalf("OpenSegmentReader for segment %d failed: %v", segID, err)
		}

		segRecordCount := 0
		for {
			rec, err := reader.Next()
			if err != nil {
				if stdErrors.Is(err, io.EOF) {
					// Clean termination at EOF
					break
				}
				t.Fatalf("segment %d read error: %v", segID, err)
			}
			readRecords = append(readRecords, rec)
			segRecordCount++
		}
		_ = reader.Close()

		if segRecordCount == 0 {
			t.Errorf("segment %d was unexpectedly empty", segID)
		}
	}

	// Verify exact count match
	if len(readRecords) != len(writtenRecords) {
		t.Fatalf("record count mismatch: wrote %d, read %d across %d segments",
			len(writtenRecords), len(readRecords), len(segIDs))
	}

	// Verify field-by-field equality and continuous ordering
	for i := 0; i < len(writtenRecords); i++ {
		want := writtenRecords[i]
		got := readRecords[i]

		if got.SeqNum != want.SeqNum {
			t.Errorf("record %d SeqNum mismatch: want %d, got %d", i, want.SeqNum, got.SeqNum)
		}
		if got.Type != want.Type {
			t.Errorf("record %d Type mismatch: want %v, got %v", i, want.Type, got.Type)
		}
		if !bytes.Equal(got.Key, want.Key) {
			t.Errorf("record %d Key mismatch: want %s, got %s", i, string(want.Key), string(got.Key))
		}
		if !bytes.Equal(got.Value, want.Value) {
			t.Errorf("record %d Value mismatch: want %s, got %s", i, string(want.Value), string(got.Value))
		}
	}
}

// 34. Boundary & size accounting: exact fit, one byte under, one byte over.
func TestRotation_ExactByteFitMatrix(t *testing.T) {
	// MinRecordSize = 27
	// rec1: key="k1" (2B), val="v1" (2B) -> wire size: 27 + 2 + 2 = 31 B

	t.Run("exact fit S+R == M", func(t *testing.T) {
		dir := t.TempDir()
		// Threshold M = 62 bytes (exactly 2 records of 31 bytes)
		rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 62})
		if err != nil {
			t.Fatalf("OpenRotatingWriter failed: %v", err)
		}
		defer func() { _ = rw.Close() }()

		r1 := makeRotRecord(1, "k1", "v1") // 31 B
		r2 := makeRotRecord(2, "k2", "v2") // 31 B
		_ = rw.AppendSync(r1)
		_ = rw.AppendSync(r2)

		// Both must be in segment 1 because 31 + 31 == 62 <= 62
		if rw.ActiveSegmentID() != 1 {
			t.Errorf("expected segment 1 for exact fit, got %d", rw.ActiveSegmentID())
		}
		if rw.ActiveSegmentSize() != 62 {
			t.Errorf("expected size 62, got %d", rw.ActiveSegmentSize())
		}
	})

	t.Run("one byte under S+R == M-1", func(t *testing.T) {
		dir := t.TempDir()
		// Threshold M = 63 bytes (2 records of 31 bytes leave 1 byte spare)
		rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 63})
		if err != nil {
			t.Fatalf("OpenRotatingWriter failed: %v", err)
		}
		defer func() { _ = rw.Close() }()

		r1 := makeRotRecord(1, "k1", "v1") // 31 B
		r2 := makeRotRecord(2, "k2", "v2") // 31 B
		_ = rw.AppendSync(r1)
		_ = rw.AppendSync(r2)

		// Both must be in segment 1 because 31 + 31 = 62 < 63
		if rw.ActiveSegmentID() != 1 {
			t.Errorf("expected segment 1 for one byte under, got %d", rw.ActiveSegmentID())
		}
		if rw.ActiveSegmentSize() != 62 {
			t.Errorf("expected size 62, got %d", rw.ActiveSegmentSize())
		}
	})

	t.Run("one byte over S+R == M+1", func(t *testing.T) {
		dir := t.TempDir()
		// Threshold M = 61 bytes (2 records of 31 bytes = 62, which is 61 + 1)
		rw, err := wal.OpenRotatingWriter(dir, wal.Options{SegmentSize: 61})
		if err != nil {
			t.Fatalf("OpenRotatingWriter failed: %v", err)
		}
		defer func() { _ = rw.Close() }()

		r1 := makeRotRecord(1, "k1", "v1") // 31 B
		r2 := makeRotRecord(2, "k2", "v2") // 31 B
		_ = rw.AppendSync(r1)
		// Active size is 31. Appending r2: 31 + 31 = 62 > 61 -> MUST ROTATE to segment 2
		_ = rw.AppendSync(r2)

		if rw.ActiveSegmentID() != 2 {
			t.Errorf("expected segment 2 for one byte over, got %d", rw.ActiveSegmentID())
		}
		if rw.ActiveSegmentSize() != 31 {
			t.Errorf("expected segment 2 size 31, got %d", rw.ActiveSegmentSize())
		}

		info1, _ := os.Stat(wal.SegmentPath(dir, 1))
		if info1.Size() != 31 {
			t.Errorf("expected segment 1 size 31, got %d", info1.Size())
		}
	})
}

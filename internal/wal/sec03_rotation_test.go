package wal_test

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

func TestSEC03_Rotation_01_ExactFitAndOneByteOverBoundaries(t *testing.T) {
	h := NewSecurityHarness(t)

	rec1 := h.MakeRecord(1, "k1", "v1")
	rec1WireSize := wal.RecordWireSize(rec1) // e.g. 27 + 2 + 2 = 31 bytes

	// 1. Set SegmentSize to exact size of 1 record
	opts := wal.Options{
		SegmentSize: rec1WireSize,
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}

	// First record fits exactly (activeLen becomes rec1WireSize == SegmentSize)
	if err := rw.AppendSync(rec1); err != nil {
		t.Fatalf("AppendSync rec1 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 1 {
		t.Errorf("expected active segment 1, got %d", rw.ActiveSegmentID())
	}
	if rw.ActiveSegmentSize() != rec1WireSize {
		t.Errorf("expected active size %d, got %d", rec1WireSize, rw.ActiveSegmentSize())
	}

	// 2. Next record would exceed SegmentSize by at least 1 byte -> must trigger rotation to segment 2
	rec2 := h.MakeRecord(2, "k2", "v2")
	if err := rw.AppendSync(rec2); err != nil {
		t.Fatalf("AppendSync rec2 failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Errorf("expected rotation to segment 2, got %d", rw.ActiveSegmentID())
	}

	_ = rw.Close()

	// Verify both segments exist and are clean
	ids, err := wal.ListSegments(h.RootDir())
	if err != nil || len(ids) != 2 {
		t.Fatalf("expected segments [1, 2], got %v", ids)
	}
}

func TestSEC03_Rotation_02_OversizedIndividualRecordAccepted(t *testing.T) {
	h := NewSecurityHarness(t)

	// Configure tiny SegmentSize (50 bytes)
	opts := wal.Options{
		SegmentSize: 50,
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// An oversized record of 200 bytes when activeLen == 0 must be accepted into the empty segment
	// rather than entering an infinite rotation loop (anti-deadlock policy).
	oversizedRec := h.MakeRecord(1, "key-50-bytes-padding-to-exceed-the-segment-boundary", "val-150-bytes-large-payload-exceeding-threshold-completely")
	if err := rw.AppendSync(oversizedRec); err != nil {
		t.Fatalf("AppendSync oversized record to empty segment failed: %v", err)
	}

	if rw.ActiveSegmentID() != 1 {
		t.Errorf("oversized record should be accepted in segment 1, got %d", rw.ActiveSegmentID())
	}

	// The very next record must trigger rotation because segment 1 is now over-limit
	nextRec := h.MakeRecord(2, "k2", "v2")
	if err := rw.AppendSync(nextRec); err != nil {
		t.Fatalf("AppendSync nextRec failed: %v", err)
	}
	if rw.ActiveSegmentID() != 2 {
		t.Errorf("expected rotation to segment 2, got %d", rw.ActiveSegmentID())
	}
}

func TestSEC03_Rotation_03_SegmentIDExhaustionPreventsWraparound(t *testing.T) {
	h := NewSecurityHarness(t)

	opts := wal.Options{
		InitialSegmentID: math.MaxUint64,
		SegmentSize:      100,
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter at MaxUint64 failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Calling Rotate explicitly at MaxUint64 must fail with SegmentIDOverflowError
	err = rw.Rotate()
	if err == nil {
		t.Fatalf("expected Rotate() at MaxUint64 to fail with SegmentIDOverflowError")
	}
	var overflowErr *latticeErrors.SegmentIDOverflowError
	if !errors.As(err, &overflowErr) {
		t.Errorf("expected SegmentIDOverflowError, got: %T (%v)", err, err)
	}
}

func TestSEC03_Rotation_04_SegmentNamingCollisionFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	opts := wal.Options{
		SegmentSize: 50,
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Write record 1 into segment 1
	_ = rw.AppendSync(h.MakeRecord(1, "k1", "v1"))

	// Pre-create segment 2 with unauthorized collision data (simulating malicious pre-creation)
	seg2Path := wal.SegmentPath(h.RootDir(), 2)
	unauthorizedData := []byte("PRE_EXISTING_UNAUTHORIZED_SEGMENT_DATA")
	if err := os.WriteFile(seg2Path, unauthorizedData, 0600); err != nil {
		t.Fatalf("failed to create collision file: %v", err)
	}

	// Appending record 2 triggers rotation to segment 2.
	// Because segment 2 already exists, CreateWriter (O_EXCL) must reject it with os.ErrExist.
	err = rw.AppendSync(h.MakeRecord(2, "k2", "v2"))
	if err == nil {
		t.Fatalf("expected AppendSync to fail when segment 2 collision occurs")
	}

	// Verify the collision target was NOT overwritten or truncated
	data, _ := os.ReadFile(seg2Path)
	if string(data) != string(unauthorizedData) {
		t.Fatalf("unauthorized pre-existing segment was overwritten: %s", string(data))
	}
}

func TestSEC03_Rotation_05_PreexistingSymlinkCollision(t *testing.T) {
	h := NewSecurityHarness(t)
	h.RequireSymlinks()

	opts := wal.Options{
		SegmentSize: 50,
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	_ = rw.AppendSync(h.MakeRecord(1, "k1", "v1"))

	// Create a symlink at the path of the next segment (segment 2) pointing to a target file
	victimPath := filepath.Join(h.RootDir(), "sensitive_victim_file.txt")
	_ = os.WriteFile(victimPath, []byte("SENSITIVE_SYS_CONFIG"), 0600)

	seg2Path := wal.SegmentPath(h.RootDir(), 2)
	if err := h.CreateSymlink(victimPath, seg2Path); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// Rotation to segment 2 must fail closed
	err = rw.AppendSync(h.MakeRecord(2, "k2", "v2"))
	if err == nil {
		t.Fatalf("expected AppendSync to fail on symlink collision")
	}

	// Verify victim file untouched
	victimData, _ := os.ReadFile(victimPath)
	if string(victimData) != "SENSITIVE_SYS_CONFIG" {
		t.Fatalf("victim file was mutated via symlink collision: %s", string(victimData))
	}
}

func TestSEC03_Rotation_06_ConcurrentAppendsDuringRotation(t *testing.T) {
	h := NewSecurityHarness(t)

	opts := wal.Options{
		SegmentSize: 100, // rapid rotations
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}

	const goroutines = 8
	const recordsPerGoroutine = 15
	var wg sync.WaitGroup

	errChan := make(chan error, goroutines*recordsPerGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < recordsPerGoroutine; i++ {
				seq := uint64(gid*1000 + i + 1)
				rec := h.MakeRecord(seq, "key-concurrent", "val-concurrent-payload")
				if appendErr := rw.AppendSync(rec); appendErr != nil {
					errChan <- appendErr
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("concurrent append error: %v", err)
	}

	_ = rw.Close()

	// Verify all created segments are valid and can be listed
	ids, err := wal.ListSegments(h.RootDir())
	if err != nil {
		t.Fatalf("ListSegments failed after concurrent rotations: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("expected segments created")
	}
}

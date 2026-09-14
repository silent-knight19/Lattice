package version

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TestSEC_P07_008_SSTablePhysicalSizeValidation verifies that manifest replay validates physical
// SSTable file size against FileMetadata.FileSize and fails closed on mismatch (P07-SEC-008).
func TestSEC_P07_008_SSTablePhysicalSizeValidation(t *testing.T) {
	makeKey := func(k string, seq uint64) []byte {
		ik, _ := binary.NewInternalKey([]byte(k), binary.SeqNum(seq), binary.OpTypePut)
		return binary.EncodeInternalKey(ik)
	}

	t.Run("Actual equals expected: success", func(t *testing.T) {
		dir := t.TempDir()
		e := NewVersionEdit()
		_ = e.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       2048,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("z", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		createDummySSTable(t, dir, 1, 2048)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success when physical size matches metadata, got %v", err)
		}
		defer res.Version.Unref()

		if res.ValidRecords != 1 {
			t.Errorf("expected 1 valid record, got %d", res.ValidRecords)
		}
	})

	t.Run("Actual smaller than expected: truncated sstable fails closed", func(t *testing.T) {
		dir := t.TempDir()
		e := NewVersionEdit()
		_ = e.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       2048,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("z", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		// Write only 100 bytes instead of 2048 (truncated write or disk corruption)
		createDummySSTable(t, dir, 1, 100)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err == nil {
			defer res.Version.Unref()
			t.Fatalf("SECURITY VIOLATION: accepted truncated sstable (expected 2048, got 100)")
		}

		if !stdErrors.Is(err, errors.ErrSSTableSizeMismatch) {
			t.Errorf("expected ErrSSTableSizeMismatch, got %v", err)
		}

		var sizeErr *errors.SSTableSizeMismatchError
		if !stdErrors.As(err, &sizeErr) {
			t.Fatalf("expected *errors.SSTableSizeMismatchError, got %T (%v)", err, err)
		}
		if sizeErr.Expected != 2048 || sizeErr.Actual != 100 {
			t.Errorf("size mismatch details: expected (2048, 100), got (%d, %d)", sizeErr.Expected, sizeErr.Actual)
		}
	})

	t.Run("Actual larger than expected: oversized sstable fails closed", func(t *testing.T) {
		dir := t.TempDir()
		e := NewVersionEdit()
		_ = e.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("z", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		// Write 5000 bytes instead of 1024
		createDummySSTable(t, dir, 1, 5000)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err == nil {
			defer res.Version.Unref()
			t.Fatalf("SECURITY VIOLATION: accepted oversized sstable (expected 1024, got 5000)")
		}

		if !stdErrors.Is(err, errors.ErrSSTableSizeMismatch) {
			t.Errorf("expected ErrSSTableSizeMismatch, got %v", err)
		}

		var sizeErr *errors.SSTableSizeMismatchError
		if !stdErrors.As(err, &sizeErr) {
			t.Fatalf("expected *errors.SSTableSizeMismatchError, got %T (%v)", err, err)
		}
		if sizeErr.Expected != 1024 || sizeErr.Actual != 5000 {
			t.Errorf("size mismatch details: expected (1024, 5000), got (%d, %d)", sizeErr.Expected, sizeErr.Actual)
		}
	})

	t.Run("Large valid FileSize: succeeds", func(t *testing.T) {
		dir := t.TempDir()
		const largeSize = 64 * 1024 * 1024 // 64 MiB
		e := NewVersionEdit()
		_ = e.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       largeSize,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("z", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		createDummySSTable(t, dir, 1, largeSize)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success with 64 MiB file size, got %v", err)
		}
		defer res.Version.Unref()
	})
}

// TestSEC_P07_011_ManifestScalarRegressionFailsClosed verifies that manifest scalar regression
// (NextFileNum or LastSeqNum decreasing) fails closed immediately with ErrCorruptedVersionEdit (P07-SEC-011).
func TestSEC_P07_011_ManifestScalarRegressionFailsClosed(t *testing.T) {
	makeKey := func(k string, seq uint64) []byte {
		ik, _ := binary.NewInternalKey([]byte(k), binary.SeqNum(seq), binary.OpTypePut)
		return binary.EncodeInternalKey(ik)
	}

	t.Run("NextFileNum regression: 100 -> 50 fails closed", func(t *testing.T) {
		dir := t.TempDir()
		e1 := NewVersionEdit()
		e1.SetNextFileNum(100)
		e1.SetLastSeqNum(10)
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		createDummySSTable(t, dir, 1, 1024)

		// Regressing edit: NextFileNum drops to 50
		e2 := NewVersionEdit()
		e2.SetNextFileNum(50)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: accepted regressing NextFileNum (100 -> 50)")
		}
		if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
			t.Errorf("expected ErrCorruptedVersionEdit, got %v", err)
		}
	})

	t.Run("NextFileNum monotonicity progression: 0 -> 1 -> 1 -> 2", func(t *testing.T) {
		dir := t.TempDir()
		createDummySSTable(t, dir, 1, 1024)

		e1 := NewVersionEdit()
		e1.SetNextFileNum(1)
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})

		// Equal value: 1 -> 1 allowed
		e2 := NewVersionEdit()
		e2.SetNextFileNum(1)

		// Increasing: 1 -> 2 allowed
		e3 := NewVersionEdit()
		e3.SetNextFileNum(2)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2, e3})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success for monotonic progression, got %v", err)
		}
		defer res.Version.Unref()

		if res.NextFileNum != 2 {
			t.Errorf("expected NextFileNum=2, got %d", res.NextFileNum)
		}
	})

	t.Run("LastSeqNum regression: 11 -> 9 fails closed", func(t *testing.T) {
		dir := t.TempDir()
		createDummySSTable(t, dir, 1, 1024)

		e1 := NewVersionEdit()
		e1.SetLastSeqNum(11)
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})

		// Regressing edit: LastSeqNum drops to 9
		e2 := NewVersionEdit()
		e2.SetLastSeqNum(9)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatalf("SECURITY VIOLATION: accepted regressing LastSeqNum (11 -> 9)")
		}
		if !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
			t.Errorf("expected ErrCorruptedVersionEdit, got %v", err)
		}
	})

	t.Run("LastSeqNum monotonicity progression: 0 -> 10 -> 10 -> 11", func(t *testing.T) {
		dir := t.TempDir()
		createDummySSTable(t, dir, 1, 1024)

		e1 := NewVersionEdit()
		e1.SetLastSeqNum(10)
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})

		// Equal value: 10 -> 10 allowed
		e2 := NewVersionEdit()
		e2.SetLastSeqNum(10)

		// Increasing: 10 -> 11 allowed
		e3 := NewVersionEdit()
		e3.SetLastSeqNum(11)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2, e3})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success for monotonic LastSeqNum progression, got %v", err)
		}
		defer res.Version.Unref()

		if res.LastSeqNum != 11 {
			t.Errorf("expected LastSeqNum=11, got %d", res.LastSeqNum)
		}
	})

	t.Run("Edits without scalars: succeed without corruption", func(t *testing.T) {
		dir := t.TempDir()
		createDummySSTable(t, dir, 1, 1024)

		e1 := NewVersionEdit()
		e1.SetNextFileNum(5)
		e1.SetLastSeqNum(50)
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})

		// Edit without scalars (only file addition)
		createDummySSTable(t, dir, 2, 1024)
		e2 := NewVersionEdit()
		_ = e2.AddFile(0, FileMetadata{
			FileNum:        2,
			FileSize:       1024,
			SmallestKey:    makeKey("c", 3),
			LargestKey:     makeKey("d", 4),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		})

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success when scalars omitted, got %v", err)
		}
		defer res.Version.Unref()

		if res.NextFileNum != 5 || res.LastSeqNum != 50 {
			t.Errorf("scalars unexpectedly altered: nextFileNum=%d, lastSeqNum=%d", res.NextFileNum, res.LastSeqNum)
		}
	})
}

// TestSEC_P07_012_ReplayErrorProvenanceAccuracy verifies that post-scan errors
// (missing SSTable, overlapping key ranges, size mismatch) report the exact source edit
// RecordIndex and Offset rather than EOF (P07-SEC-012).
func TestSEC_P07_012_ReplayErrorProvenanceAccuracy(t *testing.T) {
	makeKey := func(k string, seq uint64) []byte {
		ik, _ := binary.NewInternalKey([]byte(k), binary.SeqNum(seq), binary.OpTypePut)
		return binary.EncodeInternalKey(ik)
	}

	t.Run("Missing SSTable reports originating edit index and offset", func(t *testing.T) {
		dir := t.TempDir()

		// Edit 0: Adds File 1 (physical file exists)
		e0 := NewVersionEdit()
		_ = e0.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		createDummySSTable(t, dir, 1, 1024)

		// Edit 1: Adds File 2 (physical file is MISSING!)
		e1 := NewVersionEdit()
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        2,
			FileSize:       1024,
			SmallestKey:    makeKey("c", 3),
			LargestKey:     makeKey("d", 4),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		})

		// Edit 2: Scalar update
		e2 := NewVersionEdit()
		e2.SetNextFileNum(10)
		e2.SetLastSeqNum(100)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e0, e1, e2})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatalf("expected failure on missing sstable 2")
		}

		var replayErr *ReplayError
		if !stdErrors.As(err, &replayErr) {
			t.Fatalf("expected *ReplayError, got %T (%v)", err, err)
		}

		// Invariant: RecordIndex MUST point to Edit 1 (which added File 2), NOT Edit 2 or total records (3)
		if replayErr.RecordIndex != 1 {
			t.Errorf("diagnostic inaccurate: expected RecordIndex=1 (origin of File 2), got %d", replayErr.RecordIndex)
		}
		if replayErr.Offset <= 0 {
			t.Errorf("diagnostic inaccurate: expected positive byte offset for edit 1, got %d", replayErr.Offset)
		}
		if !stdErrors.Is(replayErr.Err, errors.ErrMissingSSTable) {
			t.Errorf("expected ErrMissingSSTable, got %v", replayErr.Err)
		}
	})

	t.Run("Physical size mismatch reports originating edit index and offset", func(t *testing.T) {
		dir := t.TempDir()

		// Edit 0: Adds File 1 (size matches 1024)
		e0 := NewVersionEdit()
		_ = e0.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("b", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		createDummySSTable(t, dir, 1, 1024)

		// Edit 1: Adds File 2 (expected 2048, but file on disk is only 500)
		e1 := NewVersionEdit()
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        2,
			FileSize:       2048,
			SmallestKey:    makeKey("c", 3),
			LargestKey:     makeKey("d", 4),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		})
		createDummySSTable(t, dir, 2, 500)

		// Edit 2: Scalar update
		e2 := NewVersionEdit()
		e2.SetNextFileNum(10)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e0, e1, e2})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatalf("expected failure on sstable size mismatch")
		}

		var replayErr *ReplayError
		if !stdErrors.As(err, &replayErr) {
			t.Fatalf("expected *ReplayError, got %T (%v)", err, err)
		}

		// Must point to Edit 1
		if replayErr.RecordIndex != 1 {
			t.Errorf("expected RecordIndex=1, got %d", replayErr.RecordIndex)
		}
		if replayErr.Offset <= 0 {
			t.Errorf("expected positive Offset, got %d", replayErr.Offset)
		}
		if !stdErrors.Is(replayErr.Err, errors.ErrSSTableSizeMismatch) {
			t.Errorf("expected ErrSSTableSizeMismatch, got %v", replayErr.Err)
		}
	})

	t.Run("Overlapping key ranges in L1 reports responsible edit index", func(t *testing.T) {
		dir := t.TempDir()

		// Edit 0: Adds File 1 at L1 with keys ["a".."m"]
		e0 := NewVersionEdit()
		_ = e0.AddFile(1, FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    makeKey("a", 1),
			LargestKey:     makeKey("m", 2),
			SmallestSeqNum: 1,
			LargestSeqNum:  2,
		})
		createDummySSTable(t, dir, 1, 1024)

		// Edit 1: Adds File 2 at L1 with overlapping keys ["g".."z"] (overlaps with File 1!)
		e1 := NewVersionEdit()
		_ = e1.AddFile(1, FileMetadata{
			FileNum:        2,
			FileSize:       1024,
			SmallestKey:    makeKey("g", 3),
			LargestKey:     makeKey("z", 4),
			SmallestSeqNum: 3,
			LargestSeqNum:  4,
		})
		createDummySSTable(t, dir, 2, 1024)

		// Edit 2: Scalar update
		e2 := NewVersionEdit()
		e2.SetNextFileNum(10)

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{e0, e1, e2})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatalf("expected failure on overlapping L1 ranges")
		}

		var replayErr *ReplayError
		if !stdErrors.As(err, &replayErr) {
			t.Fatalf("expected *ReplayError, got %T (%v)", err, err)
		}

		// Must point to Edit 1 (the edit that introduced the overlap)
		if replayErr.RecordIndex != 1 {
			t.Errorf("expected RecordIndex=1 for overlapping file, got %d", replayErr.RecordIndex)
		}
		if replayErr.Offset <= 0 {
			t.Errorf("expected positive Offset, got %d", replayErr.Offset)
		}
		if !stdErrors.Is(replayErr.Err, errors.ErrInvalidKeyRange) {
			t.Errorf("expected ErrInvalidKeyRange, got %v", replayErr.Err)
		}
	})
}

// TestParseTableFilename validates canonical SSTable filename parsing.
func TestParseTableFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected uint64
		valid    bool
	}{
		{"canonical 1", "000001.sst", 1, true},
		{"canonical 42", "000042.sst", 42, true},
		{"canonical 999999", "999999.sst", 999999, true},
		{"canonical 1000000", "1000000.sst", 1000000, true},
		{"non-canonical short", "1.sst", 0, false},
		{"non-canonical zero", "000000.sst", 0, false},
		{"non-canonical prefix", ".tmp_000001.sst_123", 0, false},
		{"non-canonical suffix", "000001.sst.tmp", 0, false},
		{"non-numeric", "abcdef.sst", 0, false},
		{"no extension", "000001", 0, false},
		{"wrong extension", "000001.txt", 0, false},
		{"empty string", "", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			num, ok := ParseTableFilename(tc.input)
			if ok != tc.valid {
				t.Fatalf("ParseTableFilename(%q): expected valid=%v, got %v", tc.input, tc.valid, ok)
			}
			if ok && num != tc.expected {
				t.Errorf("ParseTableFilename(%q): expected %d, got %d", tc.input, tc.expected, num)
			}
		})
	}
}

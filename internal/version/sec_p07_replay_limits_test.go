package version

import (
	stdErrors "errors"
	"fmt"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// makeDummyEdit creates a single VersionEdit adding one SSTable to level 0.
func makeDummyEdit(fileNum uint64) *VersionEdit {
	edit := NewVersionEdit()
	edit.SetNextFileNum(fileNum + 1)
	edit.SetLastSeqNum(binary.SeqNum(fileNum * 10))
	_ = edit.AddFile(0, FileMetadata{
		FileNum:        fileNum,
		FileSize:       1024,
		SmallestKey:    makeTestIK(fmt.Sprintf("user:%04d", fileNum), fileNum*10, binary.OpTypePut),
		LargestKey:     makeTestIK(fmt.Sprintf("user:%04d", fileNum), fileNum*10, binary.OpTypePut),
		SmallestSeqNum: fileNum * 10,
		LargestSeqNum:  fileNum * 10,
	})
	return edit
}

// TestSEC_P07_05_RecordCountLimit verifies that MANIFEST replay strictly enforces the
// record count budget before processing, failing closed with ErrManifestReplayLimit.
func TestSEC_P07_05_RecordCountLimit(t *testing.T) {
	const recordLimit = 5
	restore := SetManifestReplayLimitsForTesting(MaxManifestReplayBytes, recordLimit, MaxManifestLiveFiles)
	defer restore()

	// 1. Exactly-at-limit (5 records): must succeed
	{
		dir := t.TempDir()
		var edits []*VersionEdit
		for i := 1; i <= recordLimit; i++ {
			fn := uint64(i)
			createDummySSTable(t, dir, fn)
			edits = append(edits, makeDummyEdit(fn))
		}

		disc := setupTestManifest(t, dir, 1, edits)
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success at exact record limit, got: %v", err)
		}
		if res.ValidRecords != recordLimit {
			t.Fatalf("expected %d valid records, got %d", recordLimit, res.ValidRecords)
		}
		res.Version.Unref()
	}

	// 2. One-over-limit (6 records): must fail closed on the 6th record
	{
		dir := t.TempDir()
		var edits []*VersionEdit
		for i := 1; i <= recordLimit+1; i++ {
			fn := uint64(i)
			createDummySSTable(t, dir, fn)
			edits = append(edits, makeDummyEdit(fn))
		}

		disc := setupTestManifest(t, dir, 1, edits)
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err == nil {
			t.Fatal("expected failure on record exceeding limit, got nil")
		}
		if !stdErrors.Is(err, errors.ErrManifestReplayLimit) {
			t.Fatalf("expected ErrManifestReplayLimit, got: %v", err)
		}
		var limitErr *errors.ManifestReplayLimitError
		if !stdErrors.As(err, &limitErr) {
			t.Fatalf("expected ManifestReplayLimitError, got: %v", err)
		}
		if limitErr.Resource != "records" {
			t.Fatalf("expected limitErr.Resource == 'records', got %q", limitErr.Resource)
		}
		if res != nil {
			t.Fatal("expected nil ReplayResult on limit failure (no partial state escape)")
		}
	}
}

// TestSEC_P07_05_LiveFilesLimit verifies that MANIFEST replay enforces the active
// live files budget, rejecting manifests that accumulate more active files than allowed.
func TestSEC_P07_05_LiveFilesLimit(t *testing.T) {
	const filesLimit = 3
	restore := SetManifestReplayLimitsForTesting(MaxManifestReplayBytes, MaxManifestReplayRecords, filesLimit)
	defer restore()

	// 1. Exactly-at-limit (3 files): must succeed
	{
		dir := t.TempDir()
		var edits []*VersionEdit
		for i := 1; i <= filesLimit; i++ {
			fn := uint64(i)
			createDummySSTable(t, dir, fn)
			edits = append(edits, makeDummyEdit(fn))
		}

		disc := setupTestManifest(t, dir, 1, edits)
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err != nil {
			t.Fatalf("expected success at exact live files limit, got: %v", err)
		}
		if res.Version.NumFiles(0) != filesLimit {
			t.Fatalf("expected %d live files, got %d", filesLimit, res.Version.NumFiles(0))
		}
		res.Version.Unref()
	}

	// 2. One-over-limit (4 files): must fail closed
	{
		dir := t.TempDir()
		var edits []*VersionEdit
		for i := 1; i <= filesLimit+1; i++ {
			fn := uint64(i)
			createDummySSTable(t, dir, fn)
			edits = append(edits, makeDummyEdit(fn))
		}

		disc := setupTestManifest(t, dir, 1, edits)
		defer func() { _ = disc.Close() }()

		res, err := ReplayManifest(disc)
		if err == nil {
			t.Fatal("expected failure on live files exceeding limit, got nil")
		}
		if !stdErrors.Is(err, errors.ErrManifestReplayLimit) {
			t.Fatalf("expected ErrManifestReplayLimit, got: %v", err)
		}
		var limitErr *errors.ManifestReplayLimitError
		if !stdErrors.As(err, &limitErr) {
			t.Fatalf("expected ManifestReplayLimitError, got: %v", err)
		}
		if limitErr.Resource != "live_files" {
			t.Fatalf("expected limitErr.Resource == 'live_files', got %q", limitErr.Resource)
		}
		if res != nil {
			t.Fatal("expected nil ReplayResult on limit failure")
		}
	}
}

// TestSEC_P07_05_AddDeleteChurnBudget verifies that delete operations safely decrease
// the live file counter, permitting realistic add/delete churn without falsely tripping
// the live files budget.
func TestSEC_P07_05_AddDeleteChurnBudget(t *testing.T) {
	const filesLimit = 3
	restore := SetManifestReplayLimitsForTesting(MaxManifestReplayBytes, MaxManifestReplayRecords, filesLimit)
	defer restore()

	dir := t.TempDir()

	// Edit 1: Add files 1, 2, 3 (live count = 3)
	e1 := NewVersionEdit()
	e1.SetNextFileNum(4)
	e1.SetLastSeqNum(100)
	for i := uint64(1); i <= 3; i++ {
		createDummySSTable(t, dir, i)
		_ = e1.AddFile(0, FileMetadata{
			FileNum:        i,
			FileSize:       1024,
			SmallestKey:    makeTestIK(fmt.Sprintf("k%d", i), 10, binary.OpTypePut),
			LargestKey:     makeTestIK(fmt.Sprintf("k%d", i), 10, binary.OpTypePut),
			SmallestSeqNum: 10,
			LargestSeqNum:  10,
		})
	}

	// Edit 2: Delete files 1, 2 (live count drops to 1)
	e2 := NewVersionEdit()
	e2.SetNextFileNum(4)
	e2.SetLastSeqNum(110)
	e2.DeleteFile(0, 1)
	e2.DeleteFile(0, 2)

	// Edit 3: Add files 4, 5 (live count increases to 3 <= limit 3)
	e3 := NewVersionEdit()
	e3.SetNextFileNum(6)
	e3.SetLastSeqNum(120)
	for i := uint64(4); i <= 5; i++ {
		createDummySSTable(t, dir, i)
		_ = e3.AddFile(0, FileMetadata{
			FileNum:        i,
			FileSize:       1024,
			SmallestKey:    makeTestIK(fmt.Sprintf("k%d", i), 20, binary.OpTypePut),
			LargestKey:     makeTestIK(fmt.Sprintf("k%d", i), 20, binary.OpTypePut),
			SmallestSeqNum: 20,
			LargestSeqNum:  20,
		})
	}

	// Total files added across history: 5 (greater than filesLimit 3)
	// But current live files at any point in time <= 3
	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2, e3})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("expected successful churn replay within live files budget, got: %v", err)
	}
	if res.Version.NumFiles(0) != 3 {
		t.Fatalf("expected exactly 3 live files remaining, got %d", res.Version.NumFiles(0))
	}
	res.Version.Unref()
}

// TestSEC_P07_05_StreamByteBudget verifies that MANIFEST replay halts before allocating
// payload buffers when the cumulative physical stream byte budget is exhausted.
func TestSEC_P07_05_StreamByteBudget(t *testing.T) {
	dir := t.TempDir()
	createDummySSTable(t, dir, 1)
	createDummySSTable(t, dir, 2)

	e1 := makeDummyEdit(1)
	e2 := makeDummyEdit(2)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
	defer func() { _ = disc.Close() }()

	// Inspect file size
	fi, err := disc.File.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	totalBytes := fi.Size()
	if totalBytes < 50 {
		t.Fatalf("expected totalBytes >= 50, got %d", totalBytes)
	}

	// Set byte budget to allow only first edit (~half of totalBytes)
	byteLimit := totalBytes / 2
	restore := SetManifestReplayLimitsForTesting(byteLimit, MaxManifestReplayRecords, MaxManifestLiveFiles)
	defer restore()

	res, err := ReplayManifest(disc)
	if err == nil {
		t.Fatal("expected failure when stream bytes exceed budget, got nil")
	}
	if !stdErrors.Is(err, errors.ErrManifestReplayLimit) {
		t.Fatalf("expected ErrManifestReplayLimit, got: %v", err)
	}

	var limitErr *errors.ManifestReplayLimitError
	if !stdErrors.As(err, &limitErr) {
		t.Fatalf("expected ManifestReplayLimitError, got: %v", err)
	}
	if limitErr.Resource != "bytes" {
		t.Fatalf("expected limitErr.Resource == 'bytes', got %q", limitErr.Resource)
	}
	if res != nil {
		t.Fatal("expected nil ReplayResult on byte limit failure")
	}
}

// TestSEC_P07_05_IntegerOverflowBoundary verifies safe arithmetic boundaries when
// calculating remaining stream byte budgets without integer overflow.
func TestSEC_P07_05_IntegerOverflowBoundary(t *testing.T) {
	// If stream budget is configured high (e.g. math.MaxInt64), offset arithmetic must not overflow
	restore := SetManifestReplayLimitsForTesting(math.MaxInt64, MaxManifestReplayRecords, MaxManifestLiveFiles)
	defer restore()

	dir := t.TempDir()
	createDummySSTable(t, dir, 1)
	e1 := makeDummyEdit(1)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("expected success with MaxInt64 byte limit, got: %v", err)
	}
	res.Version.Unref()
}

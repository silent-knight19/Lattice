package version

import (
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// setupTestManifest is a test helper that writes a series of VersionEdits to a MANIFEST file,
// writes the CURRENT pointer, discovers the active manifest, and returns *DiscoveredManifest.
func setupTestManifest(t *testing.T, dir string, manifestNum uint64, edits []*VersionEdit) *DiscoveredManifest {
	t.Helper()

	manifestPath := ManifestPath(dir, manifestNum)
	w, err := CreateManifestWriter(manifestPath)
	if err != nil {
		t.Fatalf("setupTestManifest: CreateManifestWriter failed: %v", err)
	}

	for i, edit := range edits {
		if err := w.LogEditPtr(edit); err != nil {
			_ = w.Close()
			t.Fatalf("setupTestManifest: LogEditPtr[%d] failed: %v", i, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("setupTestManifest: Close failed: %v", err)
	}

	if err := SetCurrentManifest(dir, manifestNum); err != nil {
		t.Fatalf("setupTestManifest: SetCurrentManifest failed: %v", err)
	}

	disc, err := DiscoverActiveManifest(dir)
	if err != nil {
		t.Fatalf("setupTestManifest: DiscoverActiveManifest failed: %v", err)
	}

	return disc
}

// createDummySSTable creates a valid regular file for the given file number.
func createDummySSTable(t *testing.T, dir string, fileNum uint64) string {
	t.Helper()
	p := TablePath(dir, fileNum)
	if err := os.WriteFile(p, []byte("valid-sstable-regular-file-content"), 0600); err != nil {
		t.Fatalf("createDummySSTable: failed to write %s: %v", p, err)
	}
	return p
}

// -----------------------------------------------------------------------------
// 1. BASIC VALIDATIONS & REPLAY CONTRACTS
// -----------------------------------------------------------------------------

func TestReplayManifest_NilDiscovered(t *testing.T) {
	// Nil DiscoveredManifest
	_, err := ReplayManifest(nil)
	if err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected os.ErrInvalid for nil DiscoveredManifest, got: %v", err)
	}

	// DiscoveredManifest with nil File
	disc := &DiscoveredManifest{Dir: t.TempDir(), File: nil}
	_, err = ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected os.ErrInvalid for nil File descriptor, got: %v", err)
	}

	// DiscoveredManifest with empty Dir
	f, err := os.CreateTemp(t.TempDir(), "manifest_*")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer func() { _ = f.Close() }()

	discEmptyDir := &DiscoveredManifest{Dir: "", File: f}
	_, err = ReplayManifest(discEmptyDir)
	if err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected os.ErrInvalid for empty Dir, got: %v", err)
	}
}

func TestReplayManifest_EmptyManifest(t *testing.T) {
	dir := t.TempDir()
	disc := setupTestManifest(t, dir, 1, nil)
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed on empty manifest: %v", err)
	}
	if res == nil {
		t.Fatalf("expected non-nil ReplayResult")
	}
	if res.ValidRecords != 0 {
		t.Fatalf("expected 0 valid records, got %d", res.ValidRecords)
	}
	if res.FinalOffset != 0 {
		t.Fatalf("expected final offset 0, got %d", res.FinalOffset)
	}
	if res.Version == nil {
		t.Fatalf("expected non-nil Version")
	}
	defer res.Version.Unref()

	for lvl := 0; lvl < NumLevels; lvl++ {
		if count := res.Version.NumFiles(lvl); count != 0 {
			t.Fatalf("level %d has %d files, expected 0", lvl, count)
		}
	}
}

func TestReplayManifest_SingleAddFile(t *testing.T) {
	dir := t.TempDir()

	edit := NewVersionEdit()
	edit.SetNextFileNum(10)
	edit.SetLastSeqNum(100)
	err := edit.AddFile(0, FileMetadata{
		FileNum:        1,
		FileSize:       1024,
		SmallestKey:    makeTestIK("user:001", 10, binary.OpTypePut),
		LargestKey:     makeTestIK("user:050", 20, binary.OpTypePut),
		SmallestSeqNum: 10,
		LargestSeqNum:  20,
	})
	if err != nil {
		t.Fatalf("AddFile failed: %v", err)
	}

	createDummySSTable(t, dir, 1)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{edit})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer res.Version.Unref()

	if res.ValidRecords != 1 {
		t.Fatalf("expected 1 valid record, got %d", res.ValidRecords)
	}
	if res.NextFileNum != 10 {
		t.Fatalf("expected NextFileNum=10, got %d", res.NextFileNum)
	}
	if res.LastSeqNum != 100 {
		t.Fatalf("expected LastSeqNum=100, got %d", res.LastSeqNum)
	}

	if res.Version.NumFiles(0) != 1 {
		t.Fatalf("expected 1 file in L0, got %d", res.Version.NumFiles(0))
	}
	filesL0 := res.Version.Files(0)
	if filesL0[0].FileNum != 1 || filesL0[0].FileSize != 1024 {
		t.Fatalf("unexpected metadata: %+v", filesL0[0])
	}
}

// -----------------------------------------------------------------------------
// 2. INVARIANT INV-06: HISTORICALLY DELETED SSTABLE NEED NOT EXIST ON DISK
// -----------------------------------------------------------------------------

func TestReplayManifest_HistoricallyDeletedSSTable(t *testing.T) {
	dir := t.TempDir()

	// Edit 1: Add File 1 to L0
	e1 := NewVersionEdit()
	e1.SetNextFileNum(2)
	e1.SetLastSeqNum(50)
	_ = e1.AddFile(0, FileMetadata{
		FileNum:        1,
		FileSize:       500,
		SmallestKey:    makeTestIK("a", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("b", 2, binary.OpTypePut),
		SmallestSeqNum: 1,
		LargestSeqNum:  2,
	})

	// Edit 2: Delete File 1 from L0, Add File 2 to L0
	e2 := NewVersionEdit()
	e2.SetNextFileNum(3)
	e2.SetLastSeqNum(100)
	_ = e2.DeleteFile(0, 1)
	_ = e2.AddFile(0, FileMetadata{
		FileNum:        2,
		FileSize:       800,
		SmallestKey:    makeTestIK("c", 3, binary.OpTypePut),
		LargestKey:     makeTestIK("d", 4, binary.OpTypePut),
		SmallestSeqNum: 3,
		LargestSeqNum:  4,
	})

	// Crucial: File 1 does NOT exist on disk! Only File 2 exists.
	createDummySSTable(t, dir, 2)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer res.Version.Unref()

	if res.ValidRecords != 2 {
		t.Fatalf("expected 2 valid records, got %d", res.ValidRecords)
	}
	if res.Version.NumFiles(0) != 1 {
		t.Fatalf("expected 1 file in L0, got %d", res.Version.NumFiles(0))
	}
	filesL0 := res.Version.Files(0)
	if filesL0[0].FileNum != 2 {
		t.Fatalf("expected active file 2 in L0, got %d", filesL0[0].FileNum)
	}
}

// -----------------------------------------------------------------------------
// 3. MULTIPLE LEVELS & CANONICAL ORDERING (L1..L6)
// -----------------------------------------------------------------------------

func TestReplayManifest_MultipleLevelsAndOrdering(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	e.SetNextFileNum(100)
	e.SetLastSeqNum(500)

	// L0: 2 files (unsorted insertion)
	_ = e.AddFile(0, FileMetadata{
		FileNum: 5, FileSize: 100,
		SmallestKey:    makeTestIK("z", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("z9", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})
	_ = e.AddFile(0, FileMetadata{
		FileNum: 2, FileSize: 100,
		SmallestKey:    makeTestIK("a", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("a9", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})

	// L1: 3 non-overlapping files (inserted out of order)
	_ = e.AddFile(1, FileMetadata{
		FileNum: 20, FileSize: 200,
		SmallestKey:    makeTestIK("m", 10, binary.OpTypePut),
		LargestKey:     makeTestIK("p", 20, binary.OpTypePut),
		SmallestSeqNum: 10, LargestSeqNum: 20,
	})
	_ = e.AddFile(1, FileMetadata{
		FileNum: 10, FileSize: 200,
		SmallestKey:    makeTestIK("b", 10, binary.OpTypePut),
		LargestKey:     makeTestIK("d", 20, binary.OpTypePut),
		SmallestSeqNum: 10, LargestSeqNum: 20,
	})
	_ = e.AddFile(1, FileMetadata{
		FileNum: 30, FileSize: 200,
		SmallestKey:    makeTestIK("s", 10, binary.OpTypePut),
		LargestKey:     makeTestIK("w", 20, binary.OpTypePut),
		SmallestSeqNum: 10, LargestSeqNum: 20,
	})

	// Create physical SSTables
	createDummySSTable(t, dir, 5)
	createDummySSTable(t, dir, 2)
	createDummySSTable(t, dir, 20)
	createDummySSTable(t, dir, 10)
	createDummySSTable(t, dir, 30)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer res.Version.Unref()

	// Assert L0 sorted by FileNum ASC
	l0 := res.Version.Files(0)
	if len(l0) != 2 || l0[0].FileNum != 2 || l0[1].FileNum != 5 {
		t.Fatalf("L0 not sorted by FileNum ASC: %+v", l0)
	}

	// Assert L1 sorted canonically by SmallestKey
	l1 := res.Version.Files(1)
	if len(l1) != 3 {
		t.Fatalf("expected 3 files in L1, got %d", len(l1))
	}
	if l1[0].FileNum != 10 || l1[1].FileNum != 20 || l1[2].FileNum != 30 {
		t.Fatalf("L1 files not sorted canonically by SmallestKey: %+v", l1)
	}
}

// -----------------------------------------------------------------------------
// 4. MONOTONIC SCALARS
// -----------------------------------------------------------------------------

func TestReplayManifest_MonotonicScalars(t *testing.T) {
	dir := t.TempDir()

	e1 := NewVersionEdit()
	e1.SetNextFileNum(50)
	e1.SetLastSeqNum(100)

	e2 := NewVersionEdit()
	e2.SetNextFileNum(75)
	e2.SetLastSeqNum(250)

	// e3 does not set scalars
	e3 := NewVersionEdit()

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2, e3})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer res.Version.Unref()

	if res.NextFileNum != 75 {
		t.Fatalf("expected NextFileNum 75, got %d", res.NextFileNum)
	}
	if res.LastSeqNum != 250 {
		t.Fatalf("expected LastSeqNum 250, got %d", res.LastSeqNum)
	}
}

// -----------------------------------------------------------------------------
// 5. INVARIANT INV-05: MISSING SSTABLE FILE FAILS WITH ErrMissingSSTable
// -----------------------------------------------------------------------------

func TestReplayManifest_MissingSSTable(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	_ = e.AddFile(0, FileMetadata{
		FileNum:        99,
		FileSize:       1000,
		SmallestKey:    makeTestIK("k1", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("k2", 2, binary.OpTypePut),
		SmallestSeqNum: 1,
		LargestSeqNum:  2,
	})

	// Intentionally do NOT create 000099.sst on disk!
	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err == nil {
		if res != nil && res.Version != nil {
			res.Version.Unref()
		}
		t.Fatalf("expected ErrMissingSSTable, got success")
	}

	if !stdErrors.Is(err, errors.ErrMissingSSTable) {
		t.Fatalf("expected error wrapping ErrMissingSSTable, got: %v", err)
	}

	var replayErr *ReplayError
	if !stdErrors.As(err, &replayErr) {
		t.Fatalf("expected error to be *ReplayError, got: %T", err)
	}
	if replayErr.ManifestNum != 1 {
		t.Fatalf("expected ManifestNum 1, got %d", replayErr.ManifestNum)
	}
}

// -----------------------------------------------------------------------------
// 6. SSTABLE OBJECT-TYPE VALIDATION: SYMLINK AND DIRECTORY
// -----------------------------------------------------------------------------

func TestReplayManifest_SSTableIsSymlink(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	_ = e.AddFile(0, FileMetadata{
		FileNum:        7,
		FileSize:       1000,
		SmallestKey:    makeTestIK("k1", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("k2", 2, binary.OpTypePut),
		SmallestSeqNum: 1,
		LargestSeqNum:  2,
	})

	// Create real file, then create a symlink with the SSTable name pointing to it
	realFile := filepath.Join(dir, "real_data.bin")
	if err := os.WriteFile(realFile, []byte("payload"), 0600); err != nil {
		t.Fatalf("failed to write realFile: %v", err)
	}
	sstPath := TablePath(dir, 7)
	if err := os.Symlink(realFile, sstPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	_, err := ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrSSTableSymlink) {
		t.Fatalf("expected ErrSSTableSymlink for symlink SSTable, got: %v", err)
	}
}

func TestReplayManifest_SSTableIsDirectory(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	_ = e.AddFile(0, FileMetadata{
		FileNum:        8,
		FileSize:       1000,
		SmallestKey:    makeTestIK("k1", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("k2", 2, binary.OpTypePut),
		SmallestSeqNum: 1,
		LargestSeqNum:  2,
	})

	// Create directory masquerading as SSTable
	sstPath := TablePath(dir, 8)
	if err := os.Mkdir(sstPath, 0700); err != nil {
		t.Fatalf("failed to mkdir %s: %v", sstPath, err)
	}

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	_, err := ReplayManifest(disc)
	if err == nil {
		t.Fatalf("expected error for directory SSTable, got nil")
	}
	var notADirErr *errors.NotADirectoryError
	if !stdErrors.As(err, &notADirErr) {
		t.Fatalf("expected *errors.NotADirectoryError, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 7. CORRUPTION MATRIX: CRC, TRUNCATION, OVERSIZED PAYLOAD, MALFORMED BYTES
// -----------------------------------------------------------------------------

func TestReplayManifest_BadCRC(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	e.SetNextFileNum(10)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	// Corrupt CRC byte in the file directly
	manifestPath := ManifestPath(dir, 1)
	f, err := os.OpenFile(manifestPath, os.O_RDWR, 0) // #nosec G304 - test-only file corruption
	if err != nil {
		t.Fatalf("failed to open manifest for corruption: %v", err)
	}
	// Flip first byte of CRC
	var b [1]byte
	_, _ = f.ReadAt(b[:], 0)
	b[0] ^= 0xFF
	_, _ = f.WriteAt(b[:], 0)
	_ = f.Close()

	_, err = ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got: %v", err)
	}
}

func TestReplayManifest_TruncatedHeader(t *testing.T) {
	dir := t.TempDir()

	disc := setupTestManifest(t, dir, 1, nil)
	defer func() { _ = disc.Close() }()

	// Write 5 bytes (fewer than 8-byte ManifestHeaderSize)
	manifestPath := ManifestPath(dir, 1)
	if err := os.WriteFile(manifestPath, []byte{0x01, 0x02, 0x03, 0x04, 0x05}, 0600); err != nil {
		t.Fatalf("failed to truncate header: %v", err)
	}

	_, err := ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrManifestHeaderTruncated) {
		t.Fatalf("expected ErrManifestHeaderTruncated, got: %v", err)
	}
}

func TestReplayManifest_TruncatedPayload(t *testing.T) {
	dir := t.TempDir()

	disc := setupTestManifest(t, dir, 1, nil)
	defer func() { _ = disc.Close() }()

	// Header declares 100 bytes, but we only supply 10 bytes
	var record [18]byte
	binary.PutUint32(record[0:4], 0x12345678)
	binary.PutUint32(record[4:8], 100) // declares 100 bytes

	manifestPath := ManifestPath(dir, 1)
	if err := os.WriteFile(manifestPath, record[:], 0600); err != nil {
		t.Fatalf("failed to write truncated payload: %v", err)
	}

	_, err := ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrManifestPayloadTruncated) {
		t.Fatalf("expected ErrManifestPayloadTruncated, got: %v", err)
	}
}

func TestReplayManifest_OversizedPayload(t *testing.T) {
	dir := t.TempDir()

	disc := setupTestManifest(t, dir, 1, nil)
	defer func() { _ = disc.Close() }()

	// Header declares > MaxVersionEditBytes (16 MiB + 1)
	var header [8]byte
	binary.PutUint32(header[0:4], 0x12345678)
	binary.PutUint32(header[4:8], MaxVersionEditBytes+1)

	manifestPath := ManifestPath(dir, 1)
	if err := os.WriteFile(manifestPath, header[:], 0600); err != nil {
		t.Fatalf("failed to write header: %v", err)
	}

	_, err := ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrManifestCorrupted) {
		t.Fatalf("expected ErrManifestCorrupted for oversized payload, got: %v", err)
	}
}

func TestReplayManifest_InvalidVersionEditBytes(t *testing.T) {
	dir := t.TempDir()

	// Write a record with valid CRC and length, but corrupted VersionEdit payload (e.g. invalid format byte 0xFF)
	payload := []byte{0xFF, 0x01, 0x02}
	var header [8]byte
	binary.PutUint32(header[4:8], uint32(len(payload))) // #nosec G115 - test payload length is 3
	crc := crc32.Update(0, crc32.IEEETable, header[4:8])
	crc = crc32.Update(crc, crc32.IEEETable, payload)
	binary.PutUint32(header[0:4], crc)

	record := append(header[:], payload...)

	manifestPath := ManifestPath(dir, 1)
	if err := os.WriteFile(manifestPath, record, 0600); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}
	if err := SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("failed to set CURRENT: %v", err)
	}

	disc, err := DiscoverActiveManifest(dir)
	if err != nil {
		t.Fatalf("DiscoverActiveManifest failed: %v", err)
	}
	defer func() { _ = disc.Close() }()

	_, err = ReplayManifest(disc)
	if err == nil {
		t.Fatalf("expected error for invalid VersionEdit payload, got nil")
	}
	var unsupportedErr *errors.UnsupportedVersionEditError
	if !stdErrors.As(err, &unsupportedErr) {
		t.Fatalf("expected UnsupportedVersionEditError, got: %v", err)
	}
}

func TestReplayManifest_OverlappingKeyRangeInLevel1(t *testing.T) {
	dir := t.TempDir()

	// Two edits that individually are valid, but combined create overlapping key ranges in L1
	e1 := NewVersionEdit()
	_ = e1.AddFile(1, FileMetadata{
		FileNum: 1, FileSize: 100,
		SmallestKey:    makeTestIK("a", 10, binary.OpTypePut),
		LargestKey:     makeTestIK("m", 20, binary.OpTypePut),
		SmallestSeqNum: 10, LargestSeqNum: 20,
	})

	e2 := NewVersionEdit()
	_ = e2.AddFile(1, FileMetadata{
		FileNum: 2, FileSize: 100,
		SmallestKey:    makeTestIK("h", 10, binary.OpTypePut), // overlaps with "a".."m"
		LargestKey:     makeTestIK("z", 20, binary.OpTypePut),
		SmallestSeqNum: 10, LargestSeqNum: 20,
	})

	createDummySSTable(t, dir, 1)
	createDummySSTable(t, dir, 2)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
	defer func() { _ = disc.Close() }()

	_, err := ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
		t.Fatalf("expected ErrInvalidKeyRange for overlapping keys in L1, got: %v", err)
	}
}

func TestReplayManifest_ConflictingFileNumberAcrossLevels(t *testing.T) {
	dir := t.TempDir()

	// e1 adds File 1 to L0
	e1 := NewVersionEdit()
	_ = e1.AddFile(0, FileMetadata{
		FileNum: 1, FileSize: 100,
		SmallestKey:    makeTestIK("a", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("b", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})

	// e2 adds File 1 to L1 without deleting from L0
	e2 := NewVersionEdit()
	_ = e2.AddFile(1, FileMetadata{
		FileNum: 1, FileSize: 100,
		SmallestKey:    makeTestIK("c", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("d", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})

	createDummySSTable(t, dir, 1)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
	defer func() { _ = disc.Close() }()

	_, err := ReplayManifest(disc)
	if err == nil || !stdErrors.Is(err, errors.ErrCorruptedVersionEdit) {
		t.Fatalf("expected ErrCorruptedVersionEdit for duplicate FileNum across levels, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 8. 100-HISTORICAL-EDIT ACCEPTANCE TEST (DIFFERENTIAL VERIFICATION)
// -----------------------------------------------------------------------------

type referenceOracle struct {
	levels      [NumLevels]map[uint64]FileMetadata
	nextFileNum uint64
	lastSeqNum  binary.SeqNum
}

func newReferenceOracle() *referenceOracle {
	o := &referenceOracle{}
	for i := 0; i < NumLevels; i++ {
		o.levels[i] = make(map[uint64]FileMetadata)
	}
	return o
}

func (o *referenceOracle) apply(edit *VersionEdit) {
	for _, d := range edit.DeletedFiles() {
		if d.Level < NumLevels {
			delete(o.levels[d.Level], d.FileNum)
		}
	}
	for _, a := range edit.AddedFiles() {
		o.levels[a.Level][a.Meta.FileNum] = a.Meta.Clone()
	}
	if nextNum, ok := edit.NextFileNum(); ok {
		if nextNum > o.nextFileNum {
			o.nextFileNum = nextNum
		}
	}
	if lastSeq, ok := edit.LastSeqNum(); ok {
		if lastSeq > o.lastSeqNum {
			o.lastSeqNum = lastSeq
		}
	}
}

func TestReplayManifest_100HistoricalEdits_Differential(t *testing.T) {
	dir := t.TempDir()
	oracle := newReferenceOracle()

	const numEdits = 100
	edits := make([]*VersionEdit, numEdits)

	var fileCounter uint64 = 1
	var seqCounter uint64 = 100

	// Construct 100 realistic edits
	for i := 0; i < numEdits; i++ {
		e := NewVersionEdit()
		seqCounter += 10
		e.SetLastSeqNum(binary.SeqNum(seqCounter))
		e.SetNextFileNum(fileCounter + 20)

		switch {
		case i < 40:
			// Initial flushes to L0
			fileNum := fileCounter
			fileCounter++
			sk := fmt.Sprintf("key:%06d:a", i)
			lk := fmt.Sprintf("key:%06d:z", i)
			_ = e.AddFile(0, FileMetadata{
				FileNum:        fileNum,
				FileSize:       1024 * uint64(i+1),
				SmallestKey:    makeTestIK(sk, seqCounter-5, binary.OpTypePut),
				LargestKey:     makeTestIK(lk, seqCounter, binary.OpTypePut),
				SmallestSeqNum: seqCounter - 5,
				LargestSeqNum:  seqCounter,
			})

		case i < 70:
			// Compaction from L0 to L1: delete an earlier L0 file, add non-overlapping file to L1
			victimL0 := uint64(i - 39) // delete file 1, 2, ...
			_ = e.DeleteFile(0, victimL0)

			fileNum := fileCounter
			fileCounter++
			// Non-overlapping range partition for L1: use strictly segregated prefix by fileNum
			sk := fmt.Sprintf("p%03d:aaa", i)
			lk := fmt.Sprintf("p%03d:zzz", i)
			_ = e.AddFile(1, FileMetadata{
				FileNum:        fileNum,
				FileSize:       2048 * uint64(i),
				SmallestKey:    makeTestIK(sk, seqCounter-5, binary.OpTypePut),
				LargestKey:     makeTestIK(lk, seqCounter, binary.OpTypePut),
				SmallestSeqNum: seqCounter - 5,
				LargestSeqNum:  seqCounter,
			})

		default:
			// Deeper levels (L2..L6) additions and L1 deletions
			targetLevel := uint32(2 + (i % 5)) // 2..6
			fileNum := fileCounter
			fileCounter++
			sk := fmt.Sprintf("k%03d:min", i)
			lk := fmt.Sprintf("k%03d:max", i)
			_ = e.AddFile(targetLevel, FileMetadata{
				FileNum:        fileNum,
				FileSize:       4096,
				SmallestKey:    makeTestIK(sk, seqCounter-5, binary.OpTypePut),
				LargestKey:     makeTestIK(lk, seqCounter, binary.OpTypePut),
				SmallestSeqNum: seqCounter - 5,
				LargestSeqNum:  seqCounter,
			})
		}

		if err := e.Validate(); err != nil {
			t.Fatalf("edit %d failed validation: %v", i, err)
		}

		oracle.apply(e)
		edits[i] = e
	}

	// Create dummy SSTable regular files ONLY for surviving active files in the oracle
	for lvl := 0; lvl < NumLevels; lvl++ {
		for fileNum := range oracle.levels[lvl] {
			createDummySSTable(t, dir, fileNum)
		}
	}

	disc := setupTestManifest(t, dir, 1, edits)
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed on 100 historical edits: %v", err)
	}
	defer res.Version.Unref()

	if res.ValidRecords != 100 {
		t.Fatalf("expected 100 valid records, got %d", res.ValidRecords)
	}
	if res.NextFileNum != oracle.nextFileNum {
		t.Fatalf("NextFileNum mismatch: expected %d, got %d", oracle.nextFileNum, res.NextFileNum)
	}
	if res.LastSeqNum != oracle.lastSeqNum {
		t.Fatalf("LastSeqNum mismatch: expected %d, got %d", oracle.lastSeqNum, res.LastSeqNum)
	}

	// Differential comparison across all 7 levels
	for lvl := 0; lvl < NumLevels; lvl++ {
		expectedMap := oracle.levels[lvl]
		actualFiles := res.Version.Files(lvl)

		if len(actualFiles) != len(expectedMap) {
			t.Fatalf("level %d file count mismatch: expected %d, got %d", lvl, len(expectedMap), len(actualFiles))
		}

		for _, actual := range actualFiles {
			expected, exists := expectedMap[actual.FileNum]
			if !exists {
				t.Fatalf("level %d contains unexpected file %d", lvl, actual.FileNum)
			}
			if !actual.Equal(expected) {
				t.Fatalf("level %d file %d metadata mismatch: expected %+v, got %+v", lvl, actual.FileNum, expected, actual)
			}
		}

		// Verify canonical sorting on L1..L6
		if lvl >= 1 && len(actualFiles) > 1 {
			for i := 0; i < len(actualFiles)-1; i++ {
				ikA := decodeInternalKeyNoAlloc(actualFiles[i].SmallestKey)
				ikB := decodeInternalKeyNoAlloc(actualFiles[i+1].SmallestKey)
				if binary.CompareInternalKey(ikA, ikB) >= 0 {
					t.Fatalf("level %d files not sorted canonically: file %d >= file %d", lvl, actualFiles[i].FileNum, actualFiles[i+1].FileNum)
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// 9. INVARIANTS INV-04 & INV-10: STATE ISOLATION & ZERO MUTATION ON FAILURE
// -----------------------------------------------------------------------------

func TestReplayManifest_IsolationOnFailure(t *testing.T) {
	dir := t.TempDir()

	// 1. Establish initial live VersionSet with V0
	vset := NewVersionSet()
	var initialLevels [NumLevels][]FileMetadata
	v0 := NewVersion(initialLevels)
	if err := vset.AppendVersion(v0); err != nil {
		t.Fatalf("AppendVersion failed: %v", err)
	}
	curBefore := vset.Current()
	defer curBefore.Unref()

	// 2. Create MANIFEST with 2 valid edits, but the second references a missing SSTable
	e1 := NewVersionEdit()
	e1.SetNextFileNum(10)
	_ = e1.AddFile(0, FileMetadata{
		FileNum: 1, FileSize: 500,
		SmallestKey:    makeTestIK("a", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("b", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})

	e2 := NewVersionEdit()
	_ = e2.AddFile(0, FileMetadata{
		FileNum: 2, FileSize: 500, // file 2 is missing on disk!
		SmallestKey:    makeTestIK("c", 3, binary.OpTypePut),
		LargestKey:     makeTestIK("d", 4, binary.OpTypePut),
		SmallestSeqNum: 3, LargestSeqNum: 4,
	})

	createDummySSTable(t, dir, 1) // create file 1, but NOT file 2

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e1, e2})
	defer func() { _ = disc.Close() }()

	manifestStatBefore, _ := os.Stat(disc.Path)
	currentStatBefore, _ := os.Stat(filepath.Join(dir, "CURRENT"))

	// 3. Run replay - must fail due to missing SSTable
	res, err := ReplayManifest(disc)
	if err == nil {
		if res != nil && res.Version != nil {
			res.Version.Unref()
		}
		t.Fatalf("expected replay to fail due to missing SSTable, got success")
	}

	// 4. Assert live VersionSet is completely unaltered
	curAfter := vset.Current()
	defer curAfter.Unref()

	if curBefore.ID() != curAfter.ID() {
		t.Fatalf("VersionSet.Current ID mutated from %d to %d", curBefore.ID(), curAfter.ID())
	}
	if vset.ActiveCount() != 1 {
		t.Fatalf("expected ActiveCount 1, got %d", vset.ActiveCount())
	}

	// 5. Assert zero disk mutation on MANIFEST and CURRENT
	manifestStatAfter, _ := os.Stat(disc.Path)
	currentStatAfter, _ := os.Stat(filepath.Join(dir, "CURRENT"))

	if manifestStatBefore.Size() != manifestStatAfter.Size() {
		t.Fatalf("MANIFEST size mutated from %d to %d", manifestStatBefore.Size(), manifestStatAfter.Size())
	}
	if currentStatBefore.Size() != currentStatAfter.Size() {
		t.Fatalf("CURRENT size mutated from %d to %d", currentStatBefore.Size(), currentStatAfter.Size())
	}
}

// -----------------------------------------------------------------------------
// 10. INVARIANT INV-12 & SECTION 26: DESCRIPTOR BORROWING & REUSE
// -----------------------------------------------------------------------------

func TestReplayManifest_DescriptorReuseAndOwnership(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	e.SetNextFileNum(5)
	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})

	// Run ReplayManifest
	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer res.Version.Unref()

	// Verify descriptor is still open and owned by caller
	var b [1]byte
	_, readErr := disc.File.ReadAt(b[:], 0)
	if readErr != nil {
		t.Fatalf("descriptor was unexpectedly closed by ReplayManifest: %v", readErr)
	}

	// Caller closes descriptor cleanly
	if err := disc.Close(); err != nil {
		t.Fatalf("disc.Close failed: %v", err)
	}

	// Double close is safe idempotent no-op
	if err := disc.Close(); err != nil {
		t.Fatalf("idempotent disc.Close failed: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 11. INVARIANT INV-08: REPLAY DETERMINISM
// -----------------------------------------------------------------------------

func TestReplayManifest_Determinism(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	e.SetNextFileNum(15)
	e.SetLastSeqNum(300)
	_ = e.AddFile(0, FileMetadata{
		FileNum: 1, FileSize: 100,
		SmallestKey:    makeTestIK("k1", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("k2", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})
	_ = e.AddFile(1, FileMetadata{
		FileNum: 2, FileSize: 200,
		SmallestKey:    makeTestIK("k3", 3, binary.OpTypePut),
		LargestKey:     makeTestIK("k4", 4, binary.OpTypePut),
		SmallestSeqNum: 3, LargestSeqNum: 4,
	})

	createDummySSTable(t, dir, 1)
	createDummySSTable(t, dir, 2)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	// Replay run 1
	res1, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}
	defer res1.Version.Unref()

	// Replay run 2
	res2, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}
	defer res2.Version.Unref()

	if res1.ValidRecords != res2.ValidRecords {
		t.Fatalf("record count mismatch: %d vs %d", res1.ValidRecords, res2.ValidRecords)
	}
	if res1.FinalOffset != res2.FinalOffset {
		t.Fatalf("final offset mismatch: %d vs %d", res1.FinalOffset, res2.FinalOffset)
	}
	if res1.NextFileNum != res2.NextFileNum {
		t.Fatalf("nextFileNum mismatch: %d vs %d", res1.NextFileNum, res2.NextFileNum)
	}
	if res1.LastSeqNum != res2.LastSeqNum {
		t.Fatalf("lastSeqNum mismatch: %d vs %d", res1.LastSeqNum, res2.LastSeqNum)
	}

	for lvl := 0; lvl < NumLevels; lvl++ {
		f1 := res1.Version.Files(lvl)
		f2 := res2.Version.Files(lvl)
		if len(f1) != len(f2) {
			t.Fatalf("level %d count mismatch: %d vs %d", lvl, len(f1), len(f2))
		}
		for i := range f1 {
			if !f1[i].Equal(f2[i]) {
				t.Fatalf("level %d file %d mismatch: %+v vs %+v", lvl, i, f1[i], f2[i])
			}
		}
	}
}

// -----------------------------------------------------------------------------
// 12. UNREFERENCED EXTRA SSTABLE ON DISK (IGNORED BY MANIFEST REPLAY)
// -----------------------------------------------------------------------------

func TestReplayManifest_UnreferencedExtraSSTable(t *testing.T) {
	dir := t.TempDir()

	e := NewVersionEdit()
	_ = e.AddFile(0, FileMetadata{
		FileNum: 1, FileSize: 100,
		SmallestKey:    makeTestIK("a", 1, binary.OpTypePut),
		LargestKey:     makeTestIK("b", 2, binary.OpTypePut),
		SmallestSeqNum: 1, LargestSeqNum: 2,
	})

	createDummySSTable(t, dir, 1)

	// Extra SSTables on disk not referenced in the manifest (e.g. obsolete or staged)
	createDummySSTable(t, dir, 999)
	createDummySSTable(t, dir, 1000)

	disc := setupTestManifest(t, dir, 1, []*VersionEdit{e})
	defer func() { _ = disc.Close() }()

	res, err := ReplayManifest(disc)
	if err != nil {
		t.Fatalf("ReplayManifest failed: %v", err)
	}
	defer res.Version.Unref()

	if res.Version.NumFiles(0) != 1 || res.Version.Files(0)[0].FileNum != 1 {
		t.Fatalf("unexpected files in L0: %+v", res.Version.Files(0))
	}
}

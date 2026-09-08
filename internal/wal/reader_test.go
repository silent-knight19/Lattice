package wal_test

import (
	"crypto/sha256"
	stdErrors "errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestReader_EmptyWAL verifies Requirement 1:
// Opening an empty 0-byte file immediately produces io.EOF on the first Next() call,
// with Offset remaining 0.
func TestReader_EmptyWAL(t *testing.T) {
	dir := t.TempDir()
	emptyPath := filepath.Join(dir, "empty.log")
	if err := os.WriteFile(emptyPath, nil, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(emptyPath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	if r.Offset() != 0 {
		t.Errorf("initial offset must be 0, got %d", r.Offset())
	}

	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on empty file, got %v", err)
	}

	if r.Offset() != 0 {
		t.Errorf("offset must remain 0 after EOF, got %d", r.Offset())
	}
}

// TestReader_SingleRecord verifies Requirement 2:
// Reading a single record returns the exact record, subsequent Next() returns io.EOF,
// and Offset advances by exactly the physical wire size.
func TestReader_SingleRecord(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(42),
		Timestamp: 1725800000,
		Key:       []byte("single_key"),
		Value:     []byte("single_value_payload"),
	}
	if err := w.AppendSync(rec); err != nil {
		t.Fatalf("AppendSync failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	r, err := wal.OpenSegmentReader(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	expectedLen := int64(wal.MinRecordSize + len(rec.Key) + len(rec.Value))

	gotRec, err := r.Next()
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if !rec.Equal(gotRec) {
		t.Errorf("record mismatch: got %+v, want %+v", gotRec, rec)
	}
	if r.Offset() != expectedLen {
		t.Errorf("offset mismatch: got %d, want %d", r.Offset(), expectedLen)
	}

	// Clean EOF on second call
	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on second call, got %v", err)
	}
	if r.Offset() != expectedLen {
		t.Errorf("offset must remain %d after EOF, got %d", expectedLen, r.Offset())
	}
}

// TestReader_MultipleRecords verifies Requirement 3:
// Multiple sequential records are returned in exact order, with offsets advancing monotonically.
func TestReader_MultipleRecords(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 2)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}

	records := []wal.Record{
		{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 1000, Key: []byte("k1"), Value: []byte("v1")},
		{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 1001, Key: []byte("k2"), Value: []byte("v2_longer_payload")},
		{Type: wal.RecordTypeDelete, SeqNum: 3, Timestamp: 1002, Key: []byte("k3"), Value: nil},
		{Type: wal.RecordTypePut, SeqNum: 4, Timestamp: 1003, Key: []byte("k4"), Value: []byte("v4_final")},
	}

	for _, r := range records {
		if err := w.AppendSync(r); err != nil {
			t.Fatalf("AppendSync failed: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	r, err := wal.OpenSegmentReader(dbPath, 2)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	var cumulativeOffset int64
	for i, want := range records {
		recLen := int64(wal.MinRecordSize + len(want.Key) + len(want.Value))
		cumulativeOffset += recLen

		got, err := r.Next()
		if err != nil {
			t.Fatalf("record %d Next failed: %v", i, err)
		}
		if !want.Equal(got) {
			t.Errorf("record %d mismatch: got %+v, want %+v", i, got, want)
		}
		if r.Offset() != cumulativeOffset {
			t.Errorf("record %d offset mismatch: got %d, want %d", i, r.Offset(), cumulativeOffset)
		}
	}

	// Clean EOF
	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF at end of stream, got %v", err)
	}
	if r.Offset() != cumulativeOffset {
		t.Errorf("final offset mismatch: got %d, want %d", r.Offset(), cumulativeOffset)
	}
}

// TestReader_AllRecordTypes verifies Requirement 4:
// PUT, DELETE, BATCH_START, and BATCH_COMMIT records are read and validated.
func TestReader_AllRecordTypes(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 3)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}

	records := []wal.Record{
		{Type: wal.RecordTypeBatchStart, SeqNum: 10, Timestamp: 100},
		{Type: wal.RecordTypePut, SeqNum: 11, Timestamp: 101, Key: []byte("key_put"), Value: []byte("val_put")},
		{Type: wal.RecordTypeDelete, SeqNum: 12, Timestamp: 102, Key: []byte("key_del"), Value: nil},
		{Type: wal.RecordTypeBatchCommit, SeqNum: 13, Timestamp: 103},
	}

	for _, rec := range records {
		if err := w.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync failed: %v", err)
		}
	}
	_ = w.Close()

	r, err := wal.OpenSegmentReader(dbPath, 3)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	for i, want := range records {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("record %d Next failed: %v", i, err)
		}
		if !want.Equal(got) {
			t.Errorf("record %d mismatch: got %+v, want %+v", i, got, want)
		}
	}

	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

// TestReader_MaximumValidRecord verifies Requirement 5:
// Reading records at maximum architectural boundaries: MaxKeyLen (64 KB - 1) and MaxValueLen (4 MB).
func TestReader_MaximumValidRecord(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 4)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}

	maxKey := make([]byte, binary.MaxKeyLen)
	for i := range maxKey {
		maxKey[i] = byte(i % 256)
	}

	maxVal := make([]byte, binary.MaxValueLen)
	maxVal[0] = 0xAA
	maxVal[len(maxVal)-1] = 0xBB

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    100,
		Timestamp: 1725800000,
		Key:       maxKey,
		Value:     maxVal,
	}

	if err := w.AppendSync(rec); err != nil {
		t.Fatalf("AppendSync max record failed: %v", err)
	}
	_ = w.Close()

	r, err := wal.OpenSegmentReader(dbPath, 4)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := r.Next()
	if err != nil {
		t.Fatalf("Next max record failed: %v", err)
	}
	if !rec.Equal(got) {
		t.Fatalf("max record mismatch")
	}

	expectedOffset := int64(wal.MinRecordSize + len(maxKey) + len(maxVal))
	if r.Offset() != expectedOffset {
		t.Errorf("offset mismatch: got %d, want %d", r.Offset(), expectedOffset)
	}

	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF after max record, got %v", err)
	}
}

// TestReader_CleanEOF verifies Requirement 6:
// Clean EOF exactly at a record boundary returns io.EOF, never io.ErrUnexpectedEOF or ErrTornWrite.
func TestReader_CleanEOF(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 5)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	_ = w.AppendSync(wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("k"),
		Value:     []byte("v"),
	})
	_ = w.Close()

	r, err := wal.OpenSegmentReader(dbPath, 5)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err != nil {
		t.Fatalf("Next 1 failed: %v", err)
	}

	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected clean io.EOF, got %v", err)
	}
	if stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("clean EOF must not be io.ErrUnexpectedEOF")
	}
}

// TestReader_TruncatedHeader verifies Requirement 7:
// File with 1..20 bytes of a header produces ErrHeaderTruncated and io.ErrUnexpectedEOF.
func TestReader_TruncatedHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "truncated_header.log")

	// 10 bytes (less than HeaderSize = 21)
	partialHeader := make([]byte, 10)
	if err := os.WriteFile(path, partialHeader, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on truncated header, got nil")
	}
	if !stdErrors.Is(err, errors.ErrHeaderTruncated) {
		t.Errorf("expected ErrHeaderTruncated, got %v", err)
	}
	if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if r.Offset() != 0 {
		t.Errorf("offset must remain 0 on truncation error, got %d", r.Offset())
	}
}

// TestReader_TruncatedKey verifies Requirement 8:
// File truncated mid-way through KeyBytes produces io.ErrUnexpectedEOF.
func TestReader_TruncatedKey(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("long_key_to_truncate"),
		Value:     []byte("value"),
	}
	fullBytes, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Truncate halfway through key bytes: HeaderSize(21) + KeyLen(2) + 5
	truncated := fullBytes[:wal.HeaderSize+2+5]

	dir := t.TempDir()
	path := filepath.Join(dir, "truncated_key.log")
	if err := os.WriteFile(path, truncated, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on truncated key, got nil")
	}
	if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if r.Offset() != 0 {
		t.Errorf("offset must remain 0 on truncated key, got %d", r.Offset())
	}
}

// TestReader_TruncatedValue verifies Requirement 9:
// File truncated mid-way through ValueBytes produces io.ErrUnexpectedEOF.
func TestReader_TruncatedValue(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("key"),
		Value:     []byte("value_payload_to_truncate_partially"),
	}
	fullBytes, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Truncate 5 bytes before end of value
	truncated := fullBytes[:len(fullBytes)-5]

	dir := t.TempDir()
	path := filepath.Join(dir, "truncated_val.log")
	if err := os.WriteFile(path, truncated, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on truncated value, got nil")
	}
	if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	if r.Offset() != 0 {
		t.Errorf("offset must remain 0 on truncated value, got %d", r.Offset())
	}
}

// TestReader_ChecksumCorruption verifies Requirement 10:
// Single bit-flip corruption produces ErrChecksumMismatch.
func TestReader_ChecksumCorruption(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("corrupt_key"),
		Value:     []byte("corrupt_value"),
	}
	fullBytes, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Flip a bit in the value payload
	fullBytes[len(fullBytes)-1] ^= 0x01

	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt_crc.log")
	if err := os.WriteFile(path, fullBytes, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on corrupted record, got nil")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got %v", err)
	}
	var mismatch *errors.ChecksumMismatchError
	if !stdErrors.As(err, &mismatch) {
		t.Errorf("expected *errors.ChecksumMismatchError, got %T", err)
	}
	if r.Offset() != 0 {
		t.Errorf("offset must remain 0 on checksum error, got %d", r.Offset())
	}
}

// TestReader_MiddleCorruption verifies Requirement 11 & Middle-Corruption Isolation:
// When stream is A || corrupt(B) || C:
// - Next() returns A
// - Next() returns ErrChecksumMismatch
// - Next() does NOT silently return C
// - File bytes are completely unmodified (reader does not repair).
func TestReader_MiddleCorruption(t *testing.T) {
	recA := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("kA"), Value: []byte("vA")}
	recB := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("kB"), Value: []byte("vB")}
	recC := wal.Record{Type: wal.RecordTypePut, SeqNum: 3, Timestamp: 102, Key: []byte("kC"), Value: []byte("vC")}

	encA, _ := wal.EncodeRecord(recA)
	encB, _ := wal.EncodeRecord(recB)
	encC, _ := wal.EncodeRecord(recC)

	// Corrupt record B's payload
	encB[len(encB)-1] ^= 0x02

	var combined []byte
	combined = append(combined, encA...)
	combined = append(combined, encB...)
	combined = append(combined, encC...)

	dir := t.TempDir()
	path := filepath.Join(dir, "middle_corrupt.log")
	if err := os.WriteFile(path, combined, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	initialHash := sha256.Sum256(combined)

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	// First Next() must return record A cleanly
	gotA, err := r.Next()
	if err != nil {
		t.Fatalf("expected record A, got error: %v", err)
	}
	if !recA.Equal(gotA) {
		t.Errorf("record A mismatch: got %+v, want %+v", gotA, recA)
	}
	if r.Offset() != int64(len(encA)) {
		t.Errorf("offset after A mismatch: got %d, want %d", r.Offset(), len(encA))
	}

	// Second Next() must fail with checksum mismatch on B
	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected corruption error on record B, got nil")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got %v", err)
	}

	// Crucial invariant: Offset remains at start of B, NOT advanced past B
	if r.Offset() != int64(len(encA)) {
		t.Errorf("offset must remain at start of B (%d), got %d", len(encA), r.Offset())
	}

	// File MUST remain unchanged: reader never repairs or deletes corrupted records
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	afterHash := sha256.Sum256(afterBytes)
	if initialHash != afterHash {
		t.Fatalf("WAL file was modified during read! Reader must be strictly read-only.")
	}
}

// TestReader_InvalidRecordType verifies Requirement 12:
// Corrupted record type byte propagates InvalidRecordTypeError.
func TestReader_InvalidRecordType(t *testing.T) {
	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")}
	enc, _ := wal.EncodeRecord(rec)

	// Corrupt RecordType at byte offset 4 to 0x77
	enc[4] = 0x77

	dir := t.TempDir()
	path := filepath.Join(dir, "invalid_type.log")
	if err := os.WriteFile(path, enc, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on invalid record type, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
		t.Errorf("expected ErrInvalidRecordType, got %v", err)
	}
}

// TestReader_OversizedLength verifies Requirement 13:
// Malformed value length (>4MB) triggers ValueTooLargeError without allocating memory.
func TestReader_OversizedLength(t *testing.T) {
	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")}
	enc, _ := wal.EncodeRecord(rec)

	// In enc: offset 21..22 is KeyLen (2B = 1), key is at 23, ValLen is at 24..27
	valLenOffset := 23 + len(rec.Key)
	binary.PutUint32(enc[valLenOffset:valLenOffset+4], binary.MaxValueLen+100)

	dir := t.TempDir()
	path := filepath.Join(dir, "oversized_len.log")
	if err := os.WriteFile(path, enc, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on oversized length, got nil")
	}
	if !stdErrors.Is(err, errors.ErrValueTooLarge) {
		t.Errorf("expected ErrValueTooLarge, got %v", err)
	}
}

// TestReader_WrongFileType verifies Requirement 14:
// Attempting to open a directory path as a WALReader fails with *errors.NotADirectoryError.
func TestReader_WrongFileType(t *testing.T) {
	dir := t.TempDir()
	_, err := wal.OpenReader(dir)
	if err == nil {
		t.Fatalf("expected error opening directory as reader, got nil")
	}
	var notDirErr *errors.NotADirectoryError
	if !stdErrors.As(err, &notDirErr) {
		t.Errorf("expected *errors.NotADirectoryError, got %v", err)
	}
}

// TestReader_MissingFile verifies Requirement 15:
// Opening a non-existent WAL segment preserves fs.ErrNotExist.
func TestReader_MissingFile(t *testing.T) {
	dir := t.TempDir()
	missingPath := filepath.Join(dir, "missing_segment.log")

	_, err := wal.OpenReader(missingPath)
	if err == nil {
		t.Fatalf("expected error opening missing file, got nil")
	}
	if !stdErrors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected error wrapping fs.ErrNotExist, got %v", err)
	}
}

// TestReader_ReaderClose verifies Requirement 16:
// Calling Close() is idempotent, and subsequent Next() calls return ErrReaderClosed.
func TestReader_ReaderClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal.log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("first Close failed: %v", err)
	}

	// Idempotent second close
	if err := r.Close(); err != nil {
		t.Fatalf("second Close must return nil, got %v", err)
	}

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected error on Next() after Close(), got nil")
	}
	if !stdErrors.Is(err, errors.ErrReaderClosed) {
		t.Errorf("expected ErrReaderClosed, got %v", err)
	}
}

// TestReader_ReopenReRead verifies Requirement 17:
// Reading a file completely, closing it, and opening a second reader returns exact same records.
func TestReader_ReopenReRead(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, _ := wal.OpenSegmentWriter(dbPath, 6)
	rec1 := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 10, Key: []byte("k1"), Value: []byte("v1")}
	rec2 := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 20, Key: []byte("k2"), Value: []byte("v2")}
	_ = w.AppendSync(rec1)
	_ = w.AppendSync(rec2)
	_ = w.Close()

	// Pass 1
	r1, err := wal.OpenSegmentReader(dbPath, 6)
	if err != nil {
		t.Fatalf("OpenSegmentReader 1 failed: %v", err)
	}
	g1A, _ := r1.Next()
	g1B, _ := r1.Next()
	_ = r1.Close()

	// Pass 2
	r2, err := wal.OpenSegmentReader(dbPath, 6)
	if err != nil {
		t.Fatalf("OpenSegmentReader 2 failed: %v", err)
	}
	defer func() { _ = r2.Close() }()

	g2A, _ := r2.Next()
	g2B, _ := r2.Next()

	if !g1A.Equal(g2A) {
		t.Errorf("first record mismatch across readers")
	}
	if !g1B.Equal(g2B) {
		t.Errorf("second record mismatch across readers")
	}
}

// TestReader_ReaderDoesNotModifyFile verifies Requirement 18:
// The reader performs strictly read-only operations and does not alter file length or contents.
func TestReader_ReaderDoesNotModifyFile(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, _ := wal.OpenSegmentWriter(dbPath, 7)
	for i := 0; i < 10; i++ {
		_ = w.AppendSync(wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(i + 1),
			Timestamp: uint64(i),
			Key:       []byte(fmt.Sprintf("key_%d", i)),
			Value:     []byte(fmt.Sprintf("val_%d", i)),
		})
	}
	_ = w.Close()

	initialBytes, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	initialHash := sha256.Sum256(initialBytes)

	r, err := wal.OpenSegmentReader(dbPath, 7)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	for {
		_, err := r.Next()
		if stdErrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}
	_ = r.Close()

	afterBytes, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatalf("ReadFile after read failed: %v", err)
	}
	afterHash := sha256.Sum256(afterBytes)

	if initialHash != afterHash || len(initialBytes) != len(afterBytes) {
		t.Fatalf("WAL file modified by reader! Must be strictly read-only.")
	}
}

// TestReader_RecordOwnership verifies Requirement 19:
// Returned records have independent byte memory allocations that do not alias across Next() calls.
func TestReader_RecordOwnership(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, _ := wal.OpenSegmentWriter(dbPath, 8)
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 1, Key: []byte("alpha"), Value: []byte("val_a")})
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 2, Key: []byte("bravo"), Value: []byte("val_b")})
	_ = w.Close()

	r, err := wal.OpenSegmentReader(dbPath, 8)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	rec1, err := r.Next()
	if err != nil {
		t.Fatalf("rec1 Next failed: %v", err)
	}
	rec2, err := r.Next()
	if err != nil {
		t.Fatalf("rec2 Next failed: %v", err)
	}

	// Mutate rec1's memory
	rec1.Key[0] = 'X'
	rec1.Value[0] = 'Z'

	// Ensure rec2 is completely unaffected
	if string(rec2.Key) != "bravo" || string(rec2.Value) != "val_b" {
		t.Fatalf("memory aliasing detected between returned records: rec2 changed to key=%s val=%s", rec2.Key, rec2.Value)
	}
}

// TestReader_OffsetAccounting verifies Requirement 21:
// For N records of varying sizes, cumulative encoded lengths equal the final reader offset.
func TestReader_OffsetAccounting(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, _ := wal.OpenSegmentWriter(dbPath, 9)
	var expectedCumulative int64
	for i := 0; i < 20; i++ {
		key := []byte(fmt.Sprintf("key_%d", i))
		val := make([]byte, i*17)
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(i + 1),
			Timestamp: uint64(i),
			Key:       key,
			Value:     val,
		}
		_ = w.AppendSync(rec)
		expectedCumulative += int64(wal.MinRecordSize + len(key) + len(val))
	}
	_ = w.Close()

	r, err := wal.OpenSegmentReader(dbPath, 9)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	for {
		_, err := r.Next()
		if stdErrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
	}

	if r.Offset() != expectedCumulative {
		t.Errorf("final offset mismatch: got %d, want %d (drift detected)", r.Offset(), expectedCumulative)
	}
}

// TestReader_LargeRecordStream verifies Requirement 22:
// Sequentially stream and decode 1,000 valid records; final offset equals physical file size.
func TestReader_LargeRecordStream(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 10)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}

	const numRecords = 1000
	records := make([]wal.Record, numRecords)
	for i := 0; i < numRecords; i++ {
		records[i] = wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(i + 1),
			Timestamp: uint64(1725800000 + i),
			Key:       []byte(fmt.Sprintf("user_key_%06d", i)),
			Value:     []byte(fmt.Sprintf("user_val_%06d_payload_block", i)),
		}
		if err := w.AppendSync(records[i]); err != nil {
			t.Fatalf("AppendSync %d failed: %v", i, err)
		}
	}
	_ = w.Close()

	fi, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	fileSize := fi.Size()

	r, err := wal.OpenSegmentReader(dbPath, 10)
	if err != nil {
		t.Fatalf("OpenSegmentReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	count := 0
	for {
		rec, err := r.Next()
		if stdErrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("record %d Next failed: %v", count, err)
		}
		if !records[count].Equal(rec) {
			t.Fatalf("record %d mismatch: got %+v, want %+v", count, rec, records[count])
		}
		count++
	}

	if count != numRecords {
		t.Errorf("expected %d records, got %d", numRecords, count)
	}
	if r.Offset() != fileSize {
		t.Errorf("final offset %d does not equal file size %d", r.Offset(), fileSize)
	}
}

// TestReader_TornTailTestIsolation verifies Requirement: Torn-Tail Test Isolation
// Stream: valid A || valid B || partial C.
// Reader must return A, then B, then truncation error.
// The file MUST remain unchanged: reader must NEVER truncate the file.
func TestReader_TornTailTestIsolation(t *testing.T) {
	recA := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("kA"), Value: []byte("vA")}
	recB := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("kB"), Value: []byte("vB")}
	recC := wal.Record{Type: wal.RecordTypePut, SeqNum: 3, Timestamp: 102, Key: []byte("kC"), Value: []byte("vC_long_val")}

	encA, _ := wal.EncodeRecord(recA)
	encB, _ := wal.EncodeRecord(recB)
	encC, _ := wal.EncodeRecord(recC)

	// Incomplete record C (only 12 bytes of header)
	partialC := encC[:12]

	var combined []byte
	combined = append(combined, encA...)
	combined = append(combined, encB...)
	combined = append(combined, partialC...)

	dir := t.TempDir()
	path := filepath.Join(dir, "torn_tail.log")
	if err := os.WriteFile(path, combined, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	initialLen := int64(len(combined))

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	// Return A
	gotA, err := r.Next()
	if err != nil {
		t.Fatalf("Next A failed: %v", err)
	}
	if !recA.Equal(gotA) {
		t.Errorf("recA mismatch")
	}

	// Return B
	gotB, err := r.Next()
	if err != nil {
		t.Fatalf("Next B failed: %v", err)
	}
	if !recB.Equal(gotB) {
		t.Errorf("recB mismatch")
	}

	expectedOffsetAfterB := int64(len(encA) + len(encB))
	if r.Offset() != expectedOffsetAfterB {
		t.Errorf("offset after B mismatch: got %d, want %d", r.Offset(), expectedOffsetAfterB)
	}

	// Next on partial C must return truncation error
	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected truncation error on partial C, got nil")
	}
	if !stdErrors.Is(err, errors.ErrHeaderTruncated) && !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected ErrHeaderTruncated or ErrUnexpectedEOF, got %v", err)
	}

	// Offset must remain at start of C
	if r.Offset() != expectedOffsetAfterB {
		t.Errorf("offset must remain %d, got %d", expectedOffsetAfterB, r.Offset())
	}

	// Critical Architectural Invariant: Reader MUST NOT truncate the file!
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if fi.Size() != initialLen {
		t.Fatalf("WAL file was truncated by reader! Initial size was %d, but file is now %d. Reader must never truncate.", initialLen, fi.Size())
	}
}

// TestReader_SymlinkRejected verifies that attempting to open a symlink returns an error.
func TestReader_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real_target.log")
	if err := os.WriteFile(target, []byte("wal_data"), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	symlink := filepath.Join(dir, "symlink_reader.log")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	_, err := wal.OpenReader(symlink)
	if err == nil {
		t.Fatalf("expected error opening symlink as reader, got nil")
	}
}

// TestReader_PathSafety tests path accessor and empty path rejection.
func TestReader_PathSafety(t *testing.T) {
	t.Run("empty path rejected", func(t *testing.T) {
		_, err := wal.OpenReader("")
		if err == nil {
			t.Fatalf("expected error on empty path, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})

	t.Run("Path accessor", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "wal_000000000001.log")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		r, err := wal.OpenReader(path)
		if err != nil {
			t.Fatalf("OpenReader failed: %v", err)
		}
		defer func() { _ = r.Close() }()

		if r.Path() != path {
			t.Errorf("Path() mismatch: got %q, want %q", r.Path(), path)
		}
	})
}

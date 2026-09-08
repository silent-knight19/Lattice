package wal_test

import (
	"crypto/sha256"
	stdErrors "errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// helper to create a valid record
func makeValidRecord(seq uint64, key string, val string) wal.Record {
	return wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: 1700000000 + seq,
		Key:       []byte(key),
		Value:     []byte(val),
	}
}

// helper to append bytes to file
func appendBytesToFile(t *testing.T, path string, p []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(p); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
}

// helper to append a record to file and return bytes written
func appendRecordToFile(t *testing.T, path string, rec wal.Record) int {
	t.Helper()
	buf, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	appendBytesToFile(t, path, buf)
	return len(buf)
}

// 1. EMPTY FILE: 0 bytes -> no truncation, success, final size = 0
func TestRecovery_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment on empty file returned error: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false on empty file, got true")
	}
	if res.ValidRecords != 0 {
		t.Errorf("expected ValidRecords=0, got %d", res.ValidRecords)
	}
	if res.RecoveredOffset != 0 {
		t.Errorf("expected RecoveredOffset=0, got %d", res.RecoveredOffset)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("expected file size 0, got %d", info.Size())
	}
}

// 2. ONE VALID RECORD: unchanged size, readable afterward, exact same record
func TestRecovery_OneValidRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "key1", "val1")
	n := appendRecordToFile(t, path, rec)

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false on clean file, got true")
	}
	if res.ValidRecords != 1 {
		t.Errorf("expected ValidRecords=1, got %d", res.ValidRecords)
	}
	if res.RecoveredOffset != int64(n) {
		t.Errorf("expected RecoveredOffset=%d, got %d", n, res.RecoveredOffset)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != int64(n) {
		t.Errorf("expected size %d, got %d", n, info.Size())
	}

	// Verify readable via WALReader
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	readRec, err := r.Next()
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if !readRec.Equal(rec) {
		t.Errorf("read record does not match written record")
	}
	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

// 3. MANY VALID RECORDS: exact size preserved, all records preserved byte-for-byte
func TestRecovery_ManyValidRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	records := make([]wal.Record, 10)
	var totalBytes int64
	for i := range records {
		records[i] = makeValidRecord(uint64(i+1), fmt.Sprintf("key_%d", i), fmt.Sprintf("value_%d", i))
		totalBytes += int64(appendRecordToFile(t, path, records[i]))
	}

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false, got true")
	}
	if res.ValidRecords != 10 {
		t.Errorf("expected ValidRecords=10, got %d", res.ValidRecords)
	}
	if res.RecoveredOffset != totalBytes {
		t.Errorf("expected RecoveredOffset=%d, got %d", totalBytes, res.RecoveredOffset)
	}

	// Verify all records byte-for-byte via reader
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	for i := range records {
		readRec, err := r.Next()
		if err != nil {
			t.Fatalf("Next record %d failed: %v", i, err)
		}
		if !readRec.Equal(records[i]) {
			t.Errorf("record %d mismatch", i)
		}
	}
	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

// 4. CLEAN EOF AFTER MAX-SIZED RECORD: ensure no false torn-tail classification
func TestRecovery_CleanEOFAfterMaxSizedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	maxKey := make([]byte, binary.MaxKeyLen)
	for i := range maxKey {
		maxKey[i] = 'K'
	}
	maxVal := make([]byte, 1024*1024) // 1 MiB value
	for i := range maxVal {
		maxVal[i] = 'V'
	}
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 1700000001,
		Key:       maxKey,
		Value:     maxVal,
	}
	n := appendRecordToFile(t, path, rec)

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false for clean max-sized record, got true")
	}
	if res.ValidRecords != 1 {
		t.Errorf("expected ValidRecords=1, got %d", res.ValidRecords)
	}
	if res.RecoveredOffset != int64(n) {
		t.Errorf("expected RecoveredOffset=%d, got %d", n, res.RecoveredOffset)
	}
}

// 5. ONE-BYTE TAIL: append 1 byte after valid record -> truncates exactly that byte
func TestRecovery_OneByteTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "validKey", "validVal")
	validBytes := appendRecordToFile(t, path, rec)

	// Append 1 garbage byte
	appendBytesToFile(t, path, []byte{0xFF})

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected Truncated=true, got false")
	}
	if res.ValidRecords != 1 {
		t.Errorf("expected ValidRecords=1, got %d", res.ValidRecords)
	}
	if res.RecoveredOffset != int64(validBytes) {
		t.Errorf("expected RecoveredOffset=%d, got %d", validBytes, res.RecoveredOffset)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() != int64(validBytes) {
		t.Errorf("expected truncated size %d, got %d", validBytes, info.Size())
	}

	// Verify reader reads 1 record then clean EOF
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	readRec, err := r.Next()
	if err != nil {
		t.Fatalf("Next failed: %v", err)
	}
	if !readRec.Equal(rec) {
		t.Errorf("recovered record mismatch")
	}
	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

// 6. EVERY HEADER PREFIX LENGTH 1..20: for each possible number of header bytes, truncate back
func TestRecovery_EveryHeaderPrefixLength1To20(t *testing.T) {
	for n := 1; n <= 20; n++ {
		t.Run(fmt.Sprintf("header_prefix_%d_bytes", n), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wal_000000000001.log")
			rec := makeValidRecord(1, "key1", "val1")
			validLen := appendRecordToFile(t, path, rec)

			// Next record encoded, but only write n bytes of its header
			rec2 := makeValidRecord(2, "key2", "val2")
			buf2, err := wal.EncodeRecord(rec2)
			if err != nil {
				t.Fatalf("EncodeRecord failed: %v", err)
			}
			appendBytesToFile(t, path, buf2[:n])

			res, err := wal.RecoverSegment(path)
			if err != nil {
				t.Fatalf("RecoverSegment failed for prefix length %d: %v", n, err)
			}
			if !res.Truncated {
				t.Errorf("expected Truncated=true for prefix %d", n)
			}
			if res.ValidRecords != 1 {
				t.Errorf("expected ValidRecords=1 for prefix %d, got %d", n, res.ValidRecords)
			}
			if res.RecoveredOffset != int64(validLen) {
				t.Errorf("expected RecoveredOffset=%d, got %d", validLen, res.RecoveredOffset)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat failed: %v", err)
			}
			if info.Size() != int64(validLen) {
				t.Errorf("expected file size %d, got %d", validLen, info.Size())
			}
		})
	}
}

// 7. TRUNCATED KEY: create valid record and remove bytes from key payload
func TestRecovery_TruncatedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "key1", "val1")
	validLen := appendRecordToFile(t, path, rec1)

	// Encode rec2 with 10-byte key, truncate inside key bytes
	rec2 := makeValidRecord(2, "long_key_01", "val2")
	buf2, _ := wal.EncodeRecord(rec2)
	// HeaderSize (21) + keyLen (2) = 23. Key is 11 bytes. Truncate at 23 + 5 = 28 bytes.
	truncatedBuf := buf2[:28]
	appendBytesToFile(t, path, truncatedBuf)

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated || res.ValidRecords != 1 || res.RecoveredOffset != int64(validLen) {
		t.Errorf("unexpected result: %+v", res)
	}

	info, _ := os.Stat(path)
	if info.Size() != int64(validLen) {
		t.Errorf("expected size %d, got %d", validLen, info.Size())
	}
}

// 8. TRUNCATED VALUE: create valid record and remove bytes from value payload
func TestRecovery_TruncatedValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "key1", "val1")
	validLen := appendRecordToFile(t, path, rec1)

	rec2 := makeValidRecord(2, "key2", "long_value_for_testing_truncation")
	buf2, _ := wal.EncodeRecord(rec2)
	// Truncate 5 bytes before the end of the record
	truncatedBuf := buf2[:len(buf2)-5]
	appendBytesToFile(t, path, truncatedBuf)

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated || res.ValidRecords != 1 || res.RecoveredOffset != int64(validLen) {
		t.Errorf("unexpected result: %+v", res)
	}

	info, _ := os.Stat(path)
	if info.Size() != int64(validLen) {
		t.Errorf("expected size %d, got %d", validLen, info.Size())
	}
}

// 9. TRUNCATED KEY LENGTH FIELD: 21 bytes header + 1 byte of key length
func TestRecovery_TruncatedKeyLengthField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "key1", "val1")
	validLen := appendRecordToFile(t, path, rec1)

	rec2 := makeValidRecord(2, "key2", "val2")
	buf2, _ := wal.EncodeRecord(rec2)
	// HeaderSize (21) + 1 byte of key length = 22 bytes
	truncatedBuf := buf2[:22]
	appendBytesToFile(t, path, truncatedBuf)

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated || res.ValidRecords != 1 || res.RecoveredOffset != int64(validLen) {
		t.Errorf("unexpected result: %+v", res)
	}
}

// 10. TRUNCATED VALUE LENGTH FIELD: 21B header + 2B keyLen + key + 1..3 bytes of valLen
func TestRecovery_TruncatedValueLengthField(t *testing.T) {
	for extra := 1; extra <= 3; extra++ {
		t.Run(fmt.Sprintf("valLen_extra_%d_bytes", extra), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wal_000000000001.log")
			rec1 := makeValidRecord(1, "key1", "val1")
			validLen := appendRecordToFile(t, path, rec1)

			rec2 := makeValidRecord(2, "k2", "v2")
			buf2, _ := wal.EncodeRecord(rec2)
			// HeaderSize (21) + 2B keyLen + 2B key ("k2") = 25B. Then extra bytes of valLen (4B)
			truncatedBuf := buf2[:25+extra]
			appendBytesToFile(t, path, truncatedBuf)

			res, err := wal.RecoverSegment(path)
			if err != nil {
				t.Fatalf("RecoverSegment failed: %v", err)
			}
			if !res.Truncated || res.ValidRecords != 1 || res.RecoveredOffset != int64(validLen) {
				t.Errorf("unexpected result: %+v", res)
			}
		})
	}
}

// 11. COMPLETE CHECKSUM-CORRUPTED RECORD AT EOF:
// A fully formed record with a bad CRC is NOT automatically a torn tail.
// Expected: DO NOT truncate, return corruption error
func TestRecovery_CompleteChecksumCorruptedRecordAtEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "key1", "val1")
	appendRecordToFile(t, path, rec1)

	// Complete rec2 with corrupted payload byte
	rec2 := makeValidRecord(2, "key2", "val2")
	buf2, _ := wal.EncodeRecord(rec2)
	buf2[len(buf2)-1] ^= 0xFF // corrupt value byte
	appendBytesToFile(t, path, buf2)

	statBefore, _ := os.Stat(path)

	res, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected ChecksumMismatchError, got nil")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false on complete corrupt record, got true")
	}

	// Verify file size was NOT truncated or modified
	statAfter, _ := os.Stat(path)
	if statAfter.Size() != statBefore.Size() {
		t.Errorf("file was mutated: before=%d, after=%d", statBefore.Size(), statAfter.Size())
	}
}

// 12. COMPLETE INVALID RECORD TYPE AT EOF:
// Expected: DO NOT truncate, return structural corruption error
func TestRecovery_CompleteInvalidRecordTypeAtEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "key1", "val1")
	appendRecordToFile(t, path, rec1)

	// Complete 21-byte header with invalid record type 0x99
	var badHeader [wal.HeaderSize]byte
	badHeader[4] = 0x99 // invalid type
	appendBytesToFile(t, path, badHeader[:])

	statBefore, _ := os.Stat(path)

	res, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected InvalidRecordTypeError, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
		t.Errorf("expected ErrInvalidRecordType, got: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false, got true")
	}

	statAfter, _ := os.Stat(path)
	if statAfter.Size() != statBefore.Size() {
		t.Errorf("file was mutated: before=%d, after=%d", statBefore.Size(), statAfter.Size())
	}
}

// 13. MIDDLE CHECKSUM CORRUPTION: A -> corrupt B -> C
// Expected: no truncation, return corruption, C must never be treated as valid continuation
func TestRecovery_MiddleChecksumCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	recA := makeValidRecord(1, "keyA", "valA")
	appendRecordToFile(t, path, recA)

	recB := makeValidRecord(2, "keyB", "valB")
	bufB, _ := wal.EncodeRecord(recB)
	bufB[len(bufB)-1] ^= 0xFF // corrupt B's value
	appendBytesToFile(t, path, bufB)

	recC := makeValidRecord(3, "keyC", "valC")
	appendRecordToFile(t, path, recC)

	statBefore, _ := os.Stat(path)

	res, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected error on middle corruption, got nil")
	}
	if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false on middle corruption, got true")
	}
	if res.ValidRecords != 1 {
		t.Errorf("expected ValidRecords=1, got %d", res.ValidRecords)
	}

	statAfter, _ := os.Stat(path)
	if statAfter.Size() != statBefore.Size() {
		t.Errorf("file size was mutated: before=%d, after=%d", statBefore.Size(), statAfter.Size())
	}
}

// 14. MIDDLE STRUCTURAL CORRUPTION: A -> invalid type B -> C
// Expected: no truncation, return structural error, fail closed
func TestRecovery_MiddleStructuralCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	recA := makeValidRecord(1, "keyA", "valA")
	appendRecordToFile(t, path, recA)

	var badHeader [wal.HeaderSize]byte
	badHeader[4] = 0x00 // RecordTypeInvalid
	appendBytesToFile(t, path, badHeader[:])

	recC := makeValidRecord(3, "keyC", "valC")
	appendRecordToFile(t, path, recC)

	statBefore, _ := os.Stat(path)

	res, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected error on middle structural corruption, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
		t.Errorf("expected ErrInvalidRecordType, got: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false, got true")
	}

	statAfter, _ := os.Stat(path)
	if statAfter.Size() != statBefore.Size() {
		t.Errorf("file size was mutated: before=%d, after=%d", statBefore.Size(), statAfter.Size())
	}
}

// 15. VALID PREFIX IMMUTABILITY: calculate hash of valid prefix before recovery;
// after recovery, file bytes equal valid prefix bytes byte-for-byte.
func TestRecovery_ValidPrefixImmutability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "key1", "val1")
	rec2 := makeValidRecord(2, "key2", "val2")
	appendRecordToFile(t, path, rec1)
	appendRecordToFile(t, path, rec2)

	// Capture SHA256 of valid prefix
	prefixBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	expectedHash := sha256.Sum256(prefixBytes)

	// Append a 7-byte torn tail
	appendBytesToFile(t, path, []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03})

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected Truncated=true")
	}

	// Verify post-recovery file bytes have identical hash to prefixBytes
	recoveredBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	actualHash := sha256.Sum256(recoveredBytes)

	if actualHash != expectedHash {
		t.Errorf("hash mismatch: valid prefix was altered during recovery")
	}
}

// 16. TRUNCATION SIZE EXACTNESS: verify final file size == expected valid prefix offset
func TestRecovery_TruncationSizeExactness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec1 := makeValidRecord(1, "k1", "v1")
	rec2 := makeValidRecord(2, "k2", "v2")
	n1 := appendRecordToFile(t, path, rec1)
	n2 := appendRecordToFile(t, path, rec2)
	expectedValidSize := int64(n1 + n2)

	// Append 13 bytes of torn header
	appendBytesToFile(t, path, make([]byte, 13))

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if res.RecoveredOffset != expectedValidSize {
		t.Errorf("expected RecoveredOffset=%d, got %d", expectedValidSize, res.RecoveredOffset)
	}

	info, _ := os.Stat(path)
	if info.Size() != expectedValidSize {
		t.Errorf("expected physical file size=%d, got %d", expectedValidSize, info.Size())
	}
}

// 17. RECOVERY IDEMPOTENCE: run recovery twice on torn WAL
// Run 1: truncates tail
// Run 2: clean EOF, no additional mutation, success
func TestRecovery_RecoveryIdempotence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "key1", "val1")
	validLen := appendRecordToFile(t, path, rec)

	// Append torn tail
	appendBytesToFile(t, path, []byte{0x01, 0x02, 0x03})

	// Run 1: truncates
	res1, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("Run 1 failed: %v", err)
	}
	if !res1.Truncated || res1.ValidRecords != 1 || res1.RecoveredOffset != int64(validLen) {
		t.Errorf("unexpected Run 1 result: %+v", res1)
	}

	// Run 2: clean EOF, no mutation
	res2, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("Run 2 failed: %v", err)
	}
	if res2.Truncated {
		t.Errorf("expected Run 2 Truncated=false, got true")
	}
	if res2.ValidRecords != 1 || res2.RecoveredOffset != int64(validLen) {
		t.Errorf("unexpected Run 2 result: %+v", res2)
	}
}

// 18. CLEAN FILE IDEMPOTENCE: run recovery twice on clean file -> both no-op successes
func TestRecovery_CleanFileIdempotence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "cleanKey", "cleanVal")
	validLen := appendRecordToFile(t, path, rec)

	res1, err := wal.RecoverSegment(path)
	if err != nil || res1.Truncated || res1.RecoveredOffset != int64(validLen) {
		t.Fatalf("Run 1 failed: res=%+v, err=%v", res1, err)
	}

	res2, err := wal.RecoverSegment(path)
	if err != nil || res2.Truncated || res2.RecoveredOffset != int64(validLen) {
		t.Fatalf("Run 2 failed: res=%+v, err=%v", res2, err)
	}
}

// 19. READABILITY AFTER RECOVERY: reopen with WALReader and verify clean stream to io.EOF
func TestRecovery_ReadabilityAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	recs := []wal.Record{
		makeValidRecord(1, "k1", "v1"),
		makeValidRecord(2, "k2", "v2"),
		makeValidRecord(3, "k3", "v3"),
	}
	for _, r := range recs {
		appendRecordToFile(t, path, r)
	}

	// Append torn tail
	appendBytesToFile(t, path, []byte{0xAA, 0xBB, 0xCC, 0xDD})

	if _, err := wal.RecoverSegment(path); err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	for i, expected := range recs {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("record %d Next failed: %v", i, err)
		}
		if !got.Equal(expected) {
			t.Errorf("record %d mismatch", i)
		}
	}

	_, err = r.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected clean io.EOF, got %v", err)
	}
}

// 20. NO RECORD REORDERING: ensure recovery does not alter record sequence or values
func TestRecovery_NoRecordReordering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	count := 25
	for i := 1; i <= count; i++ {
		appendRecordToFile(t, path, makeValidRecord(uint64(i), fmt.Sprintf("k%03d", i), fmt.Sprintf("v%03d", i)))
	}

	// Append torn tail
	appendBytesToFile(t, path, []byte{0x12, 0x34})

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if res.ValidRecords != count {
		t.Fatalf("expected ValidRecords=%d, got %d", count, res.ValidRecords)
	}

	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	for i := 1; i <= count; i++ {
		rec, err := r.Next()
		if err != nil {
			t.Fatalf("Next %d failed: %v", i, err)
		}
		if rec.SeqNum != binary.SeqNum(i) {
			t.Errorf("expected SeqNum %d, got %d", i, rec.SeqNum)
		}
		if string(rec.Key) != fmt.Sprintf("k%03d", i) {
			t.Errorf("expected key k%03d, got %s", i, string(rec.Key))
		}
	}
}

// 21. PERMISSION FAILURE: read-only file where write/truncate is denied
func TestRecovery_PermissionFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "k1", "v1")
	appendRecordToFile(t, path, rec)

	// Append torn tail
	appendBytesToFile(t, path, []byte{0x01})

	// Make file read-only
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}
	defer func() { _ = os.Chmod(path, 0600) }()

	_, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected error on read-only file, got nil")
	}
	if !stdErrors.Is(err, fs.ErrPermission) {
		t.Logf("permission error: %v (note: matches OS permission error)", err)
	}
}

// 22. MISSING FILE: preserve fs.ErrNotExist
func TestRecovery_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_nonexistent.log")

	_, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected error on missing file, got nil")
	}
	if !stdErrors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got: %v", err)
	}
}

// 23. DIRECTORY: reject directories with NotADirectoryError
func TestRecovery_Directory(t *testing.T) {
	dir := t.TempDir()

	_, err := wal.RecoverSegment(dir)
	if err == nil {
		t.Fatalf("expected error for directory, got nil")
	}
	var notDirErr *errors.NotADirectoryError
	if !stdErrors.As(err, &notDirErr) {
		t.Errorf("expected *errors.NotADirectoryError, got: %v", err)
	}
	if !stdErrors.Is(err, errors.ErrNotADirectory) {
		t.Errorf("expected ErrNotADirectory match, got: %v", err)
	}
}

// 24. SYMLINK: reject symbolic links
func TestRecovery_Symlink(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "wal_real.log")
	symlinkPath := filepath.Join(dir, "wal_symlink.log")

	rec := makeValidRecord(1, "k1", "v1")
	appendRecordToFile(t, realPath, rec)

	if err := os.Symlink(realPath, symlinkPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	_, err := wal.RecoverSegment(symlinkPath)
	if err == nil {
		t.Fatalf("expected error on symlink, got nil")
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid, got: %v", err)
	}
}

// 25. INODE REPLACEMENT / FILE SWAP DEFENSE
func TestRecovery_EmptyPath(t *testing.T) {
	_, err := wal.RecoverSegment("")
	if err == nil {
		t.Fatalf("expected error on empty path, got nil")
	}
	if !stdErrors.Is(err, os.ErrInvalid) {
		t.Errorf("expected os.ErrInvalid on empty path, got: %v", err)
	}
}

// 26. LARGE PREFIX: 500 records + torn tail -> exact truncation
func TestRecovery_LargePrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	count := 500
	var totalValidBytes int64

	for i := 1; i <= count; i++ {
		rec := makeValidRecord(uint64(i), fmt.Sprintf("key_%05d", i), fmt.Sprintf("val_%05d", i))
		n := appendRecordToFile(t, path, rec)
		totalValidBytes += int64(n)
	}

	// Append 11 bytes of a torn header
	appendBytesToFile(t, path, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B})

	res, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected Truncated=true")
	}
	if res.ValidRecords != count {
		t.Errorf("expected ValidRecords=%d, got %d", count, res.ValidRecords)
	}
	if res.RecoveredOffset != totalValidBytes {
		t.Errorf("expected RecoveredOffset=%d, got %d", totalValidBytes, res.RecoveredOffset)
	}

	info, _ := os.Stat(path)
	if info.Size() != totalValidBytes {
		t.Errorf("expected physical size %d, got %d", totalValidBytes, info.Size())
	}
}

// 27. RECORD BOUNDARY MATRIX: test torn tails across all 4 operational record types
func TestRecovery_AllRecordTypesTornTail(t *testing.T) {
	types := []struct {
		name string
		rec  wal.Record
	}{
		{"PUT", wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")}},
		{"DELETE", wal.Record{Type: wal.RecordTypeDelete, SeqNum: 2, Timestamp: 200, Key: []byte("k")}},
		{"BATCH_START", wal.Record{Type: wal.RecordTypeBatchStart, SeqNum: 3, Timestamp: 300}},
		{"BATCH_COMMIT", wal.Record{Type: wal.RecordTypeBatchCommit, SeqNum: 4, Timestamp: 400}},
	}

	for _, tc := range types {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "wal_000000000001.log")
			recValid := makeValidRecord(1, "baseKey", "baseVal")
			validLen := appendRecordToFile(t, path, recValid)

			// Append partial record of this type
			buf, err := wal.EncodeRecord(tc.rec)
			if err != nil {
				t.Fatalf("EncodeRecord failed: %v", err)
			}
			appendBytesToFile(t, path, buf[:len(buf)/2]) // half written

			res, err := wal.RecoverSegment(path)
			if err != nil {
				t.Fatalf("RecoverSegment failed: %v", err)
			}
			if !res.Truncated || res.ValidRecords != 1 || res.RecoveredOffset != int64(validLen) {
				t.Errorf("unexpected recovery result for %s: %+v", tc.name, res)
			}
		})
	}
}

// 28. RANDOMIZED TAIL LENGTHS: deterministic seeded test truncating at random byte offsets
func TestRecovery_RandomizedTailLengths(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	for iter := 0; iter < 20; iter++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "wal_000000000001.log")
		numRecs := rng.Intn(5) + 3 // 3..7 records
		offsets := make([]int64, numRecs+1)
		offsets[0] = 0

		for i := 0; i < numRecs; i++ {
			rec := makeValidRecord(uint64(i+1), fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
			n := appendRecordToFile(t, path, rec)
			offsets[i+1] = offsets[i] + int64(n)
		}

		fullSize := offsets[numRecs]
		// Choose a random truncation point in (0..fullSize]
		truncateAt := int64(rng.Intn(int(fullSize))) + 1

		// Truncate the file physically to simulate crash
		if err := os.Truncate(path, truncateAt); err != nil {
			t.Fatalf("Truncate failed: %v", err)
		}

		// Determine which boundary this falls on
		var expectedValidRecords int
		var expectedOffset int64
		for i := 0; i < len(offsets); i++ {
			if offsets[i] <= truncateAt {
				expectedValidRecords = i
				expectedOffset = offsets[i]
			} else {
				break
			}
		}

		res, err := wal.RecoverSegment(path)
		if err != nil {
			t.Fatalf("iteration %d failed: %v (truncatedAt=%d, expectedValid=%d, expectedOffset=%d)",
				iter, err, truncateAt, expectedValidRecords, expectedOffset)
		}

		if res.ValidRecords != expectedValidRecords {
			t.Errorf("iter %d: expected ValidRecords=%d, got %d (truncateAt=%d)",
				iter, expectedValidRecords, res.ValidRecords, truncateAt)
		}
		if res.RecoveredOffset != expectedOffset {
			t.Errorf("iter %d: expected RecoveredOffset=%d, got %d (truncateAt=%d)",
				iter, expectedOffset, res.RecoveredOffset, truncateAt)
		}

		// If truncateAt was exactly on a boundary, Truncated should be false, else true
		expectedTruncated := (truncateAt != expectedOffset)
		if res.Truncated != expectedTruncated {
			t.Errorf("iter %d: expected Truncated=%v, got %v", iter, expectedTruncated, res.Truncated)
		}
	}
}

// 29. NO SILENT REPAIR: verify file is never mutated when corruption is middle corruption
func TestRecovery_NoSilentRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	recA := makeValidRecord(1, "keyA", "valA")
	recB := makeValidRecord(2, "keyB", "valB")
	recC := makeValidRecord(3, "keyC", "valC")
	appendRecordToFile(t, path, recA)

	// Corrupt B
	bufB, _ := wal.EncodeRecord(recB)
	bufB[len(bufB)-2] ^= 0x55
	appendBytesToFile(t, path, bufB)

	appendRecordToFile(t, path, recC)

	bytesBefore, _ := os.ReadFile(path)

	res, err := wal.RecoverSegment(path)
	if err == nil {
		t.Fatalf("expected corruption error, got nil")
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false, got true")
	}

	bytesAfter, _ := os.ReadFile(path)
	if string(bytesBefore) != string(bytesAfter) {
		t.Errorf("file contents were modified on middle corruption!")
	}
}

// 30. TEST SEAMS: truncate failure injected
func TestRecovery_TruncateFailureSeam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "k1", "v1")
	appendRecordToFile(t, path, rec)

	// Append torn tail
	appendBytesToFile(t, path, []byte{0x01, 0x02})

	injectedErr := stdErrors.New("injected truncate error")
	failingTruncate := func(f *os.File, size int64) error {
		return injectedErr
	}
	normalSync := func(f *os.File) error {
		return f.Sync()
	}

	res, err := wal.RecoverSegmentWithSeamsForTesting(path, normalSync, failingTruncate)
	if err == nil {
		t.Fatalf("expected error from failing truncate, got nil")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected injected error, got: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected Truncated=false on truncate failure, got true")
	}
}

// 31. TEST SEAMS: sync failure injected
// Truncate succeeds, but sync fails -> Truncated must be true (mutation occurred),
// error must be returned, and physical file size must confirm truncation actually occurred.
func TestRecovery_SyncFailureSeam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "k1", "v1")
	validLen := appendRecordToFile(t, path, rec)

	// Append torn tail
	appendBytesToFile(t, path, []byte{0x01, 0x02})

	injectedErr := stdErrors.New("injected sync error")
	normalTruncate := func(f *os.File, size int64) error {
		return f.Truncate(size)
	}
	failingSync := func(f *os.File) error {
		return injectedErr
	}

	res, err := wal.RecoverSegmentWithSeamsForTesting(path, failingSync, normalTruncate)
	if err == nil {
		t.Fatalf("expected error from failing sync, got nil")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected injected error, got: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected Truncated=true because physical truncation occurred before sync failed, got false")
	}

	// Verify physical file size confirms truncation actually occurred on disk
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("Stat failed: %v", statErr)
	}
	if info.Size() != int64(validLen) {
		t.Errorf("expected physical file size %d confirming truncation, got %d", validLen, info.Size())
	}
}

// 32. RECOVER SEGMENT BY ID: test helper
func TestRecovery_RecoverSegmentByID(t *testing.T) {
	dbDir := t.TempDir()
	if _, err := wal.InitDir(dbDir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	segPath := wal.SegmentPath(dbDir, 1)
	rec := makeValidRecord(1, "keyByID", "valByID")
	validLen := appendRecordToFile(t, segPath, rec)

	// Append torn tail
	appendBytesToFile(t, segPath, []byte{0x99, 0x88})

	res, err := wal.RecoverSegmentByID(dbDir, 1)
	if err != nil {
		t.Fatalf("RecoverSegmentByID failed: %v", err)
	}
	if !res.Truncated || res.ValidRecords != 1 || res.RecoveredOffset != int64(validLen) {
		t.Errorf("unexpected result: %+v", res)
	}
}

// 33. TEST SEAMS: post-truncation verification failure
// Truncate succeeds, sync succeeds, but post-truncation size verification fails (e.g. file size mismatch)
// -> Truncated must remain true, error must be non-nil.
func TestRecovery_PostTruncationVerificationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal_000000000001.log")
	rec := makeValidRecord(1, "k1", "v1")
	appendRecordToFile(t, path, rec)

	// Append torn tail
	appendBytesToFile(t, path, []byte{0x01, 0x02})

	// Truncate function pretends to succeed (returns nil) without actually truncating the file,
	// causing postStat.Size() != validOffset to fail during post-condition verification.
	noOpTruncate := func(f *os.File, size int64) error {
		return nil
	}
	normalSync := func(f *os.File) error {
		return f.Sync()
	}

	res, err := wal.RecoverSegmentWithSeamsForTesting(path, normalSync, noOpTruncate)
	if err == nil {
		t.Fatalf("expected error from post-truncation verification failure, got nil")
	}
	if !res.Truncated {
		t.Errorf("expected Truncated=true because Truncate succeeded before verification failed, got false")
	}
}

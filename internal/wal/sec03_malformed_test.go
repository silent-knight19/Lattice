package wal_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

func TestSEC03_Malformed_01_HeaderCorruption(t *testing.T) {
	h := NewSecurityHarness(t)
	segPath := h.CreateSegmentWithRecords(1, 1, 3)

	// Corrupt header magic/CRC of record 2 (offset > 0)
	// Record 1 wire size: MinRecordSize (27) + len("k-1-0") [5] + len("v-1-0") [5] = 37 bytes
	rec1Size := int64(wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0")))

	// Flip a bit in the CRC (bytes 0..3 of record 2)
	if err := h.CorruptByteAt(segPath, rec1Size+1, 0x01); err != nil {
		t.Fatalf("CorruptByteAt failed: %v", err)
	}

	// 1. WALReader must detect checksum mismatch when reaching record 2
	r, err := wal.OpenReader(segPath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	rec1, err := r.Next()
	if err != nil || rec1.SeqNum != 1 {
		t.Fatalf("expected rec 1 valid, got: %v, err: %v", rec1, err)
	}

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected CRC corruption error on record 2")
	}
	var crcErr *latticeErrors.ChecksumMismatchError
	if !errors.As(err, &crcErr) {
		t.Errorf("expected ChecksumMismatchError, got: %T (%v)", err, err)
	}

	// 2. Recovery on this segment must fail closed because record 2 is a complete corrupt record, NOT a torn EOF tail
	_, err = wal.RecoverSegment(segPath)
	if err == nil {
		t.Fatalf("expected RecoverSegment to fail closed on CRC mismatch")
	}
}

func TestSEC03_Malformed_02_LengthCorruption(t *testing.T) {
	// Construct a record with encoded wire bytes, then mutate length fields
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(100),
		Timestamp: 1700000000000,
		Key:       []byte("test-key"),
		Value:     []byte("test-val"),
	}

	raw, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Wire layout: [0..3: CRC] [4..11: SeqNum] [12..19: Timestamp] [20: Type] [21..22: KeyLen] [23..26: ValLen]
	// Corrupt KeyLen to 0xFFFF (65535) while buffer only has 8 bytes of key
	corrupted := make([]byte, len(raw))
	copy(corrupted, raw)
	binary.PutUint16(corrupted[21:23], 0xFFFF)
	// Recompute CRC for the corrupted payload so the length parser itself is exercised
	corruptedPayloadCRC := binary.Checksum(corrupted[4:])
	binary.PutUint32(corrupted[0:4], corruptedPayloadCRC)

	// DecodeRecord must return ErrTruncatedRecord (or unexpected EOF) rather than panicking or allocating 64KB
	_, err = wal.DecodeRecord(bytes.NewReader(corrupted))
	if err == nil {
		t.Fatalf("expected DecodeRecord to reject truncated payload")
	}
}

func TestSEC03_Malformed_03_TypeCorruption(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1),
		Timestamp: 1700000000000,
		Key:       []byte("k"),
		Value:     []byte("v"),
	}

	raw, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Mutate Type byte (offset 4) to 0x00 (RecordTypeInvalid)
	corrupted := make([]byte, len(raw))
	copy(corrupted, raw)
	corrupted[4] = byte(wal.RecordTypeInvalid)
	binary.PutUint32(corrupted[0:4], binary.Checksum(corrupted[4:]))

	_, err = wal.DecodeRecord(bytes.NewReader(corrupted))
	if err == nil {
		t.Fatalf("expected DecodeRecord to reject RecordTypeInvalid")
	}
	var typeErr *latticeErrors.InvalidRecordTypeError
	if !errors.As(err, &typeErr) {
		t.Errorf("expected InvalidRecordTypeError, got: %T (%v)", err, err)
	}

	// Mutate Type byte to 0xFF (out of range)
	corrupted[4] = 0xFF
	binary.PutUint32(corrupted[0:4], binary.Checksum(corrupted[4:]))
	_, err = wal.DecodeRecord(bytes.NewReader(corrupted))
	if err == nil {
		t.Fatalf("expected DecodeRecord to reject Type 0xFF")
	}
}

func TestSEC03_Malformed_04_ChecksumCorruption(t *testing.T) {
	h := NewSecurityHarness(t)
	segPath := h.CreateSegmentWithRecords(1, 1, 1)

	// Test zeroed CRC
	f, _ := os.OpenFile(segPath, os.O_RDWR, 0600)
	_, _ = f.WriteAt([]byte{0x00, 0x00, 0x00, 0x00}, 0)
	_ = f.Close()

	r, err := wal.OpenReader(segPath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = r.Close() }()

	_, err = r.Next()
	if err == nil {
		t.Fatalf("expected ChecksumMismatchError on zeroed CRC")
	}
}

func TestSEC03_Malformed_05_SequenceCorruption(t *testing.T) {
	h := NewSecurityHarness(t)

	// Create segment 1 with SeqNum 1, 2
	seg1 := h.CreateSegmentWithRecords(1, 1, 2)

	// Create segment 2 with a REGRESSING SeqNum (e.g. SeqNum 1 again instead of 3)
	seg2 := wal.SegmentPath(h.RootDir(), 2)
	w, err := wal.CreateWriter(seg2)
	if err != nil {
		t.Fatalf("CreateWriter seg2: %v", err)
	}
	// Write regression record: SeqNum 1 <= SeqNum 2
	_ = w.Append(h.MakeRecord(1, "k-regress", "v-regress"))
	_ = w.Sync()
	_ = w.Close()

	// RecoverWAL must detect non-monotonic sequence numbers and fail closed
	_, err = wal.RecoverWAL(h.RootDir(), nil)
	if err == nil {
		t.Fatalf("expected RecoverWAL to reject sequence regression")
	}
	var seqErr *latticeErrors.SequenceOutOfOrderError
	if !errors.As(err, &seqErr) {
		t.Errorf("expected SequenceOutOfOrderError, got: %T (%v)", err, err)
	}

	// Verify physical segment files were not mutated
	info1, _ := os.Stat(seg1)
	info2, _ := os.Stat(seg2)
	if info1.Size() == 0 || info2.Size() == 0 {
		t.Errorf("segment files should not be truncated on sequence error")
	}
}

func TestSEC03_Malformed_06_TruncatedKeyAndValue(t *testing.T) {
	h := NewSecurityHarness(t)

	rec := h.MakeRecord(1, "my-long-key-string", "my-long-value-string")
	raw, _ := wal.EncodeRecord(rec)

	// 1. Truncated mid-key: keep header + 5 bytes of key
	partialKey := raw[:wal.MinRecordSize+5]
	seg1 := filepath.Join(h.WALDir(), "wal_000000000001.log")
	_ = os.MkdirAll(h.WALDir(), 0700)
	_ = os.WriteFile(seg1, partialKey, 0600)

	// Latest segment containing torn tail at EOF is safely truncated to 0 bytes
	res, err := wal.RecoverSegment(seg1)
	if err != nil {
		t.Fatalf("RecoverSegment on torn key tail failed: %v", err)
	}
	if !res.Truncated || res.RecoveredOffset != 0 {
		t.Errorf("expected truncated to 0, got: %+v", res)
	}

	// 2. Truncated mid-value: keep header + full key + 3 bytes of value
	fullKeyLen := len(rec.Key)
	partialVal := raw[:wal.MinRecordSize+fullKeyLen+3]
	seg2 := filepath.Join(h.WALDir(), "wal_000000000002.log")
	_ = os.WriteFile(seg2, partialVal, 0600)

	res, err = wal.RecoverSegment(seg2)
	if err != nil {
		t.Fatalf("RecoverSegment on torn val tail failed: %v", err)
	}
	if !res.Truncated || res.RecoveredOffset != 0 {
		t.Errorf("expected truncated to 0, got: %+v", res)
	}
}

func TestSEC03_Malformed_07_MissingHeaderBytes(t *testing.T) {
	h := NewSecurityHarness(t)
	_ = os.MkdirAll(h.WALDir(), 0700)

	// Write only 10 bytes (< 21 bytes header)
	seg := filepath.Join(h.WALDir(), "wal_000000000001.log")
	_ = os.WriteFile(seg, []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A}, 0600)

	// RecoverSegment should truncate the 10 incomplete header bytes to 0
	res, err := wal.RecoverSegment(seg)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !res.Truncated || res.RecoveredOffset != 0 {
		t.Errorf("expected truncated to 0, got: %+v", res)
	}

	afterInfo, _ := os.Stat(seg)
	if afterInfo.Size() != 0 {
		t.Errorf("expected physical file size 0 after truncating torn header, got %d", afterInfo.Size())
	}
}

func TestSEC03_Malformed_08_EnormousDeclaredLengths(t *testing.T) {
	// Construct framing with 1-byte key and declared 2GB value length:
	// Wire order: [21B header] [2B keyLen=1] [1B key="k"] [4B valLen=2GB]
	var buf bytes.Buffer

	var header [wal.HeaderSize]byte
	header[4] = byte(wal.RecordTypePut)
	binary.PutUint64(header[5:13], 1)
	binary.PutUint64(header[13:21], 1700000000000)
	buf.Write(header[:])

	var keyLenBuf [2]byte
	binary.PutUint16(keyLenBuf[:], 1)
	buf.Write(keyLenBuf[:])
	buf.WriteByte('k')

	var valLenBuf [4]byte
	binary.PutUint32(valLenBuf[:], 2147483648) // 2GB
	buf.Write(valLenBuf[:])

	// DecodeRecord must reject this early with ValueTooLargeError without allocating 2GB
	_, err := wal.DecodeRecord(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Fatalf("expected DecodeRecord to reject 2GB length")
	}
	var lenErr *latticeErrors.ValueTooLargeError
	if !errors.As(err, &lenErr) {
		t.Errorf("expected ValueTooLargeError, got %T (%v)", err, err)
	}
}

func TestSEC03_Malformed_09_MiddleCorruptionFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	// Create segment with 5 valid records: R1, R2, R3, R4, R5
	segPath := h.CreateSegmentWithRecords(1, 1, 5)

	// Corrupt middle record R3 (offset of R1 + R2)
	recWireSize := wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0"))
	r3Offset := recWireSize * 2

	// Flip bits in R3 payload
	if err := h.CorruptByteAt(segPath, r3Offset+25, 0xFF); err != nil {
		t.Fatalf("CorruptByteAt: %v", err)
	}

	// RecoverSegment MUST fail closed! It must NOT truncate back to R2 and discard R4/R5!
	_, err := wal.RecoverSegment(segPath)
	if err == nil {
		t.Fatalf("expected RecoverSegment to fail closed on middle corruption")
	}

	// Verify file was NOT mutated/truncated
	info, _ := os.Stat(segPath)
	if info.Size() != recWireSize*5 {
		t.Fatalf("middle corruption caused unauthorized file truncation! size=%d, want=%d", info.Size(), recWireSize*5)
	}
}

func TestSEC03_Malformed_10_CorruptionsAtExactBoundaries(t *testing.T) {
	h := NewSecurityHarness(t)
	segPath := h.CreateSegmentWithRecords(1, 1, 2)
	rec1Size := wal.RecordWireSize(h.MakeRecord(1, "k-1-0", "v-1-0"))

	// 1. Truncate file at exact boundary of Record 1 (file size == rec1Size)
	_ = os.Truncate(segPath, rec1Size)
	res, err := wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment on clean boundary failed: %v", err)
	}
	if res.Truncated {
		t.Errorf("expected clean boundary to NOT be truncated")
	}
	if res.ValidRecords != 1 {
		t.Errorf("expected exactly 1 valid record, got %d", res.ValidRecords)
	}

	// 2. Truncate file at exact boundary of Record 1 + 1 byte (1 extra garbage byte at EOF)
	_ = h.AppendRawBytes(segPath, []byte{0xEE})
	res, err = wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment on 1 extra byte failed: %v", err)
	}
	if !res.Truncated || res.RecoveredOffset != rec1Size {
		t.Errorf("expected 1-byte torn tail to be truncated to rec1Size (%d), got %+v", rec1Size, res)
	}
}

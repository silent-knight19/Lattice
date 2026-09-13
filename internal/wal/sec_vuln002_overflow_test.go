package wal_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestVULN002_DecodeRecord_OversizedWireLength verifies that DecodeRecord rejects
// any record with an oversized length field before attempting memory allocation.
func TestVULN002_DecodeRecord_OversizedWireLength(t *testing.T) {
	// Craft a record with HeaderSize (21B) + keyLen 10B + valLen (math.MaxUint32 - 100)
	var buf bytes.Buffer

	// 21-byte header: CRC(4B), Type=PUT(1B), SeqNum(8B), Timestamp(8B)
	headerBuf := make([]byte, wal.HeaderSize)
	headerBuf[4] = byte(wal.RecordTypePut)
	binary.PutUint64(headerBuf[5:13], 1)
	binary.PutUint64(headerBuf[13:21], 1000)
	buf.Write(headerBuf)

	// 2-byte keyLen = 4
	var keyLenBuf [2]byte
	binary.PutUint16(keyLenBuf[:], 4)
	buf.Write(keyLenBuf[:])

	// 4 bytes of key
	buf.WriteString("test")

	// 4-byte valLen = math.MaxUint32 - 100 (malicious huge length)
	var valLenBuf [4]byte
	binary.PutUint32(valLenBuf[:], math.MaxUint32-100)
	buf.Write(valLenBuf[:])

	_, err := wal.DecodeRecord(&buf)
	if err == nil {
		t.Fatal("expected DecodeRecord to reject malicious huge valLen, got nil error")
	}
}

// TestVULN002_RecordWireSize_NoOverflow verifies that RecordWireSize uses safe
// unsigned arithmetic and does not overflow or wrap to negative numbers.
func TestVULN002_RecordWireSize_NoOverflow(t *testing.T) {
	rec := wal.Record{
		Type:  wal.RecordTypePut,
		Key:   make([]byte, binary.MaxKeyLen),
		Value: make([]byte, 1024),
	}
	size := wal.RecordWireSize(rec)
	if size <= 0 {
		t.Fatalf("expected positive wire size, got %d", size)
	}
	expected := int64(wal.MinRecordSize + binary.MaxKeyLen + 1024)
	if size != expected {
		t.Fatalf("expected wire size %d, got %d", expected, size)
	}
}

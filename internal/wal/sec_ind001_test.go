package wal_test

import (
	"bytes"
	stdErrors "errors"
	"io"
	"os"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestIND001_TwoPhaseStaging_ChecksumMismatch verifies that a record with an invalid
// checksum is rejected during Phase 2 validation with ChecksumMismatchError.
func TestIND001_TwoPhaseStaging_ChecksumMismatch(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 1000,
		Key:       []byte("test-key"),
		Value:     []byte("test-value"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Corrupt the CRC field (first 4 bytes)
	encoded[0] ^= 0xFF

	r := bytes.NewReader(encoded)
	_, err = wal.DecodeRecord(r)
	if err == nil {
		t.Fatalf("expected DecodeRecord to fail on corrupted checksum")
	}

	var csErr *errors.ChecksumMismatchError
	if !stdErrors.As(err, &csErr) {
		t.Fatalf("expected ChecksumMismatchError, got: %T (%v)", err, err)
	}
}

// TestIND001_TwoPhaseStaging_TruncatedPayload verifies that a truncated payload (torn record)
// cleanly returns io.ErrUnexpectedEOF and cannot cause partial state mutation.
func TestIND001_TwoPhaseStaging_TruncatedPayload(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    2,
		Timestamp: 2000,
		Key:       []byte("truncate-key"),
		Value:     []byte("truncate-value-data"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Test truncation at every possible byte boundary after byte 0
	for cut := 1; cut < len(encoded); cut++ {
		truncated := encoded[:cut]
		r := bytes.NewReader(truncated)
		_, err := wal.DecodeRecord(r)
		if err == nil {
			t.Fatalf("cut at %d: expected error on truncated record, got nil", cut)
		}
		if !stdErrors.Is(err, io.ErrUnexpectedEOF) && !stdErrors.Is(err, errors.ErrHeaderTruncated) {
			t.Fatalf("cut at %d: expected ErrHeaderTruncated or ErrUnexpectedEOF, got: %v", cut, err)
		}
	}
}

// TestIND001_TornTailAtEOFRecovery verifies that a WAL file with a torn tail at EOF
// is safely truncated to the last valid record boundary by RecoverSegment without panic or corruption.
func TestIND001_TornTailAtEOFRecovery(t *testing.T) {
	dir := t.TempDir()
	if _, err := wal.InitDir(dir); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}
	segPath := wal.SegmentPath(dir, 1)

	w, err := wal.CreateWriter(segPath)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}

	r1 := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("k1"),
		Value:     []byte("v1"),
	}
	r2 := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    2,
		Timestamp: 200,
		Key:       []byte("k2"),
		Value:     []byte("v2"),
	}

	if err := w.Append(r1); err != nil {
		t.Fatalf("Append r1: %v", err)
	}
	if err := w.Append(r2); err != nil {
		t.Fatalf("Append r2: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cleanStat, err := os.Stat(segPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	cleanSize := cleanStat.Size()

	// Append torn bytes (partial header of record 3)
	f, err := os.OpenFile(segPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("OpenFile append: %v", err)
	}
	tornHeader := []byte{0x12, 0x34, 0x56, 0x78, byte(wal.RecordTypePut), 0x00, 0x00}
	if _, err := f.Write(tornHeader); err != nil {
		t.Fatalf("Write tornHeader: %v", err)
	}
	_ = f.Close()

	// Execute recovery
	res, err := wal.RecoverSegment(segPath)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}

	if !res.Truncated {
		t.Fatalf("expected Truncated == true")
	}
	if res.ValidRecords != 2 {
		t.Fatalf("expected 2 valid records, got %d", res.ValidRecords)
	}
	if res.RecoveredOffset != cleanSize {
		t.Fatalf("expected RecoveredOffset %d, got %d", cleanSize, res.RecoveredOffset)
	}

	// Verify reader can replay cleanly to EOF
	reader, err := wal.OpenReader(segPath)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer reader.Close()

	replayed1, err := reader.Next()
	if err != nil || !replayed1.Equal(r1) {
		t.Fatalf("replayed r1 mismatch: %v (err: %v)", replayed1, err)
	}
	replayed2, err := reader.Next()
	if err != nil || !replayed2.Equal(r2) {
		t.Fatalf("replayed r2 mismatch: %v (err: %v)", replayed2, err)
	}
	_, err = reader.Next()
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected clean io.EOF after record 2, got: %v", err)
	}
}

// TestIND001_AttackScenario_TruncatedRecordWithHeaderChecksum verifies that an adversary
// crafting a torn record at EOF with header-only data cannot bypass validation.
func TestIND001_AttackScenario_TruncatedRecordWithHeaderChecksum(t *testing.T) {
	// Create a header for a record declaring 100-byte key and 500-byte value
	var headerBuf [wal.HeaderSize]byte
	header := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(10),
		Timestamp: 12345,
	}
	wal.EncodeHeader(headerBuf[:], header)

	// Stream only contains headerBuf and no payload
	r := bytes.NewReader(headerBuf[:])
	_, err := wal.DecodeRecord(r)
	if err == nil {
		t.Fatalf("expected DecodeRecord to reject header-only stream")
	}
	if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got: %v", err)
	}
}

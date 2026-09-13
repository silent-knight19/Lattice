package wal_test

import (
	"bytes"
	"io"
	"testing"

	stdErrors "errors"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// dripReader delivers at most one byte per Read call, simulating a
// maximally fragmented stream (network/partial-page reads).
type dripReader struct {
	buf []byte
}

func (d *dripReader) Read(p []byte) (int, error) {
	if len(d.buf) == 0 {
		return 0, io.EOF
	}
	p[0] = d.buf[0]
	d.buf = d.buf[1:]
	return 1, nil
}

func m004ValidEncoding(t *testing.T) []byte {
	t.Helper()
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    41,
		Timestamp: 1725800041,
		Key:       []byte("trunc-key"),
		Value:     []byte("trunc-value"),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	return encoded
}

// TestINDM004_TruncatedHeaderRejected proves the IND-M-004 verdict: a header
// cut mid-stream (10 of 21 bytes, then EOF) must be rejected with
// ErrHeaderTruncated — never parsed as a partial record, never CRC-accepted.
func TestINDM004_TruncatedHeaderRejected(t *testing.T) {
	encoded := m004ValidEncoding(t)

	for _, size := range []int{1, 10, wal.HeaderSize - 1} {
		truncated := bytes.NewReader(encoded[:size])
		_, err := wal.DecodeRecord(truncated)
		if err == nil {
			t.Fatalf("size %d: truncated header accepted as valid record", size)
		}
		if !stdErrors.Is(err, errors.ErrHeaderTruncated) {
			t.Errorf("size %d: want ErrHeaderTruncated, got %T (%v)", size, err, err)
		}
	}

	// Empty stream is a clean boundary (io.EOF), not a corrupt header.
	if _, err := wal.DecodeRecord(bytes.NewReader(nil)); !stdErrors.Is(err, io.EOF) {
		t.Errorf("empty stream: want io.EOF, got %v", err)
	}
}

// TestINDM004_FragmentedStreamDecodes proves the other half of the contract:
// extreme fragmentation must not cause false truncation — a valid record
// delivered one byte per read still decodes exactly.
func TestINDM004_FragmentedStreamDecodes(t *testing.T) {
	encoded := m004ValidEncoding(t)

	rec, err := wal.DecodeRecord(&dripReader{buf: append([]byte(nil), encoded...)})
	if err != nil {
		t.Fatalf("fragmented valid record rejected: %v", err)
	}
	if string(rec.Key) != "trunc-key" || string(rec.Value) != "trunc-value" {
		t.Errorf("fragmented decode mismatch: key=%q value=%q", rec.Key, rec.Value)
	}
	if uint64(rec.SeqNum) != 41 {
		t.Errorf("fragmented decode seqnum = %d, want 41", rec.SeqNum)
	}
}

// TestINDM004_TruncatedBodyRejected verifies truncation inside the body (key
// cut short) also fails closed rather than yielding a partial record.
func TestINDM004_TruncatedBodyRejected(t *testing.T) {
	encoded := m004ValidEncoding(t)
	cut := wal.HeaderSize + 2 + 3 // header + keyLen + 3 of 9 key bytes

	_, err := wal.DecodeRecord(bytes.NewReader(encoded[:cut]))
	if err == nil {
		t.Fatalf("truncated body accepted as valid record")
	}
	if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("want io.ErrUnexpectedEOF, got %T (%v)", err, err)
	}
}

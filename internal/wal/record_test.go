package wal_test

import (
	"bytes"
	stdErrors "errors"
	"io"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

func TestHeaderSizeConstants(t *testing.T) {
	if wal.HeaderSize != 21 {
		t.Fatalf("HeaderSize must be exactly 21 bytes, got %d", wal.HeaderSize)
	}
	if wal.RecordHeaderLen != 21 {
		t.Fatalf("RecordHeaderLen must be exactly 21 bytes, got %d", wal.RecordHeaderLen)
	}
}

func TestRecordTypeValidationAndMethods(t *testing.T) {
	tests := []struct {
		name          string
		rType         wal.RecordType
		wantValid     bool
		wantString    string
		wantOpType    binary.OpType
		wantOpTypeErr bool
	}{
		{
			name:          "Invalid/Zero",
			rType:         wal.RecordTypeInvalid,
			wantValid:     false,
			wantString:    "UNKNOWN(0x00)",
			wantOpType:    binary.OpTypeInvalid,
			wantOpTypeErr: true,
		},
		{
			name:          "Put",
			rType:         wal.RecordTypePut,
			wantValid:     true,
			wantString:    "PUT",
			wantOpType:    binary.OpTypePut,
			wantOpTypeErr: false,
		},
		{
			name:          "Delete",
			rType:         wal.RecordTypeDelete,
			wantValid:     true,
			wantString:    "DELETE",
			wantOpType:    binary.OpTypeDelete,
			wantOpTypeErr: false,
		},
		{
			name:          "BatchStart",
			rType:         wal.RecordTypeBatchStart,
			wantValid:     true,
			wantString:    "BATCH_START",
			wantOpType:    binary.OpTypeInvalid,
			wantOpTypeErr: true,
		},
		{
			name:          "BatchCommit",
			rType:         wal.RecordTypeBatchCommit,
			wantValid:     true,
			wantString:    "BATCH_COMMIT",
			wantOpType:    binary.OpTypeInvalid,
			wantOpTypeErr: true,
		},
		{
			name:          "Unknown byte 0x05",
			rType:         wal.RecordType(0x05),
			wantValid:     false,
			wantString:    "UNKNOWN(0x05)",
			wantOpType:    binary.OpTypeInvalid,
			wantOpTypeErr: true,
		},
		{
			name:          "Unknown byte 0xFF",
			rType:         wal.RecordType(0xFF),
			wantValid:     false,
			wantString:    "UNKNOWN(0xff)",
			wantOpType:    binary.OpTypeInvalid,
			wantOpTypeErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rType.Valid(); got != tc.wantValid {
				t.Errorf("Valid() = %v, want %v", got, tc.wantValid)
			}

			err := tc.rType.Validate()
			if tc.wantValid && err != nil {
				t.Errorf("Validate() returned unexpected error: %v", err)
			} else if !tc.wantValid && err == nil {
				t.Errorf("Validate() expected error, got nil")
			}

			if got := tc.rType.String(); got != tc.wantString {
				t.Errorf("String() = %q, want %q", got, tc.wantString)
			}

			op, opErr := tc.rType.OpType()
			if tc.wantOpTypeErr && opErr == nil {
				t.Errorf("OpType() expected error, got nil")
			} else if !tc.wantOpTypeErr && opErr != nil {
				t.Errorf("OpType() unexpected error: %v", opErr)
			}
			if op != tc.wantOpType {
				t.Errorf("OpType() = %v, want %v", op, tc.wantOpType)
			}

			// Test ParseRecordType
			parsed, parseErr := wal.ParseRecordType(byte(tc.rType))
			if tc.wantValid {
				if parseErr != nil {
					t.Errorf("ParseRecordType unexpected error: %v", parseErr)
				}
				if parsed != tc.rType {
					t.Errorf("ParseRecordType = %v, want %v", parsed, tc.rType)
				}
			} else {
				if parseErr == nil {
					t.Errorf("ParseRecordType expected error for %v, got nil", tc.rType)
				}
			}
		})
	}
}

func TestHeaderPhysicalByteAlignment(t *testing.T) {
	// Verify that each field occupies the exact offset and byte width specified in Section 18.1:
	//   Offset  0..3  : CRC32-IEEE (4 bytes)
	//   Offset  4     : RecordType (1 byte)
	//   Offset  5..12 : SeqNum     (8 bytes)
	//   Offset 13..20 : Timestamp  (8 bytes)
	h := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypeBatchStart,
		SeqNum:    binary.SeqNum(0x0102030405060708),
		Timestamp: 0x0A0B0C0D0E0F1011,
	}

	var buf [wal.HeaderSize]byte
	wal.EncodeHeader(buf[:], h)

	expected := []byte{
		// CRC32 (4 bytes)
		0x12, 0x34, 0x56, 0x78,
		// RecordType (1 byte: 0x03)
		0x03,
		// SeqNum (8 bytes)
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		// Timestamp (8 bytes)
		0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10, 0x11,
	}

	if !bytes.Equal(buf[:], expected) {
		t.Fatalf("Header byte layout mismatch:\ngot:  %#v\nwant: %#v", buf[:], expected)
	}

	// Verify decode reproduces exact original struct
	decoded, err := wal.DecodeHeader(buf[:])
	if err != nil {
		t.Fatalf("DecodeHeader failed: %v", err)
	}
	if !decoded.Equal(h) {
		t.Fatalf("Decoded header mismatch: got %+v, want %+v", decoded, h)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	testCases := []struct {
		name   string
		header wal.RecordHeader
	}{
		{
			name: "Zero/Minimum Values",
			header: wal.RecordHeader{
				CRC:       0,
				Type:      wal.RecordTypePut,
				SeqNum:    binary.MinSeqNum,
				Timestamp: 0,
			},
		},
		{
			name: "Maximum Values",
			header: wal.RecordHeader{
				CRC:       math.MaxUint32,
				Type:      wal.RecordTypeBatchCommit,
				SeqNum:    binary.MaxSeqNum,
				Timestamp: math.MaxUint64,
			},
		},
		{
			name: "Standard PUT Record",
			header: wal.RecordHeader{
				CRC:       0xEDB88320,
				Type:      wal.RecordTypePut,
				SeqNum:    1048576,
				Timestamp: uint64(time.Now().UnixNano()),
			},
		},
		{
			name: "Standard DELETE Record",
			header: wal.RecordHeader{
				CRC:       0xAABBCCDD,
				Type:      wal.RecordTypeDelete,
				SeqNum:    42,
				Timestamp: 1700000000000000000,
			},
		},
		{
			name: "Batch Start Marker",
			header: wal.RecordHeader{
				CRC:       0x00000001,
				Type:      wal.RecordTypeBatchStart,
				SeqNum:    99999999,
				Timestamp: 1720000000000000000,
			},
		},
		{
			name: "Batch Commit Marker",
			header: wal.RecordHeader{
				CRC:       0xCAFEBABE,
				Type:      wal.RecordTypeBatchCommit,
				SeqNum:    100000000,
				Timestamp: 1720000000000050000,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, wal.HeaderSize)
			wal.EncodeHeader(buf, tc.header)

			decoded, err := wal.DecodeHeader(buf)
			if err != nil {
				t.Fatalf("DecodeHeader failed: %v", err)
			}

			if !decoded.Equal(tc.header) {
				t.Errorf("Roundtrip mismatch:\ngot:  %+v\nwant: %+v", decoded, tc.header)
			}
		})
	}
}

func TestAppendHeader(t *testing.T) {
	h := wal.RecordHeader{
		CRC:       0xDEADBEEF,
		Type:      wal.RecordTypePut,
		SeqNum:    555,
		Timestamp: 123456789,
	}

	// Case 1: Append to nil slice
	buf1 := wal.AppendHeader(nil, h)
	if len(buf1) != wal.HeaderSize {
		t.Fatalf("expected len %d, got %d", wal.HeaderSize, len(buf1))
	}
	decoded1, err := wal.DecodeHeader(buf1)
	if err != nil || !decoded1.Equal(h) {
		t.Fatalf("decode of AppendHeader(nil) failed: %v, %+v", err, decoded1)
	}

	// Case 2: Append to existing slice
	prefix := []byte("prefix-bytes-")
	buf2 := wal.AppendHeader(prefix, h)
	if len(buf2) != len(prefix)+wal.HeaderSize {
		t.Fatalf("expected len %d, got %d", len(prefix)+wal.HeaderSize, len(buf2))
	}
	if !bytes.Equal(buf2[:len(prefix)], prefix) {
		t.Fatalf("prefix corrupted: %q", buf2[:len(prefix)])
	}
	decoded2, err := wal.DecodeHeader(buf2[len(prefix):])
	if err != nil || !decoded2.Equal(h) {
		t.Fatalf("decode of appended header failed: %v, %+v", err, decoded2)
	}
}

func TestDecodeHeaderTruncation(t *testing.T) {
	var validBuf [wal.HeaderSize]byte
	wal.EncodeHeader(validBuf[:], wal.RecordHeader{
		CRC:       1,
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 1,
	})

	// Every slice length from 0 up to 20 must return ErrHeaderTruncated
	for length := 0; length < wal.HeaderSize; length++ {
		subSlice := validBuf[:length]
		_, err := wal.DecodeHeader(subSlice)
		if err == nil {
			t.Fatalf("DecodeHeader with length %d expected error, got nil", length)
		}
		if !stdErrors.Is(err, errors.ErrHeaderTruncated) {
			t.Fatalf("DecodeHeader with length %d expected ErrHeaderTruncated, got %v", length, err)
		}
	}
}

func TestDecodeHeaderInvalidRecordType(t *testing.T) {
	var buf [wal.HeaderSize]byte
	wal.EncodeHeader(buf[:], wal.RecordHeader{
		CRC:       1,
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 1,
	})

	// Corrupt RecordType byte at index 4
	invalidTypes := []byte{0x00, 0x05, 0x10, 0x7F, 0xFF}
	for _, inv := range invalidTypes {
		buf[4] = inv
		_, err := wal.DecodeHeader(buf[:])
		if err == nil {
			t.Fatalf("expected error for RecordType 0x%02x, got nil", inv)
		}
		if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
			t.Fatalf("expected ErrInvalidRecordType for 0x%02x, got %v", inv, err)
		}

		var typed *errors.InvalidRecordTypeError
		if !stdErrors.As(err, &typed) {
			t.Fatalf("expected *errors.InvalidRecordTypeError, got %T", err)
		}
		if typed.Type != inv {
			t.Fatalf("expected typed.Type = 0x%02x, got 0x%02x", inv, typed.Type)
		}
	}
}

func TestEncodeHeaderEarlyBoundsCheckAntiTear(t *testing.T) {
	h := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    100,
		Timestamp: 200,
	}

	// Test undersized buffers: len 0 through 20.
	// In all cases, EncodeHeader must panic BEFORE writing any bytes to the buffer.
	for length := 0; length < wal.HeaderSize; length++ {
		buf := make([]byte, length)
		for i := range buf {
			buf[i] = 0xAA // Sentinel sentinel byte
		}

		panicked := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			wal.EncodeHeader(buf, h)
		}()

		if !panicked {
			t.Fatalf("EncodeHeader on buffer of length %d did not panic", length)
		}

		// Verify zero-tear invariant: none of the buffer bytes were modified
		for i, b := range buf {
			if b != 0xAA {
				t.Fatalf("Buffer byte at index %d was mutated to 0x%02x during torn panic!", i, b)
			}
		}
	}
}

func TestEncodeDecodeOversizedBuffer(t *testing.T) {
	h := wal.RecordHeader{
		CRC:       0xFEEDBEEF,
		Type:      wal.RecordTypeDelete,
		SeqNum:    123456,
		Timestamp: 987654321,
	}

	buf := make([]byte, 50)
	for i := range buf {
		buf[i] = 0x55 // Fill with canary bytes
	}

	wal.EncodeHeader(buf, h)

	// Trailing bytes from offset 21 to 49 must remain 0x55
	for i := wal.HeaderSize; i < len(buf); i++ {
		if buf[i] != 0x55 {
			t.Fatalf("Trailing byte at index %d was overwritten: got 0x%02x, want 0x55", i, buf[i])
		}
	}

	// DecodeHeader on the 50-byte buffer must succeed and ignore trailing bytes
	decoded, err := wal.DecodeHeader(buf)
	if err != nil {
		t.Fatalf("DecodeHeader failed on oversized buffer: %v", err)
	}
	if !decoded.Equal(h) {
		t.Fatalf("decoded header mismatch: got %+v, want %+v", decoded, h)
	}
}

func TestRecordHeaderStringAndEqual(t *testing.T) {
	h1 := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    10,
		Timestamp: 20,
	}
	h2 := h1
	if !h1.Equal(h2) {
		t.Errorf("identical headers must be Equal")
	}

	diffCRC := h1
	diffCRC.CRC = 0
	if h1.Equal(diffCRC) {
		t.Errorf("headers with different CRC must not be Equal")
	}

	diffType := h1
	diffType.Type = wal.RecordTypeDelete
	if h1.Equal(diffType) {
		t.Errorf("headers with different Type must not be Equal")
	}

	diffSeq := h1
	diffSeq.SeqNum = 999
	if h1.Equal(diffSeq) {
		t.Errorf("headers with different SeqNum must not be Equal")
	}

	diffTime := h1
	diffTime.Timestamp = 99999
	if h1.Equal(diffTime) {
		t.Errorf("headers with different Timestamp must not be Equal")
	}

	s := h1.String()
	if s == "" {
		t.Errorf("String() returned empty string")
	}
}

func TestConcurrentHeaderEncodeDecode(t *testing.T) {
	const goroutines = 64
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			var buf [wal.HeaderSize]byte
			for i := 0; i < iterations; i++ {
				h := wal.RecordHeader{
					CRC:       uint32(id*1000 + i),
					Type:      wal.RecordTypePut,
					SeqNum:    binary.SeqNum(id*10000 + i),
					Timestamp: uint64(time.Now().UnixNano()),
				}

				wal.EncodeHeader(buf[:], h)
				decoded, err := wal.DecodeHeader(buf[:])
				if err != nil {
					t.Errorf("concurrent DecodeHeader failed: %v", err)
					return
				}
				if !decoded.Equal(h) {
					t.Errorf("concurrent header mismatch: got %+v, want %+v", decoded, h)
					return
				}
			}
		}(g)
	}

	wg.Wait()
}

func FuzzRecordHeaderCodec(f *testing.F) {
	// Seed with valid headers
	validHeaders := []wal.RecordHeader{
		{CRC: 0, Type: wal.RecordTypePut, SeqNum: 0, Timestamp: 0},
		{CRC: 0xFFFFFFFF, Type: wal.RecordTypeDelete, SeqNum: binary.MaxSeqNum, Timestamp: math.MaxUint64},
		{CRC: 0x12345678, Type: wal.RecordTypeBatchStart, SeqNum: 100, Timestamp: 200},
		{CRC: 0x87654321, Type: wal.RecordTypeBatchCommit, SeqNum: 101, Timestamp: 201},
	}

	for _, h := range validHeaders {
		var buf [wal.HeaderSize]byte
		wal.EncodeHeader(buf[:], h)
		f.Add(buf[:])
	}

	// Add partial and corrupted seeds
	f.Add([]byte{})
	f.Add([]byte{0x01, 0x02})
	f.Add(bytes.Repeat([]byte{0x00}, 20))
	f.Add(bytes.Repeat([]byte{0xFF}, 21))
	f.Add(bytes.Repeat([]byte{0x01}, 30))

	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := wal.DecodeHeader(data)
		if err != nil {
			// Expected for truncated or invalid record types
			return
		}

		// If decode succeeded, re-encode and verify round-trip
		var reEncoded [wal.HeaderSize]byte
		wal.EncodeHeader(reEncoded[:], decoded)

		if !bytes.Equal(data[:wal.HeaderSize], reEncoded[:]) {
			t.Fatalf("Fuzz round-trip mismatch:\norig: %#v\nre:   %#v", data[:wal.HeaderSize], reEncoded[:])
		}
	})
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkEncodeHeader(b *testing.B) {
	h := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
	}
	var buf [wal.HeaderSize]byte
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		wal.EncodeHeader(buf[:], h)
	}
}

func BenchmarkDecodeHeader(b *testing.B) {
	h := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
	}
	var buf [wal.HeaderSize]byte
	wal.EncodeHeader(buf[:], h)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = wal.DecodeHeader(buf[:])
	}
}

func BenchmarkAppendHeader(b *testing.B) {
	h := wal.RecordHeader{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
	}
	buf := make([]byte, 0, wal.HeaderSize)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = wal.AppendHeader(buf[:0], h)
	}
}

func BenchmarkRecordType_Validate(b *testing.B) {
	t := wal.RecordTypePut
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = t.Validate()
	}
}

// ---------------------------------------------------------------------------
// Test Helper Readers for Stream Safety & Fragmentation
// ---------------------------------------------------------------------------

type singleByteReader struct {
	r io.Reader
}

func (s *singleByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var b [1]byte
	n, err := s.r.Read(b[:])
	if n > 0 {
		p[0] = b[0]
	}
	return n, err
}

type chunkReader struct {
	r         io.Reader
	chunkSize int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	toRead := len(p)
	if toRead > c.chunkSize {
		toRead = c.chunkSize
	}
	return c.r.Read(p[:toRead])
}

type shortReader struct {
	data []byte
	pos  int
	step int
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}
	rem := len(s.data) - s.pos
	toRead := s.step
	if toRead > len(p) {
		toRead = len(p)
	}
	if toRead > rem {
		toRead = rem
	}
	copy(p, s.data[s.pos:s.pos+toRead])
	s.pos += toRead
	return toRead, nil
}

type dataAndEOFReader struct {
	data []byte
	pos  int
}

func (d *dataAndEOFReader) Read(p []byte) (int, error) {
	if d.pos >= len(d.data) {
		return 0, io.EOF
	}
	n := copy(p, d.data[d.pos:])
	d.pos += n
	if d.pos >= len(d.data) {
		return n, io.EOF
	}
	return n, nil
}

type failingReader struct {
	data     []byte
	failAt   int
	pos      int
	injected error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= f.failAt {
		return 0, f.injected
	}
	toRead := len(p)
	if f.pos+toRead > f.failAt {
		toRead = f.failAt - f.pos
	}
	n := copy(p, f.data[f.pos:f.pos+toRead])
	f.pos += n
	if f.pos >= f.failAt {
		return n, f.injected
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Unit & Integration Tests for Full Record Codec
// ---------------------------------------------------------------------------

func TestMinRecordSizeConstant(t *testing.T) {
	if wal.MinRecordSize != 27 {
		t.Fatalf("MinRecordSize must be exactly 27 bytes, got %d", wal.MinRecordSize)
	}
}

func TestRecord_Validate(t *testing.T) {
	tests := []struct {
		name    string
		rec     wal.Record
		wantErr error
	}{
		{
			name: "Valid PUT with non-empty value",
			rec: wal.Record{
				Type:      wal.RecordTypePut,
				SeqNum:    1,
				Timestamp: 1000,
				Key:       []byte("test-key"),
				Value:     []byte("test-val"),
			},
			wantErr: nil,
		},
		{
			name: "Valid PUT with empty value",
			rec: wal.Record{
				Type:      wal.RecordTypePut,
				SeqNum:    1,
				Timestamp: 1000,
				Key:       []byte("test-key"),
				Value:     nil,
			},
			wantErr: nil,
		},
		{
			name: "Valid DELETE tombstone with nil value",
			rec: wal.Record{
				Type:      wal.RecordTypeDelete,
				SeqNum:    2,
				Timestamp: 1001,
				Key:       []byte("deleted-key"),
				Value:     nil,
			},
			wantErr: nil,
		},
		{
			name: "Valid DELETE tombstone with empty slice value",
			rec: wal.Record{
				Type:      wal.RecordTypeDelete,
				SeqNum:    2,
				Timestamp: 1001,
				Key:       []byte("deleted-key"),
				Value:     []byte{},
			},
			wantErr: nil,
		},
		{
			name: "Valid BATCH_START marker",
			rec: wal.Record{
				Type:      wal.RecordTypeBatchStart,
				SeqNum:    3,
				Timestamp: 1002,
				Key:       nil,
				Value:     nil,
			},
			wantErr: nil,
		},
		{
			name: "Valid BATCH_COMMIT marker",
			rec: wal.Record{
				Type:      wal.RecordTypeBatchCommit,
				SeqNum:    4,
				Timestamp: 1003,
				Key:       nil,
				Value:     nil,
			},
			wantErr: nil,
		},
		{
			name: "Invalid record type 0x00",
			rec: wal.Record{
				Type:   wal.RecordTypeInvalid,
				Key:    []byte("k"),
				Value:  []byte("v"),
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordType,
		},
		{
			name: "Invalid record type 0xFF",
			rec: wal.Record{
				Type:   wal.RecordType(0xFF),
				Key:    []byte("k"),
				Value:  []byte("v"),
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordType,
		},
		{
			name: "PUT with empty key",
			rec: wal.Record{
				Type:   wal.RecordTypePut,
				Key:    nil,
				Value:  []byte("v"),
				SeqNum: 1,
			},
			wantErr: errors.ErrEmptyKey,
		},
		{
			name: "DELETE with empty key",
			rec: wal.Record{
				Type:   wal.RecordTypeDelete,
				Key:    []byte{},
				Value:  nil,
				SeqNum: 1,
			},
			wantErr: errors.ErrEmptyKey,
		},
		{
			name: "DELETE with non-empty value",
			rec: wal.Record{
				Type:   wal.RecordTypeDelete,
				Key:    []byte("k"),
				Value:  []byte("unexpected-value"),
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordPayload,
		},
		{
			name: "BATCH_START with non-empty key",
			rec: wal.Record{
				Type:   wal.RecordTypeBatchStart,
				Key:    []byte("unexpected-key"),
				Value:  nil,
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordPayload,
		},
		{
			name: "BATCH_START with non-empty value",
			rec: wal.Record{
				Type:   wal.RecordTypeBatchStart,
				Key:    nil,
				Value:  []byte("unexpected-val"),
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordPayload,
		},
		{
			name: "BATCH_COMMIT with non-empty key",
			rec: wal.Record{
				Type:   wal.RecordTypeBatchCommit,
				Key:    []byte("unexpected-key"),
				Value:  nil,
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordPayload,
		},
		{
			name: "BATCH_COMMIT with non-empty value",
			rec: wal.Record{
				Type:   wal.RecordTypeBatchCommit,
				Key:    nil,
				Value:  []byte("unexpected-val"),
				SeqNum: 1,
			},
			wantErr: errors.ErrInvalidRecordPayload,
		},
		{
			name: "PUT with key exceeding MaxKeyLen",
			rec: wal.Record{
				Type:   wal.RecordTypePut,
				Key:    make([]byte, binary.MaxKeyLen+1),
				Value:  []byte("val"),
				SeqNum: 1,
			},
			wantErr: errors.ErrKeyTooLarge,
		},
		{
			name: "PUT with value exceeding MaxValueLen",
			rec: wal.Record{
				Type:   wal.RecordTypePut,
				Key:    []byte("k"),
				Value:  make([]byte, binary.MaxValueLen+1),
				SeqNum: 1,
			},
			wantErr: errors.ErrValueTooLarge,
		},
		{
			name: "DELETE with key exceeding MaxKeyLen",
			rec: wal.Record{
				Type:   wal.RecordTypeDelete,
				Key:    make([]byte, binary.MaxKeyLen+1),
				Value:  nil,
				SeqNum: 1,
			},
			wantErr: errors.ErrKeyTooLarge,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rec.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
			} else {
				if !stdErrors.Is(err, tc.wantErr) {
					t.Fatalf("expected error matching %v, got %v", tc.wantErr, err)
				}
			}
		})
	}
}

func TestRecord_HeaderAndEqualAndString(t *testing.T) {
	rec1 := wal.Record{
		CRC:       0xDEADBEEF,
		Type:      wal.RecordTypePut,
		SeqNum:    100,
		Timestamp: 200,
		Key:       []byte("alpha"),
		Value:     []byte("omega"),
	}

	hdr := rec1.Header()
	if hdr.CRC != rec1.CRC || hdr.Type != rec1.Type || hdr.SeqNum != rec1.SeqNum || hdr.Timestamp != rec1.Timestamp {
		t.Fatalf("Header() mismatch: got %+v, want %+v", hdr, rec1)
	}

	rec2 := rec1
	rec2.Key = []byte("alpha")
	rec2.Value = []byte("omega")
	if !rec1.Equal(rec2) {
		t.Fatalf("Equal() returned false for identical records")
	}

	// Different CRC
	diff := rec1
	diff.CRC++
	if rec1.Equal(diff) {
		t.Errorf("Equal() should be false on CRC mismatch")
	}

	// Different Type
	diff = rec1
	diff.Type = wal.RecordTypeDelete
	if rec1.Equal(diff) {
		t.Errorf("Equal() should be false on Type mismatch")
	}

	// Different SeqNum
	diff = rec1
	diff.SeqNum++
	if rec1.Equal(diff) {
		t.Errorf("Equal() should be false on SeqNum mismatch")
	}

	// Different Timestamp
	diff = rec1
	diff.Timestamp++
	if rec1.Equal(diff) {
		t.Errorf("Equal() should be false on Timestamp mismatch")
	}

	// Different Key
	diff = rec1
	diff.Key = []byte("beta")
	if rec1.Equal(diff) {
		t.Errorf("Equal() should be false on Key mismatch")
	}

	// Different Value
	diff = rec1
	diff.Value = []byte("zeta")
	if rec1.Equal(diff) {
		t.Errorf("Equal() should be false on Value mismatch")
	}

	// String() does not leak sensitive payload contents
	s := rec1.String()
	if bytes.Contains([]byte(s), []byte("alpha")) || bytes.Contains([]byte(s), []byte("omega")) {
		t.Errorf("String() leaked raw key/value payload: %s", s)
	}
	if !bytes.Contains([]byte(s), []byte("KeyLen=5")) || !bytes.Contains([]byte(s), []byte("ValLen=5")) {
		t.Errorf("String() missing length metadata: %s", s)
	}
}

// Requirement A: Minimal valid records
func TestEncodeDecode_MinimalValidRecords(t *testing.T) {
	// Minimal PUT: 1-byte key, 0-byte value
	putRec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("x"),
		Value:     nil,
	}
	encoded, err := wal.EncodeRecord(putRec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	expectedLen := wal.HeaderSize + 2 + 1 + 4 + 0 // 28 bytes
	if len(encoded) != expectedLen {
		t.Fatalf("expected encoded length %d, got %d", expectedLen, len(encoded))
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}
	if !putRec.Equal(decoded) {
		t.Fatalf("minimal PUT record round-trip mismatch:\nwant: %+v\ngot:  %+v", putRec, decoded)
	}

	// Minimal BATCH_START: 0-byte key, 0-byte value
	batchStart := wal.Record{
		Type:      wal.RecordTypeBatchStart,
		SeqNum:    2,
		Timestamp: 101,
	}
	encoded, err = wal.EncodeRecord(batchStart)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	if len(encoded) != wal.MinRecordSize { // 27 bytes
		t.Fatalf("expected batch start length %d, got %d", wal.MinRecordSize, len(encoded))
	}
	decoded, err = wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}
	if !batchStart.Equal(decoded) {
		t.Fatalf("minimal BATCH_START round-trip mismatch:\nwant: %+v\ngot:  %+v", batchStart, decoded)
	}

	// Minimal BATCH_COMMIT: 0-byte key, 0-byte value
	batchCommit := wal.Record{
		Type:      wal.RecordTypeBatchCommit,
		SeqNum:    3,
		Timestamp: 102,
	}
	encoded, err = wal.EncodeRecord(batchCommit)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	if len(encoded) != wal.MinRecordSize {
		t.Fatalf("expected batch commit length %d, got %d", wal.MinRecordSize, len(encoded))
	}
	decoded, err = wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}
	if !batchCommit.Equal(decoded) {
		t.Fatalf("minimal BATCH_COMMIT round-trip mismatch:\nwant: %+v\ngot:  %+v", batchCommit, decoded)
	}
}

// Requirement B: Maximum valid key (65,535 bytes)
func TestEncodeDecode_MaximumValidKey(t *testing.T) {
	maxKey := bytes.Repeat([]byte("K"), binary.MaxKeyLen)
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    555,
		Timestamp: 999999,
		Key:       maxKey,
		Value:     []byte("max-key-value"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed for max key: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed for max key: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("max key record mismatch after round trip")
	}
}

// Requirement C: Maximum valid value (4,194,304 bytes / 4 MiB)
func TestEncodeDecode_MaximumValidValue(t *testing.T) {
	maxVal := make([]byte, binary.MaxValueLen)
	for i := range maxVal {
		maxVal[i] = byte(i % 251)
	}
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    777,
		Timestamp: 1234567,
		Key:       []byte("4mb-key"),
		Value:     maxVal,
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed for 4MB value: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed for 4MB value: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("max value record mismatch after round trip")
	}
}

// Requirement D: Empty value handling
func TestEncodeDecode_EmptyValue(t *testing.T) {
	for _, val := range [][]byte{nil, {}} {
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    10,
			Timestamp: 200,
			Key:       []byte("empty-val-key"),
			Value:     val,
		}

		encoded, err := wal.EncodeRecord(rec)
		if err != nil {
			t.Fatalf("EncodeRecord failed: %v", err)
		}

		decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("DecodeRecord failed: %v", err)
		}

		if !rec.Equal(decoded) {
			t.Fatalf("empty value record mismatch")
		}
		if len(decoded.Value) != 0 {
			t.Fatalf("expected 0 value length, got %d", len(decoded.Value))
		}
	}
}

// Requirement E: Standard PUT record
func TestEncodeDecode_PutRecord(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    42,
		Timestamp: 1700000000000000000,
		Key:       []byte("user:1001:profile"),
		Value:     []byte(`{"name":"Alice","role":"admin"}`),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("PUT record round-trip mismatch")
	}
}

// Requirement F: DELETE record (tombstone)
func TestEncodeDecode_DeleteRecord(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypeDelete,
		SeqNum:    43,
		Timestamp: 1700000000000000001,
		Key:       []byte("user:1001:profile"),
		Value:     nil,
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("DELETE record round-trip mismatch")
	}

	// Forged DELETE with non-zero ValueLength in wire stream
	buf := make([]byte, len(encoded)+4)
	copy(buf, encoded)
	// Modify ValueLength from 0 to 1 at offset 23 + len(key)
	valOffset := 23 + len(rec.Key)
	binary.PutUint32(buf[valOffset:valOffset+4], 1)
	buf = append(buf, 0xFF) // append 1 byte of value
	// Recompute CRC so format validation is tested
	crc := binary.Checksum(buf[4:])
	binary.PutUint32(buf[0:4], crc)

	_, err = wal.DecodeRecord(bytes.NewReader(buf))
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Fatalf("expected ErrInvalidRecordPayload for forged DELETE with value, got %v", err)
	}
}

// Requirement G: BATCH_START
func TestEncodeDecode_BatchStart(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypeBatchStart,
		SeqNum:    100,
		Timestamp: 1700000000000000002,
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("BATCH_START round-trip mismatch")
	}

	// Forged BATCH_START with non-zero KeyLength in wire stream
	buf := make([]byte, len(encoded)+2)
	copy(buf, encoded)
	binary.PutUint16(buf[21:23], 2)
	buf = append(buf[:23], append([]byte("ab"), buf[23:]...)...)
	crc := binary.Checksum(buf[4:])
	binary.PutUint32(buf[0:4], crc)

	_, err = wal.DecodeRecord(bytes.NewReader(buf))
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Fatalf("expected ErrInvalidRecordPayload for forged BATCH_START with key, got %v", err)
	}

	// Forged BATCH_START with non-zero ValueLength in wire stream
	bufVal := make([]byte, len(encoded))
	copy(bufVal, encoded)
	binary.PutUint32(bufVal[23:27], 2)
	bufVal = append(bufVal, []byte("ef")...)
	crcVal := binary.Checksum(bufVal[4:])
	binary.PutUint32(bufVal[0:4], crcVal)

	_, err = wal.DecodeRecord(bytes.NewReader(bufVal))
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Fatalf("expected ErrInvalidRecordPayload for forged BATCH_START with value, got %v", err)
	}
}

// Requirement H: BATCH_COMMIT
func TestEncodeDecode_BatchCommit(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypeBatchCommit,
		SeqNum:    105,
		Timestamp: 1700000000000000003,
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("BATCH_COMMIT round-trip mismatch")
	}

	// Forged BATCH_COMMIT with non-zero ValueLength in wire stream
	buf := make([]byte, len(encoded))
	copy(buf, encoded)
	binary.PutUint32(buf[23:27], 2)
	buf = append(buf, []byte("cd")...)
	crc := binary.Checksum(buf[4:])
	binary.PutUint32(buf[0:4], crc)

	_, err = wal.DecodeRecord(bytes.NewReader(buf))
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Fatalf("expected ErrInvalidRecordPayload for forged BATCH_COMMIT with value, got %v", err)
	}

	// Forged BATCH_COMMIT with non-zero KeyLength in wire stream
	bufKey := make([]byte, len(encoded)+2)
	copy(bufKey, encoded)
	binary.PutUint16(bufKey[21:23], 2)
	bufKey = append(bufKey[:23], append([]byte("gh"), bufKey[23:]...)...)
	crcKey := binary.Checksum(bufKey[4:])
	binary.PutUint32(bufKey[0:4], crcKey)

	_, err = wal.DecodeRecord(bytes.NewReader(bufKey))
	if !stdErrors.Is(err, errors.ErrInvalidRecordPayload) {
		t.Fatalf("expected ErrInvalidRecordPayload for forged BATCH_COMMIT with key, got %v", err)
	}
}

// Requirement I: Minimum and maximum sequence numbers
func TestEncodeDecode_MinMaxSeqNum(t *testing.T) {
	for _, seq := range []binary.SeqNum{binary.MinSeqNum, binary.MaxSeqNum} {
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    seq,
			Timestamp: 500,
			Key:       []byte("seq-key"),
			Value:     []byte("seq-val"),
		}

		encoded, err := wal.EncodeRecord(rec)
		if err != nil {
			t.Fatalf("EncodeRecord failed for seq %d: %v", seq, err)
		}

		decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("DecodeRecord failed for seq %d: %v", seq, err)
		}

		if decoded.SeqNum != seq {
			t.Fatalf("SeqNum mismatch: want %d, got %d", seq, decoded.SeqNum)
		}
	}
}

// Requirement J: Minimum and maximum timestamps
func TestEncodeDecode_MinMaxTimestamp(t *testing.T) {
	for _, ts := range []uint64{0, math.MaxUint64} {
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    88,
			Timestamp: ts,
			Key:       []byte("ts-key"),
			Value:     []byte("ts-val"),
		}

		encoded, err := wal.EncodeRecord(rec)
		if err != nil {
			t.Fatalf("EncodeRecord failed for ts %d: %v", ts, err)
		}

		decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("DecodeRecord failed for ts %d: %v", ts, err)
		}

		if decoded.Timestamp != ts {
			t.Fatalf("Timestamp mismatch: want %d, got %d", ts, decoded.Timestamp)
		}
	}
}

// Requirement K: CRC correctness with exact known bytes
func TestCRC_ExactKnownBytes(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 2,
		Key:       []byte("foo"),
		Value:     []byte("bar"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// The payload over which CRC is calculated:
	// Byte 4: RecordType (1 byte) = 0x01
	// Byte 5..12: SeqNum (8 bytes, BE) = 1
	// Byte 13..20: Timestamp (8 bytes, BE) = 2
	// Byte 21..22: KeyLength (2 bytes, BE) = 3
	// Byte 23..25: KeyBytes = "foo"
	// Byte 26..29: ValueLength (4 bytes, BE) = 3
	// Byte 30..32: ValueBytes = "bar"
	expectedCRCInput := []byte{
		0x01,                                           // Type
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, // SeqNum 1
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, // Timestamp 2
		0x00, 0x03, // KeyLen 3
		'f', 'o', 'o', // Key
		0x00, 0x00, 0x00, 0x03, // ValLen 3
		'b', 'a', 'r', // Val
	}

	if !bytes.Equal(encoded[4:], expectedCRCInput) {
		t.Fatalf("serialized payload mismatch:\ngot:  %#v\nwant: %#v", encoded[4:], expectedCRCInput)
	}

	expectedCRC := binary.Checksum(expectedCRCInput)
	actualCRC := binary.GetUint32(encoded[0:4])
	if actualCRC != expectedCRC {
		t.Fatalf("CRC mismatch: encoded has 0x%08x, manual checksum gives 0x%08x", actualCRC, expectedCRC)
	}

	// Verify DecodeRecord validates and returns identical CRC
	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}
	if decoded.CRC != expectedCRC {
		t.Fatalf("decoded CRC mismatch: want 0x%08x, got 0x%08x", expectedCRC, decoded.CRC)
	}
}

// Requirement L: Single-byte corruption across all fields
func TestDecodeRecord_SingleByteCorruption(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    12345,
		Timestamp: 67890,
		Key:       []byte("corrupt-key"),
		Value:     []byte("corrupt-value"),
	}

	validBytes, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Test corrupting every single byte in the payload (offsets 4 to len-1)
	for i := 4; i < len(validBytes); i++ {
		corrupted := make([]byte, len(validBytes))
		copy(corrupted, validBytes)
		corrupted[i] ^= 0x5A // bit flip

		_, err := wal.DecodeRecord(bytes.NewReader(corrupted))
		if err == nil {
			t.Fatalf("corruption at byte offset %d was silently accepted", i)
		}

		// Corruption should be intercepted either by CRC mismatch or field validation
		isExpected := stdErrors.Is(err, errors.ErrChecksumMismatch) ||
			stdErrors.Is(err, errors.ErrInvalidRecordType) ||
			stdErrors.Is(err, errors.ErrInvalidRecordPayload) ||
			stdErrors.Is(err, errors.ErrKeyTooLarge) ||
			stdErrors.Is(err, errors.ErrValueTooLarge) ||
			stdErrors.Is(err, errors.ErrEmptyKey) ||
			stdErrors.Is(err, io.ErrUnexpectedEOF)

		if !isExpected {
			t.Fatalf("byte offset %d produced unexpected error type: %v", i, err)
		}
	}
}

// Requirement M: Truncated header
func TestDecodeRecord_TruncatedHeader(t *testing.T) {
	// Clean EOF at 0 bytes
	_, err := wal.DecodeRecord(bytes.NewReader(nil))
	if !stdErrors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on 0 bytes, got %v", err)
	}

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 10,
		Key:       []byte("k"),
		Value:     []byte("v"),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Truncated at 1 to 20 bytes
	for i := 1; i < wal.HeaderSize; i++ {
		_, err := wal.DecodeRecord(bytes.NewReader(encoded[:i]))
		if !stdErrors.Is(err, errors.ErrHeaderTruncated) {
			t.Fatalf("truncated at %d bytes: expected ErrHeaderTruncated, got %v", i, err)
		}
		if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated at %d bytes: expected io.ErrUnexpectedEOF, got %v", i, err)
		}
	}
}

// Requirement N: Truncated key
func TestDecodeRecord_TruncatedKey(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 10,
		Key:       []byte("medium-sized-key"),
		Value:     []byte("v"),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Truncated in KeyLength field (offsets 21, 22)
	for i := wal.HeaderSize; i < wal.HeaderSize+2; i++ {
		_, err := wal.DecodeRecord(bytes.NewReader(encoded[:i]))
		if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated at keylen offset %d: expected io.ErrUnexpectedEOF, got %v", i, err)
		}
	}

	// Truncated during KeyBytes (offsets 23 to 23+len(key)-1)
	keyStart := wal.HeaderSize + 2
	for i := keyStart; i < keyStart+len(rec.Key); i++ {
		_, err := wal.DecodeRecord(bytes.NewReader(encoded[:i]))
		if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated at key byte offset %d: expected io.ErrUnexpectedEOF, got %v", i, err)
		}
	}
}

// Requirement O: Truncated value
func TestDecodeRecord_TruncatedValue(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 10,
		Key:       []byte("k"),
		Value:     []byte("medium-sized-value-payload"),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	valLenOffset := wal.HeaderSize + 2 + len(rec.Key)
	// Truncated during ValueLength field
	for i := valLenOffset; i < valLenOffset+4; i++ {
		_, err := wal.DecodeRecord(bytes.NewReader(encoded[:i]))
		if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated at valLen offset %d: expected io.ErrUnexpectedEOF, got %v", i, err)
		}
	}

	// Truncated during ValueBytes
	valBytesOffset := valLenOffset + 4
	for i := valBytesOffset; i < len(encoded); i++ {
		_, err := wal.DecodeRecord(bytes.NewReader(encoded[:i]))
		if !stdErrors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated at valBytes offset %d: expected io.ErrUnexpectedEOF, got %v", i, err)
		}
	}
}

// Requirement P: Invalid record type
func TestDecodeRecord_InvalidRecordType(t *testing.T) {
	for _, invalidType := range []byte{0x00, 0x05, 0x7F, 0xFF} {
		buf := make([]byte, 30)
		buf[4] = invalidType
		_, err := wal.DecodeRecord(bytes.NewReader(buf))
		if !stdErrors.Is(err, errors.ErrInvalidRecordType) {
			t.Fatalf("expected ErrInvalidRecordType for byte 0x%02x, got %v", invalidType, err)
		}
		var typed *errors.InvalidRecordTypeError
		if !stdErrors.As(err, &typed) {
			t.Fatalf("expected *InvalidRecordTypeError for byte 0x%02x, got %v", invalidType, err)
		}
		if typed.Type != invalidType {
			t.Fatalf("typed error Type mismatch: want 0x%02x, got 0x%02x", invalidType, typed.Type)
		}
	}
}

// Requirement Q: KeyLength above authoritative maximum
func TestEncodeRecord_KeyLengthAboveMaximum(t *testing.T) {
	oversizedKey := make([]byte, binary.MaxKeyLen+1)
	rec := wal.Record{
		Type:   wal.RecordTypePut,
		Key:    oversizedKey,
		Value:  []byte("val"),
		SeqNum: 1,
	}

	_, err := wal.EncodeRecord(rec)
	if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge for oversized key, got %v", err)
	}
	var typed *errors.KeyTooLargeError
	if !stdErrors.As(err, &typed) {
		t.Fatalf("expected *KeyTooLargeError, got %v", err)
	}
	if typed.KeySize != uint32(len(oversizedKey)) {
		t.Fatalf("KeySize mismatch: got %d, want %d", typed.KeySize, len(oversizedKey))
	}
}

// Requirement R: ValueLength above authoritative maximum
func TestDecodeRecord_ValueLengthAboveMaximum(t *testing.T) {
	var buf [wal.HeaderSize + 2 + 4]byte
	buf[4] = byte(wal.RecordTypePut)
	binary.PutUint16(buf[21:23], 0) // 0 key length will be caught, so provide 1 key byte
	rec := wal.Record{
		Type:   wal.RecordTypePut,
		Key:    []byte("k"),
		SeqNum: 1,
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Set ValueLength to 4,194,305 (4MB + 1)
	valOffset := 23 + len(rec.Key)
	binary.PutUint32(encoded[valOffset:valOffset+4], binary.MaxValueLen+1)

	_, err = wal.DecodeRecord(bytes.NewReader(encoded))
	if !stdErrors.Is(err, errors.ErrValueTooLarge) {
		t.Fatalf("expected ErrValueTooLarge, got %v", err)
	}
	var typed *errors.ValueTooLargeError
	if !stdErrors.As(err, &typed) {
		t.Fatalf("expected *ValueTooLargeError, got %v", err)
	}
	if typed.ValueSize != binary.MaxValueLen+1 {
		t.Fatalf("ValueSize mismatch: got %d, want %d", typed.ValueSize, binary.MaxValueLen+1)
	}
}

// Requirement S: Malicious uint32 length fields attempting resource exhaustion
func TestDecodeRecord_MaliciousLengthResourceExhaustionDefense(t *testing.T) {
	rec := wal.Record{
		Type:   wal.RecordTypePut,
		Key:    []byte("k"),
		SeqNum: 1,
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}
	valOffset := 23 + len(rec.Key)

	maliciousLengths := []uint32{
		math.MaxUint32, // 4 GiB - 1
		0x80000000,     // 2 GiB
		0x10000000,     // 256 MiB
		binary.MaxValueLen + 1,
	}

	for _, malLen := range maliciousLengths {
		buf := make([]byte, len(encoded))
		copy(buf, encoded)
		binary.PutUint32(buf[valOffset:valOffset+4], malLen)

		// Must fail fast without allocating
		_, err := wal.DecodeRecord(bytes.NewReader(buf))
		if !stdErrors.Is(err, errors.ErrValueTooLarge) {
			t.Fatalf("expected ErrValueTooLarge for length %d, got %v", malLen, err)
		}
	}
}

// Requirement T: Partial / chunked readers
func TestDecodeRecord_PartialChunkedReader(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    9876,
		Timestamp: 54321,
		Key:       []byte("stream-key"),
		Value:     bytes.Repeat([]byte("chunked-data-payload-"), 10),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	for _, chunkSize := range []int{1, 2, 3, 5, 7, 11, 17, 31} {
		cr := &chunkReader{r: bytes.NewReader(encoded), chunkSize: chunkSize}
		decoded, err := wal.DecodeRecord(cr)
		if err != nil {
			t.Fatalf("DecodeRecord failed for chunk size %d: %v", chunkSize, err)
		}
		if !rec.Equal(decoded) {
			t.Fatalf("decoded record mismatch for chunk size %d", chunkSize)
		}
	}
}

// Requirement U: Readers returning one byte per Read
func TestDecodeRecord_SingleByteReader(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    112233,
		Timestamp: 445566,
		Key:       []byte("one-byte-at-a-time-key"),
		Value:     []byte("one-byte-at-a-time-val"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	sbr := &singleByteReader{r: bytes.NewReader(encoded)}
	decoded, err := wal.DecodeRecord(sbr)
	if err != nil {
		t.Fatalf("DecodeRecord failed with single-byte reader: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("decoded record mismatch with single-byte reader")
	}
}

// Requirement V: Readers returning short reads
func TestDecodeRecord_ShortReader(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    444,
		Timestamp: 888,
		Key:       []byte("short-read-key"),
		Value:     []byte("short-read-val"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	sr := &shortReader{data: encoded, step: 3}
	decoded, err := wal.DecodeRecord(sr)
	if err != nil {
		t.Fatalf("DecodeRecord failed with shortReader: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("decoded record mismatch with shortReader")
	}
}

// Requirement W: Readers returning data and EOF simultaneously
func TestDecodeRecord_DataAndEOFReader(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    99,
		Timestamp: 199,
		Key:       []byte("eof-key"),
		Value:     []byte("eof-val"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	dr := &dataAndEOFReader{data: encoded}
	decoded, err := wal.DecodeRecord(dr)
	if err != nil {
		t.Fatalf("DecodeRecord failed with dataAndEOFReader: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("decoded record mismatch with dataAndEOFReader")
	}
}

// Requirement X: Round-trip property tests across random records
func TestRecordCodec_RoundTripProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	types := []wal.RecordType{
		wal.RecordTypePut,
		wal.RecordTypeDelete,
		wal.RecordTypeBatchStart,
		wal.RecordTypeBatchCommit,
	}

	for i := 0; i < 500; i++ {
		rType := types[rng.Intn(len(types))]
		var key, val []byte

		switch rType {
		case wal.RecordTypePut:
			kLen := 1 + rng.Intn(1024)
			vLen := rng.Intn(4096)
			key = make([]byte, kLen)
			val = make([]byte, vLen)
			rng.Read(key)
			rng.Read(val)
		case wal.RecordTypeDelete:
			kLen := 1 + rng.Intn(1024)
			key = make([]byte, kLen)
			rng.Read(key)
		case wal.RecordTypeBatchStart, wal.RecordTypeBatchCommit:
			// Markers carry zero key/value
		}

		rec := wal.Record{
			Type:      rType,
			SeqNum:    binary.SeqNum(rng.Uint64()),
			Timestamp: rng.Uint64(),
			Key:       key,
			Value:     val,
		}

		encoded, err := wal.EncodeRecord(rec)
		if err != nil {
			t.Fatalf("iteration %d: EncodeRecord failed: %v", i, err)
		}

		decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
		if err != nil {
			t.Fatalf("iteration %d: DecodeRecord failed: %v", i, err)
		}

		if !rec.Equal(decoded) {
			t.Fatalf("iteration %d: round-trip mismatch:\norig: %+v\ngot:  %+v", i, rec, decoded)
		}
	}
}

// Requirement Y: Deterministic encoding
func TestEncodeRecord_Deterministic(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    123456,
		Timestamp: 7891011,
		Key:       []byte("deterministic-key"),
		Value:     []byte("deterministic-value"),
	}

	encoded1, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord 1 failed: %v", err)
	}

	encoded2, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord 2 failed: %v", err)
	}

	if !bytes.Equal(encoded1, encoded2) {
		t.Fatalf("encoding was non-deterministic")
	}
}

// Requirement Z: Input immutability
func TestEncodeRecord_InputImmutability(t *testing.T) {
	origKey := []byte("original-immutable-key")
	origVal := []byte("original-immutable-val")

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    10,
		Timestamp: 20,
		Key:       origKey,
		Value:     origVal,
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	// Mutate caller-owned buffers after encoding
	origKey[0] = 'X'
	origVal[0] = 'Y'

	// Verify encoded bytes still decode to original content
	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if string(decoded.Key) != "original-immutable-key" {
		t.Fatalf("encoded record key mutated: got %s", decoded.Key)
	}
	if string(decoded.Value) != "original-immutable-val" {
		t.Fatalf("encoded record value mutated: got %s", decoded.Value)
	}
}

// Requirement AA: Decoded slice ownership and aliasing behavior
func TestDecodeRecord_SliceOwnershipAndAliasing(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    100,
		Timestamp: 200,
		Key:       []byte("ownership-key"),
		Value:     []byte("ownership-val"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	decoded1, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord 1 failed: %v", err)
	}

	decoded2, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord 2 failed: %v", err)
	}

	// Mutate decoded1 slices
	decoded1.Key[0] = 'Z'
	decoded1.Value[0] = 'Q'

	if decoded2.Key[0] == 'Z' {
		t.Fatalf("decoded1 and decoded2 share underlying key memory (aliasing detected)")
	}
	if decoded2.Value[0] == 'Q' {
		t.Fatalf("decoded1 and decoded2 share underlying value memory (aliasing detected)")
	}
}

// Requirement AB: Integer arithmetic boundary cases
func TestIntegerArithmeticBoundaryCases(t *testing.T) {
	// Boundary test with MaxUint64 SeqNum and Timestamp
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.MaxSeqNum,
		Timestamp: math.MaxUint64,
		Key:       []byte("boundary-key"),
		Value:     []byte("boundary-val"),
	}

	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if decoded.SeqNum != binary.MaxSeqNum || decoded.Timestamp != math.MaxUint64 {
		t.Fatalf("integer boundary round-trip mismatch: got seq=%d, ts=%d", decoded.SeqNum, decoded.Timestamp)
	}
}

func TestAppendRecord_ReusedCapacity(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 10,
		Key:       []byte("reused-key"),
		Value:     []byte("reused-val"),
	}

	expectedLen := wal.MinRecordSize + len(rec.Key) + len(rec.Value)
	buf := make([]byte, 0, expectedLen*2)

	appended, err := wal.AppendRecord(buf, rec)
	if err != nil {
		t.Fatalf("AppendRecord failed: %v", err)
	}

	if len(appended) != expectedLen {
		t.Fatalf("expected length %d, got %d", expectedLen, len(appended))
	}

	decoded, err := wal.DecodeRecord(bytes.NewReader(appended))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if !rec.Equal(decoded) {
		t.Fatalf("appended record mismatch after decode")
	}
}

func TestDecodeRecord_InjectedReadError(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 10,
		Key:       []byte("err-key"),
		Value:     []byte("err-val"),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("EncodeRecord failed: %v", err)
	}

	customErr := stdErrors.New("simulated physical disk I/O failure")
	fr := &failingReader{
		data:     encoded,
		failAt:   wal.HeaderSize + 1,
		injected: customErr,
	}

	_, err = wal.DecodeRecord(fr)
	if !stdErrors.Is(err, customErr) {
		t.Fatalf("expected injected error %v, got %v", customErr, err)
	}
}

func TestAppendRecord_ReallocationAndInvalid(t *testing.T) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 10,
		Key:       []byte("realloc-key"),
		Value:     []byte("realloc-val"),
	}

	// 1. Append to nil
	appendedNil, err := wal.AppendRecord(nil, rec)
	if err != nil {
		t.Fatalf("AppendRecord to nil failed: %v", err)
	}
	decoded, err := wal.DecodeRecord(bytes.NewReader(appendedNil))
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}
	if !rec.Equal(decoded) {
		t.Fatalf("mismatch after append to nil")
	}

	// 2. Append to slice with existing bytes and insufficient capacity (triggers realloc branch)
	prefix := []byte{0xAA, 0xBB, 0xCC}
	appendedWithPrefix, err := wal.AppendRecord(prefix, rec)
	if err != nil {
		t.Fatalf("AppendRecord with prefix failed: %v", err)
	}
	if !bytes.Equal(appendedWithPrefix[:3], prefix) {
		t.Fatalf("prefix was corrupted")
	}
	decodedPrefix, err := wal.DecodeRecord(bytes.NewReader(appendedWithPrefix[3:]))
	if err != nil {
		t.Fatalf("DecodeRecord of suffixed record failed: %v", err)
	}
	if !rec.Equal(decodedPrefix) {
		t.Fatalf("mismatch after append with prefix")
	}

	// 3. Append invalid record fails
	invalidRec := wal.Record{Type: wal.RecordTypeInvalid}
	_, err = wal.AppendRecord(nil, invalidRec)
	if err == nil {
		t.Fatalf("expected error appending invalid record")
	}
}

func TestDecodeRecord_HeaderReadError(t *testing.T) {
	customErr := stdErrors.New("simulated physical read failure during header")
	fr := &failingReader{
		data:     make([]byte, wal.HeaderSize),
		failAt:   0,
		injected: customErr,
	}

	_, err := wal.DecodeRecord(fr)
	if !stdErrors.Is(err, customErr) {
		t.Fatalf("expected injected error %v, got %v", customErr, err)
	}
}

// ---------------------------------------------------------------------------
// Native Go Fuzzing for Full Record Codec
// ---------------------------------------------------------------------------

func FuzzRecordCodec(f *testing.F) {
	// Seed valid records
	records := []wal.Record{
		{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")},
		{Type: wal.RecordTypePut, SeqNum: 42, Timestamp: 200, Key: []byte("long-key-string"), Value: bytes.Repeat([]byte("val"), 20)},
		{Type: wal.RecordTypePut, SeqNum: math.MaxUint64, Timestamp: math.MaxUint64, Key: []byte("k"), Value: nil},
		{Type: wal.RecordTypeDelete, SeqNum: 2, Timestamp: 101, Key: []byte("tombstone-key"), Value: nil},
		{Type: wal.RecordTypeBatchStart, SeqNum: 3, Timestamp: 102, Key: nil, Value: nil},
		{Type: wal.RecordTypeBatchCommit, SeqNum: 4, Timestamp: 103, Key: nil, Value: nil},
	}
	for _, rec := range records {
		b, err := wal.EncodeRecord(rec)
		if err == nil {
			f.Add(b)
		}
	}

	// Seed partial / hostile bytes
	f.Add([]byte{})
	f.Add([]byte{0x01})
	f.Add(bytes.Repeat([]byte{0x00}, 27))
	f.Add(bytes.Repeat([]byte{0xFF}, 27))
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1, 'a', 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		rec, err := wal.DecodeRecord(r)
		if err != nil {
			return
		}

		// Invariant 1: Validated record
		if valErr := rec.Validate(); valErr != nil {
			t.Fatalf("DecodeRecord returned record that fails Validate(): %v", valErr)
		}

		// Invariant 2: Structural stability: Re-encode must succeed
		reEncoded, err := wal.EncodeRecord(rec)
		if err != nil {
			t.Fatalf("failed to re-encode decoded record: %v", err)
		}

		// Invariant 3: Re-decode must equal original decoded record
		reDecoded, err := wal.DecodeRecord(bytes.NewReader(reEncoded))
		if err != nil {
			t.Fatalf("failed to decode re-encoded record: %v", err)
		}

		if !rec.Equal(reDecoded) {
			t.Fatalf("fuzz round-trip equality mismatch:\norig: %#v\nre:   %#v", rec, reDecoded)
		}
	})
}

// ---------------------------------------------------------------------------
// Benchmarks for Full Record Serialization & Deserialization
// ---------------------------------------------------------------------------

func BenchmarkEncodeRecord_Small(b *testing.B) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
		Key:       bytes.Repeat([]byte("k"), 32),
		Value:     bytes.Repeat([]byte("v"), 128),
	}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = wal.EncodeRecord(rec)
	}
}

func BenchmarkEncodeRecord_Large(b *testing.B) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
		Key:       bytes.Repeat([]byte("k"), 1024),
		Value:     bytes.Repeat([]byte("v"), 65536),
	}
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = wal.EncodeRecord(rec)
	}
}

func BenchmarkAppendRecord_ReusedBuffer(b *testing.B) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
		Key:       bytes.Repeat([]byte("k"), 32),
		Value:     bytes.Repeat([]byte("v"), 128),
	}
	buf := make([]byte, 0, wal.MinRecordSize+len(rec.Key)+len(rec.Value))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, _ = wal.AppendRecord(buf[:0], rec)
	}
}

func BenchmarkDecodeRecord_Small(b *testing.B) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
		Key:       bytes.Repeat([]byte("k"), 32),
		Value:     bytes.Repeat([]byte("v"), 128),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		b.Fatal(err)
	}
	reader := bytes.NewReader(encoded)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader.Reset(encoded)
		_, _ = wal.DecodeRecord(reader)
	}
}

func BenchmarkDecodeRecord_Large(b *testing.B) {
	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1000,
		Timestamp: 1700000000000000000,
		Key:       bytes.Repeat([]byte("k"), 1024),
		Value:     bytes.Repeat([]byte("v"), 65536),
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		b.Fatal(err)
	}
	reader := bytes.NewReader(encoded)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		reader.Reset(encoded)
		_, _ = wal.DecodeRecord(reader)
	}
}

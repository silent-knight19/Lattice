package wal_test

import (
	"bytes"
	stdErrors "errors"
	"math"
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

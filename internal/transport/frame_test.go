package transport_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// fragmentReader is an io.Reader that returns at most chunkSize bytes per Read call,
// simulating fragmented TCP delivery across multiple packets.
type fragmentReader struct {
	r         io.Reader
	chunkSize int
}

func (fr *fragmentReader) Read(p []byte) (int, error) {
	if fr.chunkSize <= 0 {
		fr.chunkSize = 1
	}
	limit := len(p)
	if limit > fr.chunkSize {
		limit = fr.chunkSize
	}
	return fr.r.Read(p[:limit])
}

func TestHeader_BinaryLayout(t *testing.T) {
	hdr := transport.Header{
		Magic:         transport.Magic,
		OpCode:        transport.OpPut,
		Flags:         transport.FlagSnappy,
		SeqID:         0x0102030405060708,
		PayloadLength: 1024,
	}

	var buf [transport.HeaderSize]byte
	hdr.Encode(buf[:])

	// Byte 0..3: Magic Big-Endian
	if binary.GetUint32(buf[0:4]) != transport.Magic {
		t.Fatalf("magic mismatch: got 0x%08x, want 0x%08x", binary.GetUint32(buf[0:4]), transport.Magic)
	}
	if buf[0] != 'L' || buf[1] != 'A' || buf[2] != 'T' || buf[3] != 'T' {
		t.Fatalf("magic ASCII mismatch: %q", string(buf[0:4]))
	}

	// Byte 4: OpCode
	if buf[4] != byte(transport.OpPut) {
		t.Fatalf("opcode mismatch: got 0x%02x, want 0x%02x", buf[4], byte(transport.OpPut))
	}

	// Byte 5: Flags
	if buf[5] != transport.FlagSnappy {
		t.Fatalf("flags mismatch: got 0x%02x, want 0x%02x", buf[5], transport.FlagSnappy)
	}

	// Bytes 6..13: SeqID Big-Endian
	if binary.GetUint64(buf[6:14]) != 0x0102030405060708 {
		t.Fatalf("seqID mismatch: got 0x%016x", binary.GetUint64(buf[6:14]))
	}

	// Bytes 14..17: PayloadLength Big-Endian
	if binary.GetUint32(buf[14:18]) != 1024 {
		t.Fatalf("payload length mismatch: got %d, want 1024", binary.GetUint32(buf[14:18]))
	}

	// Decode back
	decoded, err := transport.DecodeHeaderBytes(buf[:])
	if err != nil {
		t.Fatalf("DecodeHeaderBytes failed: %v", err)
	}
	if decoded.Magic != hdr.Magic || decoded.OpCode != hdr.OpCode || decoded.Flags != hdr.Flags ||
		decoded.SeqID != hdr.SeqID || decoded.PayloadLength != hdr.PayloadLength {
		t.Fatalf("decoded header mismatch: %+v vs %+v", decoded, hdr)
	}
}

func TestHeader_MagicValidation(t *testing.T) {
	// 1. Valid magic
	var validBuf [transport.HeaderSize]byte
	binary.PutUint32(validBuf[0:4], transport.Magic)
	binary.PutUint32(validBuf[14:18], 10)
	_, err := transport.DecodeHeaderBytes(validBuf[:])
	if err != nil {
		t.Fatalf("expected valid magic to pass, got: %v", err)
	}

	// 2. Invalid magic (e.g. HTTP GET probe)
	var httpProbe [transport.HeaderSize]byte
	copy(httpProbe[:], "GET / HTTP/1.1\r\n")
	_, err = transport.DecodeHeaderBytes(httpProbe[:])
	if err == nil {
		t.Fatal("expected error on invalid magic, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidMagic) {
		t.Fatalf("expected ErrInvalidMagic, got: %v", err)
	}
	var magicErr *errors.InvalidMagicError
	if !stdErrors.As(err, &magicErr) {
		t.Fatalf("expected *InvalidMagicError, got: %T", err)
	}
	if magicErr.Expected != transport.Magic {
		t.Errorf("expected 0x%08x, got 0x%08x", transport.Magic, magicErr.Expected)
	}
}

func TestHeader_FrameBombPayloadCeiling(t *testing.T) {
	// 1. Exactly maximum allowed (5 MB = 5,242,880 bytes)
	var buf [transport.HeaderSize]byte
	binary.PutUint32(buf[0:4], transport.Magic)
	binary.PutUint32(buf[14:18], transport.MaxPayloadLength)
	hdr, err := transport.DecodeHeaderBytes(buf[:])
	if err != nil {
		t.Fatalf("expected 5MB payload to be legal, got: %v", err)
	}
	if hdr.PayloadLength != transport.MaxPayloadLength {
		t.Fatalf("expected payload length %d, got %d", transport.MaxPayloadLength, hdr.PayloadLength)
	}

	// 2. 5MB + 1 byte -> rejected immediately
	binary.PutUint32(buf[14:18], transport.MaxPayloadLength+1)
	_, err = transport.DecodeHeaderBytes(buf[:])
	if err == nil {
		t.Fatal("expected error for 5MB+1, got nil")
	}
	if !stdErrors.Is(err, errors.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got: %v", err)
	}
	var largeErr *errors.FrameTooLargeError
	if !stdErrors.As(err, &largeErr) {
		t.Fatalf("expected *FrameTooLargeError, got: %T", err)
	}
	if largeErr.PayloadSize != transport.MaxPayloadLength+1 || largeErr.MaxSize != transport.MaxPayloadLength {
		t.Errorf("largeErr details mismatch: %+v", largeErr)
	}

	// 3. 2 GB (Frame bomb attack vector from threat model)
	binary.PutUint32(buf[14:18], 2*1024*1024*1024)
	_, err = transport.DecodeHeaderBytes(buf[:])
	if !stdErrors.Is(err, errors.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge for 2GB payload, got: %v", err)
	}

	// 4. MaxUint32 (4,294,967,295 bytes)
	binary.PutUint32(buf[14:18], math.MaxUint32)
	_, err = transport.DecodeHeaderBytes(buf[:])
	if !stdErrors.Is(err, errors.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge for MaxUint32 payload, got: %v", err)
	}
}

func TestFrame_EncodeDecodeRoundTrip(t *testing.T) {
	testCases := []struct {
		name    string
		opCode  transport.OpCode
		flags   byte
		seqID   uint64
		payload []byte
	}{
		{
			name:    "empty payload (22B minimum frame)",
			opCode:  transport.OpStats,
			flags:   transport.FlagNone,
			seqID:   1,
			payload: nil,
		},
		{
			name:    "small payload",
			opCode:  transport.OpGet,
			flags:   transport.FlagNone,
			seqID:   42,
			payload: []byte("user_key_12345"),
		},
		{
			name:    "binary payload with zeroes and 0xFF",
			opCode:  transport.OpPut,
			flags:   transport.FlagSnappy,
			seqID:   1000000,
			payload: []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0xAA, 0x55},
		},
		{
			name:    "medium 64KB payload",
			opCode:  transport.OpPut,
			flags:   transport.FlagTracing,
			seqID:   999999,
			payload: bytes.Repeat([]byte("A"), 64*1024),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frame := &transport.Frame{
				Header: transport.Header{
					OpCode: tc.opCode,
					Flags:  tc.flags,
					SeqID:  tc.seqID,
				},
				Payload: tc.payload,
			}

			var buf bytes.Buffer
			if err := transport.EncodeFrame(&buf, frame); err != nil {
				t.Fatalf("EncodeFrame failed: %v", err)
			}

			// Wire size must be exactly 18 + len(payload) + 4
			expectedWireLen := transport.HeaderSize + len(tc.payload) + transport.TrailerSize
			if buf.Len() != expectedWireLen {
				t.Fatalf("wire size mismatch: got %d, want %d", buf.Len(), expectedWireLen)
			}

			decoded, err := transport.DecodeFrame(&buf)
			if err != nil {
				t.Fatalf("DecodeFrame failed: %v", err)
			}

			if decoded.Header.Magic != transport.Magic {
				t.Errorf("magic mismatch: got 0x%08x", decoded.Header.Magic)
			}
			if decoded.Header.OpCode != tc.opCode {
				t.Errorf("opcode mismatch: got %v, want %v", decoded.Header.OpCode, tc.opCode)
			}
			if decoded.Header.Flags != tc.flags {
				t.Errorf("flags mismatch: got %v, want %v", decoded.Header.Flags, tc.flags)
			}
			if decoded.Header.SeqID != tc.seqID {
				t.Errorf("seqID mismatch: got %d, want %d", decoded.Header.SeqID, tc.seqID)
			}
			if decoded.Header.PayloadLength != uint32(len(tc.payload)) {
				t.Errorf("payload length mismatch: got %d, want %d", decoded.Header.PayloadLength, len(tc.payload))
			}
			if !bytes.Equal(decoded.Payload, tc.payload) {
				t.Errorf("payload content mismatch: got %x, want %x", decoded.Payload, tc.payload)
			}
			if decoded.CRC != frame.CRC {
				t.Errorf("CRC mismatch: got 0x%08x, want 0x%08x", decoded.CRC, frame.CRC)
			}
		})
	}
}

func TestFrame_CRC32BitFlipDetection(t *testing.T) {
	frame := &transport.Frame{
		Header: transport.Header{
			OpCode: transport.OpPut,
			Flags:  0,
			SeqID:  555,
		},
		Payload: []byte("sensitive_database_payload_bytes"),
	}

	var buf bytes.Buffer
	if err := transport.EncodeFrame(&buf, frame); err != nil {
		t.Fatalf("EncodeFrame failed: %v", err)
	}

	originalBytes := buf.Bytes()

	// Test bit flips across various positions:
	// 1. In header (e.g. byte 4: opcode)
	// 2. In payload (e.g. byte 20)
	// 3. In trailer (e.g. last byte)
	flipPositions := []int{4, 10, 18, 25, len(originalBytes) - 1}

	for _, pos := range flipPositions {
		corrupted := make([]byte, len(originalBytes))
		copy(corrupted, originalBytes)
		corrupted[pos] ^= 0x01 // flip one bit

		_, err := transport.DecodeFrame(bytes.NewReader(corrupted))
		if err == nil {
			t.Fatalf("expected CRC error on bit flip at position %d, got nil", pos)
		}
		if !stdErrors.Is(err, errors.ErrChecksumMismatch) && !stdErrors.Is(err, errors.ErrInvalidMagic) {
			t.Fatalf("expected ErrChecksumMismatch at position %d, got: %v", pos, err)
		}
	}
}

func TestFrame_TruncatedStreamHandling(t *testing.T) {
	frame := &transport.Frame{
		Header: transport.Header{
			OpCode: transport.OpGet,
			SeqID:  123,
		},
		Payload: []byte("test_key_truncation"),
	}

	var buf bytes.Buffer
	if err := transport.EncodeFrame(&buf, frame); err != nil {
		t.Fatalf("EncodeFrame failed: %v", err)
	}

	fullBytes := buf.Bytes()

	// Test EOF at byte 0 (clean disconnect)
	_, err := transport.DecodeFrame(bytes.NewReader(nil))
	if err != io.EOF {
		t.Fatalf("expected clean io.EOF on 0 bytes, got: %v", err)
	}

	// Test truncation at every single byte boundary from 1 to len(fullBytes)-1
	for cut := 1; cut < len(fullBytes); cut++ {
		partial := fullBytes[:cut]
		_, err := transport.DecodeFrame(bytes.NewReader(partial))
		if err == nil {
			t.Fatalf("expected truncation error when cutting at byte %d/%d, got nil", cut, len(fullBytes))
		}
		if !stdErrors.Is(err, errors.ErrFrameTruncated) {
			t.Fatalf("expected ErrFrameTruncated at cut %d, got: %v", cut, err)
		}
	}
}

func TestFrame_FragmentedStreamDelivery(t *testing.T) {
	// Construct a frame and feed it through a reader that delivers 1 byte per Read call
	frame := &transport.Frame{
		Header: transport.Header{
			OpCode: transport.OpPut,
			SeqID:  987654321,
		},
		Payload: []byte("fragmented_tcp_packet_stream_verification"),
	}

	var buf bytes.Buffer
	if err := transport.EncodeFrame(&buf, frame); err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	// Read 1 byte at a time
	fragR := &fragmentReader{r: bytes.NewReader(buf.Bytes()), chunkSize: 1}
	decoded, err := transport.DecodeFrame(fragR)
	if err != nil {
		t.Fatalf("DecodeFrame over 1-byte fragmented reader failed: %v", err)
	}
	if !bytes.Equal(decoded.Payload, frame.Payload) {
		t.Fatalf("payload mismatch over fragmented reader: got %q, want %q", decoded.Payload, frame.Payload)
	}

	// Read 3 bytes at a time
	fragR3 := &fragmentReader{r: bytes.NewReader(buf.Bytes()), chunkSize: 3}
	decoded3, err := transport.DecodeFrame(fragR3)
	if err != nil {
		t.Fatalf("DecodeFrame over 3-byte fragmented reader failed: %v", err)
	}
	if !bytes.Equal(decoded3.Payload, frame.Payload) {
		t.Fatalf("payload mismatch over 3-byte fragmented reader")
	}
}

func TestFrame_CoalescedMultipleFramesInStream(t *testing.T) {
	var streamBuf bytes.Buffer

	frames := make([]*transport.Frame, 10)
	for i := 0; i < 10; i++ {
		frames[i] = &transport.Frame{
			Header: transport.Header{
				OpCode: transport.OpPut,
				SeqID:  uint64(i + 1),
			},
			Payload: []byte(fmt.Sprintf("key_%d", i)),
		}
		if err := transport.EncodeFrame(&streamBuf, frames[i]); err != nil {
			t.Fatalf("EncodeFrame %d failed: %v", i, err)
		}
	}

	// Read all 10 frames sequentially from the same buffer
	r := bytes.NewReader(streamBuf.Bytes())
	for i := 0; i < 10; i++ {
		decoded, err := transport.DecodeFrame(r)
		if err != nil {
			t.Fatalf("DecodeFrame %d failed: %v", i, err)
		}
		if decoded.Header.SeqID != uint64(i+1) {
			t.Errorf("seqID mismatch at %d: got %d, want %d", i, decoded.Header.SeqID, i+1)
		}
		if !bytes.Equal(decoded.Payload, frames[i].Payload) {
			t.Errorf("payload mismatch at %d: got %q, want %q", i, decoded.Payload, frames[i].Payload)
		}
	}

	// Next read should return clean io.EOF
	_, err := transport.DecodeFrame(r)
	if err != io.EOF {
		t.Fatalf("expected io.EOF at end of stream, got: %v", err)
	}
}

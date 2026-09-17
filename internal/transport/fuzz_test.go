package transport_test

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/transport"
)

func FuzzDecodeFrame(f *testing.F) {
	// Seed corpus with valid frames
	seedFrames := []*transport.Frame{
		{
			Header: transport.Header{
				OpCode: transport.OpStats,
				SeqID:  1,
			},
			Payload: nil,
		},
		{
			Header: transport.Header{
				OpCode: transport.OpGet,
				SeqID:  2,
			},
			Payload: []byte("seed_key"),
		},
		{
			Header: transport.Header{
				OpCode: transport.OpPut,
				Flags:  transport.FlagSnappy,
				SeqID:  3,
			},
			Payload: []byte{0x00, 0x03, 'k', 'e', 'y', 'v', 'a', 'l'},
		},
	}

	for _, sf := range seedFrames {
		var buf bytes.Buffer
		if err := transport.EncodeFrame(&buf, sf); err == nil {
			f.Add(buf.Bytes())
		}
	}

	// Add random edge case byte sequences
	f.Add([]byte{})
	f.Add([]byte{0x4C, 0x41, 0x54, 0x54})
	f.Add(bytes.Repeat([]byte{0xFF}, 18))
	f.Add(bytes.Repeat([]byte{0x00}, 22))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Invariant S3: Untrusted input must never cause a panic
		frame, err := transport.DecodeFrame(bytes.NewReader(data))
		if err != nil {
			// Malformed frame rejected cleanly
			return
		}

		// If decoding succeeded, verify structural invariants:
		if frame.Header.Magic != transport.Magic {
			t.Fatalf("decoded frame has invalid magic: 0x%08x", frame.Header.Magic)
		}
		if frame.Header.PayloadLength > transport.MaxPayloadLength {
			t.Fatalf("decoded frame payload length exceeds ceiling: %d", frame.Header.PayloadLength)
		}
		if len(frame.Payload) != int(frame.Header.PayloadLength) {
			t.Fatalf("payload slice length %d does not match Header.PayloadLength %d", len(frame.Payload), frame.Header.PayloadLength)
		}

		// Re-encoding and decoding must round-trip deterministically
		var out bytes.Buffer
		if encErr := transport.EncodeFrame(&out, frame); encErr != nil {
			t.Fatalf("failed to encode valid frame: %v", encErr)
		}
		frame2, decErr := transport.DecodeFrame(&out)
		if decErr != nil {
			t.Fatalf("failed to decode re-encoded frame: %v", decErr)
		}
		if frame2.Header.SeqID != frame.Header.SeqID || frame2.CRC != frame.CRC || !bytes.Equal(frame2.Payload, frame.Payload) {
			t.Fatalf("frame round-trip discrepancy")
		}
	})
}

func FuzzDecodeRequest(f *testing.F) {
	// Seed with valid request frames
	seeds := []*transport.Request{
		{
			OpCode: transport.OpPut,
			SeqID:  10,
			Key:    []byte("k"),
			Value:  []byte("v"),
		},
		{
			OpCode: transport.OpGet,
			SeqID:  20,
			Key:    []byte("k"),
		},
		{
			OpCode: transport.OpDelete,
			SeqID:  30,
			Key:    []byte("k"),
		},
		{
			OpCode: transport.OpExists,
			SeqID:  40,
			Key:    []byte("k"),
		},
		{
			OpCode: transport.OpStats,
			SeqID:  50,
		},
	}

	for _, req := range seeds {
		if frame, err := transport.EncodeRequest(req); err == nil {
			f.Add(byte(frame.Header.OpCode), frame.Header.Flags, frame.Header.SeqID, frame.Payload)
		}
	}

	// Add random edge cases
	f.Add(byte(transport.OpPut), byte(0), uint64(1), []byte{})
	f.Add(byte(0xFF), byte(0), uint64(2), []byte("invalid_op"))
	f.Add(byte(transport.OpBatch), byte(0), uint64(3), []byte{0x00, 0x00, 0x00, 0x01})

	f.Fuzz(func(t *testing.T, opCodeByte byte, flags byte, seqID uint64, payload []byte) {
		// Cap payload to 5MB to respect memory bound in test
		if len(payload) > int(transport.MaxPayloadLength) {
			return
		}

		frame := &transport.Frame{
			Header: transport.Header{
				Magic:         transport.Magic,
				OpCode:        transport.OpCode(opCodeByte),
				Flags:         flags,
				SeqID:         seqID,
				PayloadLength: uint32(len(payload)),
			},
			Payload: payload,
		}

		// Invariant S3: Decoding must never panic
		req, err := transport.DecodeRequest(frame)
		if err != nil {
			// Malformed request rejected cleanly
			return
		}

		// Validated request invariants
		if !req.OpCode.Valid() {
			t.Fatalf("decoded request has invalid opcode: %v", req.OpCode)
		}
		if req.OpCode == transport.OpPut || req.OpCode == transport.OpGet ||
			req.OpCode == transport.OpDelete || req.OpCode == transport.OpExists {
			if len(req.Key) < 1 || len(req.Key) > transport.MaxKeyLength {
				t.Fatalf("key length out of bounds: %d", len(req.Key))
			}
		}
		if req.OpCode == transport.OpPut {
			if len(req.Value) > transport.MaxValueLength {
				t.Fatalf("value length out of bounds: %d", len(req.Value))
			}
		}

		// Round-trip validation
		reFrame, encErr := transport.EncodeRequest(req)
		if encErr != nil {
			t.Fatalf("failed to encode decoded request: %v", encErr)
		}
		reReq, decErr := transport.DecodeRequest(reFrame)
		if decErr != nil {
			t.Fatalf("failed to re-decode encoded request: %v", decErr)
		}
		if reReq.OpCode != req.OpCode || reReq.SeqID != req.SeqID || !bytes.Equal(reReq.Key, req.Key) || !bytes.Equal(reReq.Value, req.Value) {
			t.Fatalf("request round-trip discrepancy")
		}
	})
}

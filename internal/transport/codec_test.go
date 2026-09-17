package transport_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

func TestCodec_PutRoundTrip(t *testing.T) {
	testCases := []struct {
		name  string
		key   []byte
		value []byte
	}{
		{
			name:  "standard ascii key and value",
			key:   []byte("user:1001:profile"),
			value: []byte(`{"name":"Alice","status":"active"}`),
		},
		{
			name:  "binary key and value with embedded zeros and 0xFF",
			key:   []byte{0x00, 0x01, 0x00, 0xFF, 0xFE, 0x00},
			value: []byte{0xFF, 0x00, 0xAA, 0x55, 0x00, 0x00, 0xFF},
		},
		{
			name:  "zero-length value marker",
			key:   []byte("marker_key"),
			value: []byte{}, // legal in Lattice
		},
		{
			name:  "maximum legal key length (65,535 bytes)",
			key:   bytes.Repeat([]byte("k"), transport.MaxKeyLength),
			value: []byte("val"),
		},
		{
			name:  "large 1MB value",
			key:   []byte("large_val_key"),
			value: bytes.Repeat([]byte("V"), 1024*1024),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := &transport.Request{
				OpCode: transport.OpPut,
				Flags:  transport.FlagNone,
				SeqID:  12345,
				Key:    tc.key,
				Value:  tc.value,
			}

			frame, err := transport.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest failed: %v", err)
			}

			decoded, err := transport.DecodeRequest(frame)
			if err != nil {
				t.Fatalf("DecodeRequest failed: %v", err)
			}

			if decoded.OpCode != transport.OpPut {
				t.Errorf("opcode mismatch: got %v", decoded.OpCode)
			}
			if decoded.SeqID != req.SeqID {
				t.Errorf("seqID mismatch: got %d, want %d", decoded.SeqID, req.SeqID)
			}
			if !bytes.Equal(decoded.Key, tc.key) {
				t.Errorf("key mismatch: got %x, want %x", decoded.Key, tc.key)
			}
			if !bytes.Equal(decoded.Value, tc.value) {
				t.Errorf("value mismatch: got %x, want %x", decoded.Value, tc.value)
			}
		})
	}
}

func TestCodec_GetDeleteExistsRoundTrip(t *testing.T) {
	opCodes := []transport.OpCode{transport.OpGet, transport.OpDelete, transport.OpExists}

	for _, op := range opCodes {
		t.Run(op.String(), func(t *testing.T) {
			key := []byte("entity:account:9999")
			req := &transport.Request{
				OpCode: op,
				SeqID:  777,
				Key:    key,
			}

			frame, err := transport.EncodeRequest(req)
			if err != nil {
				t.Fatalf("EncodeRequest(%s) failed: %v", op, err)
			}

			decoded, err := transport.DecodeRequest(frame)
			if err != nil {
				t.Fatalf("DecodeRequest(%s) failed: %v", op, err)
			}

			if decoded.OpCode != op {
				t.Errorf("opcode mismatch: got %v, want %v", decoded.OpCode, op)
			}
			if decoded.SeqID != 777 {
				t.Errorf("seqID mismatch: got %d", decoded.SeqID)
			}
			if !bytes.Equal(decoded.Key, key) {
				t.Errorf("key mismatch: got %s, want %s", decoded.Key, key)
			}
			if decoded.Value != nil {
				t.Errorf("value should be nil for %s, got %v", op, decoded.Value)
			}
		})
	}
}

func TestCodec_BatchRoundTrip(t *testing.T) {
	ops := []transport.BatchOp{
		{Type: transport.BatchOpPut, Key: []byte("k1"), Value: []byte("v1")},
		{Type: transport.BatchOpDelete, Key: []byte("k2"), Value: nil},
		{Type: transport.BatchOpPut, Key: []byte("k3"), Value: []byte{}}, // zero-length value
		{Type: transport.BatchOpPut, Key: []byte{0x00, 0xFF}, Value: []byte{0xFE, 0x00}},
	}

	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  888,
		Batch:  ops,
	}

	frame, err := transport.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest(BATCH) failed: %v", err)
	}

	decoded, err := transport.DecodeRequest(frame)
	if err != nil {
		t.Fatalf("DecodeRequest(BATCH) failed: %v", err)
	}

	if decoded.OpCode != transport.OpBatch {
		t.Fatalf("opcode mismatch: got %v", decoded.OpCode)
	}
	if len(decoded.Batch) != len(ops) {
		t.Fatalf("batch len mismatch: got %d, want %d", len(decoded.Batch), len(ops))
	}

	for i, op := range decoded.Batch {
		expected := ops[i]
		if op.Type != expected.Type {
			t.Errorf("op %d type mismatch: got %v, want %v", i, op.Type, expected.Type)
		}
		if !bytes.Equal(op.Key, expected.Key) {
			t.Errorf("op %d key mismatch: got %x, want %x", i, op.Key, expected.Key)
		}
		if !bytes.Equal(op.Value, expected.Value) {
			t.Errorf("op %d value mismatch: got %x, want %x", i, op.Value, expected.Value)
		}
	}
}

func TestCodec_StatsRoundTrip(t *testing.T) {
	req := &transport.Request{
		OpCode: transport.OpStats,
		SeqID:  1001,
	}

	frame, err := transport.EncodeRequest(req)
	if err != nil {
		t.Fatalf("EncodeRequest(STATS): %v", err)
	}
	if frame.Header.PayloadLength != 0 {
		t.Fatalf("STATS payload length should be 0, got %d", frame.Header.PayloadLength)
	}

	decoded, err := transport.DecodeRequest(frame)
	if err != nil {
		t.Fatalf("DecodeRequest(STATS): %v", err)
	}
	if decoded.OpCode != transport.OpStats {
		t.Fatalf("expected OpStats, got: %v", decoded.OpCode)
	}

	// STATS with unexpected trailing payload bytes must be rejected
	frame.Payload = []byte("unexpected_stats_data")
	frame.Header.PayloadLength = uint32(len(frame.Payload))
	_, err = transport.DecodeRequest(frame)
	if err == nil {
		t.Fatal("expected error on STATS with payload, got nil")
	}
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got: %v", err)
	}
}

func TestCodec_InvalidOpCodes(t *testing.T) {
	invalidCodes := []transport.OpCode{
		transport.OpUnknown, // 0x00
		transport.OpCode(0x07),
		transport.OpCode(0x80),
		transport.OpCode(0xFF),
	}

	for _, code := range invalidCodes {
		t.Run(fmt.Sprintf("OpCode_0x%02x", byte(code)), func(t *testing.T) {
			frame := &transport.Frame{
				Header: transport.Header{
					Magic:  transport.Magic,
					OpCode: code,
					SeqID:  1,
				},
				Payload: []byte("some_key"),
			}

			_, err := transport.DecodeRequest(frame)
			if err == nil {
				t.Fatalf("expected error on opcode 0x%02x, got nil", byte(code))
			}
			if !stdErrors.Is(err, errors.ErrInvalidOpCode) {
				t.Fatalf("expected ErrInvalidOpCode, got: %v", err)
			}
			var opErr *errors.InvalidOpCodeError
			if !stdErrors.As(err, &opErr) {
				t.Fatalf("expected *InvalidOpCodeError, got: %T", err)
			}
			if opErr.OpCode != byte(code) {
				t.Errorf("expected opcode 0x%02x in error, got 0x%02x", byte(code), opErr.OpCode)
			}

			// EncodeRequest must also reject invalid opcodes
			req := &transport.Request{OpCode: code, SeqID: 1}
			_, err = transport.EncodeRequest(req)
			if !stdErrors.Is(err, errors.ErrInvalidOpCode) {
				t.Fatalf("EncodeRequest: expected ErrInvalidOpCode, got: %v", err)
			}
		})
	}
}

func TestCodec_MalformedPutPayloads(t *testing.T) {
	// 1. Shorter than 2 bytes (no key length prefix)
	f1 := &transport.Frame{
		Header:  transport.Header{Magic: transport.Magic, OpCode: transport.OpPut},
		Payload: []byte{0x00},
	}
	_, err := transport.DecodeRequest(f1)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for 1-byte PUT payload, got: %v", err)
	}

	// 2. Declared key length is 0
	f2 := &transport.Frame{
		Header:  transport.Header{Magic: transport.Magic, OpCode: transport.OpPut},
		Payload: []byte{0x00, 0x00, 'v', 'a', 'l'},
	}
	_, err = transport.DecodeRequest(f2)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for 0-length key, got: %v", err)
	}

	// 3. Declared key length exceeds payload bounds
	f3 := &transport.Frame{
		Header:  transport.Header{Magic: transport.Magic, OpCode: transport.OpPut},
		Payload: []byte{0x00, 0x05, 'k', 'e'}, // claims 5 bytes, only 2 present
	}
	_, err = transport.DecodeRequest(f3)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for truncated key, got: %v", err)
	}

	// 4. EncodeRequest with empty key
	reqEmpty := &transport.Request{OpCode: transport.OpPut, Key: nil, Value: []byte("v")}
	_, err = transport.EncodeRequest(reqEmpty)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for empty key, got: %v", err)
	}

	// 5. EncodeRequest with oversized key (> 65,535 bytes)
	reqOversizedKey := &transport.Request{
		OpCode: transport.OpPut,
		Key:    make([]byte, transport.MaxKeyLength+1),
		Value:  []byte("v"),
	}
	_, err = transport.EncodeRequest(reqOversizedKey)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for oversized key, got: %v", err)
	}

	// 6. EncodeRequest with oversized value (> 4 MiB)
	reqOversizedVal := &transport.Request{
		OpCode: transport.OpPut,
		Key:    []byte("k"),
		Value:  make([]byte, transport.MaxValueLength+1),
	}
	_, err = transport.EncodeRequest(reqOversizedVal)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for oversized value, got: %v", err)
	}
}

func TestCodec_MalformedBatchPayloads(t *testing.T) {
	// 1. Shorter than 4 bytes (no count)
	f1 := &transport.Frame{
		Header:  transport.Header{Magic: transport.Magic, OpCode: transport.OpBatch},
		Payload: []byte{0x00, 0x01},
	}
	_, err := transport.DecodeRequest(f1)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload, got: %v", err)
	}

	// 2. Count is 0
	f2 := &transport.Frame{
		Header:  transport.Header{Magic: transport.Magic, OpCode: transport.OpBatch},
		Payload: []byte{0x00, 0x00, 0x00, 0x00},
	}
	_, err = transport.DecodeRequest(f2)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for 0 ops, got: %v", err)
	}

	// 3. Count > MaxBatchOps (1024)
	var countBuf [4]byte
	binary.PutUint32(countBuf[:], 1025)
	f3 := &transport.Frame{
		Header:  transport.Header{Magic: transport.Magic, OpCode: transport.OpBatch},
		Payload: countBuf[:],
	}
	_, err = transport.DecodeRequest(f3)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for >1024 ops, got: %v", err)
	}

	// 4. Invalid batch op type (0x99)
	f4 := &transport.Frame{
		Header: transport.Header{Magic: transport.Magic, OpCode: transport.OpBatch},
		Payload: []byte{
			0x00, 0x00, 0x00, 0x01, // count = 1
			0x99,       // invalid op type
			0x00, 0x01, // key len = 1
			'k',
		},
	}
	_, err = transport.DecodeRequest(f4)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for invalid op type, got: %v", err)
	}

	// 5. Unexpected trailing bytes after declared operations
	f5 := &transport.Frame{
		Header: transport.Header{Magic: transport.Magic, OpCode: transport.OpBatch},
		Payload: []byte{
			0x00, 0x00, 0x00, 0x01, // count = 1
			0x02,                 // DELETE
			0x00, 0x02, 'k', '1', // key len = 2, "k1"
			0xDE, 0xAD, 0xBE, 0xEF, // trailing corrupt bytes
		},
	}
	_, err = transport.DecodeRequest(f5)
	if !stdErrors.Is(err, errors.ErrInvalidPayload) {
		t.Fatalf("expected ErrInvalidPayload for trailing bytes, got: %v", err)
	}
}

func TestCodec_BufferOwnershipImmutability(t *testing.T) {
	// Ensure that modifying the source frame buffer after DecodeRequest does NOT
	// corrupt the returned Request's Key or Value slices.
	sourcePayload := make([]byte, 2+4+5)
	binary.PutUint16(sourcePayload[0:2], 4)
	copy(sourcePayload[2:6], "keys")
	copy(sourcePayload[6:], "value")

	frame := &transport.Frame{
		Header: transport.Header{
			Magic:  transport.Magic,
			OpCode: transport.OpPut,
			SeqID:  10,
		},
		Payload: sourcePayload,
	}

	req, err := transport.DecodeRequest(frame)
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}

	// Poison the underlying source buffer
	for i := range sourcePayload {
		sourcePayload[i] = 0xFF
	}

	// Assert that req.Key and req.Value were not modified
	if string(req.Key) != "keys" {
		t.Fatalf("request.Key was corrupted by source buffer mutation! got %x, want 'keys'", req.Key)
	}
	if string(req.Value) != "value" {
		t.Fatalf("request.Value was corrupted by source buffer mutation! got %x, want 'value'", req.Value)
	}
}

func TestCodec_ResponseRoundTrip(t *testing.T) {
	testCases := []struct {
		name    string
		opCode  transport.OpCode
		status  transport.StatusCode
		seqID   uint64
		val     []byte
		exists  bool
		message string
	}{
		{
			name:   "GET success with value",
			opCode: transport.OpGet,
			status: transport.StatusOk,
			seqID:  100,
			val:    []byte("returned_database_value"),
		},
		{
			name:   "EXISTS true",
			opCode: transport.OpExists,
			status: transport.StatusOk,
			seqID:  101,
			exists: true,
		},
		{
			name:   "EXISTS false",
			opCode: transport.OpExists,
			status: transport.StatusOk,
			seqID:  102,
			exists: false,
		},
		{
			name:   "PUT success",
			opCode: transport.OpPut,
			status: transport.StatusOk,
			seqID:  103,
		},
		{
			name:   "DELETE success",
			opCode: transport.OpDelete,
			status: transport.StatusOk,
			seqID:  104,
		},
		{
			name:   "GET not found",
			opCode: transport.OpGet,
			status: transport.StatusKeyNotFound,
			seqID:  105,
		},
		{
			name:    "Internal storage error with message",
			opCode:  transport.OpPut,
			status:  transport.StatusError,
			seqID:   106,
			message: "WAL sync barrier failure: disk I/O timeout",
		},
		{
			name:    "Invalid request error",
			opCode:  transport.OpPut,
			status:  transport.StatusInvalidRequest,
			seqID:   107,
			message: "key exceeds 65535 bytes limit",
		},
		{
			name:    "Throttled write error",
			opCode:  transport.OpPut,
			status:  transport.StatusThrottled,
			seqID:   108,
			message: "memory limit exceeded: backpressure active",
		},
		{
			name:    "Server closed error",
			opCode:  transport.OpPut,
			status:  transport.StatusServerClosed,
			seqID:   109,
			message: "server is shutting down",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			resp := &transport.Response{
				OpCode:  tc.opCode,
				Status:  tc.status,
				SeqID:   tc.seqID,
				Value:   tc.val,
				Exists:  tc.exists,
				Message: tc.message,
			}

			frame, err := transport.EncodeResponse(resp)
			if err != nil {
				t.Fatalf("EncodeResponse failed: %v", err)
			}

			decoded, err := transport.DecodeResponse(frame)
			if err != nil {
				t.Fatalf("DecodeResponse failed: %v", err)
			}

			if decoded.OpCode != tc.opCode {
				t.Errorf("opcode mismatch: got %v, want %v", decoded.OpCode, tc.opCode)
			}
			if decoded.Status != tc.status {
				t.Errorf("status mismatch: got %v, want %v", decoded.Status, tc.status)
			}
			if decoded.SeqID != tc.seqID {
				t.Errorf("seqID mismatch: got %d, want %d", decoded.SeqID, tc.seqID)
			}
			if tc.status == transport.StatusOk {
				if tc.opCode == transport.OpGet && !bytes.Equal(decoded.Value, tc.val) {
					t.Errorf("value mismatch: got %x, want %x", decoded.Value, tc.val)
				}
				if tc.opCode == transport.OpExists && decoded.Exists != tc.exists {
					t.Errorf("exists mismatch: got %v, want %v", decoded.Exists, tc.exists)
				}
			} else {
				if decoded.Message != tc.message {
					t.Errorf("message mismatch: got %q, want %q", decoded.Message, tc.message)
				}
			}
		})
	}
}

func TestCodec_StreamPipeIntegration(t *testing.T) {
	// Test ReadRequest / WriteRequest across an in-memory io.Pipe
	pr, pw := io.Pipe()

	expectedReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  4242,
		Key:    []byte("pipe_test_key"),
		Value:  []byte("pipe_test_val"),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- transport.WriteRequest(pw, expectedReq)
		_ = pw.Close()
	}()

	receivedReq, err := transport.ReadRequest(pr)
	if err != nil {
		t.Fatalf("ReadRequest from pipe failed: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("WriteRequest to pipe failed: %v", writeErr)
	}

	if receivedReq.OpCode != expectedReq.OpCode || receivedReq.SeqID != expectedReq.SeqID ||
		!bytes.Equal(receivedReq.Key, expectedReq.Key) || !bytes.Equal(receivedReq.Value, expectedReq.Value) {
		t.Fatalf("pipe request mismatch: got %+v, want %+v", receivedReq, expectedReq)
	}

	// Test ReadResponse / WriteResponse across an in-memory io.Pipe
	respR, respW := io.Pipe()

	expectedResp := &transport.Response{
		OpCode:  transport.OpGet,
		Status:  transport.StatusOk,
		SeqID:   4242,
		Value:   []byte("pipe_response_data"),
		Message: "",
	}

	go func() {
		_ = transport.WriteResponse(respW, expectedResp)
		_ = respW.Close()
	}()

	receivedResp, err := transport.ReadResponse(respR)
	if err != nil {
		t.Fatalf("ReadResponse from pipe failed: %v", err)
	}
	if receivedResp.OpCode != expectedResp.OpCode || receivedResp.Status != expectedResp.Status ||
		receivedResp.SeqID != expectedResp.SeqID || !bytes.Equal(receivedResp.Value, expectedResp.Value) {
		t.Fatalf("pipe response mismatch: got %+v, want %+v", receivedResp, expectedResp)
	}
}

func TestEncodeResponse_MaxPayloadLengthExceeded(t *testing.T) {
	// Test error message exceeding MaxPayloadLength
	hugeMsg := string(make([]byte, transport.MaxPayloadLength+1))
	resp := &transport.Response{
		OpCode:  transport.OpGet,
		Status:  transport.StatusError,
		SeqID:   1,
		Message: hugeMsg,
	}

	_, err := transport.EncodeResponse(resp)
	if err == nil {
		t.Fatal("expected FrameTooLargeError for oversized response message, got nil")
	}
	var frameErr *errors.FrameTooLargeError
	if !stdErrors.As(err, &frameErr) {
		t.Fatalf("expected *errors.FrameTooLargeError, got: %T (%v)", err, err)
	}
	if frameErr.PayloadSize != transport.MaxPayloadLength+1 {
		t.Fatalf("expected PayloadSize %d, got %d", transport.MaxPayloadLength+1, frameErr.PayloadSize)
	}

	// Test stats payload exceeding MaxPayloadLength
	statsResp := &transport.Response{
		OpCode: transport.OpStats,
		Status: transport.StatusOk,
		SeqID:  2,
		Value:  make([]byte, transport.MaxPayloadLength+1),
	}
	_, err = transport.EncodeResponse(statsResp)
	if err == nil {
		t.Fatal("expected FrameTooLargeError for oversized stats response, got nil")
	}
	if !stdErrors.As(err, &frameErr) {
		t.Fatalf("expected *errors.FrameTooLargeError, got: %T (%v)", err, err)
	}
}

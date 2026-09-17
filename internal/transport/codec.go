package transport

import (
	"fmt"
	"io"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// DecodeRequest unpacks and validates a typed Request from a raw binary Frame.
//
// Invariants:
//   - Verifies that OpCode is recognized; rejects invalid opcodes with *errors.InvalidOpCodeError.
//   - Verifies per-operation payload layout and boundary constraints.
//   - Enforces key constraints (1 <= len(key) <= 65,535) and value constraints (len(val) <= 4 MiB).
//   - Rejects unexpected trailing bytes in payloads with *errors.InvalidPayloadError.
//   - Buffer Safety: Returns independent, defensive byte copies of Key, Value, and Batch slices,
//     ensuring the returned Request remains immutable even if frame buffers are reused.
func DecodeRequest(f *Frame) (*Request, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}

	if !f.Header.OpCode.Valid() {
		return nil, &errors.InvalidOpCodeError{OpCode: byte(f.Header.OpCode)}
	}

	req := &Request{
		OpCode: f.Header.OpCode,
		Flags:  f.Header.Flags,
		SeqID:  f.Header.SeqID,
	}

	payload := f.Payload

	switch req.OpCode {
	case OpPut:
		// Payload Layout: [ KeyLen (2B uint16) | Key (KeyLen B) | Value (remainder) ]
		if len(payload) < 2 {
			return nil, &errors.InvalidPayloadError{Reason: "PUT payload shorter than minimum 2-byte key length prefix"}
		}
		kLen := int(binary.GetUint16(payload[0:2]))
		if kLen < 1 || kLen > MaxKeyLength {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("PUT key length %d out of bounds [1, %d]", kLen, MaxKeyLength),
			}
		}
		if len(payload) < 2+kLen {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("PUT payload size %d shorter than declared key size %d", len(payload), kLen),
			}
		}

		vLen := len(payload) - (2 + kLen)
		if vLen > MaxValueLength {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("PUT value length %d exceeds maximum %d", vLen, MaxValueLength),
			}
		}

		// Defensive copies for memory isolation
		keyCopy := make([]byte, kLen)
		copy(keyCopy, payload[2:2+kLen])

		valCopy := make([]byte, vLen)
		copy(valCopy, payload[2+kLen:])

		req.Key = keyCopy
		req.Value = valCopy

	case OpGet, OpDelete, OpExists:
		// Payload Layout: [ Key (raw bytes, length equals PayloadLength) ]
		if len(payload) < 1 {
			return nil, &errors.InvalidPayloadError{Reason: fmt.Sprintf("%s requires non-empty key", req.OpCode)}
		}
		if len(payload) > MaxKeyLength {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("%s key length %d exceeds maximum %d", req.OpCode, len(payload), MaxKeyLength),
			}
		}

		keyCopy := make([]byte, len(payload))
		copy(keyCopy, payload)
		req.Key = keyCopy

	case OpBatch:
		// Payload Layout: [ Count (4B uint32) | Entries... ]
		// Entry: [ OpType (1B) | KeyLen (2B uint16) | Key (KeyLen B) | [ValLen (4B uint32) | Val (ValLen B)] ]
		if len(payload) < 4 {
			return nil, &errors.InvalidPayloadError{Reason: "BATCH payload missing 4-byte operation count"}
		}
		count := binary.GetUint32(payload[0:4])
		if count == 0 {
			return nil, &errors.InvalidPayloadError{Reason: "BATCH operation count must be greater than zero"}
		}
		if count > MaxBatchOps {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("BATCH operation count %d exceeds maximum %d", count, MaxBatchOps),
			}
		}

		offset := 4
		ops := make([]BatchOp, 0, count)
		for i := uint32(0); i < count; i++ {
			if offset+3 > len(payload) {
				return nil, &errors.InvalidPayloadError{Reason: fmt.Sprintf("batch op %d header truncated", i)}
			}
			opType := BatchOpType(payload[offset])
			if !opType.Valid() {
				return nil, &errors.InvalidPayloadError{Reason: fmt.Sprintf("invalid batch operation type: 0x%02x", byte(opType))}
			}
			kLen := int(binary.GetUint16(payload[offset+1 : offset+3]))
			offset += 3
			if kLen < 1 || kLen > MaxKeyLength {
				return nil, &errors.InvalidPayloadError{
					Reason: fmt.Sprintf("batch op %d key length %d out of bounds", i, kLen),
				}
			}
			if offset+kLen > len(payload) {
				return nil, &errors.InvalidPayloadError{Reason: fmt.Sprintf("batch op %d key data truncated", i)}
			}
			key := make([]byte, kLen)
			copy(key, payload[offset:offset+kLen])
			offset += kLen

			var val []byte
			if opType == BatchOpPut {
				if offset+4 > len(payload) {
					return nil, &errors.InvalidPayloadError{Reason: fmt.Sprintf("batch op %d value length truncated", i)}
				}
				vLen := int(binary.GetUint32(payload[offset : offset+4]))
				offset += 4
				if vLen < 0 || vLen > MaxValueLength {
					return nil, &errors.InvalidPayloadError{
						Reason: fmt.Sprintf("batch op %d value length %d out of bounds", i, vLen),
					}
				}
				if offset+vLen > len(payload) {
					return nil, &errors.InvalidPayloadError{Reason: fmt.Sprintf("batch op %d value data truncated", i)}
				}
				val = make([]byte, vLen)
				copy(val, payload[offset:offset+vLen])
				offset += vLen
			}

			ops = append(ops, BatchOp{
				Type:  opType,
				Key:   key,
				Value: val,
			})
		}

		if offset != len(payload) {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("unexpected trailing %d bytes in BATCH payload", len(payload)-offset),
			}
		}
		req.Batch = ops

	case OpStats:
		if len(payload) != 0 {
			return nil, &errors.InvalidPayloadError{Reason: "STATS request must not contain a payload"}
		}

	default:
		return nil, &errors.InvalidOpCodeError{OpCode: byte(req.OpCode)}
	}

	return req, nil
}

// EncodeRequest serializes a strongly-typed Request into a binary protocol Frame.
//
// Invariants:
//   - Validates opcode, key length, and value length constraints.
//   - Encodes fields using Big-Endian network byte order.
//   - Sets Magic = 0x4C415454 and SeqID.
func EncodeRequest(req *Request) (*Frame, error) {
	if req == nil {
		return nil, errors.ErrNilReceiver
	}
	if !req.OpCode.Valid() {
		return nil, &errors.InvalidOpCodeError{OpCode: byte(req.OpCode)}
	}

	var payload []byte

	switch req.OpCode {
	case OpPut:
		kLen := len(req.Key)
		if kLen < 1 || kLen > MaxKeyLength {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("PUT key length %d out of bounds [1, %d]", kLen, MaxKeyLength),
			}
		}
		vLen := len(req.Value)
		if vLen > MaxValueLength {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("PUT value length %d exceeds maximum %d", vLen, MaxValueLength),
			}
		}
		payload = make([]byte, 2+kLen+vLen)
		binary.PutUint16(payload[0:2], uint16(kLen))
		copy(payload[2:2+kLen], req.Key)
		copy(payload[2+kLen:], req.Value)

	case OpGet, OpDelete, OpExists:
		kLen := len(req.Key)
		if kLen < 1 || kLen > MaxKeyLength {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("%s key length %d out of bounds [1, %d]", req.OpCode, kLen, MaxKeyLength),
			}
		}
		payload = make([]byte, kLen)
		copy(payload, req.Key)

	case OpBatch:
		if len(req.Batch) == 0 {
			return nil, &errors.InvalidPayloadError{Reason: "BATCH must contain at least one operation"}
		}
		if len(req.Batch) > MaxBatchOps {
			return nil, &errors.InvalidPayloadError{
				Reason: fmt.Sprintf("BATCH operations count %d exceeds maximum %d", len(req.Batch), MaxBatchOps),
			}
		}

		// Calculate total payload size with integer overflow checks
		totalBytes := uint64(4) // 4B count
		for i, op := range req.Batch {
			if !op.Type.Valid() {
				return nil, &errors.InvalidPayloadError{
					Reason: fmt.Sprintf("batch op %d has invalid type: 0x%02x", i, byte(op.Type)),
				}
			}
			if len(op.Key) < 1 || len(op.Key) > MaxKeyLength {
				return nil, &errors.InvalidPayloadError{
					Reason: fmt.Sprintf("batch op %d key length %d out of bounds", i, len(op.Key)),
				}
			}
			totalBytes += 1 + 2 + uint64(len(op.Key)) // OpType + KeyLen + Key
			if op.Type == BatchOpPut {
				if len(op.Value) > MaxValueLength {
					return nil, &errors.InvalidPayloadError{
						Reason: fmt.Sprintf("batch op %d value length %d exceeds maximum", i, len(op.Value)),
					}
				}
				totalBytes += 4 + uint64(len(op.Value)) // ValLen + Value
			}
			if totalBytes > uint64(MaxPayloadLength) {
				return nil, &errors.FrameTooLargeError{PayloadSize: uint32(totalBytes), MaxSize: MaxPayloadLength}
			}
		}

		payload = make([]byte, totalBytes)
		binary.PutUint32(payload[0:4], uint32(len(req.Batch)))
		offset := 4
		for _, op := range req.Batch {
			payload[offset] = byte(op.Type)
			binary.PutUint16(payload[offset+1:offset+3], uint16(len(op.Key)))
			offset += 3
			copy(payload[offset:offset+len(op.Key)], op.Key)
			offset += len(op.Key)

			if op.Type == BatchOpPut {
				binary.PutUint32(payload[offset:offset+4], uint32(len(op.Value)))
				offset += 4
				copy(payload[offset:offset+len(op.Value)], op.Value)
				offset += len(op.Value)
			}
		}

	case OpStats:
		payload = nil
	}

	return &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        req.OpCode,
			Flags:         req.Flags,
			SeqID:         req.SeqID,
			PayloadLength: uint32(len(payload)),
		},
		Payload: payload,
	}, nil
}

// DecodeResponse unpacks and validates a typed Response from a raw binary Frame.
//
// Invariants:
//   - Verifies that Status is a recognized StatusCode.
//   - When Status == StatusOk:
//   - OpGet: extracts defensive copy of Value.
//   - OpExists: extracts boolean byte (0x01 = true, 0x00 = false).
//   - When Status != StatusOk:
//   - extracts error diagnostic message string.
func DecodeResponse(f *Frame) (*Response, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	if !f.Header.OpCode.Valid() {
		return nil, &errors.InvalidOpCodeError{OpCode: byte(f.Header.OpCode)}
	}

	status := f.Header.Status
	if !status.Valid() {
		return nil, &errors.InvalidStatusError{Status: byte(status)}
	}

	resp := &Response{
		OpCode: f.Header.OpCode,
		Status: status,
		SeqID:  f.Header.SeqID,
	}

	if status == StatusOk {
		switch f.Header.OpCode {
		case OpGet:
			valCopy := make([]byte, len(f.Payload))
			copy(valCopy, f.Payload)
			resp.Value = valCopy
		case OpExists:
			if len(f.Payload) != 1 {
				return nil, &errors.InvalidPayloadError{
					Reason: fmt.Sprintf("EXISTS response payload must be exactly 1 byte, got %d", len(f.Payload)),
				}
			}
			resp.Exists = f.Payload[0] == 0x01
		case OpPut, OpDelete, OpBatch:
			if len(f.Payload) != 0 {
				return nil, &errors.InvalidPayloadError{
					Reason: fmt.Sprintf("%s success response must not have payload, got %d bytes", f.Header.OpCode, len(f.Payload)),
				}
			}
		case OpStats:
			valCopy := make([]byte, len(f.Payload))
			copy(valCopy, f.Payload)
			resp.Value = valCopy
		}
	} else {
		// Non-OK status: payload is human-readable diagnostic error message
		resp.Message = string(f.Payload)
	}

	return resp, nil
}

// EncodeResponse serializes a strongly-typed Response into a binary protocol Frame.
//
// Invariants:
//   - Sets Magic = 0x4C415454 and SeqID matching the client's request.
//   - Encodes Status into the Flags/Status byte (byte 5) of the 18-byte header.
//   - Success: encodes Value (for GET) or Exists byte (for EXISTS).
//   - Failure: encodes Message string into payload.
func EncodeResponse(resp *Response) (*Frame, error) {
	if resp == nil {
		return nil, errors.ErrNilReceiver
	}
	if !resp.OpCode.Valid() {
		return nil, &errors.InvalidOpCodeError{OpCode: byte(resp.OpCode)}
	}
	if !resp.Status.Valid() {
		return nil, &errors.InvalidStatusError{Status: byte(resp.Status)}
	}

	var payload []byte

	if resp.Status == StatusOk {
		switch resp.OpCode {
		case OpGet:
			if len(resp.Value) > MaxValueLength {
				return nil, &errors.InvalidPayloadError{
					Reason: fmt.Sprintf("GET response value length %d exceeds maximum %d", len(resp.Value), MaxValueLength),
				}
			}
			payload = make([]byte, len(resp.Value))
			copy(payload, resp.Value)
		case OpExists:
			if resp.Exists {
				payload = []byte{0x01}
			} else {
				payload = []byte{0x00}
			}
		case OpStats:
			payload = make([]byte, len(resp.Value))
			copy(payload, resp.Value)
		default:
			payload = nil
		}
	} else {
		payload = []byte(resp.Message)
	}

	return &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        resp.OpCode,
			Flags:         byte(resp.Status),
			Status:        resp.Status,
			SeqID:         resp.SeqID,
			PayloadLength: uint32(len(payload)),
		},
		Payload: payload,
	}, nil
}

// ReadRequest reads, validates, and decodes a typed Request from an incoming stream r.
func ReadRequest(r io.Reader) (*Request, error) {
	frame, err := DecodeFrame(r)
	if err != nil {
		return nil, err
	}
	return DecodeRequest(frame)
}

// WriteRequest serializes and writes a typed Request as a framed message to w.
func WriteRequest(w io.Writer, req *Request) error {
	frame, err := EncodeRequest(req)
	if err != nil {
		return err
	}
	return EncodeFrame(w, frame)
}

// ReadResponse reads, validates, and decodes a typed Response from an incoming stream r.
func ReadResponse(r io.Reader) (*Response, error) {
	frame, err := DecodeFrame(r)
	if err != nil {
		return nil, err
	}
	return DecodeResponse(frame)
}

// WriteResponse serializes and writes a typed Response as a framed message to w.
func WriteResponse(w io.Writer, resp *Response) error {
	frame, err := EncodeResponse(resp)
	if err != nil {
		return err
	}
	return EncodeFrame(w, frame)
}

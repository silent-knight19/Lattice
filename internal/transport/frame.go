package transport

import (
	"fmt"
	"hash/crc32"
	"io"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const pooledBufferSize = 64 * 1024 // 64 KiB covers standard key-value operations

var (
	pool64K = sync.Pool{
		New: func() any {
			b := make([]byte, pooledBufferSize)
			return &b
		},
	}
	poolMax = sync.Pool{
		New: func() any {
			b := make([]byte, MaxPayloadLength)
			return &b
		},
	}
)

func getPooledBuffer(size uint32) (*[]byte, bool) {
	if size <= pooledBufferSize {
		return pool64K.Get().(*[]byte), true
	}
	return poolMax.Get().(*[]byte), false
}

func putPooledBuffer(buf *[]byte, is64K bool) {
	if is64K {
		pool64K.Put(buf)
	} else {
		poolMax.Put(buf)
	}
}

// DecodeHeader reads and decodes an 18-byte fixed frame header from r.
//
// Invariants:
//   - Reads exactly HeaderSize (18) bytes.
//   - Verifies magic number equals 0x4C415454; returns *errors.InvalidMagicError if mismatched.
//   - Verifies PayloadLength <= MaxPayloadLength (5 MiB); returns *errors.FrameTooLargeError if exceeded.
//   - Zero heap allocations.
func DecodeHeader(r io.Reader) (*Header, error) {
	var buf [HeaderSize]byte
	n, err := io.ReadFull(r, buf[:])
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: read %d/%d header bytes: %v", errors.ErrFrameTruncated, n, HeaderSize, err)
	}
	return DecodeHeaderBytes(buf[:])
}

// DecodeHeaderBytes parses and validates an 18-byte header from a byte slice.
//
// Invariants:
//   - Requires len(src) >= HeaderSize (18).
//   - Validates Magic and PayloadLength bounds.
//   - Zero heap allocations.
func DecodeHeaderBytes(src []byte) (*Header, error) {
	if len(src) < HeaderSize {
		return nil, fmt.Errorf("%w: header buffer has %d bytes, need %d", errors.ErrFrameTruncated, len(src), HeaderSize)
	}

	magic := binary.GetUint32(src[0:4])
	if magic != Magic {
		return nil, &errors.InvalidMagicError{Expected: Magic, Actual: magic}
	}

	payloadLen := binary.GetUint32(src[14:18])
	if payloadLen > MaxPayloadLength {
		return nil, &errors.FrameTooLargeError{PayloadSize: payloadLen, MaxSize: MaxPayloadLength}
	}

	opCode := OpCode(src[4])
	flags := src[5]
	status := StatusCode(src[5])
	seqID := binary.GetUint64(src[6:14])

	return &Header{
		Magic:         magic,
		OpCode:        opCode,
		Flags:         flags,
		Status:        status,
		SeqID:         seqID,
		PayloadLength: payloadLen,
	}, nil
}

// Encode serializes the Header into dst using Big-Endian network byte order.
// Requires len(dst) >= HeaderSize (18). Panics if dst is undersized.
func (h *Header) Encode(dst []byte) {
	_ = dst[17] // early bounds check
	binary.PutUint32(dst[0:4], h.Magic)
	dst[4] = byte(h.OpCode)
	// If Status is set and non-zero while Flags is zero, use Status; otherwise Flags.
	if h.Status != StatusOk && h.Flags == FlagNone {
		dst[5] = byte(h.Status)
	} else {
		dst[5] = h.Flags
	}
	binary.PutUint64(dst[6:14], h.SeqID)
	binary.PutUint32(dst[14:18], h.PayloadLength)
}

// DecodeFrame reads and validates a complete length-prefixed frame from r.
//
// Framing Lifecycle & Security Checks:
//  1. Reads exactly HeaderSize (18) bytes.
//  2. Validates Magic (0x4C415454) and PayloadLength (<= 5 MiB).
//     FAIL-FAST: Rejection occurs BEFORE allocating any payload memory.
//  3. Allocates payload buffer only after length is proven valid.
//  4. Reads declared payload bytes.
//  5. Reads 4-byte CRC32-IEEE checksum trailer.
//  6. Verifies CRC32 over header + payload; returns *errors.ChecksumMismatchError if corrupted.
func DecodeFrame(r io.Reader) (*Frame, error) {
	var headerBuf [HeaderSize]byte
	n, err := io.ReadFull(r, headerBuf[:])
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: read %d/%d header bytes: %v", errors.ErrFrameTruncated, n, HeaderSize, err)
	}

	hdr, err := DecodeHeaderBytes(headerBuf[:])
	if err != nil {
		return nil, err
	}

	var payload []byte
	if hdr.PayloadLength > 0 {
		bufPtr, is64K := getPooledBuffer(hdr.PayloadLength)
		tempBuf := (*bufPtr)[:hdr.PayloadLength]

		pn, pErr := io.ReadFull(r, tempBuf)
		if pErr != nil {
			putPooledBuffer(bufPtr, is64K)
			return nil, fmt.Errorf("%w: read %d/%d payload bytes: %v", errors.ErrFrameTruncated, pn, hdr.PayloadLength, pErr)
		}

		var trailerBuf [TrailerSize]byte
		tn, tErr := io.ReadFull(r, trailerBuf[:])
		if tErr != nil {
			putPooledBuffer(bufPtr, is64K)
			return nil, fmt.Errorf("%w: read %d/%d crc trailer bytes: %v", errors.ErrFrameTruncated, tn, TrailerSize, tErr)
		}

		// Compute CRC32-IEEE incrementally over header and payload with zero heap allocation.
		computedCRC := crc32.ChecksumIEEE(headerBuf[:])
		computedCRC = crc32.Update(computedCRC, crc32.IEEETable, tempBuf)

		expectedCRC := binary.GetUint32(trailerBuf[:])
		if computedCRC != expectedCRC {
			putPooledBuffer(bufPtr, is64K)
			return nil, &errors.ChecksumMismatchError{Expected: expectedCRC, Actual: computedCRC}
		}

		payload = make([]byte, hdr.PayloadLength)
		copy(payload, tempBuf)
		putPooledBuffer(bufPtr, is64K)

		return &Frame{
			Header:  *hdr,
			Payload: payload,
			CRC:     expectedCRC,
		}, nil
	}

	var trailerBuf [TrailerSize]byte
	tn, tErr := io.ReadFull(r, trailerBuf[:])
	if tErr != nil {
		return nil, fmt.Errorf("%w: read %d/%d crc trailer bytes: %v", errors.ErrFrameTruncated, tn, TrailerSize, tErr)
	}

	computedCRC := crc32.ChecksumIEEE(headerBuf[:])
	expectedCRC := binary.GetUint32(trailerBuf[:])
	if computedCRC != expectedCRC {
		return nil, &errors.ChecksumMismatchError{Expected: expectedCRC, Actual: computedCRC}
	}

	return &Frame{
		Header:  *hdr,
		Payload: nil,
		CRC:     expectedCRC,
	}, nil
}

// EncodeFrame serializes f into w as a complete on-wire binary frame.
//
// Invariants:
//   - Sets Magic = 0x4C415454 automatically if uninitialized.
//   - Enforces PayloadLength == len(f.Payload) <= MaxPayloadLength.
//   - Calculates CRC32-IEEE over Header (18B) + Payload (Var).
//   - Writes Header + Payload + CRC Trailer.
func EncodeFrame(w io.Writer, f *Frame) error {
	if f == nil {
		return errors.ErrNilReceiver
	}

	payloadLen := len(f.Payload)
	if uint64(payloadLen) > uint64(MaxPayloadLength) {
		return &errors.FrameTooLargeError{PayloadSize: uint32(payloadLen), MaxSize: MaxPayloadLength}
	}

	f.Header.Magic = Magic
	f.Header.PayloadLength = uint32(payloadLen)

	totalWireSize := HeaderSize + payloadLen + TrailerSize
	wireBuf := make([]byte, totalWireSize)

	f.Header.Encode(wireBuf[:HeaderSize])
	if payloadLen > 0 {
		copy(wireBuf[HeaderSize:HeaderSize+payloadLen], f.Payload)
	}

	// Calculate CRC32 over header + payload
	f.CRC = crc32.ChecksumIEEE(wireBuf[:HeaderSize+payloadLen])
	binary.PutUint32(wireBuf[HeaderSize+payloadLen:], f.CRC)

	_, err := w.Write(wireBuf)
	return err
}

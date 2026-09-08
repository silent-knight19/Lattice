package wal

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// HeaderSize is the fixed size in bytes of the WAL physical binary record header.
// Layout: [ CRC32 (4B) | RecordType (1B) | SeqNum (8B) | Timestamp (8B) ] = 21 bytes.
const HeaderSize = 21

// RecordHeaderLen is an alias for HeaderSize, matching storage engine terminology.
const RecordHeaderLen = HeaderSize

// RecordType represents the 1-byte framing marker at byte offset 4 of every WAL record.
type RecordType byte

const (
	// RecordTypeInvalid represents an uninitialized or corrupted record type (0x00).
	RecordTypeInvalid RecordType = 0x00

	// RecordTypePut represents a standard key-value insertion or overwrite (0x01).
	RecordTypePut RecordType = 0x01

	// RecordTypeDelete represents a tombstone deletion marker (0x02).
	RecordTypeDelete RecordType = 0x02

	// RecordTypeBatchStart represents the opening marker of an atomic multi-operation batch (0x03).
	RecordTypeBatchStart RecordType = 0x03

	// RecordTypeBatchCommit represents the commit marker completing an atomic batch (0x04).
	RecordTypeBatchCommit RecordType = 0x04
)

// Valid reports whether the record type is one of the recognized operations (PUT, DELETE, BATCH_START, BATCH_COMMIT).
func (t RecordType) Valid() bool {
	return t >= RecordTypePut && t <= RecordTypeBatchCommit
}

// Validate returns nil if the record type is valid, or an *errors.InvalidRecordTypeError if invalid.
func (t RecordType) Validate() error {
	if t.Valid() {
		return nil
	}
	return &errors.InvalidRecordTypeError{Type: byte(t)}
}

// String returns the human-readable string representation of the record type.
func (t RecordType) String() string {
	switch t {
	case RecordTypePut:
		return "PUT"
	case RecordTypeDelete:
		return "DELETE"
	case RecordTypeBatchStart:
		return "BATCH_START"
	case RecordTypeBatchCommit:
		return "BATCH_COMMIT"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", byte(t))
	}
}

// OpType converts a WAL RecordType into a storage engine binary.OpType.
// Returns binary.OpTypeInvalid and an error if the RecordType is not a direct key-value operation
// (e.g. for BATCH_START or BATCH_COMMIT).
func (t RecordType) OpType() (binary.OpType, error) {
	switch t {
	case RecordTypePut:
		return binary.OpTypePut, nil
	case RecordTypeDelete:
		return binary.OpTypeDelete, nil
	default:
		return binary.OpTypeInvalid, &errors.InvalidRecordTypeError{Type: byte(t)}
	}
}

// ParseRecordType parses a raw byte into a validated RecordType.
// Returns RecordTypeInvalid and an *errors.InvalidRecordTypeError if unrecognized.
func ParseRecordType(b byte) (RecordType, error) {
	t := RecordType(b)
	if err := t.Validate(); err != nil {
		return RecordTypeInvalid, err
	}
	return t, nil
}

// RecordHeader defines the fixed 21-byte physical framing header prepended
// to every record stored in a Write-Ahead Log segment.
//
// Binary Framing Layout:
//
//	Offset  0..3  : CRC32-IEEE (4 bytes, uint32, Big-Endian)
//	Offset  4     : RecordType (1 byte, uint8)
//	Offset  5..12 : SeqNum     (8 bytes, uint64, Big-Endian)
//	Offset 13..20 : Timestamp  (8 bytes, uint64, Big-Endian)
type RecordHeader struct {
	CRC       uint32
	Type      RecordType
	SeqNum    binary.SeqNum
	Timestamp uint64
}

// Equal reports whether h and other have identical header fields.
func (h RecordHeader) Equal(other RecordHeader) bool {
	return h.CRC == other.CRC &&
		h.Type == other.Type &&
		h.SeqNum == other.SeqNum &&
		h.Timestamp == other.Timestamp
}

// String returns a human-readable representation of the RecordHeader.
func (h RecordHeader) String() string {
	return fmt.Sprintf("RecordHeader(CRC=0x%08x, Type=%s, SeqNum=%s, Timestamp=%d)",
		h.CRC, h.Type.String(), h.SeqNum.String(), h.Timestamp)
}

// EncodeHeader serializes the 21-byte RecordHeader into buf using Big-Endian byte ordering.
//
// Buffer contract:
//   - Requires len(buf) >= HeaderSize (21 bytes).
//   - If len(buf) < HeaderSize or buf == nil, it panics with a runtime bounds error.
//   - Bounds check is performed eagerly at index 20 before any bytes are written,
//     preventing partial or torn writes to undersized buffers.
//   - Trailing bytes beyond HeaderSize are completely untouched.
//   - Zero heap allocations.
func EncodeHeader(buf []byte, h RecordHeader) {
	_ = buf[HeaderSize-1] // Early bounds check: panics before partial write if len(buf) < 21
	binary.PutUint32(buf[0:4], h.CRC)
	buf[4] = byte(h.Type)
	binary.PutUint64(buf[5:13], uint64(h.SeqNum))
	binary.PutUint64(buf[13:21], h.Timestamp)
}

// AppendHeader serializes h into a 21-byte header and appends it to dst,
// returning the extended slice.
//
// Zero heap allocations when dst has sufficient spare capacity (cap(dst) - len(dst) >= 21).
func AppendHeader(dst []byte, h RecordHeader) []byte {
	var b [HeaderSize]byte
	EncodeHeader(b[:], h)
	return append(dst, b[:]...)
}

// DecodeHeader deserializes a 21-byte RecordHeader from buf using Big-Endian byte ordering.
//
// Return contract:
//   - On success: returns (RecordHeader, nil).
//   - If len(buf) < HeaderSize: returns (RecordHeader{}, errors.ErrHeaderTruncated).
//   - If the RecordType byte is invalid: returns (RecordHeader{}, *errors.InvalidRecordTypeError).
//   - Trailing bytes beyond HeaderSize are untouched and ignored.
//   - Zero heap allocations.
func DecodeHeader(buf []byte) (RecordHeader, error) {
	if len(buf) < HeaderSize {
		return RecordHeader{}, errors.ErrHeaderTruncated
	}

	crc := binary.GetUint32(buf[0:4])
	recType, err := ParseRecordType(buf[4])
	if err != nil {
		return RecordHeader{}, err
	}
	seqNum := binary.SeqNum(binary.GetUint64(buf[5:13]))
	timestamp := binary.GetUint64(buf[13:21])

	return RecordHeader{
		CRC:       crc,
		Type:      recType,
		SeqNum:    seqNum,
		Timestamp: timestamp,
	}, nil
}

package wal

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"io"

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

// MinRecordSize is the minimum serialized size in bytes of any valid WAL record:
// HeaderSize (21B) + KeyLength (2B) + ValueLength (4B) = 27 bytes (e.g. for batch markers).
const MinRecordSize = HeaderSize + 2 + 4

// Record represents a complete Write-Ahead Log entry comprising the physical framing header,
// operation metadata, user key, and payload value.
//
// In-Memory Representation:
//   - CRC: The 32-bit CRC32-IEEE checksum computed across all bytes following the CRC field.
//     When encoding, CRC is automatically computed and written to the record header.
//     When decoding, CRC is verified against the payload; Record is returned only if valid.
//   - Type: WAL framing record type (PUT, DELETE, BATCH_START, BATCH_COMMIT).
//   - SeqNum: Total database operation sequence number.
//   - Timestamp: Unix nanosecond timestamp when sequenced.
//   - Key: User key payload. Must be empty for batch markers.
//   - Value: User value payload. Must be empty for tombstone DELETE and batch markers.
type Record struct {
	CRC       uint32
	Type      RecordType
	SeqNum    binary.SeqNum
	Timestamp uint64
	Key       []byte
	Value     []byte
}

// Header returns the RecordHeader corresponding to this Record.
func (r Record) Header() RecordHeader {
	return RecordHeader{
		CRC:       r.CRC,
		Type:      r.Type,
		SeqNum:    r.SeqNum,
		Timestamp: r.Timestamp,
	}
}

// Equal reports whether r and other have identical fields and byte payloads.
// If both records have non-zero CRC values, the CRCs must match.
func (r Record) Equal(other Record) bool {
	if r.CRC != 0 && other.CRC != 0 && r.CRC != other.CRC {
		return false
	}
	return r.Type == other.Type &&
		r.SeqNum == other.SeqNum &&
		r.Timestamp == other.Timestamp &&
		bytes.Equal(r.Key, other.Key) &&
		bytes.Equal(r.Value, other.Value)
}

// String returns a human-readable representation of the Record.
// Note: To prevent sensitive data leakage, payload contents are represented by their lengths.
func (r Record) String() string {
	return fmt.Sprintf("Record(CRC=0x%08x, Type=%s, SeqNum=%s, Timestamp=%d, KeyLen=%d, ValLen=%d)",
		r.CRC, r.Type.String(), r.SeqNum.String(), r.Timestamp, len(r.Key), len(r.Value))
}

// Validate verifies that the Record satisfies all architectural storage invariants:
//   - RecordType must be one of PUT, DELETE, BATCH_START, BATCH_COMMIT.
//   - PUT: Key length 1..65,535 bytes; Value length 0..4,194,304 bytes.
//   - DELETE: Key length 1..65,535 bytes; Value length must be exactly 0 (tombstone).
//   - BATCH_START / BATCH_COMMIT: Key and Value lengths must both be exactly 0.
func (r Record) Validate() error {
	if err := r.Type.Validate(); err != nil {
		return err
	}

	switch r.Type {
	case RecordTypePut:
		if err := binary.ValidateKey(r.Key); err != nil {
			return err
		}
		if err := binary.ValidateValue(r.Value); err != nil {
			return err
		}
	case RecordTypeDelete:
		if err := binary.ValidateKey(r.Key); err != nil {
			return err
		}
		if len(r.Value) > 0 {
			return &errors.InvalidRecordPayloadError{
				Type:   byte(r.Type),
				Reason: "delete tombstone cannot have a value payload",
			}
		}
	case RecordTypeBatchStart, RecordTypeBatchCommit:
		if len(r.Key) > 0 {
			return &errors.InvalidRecordPayloadError{
				Type:   byte(r.Type),
				Reason: "batch marker cannot have a key payload",
			}
		}
		if len(r.Value) > 0 {
			return &errors.InvalidRecordPayloadError{
				Type:   byte(r.Type),
				Reason: "batch marker cannot have a value payload",
			}
		}
	}
	return nil
}

// AppendRecord serializes record into physical binary wire format and appends it to dst,
// returning the extended slice.
//
// Binary Framing Layout:
//
//	Offset  0..3             : CRC32-IEEE (4 bytes, uint32, Big-Endian)
//	Offset  4                : RecordType (1 byte, uint8)
//	Offset  5..12            : SeqNum     (8 bytes, uint64, Big-Endian)
//	Offset 13..20            : Timestamp  (8 bytes, uint64, Big-Endian)
//	Offset 21..22            : KeyLength  (2 bytes, uint16, Big-Endian)
//	Offset 23..23+KeyLen-1   : KeyBytes   (KeyLen bytes)
//	Offset 23+KeyLen..+3     : ValLength  (4 bytes, uint32, Big-Endian)
//	Offset 27+KeyLen..       : ValBytes   (ValLen bytes)
//
// Total record size = 27 + len(record.Key) + len(record.Value).
//
// Invariants:
//   - Validates the record before serializing.
//   - CRC32-IEEE is computed across all bytes following the CRC field (offsets 4 to end).
//   - The caller's record.Key and record.Value slices are never mutated.
//   - Zero heap allocations if cap(dst) - len(dst) >= record size.
func AppendRecord(dst []byte, record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}

	keyLen := len(record.Key)
	valLen := len(record.Value)
	recSize := MinRecordSize + keyLen + valLen

	startOffset := len(dst)
	targetLen := startOffset + recSize

	if cap(dst) >= targetLen {
		dst = dst[:targetLen]
	} else {
		newDst := make([]byte, targetLen)
		copy(newDst, dst)
		dst = newDst
	}

	buf := dst[startOffset:]

	// Serialize header fields at offsets 4..21 (offsets 0..3 reserved for CRC)
	buf[4] = byte(record.Type)
	binary.PutUint64(buf[5:13], uint64(record.SeqNum))
	binary.PutUint64(buf[13:21], record.Timestamp)

	// Serialize KeyLength and KeyBytes
	binary.PutUint16(buf[21:23], uint16(keyLen))
	if keyLen > 0 {
		copy(buf[23:23+keyLen], record.Key)
	}

	// Serialize ValueLength and ValueBytes
	valOffset := 23 + keyLen
	binary.PutUint32(buf[valOffset:valOffset+4], uint32(valLen))
	if valLen > 0 {
		copy(buf[valOffset+4:valOffset+4+valLen], record.Value)
	}

	// Compute CRC32-IEEE over all bytes following the CRC field (buf[4:])
	crc := binary.Checksum(buf[4:recSize])
	binary.PutUint32(buf[0:4], crc)

	return dst, nil
}

// EncodeRecord serializes record into a newly allocated byte slice adhering to the
// physical binary wire format.
//
// Invariants:
//   - Validates record before serializing.
//   - Non-mutating: does not modify caller's record.Key or record.Value.
//   - Exactly one heap allocation for the returned byte slice.
func EncodeRecord(record Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	recSize := MinRecordSize + len(record.Key) + len(record.Value)
	return AppendRecord(make([]byte, 0, recSize), record)
}

// DecodeRecord deserializes a single Record from r adhering to the physical binary wire format.
//
// Stream-Safety & Anti-DoS Protections:
//   - Handles partial reads, single-byte reads, and fragmented streams correctly via io.ReadFull.
//   - Returns io.EOF if and only if EOF occurs cleanly at the 0-byte start of a record.
//   - Returns an error matching errors.ErrHeaderTruncated and io.ErrUnexpectedEOF if the stream
//     terminates mid-header (1..20 bytes).
//   - Validates RecordType immediately after reading the header.
//   - Enforces key length constraints before allocating key memory.
//   - Validates ValueLength against binary.MaxValueLen (4 MiB) BEFORE allocating value memory,
//     preventing memory exhaustion DoS attacks from malicious 32-bit length fields.
//   - Returns io.ErrUnexpectedEOF if the stream terminates prematurely during key or value reads.
//   - Reconstructs CRC32-IEEE across all bytes following the CRC field using zero-allocation streaming update.
//   - Verifies CRC32-IEEE and returns *errors.ChecksumMismatchError (matching errors.ErrChecksumMismatch)
//     if bit-rot, corruption, or mismatch is detected.
//   - Decoded Key and Value slices are newly allocated and strictly owned by the returned Record,
//     guaranteeing no aliasing with reader buffers or future decode calls.
func DecodeRecord(r io.Reader) (Record, error) {
	var headerBuf [HeaderSize]byte
	n, err := io.ReadFull(r, headerBuf[:])
	if err != nil {
		if stdErrors.Is(err, io.EOF) && n == 0 {
			return Record{}, io.EOF
		}
		if stdErrors.Is(err, io.ErrUnexpectedEOF) || (stdErrors.Is(err, io.EOF) && n > 0) {
			return Record{}, fmt.Errorf("%w: %w", errors.ErrHeaderTruncated, io.ErrUnexpectedEOF)
		}
		return Record{}, err
	}

	header, err := DecodeHeader(headerBuf[:])
	if err != nil {
		return Record{}, err
	}

	// Read 2-byte KeyLength
	var keyLenBuf [2]byte
	if err := readFullOrUnexpectedEOF(r, keyLenBuf[:]); err != nil {
		return Record{}, err
	}
	keyLen := binary.GetUint16(keyLenBuf[:])

	// Validate KeyLength against record type constraints before allocating
	if uint32(keyLen) > binary.MaxKeyLen {
		return Record{}, &errors.KeyTooLargeError{
			KeySize: uint32(keyLen),
			MaxSize: binary.MaxKeyLen,
		}
	}
	switch header.Type {
	case RecordTypePut, RecordTypeDelete:
		if keyLen == 0 {
			return Record{}, errors.ErrEmptyKey
		}
	case RecordTypeBatchStart, RecordTypeBatchCommit:
		if keyLen > 0 {
			return Record{}, &errors.InvalidRecordPayloadError{
				Type:   byte(header.Type),
				Reason: "batch marker cannot have a key payload",
			}
		}
	}

	// Read KeyBytes
	var key []byte
	if keyLen > 0 {
		key = make([]byte, keyLen)
		if err := readFullOrUnexpectedEOF(r, key); err != nil {
			return Record{}, err
		}
	}

	// Read 4-byte ValueLength
	var valLenBuf [4]byte
	if err := readFullOrUnexpectedEOF(r, valLenBuf[:]); err != nil {
		return Record{}, err
	}
	valLen := binary.GetUint32(valLenBuf[:])

	// Anti-DoS Defense: Reject oversized values BEFORE allocating
	if valLen > binary.MaxValueLen {
		return Record{}, &errors.ValueTooLargeError{
			ValueSize: valLen,
			MaxSize:   binary.MaxValueLen,
		}
	}

	// Validate ValueLength against record type constraints before allocating
	switch header.Type {
	case RecordTypeDelete:
		if valLen > 0 {
			return Record{}, &errors.InvalidRecordPayloadError{
				Type:   byte(header.Type),
				Reason: "delete tombstone cannot have a value payload",
			}
		}
	case RecordTypeBatchStart, RecordTypeBatchCommit:
		if valLen > 0 {
			return Record{}, &errors.InvalidRecordPayloadError{
				Type:   byte(header.Type),
				Reason: "batch marker cannot have a value payload",
			}
		}
	}

	// Read ValueBytes
	var val []byte
	if valLen > 0 {
		val = make([]byte, valLen)
		if err := readFullOrUnexpectedEOF(r, val); err != nil {
			return Record{}, err
		}
	}

	// Reconstruct CRC32-IEEE across all bytes following CRC field:
	// headerBuf[4:21] (17B) + keyLenBuf (2B) + key + valLenBuf (4B) + val
	crc := crc32.Update(0, crc32.IEEETable, headerBuf[4:HeaderSize])
	crc = crc32.Update(crc, crc32.IEEETable, keyLenBuf[:])
	if keyLen > 0 {
		crc = crc32.Update(crc, crc32.IEEETable, key)
	}
	crc = crc32.Update(crc, crc32.IEEETable, valLenBuf[:])
	if valLen > 0 {
		crc = crc32.Update(crc, crc32.IEEETable, val)
	}

	if crc != header.CRC {
		return Record{}, &errors.ChecksumMismatchError{
			Expected: header.CRC,
			Actual:   crc,
		}
	}

	return Record{
		CRC:       header.CRC,
		Type:      header.Type,
		SeqNum:    header.SeqNum,
		Timestamp: header.Timestamp,
		Key:       key,
		Value:     val,
	}, nil
}

// readFullOrUnexpectedEOF reads exactly len(buf) bytes from r.
// If EOF is encountered before buf is filled, it returns io.ErrUnexpectedEOF.
func readFullOrUnexpectedEOF(r io.Reader, buf []byte) error {
	_, err := io.ReadFull(r, buf)
	if err == nil {
		return nil
	}
	if stdErrors.Is(err, io.EOF) || stdErrors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

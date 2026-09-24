package transport

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/binary"
)

const (
	// Magic is the 4-byte constant identifying the Lattice TCP binary protocol (0x4C415454, ASCII "LATT").
	// Sent as Big-Endian uint32 at the very beginning of every frame to reject scanner probes immediately.
	Magic uint32 = 0x4C415454

	// HeaderSize is the fixed size in bytes of the frame header.
	// Layout: [ Magic (4B) | OpCode (1B) | Flags/Status (1B) | SeqID (8B) | PayloadLength (4B) ] = 18 bytes.
	HeaderSize = 18

	// TrailerSize is the fixed size in bytes of the frame CRC32-IEEE checksum trailer.
	TrailerSize = 4

	// MinFrameSize is the minimum legal frame size on the wire (18B header + 0B payload + 4B trailer).
	MinFrameSize = HeaderSize + TrailerSize // 22 bytes

	// MaxPayloadLength is the maximum permissible payload length in bytes (5 MiB = 5,242,880 bytes).
	// Frames advertising PayloadLength > MaxPayloadLength are rejected immediately without allocation.
	MaxPayloadLength uint32 = 5 * 1024 * 1024

	// MaxFrameSize is the absolute maximum legal wire frame size (MinFrameSize + MaxPayloadLength).
	MaxFrameSize uint32 = MinFrameSize + MaxPayloadLength // 5,242,902 bytes

	// MaxKeyLength is the maximum legal user key length in bytes (65,535 bytes = 64 KB - 1).
	MaxKeyLength = binary.MaxKeyLen

	// MaxValueLength is the maximum legal value length in bytes (4 MiB = 4,194,304 bytes).
	MaxValueLength = binary.MaxValueLen

	// MaxBatchOps is the maximum number of operations allowed in a single OP_BATCH request.
	MaxBatchOps = 1024
)

// OpCode represents the 1-byte operation identifier at byte offset 4 of every frame.
type OpCode byte

const (
	// OpUnknown represents an uninitialized or unrecognized operation code.
	OpUnknown OpCode = 0x00

	// OpPut represents a key-value write operation (0x01).
	OpPut OpCode = 0x01

	// OpGet represents a point lookup operation (0x02).
	OpGet OpCode = 0x02

	// OpDelete represents a tombstone deletion operation (0x03).
	OpDelete OpCode = 0x03

	// OpExists represents a key existence check operation (0x04).
	OpExists OpCode = 0x04

	// OpBatch represents an atomic multi-operation batch (0x05).
	OpBatch OpCode = 0x05

	// OpStats represents a telemetry and engine metrics request (0x06).
	OpStats OpCode = 0x06
)

// Valid reports whether the opcode is one of the recognized operations (OP_PUT..OP_STATS).
func (op OpCode) Valid() bool {
	return op >= OpPut && op <= OpStats
}

// String returns the human-readable string representation of the operation code.
func (op OpCode) String() string {
	switch op {
	case OpPut:
		return "OP_PUT"
	case OpGet:
		return "OP_GET"
	case OpDelete:
		return "OP_DELETE"
	case OpExists:
		return "OP_EXISTS"
	case OpBatch:
		return "OP_BATCH"
	case OpStats:
		return "OP_STATS"
	default:
		return fmt.Sprintf("OP_UNKNOWN(0x%02x)", byte(op))
	}
}

// StatusCode represents the 1-byte status code in response frames.
type StatusCode byte

const (
	// StatusOk indicates successful execution of the requested operation (0x00).
	StatusOk StatusCode = 0x00

	// StatusKeyNotFound indicates that the requested key was not found in storage (0x01).
	StatusKeyNotFound StatusCode = 0x01

	// StatusError indicates a storage engine failure or internal server error (0x02).
	StatusError StatusCode = 0x02

	// StatusInvalidRequest indicates a malformed frame, invalid opcode, or payload boundary violation (0x03).
	StatusInvalidRequest StatusCode = 0x03

	// StatusThrottled indicates that the operation was rejected due to backpressure or rate limits (0x04).
	StatusThrottled StatusCode = 0x04

	// StatusServerClosed indicates that the database engine is shutting down or closed (0x05).
	StatusServerClosed StatusCode = 0x05

	// StatusNotLeader indicates that the node is not the Raft leader and cannot process client writes (0x06).
	StatusNotLeader StatusCode = 0x06

	// StatusPermissionDenied indicates that the client principal is not authorized to execute the operation (0x07).
	StatusPermissionDenied StatusCode = 0x07
)

// Valid reports whether the status code is a recognized StatusCode.
func (sc StatusCode) Valid() bool {
	return sc <= StatusPermissionDenied
}

// String returns the human-readable string representation of the status code.
func (sc StatusCode) String() string {
	switch sc {
	case StatusOk:
		return "STATUS_OK"
	case StatusKeyNotFound:
		return "STATUS_KEY_NOT_FOUND"
	case StatusError:
		return "STATUS_ERROR"
	case StatusInvalidRequest:
		return "STATUS_INVALID_REQUEST"
	case StatusThrottled:
		return "STATUS_THROTTLED"
	case StatusServerClosed:
		return "STATUS_SERVER_CLOSED"
	case StatusNotLeader:
		return "STATUS_NOT_LEADER"
	case StatusPermissionDenied:
		return "STATUS_PERMISSION_DENIED"
	default:
		return fmt.Sprintf("STATUS_UNKNOWN(0x%02x)", byte(sc))
	}
}

// Frame header flag bitmask constants.
const (
	FlagNone    byte = 0x00
	FlagSnappy  byte = 0x01
	FlagTracing byte = 0x02
)

// BatchOpType identifies the operation type for an entry within an OP_BATCH payload.
type BatchOpType byte

const (
	// BatchOpPut represents a PUT insertion/update within a batch.
	BatchOpPut BatchOpType = 0x01

	// BatchOpDelete represents a DELETE tombstone within a batch.
	BatchOpDelete BatchOpType = 0x02
)

// Valid reports whether the batch operation type is valid.
func (t BatchOpType) Valid() bool {
	return t == BatchOpPut || t == BatchOpDelete
}

// String returns the human-readable string representation of the batch operation type.
func (t BatchOpType) String() string {
	switch t {
	case BatchOpPut:
		return "BATCH_PUT"
	case BatchOpDelete:
		return "BATCH_DELETE"
	default:
		return fmt.Sprintf("BATCH_UNKNOWN(0x%02x)", byte(t))
	}
}

// BatchOp represents a single mutation within a BatchRequest.
type BatchOp struct {
	Type  BatchOpType
	Key   []byte
	Value []byte // nil for BatchOpDelete
}

// Header represents the decoded 18-byte fixed frame header.
// Wire Layout:
//
//	Offset  0..3  : Magic (4B uint32 Big-Endian)
//	Offset  4     : OpCode (1B)
//	Offset  5     : Flags (1B in requests) / Status (1B in responses)
//	Offset  6..13 : SeqID (8B uint64 Big-Endian)
//	Offset 14..17 : PayloadLength (4B uint32 Big-Endian)
type Header struct {
	Magic         uint32
	OpCode        OpCode
	Flags         byte       // In requests: bitmask flags (compression, tracing)
	Status        StatusCode // In responses: execution status code (aliased to byte 5)
	SeqID         uint64     // Client-generated correlation ID
	PayloadLength uint32     // Payload size in bytes (0 <= len <= 5MB)
}

// Frame represents a raw, validated binary protocol frame on the wire.
type Frame struct {
	Header  Header
	Payload []byte
	CRC     uint32
}

// Request is the strongly-typed, memory-safe representation of an incoming database command.
// Decoded slices (Key, Value, Batch) are guaranteed to be independent, defensive copies.
type Request struct {
	OpCode OpCode
	Flags  byte
	SeqID  uint64
	Key    []byte    // non-nil for OpPut, OpGet, OpDelete, OpExists
	Value  []byte    // non-nil for OpPut (zero-length slice is a valid 0-byte value)
	Batch  []BatchOp // non-empty for OpBatch
}

// Response is the strongly-typed representation of an outgoing operation result.
type Response struct {
	OpCode     OpCode
	Status     StatusCode
	SeqID      uint64
	Value      []byte // returned value for OpGet when Status == StatusOk
	Exists     bool   // boolean result for OpExists when Status == StatusOk
	Message    string // human-readable diagnostic message when Status != StatusOk
	LeaderID   uint64 // resolved leader node ID if Status == StatusNotLeader and known
	LeaderAddr string // resolved leader endpoint if Status == StatusNotLeader and known
}

const redirectPrefix = "not leader: leader is node "

// FormatRedirectMessage formats a canonical human-readable redirect message for StatusNotLeader.
// The address must not contain control characters (P16-SEC-F08).
func FormatRedirectMessage(leaderID uint64, addr string) string {
	return fmt.Sprintf("not leader: leader is node %d at %s", leaderID, addr)
}

// containsControlChars reports whether s contains any ASCII control characters
// (bytes 0x00-0x1F or 0x7F). Used to prevent log-injection and framing attacks (P16-SEC-F08).
func containsControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 || b == 0x7F {
			return true
		}
	}
	return false
}

// ParseRedirectMessage parses a canonical redirect message, returning (leaderID, addr, ok).
// Expects format produced by FormatRedirectMessage: "not leader: leader is node <id> at <addr>".
// Rejects messages containing control characters (P16-SEC-F08).
func ParseRedirectMessage(msg string) (uint64, string, bool) {
	// P16-SEC-F08: Reject messages with control characters to prevent log injection.
	if containsControlChars(msg) {
		return 0, "", false
	}
	if !strings.HasPrefix(msg, redirectPrefix) {
		return 0, "", false
	}
	rest := msg[len(redirectPrefix):]
	atIdx := strings.Index(rest, " at ")
	if atIdx <= 0 {
		return 0, "", false
	}
	idStr := rest[:atIdx]
	addr := strings.TrimSpace(rest[atIdx+4:])
	if len(addr) == 0 {
		return 0, "", false
	}
	// P16-SEC-F08: Reject addresses containing control characters.
	if containsControlChars(addr) {
		return 0, "", false
	}
	id, err := strconv.ParseUint(idStr, 10, 64)
	if err != nil || id == 0 {
		return 0, "", false
	}
	return id, addr, true
}

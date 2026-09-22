package raft

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// CommandHeaderSize is the fixed size in bytes of the serialized command header:
// Op (1B) | KeyLen (2B uint16 BigEndian) | ValLen (4B uint32 BigEndian) = 7 bytes.
const CommandHeaderSize = 7

// Command represents an application-level state machine mutation encapsulated within
// a committed Raft LogEntry (P16-S01-M01).
//
// Invariants:
//   - Op is either binary.OpTypePut (0x01) or binary.OpTypeDelete (0x02).
//   - 1 <= len(Key) <= binary.MaxKeyLen (65,535 bytes).
//   - len(Value) <= binary.MaxValueLen (4 MiB).
//   - For OpTypeDelete, Value must be empty (0 bytes).
//   - Encoded command size <= MaxLogEntryDataSize (4 MiB).
type Command struct {
	Op    binary.OpType
	Key   []byte
	Value []byte
}

// Clone returns a deep copy of the Command with cloned key and value byte slices.
func (c Command) Clone() Command {
	cp := Command{
		Op: c.Op,
	}
	if len(c.Key) > 0 {
		cp.Key = make([]byte, len(c.Key))
		copy(cp.Key, c.Key)
	}
	if len(c.Value) > 0 {
		cp.Value = make([]byte, len(c.Value))
		copy(cp.Value, c.Value)
	}
	return cp
}

// Validate verifies that the command satisfies all structural and security invariants.
func (c Command) Validate() error {
	if !c.Op.Valid() {
		return fmt.Errorf("%w: unrecognized command op 0x%02x", errors.ErrInvalidOpType, byte(c.Op))
	}
	if err := binary.ValidateKey(c.Key); err != nil {
		return err
	}
	if c.Op == binary.OpTypePut {
		if err := binary.ValidateValue(c.Value); err != nil {
			return err
		}
	} else if c.Op == binary.OpTypeDelete {
		if len(c.Value) != 0 {
			return fmt.Errorf("%w: DELETE command cannot contain non-empty value (got %d bytes)",
				errors.ErrInvalidOpType, len(c.Value))
		}
	}
	encodedLen := CommandHeaderSize + len(c.Key) + len(c.Value)
	if encodedLen > MaxLogEntryDataSize {
		return fmt.Errorf("%w: encoded command size %d exceeds maximum %d",
			errors.ErrRaftEntryTooLarge, encodedLen, MaxLogEntryDataSize)
	}
	return nil
}

// EncodeCommand serializes a command into canonical binary wire representation:
// [ Op (1B) | KeyLen (2B BigEndian) | ValLen (4B BigEndian) | Key (KeyLen B) | Value (ValLen B) ]
func EncodeCommand(cmd Command) ([]byte, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	kLen := len(cmd.Key)
	vLen := len(cmd.Value)
	buf := make([]byte, CommandHeaderSize+kLen+vLen)
	buf[0] = byte(cmd.Op)
	binary.PutUint16(buf[1:3], uint16(kLen))
	binary.PutUint32(buf[3:7], uint32(vLen))
	copy(buf[7:7+kLen], cmd.Key)
	if vLen > 0 {
		copy(buf[7+kLen:], cmd.Value)
	}
	return buf, nil
}

// DecodeCommand unpacks a canonical binary command and validates all fields.
// Returns defensive deep copies of Key and Value slices.
func DecodeCommand(data []byte) (Command, error) {
	if len(data) < CommandHeaderSize {
		return Command{}, fmt.Errorf("%w: command data truncated (got %d bytes, min header %d)",
			errors.ErrHeaderTruncated, len(data), CommandHeaderSize)
	}
	if len(data) > MaxLogEntryDataSize {
		return Command{}, fmt.Errorf("%w: command data length %d exceeds maximum %d",
			errors.ErrRaftEntryTooLarge, len(data), MaxLogEntryDataSize)
	}
	op, err := binary.ParseOpType(data[0])
	if err != nil {
		return Command{}, err
	}
	kLen := int(binary.GetUint16(data[1:3]))
	if kLen < 1 || kLen > binary.MaxKeyLen {
		return Command{}, fmt.Errorf("%w: key length %d out of bounds [1, %d]",
			errors.ErrKeyTooLarge, kLen, binary.MaxKeyLen)
	}
	vLen := int(binary.GetUint32(data[3:7]))
	if vLen > binary.MaxValueLen {
		return Command{}, fmt.Errorf("%w: value length %d exceeds maximum %d",
			errors.ErrValueTooLarge, vLen, binary.MaxValueLen)
	}
	if op == binary.OpTypeDelete && vLen != 0 {
		return Command{}, fmt.Errorf("%w: DELETE command cannot have non-zero value length %d",
			errors.ErrInvalidOpType, vLen)
	}

	expectedLen := CommandHeaderSize + kLen + vLen
	if len(data) < expectedLen {
		return Command{}, fmt.Errorf("%w: command data truncated (got %d bytes, expected %d)",
			errors.ErrHeaderTruncated, len(data), expectedLen)
	}
	if len(data) > expectedLen {
		return Command{}, fmt.Errorf("%w: trailing %d bytes in command payload",
			errors.ErrInvalidOpType, len(data)-expectedLen)
	}

	key := make([]byte, kLen)
	copy(key, data[7:7+kLen])

	var val []byte
	if op == binary.OpTypePut {
		if vLen > 0 {
			val = make([]byte, vLen)
			copy(val, data[7+kLen:7+kLen+vLen])
		} else {
			val = []byte{}
		}
	}

	cmd := Command{
		Op:    op,
		Key:   key,
		Value: val,
	}
	return cmd, nil
}

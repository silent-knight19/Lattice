package raft

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// CommandHeaderSize is the fixed size in bytes of the serialized command header:
// Op (1B) | KeyLen (2B uint16 BigEndian) | ValLen (4B uint32 BigEndian) = 7 bytes.
const CommandHeaderSize = 7

// CommandOp represents an individual mutation within a multi-operation batch command.
type CommandOp struct {
	Op    binary.OpType
	Key   []byte
	Value []byte
}

// Command represents an application-level state machine mutation encapsulated within
// a committed Raft LogEntry (P16-S01-M01).
//
// Invariants:
//   - Op is either binary.OpTypePut (0x01), binary.OpTypeDelete (0x02), or binary.OpTypeBatch (0x05).
//   - For single mutations (PUT/DELETE): 1 <= len(Key) <= binary.MaxKeyLen (65,535 bytes).
//   - For OpTypeDelete, Value must be empty (0 bytes).
//   - For OpTypeBatch, 1 <= len(Batch) <= 1024, and each entry satisfies Key/Value invariants.
//   - Encoded command size <= MaxLogEntryDataSize (4 MiB).
type Command struct {
	Op    binary.OpType
	Key   []byte
	Value []byte
	Batch []CommandOp
}

// Clone returns a deep copy of the Command with cloned key, value, and batch slices.
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
	if len(c.Batch) > 0 {
		cp.Batch = make([]CommandOp, len(c.Batch))
		for i, op := range c.Batch {
			cp.Batch[i] = CommandOp{
				Op: op.Op,
			}
			if len(op.Key) > 0 {
				cp.Batch[i].Key = make([]byte, len(op.Key))
				copy(cp.Batch[i].Key, op.Key)
			}
			if len(op.Value) > 0 {
				cp.Batch[i].Value = make([]byte, len(op.Value))
				copy(cp.Batch[i].Value, op.Value)
			}
		}
	}
	return cp
}

// Validate verifies that the command satisfies all structural and security invariants.
func (c Command) Validate() error {
	if c.Op != binary.OpTypePut && c.Op != binary.OpTypeDelete && c.Op != binary.OpTypeBatch {
		return fmt.Errorf("%w: unrecognized command op 0x%02x", errors.ErrInvalidOpType, byte(c.Op))
	}

	if c.Op == binary.OpTypeBatch {
		if len(c.Batch) == 0 {
			return fmt.Errorf("%w: batch command must contain at least one operation", errors.ErrInvalidOpType)
		}
		if len(c.Batch) > 1024 {
			return fmt.Errorf("%w: batch command operation count %d exceeds maximum 1024", errors.ErrInvalidOpType, len(c.Batch))
		}
		encodedLen := 1 + 4 // Op (1B) + Count (4B)
		for i, op := range c.Batch {
			if op.Op != binary.OpTypePut && op.Op != binary.OpTypeDelete {
				return fmt.Errorf("%w: batch op %d has invalid op 0x%02x", errors.ErrInvalidOpType, i, byte(op.Op))
			}
			if err := binary.ValidateKey(op.Key); err != nil {
				return err
			}
			if op.Op == binary.OpTypePut {
				if err := binary.ValidateValue(op.Value); err != nil {
					return err
				}
			} else if len(op.Value) != 0 {
				return fmt.Errorf("%w: DELETE batch op cannot contain non-empty value", errors.ErrInvalidOpType)
			}
			encodedLen += CommandHeaderSize + len(op.Key) + len(op.Value)
			if encodedLen > MaxLogEntryDataSize {
				return fmt.Errorf("%w: encoded command size %d exceeds maximum %d",
					errors.ErrRaftEntryTooLarge, encodedLen, MaxLogEntryDataSize)
			}
		}
		return nil
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

// EncodeCommand serializes a command into canonical binary wire representation.
func EncodeCommand(cmd Command) ([]byte, error) {
	if err := cmd.Validate(); err != nil {
		return nil, err
	}

	if cmd.Op == binary.OpTypeBatch {
		totalLen := 1 + 4
		for _, op := range cmd.Batch {
			totalLen += CommandHeaderSize + len(op.Key) + len(op.Value)
		}
		buf := make([]byte, totalLen)
		buf[0] = byte(cmd.Op)
		binary.PutUint32(buf[1:5], uint32(len(cmd.Batch)))
		offset := 5
		for _, op := range cmd.Batch {
			kLen := len(op.Key)
			vLen := len(op.Value)
			buf[offset] = byte(op.Op)
			binary.PutUint16(buf[offset+1:offset+3], uint16(kLen))
			binary.PutUint32(buf[offset+3:offset+7], uint32(vLen))
			copy(buf[offset+7:offset+7+kLen], op.Key)
			if vLen > 0 {
				copy(buf[offset+7+kLen:offset+7+kLen+vLen], op.Value)
			}
			offset += CommandHeaderSize + kLen + vLen
		}
		return buf, nil
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
	if len(data) == 0 {
		return Command{}, fmt.Errorf("%w: command data is empty", errors.ErrHeaderTruncated)
	}
	if len(data) > MaxLogEntryDataSize {
		return Command{}, fmt.Errorf("%w: command data length %d exceeds maximum %d",
			errors.ErrRaftEntryTooLarge, len(data), MaxLogEntryDataSize)
	}

	if data[0] == byte(binary.OpTypeBatch) {
		if len(data) < 5 {
			return Command{}, fmt.Errorf("%w: batch command data truncated (got %d bytes, min header 5)",
				errors.ErrHeaderTruncated, len(data))
		}
		count := int(binary.GetUint32(data[1:5]))
		if count < 1 || count > 1024 {
			return Command{}, fmt.Errorf("%w: invalid batch command count %d", errors.ErrInvalidOpType, count)
		}
		offset := 5
		ops := make([]CommandOp, 0, count)
		for i := 0; i < count; i++ {
			if offset+CommandHeaderSize > len(data) {
				return Command{}, fmt.Errorf("%w: batch op %d header truncated", errors.ErrHeaderTruncated, i)
			}
			opByte := data[offset]
			if opByte != byte(binary.OpTypePut) && opByte != byte(binary.OpTypeDelete) {
				return Command{}, fmt.Errorf("%w: invalid batch op %d type 0x%02x", errors.ErrInvalidOpType, i, opByte)
			}
			kLen := int(binary.GetUint16(data[offset+1 : offset+3]))
			vLen := int(binary.GetUint32(data[offset+3 : offset+7]))
			if kLen < 1 || kLen > binary.MaxKeyLen {
				return Command{}, fmt.Errorf("%w: batch op %d key length %d out of bounds", errors.ErrKeyTooLarge, i, kLen)
			}
			if vLen > binary.MaxValueLen {
				return Command{}, fmt.Errorf("%w: batch op %d value length %d exceeds maximum", errors.ErrValueTooLarge, i, vLen)
			}
			if opByte == byte(binary.OpTypeDelete) && vLen != 0 {
				return Command{}, fmt.Errorf("%w: DELETE batch op %d cannot have non-zero value length %d", errors.ErrInvalidOpType, i, vLen)
			}
			entryLen := CommandHeaderSize + kLen + vLen
			if offset+entryLen > len(data) {
				return Command{}, fmt.Errorf("%w: batch op %d data truncated", errors.ErrHeaderTruncated, i)
			}
			key := make([]byte, kLen)
			copy(key, data[offset+7:offset+7+kLen])
			var val []byte
			if opByte == byte(binary.OpTypePut) {
				if vLen > 0 {
					val = make([]byte, vLen)
					copy(val, data[offset+7+kLen:offset+entryLen])
				} else {
					val = []byte{}
				}
			}
			ops = append(ops, CommandOp{
				Op:    binary.OpType(opByte),
				Key:   key,
				Value: val,
			})
			offset += entryLen
		}
		if offset != len(data) {
			return Command{}, fmt.Errorf("%w: trailing %d bytes in batch command payload",
				errors.ErrInvalidOpType, len(data)-offset)
		}
		return Command{
			Op:    binary.OpTypeBatch,
			Batch: ops,
		}, nil
	}

	if len(data) < CommandHeaderSize {
		return Command{}, fmt.Errorf("%w: command data truncated (got %d bytes, min header %d)",
			errors.ErrHeaderTruncated, len(data), CommandHeaderSize)
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

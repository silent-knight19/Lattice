package client

import (
	"fmt"
)

// BatchOpType identifies the operation type within a WriteBatch.
type BatchOpType byte

const (
	// OpPut represents a key-value write operation in a batch.
	OpPut BatchOpType = 0x01

	// OpDelete represents a key deletion operation in a batch.
	OpDelete BatchOpType = 0x02
)

// String returns the human-readable representation of the batch operation type.
func (t BatchOpType) String() string {
	switch t {
	case OpPut:
		return "PUT"
	case OpDelete:
		return "DELETE"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", byte(t))
	}
}

// BatchOp represents a single mutation within a WriteBatch.
type BatchOp struct {
	Type  BatchOpType
	Key   []byte
	Value []byte // nil for OpDelete
}

// WriteBatch represents an ordered, heterogeneous sequence of PUT and DELETE operations
// executed atomically as a single database state transition.
type WriteBatch struct {
	ops []BatchOp
}

// NewWriteBatch creates an empty WriteBatch.
func NewWriteBatch() *WriteBatch {
	return &WriteBatch{}
}

// Put appends a PUT operation to the batch.
// The provided key and value are defensively copied.
func (b *WriteBatch) Put(key, value []byte) *WriteBatch {
	k := make([]byte, len(key))
	copy(k, key)
	var v []byte
	if value != nil {
		v = make([]byte, len(value))
		copy(v, value)
	} else {
		v = []byte{}
	}
	b.ops = append(b.ops, BatchOp{
		Type:  OpPut,
		Key:   k,
		Value: v,
	})
	return b
}

// Delete appends a DELETE operation to the batch.
// The provided key is defensively copied.
func (b *WriteBatch) Delete(key []byte) *WriteBatch {
	k := make([]byte, len(key))
	copy(k, key)
	b.ops = append(b.ops, BatchOp{
		Type: OpDelete,
		Key:  k,
	})
	return b
}

// Len returns the number of operations currently in the batch.
func (b *WriteBatch) Len() int {
	return len(b.ops)
}

// Clear resets the batch, removing all staged operations.
func (b *WriteBatch) Clear() {
	b.ops = nil
}

// Ops returns a shallow copy of the staged batch operations.
func (b *WriteBatch) Ops() []BatchOp {
	if len(b.ops) == 0 {
		return nil
	}
	out := make([]BatchOp, len(b.ops))
	copy(out, b.ops)
	return out
}

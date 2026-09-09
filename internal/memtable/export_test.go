package memtable

import "github.com/silent-knight19/lattice/internal/binary"

// Exported type aliases, methods, and functions for black-box testing from external packages.

type SkipListNode = skipListNode

func NewSkipListNodeForTesting(key binary.InternalKey, value []byte, height int) (*skipListNode, error) {
	return newSkipListNode(key, value, height)
}

func NewSentinelNodeForTesting(height int) (*skipListNode, error) {
	return newSentinelNode(height)
}

func (n *skipListNode) HeightForTesting() int {
	return n.height()
}

func (n *skipListNode) KeyForTesting() binary.InternalKey {
	return n.getKey()
}

func (n *skipListNode) ValueForTesting() []byte {
	return n.getValue()
}

func (n *skipListNode) RawValueForTesting() []byte {
	return n.rawValue()
}

func (n *skipListNode) ForwardAtForTesting(level int) (*skipListNode, error) {
	return n.forwardAt(level)
}

func (n *skipListNode) SetForwardForTesting(level int, next *skipListNode) error {
	return n.setForward(level, next)
}

func RandomHeightForTesting() int {
	return randomHeight()
}

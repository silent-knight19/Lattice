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

func (s *SkipList) InsertWithHeightForTesting(key binary.InternalKey, value []byte, height int) error {
	return s.insertInternal(key, value, height)
}

func (s *SkipList) ValidateStructureForTesting() error {
	return s.validateStructure()
}

func (s *SkipList) SearchNodeForTesting(userKey []byte) *skipListNode {
	return s.searchNode(userKey)
}

func (s *SkipList) HeadForTesting() *skipListNode {
	return s.head
}

func (s *SkipList) NodeCountAtLevelForTesting(level int) int {
	if level < 0 || level >= MaxHeight {
		return 0
	}
	count := 0
	curr := s.head.forward[level].Load()
	for curr != nil {
		count++
		curr = curr.forward[level].Load()
	}
	return count
}

func (s *SkipList) NodesAtLevelForTesting(level int) []*skipListNode {
	if level < 0 || level >= MaxHeight {
		return nil
	}
	var nodes []*skipListNode
	curr := s.head.forward[level].Load()
	for curr != nil {
		nodes = append(nodes, curr)
		curr = curr.forward[level].Load()
	}
	return nodes
}

func (s *SkipList) WriterLockForTesting() {
	s.mu.Lock()
}

func (s *SkipList) WriterUnlockForTesting() {
	s.mu.Unlock()
}

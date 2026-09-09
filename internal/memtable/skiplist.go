package memtable

import (
	"bytes"
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// SkipList is an in-memory, sorted, probabilistic multi-level data structure
// maintaining ordered multi-version InternalKey-value entries.
//
// In this micro-phase (P03-S01-M02), SkipList is single-threaded.
// Concurrency (lock-free reader traversal) is scheduled for P03-S02-M01.
//
// Structural Invariants:
//   - Head sentinel node has height = MaxHeight (16).
//   - Active height satisfies 1 <= height <= MaxHeight.
//   - Every level L (0 <= L < height) maintains entries in strict ascending
//     order according to binary.CompareInternalKey:
//     1. UserKey ASC (unsigned lexicographical byte order)
//     2. SeqNum DESC (newer revisions sort before older revisions)
//     3. OpType DESC (DELETE/Tombstone sorts before PUT for identical seqNum)
//   - Traversal along forward pointers terminates at nil without cycles.
type SkipList struct {
	head   *skipListNode
	height int
	rnd    *HeightGenerator
	count  int
}

// NewSkipList creates a new single-threaded SkipList backed by the default
// entropy-seeded geometric height generator (p = 0.25, MaxHeight = 16).
func NewSkipList() *SkipList {
	return NewSkipListWithGenerator(NewDefaultHeightGenerator())
}

// NewSkipListWithGenerator creates a SkipList backed by the specified HeightGenerator.
// If rnd is nil, NewDefaultHeightGenerator() is used as a fallback.
func NewSkipListWithGenerator(rnd *HeightGenerator) *SkipList {
	if rnd == nil {
		rnd = NewDefaultHeightGenerator()
	}
	head, err := newSentinelNode(MaxHeight)
	if err != nil {
		// MaxHeight is constant 16, guaranteed valid [1, 16]
		panic(fmt.Sprintf("failed to allocate sentinel node: %v", err))
	}
	return &SkipList{
		head:   head,
		height: MinHeight,
		rnd:    rnd,
		count:  0,
	}
}

// Height returns the current active maximum tower height among inserted nodes (1 <= height <= MaxHeight).
func (s *SkipList) Height() int {
	return s.height
}

// Len returns the total number of distinct entries currently stored in the SkipList.
func (s *SkipList) Len() int {
	return s.count
}

// IsEmpty reports whether the SkipList contains zero user entries.
func (s *SkipList) IsEmpty() bool {
	return s.count == 0
}

// Insert inserts an InternalKey-value entry into the SkipList in canonical sorted order.
//
// Failure Atomicity:
//   - Input validation (key, seqNum, opType, value) occurs before any pointer mutations.
//   - If validation fails, an error is returned and the SkipList remains completely unmodified.
//
// Duplicate InternalKey Policy:
//   - Two entries with the same UserKey but different SeqNum are distinct versions and both exist.
//   - If an entry with the exact same (UserKey, SeqNum, OpType) already exists in the list
//     (binary.CompareInternalKey returns 0), Insert idempotently updates the existing node's
//     value with a defensive copy, preserving the strict weak ordering of the list.
//
// Memory Ownership:
//   - Defensive copies of UserKey and Value are created during node allocation.
//   - Callers mutating external slices after Insert will not corrupt internal state.
func (s *SkipList) Insert(key binary.InternalKey, value []byte) error {
	return s.insertInternal(key, value, 0)
}

// insertInternal executes predecessor search and splices the new node.
// If forcedHeight > 0, forcedHeight is used instead of generating a random height (used by test seams).
func (s *SkipList) insertInternal(key binary.InternalKey, value []byte, forcedHeight int) error {
	// 1. Boundary validation prior to any structural mutation
	if err := binary.ValidateKey(key.UserKey); err != nil {
		return err
	}
	if err := key.OpType.Validate(); err != nil {
		return err
	}
	if err := binary.ValidateValue(value); err != nil {
		return err
	}

	// 2. Determine node tower height
	nodeHeight := forcedHeight
	if nodeHeight <= 0 {
		nodeHeight = s.rnd.RandomHeight()
	}
	if nodeHeight < MinHeight || nodeHeight > MaxHeight {
		return &errors.InvalidSkipListHeightError{
			Height:    nodeHeight,
			MinHeight: MinHeight,
			MaxHeight: MaxHeight,
		}
	}

	// 3. Pre-allocate the node with defensive copies before modifying any list pointers
	newNode, err := newSkipListNode(key, value, nodeHeight)
	if err != nil {
		return err
	}

	// 4. Predecessor search: locate the insertion position at each level
	var update [MaxHeight]*skipListNode
	curr := s.head

	for i := s.height - 1; i >= 0; i-- {
		for curr.forward[i] != nil && binary.CompareInternalKey(curr.forward[i].key, key) < 0 {
			curr = curr.forward[i]
		}
		update[i] = curr
	}

	// 5. Check for exact duplicate InternalKey at level 0
	candidate := curr.forward[0]
	if candidate != nil && binary.CompareInternalKey(candidate.key, key) == 0 {
		// Exact duplicate (UserKey, SeqNum, OpType): idempotently update value
		var valCopy []byte
		if len(value) > 0 {
			valCopy = make([]byte, len(value))
			copy(valCopy, value)
		}
		candidate.value = valCopy
		return nil
	}

	// 6. If new node's height exceeds current list height, initialize update pointers for new levels
	if nodeHeight > s.height {
		for i := s.height; i < nodeHeight; i++ {
			update[i] = s.head
		}
		s.height = nodeHeight
	}

	// 7. Splice forward pointers for levels 0..nodeHeight-1
	for i := 0; i < nodeHeight; i++ {
		newNode.forward[i] = update[i].forward[i]
		update[i].forward[i] = newNode
	}

	s.count++
	return nil
}

// Search performs a point lookup by UserKey, returning the value associated with
// the newest version of the key according to descending sequence number ordering.
//
// Return Semantics:
//   - If userKey is invalid: returns (nil, error).
//   - If userKey does not exist: returns (nil, errors.ErrKeyNotFound).
//   - If the newest matching revision is an OpTypeDelete (tombstone): returns (nil, errors.ErrKeyNotFound).
//   - If the newest matching revision is an OpTypePut: returns (defensiveCopy, nil).
//
// Complexity:
//   - Expected O(log N) comparisons using the express-lane forward pointer hierarchy.
//   - Zero heap allocations on the search path.
func (s *SkipList) Search(userKey []byte) ([]byte, error) {
	if err := binary.ValidateKey(userKey); err != nil {
		return nil, err
	}

	node := s.searchNode(userKey)
	if node == nil || node.key.OpType == binary.OpTypeDelete {
		return nil, errors.ErrKeyNotFound
	}
	return node.getValue(), nil
}

// searchNode locates the first node whose UserKey matches userKey.
// Because keys with the same UserKey sort descending by SeqNum, the first matching
// node encountered is guaranteed to be the newest revision.
// Returns nil if no node with UserKey exists in the SkipList.
func (s *SkipList) searchNode(userKey []byte) *skipListNode {
	curr := s.head

	// Traverse express lanes from top active level down to 0
	for i := s.height - 1; i >= 0; i-- {
		for curr.forward[i] != nil && bytes.Compare(curr.forward[i].key.UserKey, userKey) < 0 {
			curr = curr.forward[i]
		}
	}

	// Immediate successor at level 0 is the lowest node with UserKey >= userKey
	candidate := curr.forward[0]
	if candidate != nil && bytes.Equal(candidate.key.UserKey, userKey) {
		return candidate
	}
	return nil
}

// validateStructure verifies all structural invariants across all levels of the SkipList.
// Used exclusively for testing and validation.
func (s *SkipList) validateStructure() error {
	if s.head == nil {
		return fmt.Errorf("head sentinel is nil")
	}
	if s.head.height() != MaxHeight {
		return fmt.Errorf("head sentinel height %d != MaxHeight %d", s.head.height(), MaxHeight)
	}
	if s.height < MinHeight || s.height > MaxHeight {
		return fmt.Errorf("active height %d outside [%d, %d]", s.height, MinHeight, MaxHeight)
	}

	// 1. Level 0 traversal: verify count, acyclicity, and strict ordering
	visitedL0 := make(map[*skipListNode]int)
	curr := s.head.forward[0]
	idx := 0

	for curr != nil {
		if prevIdx, seen := visitedL0[curr]; seen {
			return fmt.Errorf("cycle detected at level 0: node at index %d was previously seen at index %d", idx, prevIdx)
		}
		visitedL0[curr] = idx

		if curr.height() < MinHeight || curr.height() > MaxHeight {
			return fmt.Errorf("node at index %d has invalid height %d", idx, curr.height())
		}

		next := curr.forward[0]
		if next != nil {
			cmp := binary.CompareInternalKey(curr.key, next.key)
			if cmp >= 0 {
				return fmt.Errorf("level 0 order violation at index %d: key %v >= next key %v (cmp=%d)",
					idx, curr.key, next.key, cmp)
			}
		}
		curr = next
		idx++
	}

	if idx != s.count {
		return fmt.Errorf("level 0 node count %d != reported count %d", idx, s.count)
	}

	// 2. Higher levels traversal: verify strict ordering, acyclicity, and tower height bounds
	visitedPerLevel := make([]map[*skipListNode]bool, MaxHeight)
	visitedPerLevel[0] = make(map[*skipListNode]bool, len(visitedL0))
	for n := range visitedL0 {
		visitedPerLevel[0][n] = true
	}

	for lvl := 1; lvl < MaxHeight; lvl++ {
		visitedPerLevel[lvl] = make(map[*skipListNode]bool)
		curr = s.head.forward[lvl]

		if lvl >= s.height && curr != nil {
			return fmt.Errorf("level %d >= active height %d contains non-nil forward pointer", lvl, s.height)
		}

		for curr != nil {
			if visitedPerLevel[lvl][curr] {
				return fmt.Errorf("cycle detected at level %d for node %v", lvl, curr.key)
			}
			visitedPerLevel[lvl][curr] = true

			// P03-S01-M02-INV-02: Every node reachable at level L has a tower height >= L+1
			if curr.height() < lvl+1 {
				return fmt.Errorf("node %v reachable at level %d has insufficient height %d",
					curr.key, lvl, curr.height())
			}

			// P03-S01-M02-INV-04: Every node reachable at level L must exist in level 0
			if _, exists := visitedL0[curr]; !exists {
				return fmt.Errorf("node %v at level %d does not exist in level 0", curr.key, lvl)
			}

			next := curr.forward[lvl]
			if next != nil {
				// P03-S01-M02-INV-01: Strict ordering at level L
				cmp := binary.CompareInternalKey(curr.key, next.key)
				if cmp >= 0 {
					return fmt.Errorf("level %d order violation: key %v >= next key %v (cmp=%d)",
						lvl, curr.key, next.key, cmp)
				}
			}
			curr = next
		}
	}

	// 3. P03-S01-M02-INV-05: Node must appear on all levels 0..height-1 and never at >= height
	for node := range visitedL0 {
		h := node.height()
		for lvl := 0; lvl < MaxHeight; lvl++ {
			inLevel := visitedPerLevel[lvl][node]
			if lvl < h && !inLevel {
				return fmt.Errorf("node %v of height %d is not reachable at level %d", node.key, h, lvl)
			}
			if lvl >= h && inLevel {
				return fmt.Errorf("node %v of height %d unexpectedly present at level %d", node.key, h, lvl)
			}
		}
	}

	return nil
}

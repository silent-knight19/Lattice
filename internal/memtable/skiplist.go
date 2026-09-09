package memtable

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// SkipList is an in-memory, sorted, probabilistic multi-level data structure
// maintaining ordered multi-version InternalKey-value entries.
//
// Concurrency Model (P03-S02-M01):
//   - Serialized Writer: Structural insertions acquire an exclusive mutex lock (mu.Lock()).
//   - Lock-Free Readers: Point lookups (SearchConcurrent) acquire ZERO locks (neither Lock nor RLock)
//     and traverse forward pointer express lanes completely lock-free using atomic pointer reads.
//   - Safe Publication: New nodes are fully allocated and initialized (key, value, forward targets)
//     in private writer memory before being spliced into predecessor pointers bottom-up
//     (Level 0 to Level H-1) via atomic stores (atomic.Pointer.Store).
//   - Active Height Concurrency: Active list height is maintained in an atomic.Int32, ensuring
//     readers dynamically observe safe, monotonically non-decreasing levels without data races.
//   - Node Immutability: Once published, a node's key, height, and forward pointers are immutable.
//     Exact duplicate InternalKey updates atomically replace the value container pointer without
//     in-place slice mutation.
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
	mu       sync.RWMutex
	head     *skipListNode
	height   atomic.Int32
	rnd      *HeightGenerator
	count    atomic.Int64
	byteSize atomic.Uint64
}

// NewSkipList creates a new SkipList backed by the default
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
	sl := &SkipList{
		head: head,
		rnd:  rnd,
	}
	sl.height.Store(MinHeight)
	sl.count.Store(0)
	sl.byteSize.Store(0)
	return sl
}

// Height returns the current active maximum tower height among inserted nodes (1 <= height <= MaxHeight).
func (s *SkipList) Height() int {
	return int(s.height.Load())
}

// Len returns the total number of distinct entries currently stored in the SkipList.
func (s *SkipList) Len() int {
	return int(s.count.Load())
}

// ByteSize returns the exact heap bytes directly owned by user-record structures
// (nodes, key backing arrays, forward pointer towers, value containers, and value backing arrays)
// currently stored in the SkipList.
//
// Memory Accounting Model (P03-S02-M02):
//   - Model A (User-Record Owned Storage): ByteSize starts at 0 for an empty SkipList.
//   - Each inserted entry accounts for:
//     NodeStructSize (72B on 64-bit) + len(UserKey) + height * PointerSize (8B on 64-bit)
//   - (if valueLen > 0: NodeValueStructSize (24B) + len(value))
//   - Exact duplicate InternalKey updates adjust ByteSize strictly by the value container/payload delta.
//   - Sentinel node infrastructure is excluded from user-record ByteSize.
//   - Excludes Go runtime allocator metadata, GC structures, and fragmentation.
//
// Guarantees:
//   - Complexity: O(1) time, 0 allocations.
//   - Concurrency: Thread-safe for concurrent readers and serialized writers via atomic.Uint64 load.
//   - Invariant: Updated atomically on insertion and duplicate replacement.
func (s *SkipList) ByteSize() uint64 {
	return s.byteSize.Load()
}

// IsEmpty reports whether the SkipList contains zero user entries.
func (s *SkipList) IsEmpty() bool {
	return s.count.Load() == 0
}

// Insert inserts an InternalKey-value entry into the SkipList in canonical sorted order.
//
// Concurrency & Synchronization:
//   - Writers acquire an exclusive lock for structural mutation.
//   - Pre-allocation validation occurs prior to acquiring the lock to guarantee failure atomicity.
//   - Forward pointers are published bottom-up using atomic pointer stores.
//
// Failure Atomicity:
//   - Input validation (key, seqNum, opType, value) occurs before any pointer mutations.
//   - If validation fails, an error is returned and the SkipList remains completely unmodified.
//
// Duplicate InternalKey Policy:
//   - Two entries with the same UserKey but different SeqNum are distinct versions and both exist.
//   - If an entry with the exact same (UserKey, SeqNum, OpType) already exists in the list
//     (binary.CompareInternalKey returns 0), Insert idempotently updates the existing node's
//     value by atomically swapping its value container pointer with a defensive copy, preserving
//     both the strict weak ordering of the list and complete race freedom under concurrent readers.
//
// Memory Ownership:
//   - Defensive copies of UserKey and Value are created during node allocation.
//   - Callers mutating external slices after Insert will not corrupt internal state.
func (s *SkipList) Insert(key binary.InternalKey, value []byte) error {
	return s.insertInternal(key, value, 0)
}

// insertInternal executes predecessor search and splices the new node.
// If forcedHeight != 0, forcedHeight is used instead of generating a random height (used by test seams).
func (s *SkipList) insertInternal(key binary.InternalKey, value []byte, forcedHeight int) error {
	// 1. Boundary validation prior to acquiring locks or mutating structure
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
	if nodeHeight == 0 {
		nodeHeight = s.rnd.RandomHeight()
	}
	if nodeHeight < MinHeight || nodeHeight > MaxHeight {
		return &errors.InvalidSkipListHeightError{
			Height:    nodeHeight,
			MinHeight: MinHeight,
			MaxHeight: MaxHeight,
		}
	}

	// 3. Pre-allocate and fully initialize the node in private writer memory before taking the lock
	newNode, err := newSkipListNode(key, value, nodeHeight)
	if err != nil {
		return err
	}
	entryBytes := nodeMemoryBytes(len(key.UserKey), len(value), nodeHeight)

	// 4. Acquire exclusive writer mutation lock
	s.mu.Lock()
	defer s.mu.Unlock()

	// 5. Predecessor search: locate the insertion position at each level
	var update [MaxHeight]*skipListNode
	curr := s.head
	currentHeight := int(s.height.Load())

	for i := currentHeight - 1; i >= 0; i-- {
		next := curr.forward[i].Load()
		for next != nil && binary.CompareInternalKey(next.key, key) < 0 {
			curr = next
			next = curr.forward[i].Load()
		}
		update[i] = curr
	}

	// 6. Check for exact duplicate InternalKey at level 0
	candidate := curr.forward[0].Load()
	if candidate != nil && binary.CompareInternalKey(candidate.key, key) == 0 {
		// Exact duplicate (UserKey, SeqNum, OpType): atomically replace value container
		oldVal := candidate.value.Load()
		oldValBytes := uint64(0)
		if oldVal != nil {
			oldValBytes = valueMemoryBytes(len(oldVal.data))
		}
		newValBytes := valueMemoryBytes(len(value))

		var newVal *nodeValue
		if len(value) > 0 {
			valCopy := make([]byte, len(value))
			copy(valCopy, value)
			newVal = &nodeValue{data: valCopy}
		}
		candidate.value.Store(newVal)

		// Adjust ByteSize by the delta between new and old value container/payload allocations
		if newValBytes > oldValBytes {
			safeAddUint64(&s.byteSize, newValBytes-oldValBytes)
		} else if newValBytes < oldValBytes {
			safeSubUint64(&s.byteSize, oldValBytes-newValBytes)
		}
		return nil
	}

	// 7. If new node's height exceeds current list height, initialize update pointers for new levels
	if nodeHeight > currentHeight {
		for i := currentHeight; i < nodeHeight; i++ {
			update[i] = s.head
		}
	}

	// 8. Connect new node's forward pointers to its successors
	for i := 0; i < nodeHeight; i++ {
		newNode.forward[i].Store(update[i].forward[i].Load())
	}

	// 9. Bottom-up atomic publication: link predecessors to newNode from Level 0 to Level H-1
	// Publishing Level 0 first guarantees the node is logically present in the base list before
	// upper-level express lanes make it visible to descending readers.
	for i := 0; i < nodeHeight; i++ {
		update[i].forward[i].Store(newNode)
	}

	// 10. Atomically publish increased active height if applicable
	if nodeHeight > currentHeight {
		s.height.Store(int32(nodeHeight))
	}

	s.count.Add(1)
	safeAddUint64(&s.byteSize, entryBytes)
	return nil
}

// Search performs a point lookup by UserKey, acquiring a read lock to provide
// synchronized reader semantics.
//
// Return Semantics:
//   - If userKey is invalid: returns (nil, error).
//   - If userKey does not exist: returns (nil, errors.ErrKeyNotFound).
//   - If the newest matching revision is an OpTypeDelete (tombstone): returns (nil, errors.ErrKeyNotFound).
//   - If the newest matching revision is an OpTypePut: returns (defensiveCopy, nil).
func (s *SkipList) Search(userKey []byte) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.SearchConcurrent(userKey)
}

// SearchConcurrent performs a lock-free point lookup by UserKey.
//
// Lock-Free Property (P03-S02-M01-INV-04):
//   - Acquires ZERO locks (neither Lock nor RLock on s.mu).
//   - Traverses forward pointer express lanes using atomic pointer loads (atomic.Pointer.Load).
//   - Guaranteed to never block or be blocked by serialized writers.
//   - Thread-safe for arbitrary numbers of simultaneous readers running concurrently with a writer.
//
// Return Semantics:
//   - If userKey is invalid: returns (nil, error).
//   - If userKey does not exist: returns (nil, errors.ErrKeyNotFound).
//   - If the newest matching revision is an OpTypeDelete (tombstone): returns (nil, errors.ErrKeyNotFound).
//   - If the newest matching revision is an OpTypePut: returns (defensiveCopy, nil).
//
// Complexity:
//   - Expected O(log N) comparisons using the express-lane forward pointer hierarchy.
//   - Traversal state allocates zero heap memory; returning a non-nil value creates one defensive copy.
func (s *SkipList) SearchConcurrent(userKey []byte) ([]byte, error) {
	if err := binary.ValidateKey(userKey); err != nil {
		return nil, err
	}

	node := s.searchNodeConcurrent(userKey)
	if node == nil || node.key.OpType == binary.OpTypeDelete {
		return nil, errors.ErrKeyNotFound
	}
	return node.getValue(), nil
}

// searchNodeConcurrent locates the first node whose UserKey matches userKey using lock-free atomic traversal.
// Because keys with the same UserKey sort descending by SeqNum, the first matching
// node encountered is guaranteed to be the newest revision.
// Returns nil if no node with UserKey exists in the SkipList.
func (s *SkipList) searchNodeConcurrent(userKey []byte) *skipListNode {
	curr := s.head

	// Atomically load current active height and clamp to valid range
	h := int(s.height.Load())
	if h > MaxHeight {
		h = MaxHeight
	} else if h < MinHeight {
		h = MinHeight
	}

	// Traverse express lanes from top active level down to 0
	for i := h - 1; i >= 0; i-- {
		next := curr.forward[i].Load()
		for next != nil && bytes.Compare(next.key.UserKey, userKey) < 0 {
			curr = next
			next = curr.forward[i].Load()
		}
	}

	// Immediate successor at level 0 is the lowest node with UserKey >= userKey
	candidate := curr.forward[0].Load()
	if candidate != nil && bytes.Equal(candidate.key.UserKey, userKey) {
		return candidate
	}
	return nil
}

// searchNode is an internal helper retained for compatibility with test seams.
func (s *SkipList) searchNode(userKey []byte) *skipListNode {
	return s.searchNodeConcurrent(userKey)
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
	activeHeight := int(s.height.Load())
	if activeHeight < MinHeight || activeHeight > MaxHeight {
		return fmt.Errorf("active height %d outside [%d, %d]", activeHeight, MinHeight, MaxHeight)
	}

	// 1. Level 0 traversal: verify count, acyclicity, and strict ordering
	visitedL0 := make(map[*skipListNode]int)
	curr := s.head.forward[0].Load()
	idx := 0

	for curr != nil {
		if prevIdx, seen := visitedL0[curr]; seen {
			return fmt.Errorf("cycle detected at level 0: node at index %d was previously seen at index %d", idx, prevIdx)
		}
		visitedL0[curr] = idx

		if curr.height() < MinHeight || curr.height() > MaxHeight {
			return fmt.Errorf("node at index %d has invalid height %d", idx, curr.height())
		}

		next := curr.forward[0].Load()
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

	expectedCount := int(s.count.Load())
	if idx != expectedCount {
		return fmt.Errorf("level 0 node count %d != reported count %d", idx, expectedCount)
	}

	// 2. Higher levels traversal: verify strict ordering, acyclicity, and tower height bounds
	visitedPerLevel := make([]map[*skipListNode]bool, MaxHeight)
	visitedPerLevel[0] = make(map[*skipListNode]bool, len(visitedL0))
	for n := range visitedL0 {
		visitedPerLevel[0][n] = true
	}

	for lvl := 1; lvl < MaxHeight; lvl++ {
		visitedPerLevel[lvl] = make(map[*skipListNode]bool)
		curr = s.head.forward[lvl].Load()

		if lvl >= activeHeight && curr != nil {
			return fmt.Errorf("level %d >= active height %d contains non-nil forward pointer", lvl, activeHeight)
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

			next := curr.forward[lvl].Load()
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

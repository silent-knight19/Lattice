package cache

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// lruNode represents a single cached element in the circular doubly-linked list.
type lruNode struct {
	key   BlockKey
	value []byte
	prev  *lruNode
	next  *lruNode
}

// LRUShard represents an independent, thread-safe LRU cache partition.
// It stores SSTable data blocks indexed by BlockKey, maintaining strict O(1)
// recency updates via a circular doubly-linked list with a sentinel node.
//
// Concurrency & Lifetime Safety:
//  1. All public methods acquire an internal sync.Mutex, guaranteeing thread safety across concurrent goroutines.
//  2. Values are defensively copied upon Put and Get via bytes.Clone, ensuring that callers cannot mutate
//     cached data or introduce aliasing bugs.
//  3. No disk I/O, network calls, or logging callbacks are executed while holding the shard lock.
//
// Invariants (Enforced at all times under mu):
//  1. sentinel.next points to the Most Recently Used (MRU) node, or &sentinel if empty.
//  2. sentinel.prev points to the Least Recently Used (LRU) node, or &sentinel if empty.
//  3. For every node n: n.next.prev == n and n.prev.next == n.
//  4. len(table) == number of list nodes <= capacity (when capacity > 0).
//  5. Every key in table maps to a node whose key equals that key.
//  6. Every node in the linked list exists in table.
//  7. When capacity is exceeded, exactly the LRU node (sentinel.prev) is evicted.
type LRUShard struct {
	mu       sync.Mutex
	capacity int
	table    map[BlockKey]*lruNode
	sentinel lruNode
}

// NewLRUShard initializes a new LRUShard with the given block capacity.
//
// Capacity Contract:
//   - If capacity < 0, returns *errors.InvalidCacheCapacityError.
//   - If capacity == 0, returns a valid zero-capacity shard where Put immediately discards and Get always misses.
//   - If capacity > 0, returns a shard configured to hold at most capacity block entries.
//
// initLRUShard initializes the internal map and sentinel list for an LRUShard.
func initLRUShard(shard *LRUShard, capacity int) {
	shard.capacity = capacity
	shard.table = make(map[BlockKey]*lruNode, capacity)
	shard.sentinel.next = &shard.sentinel
	shard.sentinel.prev = &shard.sentinel
}

// NewLRUShard initializes a new LRUShard with the given block capacity.
//
// Capacity Contract:
//   - If capacity < 0, returns *errors.InvalidCacheCapacityError.
//   - If capacity == 0, returns a valid zero-capacity shard where Put immediately discards and Get always misses.
//   - If capacity > 0, returns a shard configured to hold at most capacity block entries.
func NewLRUShard(capacity int) (*LRUShard, error) {
	if capacity < 0 {
		return nil, &errors.InvalidCacheCapacityError{Capacity: capacity}
	}

	shard := &LRUShard{}
	initLRUShard(shard, capacity)
	return shard, nil
}

// Get retrieves the cached value associated with key.
//
// Returns (value, true) if found, moving the accessed entry to the MRU position.
// Returns (nil, false) if absent, leaving the list order and map unchanged.
// The returned byte slice is an owned defensive copy.
func (s *LRUShard) Get(key BlockKey) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, found := s.table[key]
	if !found {
		return nil, false
	}

	s.moveToMRULocked(node)
	return bytes.Clone(node.value), true
}

// Put inserts or updates a key-value entry in the cache.
//
// Semantics:
//   - If capacity == 0, the entry is dropped immediately and not retained.
//   - If key already exists, updates its value with an owned defensive copy and moves it to MRU.
//     The entry count does not increase and no duplicate list node is created.
//   - If key does not exist, inserts a new MRU entry with an owned defensive copy.
//     If the shard is at capacity, the LRU entry (sentinel.prev) is evicted before insertion.
func (s *LRUShard) Put(key BlockKey, val []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.capacity == 0 {
		return
	}

	valCopy := bytes.Clone(val)

	if node, found := s.table[key]; found {
		node.value = valCopy
		s.moveToMRULocked(node)
		return
	}

	if len(s.table) >= s.capacity {
		s.evictLRULocked()
	}

	newNode := &lruNode{
		key:   key,
		value: valCopy,
	}
	s.insertMRULocked(newNode)
	s.table[key] = newNode
}

// Peek retrieves the cached value for key without modifying its recency.
// Returns (defensiveCopy, true) if found, or (nil, false) if absent.
func (s *LRUShard) Peek(key BlockKey) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, found := s.table[key]
	if !found {
		return nil, false
	}
	return bytes.Clone(node.value), true
}

// Contains reports whether key exists in the cache without modifying recency.
func (s *LRUShard) Contains(key BlockKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, found := s.table[key]
	return found
}

// Remove deletes key and its corresponding node from the cache if present.
// Returns true if an entry was removed, or false if it did not exist.
func (s *LRUShard) Remove(key BlockKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, found := s.table[key]
	if !found {
		return false
	}

	delete(s.table, key)
	s.removeNodeLocked(node)
	return true
}

// Len returns the current number of cached entries in the shard.
func (s *LRUShard) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.table)
}

// Capacity returns the maximum block capacity configured for this shard.
func (s *LRUShard) Capacity() int {
	return s.capacity
}

// Clear purges all entries from the shard, resetting map and linked-list state to empty.
func (s *LRUShard) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	curr := s.sentinel.next
	for curr != &s.sentinel {
		next := curr.next
		curr.prev = nil
		curr.next = nil
		curr = next
	}

	s.sentinel.next = &s.sentinel
	s.sentinel.prev = &s.sentinel
	s.table = make(map[BlockKey]*lruNode, s.capacity)
}

// insertMRULocked splices a new node into the list immediately after the sentinel (MRU position).
func (s *LRUShard) insertMRULocked(n *lruNode) {
	n.next = s.sentinel.next
	n.prev = &s.sentinel
	s.sentinel.next.prev = n
	s.sentinel.next = n
}

// removeNodeLocked unlinks a node from the doubly-linked list and clears its pointers.
func (s *LRUShard) removeNodeLocked(n *lruNode) {
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev = nil
	n.next = nil
}

// moveToMRULocked moves an existing active node to the MRU position (sentinel.next).
func (s *LRUShard) moveToMRULocked(n *lruNode) {
	if s.sentinel.next == n {
		return // already MRU
	}
	// Detach from current position
	n.prev.next = n.next
	n.next.prev = n.prev
	// Re-insert at MRU
	n.next = s.sentinel.next
	n.prev = &s.sentinel
	s.sentinel.next.prev = n
	s.sentinel.next = n
}

// evictLRULocked unlinks the LRU node (sentinel.prev), deletes it from the map,
// and returns the evicted node. If the cache is empty, returns nil.
func (s *LRUShard) evictLRULocked() *lruNode {
	if s.sentinel.prev == &s.sentinel {
		return nil
	}
	victim := s.sentinel.prev
	delete(s.table, victim.key)
	s.removeNodeLocked(victim)
	return victim
}

// checkInvariantsLocked rigorously validates all structural invariants of the shard.
func (s *LRUShard) checkInvariantsLocked() error {
	// 1. Capacity boundary
	if s.capacity > 0 && len(s.table) > s.capacity {
		return fmt.Errorf("invariant violation: table size %d exceeds capacity %d", len(s.table), s.capacity)
	}
	if s.capacity == 0 && len(s.table) != 0 {
		return fmt.Errorf("invariant violation: table size %d in zero-capacity shard", len(s.table))
	}

	// 2. Empty list boundary
	if len(s.table) == 0 {
		if s.sentinel.next != &s.sentinel || s.sentinel.prev != &s.sentinel {
			return fmt.Errorf("invariant violation: empty table but sentinel pointers not self-referencing")
		}
		return nil
	}

	// 3. Forward traversal count and cycle detection
	forwardCount := 0
	curr := s.sentinel.next
	visited := make(map[*lruNode]struct{}, len(s.table))

	for curr != &s.sentinel {
		forwardCount++
		if forwardCount > len(s.table) {
			return fmt.Errorf("invariant violation: forward traversal exceeded table length %d (cycle detected)", len(s.table))
		}
		if _, seen := visited[curr]; seen {
			return fmt.Errorf("invariant violation: duplicate node encountered in forward traversal at key %v", curr.key)
		}
		visited[curr] = struct{}{}

		// Check bidirectional pointer reciprocity
		if curr.next == nil || curr.prev == nil {
			return fmt.Errorf("invariant violation: node at key %v has nil link", curr.key)
		}
		if curr.next.prev != curr {
			return fmt.Errorf("invariant violation: broken backward link: curr.next.prev != curr at key %v", curr.key)
		}
		if curr.prev.next != curr {
			return fmt.Errorf("invariant violation: broken forward link: curr.prev.next != curr at key %v", curr.key)
		}

		// Verify presence in map
		mapNode, exists := s.table[curr.key]
		if !exists {
			return fmt.Errorf("invariant violation: list node %v missing from map", curr.key)
		}
		if mapNode != curr {
			return fmt.Errorf("invariant violation: map points to different node instance for key %v", curr.key)
		}

		curr = curr.next
	}

	if forwardCount != len(s.table) {
		return fmt.Errorf("invariant violation: forward count %d does not match map size %d", forwardCount, len(s.table))
	}

	// 4. Backward traversal count
	backwardCount := 0
	curr = s.sentinel.prev
	for curr != &s.sentinel {
		backwardCount++
		if backwardCount > len(s.table) {
			return fmt.Errorf("invariant violation: backward traversal exceeded table length %d (cycle detected)", len(s.table))
		}
		curr = curr.prev
	}

	if backwardCount != forwardCount {
		return fmt.Errorf("invariant violation: backward count %d != forward count %d", backwardCount, forwardCount)
	}

	// 5. Map-to-list completeness: every map entry must be in the visited set
	for k, node := range s.table {
		if node == nil {
			return fmt.Errorf("invariant violation: nil node in map for key %v", k)
		}
		if node.key != k {
			return fmt.Errorf("invariant violation: node key %v != map key %v", node.key, k)
		}
		if _, ok := visited[node]; !ok {
			return fmt.Errorf("invariant violation: map node %v unreachable from sentinel list", k)
		}
	}

	return nil
}

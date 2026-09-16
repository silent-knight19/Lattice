package cache

// CheckInvariants exposes the internal structural invariant checker for testing and fuzzing.
func (s *LRUShard) CheckInvariants() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkInvariantsLocked()
}

// KeysMRUtoLRU returns all keys currently in the shard ordered from MRU (head) to LRU (tail).
func (s *LRUShard) KeysMRUtoLRU() []BlockKey {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := make([]BlockKey, 0, len(s.table))
	curr := s.sentinel.next
	for curr != &s.sentinel {
		keys = append(keys, curr.key)
		curr = curr.next
	}
	return keys
}

// KeysLRUtoMRU returns all keys currently in the shard ordered from LRU (tail) to MRU (head).
func (s *LRUShard) KeysLRUtoMRU() []BlockKey {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := make([]BlockKey, 0, len(s.table))
	curr := s.sentinel.prev
	for curr != &s.sentinel {
		keys = append(keys, curr.key)
		curr = curr.prev
	}
	return keys
}

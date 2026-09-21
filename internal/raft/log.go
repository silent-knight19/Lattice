package raft

import (
	"fmt"
	"math"
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// InMemLog maintains an in-memory 1-based indexed representation of Raft log entries.
//
// Indexing & Mapping Semantics:
//   - Logical Raft indices start at 1.
//   - Index 0 is strictly reserved as the sentinel initial state (lastLogIndex=0, lastLogTerm=0).
//   - entries[0] has logical Index 1, entries[k] has logical Index k+1.
//   - All slice reads return defensive deep copies.
//   - Suffix truncation safely removes divergent entries on leader conflict.
type InMemLog struct {
	mu      sync.RWMutex
	entries []LogEntry
}

// NewInMemLog initializes an empty in-memory Raft log.
func NewInMemLog() *InMemLog {
	return &InMemLog{
		entries: make([]LogEntry, 0, 64),
	}
}

// NewInMemLogWithEntries initializes an in-memory log populated with the given entries.
// Validates that entries are strictly contiguous and 1-based.
func NewInMemLogWithEntries(entries []LogEntry) (*InMemLog, error) {
	l := NewInMemLog()
	for i, e := range entries {
		expectedIndex := LogIndex(i + 1)
		if e.Index != expectedIndex {
			return nil, fmt.Errorf("%w: entry at slice position %d has index %d, expected %d",
				errors.ErrRaftLogIndexGap, i, e.Index, expectedIndex)
		}
		if err := e.Validate(); err != nil {
			return nil, err
		}
		l.entries = append(l.entries, e.Clone())
	}
	return l, nil
}

// LastIndexAndTerm returns the logical index and term of the last entry in the log.
// If the log is empty, returns (0, 0).
func (l *InMemLog) LastIndexAndTerm() (LogIndex, Term) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	n := len(l.entries)
	if n == 0 {
		return 0, 0
	}
	last := l.entries[n-1]
	return last.Index, last.Term
}

// LastIndex returns the 1-based index of the last entry, or 0 if empty.
func (l *InMemLog) LastIndex() LogIndex {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return LogIndex(len(l.entries))
}

// Term returns the term of the entry at logical index.
// Index 0 returns Term 0 without error.
// If index is outside [0, LastIndex()], returns ErrRaftLogIndexOutOfBounds.
func (l *InMemLog) Term(index LogIndex) (Term, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if index == 0 {
		return 0, nil
	}
	if index > LogIndex(len(l.entries)) {
		return 0, fmt.Errorf("%w: index %d exceeds last log index %d", errors.ErrRaftLogIndexOutOfBounds, index, len(l.entries))
	}
	return l.entries[index-1].Term, nil
}

// Entry returns a defensive deep copy of the entry at logical index (1-based).
// If index is 0 or exceeds LastIndex(), returns ErrRaftLogIndexOutOfBounds.
func (l *InMemLog) Entry(index LogIndex) (LogEntry, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if index == 0 || index > LogIndex(len(l.entries)) {
		return LogEntry{}, fmt.Errorf("%w: requested index %d outside valid range [1, %d]",
			errors.ErrRaftLogIndexOutOfBounds, index, len(l.entries))
	}
	return l.entries[index-1].Clone(), nil
}

// Entries returns a slice of defensive copies for entries in the half-open range [from, to).
//
// Invariants:
//   - 1 <= from <= to <= LastIndex() + 1.
//   - If from == to, returns an empty slice and nil error.
//   - If from < 1 or to > LastIndex() + 1, returns ErrRaftLogIndexOutOfBounds.
func (l *InMemLog) Entries(from, to LogIndex) ([]LogEntry, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	last := LogIndex(len(l.entries))
	if from < 1 || from > to || to > last+1 {
		return nil, fmt.Errorf("%w: range [%d, %d) invalid for log size %d",
			errors.ErrRaftLogIndexOutOfBounds, from, to, last)
	}

	count := to - from
	result := make([]LogEntry, count)
	for i := LogIndex(0); i < count; i++ {
		result[i] = l.entries[from-1+i].Clone()
	}
	return result, nil
}

// Append appends entries to the end of the log.
//
// Invariants enforced:
//   - If entries is empty, returns nil without mutation.
//   - First entry must have Index == LastIndex() + 1.
//   - Subsequent entries must have strictly contiguous indices (e[i].Index == e[i-1].Index + 1).
//   - Prevents uint64 overflow on index calculation.
//   - Each entry is defensively cloned.
func (l *InMemLog) Append(entries ...LogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	last := LogIndex(len(l.entries))
	for i, e := range entries {
		expectedIndex := last + 1 + LogIndex(i)
		if expectedIndex == 0 { // overflow check
			return fmt.Errorf("%w: log index arithmetic overflow", errors.ErrSeqNumOverflow)
		}
		if e.Index != expectedIndex {
			return fmt.Errorf("%w: entry index %d does not match expected contiguous index %d",
				errors.ErrRaftLogIndexGap, e.Index, expectedIndex)
		}
		if err := e.Validate(); err != nil {
			return err
		}
	}

	for _, e := range entries {
		l.entries = append(l.entries, e.Clone())
	}
	return nil
}

// TruncateSuffix removes all entries starting at fromIndex and above.
//
// Invariants:
//   - fromIndex must be >= 1.
//   - If fromIndex > LastIndex(), no entries are removed.
//   - If fromIndex == 1, all entries are removed (log becomes empty).
//   - Preserves underlying slice allocation if practical or clears pointers to avoid memory leaks.
func (l *InMemLog) TruncateSuffix(fromIndex LogIndex) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if fromIndex < 1 {
		fromIndex = 1
	}
	last := LogIndex(len(l.entries))
	if fromIndex > last {
		return
	}

	// Clear references for garbage collection
	cutoff := int(fromIndex - 1)
	for i := cutoff; i < len(l.entries); i++ {
		l.entries[i] = LogEntry{}
	}
	l.entries = l.entries[:cutoff]
}

// Len returns the number of entries in the log.
func (l *InMemLog) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.entries)
}

// CloneEntries returns deep copies of all entries currently in the log.
func (l *InMemLog) CloneEntries() []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	res := make([]LogEntry, len(l.entries))
	for i, e := range l.entries {
		res[i] = e.Clone()
	}
	return res
}

// CheckContiguity validates that the provided slice of entries starts at expectedStart
// and has strictly increasing contiguous indices without gaps or integer overflow.
func CheckContiguity(entries []LogEntry, expectedStart LogIndex) error {
	if len(entries) == 0 {
		return nil
	}
	for i, e := range entries {
		if uint64(expectedStart) > math.MaxUint64-uint64(i) {
			return errors.ErrSeqNumOverflow
		}
		exp := expectedStart + LogIndex(i)
		if e.Index != exp {
			return fmt.Errorf("%w: entry %d has index %d, expected %d", errors.ErrRaftLogIndexGap, i, e.Index, exp)
		}
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return nil
}

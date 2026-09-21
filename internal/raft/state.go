package raft

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// MaxLogEntryDataSize defines the maximum permissible data byte length of a single Raft log entry.
// Set to 4 MiB to remain strictly bounded within the 5 MiB wire framing ceiling (transport.MaxPayloadLength)
// while accommodating large multi-operation client write batches.
const MaxLogEntryDataSize = 4 * 1024 * 1024

// LogIndex represents a 64-bit monotonically increasing 1-based Raft log index.
// Index 0 is strictly reserved as the empty / initial sentinel state.
type LogIndex uint64

// Term represents a 64-bit monotonically increasing Raft election term.
// Term 0 is the initial unvoted / cold-boot term.
type Term uint64

// LogEntry represents an individual persistent Raft log entry.
//
// Invariants:
//   - Index is strictly 1-based (Index >= 1).
//   - Term is the election term in which the entry was created by the leader (Term >= 1).
//   - Type must be a recognized transport.PeerEntryType (Normal, Configuration, Noop).
//   - len(Data) <= MaxLogEntryDataSize.
type LogEntry struct {
	Index LogIndex
	Term  Term
	Type  transport.PeerEntryType
	Data  []byte
}

// Clone returns a deep copy of the LogEntry, ensuring callers and internal log slices
// do not share mutable underlying byte array memory.
func (e LogEntry) Clone() LogEntry {
	cp := LogEntry{
		Index: e.Index,
		Term:  e.Term,
		Type:  e.Type,
	}
	if len(e.Data) > 0 {
		cp.Data = make([]byte, len(e.Data))
		copy(cp.Data, e.Data)
	}
	return cp
}

// Validate validates that the LogEntry conforms to all structural and security invariants.
func (e LogEntry) Validate() error {
	if e.Index == 0 {
		return fmt.Errorf("%w: entry index must be greater than zero", errors.ErrRaftLogIndexOutOfBounds)
	}
	if e.Term == 0 {
		return fmt.Errorf("%w: entry term must be greater than zero", errors.ErrRaftCorruptedState)
	}
	if !e.Type.Valid() {
		return fmt.Errorf("%w: unrecognized entry type 0x%02x", errors.ErrRaftInvalidEntryType, byte(e.Type))
	}
	if len(e.Data) > MaxLogEntryDataSize {
		return fmt.Errorf("%w: entry data size %d exceeds maximum %d", errors.ErrRaftEntryTooLarge, len(e.Data), MaxLogEntryDataSize)
	}
	return nil
}

// HardState encapsulates the persistent consensus metadata defined in Figure 2 of the Raft paper:
// currentTerm and votedFor.
//
// Invariants:
//   - Term must be monotonic (never regresses).
//   - VotedFor is either cluster.NodeIDNil (0, indicating no vote cast in this term) or a valid node ID (> 0).
type HardState struct {
	Term     Term
	VotedFor cluster.NodeID
}

// Clone returns an exact copy of the HardState.
func (s HardState) Clone() HardState {
	return HardState{
		Term:     s.Term,
		VotedFor: s.VotedFor,
	}
}

// Validate checks that the HardState contains structurally valid fields.
func (s HardState) Validate() error {
	// Term is unsigned (uint64), so cannot be negative.
	// VotedFor can be NodeIDNil (0) or any valid NodeID (> 0).
	return nil
}

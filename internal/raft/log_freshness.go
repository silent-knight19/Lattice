package raft

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/errors"
)

// ValidateCandidateLogCoordinates verifies that candidate log index and term respect
// foundational Raft invariants.
// The only valid zero/sentinel coordinate is (0, 0) for an empty log.
// Valid non-empty coordinates require index > 0 AND term > 0.
// Rejects both incoherent combinations:
//   - index > 0 with term == 0 (impossible: entry exists without term)
//   - index == 0 with term > 0 (impossible: term claimed without entry)
func ValidateCandidateLogCoordinates(index, term uint64) error {
	if index > 0 && term == 0 {
		return fmt.Errorf("%w: invalid candidate log coordinates (index %d > 0 with term 0)",
			errors.ErrRaftInvalidLogEntry, index)
	}
	if index == 0 && term != 0 {
		return fmt.Errorf("%w: invalid candidate log coordinates (index 0 with term %d)",
			errors.ErrRaftInvalidLogEntry, term)
	}
	return nil
}

// IsCandidateLogUpToDate evaluates whether a candidate's log is at least as up-to-date
// as the local voter's log according to Raft Section 5.4.1.
//
// Comparison Rules:
//  1. If candidate's last log term differs from voter's last log term, the one with
//     the greater term is more up-to-date.
//  2. If the terms are identical, whichever log has the greater or equal last index
//     is at least as up-to-date.
//
// Term always has strict priority over index.
func IsCandidateLogUpToDate(candTerm, candIndex, voterTerm, voterIndex uint64) bool {
	if candTerm != voterTerm {
		return candTerm > voterTerm
	}
	return candIndex >= voterIndex
}

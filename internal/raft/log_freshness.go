package raft

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/errors"
)

// ValidateCandidateLogCoordinates verifies that candidate log index and term respect
// foundational Raft invariants (e.g. index > 0 requires term > 0).
func ValidateCandidateLogCoordinates(index, term uint64) error {
	if index > 0 && term == 0 {
		return fmt.Errorf("%w: invalid candidate log coordinates (index %d > 0 with term 0)",
			errors.ErrRaftInvalidLogEntry, index)
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

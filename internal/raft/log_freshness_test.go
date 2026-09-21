package raft_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/silent-knight19/lattice/internal/raft"
)

func TestLogFreshness_Section57Matrix(t *testing.T) {
	// Section 57: Explicitly test all combinations of candidate/voter term and index
	tests := []struct {
		name       string
		candTerm   uint64
		candIndex  uint64
		voterTerm  uint64
		voterIndex uint64
		want       bool
	}{
		{
			name:       "higher_candidate_term_smaller_index",
			candTerm:   3,
			candIndex:  1,
			voterTerm:  2,
			voterIndex: 100,
			want:       true, // Term has strict priority
		},
		{
			name:       "equal_candidate_term_higher_index",
			candTerm:   2,
			candIndex:  5,
			voterTerm:  2,
			voterIndex: 4,
			want:       true,
		},
		{
			name:       "equal_candidate_term_equal_index",
			candTerm:   2,
			candIndex:  4,
			voterTerm:  2,
			voterIndex: 4,
			want:       true,
		},
		{
			name:       "equal_candidate_term_smaller_index",
			candTerm:   2,
			candIndex:  3,
			voterTerm:  2,
			voterIndex: 4,
			want:       false,
		},
		{
			name:       "lower_candidate_term_huge_index",
			candTerm:   1,
			candIndex:  math.MaxUint64,
			voterTerm:  2,
			voterIndex: 1,
			want:       false, // Term has strict priority
		},
		{
			name:       "both_empty_logs",
			candTerm:   0,
			candIndex:  0,
			voterTerm:  0,
			voterIndex: 0,
			want:       true,
		},
		{
			name:       "candidate_non_empty_voter_empty",
			candTerm:   1,
			candIndex:  1,
			voterTerm:  0,
			voterIndex: 0,
			want:       true,
		},
		{
			name:       "candidate_empty_voter_non_empty",
			candTerm:   0,
			candIndex:  0,
			voterTerm:  1,
			voterIndex: 1,
			want:       false,
		},
		{
			name:       "max_values_equal",
			candTerm:   math.MaxUint64,
			candIndex:  math.MaxUint64,
			voterTerm:  math.MaxUint64,
			voterIndex: math.MaxUint64,
			want:       true,
		},
		{
			name:       "max_term_candidate_beats_smaller_voter_term",
			candTerm:   math.MaxUint64,
			candIndex:  0,
			voterTerm:  math.MaxUint64 - 1,
			voterIndex: math.MaxUint64,
			want:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := raft.IsCandidateLogUpToDate(tc.candTerm, tc.candIndex, tc.voterTerm, tc.voterIndex)
			if got != tc.want {
				t.Fatalf("IsCandidateLogUpToDate(candTerm=%d, candIdx=%d, voterTerm=%d, voterIdx=%d) = %v, want %v",
					tc.candTerm, tc.candIndex, tc.voterTerm, tc.voterIndex, got, tc.want)
			}
		})
	}
}

func TestLogFreshness_CandidateCoordinatesValidation(t *testing.T) {
	// Section 33: index > 0 with term == 0 is an impossible log coordinate
	if err := raft.ValidateCandidateLogCoordinates(1, 0); err == nil {
		t.Fatalf("expected error for candidate index > 0 with term 0")
	}
	if err := raft.ValidateCandidateLogCoordinates(100, 0); err == nil {
		t.Fatalf("expected error for candidate index 100 with term 0")
	}

	// Legal combinations
	if err := raft.ValidateCandidateLogCoordinates(0, 0); err != nil {
		t.Fatalf("empty log coordinates (0, 0) should be valid: %v", err)
	}
	if err := raft.ValidateCandidateLogCoordinates(1, 1); err != nil {
		t.Fatalf("valid coordinates (1, 1) should be valid: %v", err)
	}
	if err := raft.ValidateCandidateLogCoordinates(10, 2); err != nil {
		t.Fatalf("valid coordinates (10, 2) should be valid: %v", err)
	}
}

// referenceLogUpToDate is the specification model of Raft Section 5.4.1.
func referenceLogUpToDate(candTerm, candIndex, voterTerm, voterIndex uint64) bool {
	if candTerm > voterTerm {
		return true
	}
	if candTerm < voterTerm {
		return false
	}
	return candIndex >= voterIndex
}

func TestLogFreshness_DifferentialPropertyTest(t *testing.T) {
	// Section 62: Differential comparison against reference specification
	const iterations = 50000

	for i := 0; i < iterations; i++ {
		cTerm := rand.Uint64()
		cIdx := rand.Uint64()
		vTerm := rand.Uint64()
		vIdx := rand.Uint64()

		got := raft.IsCandidateLogUpToDate(cTerm, cIdx, vTerm, vIdx)
		expected := referenceLogUpToDate(cTerm, cIdx, vTerm, vIdx)

		if got != expected {
			t.Fatalf("differential mismatch at iter %d: got %v, want %v (cand=(%d, %d), voter=(%d, %d))",
				i, got, expected, cTerm, cIdx, vTerm, vIdx)
		}
	}
}

package compaction

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

// FuzzCompactionPlanning tests the Planner with randomized level and key parameters,
// asserting memory safety, panic-freedom, invariant preservation, and determinism.
func FuzzCompactionPlanning(f *testing.F) {
	// Seed corpus with valid variations
	f.Add(byte('a'), byte('c'), byte('b'), byte('d'), uint64(100), uint64(200), 4)
	f.Add(byte('x'), byte('z'), byte('a'), byte('c'), uint64(1000), uint64(500), 2)
	f.Add(byte('m'), byte('m'), byte('m'), byte('m'), uint64(1), uint64(1), 1)

	f.Fuzz(func(t *testing.T, minA, maxA, minB, maxB byte, sizeA, sizeB uint64, l0Trigger int) {
		if l0Trigger <= 0 || l0Trigger > 100 {
			return
		}
		if sizeA == 0 || sizeB == 0 {
			return
		}

		// Ensure valid user keys
		if minA > maxA {
			minA, maxA = maxA, minA
		}
		if minB > maxB {
			minB, maxB = maxB, minB
		}

		ikSmallA, err := binary.NewInternalKey([]byte{minA}, 10, binary.OpTypePut)
		if err != nil {
			return
		}
		ikLargeA, err := binary.NewInternalKey([]byte{maxA}, 9, binary.OpTypePut)
		if err != nil {
			return
		}

		ikSmallB, err := binary.NewInternalKey([]byte{minB}, 8, binary.OpTypePut)
		if err != nil {
			return
		}
		ikLargeB, err := binary.NewInternalKey([]byte{maxB}, 7, binary.OpTypePut)
		if err != nil {
			return
		}

		f1 := version.FileMetadata{
			FileNum:        1,
			FileSize:       sizeA,
			SmallestKey:    binary.EncodeInternalKey(ikSmallA),
			LargestKey:     binary.EncodeInternalKey(ikLargeA),
			SmallestSeqNum: 9,
			LargestSeqNum:  10,
		}

		f2 := version.FileMetadata{
			FileNum:        2,
			FileSize:       sizeB,
			SmallestKey:    binary.EncodeInternalKey(ikSmallB),
			LargestKey:     binary.EncodeInternalKey(ikLargeB),
			SmallestSeqNum: 7,
			LargestSeqNum:  8,
		}

		policy := DefaultCompactionPolicy()
		policy.L0TriggerCount = l0Trigger

		planner, err := NewPlanner(policy)
		if err != nil {
			return
		}

		var levels [version.NumLevels][]version.FileMetadata
		levels[0] = []version.FileMetadata{f1, f2}

		v := version.NewVersion(levels)
		defer v.Unref()

		// Execute PickCompaction: must not panic
		plan1, err1 := planner.PickCompaction(v)
		plan2, err2 := planner.PickCompaction(v)

		// Assert deterministic outcome
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("nondeterministic error: err1=%v, err2=%v", err1, err2)
		}
		if (plan1 == nil) != (plan2 == nil) {
			t.Fatalf("nondeterministic plan presence: plan1=%v, plan2=%v", plan1, plan2)
		}

		if plan1 != nil {
			// Invariant: plan must validate cleanly
			if err := plan1.Validate(); err != nil {
				t.Fatalf("plan failed Validate: %v", err)
			}
			if err := plan1.ValidateAgainstVersion(v); err != nil {
				t.Fatalf("plan failed ValidateAgainstVersion: %v", err)
			}

			// Invariant: TargetLevel == SourceLevel + 1
			if plan1.TargetLevel() != plan1.SourceLevel()+1 {
				t.Fatalf("invalid target level: %d vs %d", plan1.TargetLevel(), plan1.SourceLevel())
			}

			// Invariant: TotalInputBytes matches sum
			if plan1.TotalInputBytes() != plan1.SourceInputBytes()+plan1.TargetInputBytes() {
				t.Fatalf("byte count mismatch")
			}

			// Invariant: no duplicate inputs
			seen := make(map[uint64]bool)
			for _, f := range plan1.AllInputFiles() {
				if seen[f.FileNum] {
					t.Fatalf("duplicate input file %d in plan", f.FileNum)
				}
				seen[f.FileNum] = true
			}

			// Invariant: ranges are monotonic
			if bytes.Compare(plan1.SmallestUserKey(), plan1.LargestUserKey()) > 0 {
				t.Fatalf("inverted user key range in plan: %q > %q", plan1.SmallestUserKey(), plan1.LargestUserKey())
			}
		}
	})
}

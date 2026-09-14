package compaction

import (
	"bytes"
	"cmp"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

// referencePairwiseOverlap checks if file A and file B overlap by pairwise user key comparison.
func referencePairwiseOverlap(a, b version.FileMetadata) bool {
	aMin, aMax, _ := ExtractUserKeyRange(a)
	bMin, bMax, _ := ExtractUserKeyRange(b)
	return bytes.Compare(aMax, bMin) >= 0 && bytes.Compare(bMax, aMin) >= 0
}

// referenceL0Closure computes transitive overlap closure by iterative pairwise search until fixed point.
func referenceL0Closure(allL0 []version.FileMetadata, seed []version.FileMetadata) []version.FileMetadata {
	selected := make(map[uint64]version.FileMetadata, len(allL0))
	for _, f := range seed {
		selected[f.FileNum] = f.Clone()
	}

	for {
		added := false
		for _, f := range allL0 {
			if _, inSelected := selected[f.FileNum]; inSelected {
				continue
			}
			// If f overlaps ANY file currently in selected, add it
			for _, sel := range selected {
				if referencePairwiseOverlap(f, sel) {
					selected[f.FileNum] = f.Clone()
					added = true
					break
				}
			}
		}
		if !added {
			break
		}
	}

	result := make([]version.FileMetadata, 0, len(selected))
	for _, f := range selected {
		result = append(result, f)
	}
	slices.SortFunc(result, func(a, b version.FileMetadata) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})
	return result
}

// referenceTargetOverlap finds target files overlapping [minUser, maxUser] by simple linear scan.
func referenceTargetOverlap(targets []version.FileMetadata, minUser, maxUser []byte) []version.FileMetadata {
	var result []version.FileMetadata
	for _, f := range targets {
		fMin, fMax, _ := ExtractUserKeyRange(f)
		if bytes.Compare(fMax, minUser) >= 0 && bytes.Compare(maxUser, fMin) >= 0 {
			result = append(result, f.Clone())
		}
	}
	slices.SortFunc(result, func(a, b version.FileMetadata) int {
		ikA, _ := binary.DecodeInternalKey(a.SmallestKey)
		ikB, _ := binary.DecodeInternalKey(b.SmallestKey)
		return binary.CompareInternalKey(ikA, ikB)
	})
	return result
}

func TestCompaction_DifferentialOverlapTesting(t *testing.T) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) // #nosec G404 -- randomized differential testing

	policy := DefaultCompactionPolicy()
	policy.L0TriggerCount = 1 // allow any file count to plan
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	// 2000 randomized scenarios
	for iter := 0; iter < 2000; iter++ {
		numL0 := rng.Intn(8) + 1 // 1 to 8 L0 files
		numL1 := rng.Intn(10)    // 0 to 9 L1 files

		var l0Files []version.FileMetadata
		for i := 0; i < numL0; i++ {
			c1 := byte('a' + rng.Intn(15))     // #nosec G115 -- bounded character addition
			c2 := byte(c1 + byte(rng.Intn(5))) // #nosec G115 -- bounded character addition
			fNum := uint64(i + 1)
			fSize := uint64(rng.Intn(5000) + 100) // #nosec G115 -- test size generation
			l0Files = append(l0Files, makeTestFile(t, fNum, fSize, string([]byte{c1}), 100-uint64(i), binary.OpTypePut, string([]byte{c2}), 99-uint64(i), binary.OpTypePut))
		}

		var l1Files []version.FileMetadata
		curChar := byte('a')
		for i := 0; i < numL1; i++ {
			if curChar > 'y' {
				break
			}
			span := byte(rng.Intn(2)) // #nosec G115 -- bounded random int
			endChar := curChar + span
			fNum := uint64(100 + i)
			fSize := uint64(rng.Intn(5000) + 100) // #nosec G115 -- test size generation
			l1Files = append(l1Files, makeTestFile(t, fNum, fSize, fmt.Sprintf("%c1", curChar), 50, binary.OpTypePut, fmt.Sprintf("%c2", endChar), 49, binary.OpTypePut))
			curChar = endChar + 1
		}

		// Pick random seed from L0
		seedIdx := rng.Intn(len(l0Files))
		seed := []version.FileMetadata{l0Files[seedIdx]}

		var levels [version.NumLevels][]version.FileMetadata
		levels[0] = l0Files
		levels[1] = l1Files

		v := version.NewVersion(levels)

		// 1. Reference execution
		refL0 := referenceL0Closure(l0Files, seed)
		_, _, refMinUser, refMaxUser, err := ComputeKeyRange(refL0)
		if err != nil {
			v.Unref()
			t.Fatalf("iter %d: reference ComputeKeyRange failed: %v", iter, err)
		}
		refL1 := referenceTargetOverlap(l1Files, refMinUser, refMaxUser)

		var refSrcBytes, refTgtBytes uint64
		for _, f := range refL0 {
			refSrcBytes += f.FileSize
		}
		for _, f := range refL1 {
			refTgtBytes += f.FileSize
		}

		// 2. Production Planner execution
		plan, planErr := planner.PlanCompaction(v, 0, seed)
		v.Unref()

		if planErr != nil {
			t.Fatalf("iter %d: PlanCompaction failed unexpectedly: %v", iter, planErr)
		}

		// 3. Differential assertions
		prodL0 := plan.SourceFiles()
		if len(prodL0) != len(refL0) {
			t.Fatalf("iter %d: L0 file count mismatch: prod=%d, ref=%d", iter, len(prodL0), len(refL0))
		}
		for idx := range prodL0 {
			if prodL0[idx].FileNum != refL0[idx].FileNum {
				t.Fatalf("iter %d: L0 file[%d] mismatch: prod=%d, ref=%d", iter, idx, prodL0[idx].FileNum, refL0[idx].FileNum)
			}
		}

		prodL1 := plan.TargetFiles()
		if len(prodL1) != len(refL1) {
			t.Fatalf("iter %d: L1 file count mismatch: prod=%d, ref=%d", iter, len(prodL1), len(refL1))
		}
		for idx := range prodL1 {
			if prodL1[idx].FileNum != refL1[idx].FileNum {
				t.Fatalf("iter %d: L1 file[%d] mismatch: prod=%d, ref=%d", iter, idx, prodL1[idx].FileNum, refL1[idx].FileNum)
			}
		}

		if plan.SourceInputBytes() != refSrcBytes {
			t.Fatalf("iter %d: SourceInputBytes mismatch: prod=%d, ref=%d", iter, plan.SourceInputBytes(), refSrcBytes)
		}
		if plan.TargetInputBytes() != refTgtBytes {
			t.Fatalf("iter %d: TargetInputBytes mismatch: prod=%d, ref=%d", iter, plan.TargetInputBytes(), refTgtBytes)
		}
		if plan.TotalInputBytes() != refSrcBytes+refTgtBytes {
			t.Fatalf("iter %d: TotalInputBytes mismatch", iter)
		}
	}
}

package compaction

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

type refEntry struct {
	rec        testRecord
	childIndex int
}

// referenceMerge is an independent, non-heap reference implementation that:
//  1. Gathers all records from all valid children
//  2. Sorts them canonically via binary.CompareInternalKey with child-index tie breaking
//  3. Deduplicates older revisions of identical UserKeys (or preserves all in raw mode)
func referenceMerge(children [][]testRecord, raw bool) []testRecord {
	var all []refEntry
	for childIdx, list := range children {
		for _, r := range list {
			all = append(all, refEntry{
				rec:        r,
				childIndex: childIdx,
			})
		}
	}

	sort.SliceStable(all, func(i, j int) bool {
		cmp := binary.CompareInternalKey(all[i].rec.key, all[j].rec.key)
		if cmp != 0 {
			return cmp < 0
		}
		return all[i].childIndex < all[j].childIndex
	})

	if raw {
		out := make([]testRecord, len(all))
		for i, e := range all {
			out[i] = e.rec
		}
		return out
	}

	// Deduplication pass: keep only the first record encountered for each unique UserKey
	var out []testRecord
	var lastUserKey []byte
	hasEmitted := false

	for _, e := range all {
		if hasEmitted && bytes.Equal(e.rec.key.UserKey, lastUserKey) {
			continue // suppress older revision
		}
		out = append(out, e.rec)
		lastUserKey = append(lastUserKey[:0], e.rec.key.UserKey...)
		hasEmitted = true
	}

	return out
}

func TestMergingIterator_DifferentialSuite(t *testing.T) {
	const iterations = 2500
	// #nosec G404 - Deterministic pseudo-random number generator for reproducible differential testing
	rng := rand.New(rand.NewSource(42))

	userUserPool := []string{
		"a", "b", "c", "d", "e", "f", "g", "h",
		"apple", "banana", "cherry", "date", "elderberry",
		"k1", "k2", "k3", "k4", "k5",
	}

	for iterIdx := 0; iterIdx < iterations; iterIdx++ {
		numChildren := rng.Intn(8) + 1 // 1 to 8 children
		rawMode := (iterIdx % 5) == 0  // 20% test raw mode, 80% test deduplicated mode

		childStreams := make([][]testRecord, numChildren)
		childIters := make([]Iterator, numChildren)

		for c := 0; c < numChildren; c++ {
			recCount := rng.Intn(20) // 0 to 19 records per child
			recs := make([]testRecord, recCount)
			for r := 0; r < recCount; r++ {
				uKey := userUserPool[rng.Intn(len(userUserPool))]
				// #nosec G115 - Non-negative integer fits within uint64
				seq := uint64(rng.Intn(500) + 1)
				op := binary.OpTypePut
				var val []byte
				if rng.Intn(4) == 0 {
					op = binary.OpTypeDelete
					val = nil
				} else {
					val = []byte(fmt.Sprintf("val-%s-%d-%d", uKey, seq, c))
				}
				recs[r] = testRecord{
					key:   makeKey(uKey, seq, op),
					value: val,
				}
			}

			// Each child stream in LSM is pre-sorted internally
			sort.Slice(recs, func(i, j int) bool {
				return binary.CompareInternalKey(recs[i].key, recs[j].key) < 0
			})

			childStreams[c] = recs
			childIters[c] = newMockIterator(recs)
		}

		// Compute expected output via independent reference model
		expected := referenceMerge(childStreams, rawMode)

		// Run production MergingIterator
		var mergeIt *MergingIterator
		if rawMode {
			mergeIt = NewRawMergingIterator(childIters)
		} else {
			mergeIt = NewMergingIterator(childIters)
		}

		var actual []testRecord
		for mergeIt.Next() {
			actual = append(actual, testRecord{
				key:   mergeIt.Key(),
				value: mergeIt.Value(),
			})
		}

		if err := mergeIt.Err(); err != nil {
			t.Fatalf("iteration %d: unexpected Err(): %v", iterIdx, err)
		}
		if err := mergeIt.Close(); err != nil {
			t.Fatalf("iteration %d: Close() failed: %v", iterIdx, err)
		}

		if len(actual) != len(expected) {
			t.Fatalf("iteration %d (raw=%v): count mismatch: got %d, want %d",
				iterIdx, rawMode, len(actual), len(expected))
		}

		for i := range expected {
			want := expected[i]
			got := actual[i]

			if binary.CompareInternalKey(got.key, want.key) != 0 {
				t.Fatalf("iteration %d record %d key mismatch:\ngot:  %v\nwant: %v",
					iterIdx, i, got.key, want.key)
			}
			if !bytes.Equal(got.value, want.value) {
				t.Fatalf("iteration %d record %d value mismatch:\ngot:  %s\nwant: %s",
					iterIdx, i, got.value, want.value)
			}
		}
	}
}

package compaction

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

// referenceCanDropTombstone is an independent, brute-force reference checker
// that manually scans all deeper levels without using internal/compaction helpers.
func referenceCanDropTombstone(levels [version.NumLevels][]version.FileMetadata, userKey []byte, targetLevel int) bool {
	if len(userKey) == 0 || len(userKey) > binary.MaxKeyLen {
		return false
	}
	if targetLevel < 0 || targetLevel >= version.NumLevels {
		return false
	}
	if targetLevel == version.NumLevels-1 {
		return true
	}

	for lvl := targetLevel + 1; lvl < version.NumLevels; lvl++ {
		for _, f := range levels[lvl] {
			if f.FileNum == 0 || f.FileSize == 0 || f.SmallestSeqNum > f.LargestSeqNum {
				return false
			}
			if len(f.SmallestKey) < binary.InternalKeyTrailerLen || len(f.LargestKey) < binary.InternalKeyTrailerLen {
				return false
			}
			minUser := f.SmallestKey[:len(f.SmallestKey)-binary.InternalKeyTrailerLen]
			maxUser := f.LargestKey[:len(f.LargestKey)-binary.InternalKeyTrailerLen]

			smallIK, err1 := binary.DecodeInternalKey(f.SmallestKey)
			largeIK, err2 := binary.DecodeInternalKey(f.LargestKey)
			if err1 != nil || err2 != nil || binary.CompareInternalKey(smallIK, largeIK) > 0 {
				return false
			}

			if bytes.Compare(userKey, minUser) >= 0 && bytes.Compare(userKey, maxUser) <= 0 {
				return false // overlaps deeper file
			}
		}
	}

	return true
}

func TestCompactor_DifferentialSuite(t *testing.T) {
	const iterations = 2500
	// #nosec G404 - Deterministic pseudo-random number generator for reproducible differential suite
	rng := rand.New(rand.NewSource(987654321))

	keyPool := []string{
		"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m",
		"n", "o", "p", "q", "r", "s", "t", "u", "v", "w", "x", "y", "z",
		"apple", "banana", "cherry", "date", "elderberry", "fig", "grape",
	}

	var globalFileNum uint64 = 1

	for iterIdx := 0; iterIdx < iterations; iterIdx++ {
		var levels [version.NumLevels][]version.FileMetadata

		// Randomly populate levels 0..6
		for lvl := 0; lvl < version.NumLevels; lvl++ {
			numFiles := rng.Intn(5) // 0 to 4 files per level
			if numFiles == 0 {
				continue
			}

			// Generate sorted, non-overlapping ranges for L1..L6
			// For L0, ranges can overlap
			ranges := make([][2]string, numFiles)
			for f := 0; f < numFiles; f++ {
				idx1 := rng.Intn(len(keyPool))
				idx2 := rng.Intn(len(keyPool))
				if idx1 > idx2 {
					idx1, idx2 = idx2, idx1
				}
				ranges[f] = [2]string{keyPool[idx1], keyPool[idx2]}
			}

			if lvl >= 1 {
				// Sort ranges so they are strictly non-overlapping
				sort.Slice(ranges, func(i, j int) bool {
					return ranges[i][0] < ranges[j][0]
				})
				// Adjust to eliminate overlap within L1+
				var nonOverlapping [][2]string
				var lastEnd string
				for _, r := range ranges {
					if len(nonOverlapping) == 0 {
						nonOverlapping = append(nonOverlapping, r)
						lastEnd = r[1]
					} else if r[0] > lastEnd {
						nonOverlapping = append(nonOverlapping, r)
						lastEnd = r[1]
					}
				}
				ranges = nonOverlapping
			}

			for _, r := range ranges {
				globalFileNum++
				levels[lvl] = append(levels[lvl], makeTestFileMeta(
					globalFileNum,
					r[0],
					r[1],
					// #nosec G115 - Non-negative integer fits within uint64
					uint64(rng.Intn(100)+1),
					// #nosec G115 - Non-negative integer fits within uint64
					uint64(rng.Intn(100)+101),
				))
			}
		}

		v := version.NewVersion(levels)

		c, err := NewCompactor(v)
		if err != nil {
			v.Unref()
			t.Fatalf("iteration %d: NewCompactor failed: %v", iterIdx, err)
		}

		// Test multiple target levels and keys per iteration
		for sub := 0; sub < 4; sub++ {
			targetLevel := rng.Intn(version.NumLevels+2) - 1 // -1, 0..6, 7
			testKey := []byte(keyPool[rng.Intn(len(keyPool))])
			if rng.Intn(10) == 0 {
				testKey = []byte(fmt.Sprintf("random-%d", rng.Intn(1000)))
			}

			expected := referenceCanDropTombstone(levels, testKey, targetLevel)
			actual := c.CanDropTombstone(testKey, targetLevel)
			stateless := CanDropTombstoneInVersion(v, testKey, targetLevel)

			if actual != expected {
				t.Fatalf("iteration %d sub %d mismatch for key %q, targetLevel %d: got %v, want %v",
					iterIdx, sub, testKey, targetLevel, actual, expected)
			}
			if stateless != expected {
				t.Fatalf("iteration %d sub %d stateless mismatch for key %q, targetLevel %d: got %v, want %v",
					iterIdx, sub, testKey, targetLevel, stateless, expected)
			}
		}

		if err := c.Close(); err != nil {
			v.Unref()
			t.Fatalf("iteration %d: Close failed: %v", iterIdx, err)
		}
		v.Unref()
	}
}

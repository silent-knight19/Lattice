package compaction

import (
	"bytes"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

func FuzzCanDropTombstone(f *testing.F) {
	// Seed corpus
	f.Add([]byte{0, 1, 'k', 1, 2, 'a', 'z', 10, 20})
	f.Add([]byte{6, 1, 'x'})
	f.Add([]byte{255, 0})
	f.Add([]byte{1, 4, 't', 'e', 's', 't', 2, 1, 'a', 'm', 1, 'n', 'z'})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2 {
			return
		}

		// #nosec G115 - int8 conversion permits testing negative and boundary target levels
		targetLevel := int(int8(data[0]))
		offset := 1

		keyLen := int(data[offset] % 16)
		offset++
		if offset+keyLen > len(data) {
			return
		}
		userKey := make([]byte, keyLen)
		copy(userKey, data[offset:offset+keyLen])
		offset += keyLen

		// Build a version with files from remaining data
		var levels [version.NumLevels][]version.FileMetadata
		var fileNum uint64 = 1

		for offset < len(data) {
			lvl := int(data[offset]) % version.NumLevels
			offset++
			if offset >= len(data) {
				break
			}
			fKeyLen := int(data[offset]%8) + 1
			offset++
			if offset+fKeyLen*2 > len(data) {
				break
			}
			k1 := make([]byte, fKeyLen)
			k2 := make([]byte, fKeyLen)
			copy(k1, data[offset:offset+fKeyLen])
			offset += fKeyLen
			copy(k2, data[offset:offset+fKeyLen])
			offset += fKeyLen

			if bytes.Compare(k1, k2) > 0 {
				k1, k2 = k2, k1
			}

			fileNum++
			levels[lvl] = append(levels[lvl], makeTestFileMeta(
				fileNum,
				string(k1),
				string(k2),
				10,
				20,
			))
		}

		// In L1..L6, sort files by smallest key
		for lvl := 1; lvl < version.NumLevels; lvl++ {
			sort.Slice(levels[lvl], func(i, j int) bool {
				return bytes.Compare(levels[lvl][i].SmallestKey, levels[lvl][j].SmallestKey) < 0
			})
		}

		v := version.NewVersion(levels)
		c, err := NewCompactor(v)
		if err != nil {
			v.Unref()
			return
		}

		// Property 1: No panic, deterministic output
		res1 := c.CanDropTombstone(userKey, targetLevel)
		res2 := c.CanDropTombstone(userKey, targetLevel)
		if res1 != res2 {
			t.Fatalf("determinism violation: res1=%v != res2=%v", res1, res2)
		}

		// Property 2: Invalid inputs must strictly evaluate to false
		if binary.ValidateKey(userKey) != nil || targetLevel < 0 || targetLevel >= version.NumLevels {
			if res1 {
				t.Fatalf("security violation: invalid key or level evaluated to true: key=%q, targetLevel=%d",
					userKey, targetLevel)
			}
		}

		// Property 3: Bottom-level target with valid key must strictly evaluate to true
		if binary.ValidateKey(userKey) == nil && targetLevel == version.NumLevels-1 {
			if !res1 {
				t.Fatalf("invariant violation: bottom level target must be droppable: key=%q", userKey)
			}
		}

		// Property 4: Stateless helper matches Compactor result
		statelessRes := CanDropTombstoneInVersion(v, userKey, targetLevel)
		if statelessRes != res1 {
			t.Fatalf("stateless helper mismatch: stateless=%v, compactor=%v", statelessRes, res1)
		}

		// Property 5: Close works and post-close calls return false
		if err := c.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
		if c.CanDropTombstone(userKey, targetLevel) {
			t.Fatalf("post-close CanDropTombstone must return false")
		}

		v.Unref()
	})
}

package compaction

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// FuzzCompactionOutput fuzz tests the compaction output generator against random records,
// extreme lengths, tombstone mixtures, boundary sizes, and partition targets.
func FuzzCompactionOutput(f *testing.F) {
	// Seed corpus 1: Empty input
	f.Add([]byte{}, uint32(2048), int8(1))

	// Seed corpus 2: Single PUT record
	f.Add([]byte("user_key:value_data"), uint32(2048), int8(1))

	// Seed corpus 3: Multiple records with delimiters
	f.Add([]byte("k1:v1;k2:v2;k3:v3"), uint32(1024), int8(2))

	// Seed corpus 4: Tombstone markers
	f.Add([]byte("k1:v1;k2:DELETE;k3:v3;k4:DELETE"), uint32(1024), int8(0))

	// Seed corpus 5: Large data boundary
	f.Add(bytes.Repeat([]byte("a:b;"), 50), uint32(512), int8(3))

	f.Fuzz(func(t *testing.T, data []byte, partitionTarget uint32, targetLevel int8) {
		dbDir, err := os.MkdirTemp("", "fuzz_compaction_output_*")
		if err != nil {
			t.Fatalf("failed to create temp dir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dbDir) }()

		// Constrain targetLevel to valid range [0, NumLevels-1]
		lvl := int(targetLevel)
		if lvl < 0 {
			lvl = -lvl
		}
		lvl = lvl % 7

		// Constrain partitionTarget to [256, 65536] bytes for fast fuzz iterations
		targetSize := uint64(partitionTarget)
		if targetSize < 256 {
			targetSize = 256
		} else if targetSize > 65536 {
			targetSize = 65536
		}

		// Parse fuzz data into synthetic sorted records
		pairs := bytes.Split(data, []byte(";"))
		var parsedRecords []mockRecord
		keySet := make(map[string]struct{})

		for i, pair := range pairs {
			parts := bytes.SplitN(pair, []byte(":"), 2)
			var userKey []byte
			var val []byte
			op := binary.OpTypePut

			if len(parts) == 2 {
				userKey = parts[0]
				val = parts[1]
			} else {
				userKey = pair
				val = []byte("val")
			}

			// Validate userKey bounds according to binary format
			if len(userKey) == 0 {
				userKey = []byte(fmt.Sprintf("key_%d", i))
			} else if len(userKey) > binary.MaxKeyLen {
				userKey = userKey[:binary.MaxKeyLen]
			}

			if _, exists := keySet[string(userKey)]; exists {
				continue
			}
			keySet[string(userKey)] = struct{}{}

			if bytes.Equal(val, []byte("DELETE")) {
				op = binary.OpTypeDelete
				val = nil
			} else if len(val) > 4096 {
				val = val[:4096]
			}

			parsedRecords = append(parsedRecords, mockRecord{
				key: binary.InternalKey{
					UserKey: userKey,
					SeqNum:  binary.SeqNum(uint64(i + 1)),
					OpType:  op,
				},
				val: val,
			})
		}

		// Ensure records are strictly ordered by canonical UserKey ASC
		sort.Slice(parsedRecords, func(i, j int) bool {
			return bytes.Compare(parsedRecords[i].key.UserKey, parsedRecords[j].key.UserKey) < 0
		})

		safety := TombstoneSafetyFunc(func(userKey []byte, targetLevel int) bool {
			// Deterministically drop tombstones whose user key length is even
			return len(userKey)%2 == 0
		})

		alloc := simpleSeqAllocator(1)
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = lvl
		cfg.TargetPartitionSize = targetSize
		cfg.SkipValidation = false

		iter := newMockSliceIterator(parsedRecords)
		out, err := BuildCompactionOutput(iter, safety, alloc, cfg)
		if err != nil {
			// Fail-closed error: verify no orphaned/corrupt files were reported
			if out != nil && len(out.Files) > 0 {
				t.Fatalf("FUZZ ERROR: non-nil output reported on error: %v", err)
			}
			return
		}

		// On success, verify basic invariant properties
		if out.Stats.WrittenRecords+out.Stats.OmittedTombstones != out.Stats.InputRecords {
			t.Fatalf("FUZZ ERROR: record accounting invariant violated: written(%d) + omitted(%d) != input(%d)",
				out.Stats.WrittenRecords, out.Stats.OmittedTombstones, out.Stats.InputRecords)
		}

		// Verify non-overlapping partitions and valid metadata
		for i := 0; i < len(out.Files); i++ {
			f := out.Files[i]
			if f.EntryCount == 0 {
				t.Fatalf("FUZZ ERROR: output file %d has 0 entries", f.FileNum)
			}
			if f.Meta.FileSize == 0 {
				t.Fatalf("FUZZ ERROR: output file %d has 0 file size", f.FileNum)
			}
			if i < len(out.Files)-1 {
				next := out.Files[i+1]
				if bytes.Compare(f.LargestUserKey, next.SmallestUserKey) >= 0 {
					t.Fatalf("FUZZ ERROR: partition overlap: file %d largest %s >= file %d smallest %s",
						f.FileNum, f.LargestUserKey, next.FileNum, next.SmallestUserKey)
				}
			}
		}
	})
}

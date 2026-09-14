package compaction

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// refRecord represents a record in the reference model.
type refRecord struct {
	userKey []byte
	seqNum  binary.SeqNum
	opType  binary.OpType
	val     []byte
}

// TestCompactionOutput_DifferentialSuite executes 1,000+ randomized differential tests
// comparing BuildCompactionOutput against an independent reference model.
func TestCompactionOutput_DifferentialSuite(t *testing.T) {
	//nolint:gosec // math/rand is sufficient for deterministic differential testing
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	iterations := 1000

	for iterIdx := 0; iterIdx < iterations; iterIdx++ {
		dbDir := t.TempDir()
		alloc := simpleSeqAllocator(uint64(iterIdx)*1000 + 1)

		// 1. Generate randomized unique keys
		numKeys := rng.Intn(60) // 0 to 59 keys
		keySet := make(map[string]struct{})
		for len(keySet) < numKeys {
			k := fmt.Sprintf("k_%04d_%d", rng.Intn(1000), rng.Intn(100))
			keySet[k] = struct{}{}
		}

		sortedKeys := make([]string, 0, len(keySet))
		for k := range keySet {
			sortedKeys = append(sortedKeys, k)
		}
		sort.Strings(sortedKeys)

		// 2. Assign operations, values, and tombstone droppability
		safeDropSet := make(map[string]bool)
		var inputRecords []mockRecord

		for _, kStr := range sortedKeys {
			kBytes := []byte(kStr)
			isDelete := rng.Float64() < 0.3 // 30% deletes
			op := binary.OpTypePut
			var val []byte
			if isDelete {
				op = binary.OpTypeDelete
				val = nil
				safeDropSet[kStr] = rng.Float64() < 0.5 // 50% of deletes are safe to drop
			} else {
				valLen := rng.Intn(400) // 0 to 399 bytes
				val = make([]byte, valLen)
				_, _ = rng.Read(val)
			}

			// #nosec G115 -- bounded positive random int
			seqNum := binary.SeqNum(uint64(rng.Intn(10000) + 1))
			inputRecords = append(inputRecords, mockRecord{
				key: binary.InternalKey{
					UserKey: kBytes,
					SeqNum:  seqNum,
					OpType:  op,
				},
				val: val,
			})
		}

		targetLevel := rng.Intn(6) // 0 to 5
		safetyChecker := TombstoneSafetyFunc(func(userKey []byte, lvl int) bool {
			return safeDropSet[string(userKey)]
		})

		// 3. Independent Reference Model: compute expected surviving records
		var expectedRecords []refRecord
		var expectedOmitted uint64
		var expectedRetained uint64
		var expectedWritten uint64

		for _, rec := range inputRecords {
			if rec.key.OpType == binary.OpTypeDelete && safetyChecker.CanDropTombstone(rec.key.UserKey, targetLevel) {
				expectedOmitted++
				continue
			}

			if rec.key.OpType == binary.OpTypeDelete {
				expectedRetained++
			}
			expectedWritten++
			expectedRecords = append(expectedRecords, refRecord{
				userKey: bytes.Clone(rec.key.UserKey),
				seqNum:  rec.key.SeqNum,
				opType:  rec.key.OpType,
				val:     bytes.Clone(rec.val),
			})
		}

		// 4. Run BuildCompactionOutput
		// #nosec G115 -- bounded positive random int
		targetPartitionSize := uint64(1024) + uint64(rng.Intn(4096)) // 1 KiB to 5 KiB
		cfg := DefaultCompactionOutputConfig()
		cfg.DbDir = dbDir
		cfg.TargetLevel = targetLevel
		cfg.TargetPartitionSize = targetPartitionSize
		cfg.SkipValidation = false

		iter := newMockSliceIterator(inputRecords)
		out, err := BuildCompactionOutput(iter, safetyChecker, alloc, cfg)
		if err != nil {
			t.Fatalf("[iter %d] BuildCompactionOutput failed: %v", iterIdx, err)
		}

		// 5. Differential Assertions
		if out.Stats.InputRecords != uint64(len(inputRecords)) {
			t.Fatalf("[iter %d] input records mismatch: got %d, expected %d",
				iterIdx, out.Stats.InputRecords, len(inputRecords))
		}
		if out.Stats.OmittedTombstones != expectedOmitted {
			t.Fatalf("[iter %d] omitted tombstones mismatch: got %d, expected %d",
				iterIdx, out.Stats.OmittedTombstones, expectedOmitted)
		}
		if out.Stats.RetainedTombstones != expectedRetained {
			t.Fatalf("[iter %d] retained tombstones mismatch: got %d, expected %d",
				iterIdx, out.Stats.RetainedTombstones, expectedRetained)
		}
		if out.Stats.WrittenRecords != expectedWritten {
			t.Fatalf("[iter %d] written records mismatch: got %d, expected %d",
				iterIdx, out.Stats.WrittenRecords, expectedWritten)
		}

		if len(expectedRecords) == 0 {
			if len(out.Files) != 0 {
				t.Fatalf("[iter %d] expected 0 output files for empty surviving stream, got %d",
					iterIdx, len(out.Files))
			}
			continue
		}

		if len(out.Files) == 0 {
			t.Fatalf("[iter %d] expected >= 1 output files, got 0", iterIdx)
		}

		// Collect all records across all generated SSTables
		var actualRecords []refRecord
		for pIdx, f := range out.Files {
			if f.EntryCount == 0 {
				t.Fatalf("[iter %d, part %d] output file has 0 entries", iterIdx, pIdx)
			}
			if f.Level != targetLevel {
				t.Fatalf("[iter %d, part %d] level mismatch: got %d, expected %d",
					iterIdx, pIdx, f.Level, targetLevel)
			}

			// Verify SSTable physical integrity
			if err := ValidateSSTableOutput(f.Path, f.Meta); err != nil {
				t.Fatalf("[iter %d, part %d] ValidateSSTableOutput failed: %v", iterIdx, pIdx, err)
			}

			reader, err := sstable.NewTableReader(f.Path)
			if err != nil {
				t.Fatalf("[iter %d, part %d] failed to open reader: %v", iterIdx, pIdx, err)
			}
			rit, err := reader.NewIterator()
			if err != nil {
				_ = reader.Close()
				t.Fatalf("[iter %d, part %d] failed to create reader iterator: %v", iterIdx, pIdx, err)
			}

			var partFirstUserKey []byte
			var partLastUserKey []byte
			for rit.Next() {
				k := rit.Key()
				if len(partFirstUserKey) == 0 {
					partFirstUserKey = bytes.Clone(k.UserKey)
				}
				partLastUserKey = bytes.Clone(k.UserKey)

				actualRecords = append(actualRecords, refRecord{
					userKey: bytes.Clone(k.UserKey),
					seqNum:  k.SeqNum,
					opType:  k.OpType,
					val:     bytes.Clone(rit.Value()),
				})
			}
			_ = rit.Close()
			_ = reader.Close()

			if !bytes.Equal(f.SmallestUserKey, partFirstUserKey) || !bytes.Equal(f.LargestUserKey, partLastUserKey) {
				t.Fatalf("[iter %d, part %d] user key bounds mismatch in metadata", iterIdx, pIdx)
			}
		}

		// Verify non-overlapping key ranges across consecutive partitions
		for i := 0; i < len(out.Files)-1; i++ {
			cur := out.Files[i]
			next := out.Files[i+1]
			if bytes.Compare(cur.LargestUserKey, next.SmallestUserKey) >= 0 {
				t.Fatalf("[iter %d] partition range overlap: file %d largest %s >= file %d smallest %s",
					iterIdx, cur.FileNum, cur.LargestUserKey, next.FileNum, next.SmallestUserKey)
			}
		}

		// Exact 1-to-1 comparison between actual records and reference model records
		if len(actualRecords) != len(expectedRecords) {
			t.Fatalf("[iter %d] total record count mismatch: got %d, expected %d",
				iterIdx, len(actualRecords), len(expectedRecords))
		}

		for rIdx := range expectedRecords {
			exp := expectedRecords[rIdx]
			act := actualRecords[rIdx]

			if !bytes.Equal(act.userKey, exp.userKey) {
				t.Fatalf("[iter %d, rec %d] userKey mismatch: got %s, expected %s",
					iterIdx, rIdx, act.userKey, exp.userKey)
			}
			if act.seqNum != exp.seqNum {
				t.Fatalf("[iter %d, rec %d] seqNum mismatch: got %d, expected %d",
					iterIdx, rIdx, act.seqNum, exp.seqNum)
			}
			if act.opType != exp.opType {
				t.Fatalf("[iter %d, rec %d] opType mismatch: got %v, expected %v",
					iterIdx, rIdx, act.opType, exp.opType)
			}
			if exp.opType == binary.OpTypePut {
				if !bytes.Equal(act.val, exp.val) {
					t.Fatalf("[iter %d, rec %d] value mismatch", iterIdx, rIdx)
				}
			} else {
				if act.val != nil {
					t.Fatalf("[iter %d, rec %d] tombstone value must be nil", iterIdx, rIdx)
				}
			}
		}
	}
}

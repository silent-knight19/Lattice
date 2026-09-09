package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// independentOracleEntryBytes is an independent test-side calculation of the heap bytes
// owned by a SkipList node for a given key length, value length, and tower height.
//
// Layout derivation for 64-bit platforms:
//   - skipListNode struct:
//     key binary.InternalKey (40 bytes: UserKey []byte 24B + SeqNum 8B + OpType 1B + padding 7B)
//     value atomic.Pointer[nodeValue] (8 bytes)
//     forward []atomic.Pointer[skipListNode] (24 bytes slice header)
//     Total struct = 72 bytes
//   - Key backing storage: len(UserKey) bytes
//   - Forward pointer tower array: height * 8 bytes
//   - Value container & backing storage: if len(value) > 0, nodeValue struct (24 bytes) + len(value) bytes; else 0
func independentOracleEntryBytes(keyLen, valLen, height int) uint64 {
	const (
		oracleNodeStructSize      = 72
		oraclePointerSize         = 8
		oracleNodeValueStructSize = 24
	)
	bytesTotal := uint64(oracleNodeStructSize + keyLen + height*oraclePointerSize)
	if valLen > 0 {
		bytesTotal += uint64(oracleNodeValueStructSize + valLen)
	}
	return bytesTotal
}

// TestByteSize_PlatformLayoutSanity verifies that the compile-time struct sizes on the
// running platform match the architectural constants.
func TestByteSize_PlatformLayoutSanity(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("Skipping 64-bit layout assertions on non-64-bit platform")
	}

	if memtable.NodeStructSizeForTesting != 72 {
		t.Fatalf("expected NodeStructSize=72 on 64-bit platform, got %d", memtable.NodeStructSizeForTesting)
	}
	if memtable.NodeValueStructSizeForTesting != 24 {
		t.Fatalf("expected NodeValueStructSize=24 on 64-bit platform, got %d", memtable.NodeValueStructSizeForTesting)
	}
	if memtable.PointerSizeForTesting != 8 {
		t.Fatalf("expected PointerSize=8 on 64-bit platform, got %d", memtable.PointerSizeForTesting)
	}
}

// TestByteSize_EmptyStructure verifies that an initialized SkipList has ByteSize == 0 (Model A).
func TestByteSize_EmptyStructure(t *testing.T) {
	sl := memtable.NewSkipList()
	if sl.ByteSize() != 0 {
		t.Fatalf("expected ByteSize=0 for empty SkipList, got %d", sl.ByteSize())
	}
	if sl.Len() != 0 {
		t.Fatalf("expected Len=0 for empty SkipList, got %d", sl.Len())
	}
}

// TestByteSize_ExactnessMatrix verifies Cases 1-4 from the roadmap against an independent oracle.
func TestByteSize_ExactnessMatrix(t *testing.T) {
	cases := []struct {
		name     string
		height   int
		keyLen   int
		valueLen int
	}{
		{
			name:     "Case 1: height=1, key=1, value=0",
			height:   1,
			keyLen:   1,
			valueLen: 0,
		},
		{
			name:     "Case 2: height=1, key=10, value=10",
			height:   1,
			keyLen:   10,
			valueLen: 10,
		},
		{
			name:     "Case 3: height=4, key=100, value=1000",
			height:   4,
			keyLen:   100,
			valueLen: 1000,
		},
		{
			name:     "Case 4: height=16, key=MaxKeyLen, value=65536",
			height:   16,
			keyLen:   binary.MaxKeyLen, // 65,535 bytes
			valueLen: 65536,            // 64 KB boundary test
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sl := memtable.NewSkipList()

			keyBytes := bytes.Repeat([]byte{'k'}, tc.keyLen)
			valBytes := bytes.Repeat([]byte{'v'}, tc.valueLen)

			ik, err := binary.NewInternalKey(keyBytes, 100, binary.OpTypePut)
			if err != nil {
				t.Fatalf("unexpected NewInternalKey error: %v", err)
			}

			err = sl.InsertWithHeightForTesting(ik, valBytes, tc.height)
			if err != nil {
				t.Fatalf("unexpected InsertWithHeightForTesting error: %v", err)
			}

			expected := independentOracleEntryBytes(tc.keyLen, tc.valueLen, tc.height)
			actual := sl.ByteSize()

			if actual != expected {
				t.Fatalf("ByteSize mismatch: expected %d bytes, got %d bytes (diff: %d)",
					expected, actual, int64(actual)-int64(expected))
			}
		})
	}
}

// TestByteSize_AdditiveIncrementalDeltas verifies that consecutive insertions increment ByteSize
// strictly additively (A, A+B, A+B+C).
func TestByteSize_AdditiveIncrementalDeltas(t *testing.T) {
	sl := memtable.NewSkipList()

	// Entry A: height=2, key="alpha", val="first"
	ikA, _ := binary.NewInternalKey([]byte("alpha"), 1, binary.OpTypePut)
	valA := []byte("first")
	heightA := 2
	expectedA := independentOracleEntryBytes(len(ikA.UserKey), len(valA), heightA)

	if err := sl.InsertWithHeightForTesting(ikA, valA, heightA); err != nil {
		t.Fatalf("failed to insert A: %v", err)
	}
	if sl.ByteSize() != expectedA {
		t.Fatalf("after A: expected %d, got %d", expectedA, sl.ByteSize())
	}

	// Entry B: height=3, key="beta", val="second-longer-value"
	ikB, _ := binary.NewInternalKey([]byte("beta"), 2, binary.OpTypePut)
	valB := []byte("second-longer-value")
	heightB := 3
	expectedB := independentOracleEntryBytes(len(ikB.UserKey), len(valB), heightB)

	if err := sl.InsertWithHeightForTesting(ikB, valB, heightB); err != nil {
		t.Fatalf("failed to insert B: %v", err)
	}
	if sl.ByteSize() != expectedA+expectedB {
		t.Fatalf("after B: expected %d, got %d", expectedA+expectedB, sl.ByteSize())
	}

	// Entry C: height=1, key="gamma", val="" (empty value)
	ikC, _ := binary.NewInternalKey([]byte("gamma"), 3, binary.OpTypePut)
	valC := []byte("")
	heightC := 1
	expectedC := independentOracleEntryBytes(len(ikC.UserKey), len(valC), heightC)

	if err := sl.InsertWithHeightForTesting(ikC, valC, heightC); err != nil {
		t.Fatalf("failed to insert C: %v", err)
	}
	if sl.ByteSize() != expectedA+expectedB+expectedC {
		t.Fatalf("after C: expected %d, got %d", expectedA+expectedB+expectedC, sl.ByteSize())
	}
}

// TestByteSize_DuplicateValueDeltas exercises exact duplicate value replacements:
// 1. Same length: no change
// 2. Grow (10 -> 20): delta = +10
// 3. Shrink (20 -> 5): delta = -15
// 4. Shrink to 0 (5 -> 0): delta = -(sizeof(nodeValue) + 5)
// 5. Grow from 0 (0 -> 50): delta = +(sizeof(nodeValue) + 50)
func TestByteSize_DuplicateValueDeltas(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("target-key")
	ik, _ := binary.NewInternalKey(userKey, 50, binary.OpTypePut)
	height := 3

	// Initial insert: value length = 10
	val10 := bytes.Repeat([]byte("a"), 10)
	if err := sl.InsertWithHeightForTesting(ik, val10, height); err != nil {
		t.Fatalf("initial insert failed: %v", err)
	}
	expectedInitial := independentOracleEntryBytes(len(userKey), 10, height)
	if sl.ByteSize() != expectedInitial {
		t.Fatalf("expected initial %d, got %d", expectedInitial, sl.ByteSize())
	}
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", sl.Len())
	}

	// 1. Duplicate with same length: 10 -> 10
	val10B := bytes.Repeat([]byte("b"), 10)
	if err := sl.InsertWithHeightForTesting(ik, val10B, height); err != nil {
		t.Fatalf("duplicate insert 10->10 failed: %v", err)
	}
	if sl.ByteSize() != expectedInitial {
		t.Fatalf("10->10: expected %d, got %d", expectedInitial, sl.ByteSize())
	}
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1 after duplicate, got %d", sl.Len())
	}

	// 2. Grow: 10 -> 20
	val20 := bytes.Repeat([]byte("c"), 20)
	if err := sl.InsertWithHeightForTesting(ik, val20, height); err != nil {
		t.Fatalf("duplicate insert 10->20 failed: %v", err)
	}
	expected20 := expectedInitial + 10
	if sl.ByteSize() != expected20 {
		t.Fatalf("10->20: expected %d, got %d", expected20, sl.ByteSize())
	}
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1 after duplicate, got %d", sl.Len())
	}

	// 3. Shrink: 20 -> 5
	val5 := bytes.Repeat([]byte("d"), 5)
	if err := sl.InsertWithHeightForTesting(ik, val5, height); err != nil {
		t.Fatalf("duplicate insert 20->5 failed: %v", err)
	}
	expected5 := expected20 - 15
	if sl.ByteSize() != expected5 {
		t.Fatalf("20->5: expected %d, got %d", expected5, sl.ByteSize())
	}
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1 after duplicate, got %d", sl.Len())
	}

	// 4. Shrink to 0: 5 -> 0
	val0 := []byte{}
	if err := sl.InsertWithHeightForTesting(ik, val0, height); err != nil {
		t.Fatalf("duplicate insert 5->0 failed: %v", err)
	}
	// Shrinking to 0 removes nodeValue container (24B) and 5 payload bytes = 29B
	expected0 := expected5 - (24 + 5)
	if sl.ByteSize() != expected0 {
		t.Fatalf("5->0: expected %d, got %d", expected0, sl.ByteSize())
	}
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1 after duplicate, got %d", sl.Len())
	}

	// 5. Grow from 0: 0 -> 50
	val50 := bytes.Repeat([]byte("e"), 50)
	if err := sl.InsertWithHeightForTesting(ik, val50, height); err != nil {
		t.Fatalf("duplicate insert 0->50 failed: %v", err)
	}
	// Growing from 0 adds nodeValue container (24B) and 50 payload bytes = 74B
	expected50 := expected0 + (24 + 50)
	if sl.ByteSize() != expected50 {
		t.Fatalf("0->50: expected %d, got %d", expected50, sl.ByteSize())
	}
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1 after duplicate, got %d", sl.Len())
	}
}

// TestByteSize_MultiVersionAccounting verifies that multiple revisions of the same UserKey
// create distinct physical nodes and each node is accounted for in ByteSize.
func TestByteSize_MultiVersionAccounting(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("versioned-key")

	ik100, _ := binary.NewInternalKey(userKey, 100, binary.OpTypePut)
	ik90, _ := binary.NewInternalKey(userKey, 90, binary.OpTypePut)
	ik80, _ := binary.NewInternalKey(userKey, 80, binary.OpTypePut)

	val := []byte("val")
	height := 2

	_ = sl.InsertWithHeightForTesting(ik100, val, height)
	_ = sl.InsertWithHeightForTesting(ik90, val, height)
	_ = sl.InsertWithHeightForTesting(ik80, val, height)

	if sl.Len() != 3 {
		t.Fatalf("expected 3 distinct versions, got %d", sl.Len())
	}

	expectedSingle := independentOracleEntryBytes(len(userKey), len(val), height)
	expectedTotal := expectedSingle * 3

	if sl.ByteSize() != expectedTotal {
		t.Fatalf("expected total ByteSize=%d, got %d", expectedTotal, sl.ByteSize())
	}
}

// TestByteSize_Tombstones verifies that an OpTypeDelete tombstone with nil or empty value
// does not charge for nodeValue container or value backing storage.
func TestByteSize_Tombstones(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("tombstone-key")

	ikPut, _ := binary.NewInternalKey(userKey, 10, binary.OpTypePut)
	val := []byte("active-value")
	height := 2

	if err := sl.InsertWithHeightForTesting(ikPut, val, height); err != nil {
		t.Fatalf("insert PUT failed: %v", err)
	}
	expectedPut := independentOracleEntryBytes(len(userKey), len(val), height)
	if sl.ByteSize() != expectedPut {
		t.Fatalf("PUT: expected %d, got %d", expectedPut, sl.ByteSize())
	}

	ikDel, _ := binary.NewInternalKey(userKey, 20, binary.OpTypeDelete)
	if err := sl.InsertWithHeightForTesting(ikDel, nil, height); err != nil {
		t.Fatalf("insert DELETE failed: %v", err)
	}
	expectedDel := independentOracleEntryBytes(len(userKey), 0, height)
	if sl.ByteSize() != expectedPut+expectedDel {
		t.Fatalf("PUT + DELETE: expected %d, got %d", expectedPut+expectedDel, sl.ByteSize())
	}
}

// TestByteSize_HeightSensitivity verifies that node height affects accounted memory
// by exactly (deltaHeight * pointerSize).
func TestByteSize_HeightSensitivity(t *testing.T) {
	heights := []int{1, 2, 4, 8, 16}
	key := []byte("height-test-key")
	val := []byte("height-test-val")

	var prevByteSize uint64
	var prevHeight int

	for i, h := range heights {
		sl := memtable.NewSkipList()
		ik, _ := binary.NewInternalKey(key, binary.SeqNum(i+1), binary.OpTypePut)

		if err := sl.InsertWithHeightForTesting(ik, val, h); err != nil {
			t.Fatalf("insert at height %d failed: %v", h, err)
		}

		currentSize := sl.ByteSize()
		expected := independentOracleEntryBytes(len(key), len(val), h)
		if currentSize != expected {
			t.Fatalf("height %d: expected %d, got %d", h, expected, currentSize)
		}

		if i > 0 {
			diffSize := currentSize - prevByteSize
			expectedDiff := uint64(h-prevHeight) * 8
			if diffSize != expectedDiff {
				t.Fatalf("height step %d->%d: expected diff %d, got %d",
					prevHeight, h, expectedDiff, diffSize)
			}
		}

		prevByteSize = currentSize
		prevHeight = h
	}
}

// TestByteSize_KeySizeScaling verifies that varying key lengths scales ByteSize by exactly
// the key byte difference.
func TestByteSize_KeySizeScaling(t *testing.T) {
	keyLengths := []int{1, 16, 64, 256, 1024, 4096}
	height := 3
	val := []byte("fixed-val")

	var prevSize uint64
	var prevKeyLen int

	for i, klen := range keyLengths {
		sl := memtable.NewSkipList()
		keyBytes := bytes.Repeat([]byte{'k'}, klen)
		ik, _ := binary.NewInternalKey(keyBytes, binary.SeqNum(i+1), binary.OpTypePut)

		if err := sl.InsertWithHeightForTesting(ik, val, height); err != nil {
			t.Fatalf("insert keyLen=%d failed: %v", klen, err)
		}

		currentSize := sl.ByteSize()
		expected := independentOracleEntryBytes(klen, len(val), height)
		if currentSize != expected {
			t.Fatalf("keyLen %d: expected %d, got %d", klen, expected, currentSize)
		}

		if i > 0 {
			diffSize := currentSize - prevSize
			expectedDiff := uint64(klen - prevKeyLen)
			if diffSize != expectedDiff {
				t.Fatalf("keyLen step %d->%d: expected diff %d, got %d",
					prevKeyLen, klen, expectedDiff, diffSize)
			}
		}

		prevSize = currentSize
		prevKeyLen = klen
	}
}

// TestByteSize_ValueSizeScaling verifies that varying value lengths scales ByteSize by exactly
// the value byte difference.
func TestByteSize_ValueSizeScaling(t *testing.T) {
	valLengths := []int{1, 10, 100, 1000, 10000}
	height := 2
	key := []byte("fixed-key")

	var prevSize uint64
	var prevValLen int

	for i, vlen := range valLengths {
		sl := memtable.NewSkipList()
		valBytes := bytes.Repeat([]byte{'v'}, vlen)
		ik, _ := binary.NewInternalKey(key, binary.SeqNum(i+1), binary.OpTypePut)

		if err := sl.InsertWithHeightForTesting(ik, valBytes, height); err != nil {
			t.Fatalf("insert valLen=%d failed: %v", vlen, err)
		}

		currentSize := sl.ByteSize()
		expected := independentOracleEntryBytes(len(key), vlen, height)
		if currentSize != expected {
			t.Fatalf("valLen %d: expected %d, got %d", vlen, expected, currentSize)
		}

		if i > 0 {
			diffSize := currentSize - prevSize
			expectedDiff := uint64(vlen - prevValLen)
			if diffSize != expectedDiff {
				t.Fatalf("valLen step %d->%d: expected diff %d, got %d",
					prevValLen, vlen, expectedDiff, diffSize)
			}
		}

		prevSize = currentSize
		prevValLen = vlen
	}
}

// TestByteSize_FailureAtomicity verifies that failed insertions leave ByteSize and Len unchanged.
func TestByteSize_FailureAtomicity(t *testing.T) {
	validIK, _ := binary.NewInternalKey([]byte("valid-key"), 1, binary.OpTypePut)

	failureCases := []struct {
		name   string
		key    binary.InternalKey
		value  []byte
		height int
	}{
		{
			name:   "empty key",
			key:    binary.InternalKey{UserKey: nil, SeqNum: 2, OpType: binary.OpTypePut},
			value:  []byte("v"),
			height: 2,
		},
		{
			name:   "oversized key",
			key:    binary.InternalKey{UserKey: make([]byte, binary.MaxKeyLen+1), SeqNum: 3, OpType: binary.OpTypePut},
			value:  []byte("v"),
			height: 2,
		},
		{
			name:   "oversized value",
			key:    validIK,
			value:  make([]byte, binary.MaxValueLen+1),
			height: 2,
		},
		{
			name:   "invalid OpType",
			key:    binary.InternalKey{UserKey: []byte("k"), SeqNum: 4, OpType: binary.OpTypeInvalid},
			value:  []byte("v"),
			height: 2,
		},
		{
			name:   "height below min (-1)",
			key:    binary.InternalKey{UserKey: []byte("k"), SeqNum: 5, OpType: binary.OpTypePut},
			value:  []byte("v"),
			height: -1,
		},
		{
			name:   "height above max (17)",
			key:    binary.InternalKey{UserKey: []byte("k"), SeqNum: 6, OpType: binary.OpTypePut},
			value:  []byte("v"),
			height: 17,
		},
	}

	for _, fc := range failureCases {
		t.Run(fc.name, func(t *testing.T) {
			sl := memtable.NewSkipList()
			_ = sl.InsertWithHeightForTesting(validIK, []byte("valid-val"), 2)

			initialLen := sl.Len()
			initialByteSize := sl.ByteSize()

			err := sl.InsertWithHeightForTesting(fc.key, fc.value, fc.height)
			if err == nil {
				t.Fatalf("expected error for case %q, got nil", fc.name)
			}
			if sl.Len() != initialLen {
				t.Fatalf("case %q modified Len: expected %d, got %d", fc.name, initialLen, sl.Len())
			}
			if sl.ByteSize() != initialByteSize {
				t.Fatalf("case %q modified ByteSize: expected %d, got %d", fc.name, initialByteSize, sl.ByteSize())
			}
		})
	}
}

// TestByteSize_ConcurrentObservation tests 1 writer inserting entries while 16 readers
// concurrently call ByteSize(). Verifies race-freedom, monotonicity under pure inserts,
// and exact equality at the end.
func TestByteSize_ConcurrentObservation(t *testing.T) {
	sl := memtable.NewSkipList()
	const numEntries = 1000
	const numReaders = 16

	var expectedTotal uint64
	var entries [numEntries]struct {
		key    binary.InternalKey
		val    []byte
		height int
	}

	for i := 0; i < numEntries; i++ {
		k := []byte(fmt.Sprintf("concurrent-key-%05d", i))
		v := []byte(fmt.Sprintf("concurrent-val-%05d", i))
		ik, _ := binary.NewInternalKey(k, binary.SeqNum(i+1), binary.OpTypePut)
		h := (i % 8) + 1
		entries[i] = struct {
			key    binary.InternalKey
			val    []byte
			height int
		}{key: ik, val: v, height: h}
		expectedTotal += independentOracleEntryBytes(len(k), len(v), h)
	}

	var stopReaders atomic.Bool
	var wg sync.WaitGroup

	// Spawn readers
	for r := 0; r < numReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var prev uint64
			for !stopReaders.Load() {
				bs := sl.ByteSize()
				// Under append-only writes, ByteSize must never decrease
				if bs < prev {
					t.Errorf("observed ByteSize decrease during pure inserts: %d < %d", bs, prev)
					return
				}
				prev = bs
			}
		}()
	}

	// Writer inserts entries
	for i := 0; i < numEntries; i++ {
		if err := sl.InsertWithHeightForTesting(entries[i].key, entries[i].val, entries[i].height); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	stopReaders.Store(true)
	wg.Wait()

	finalSize := sl.ByteSize()
	if finalSize != expectedTotal {
		t.Fatalf("final ByteSize mismatch: expected %d, got %d", expectedTotal, finalSize)
	}
}

// TestByteSize_ConcurrentSearchAndByteSize runs SearchConcurrent alongside ByteSize observation
// and writer mutations to verify that memory accounting does not interfere with lock-free reads.
func TestByteSize_ConcurrentSearchAndByteSize(t *testing.T) {
	sl := memtable.NewSkipList()
	const numEntries = 500
	const numWorkers = 8

	var stopWorkers atomic.Bool
	var wg sync.WaitGroup

	// Pre-populate some keys
	for i := 0; i < 50; i++ {
		k := []byte(fmt.Sprintf("key-%04d", i))
		v := []byte(fmt.Sprintf("val-%04d", i))
		ik, _ := binary.NewInternalKey(k, binary.SeqNum(i+1), binary.OpTypePut)
		_ = sl.Insert(ik, v)
	}

	// Readers calling SearchConcurrent and ByteSize
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			i := 0
			for !stopWorkers.Load() {
				searchKey := []byte(fmt.Sprintf("key-%04d", i%100))
				_, _ = sl.SearchConcurrent(searchKey)
				_ = sl.ByteSize()
				i++
			}
		}(w)
	}

	// Writer inserting new keys and updating existing keys
	for i := 50; i < numEntries; i++ {
		k := []byte(fmt.Sprintf("key-%04d", i))
		v := []byte(fmt.Sprintf("val-%04d", i))
		ik, _ := binary.NewInternalKey(k, binary.SeqNum(i+1), binary.OpTypePut)
		_ = sl.Insert(ik, v)
	}

	stopWorkers.Store(true)
	wg.Wait()

	if sl.Len() != numEntries {
		t.Fatalf("expected Len=%d, got %d", numEntries, sl.Len())
	}
	if sl.ByteSize() == 0 {
		t.Fatalf("expected non-zero ByteSize, got 0")
	}
}

// =========================================================================
// SECURITY INVARIANTS: P03-S02-M02-SEC-INV-01 through 08
// =========================================================================

// TestInvariant_P03_S02_M02_SEC_INV_01_NoAdditionOverflow verifies that ByteSize never
// silently wraps around past math.MaxUint64.
func TestInvariant_P03_S02_M02_SEC_INV_01_NoAdditionOverflow(t *testing.T) {
	var counter atomic.Uint64
	counter.Store(math.MaxUint64 - 10)

	// Adding 5 should succeed: MaxUint64 - 5
	memtable.SafeAddUint64ForTesting(&counter, 5)
	if counter.Load() != uint64(math.MaxUint64-5) {
		t.Fatalf("expected %d, got %d", uint64(math.MaxUint64-5), counter.Load())
	}

	// Adding 10 should saturate at MaxUint64, not wrap to 4
	memtable.SafeAddUint64ForTesting(&counter, 10)
	if counter.Load() != uint64(math.MaxUint64) {
		t.Fatalf("expected saturation at MaxUint64 (%d), got %d (wrapped!)", uint64(math.MaxUint64), counter.Load())
	}

	// Adding more to saturated counter keeps it at MaxUint64
	memtable.SafeAddUint64ForTesting(&counter, 1000)
	if counter.Load() != uint64(math.MaxUint64) {
		t.Fatalf("expected saturated MaxUint64, got %d", counter.Load())
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_02_NoSubtractionUnderflow verifies that safe subtraction
// never underflows below 0 into huge positive numbers.
func TestInvariant_P03_S02_M02_SEC_INV_02_NoSubtractionUnderflow(t *testing.T) {
	var counter atomic.Uint64
	counter.Store(50)

	// Subtracting 30 leaves 20
	memtable.SafeSubUint64ForTesting(&counter, 30)
	if counter.Load() != 20 {
		t.Fatalf("expected 20, got %d", counter.Load())
	}

	// Subtracting 50 should saturate at 0, not underflow to ~2^64 - 30
	memtable.SafeSubUint64ForTesting(&counter, 50)
	if counter.Load() != 0 {
		t.Fatalf("expected saturation at 0, got %d (underflowed!)", counter.Load())
	}

	// Subtracting from 0 keeps it at 0
	memtable.SafeSubUint64ForTesting(&counter, 100)
	if counter.Load() != 0 {
		t.Fatalf("expected 0, got %d", counter.Load())
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_03_FailedInsertionPreservesByteSize verifies that
// any failed insertion strictly leaves ByteSize unchanged.
func TestInvariant_P03_S02_M02_SEC_INV_03_FailedInsertionPreservesByteSize(t *testing.T) {
	sl := memtable.NewSkipList()
	ik, _ := binary.NewInternalKey([]byte("base"), 1, binary.OpTypePut)
	_ = sl.InsertWithHeightForTesting(ik, []byte("val"), 2)

	beforeSize := sl.ByteSize()

	// Attempt empty key insertion
	emptyIK := binary.InternalKey{UserKey: nil, SeqNum: 2, OpType: binary.OpTypePut}
	err := sl.Insert(emptyIK, []byte("val"))
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected ErrEmptyKey, got %v", err)
	}

	afterSize := sl.ByteSize()
	if beforeSize != afterSize {
		t.Fatalf("ByteSize changed on failed insertion: before=%d, after=%d", beforeSize, afterSize)
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_04_ExactDuplicateDoesNotChangeNodeCount verifies that
// duplicate InternalKey updates do not increment Len().
func TestInvariant_P03_S02_M02_SEC_INV_04_ExactDuplicateDoesNotChangeNodeCount(t *testing.T) {
	sl := memtable.NewSkipList()
	ik, _ := binary.NewInternalKey([]byte("dup-test"), 10, binary.OpTypePut)

	_ = sl.Insert(ik, []byte("val1"))
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1, got %d", sl.Len())
	}

	_ = sl.Insert(ik, []byte("val2-longer"))
	if sl.Len() != 1 {
		t.Fatalf("expected Len=1 after duplicate, got %d", sl.Len())
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_05_ExactDuplicateDeltaAccounting verifies that duplicate
// replacement adjusts ByteSize strictly by the value container/payload difference.
func TestInvariant_P03_S02_M02_SEC_INV_05_ExactDuplicateDeltaAccounting(t *testing.T) {
	sl := memtable.NewSkipList()
	ik, _ := binary.NewInternalKey([]byte("dup-delta"), 10, binary.OpTypePut)

	valA := []byte("1234567890") // 10 bytes
	_ = sl.InsertWithHeightForTesting(ik, valA, 2)
	sizeA := sl.ByteSize()

	valB := []byte("123456789012345") // 15 bytes (+5)
	_ = sl.InsertWithHeightForTesting(ik, valB, 2)
	sizeB := sl.ByteSize()

	if sizeB != sizeA+5 {
		t.Fatalf("expected sizeB=%d (+5), got %d", sizeA+5, sizeB)
	}

	valC := []byte("12345") // 5 bytes (-10)
	_ = sl.InsertWithHeightForTesting(ik, valC, 2)
	sizeC := sl.ByteSize()

	if sizeC != sizeB-10 {
		t.Fatalf("expected sizeC=%d (-10), got %d", sizeB-10, sizeC)
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_06_ByteSizeRaceFree verifies race freedom under concurrent calls.
func TestInvariant_P03_S02_M02_SEC_INV_06_ByteSizeRaceFree(t *testing.T) {
	sl := memtable.NewSkipList()
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				k := []byte(fmt.Sprintf("k-%d-%d", id, j))
				ik, _ := binary.NewInternalKey(k, binary.SeqNum(j+1), binary.OpTypePut)
				_ = sl.Insert(ik, []byte("v"))
				_ = sl.ByteSize()
			}
		}(i)
	}

	wg.Wait()
	if sl.ByteSize() == 0 {
		t.Fatalf("expected positive ByteSize, got 0")
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_07_SingleAttribution verifies that every live owned allocation
// is counted exactly once by comparing total ByteSize with a complete Level 0 node traversal.
func TestInvariant_P03_S02_M02_SEC_INV_07_SingleAttribution(t *testing.T) {
	sl := memtable.NewSkipList()

	for i := 0; i < 100; i++ {
		k := []byte(fmt.Sprintf("attr-key-%03d", i))
		v := []byte(fmt.Sprintf("attr-val-%03d", i))
		ik, _ := binary.NewInternalKey(k, binary.SeqNum(i+1), binary.OpTypePut)
		_ = sl.InsertWithHeightForTesting(ik, v, (i%4)+1)
	}

	// Traverse all level 0 nodes and sum their exact independently calculated bytes
	var manualSum uint64
	nodes := sl.NodesAtLevelForTesting(0)
	for _, n := range nodes {
		kLen := len(n.KeyForTesting().UserKey)
		vLen := len(n.RawValueForTesting())
		h := n.HeightForTesting()
		manualSum += independentOracleEntryBytes(kLen, vLen, h)
	}

	if sl.ByteSize() != manualSum {
		t.Fatalf("ByteSize single attribution failed: sl.ByteSize=%d != manualSum=%d",
			sl.ByteSize(), manualSum)
	}
}

// TestInvariant_P03_S02_M02_SEC_INV_08_NoTestMemoryInProductionByteSize verifies that
// test seams and diagnostic calls do not inflate ByteSize.
func TestInvariant_P03_S02_M02_SEC_INV_08_NoTestMemoryInProductionByteSize(t *testing.T) {
	sl := memtable.NewSkipList()
	ik, _ := binary.NewInternalKey([]byte("prod-key"), 1, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("val"))

	expected := sl.ByteSize()

	// Call test diagnostic seams
	_ = sl.ValidateStructureForTesting()
	_ = sl.NodesAtLevelForTesting(0)
	_ = sl.NodeCountAtLevelForTesting(0)
	_ = sl.SearchNodeForTesting([]byte("prod-key"))
	_ = sl.HeadForTesting()

	if sl.ByteSize() != expected {
		t.Fatalf("test helper inflated ByteSize: expected %d, got %d", expected, sl.ByteSize())
	}
}

// TestByteSize_DefensiveNegativeInputs verifies that calculation helpers defensively
// clamp negative input values to zero without panicking or underflowing.
func TestByteSize_DefensiveNegativeInputs(t *testing.T) {
	// Passing negative inputs should clamp to 0 and return base NodeStructSize
	b := memtable.NodeMemoryBytesForTesting(-10, -5, -2)
	if b != memtable.NodeStructSizeForTesting {
		t.Fatalf("expected clamped node bytes = %d, got %d", memtable.NodeStructSizeForTesting, b)
	}

	vb := memtable.ValueMemoryBytesForTesting(-5)
	if vb != 0 {
		t.Fatalf("expected clamped value bytes = 0, got %d", vb)
	}
}

// TestByteSize_SentinelExcludedExplicitly verifies the architectural contract of Model A:
// the head sentinel node infrastructure is explicitly excluded from ByteSize().
func TestByteSize_SentinelExcludedExplicitly(t *testing.T) {
	sl := memtable.NewSkipList()

	// 1. Initial empty list has ByteSize == 0
	if sl.ByteSize() != 0 {
		t.Fatalf("expected initial ByteSize == 0, got %d", sl.ByteSize())
	}

	// 2. Head sentinel node exists and has MaxHeight tower
	head := sl.HeadForTesting()
	if head == nil {
		t.Fatalf("head sentinel is unexpectedly nil")
	}
	if head.HeightForTesting() != memtable.MaxHeight {
		t.Fatalf("expected sentinel height %d, got %d", memtable.MaxHeight, head.HeightForTesting())
	}

	// 3. The theoretical infrastructure cost of the sentinel is:
	// NodeStructSize + MaxHeight * PointerSize = 72 + 16 * 8 = 200 bytes (on 64-bit)
	// ByteSize() explicitly excludes this infrastructure cost
	sentinelCost := memtable.NodeStructSizeForTesting + uint64(memtable.MaxHeight)*memtable.PointerSizeForTesting
	if sl.ByteSize() == sentinelCost {
		t.Fatalf("ByteSize() unexpectedly includes sentinel infrastructure overhead (%d bytes)", sentinelCost)
	}

	// 4. Inserting user entry charges ONLY the user entry, not the sentinel
	ik, _ := binary.NewInternalKey([]byte("user-key"), 1, binary.OpTypePut)
	userVal := []byte("user-val")
	height := 2
	if err := sl.InsertWithHeightForTesting(ik, userVal, height); err != nil {
		t.Fatalf("failed to insert user record: %v", err)
	}

	expectedUserEntry := independentOracleEntryBytes(len(ik.UserKey), len(userVal), height)
	if sl.ByteSize() != expectedUserEntry {
		t.Fatalf("ByteSize after user insert: expected %d, got %d", expectedUserEntry, sl.ByteSize())
	}
}

// TestByteSize_AccumulatedManyEntriesWithDuplicatesAndTombstones executes 1,000 mixed operations
// including fresh inserts, multi-version inserts, tombstones, and duplicate value replacements
// (both growing and shrinking), and verifies that final ByteSize matches the exact sum of all live nodes.
func TestByteSize_AccumulatedManyEntriesWithDuplicatesAndTombstones(t *testing.T) {
	sl := memtable.NewSkipList()
	const numKeys = 200

	// Phase 1: Insert 200 distinct keys with initial values
	for i := 0; i < numKeys; i++ {
		k := []byte(fmt.Sprintf("accum-key-%04d", i))
		v := []byte(fmt.Sprintf("initial-val-%04d", i))
		ik, _ := binary.NewInternalKey(k, 1, binary.OpTypePut)
		h := (i % 8) + 1
		if err := sl.InsertWithHeightForTesting(ik, v, h); err != nil {
			t.Fatalf("initial insert %d failed: %v", i, err)
		}
	}

	// Phase 2: Duplicate update even keys with longer values (grow)
	for i := 0; i < numKeys; i += 2 {
		k := []byte(fmt.Sprintf("accum-key-%04d", i))
		longerVal := bytes.Repeat([]byte("X"), 50)
		ik, _ := binary.NewInternalKey(k, 1, binary.OpTypePut)
		if err := sl.Insert(ik, longerVal); err != nil {
			t.Fatalf("grow duplicate update %d failed: %v", i, err)
		}
	}

	// Phase 3: Duplicate update odd keys with shorter values (shrink)
	for i := 1; i < numKeys; i += 2 {
		k := []byte(fmt.Sprintf("accum-key-%04d", i))
		shorterVal := []byte("s")
		ik, _ := binary.NewInternalKey(k, 1, binary.OpTypePut)
		if err := sl.Insert(ik, shorterVal); err != nil {
			t.Fatalf("shrink duplicate update %d failed: %v", i, err)
		}
	}

	// Phase 4: Duplicate update multiple of 10 keys to empty values (shrink to 0)
	for i := 0; i < numKeys; i += 10 {
		k := []byte(fmt.Sprintf("accum-key-%04d", i))
		ik, _ := binary.NewInternalKey(k, 1, binary.OpTypePut)
		if err := sl.Insert(ik, []byte{}); err != nil {
			t.Fatalf("shrink to 0 update %d failed: %v", i, err)
		}
	}

	// Phase 5: Insert tombstones as new versions (SeqNum = 2, OpTypeDelete)
	for i := 0; i < 50; i++ {
		k := []byte(fmt.Sprintf("accum-key-%04d", i))
		ik, _ := binary.NewInternalKey(k, 2, binary.OpTypeDelete)
		if err := sl.InsertWithHeightForTesting(ik, nil, 2); err != nil {
			t.Fatalf("tombstone insert %d failed: %v", i, err)
		}
	}

	// Verify that sl.ByteSize() matches the exact sum across all live nodes in Level 0
	var manualSum uint64
	nodes := sl.NodesAtLevelForTesting(0)
	for _, n := range nodes {
		kLen := len(n.KeyForTesting().UserKey)
		vLen := len(n.RawValueForTesting())
		h := n.HeightForTesting()
		manualSum += independentOracleEntryBytes(kLen, vLen, h)
	}

	if sl.ByteSize() != manualSum {
		t.Fatalf("accumulated ByteSize drift detected: sl.ByteSize=%d != manualSum=%d (diff: %d)",
			sl.ByteSize(), manualSum, int64(sl.ByteSize())-int64(manualSum))
	}
}

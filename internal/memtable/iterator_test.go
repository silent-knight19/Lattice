package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// helper function to create InternalKey
func makeIK(t *testing.T, userKey string, seq uint64, op binary.OpType) binary.InternalKey {
	t.Helper()
	ik, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seq), op)
	if err != nil {
		t.Fatalf("failed to create InternalKey: %v", err)
	}
	return ik
}

// TestIterator_EmptySkipList verifies iterator behavior on an empty SkipList.
func TestIterator_EmptySkipList(t *testing.T) {
	sl := memtable.NewSkipList()
	it := sl.NewIterator()

	// 1. Initially unpositioned
	if it.Valid() {
		t.Fatalf("expected unpositioned iterator to be invalid")
	}
	if it.Key().UserKey != nil {
		t.Fatalf("expected zero-value key on invalid iterator")
	}
	if it.Value() != nil {
		t.Fatalf("expected nil value on invalid iterator")
	}

	// 2. Next on empty list returns false and transitions to exhausted
	if it.Next() {
		t.Fatalf("expected Next() on empty list to return false")
	}
	if it.Valid() {
		t.Fatalf("expected exhausted iterator to be invalid")
	}

	// 3. Repeated Next calls remain false
	for i := 0; i < 5; i++ {
		if it.Next() {
			t.Fatalf("repeated Next() returned true")
		}
	}

	// 4. SeekToFirst on empty list remains invalid
	it.SeekToFirst()
	if it.Valid() {
		t.Fatalf("expected SeekToFirst on empty list to remain invalid")
	}

	// 5. Seek on empty list remains invalid
	if err := it.Seek([]byte("nonexistent")); err != nil {
		t.Fatalf("unexpected Seek error on valid key: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected Seek on empty list to remain invalid")
	}

	// 6. Close is safe and idempotent
	it.Close()
	it.Close()
	if it.Valid() {
		t.Fatalf("expected closed iterator to be invalid")
	}
}

// TestIterator_SingleEntry verifies traversal over a single entry SkipList.
func TestIterator_SingleEntry(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "single", 10, binary.OpTypePut)
	val := []byte("single-val")
	_ = sl.Insert(ik, val)

	it := sl.NewIterator()

	// Next advances to the single entry
	if !it.Next() {
		t.Fatalf("expected Next() to advance to first entry")
	}
	if !it.Valid() {
		t.Fatalf("expected iterator to be valid")
	}
	if !it.Key().Equal(ik) {
		t.Fatalf("expected key %v, got %v", ik, it.Key())
	}
	if !bytes.Equal(it.Value(), val) {
		t.Fatalf("expected value %q, got %q", val, it.Value())
	}

	// Next again reaches EOF
	if it.Next() {
		t.Fatalf("expected second Next() to return false at EOF")
	}
	if it.Valid() {
		t.Fatalf("expected iterator to be invalid at EOF")
	}

	// SeekToFirst repositions on the single entry
	it.SeekToFirst()
	if !it.Valid() {
		t.Fatalf("expected SeekToFirst to position on single entry")
	}
	if !it.Key().Equal(ik) {
		t.Fatalf("expected key %v, got %v", ik, it.Key())
	}
}

// TestIterator_MultipleOrderedEntries verifies forward traversal across multiple keys
// inserted in ascending, descending, and randomized orders.
func TestIterator_MultipleOrderedEntries(t *testing.T) {
	testOrders := []struct {
		name      string
		generator func() []string
	}{
		{
			name: "Ascending Insertion",
			generator: func() []string {
				return []string{"a", "b", "c", "d", "e", "f", "g"}
			},
		},
		{
			name: "Descending Insertion",
			generator: func() []string {
				return []string{"g", "f", "e", "d", "c", "b", "a"}
			},
		},
		{
			name: "Random Insertion",
			generator: func() []string {
				keys := []string{"d", "a", "g", "b", "f", "c", "e"}
				return keys
			},
		},
	}

	expectedSortedKeys := []string{"a", "b", "c", "d", "e", "f", "g"}

	for _, tc := range testOrders {
		t.Run(tc.name, func(t *testing.T) {
			sl := memtable.NewSkipList()
			keysToInsert := tc.generator()

			for i, k := range keysToInsert {
				ik := makeIK(t, k, uint64(i+1), binary.OpTypePut)
				val := []byte("val-" + k)
				_ = sl.Insert(ik, val)
			}

			// 1. Iterate using Next() loop
			it := sl.NewIterator()
			var observedKeys []string
			for it.Next() {
				observedKeys = append(observedKeys, string(it.Key().UserKey))
				expectedVal := "val-" + string(it.Key().UserKey)
				if string(it.Value()) != expectedVal {
					t.Fatalf("expected value %q, got %q", expectedVal, string(it.Value()))
				}
			}

			if len(observedKeys) != len(expectedSortedKeys) {
				t.Fatalf("expected %d keys, got %d", len(expectedSortedKeys), len(observedKeys))
			}
			for i := range expectedSortedKeys {
				if observedKeys[i] != expectedSortedKeys[i] {
					t.Fatalf("key mismatch at index %d: expected %q, got %q",
						i, expectedSortedKeys[i], observedKeys[i])
				}
			}

			// 2. Iterate using SeekToFirst + Valid loop
			it2 := sl.NewIterator()
			var observedKeys2 []string
			for it2.SeekToFirst(); it2.Valid(); it2.Next() {
				observedKeys2 = append(observedKeys2, string(it2.Key().UserKey))
			}
			if len(observedKeys2) != len(expectedSortedKeys) {
				t.Fatalf("SeekToFirst iteration: expected %d keys, got %d",
					len(expectedSortedKeys), len(observedKeys2))
			}
			for i := range expectedSortedKeys {
				if observedKeys2[i] != expectedSortedKeys[i] {
					t.Fatalf("SeekToFirst mismatch at index %d: expected %q, got %q",
						i, expectedSortedKeys[i], observedKeys2[i])
				}
			}
		})
	}
}

// TestIterator_MultiVersionAndTieOrdering verifies canonical multi-version sorting:
// 1. UserKey ASC
// 2. SeqNum DESC (newer version before older version)
// 3. OpType DESC (DELETE/Tombstone before PUT for same userKey and seqNum)
func TestIterator_MultiVersionAndTieOrdering(t *testing.T) {
	sl := memtable.NewSkipList()

	// Insert multiple versions for "apple": seq 100 (PUT), seq 50 (PUT), seq 200 (PUT)
	// Insert multiple versions for "banana": seq 30 (PUT), seq 30 (DELETE)
	// Insert single version for "cherry": seq 10 (PUT)

	_ = sl.Insert(makeIK(t, "apple", 100, binary.OpTypePut), []byte("apple-100"))
	_ = sl.Insert(makeIK(t, "apple", 50, binary.OpTypePut), []byte("apple-50"))
	_ = sl.Insert(makeIK(t, "apple", 200, binary.OpTypePut), []byte("apple-200"))

	_ = sl.Insert(makeIK(t, "banana", 30, binary.OpTypePut), []byte("banana-put-30"))
	_ = sl.Insert(makeIK(t, "banana", 30, binary.OpTypeDelete), nil) // tombstone

	_ = sl.Insert(makeIK(t, "cherry", 10, binary.OpTypePut), []byte("cherry-10"))

	expectedSequence := []struct {
		userKey string
		seq     uint64
		op      binary.OpType
		val     string
	}{
		{"apple", 200, binary.OpTypePut, "apple-200"},
		{"apple", 100, binary.OpTypePut, "apple-100"},
		{"apple", 50, binary.OpTypePut, "apple-50"},
		{"banana", 30, binary.OpTypeDelete, ""}, // Tombstone precedes PUT on identical seqNum
		{"banana", 30, binary.OpTypePut, "banana-put-30"},
		{"cherry", 10, binary.OpTypePut, "cherry-10"},
	}

	it := sl.NewIterator()
	idx := 0
	for it.Next() {
		if idx >= len(expectedSequence) {
			t.Fatalf("iterator produced more entries than expected (%d)", idx)
		}
		expected := expectedSequence[idx]

		k := it.Key()
		if string(k.UserKey) != expected.userKey {
			t.Fatalf("idx %d: expected userKey %q, got %q", idx, expected.userKey, string(k.UserKey))
		}
		if uint64(k.SeqNum) != expected.seq {
			t.Fatalf("idx %d: expected SeqNum %d, got %d", idx, expected.seq, uint64(k.SeqNum))
		}
		if k.OpType != expected.op {
			t.Fatalf("idx %d: expected OpType %s, got %s", idx, expected.op, k.OpType)
		}

		if expected.op == binary.OpTypeDelete {
			if it.Value() != nil {
				t.Fatalf("idx %d: expected nil value for tombstone, got %q", idx, string(it.Value()))
			}
		} else {
			if string(it.Value()) != expected.val {
				t.Fatalf("idx %d: expected value %q, got %q", idx, expected.val, string(it.Value()))
			}
		}
		idx++
	}

	if idx != len(expectedSequence) {
		t.Fatalf("iterator yielded %d entries, expected %d", idx, len(expectedSequence))
	}
}

// TestIterator_BinaryAndHighBitKeys verifies ordering of arbitrary binary byte sequences
// including 0x00, 0x7F, 0x80, 0xFF, and prefix relationships.
func TestIterator_BinaryAndHighBitKeys(t *testing.T) {
	sl := memtable.NewSkipList()

	rawKeys := [][]byte{
		{0xFF, 0x01},
		{0x80},
		{0x00, 0x01},
		{0x00},
		{0x7F, 0xFF},
		{'a'},
		{'a', 'b'},
		{'a', 0xFF},
	}

	for i, rk := range rawKeys {
		ik, err := binary.NewInternalKey(rk, binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			t.Fatalf("failed to create key for %v: %v", rk, err)
		}
		_ = sl.Insert(ik, []byte(fmt.Sprintf("val-%d", i)))
	}

	it := sl.NewIterator()
	var prev binary.InternalKey
	first := true
	count := 0

	for it.Next() {
		curr := it.Key()
		if !first {
			cmp := binary.CompareInternalKey(prev, curr)
			if cmp >= 0 {
				t.Fatalf("ordering violation: prev %v >= curr %v (cmp=%d)", prev, curr, cmp)
			}
		}
		first = false
		prev = curr
		count++
	}

	if count != len(rawKeys) {
		t.Fatalf("expected %d entries, got %d", len(rawKeys), count)
	}
}

// TestIterator_SeekExactAndBounds exercises Seek across all boundary conditions.
func TestIterator_SeekExactAndBounds(t *testing.T) {
	sl := memtable.NewSkipList()

	// Keys: "c", "e", "g", "i"
	keys := []string{"c", "e", "g", "i"}
	for i, k := range keys {
		_ = sl.Insert(makeIK(t, k, uint64(i+1), binary.OpTypePut), []byte("val-"+k))
	}

	it := sl.NewIterator()

	// 1. Seek to exact key: "e" -> lands on "e"
	if err := it.Seek([]byte("e")); err != nil {
		t.Fatalf("seek 'e' failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "e" {
		t.Fatalf("expected 'e', got valid=%v key=%q", it.Valid(), string(it.Key().UserKey))
	}

	// 2. Seek to nonexistent key between two keys: "f" -> lands on "g"
	if err := it.Seek([]byte("f")); err != nil {
		t.Fatalf("seek 'f' failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "g" {
		t.Fatalf("expected 'g', got valid=%v key=%q", it.Valid(), string(it.Key().UserKey))
	}

	// 3. Seek below minimum: "a" -> lands on "c"
	if err := it.Seek([]byte("a")); err != nil {
		t.Fatalf("seek 'a' failed: %v", err)
	}
	if !it.Valid() || string(it.Key().UserKey) != "c" {
		t.Fatalf("expected 'c', got valid=%v key=%q", it.Valid(), string(it.Key().UserKey))
	}

	// 4. Seek above maximum: "z" -> exhausted (Valid() == false)
	if err := it.Seek([]byte("z")); err != nil {
		t.Fatalf("seek 'z' failed: %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected invalid iterator for seek 'z', got %q", string(it.Key().UserKey))
	}

	// 5. Seek with empty key returns ErrEmptyKey and invalidates iterator
	err := it.Seek(nil)
	if !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected ErrEmptyKey, got %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected invalid iterator after failed seek")
	}

	// 6. Seek with oversized key returns ErrKeyTooLarge
	err = it.Seek(make([]byte, binary.MaxKeyLen+1))
	if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge, got %v", err)
	}
	if it.Valid() {
		t.Fatalf("expected invalid iterator after oversized seek")
	}
}

// TestIterator_SeekMultiVersionLandsOnNewestRevision verifies that seeking for a UserKey
// with multiple historical versions lands strictly on the newest revision (highest SeqNum).
func TestIterator_SeekMultiVersionLandsOnNewestRevision(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("target")

	_ = sl.Insert(makeIK(t, "target", 100, binary.OpTypePut), []byte("v100"))
	_ = sl.Insert(makeIK(t, "target", 50, binary.OpTypePut), []byte("v50"))
	_ = sl.Insert(makeIK(t, "target", 300, binary.OpTypePut), []byte("v300"))
	_ = sl.Insert(makeIK(t, "target", 200, binary.OpTypePut), []byte("v200"))

	it := sl.NewIterator()
	if err := it.Seek(userKey); err != nil {
		t.Fatalf("seek failed: %v", err)
	}

	if !it.Valid() {
		t.Fatalf("expected valid iterator after seek")
	}
	if uint64(it.Key().SeqNum) != 300 {
		t.Fatalf("expected seek to land on newest revision (300), got %d", uint64(it.Key().SeqNum))
	}
	if string(it.Value()) != "v300" {
		t.Fatalf("expected value 'v300', got %q", string(it.Value()))
	}

	// Advancing Next() visits descending revisions in order: 200 -> 100 -> 50
	expectedSeqs := []uint64{200, 100, 50}
	for _, expectedSeq := range expectedSeqs {
		if !it.Next() {
			t.Fatalf("expected Next() to advance to seq %d", expectedSeq)
		}
		if uint64(it.Key().SeqNum) != expectedSeq {
			t.Fatalf("expected seq %d, got %d", expectedSeq, uint64(it.Key().SeqNum))
		}
	}

	if it.Next() {
		t.Fatalf("expected EOF after last revision")
	}
}

// TestIterator_SeekInternalKey verifies seeking by explicit InternalKey.
func TestIterator_SeekInternalKey(t *testing.T) {
	sl := memtable.NewSkipList()

	_ = sl.Insert(makeIK(t, "k", 100, binary.OpTypePut), []byte("v100"))
	_ = sl.Insert(makeIK(t, "k", 50, binary.OpTypePut), []byte("v50"))
	_ = sl.Insert(makeIK(t, "m", 10, binary.OpTypePut), []byte("vm"))

	it := sl.NewIterator()

	// SeekInternalKey to (k, seq=80, PUT) -> should land on (k, seq=50, PUT)
	// because (k, seq=100) sorts BEFORE target in canonical order
	target := makeIK(t, "k", 80, binary.OpTypePut)
	if err := it.SeekInternalKey(target); err != nil {
		t.Fatalf("SeekInternalKey failed: %v", err)
	}
	if !it.Valid() {
		t.Fatalf("expected valid iterator")
	}
	if uint64(it.Key().SeqNum) != 50 {
		t.Fatalf("expected to land on seq 50, got %d", uint64(it.Key().SeqNum))
	}
}

// TestIterator_MemoryOwnershipAndDefensiveCopying verifies that mutating returned
// Key() or Value() slices does not corrupt the SkipList internal storage.
func TestIterator_MemoryOwnershipAndDefensiveCopying(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "immutable-key", 1, binary.OpTypePut)
	val := []byte("immutable-val")
	_ = sl.Insert(ik, val)

	it := sl.NewIterator()
	it.SeekToFirst()
	if !it.Valid() {
		t.Fatalf("expected valid iterator")
	}

	// 1. Mutate returned Key UserKey slice
	keyCopy := it.Key()
	keyCopy.UserKey[0] = 'X'
	keyCopy.UserKey[1] = 'Y'

	// Re-query iterator Key
	freshKey := it.Key()
	if string(freshKey.UserKey) != "immutable-key" {
		t.Fatalf("stored key was mutated: got %q", string(freshKey.UserKey))
	}

	// 2. Mutate returned Value slice
	valCopy := it.Value()
	valCopy[0] = 'Z'
	valCopy[1] = 'W'

	// Re-query iterator Value
	freshVal := it.Value()
	if string(freshVal) != "immutable-val" {
		t.Fatalf("stored value was mutated: got %q", string(freshVal))
	}

	// Search via point lookup to confirm stored state is unmodified
	storedVal, err := sl.Search([]byte("immutable-key"))
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if string(storedVal) != "immutable-val" {
		t.Fatalf("stored value in SkipList was corrupted: got %q", string(storedVal))
	}
}

// TestIterator_LiveMutationVisibility exercises the live weakly-consistent traversal contract:
// 1. Insertions ahead of current position are observed.
// 2. Exact duplicate updates at current position update Value() immediately.
func TestIterator_LiveMutationVisibility(t *testing.T) {
	sl := memtable.NewSkipList()

	_ = sl.Insert(makeIK(t, "a", 1, binary.OpTypePut), []byte("val-a"))
	_ = sl.Insert(makeIK(t, "c", 1, binary.OpTypePut), []byte("val-c"))

	it := sl.NewIterator()
	it.SeekToFirst()
	if string(it.Key().UserKey) != "a" {
		t.Fatalf("expected 'a', got %q", string(it.Key().UserKey))
	}

	// Insert "b" between "a" and "c" while iterator is resting on "a"
	_ = sl.Insert(makeIK(t, "b", 1, binary.OpTypePut), []byte("val-b"))

	// Advance iterator: should see "b"
	if !it.Next() {
		t.Fatalf("expected Next() to see inserted 'b'")
	}
	if string(it.Key().UserKey) != "b" {
		t.Fatalf("expected 'b', got %q", string(it.Key().UserKey))
	}

	// Exact duplicate update of "b" while iterator is resting on "b"
	_ = sl.Insert(makeIK(t, "b", 1, binary.OpTypePut), []byte("val-b-updated"))

	// Re-reading Value() returns updated value container
	if string(it.Value()) != "val-b-updated" {
		t.Fatalf("expected updated value 'val-b-updated', got %q", string(it.Value()))
	}

	// Advance to "c"
	if !it.Next() {
		t.Fatalf("expected Next() to advance to 'c'")
	}
	if string(it.Key().UserKey) != "c" {
		t.Fatalf("expected 'c', got %q", string(it.Key().UserKey))
	}

	// Insert "d" after "c" while resting on "c"
	_ = sl.Insert(makeIK(t, "d", 1, binary.OpTypePut), []byte("val-d"))
	if !it.Next() {
		t.Fatalf("expected Next() to advance to 'd'")
	}
	if string(it.Key().UserKey) != "d" {
		t.Fatalf("expected 'd', got %q", string(it.Key().UserKey))
	}
}

// TestIterator_ConcurrentWriterStress tests 1 writer inserting entries while 8 concurrent
// iterators continuously scan and seek without deadlocks, panics, or data races under -race.
func TestIterator_ConcurrentWriterStress(t *testing.T) {
	sl := memtable.NewSkipList()
	const numWrites = 1000
	const numIterators = 8

	// Pre-populate some keys
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("key-%05d", i*10)
		_ = sl.Insert(makeIK(t, k, 1, binary.OpTypePut), []byte("v"))
	}

	var stopSignal atomic.Bool
	var wg sync.WaitGroup

	// Spawn readers
	for w := 0; w < numIterators; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for !stopSignal.Load() {
				it := sl.NewIterator()
				// Alternate between full scan and Seek
				if id%2 == 0 {
					it.SeekToFirst()
					var prevKey binary.InternalKey
					first := true
					for it.Valid() {
						k := it.Key()
						_ = it.Value()
						if !first {
							if binary.CompareInternalKey(prevKey, k) >= 0 {
								t.Errorf("ordering violation in concurrent iterator: %v >= %v", prevKey, k)
								return
							}
						}
						first = false
						prevKey = k
						it.Next()
					}
				} else {
					target := fmt.Sprintf("key-%05d", rand.Intn(numWrites)) //nolint:gosec // PRNG suitable for concurrent test inputs
					if err := it.Seek([]byte(target)); err == nil && it.Valid() {
						_ = it.Key()
						_ = it.Value()
					}
				}
				it.Close()
			}
		}(w)
	}

	// Writer goroutine inserting fresh entries and updating duplicates
	for i := 0; i < numWrites; i++ {
		k := fmt.Sprintf("key-%05d", i)
		val := []byte(fmt.Sprintf("val-%d", i))
		_ = sl.Insert(makeIK(t, k, uint64(i+1), binary.OpTypePut), val)
	}

	stopSignal.Store(true)
	wg.Wait()

	if sl.Len() == 0 {
		t.Fatalf("expected non-empty SkipList")
	}
}

// TestIterator_ConcurrentDuplicateUpdateWhileReading exercises reading Value()
// while a serialized writer repeatedly swaps the nodeValue container.
func TestIterator_ConcurrentDuplicateUpdateWhileReading(t *testing.T) {
	sl := memtable.NewSkipList()
	ik := makeIK(t, "hot-key", 1, binary.OpTypePut)
	_ = sl.Insert(ik, []byte("initial"))

	var stopSignal atomic.Bool
	var wg sync.WaitGroup

	// 4 Readers repeatedly reading Value()
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it := sl.NewIterator()
			it.SeekToFirst()
			for !stopSignal.Load() {
				if it.Valid() {
					v := it.Value()
					if len(v) == 0 {
						t.Errorf("observed empty value during concurrent update")
						return
					}
				}
			}
		}()
	}

	// Writer repeatedly updating hot-key
	for i := 0; i < 500; i++ {
		val := []byte(fmt.Sprintf("updated-val-%d", i))
		_ = sl.Insert(ik, val)
	}

	stopSignal.Store(true)
	wg.Wait()
}

// =========================================================================
// INVARIANT REGRESSION TESTS: P03-S03-M01-INV-01 through 06
// =========================================================================

// TestInvariant_P03_S03_M01_INV_01_CompleteSingleTraversal verifies that on a static SkipList,
// repeated Next() traverses exactly the level-0 sequence once.
func TestInvariant_P03_S03_M01_INV_01_CompleteSingleTraversal(t *testing.T) {
	sl := memtable.NewSkipList()
	const count = 100

	for i := 0; i < count; i++ {
		_ = sl.Insert(makeIK(t, fmt.Sprintf("k-%03d", i), 1, binary.OpTypePut), []byte("v"))
	}

	it := sl.NewIterator()
	visited := make(map[string]int)

	for it.Next() {
		k := string(it.Key().UserKey)
		visited[k]++
	}

	if len(visited) != count {
		t.Fatalf("expected %d distinct keys visited, got %d", count, len(visited))
	}
	for k, v := range visited {
		if v != 1 {
			t.Fatalf("key %q visited %d times (expected exactly 1)", k, v)
		}
	}
}

// TestInvariant_P03_S03_M01_INV_02_MonotonicInternalKeyOrdering verifies that keys returned
// by successive valid iterator positions are non-decreasing under CompareInternalKey.
func TestInvariant_P03_S03_M01_INV_02_MonotonicInternalKeyOrdering(t *testing.T) {
	sl := memtable.NewSkipList()

	// Insert random mixture of keys and versions
	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("key-%02d", rand.Intn(50)) //nolint:gosec // PRNG suitable for property test inputs
		seq := uint64(rand.Intn(100) + 1)           //nolint:gosec // PRNG suitable for property test inputs
		op := binary.OpTypePut
		if rand.Intn(10) == 0 { //nolint:gosec // PRNG suitable for property test inputs
			op = binary.OpTypeDelete
		}
		_ = sl.Insert(makeIK(t, k, seq, op), []byte("val"))
	}

	it := sl.NewIterator()
	it.SeekToFirst()

	var prev binary.InternalKey
	first := true

	for it.Valid() {
		curr := it.Key()
		if !first {
			if binary.CompareInternalKey(prev, curr) >= 0 {
				t.Fatalf("ordering invariant violated: %v >= %v", prev, curr)
			}
		}
		first = false
		prev = curr
		it.Next()
	}
}

// TestInvariant_P03_S03_M01_INV_03_TraversalAcyclicity verifies that forward traversal
// terminates at nil and does not cycle.
func TestInvariant_P03_S03_M01_INV_03_TraversalAcyclicity(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 50; i++ {
		_ = sl.Insert(makeIK(t, fmt.Sprintf("acyc-%02d", i), 1, binary.OpTypePut), []byte("v"))
	}

	it := sl.NewIterator()
	steps := 0
	const maxSteps = 1000 // If it cycles, it will exceed maxSteps

	for it.Next() {
		steps++
		if steps > maxSteps {
			t.Fatalf("possible cycle detected: iterator exceeded %d steps", maxSteps)
		}
	}

	if steps != 50 {
		t.Fatalf("expected exactly 50 steps, got %d", steps)
	}
}

// TestInvariant_P03_S03_M01_INV_04_SeekPredicateExactness verifies that Seek returns
// the lowest position satisfying UserKey >= target.
func TestInvariant_P03_S03_M01_INV_04_SeekPredicateExactness(t *testing.T) {
	sl := memtable.NewSkipList()
	keys := []string{"10", "20", "30", "40", "50"}

	for _, k := range keys {
		_ = sl.Insert(makeIK(t, k, 1, binary.OpTypePut), []byte("v"))
	}

	it := sl.NewIterator()

	// Seek to "25" -> must land on "30"
	_ = it.Seek([]byte("25"))
	if !it.Valid() || string(it.Key().UserKey) != "30" {
		t.Fatalf("seek '25': expected '30', got %q", string(it.Key().UserKey))
	}

	// Seek to "10" -> must land on "10"
	_ = it.Seek([]byte("10"))
	if !it.Valid() || string(it.Key().UserKey) != "10" {
		t.Fatalf("seek '10': expected '10', got %q", string(it.Key().UserKey))
	}

	// Seek to "55" -> invalid
	_ = it.Seek([]byte("55"))
	if it.Valid() {
		t.Fatalf("seek '55': expected invalid, got %q", string(it.Key().UserKey))
	}
}

// TestInvariant_P03_S03_M01_INV_05_MemoryIsolation verifies that caller mutations to Key/Value
// do not affect subsequent reads or internal state.
func TestInvariant_P03_S03_M01_INV_05_MemoryIsolation(t *testing.T) {
	sl := memtable.NewSkipList()
	_ = sl.Insert(makeIK(t, "isolate", 1, binary.OpTypePut), []byte("original"))

	it := sl.NewIterator()
	it.SeekToFirst()

	k := it.Key()
	k.UserKey[0] = 'Z'

	v := it.Value()
	v[0] = 'Z'

	// Fresh read must remain original
	if string(it.Key().UserKey) != "isolate" {
		t.Fatalf("Key mutated: %s", string(it.Key().UserKey))
	}
	if string(it.Value()) != "original" {
		t.Fatalf("Value mutated: %s", string(it.Value()))
	}
}

// TestInvariant_P03_S03_M01_INV_06_RaceFreeConcurrency verifies race-freedom across concurrent
// iterators and continuous writer mutations.
func TestInvariant_P03_S03_M01_INV_06_RaceFreeConcurrency(t *testing.T) {
	sl := memtable.NewSkipList()
	var wg sync.WaitGroup

	// Writer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = sl.Insert(makeIK(t, fmt.Sprintf("rf-%03d", i), uint64(i), binary.OpTypePut), []byte("v"))
		}
	}()

	// 4 Iterator goroutines
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				it := sl.NewIterator()
				for it.Next() {
					_ = it.Key()
					_ = it.Value()
				}
				it.Close()
			}
		}()
	}

	wg.Wait()
}

func TestIterator_NilReceiverSafety(t *testing.T) {
	var it *memtable.Iterator

	if it.Valid() {
		t.Errorf("expected Valid() false for nil iterator")
	}
	if it.Next() {
		t.Errorf("expected Next() false for nil iterator")
	}
	if it.Key().UserKey != nil {
		t.Errorf("expected zero-value InternalKey for nil iterator")
	}
	if it.Value() != nil {
		t.Errorf("expected nil Value for nil iterator")
	}
	if err := it.Seek([]byte("k")); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Errorf("expected ErrNilReceiver on Seek, got: %v", err)
	}
	if err := it.SeekInternalKey(binary.InternalKey{}); !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Errorf("expected ErrNilReceiver on SeekInternalKey, got: %v", err)
	}
	// SeekToFirst and Close must not panic
	it.SeekToFirst()
	it.Close()
}

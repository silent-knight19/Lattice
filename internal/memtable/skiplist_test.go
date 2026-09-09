package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

func TestSkipList_NewAndEmpty(t *testing.T) {
	sl := memtable.NewSkipList()
	if sl == nil {
		t.Fatalf("expected non-nil SkipList")
	}
	if sl.Len() != 0 {
		t.Errorf("expected Len() == 0, got %d", sl.Len())
	}
	if !sl.IsEmpty() {
		t.Errorf("expected IsEmpty() == true")
	}
	if sl.Height() != memtable.MinHeight {
		t.Errorf("expected Height() == %d, got %d", memtable.MinHeight, sl.Height())
	}

	val, err := sl.Search([]byte("nonexistent"))
	if val != nil {
		t.Errorf("expected nil value, got %v", val)
	}
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("empty SkipList failed structural validation: %v", err)
	}
}

func TestSkipList_SingleInsertAndSearch(t *testing.T) {
	sl := memtable.NewSkipList()
	key := sampleKey(t, "user:1001", 1, binary.OpTypePut)
	value := []byte("alice")

	if err := sl.Insert(key, value); err != nil {
		t.Fatalf("unexpected insert error: %v", err)
	}

	if sl.Len() != 1 {
		t.Errorf("expected Len() == 1, got %d", sl.Len())
	}
	if sl.IsEmpty() {
		t.Errorf("expected IsEmpty() == false")
	}

	// Successful search
	got, err := sl.Search([]byte("user:1001"))
	if err != nil {
		t.Fatalf("unexpected search error: %v", err)
	}
	if !bytes.Equal(got, value) {
		t.Errorf("expected value %q, got %q", value, got)
	}

	// Miss search
	gotMiss, err := sl.Search([]byte("user:1002"))
	if gotMiss != nil {
		t.Errorf("expected nil value for miss, got %v", gotMiss)
	}
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound, got %v", err)
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_MultipleOrderedInserts(t *testing.T) {
	sl := memtable.NewSkipList()
	keys := []string{"date", "banana", "fig", "apple", "cherry"}
	for i, k := range keys {
		ikey := sampleKey(t, k, uint64(i+1), binary.OpTypePut)
		if err := sl.Insert(ikey, []byte("val-"+k)); err != nil {
			t.Fatalf("insert %q error: %v", k, err)
		}
	}

	if sl.Len() != len(keys) {
		t.Errorf("expected Len() == %d, got %d", len(keys), sl.Len())
	}

	for _, k := range keys {
		got, err := sl.Search([]byte(k))
		if err != nil {
			t.Errorf("search %q failed: %v", k, err)
		}
		if string(got) != "val-"+k {
			t.Errorf("search %q expected %q, got %q", k, "val-"+k, string(got))
		}
	}

	// Verify strict ascending order at level 0: apple, banana, cherry, date, fig
	nodes := sl.NodesAtLevelForTesting(0)
	expectedOrder := []string{"apple", "banana", "cherry", "date", "fig"}
	if len(nodes) != len(expectedOrder) {
		t.Fatalf("expected %d nodes at level 0, got %d", len(expectedOrder), len(nodes))
	}
	for i, exp := range expectedOrder {
		if string(nodes[i].KeyForTesting().UserKey) != exp {
			t.Errorf("index %d expected %q, got %q", i, exp, string(nodes[i].KeyForTesting().UserKey))
		}
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_BinaryByteOrderAndPrefixes(t *testing.T) {
	sl := memtable.NewSkipList()

	// High-bit bytes and prefixes to verify unsigned byte comparison
	byteKeys := [][]byte{
		{0xFF},
		{0x80},
		{0x00},
		{0x7F},
		[]byte("aa"),
		[]byte("a"),
		[]byte("aaa"),
		{0x80, 0x01},
		{0x00, 0x01},
	}

	for i, bk := range byteKeys {
		k, err := binary.NewInternalKey(bk, binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			t.Fatalf("failed creating key %x: %v", bk, err)
		}
		if err := sl.Insert(k, []byte(fmt.Sprintf("val-%d", i))); err != nil {
			t.Fatalf("insert %x failed: %v", bk, err)
		}
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("binary keys order validation failed: %v", err)
	}

	// Verify all keys can be searched successfully
	for i, bk := range byteKeys {
		got, err := sl.Search(bk)
		if err != nil {
			t.Errorf("search %x failed: %v", bk, err)
		}
		expectedVal := fmt.Sprintf("val-%d", i)
		if string(got) != expectedVal {
			t.Errorf("search %x expected %q, got %q", bk, expectedVal, string(got))
		}
	}
}

func TestSkipList_MultiVersionDescendingSequence(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("account:balance")

	// Insert multiple revisions with different sequence numbers in random order
	seqs := []uint64{50, 100, 10, 90, 1, 0, math.MaxUint64}
	for _, seq := range seqs {
		k, err := binary.NewInternalKey(userKey, binary.SeqNum(seq), binary.OpTypePut)
		if err != nil {
			t.Fatalf("failed creating key with seq %d: %v", seq, err)
		}
		val := []byte(fmt.Sprintf("bal-at-seq-%d", seq))
		if err := sl.Insert(k, val); err != nil {
			t.Fatalf("insert seq %d failed: %v", seq, err)
		}
	}

	if sl.Len() != len(seqs) {
		t.Fatalf("expected Len() == %d, got %d", len(seqs), sl.Len())
	}

	// P03-S01-M02-INV-07: Search must return the newest version (SeqNum MaxUint64)
	got, err := sl.Search(userKey)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	expected := fmt.Sprintf("bal-at-seq-%d", uint64(math.MaxUint64))
	if string(got) != expected {
		t.Errorf("expected newest version %q, got %q", expected, string(got))
	}

	// Verify level 0 sequence order is strictly descending:
	// MaxUint64, 100, 90, 50, 10, 1, 0
	nodes := sl.NodesAtLevelForTesting(0)
	expectedSeqs := []uint64{math.MaxUint64, 100, 90, 50, 10, 1, 0}
	if len(nodes) != len(expectedSeqs) {
		t.Fatalf("expected %d nodes, got %d", len(expectedSeqs), len(nodes))
	}
	for i, expSeq := range expectedSeqs {
		if uint64(nodes[i].KeyForTesting().SeqNum) != expSeq {
			t.Errorf("index %d expected seq %d, got %d", i, expSeq, nodes[i].KeyForTesting().SeqNum)
		}
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_TombstoneShadowing(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("user:profile")

	// 1. Initial PUT at seq 10
	k1, _ := binary.NewInternalKey(userKey, 10, binary.OpTypePut)
	if err := sl.Insert(k1, []byte("v1-alice")); err != nil {
		t.Fatalf("insert PUT failed: %v", err)
	}

	got, err := sl.Search(userKey)
	if err != nil || string(got) != "v1-alice" {
		t.Fatalf("expected v1-alice, got %q (err: %v)", got, err)
	}

	// 2. DELETE (Tombstone) at seq 20
	k2, _ := binary.NewInternalKey(userKey, 20, binary.OpTypeDelete)
	if err := sl.Insert(k2, nil); err != nil {
		t.Fatalf("insert DELETE failed: %v", err)
	}

	// Point lookup must report ErrKeyNotFound because newest version is a tombstone
	gotDel, err := sl.Search(userKey)
	if gotDel != nil {
		t.Errorf("expected nil value after tombstone, got %v", gotDel)
	}
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound after tombstone, got %v", err)
	}

	// Both versions must physically exist in the list
	if sl.Len() != 2 {
		t.Errorf("expected Len() == 2 (both versions preserved), got %d", sl.Len())
	}

	// 3. New PUT at seq 30 overrides tombstone
	k3, _ := binary.NewInternalKey(userKey, 30, binary.OpTypePut)
	if err := sl.Insert(k3, []byte("v3-alice-recreated")); err != nil {
		t.Fatalf("insert re-PUT failed: %v", err)
	}

	gotRecreated, err := sl.Search(userKey)
	if err != nil || string(gotRecreated) != "v3-alice-recreated" {
		t.Fatalf("expected v3-alice-recreated, got %q (err: %v)", gotRecreated, err)
	}

	if sl.Len() != 3 {
		t.Errorf("expected Len() == 3, got %d", sl.Len())
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_OpTypeOrderingForIdenticalSeqNum(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("key:same-seq")
	seq := binary.SeqNum(42)

	// In CompareInternalKey: OpTypeDelete (0x02) > OpTypePut (0x01), so Delete sorts before Put.
	kPut, _ := binary.NewInternalKey(userKey, seq, binary.OpTypePut)
	kDel, _ := binary.NewInternalKey(userKey, seq, binary.OpTypeDelete)

	// Insert Put then Delete
	if err := sl.Insert(kPut, []byte("put-data")); err != nil {
		t.Fatalf("insert Put failed: %v", err)
	}
	if err := sl.Insert(kDel, nil); err != nil {
		t.Fatalf("insert Delete failed: %v", err)
	}

	// Both nodes exist
	if sl.Len() != 2 {
		t.Fatalf("expected Len() == 2, got %d", sl.Len())
	}

	// Delete must sort before Put at level 0
	nodes := sl.NodesAtLevelForTesting(0)
	if nodes[0].KeyForTesting().OpType != binary.OpTypeDelete {
		t.Errorf("expected first node to be OpTypeDelete, got %v", nodes[0].KeyForTesting().OpType)
	}
	if nodes[1].KeyForTesting().OpType != binary.OpTypePut {
		t.Errorf("expected second node to be OpTypePut, got %v", nodes[1].KeyForTesting().OpType)
	}

	// Search returns ErrKeyNotFound because the first matching node is OpTypeDelete
	got, err := sl.Search(userKey)
	if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound, got %v (err: %v)", got, err)
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_ExactDuplicateInternalKey_Idempotent(t *testing.T) {
	sl := memtable.NewSkipList()
	key := sampleKey(t, "user:exact", 100, binary.OpTypePut)

	// First insert
	if err := sl.Insert(key, []byte("val-1")); err != nil {
		t.Fatalf("insert 1 failed: %v", err)
	}
	if sl.Len() != 1 {
		t.Errorf("expected count 1, got %d", sl.Len())
	}

	// Exact duplicate insert with different value (same UserKey, SeqNum, OpType)
	if err := sl.Insert(key, []byte("val-2")); err != nil {
		t.Fatalf("insert duplicate failed: %v", err)
	}

	// Count must remain 1 (no duplicate node created, preserving strict ordering INV-01)
	if sl.Len() != 1 {
		t.Errorf("expected count 1 after duplicate, got %d", sl.Len())
	}

	// Search must return the updated value
	got, err := sl.Search([]byte("user:exact"))
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if string(got) != "val-2" {
		t.Errorf("expected updated value %q, got %q", "val-2", string(got))
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_MemoryOwnershipAndIsolation(t *testing.T) {
	sl := memtable.NewSkipList()

	// 1. Caller mutating key buffer after Insert must not corrupt stored key
	keyBuf := []byte("immutable-key")
	valBuf := []byte("original-val")
	key, _ := binary.NewInternalKey(keyBuf, 1, binary.OpTypePut)

	if err := sl.Insert(key, valBuf); err != nil {
		t.Fatalf("insert failed: %v", err)
	}

	// Mutate external buffers
	keyBuf[0] = 'X'
	valBuf[0] = 'X'

	// Search by original key must succeed and return original value
	got, err := sl.Search([]byte("immutable-key"))
	if err != nil {
		t.Fatalf("search by original key failed after buffer mutation: %v", err)
	}
	if string(got) != "original-val" {
		t.Errorf("stored value was corrupted by caller mutation: %q", string(got))
	}

	// 2. Mutating returned Search value slice must not corrupt stored value
	got[0] = 'Z'
	gotAgain, err := sl.Search([]byte("immutable-key"))
	if err != nil {
		t.Fatalf("second search failed: %v", err)
	}
	if string(gotAgain) != "original-val" {
		t.Errorf("stored value was corrupted by caller mutating returned slice: %q", string(gotAgain))
	}
}

func TestSkipList_FailureAtomicity(t *testing.T) {
	sl := memtable.NewSkipList()
	validKey := sampleKey(t, "valid-key", 10, binary.OpTypePut)
	if err := sl.Insert(validKey, []byte("valid-val")); err != nil {
		t.Fatalf("initial insert failed: %v", err)
	}

	initCount := sl.Len()
	initHeight := sl.Height()

	// 1. Empty key
	err := sl.Insert(binary.InternalKey{UserKey: nil, SeqNum: 1, OpType: binary.OpTypePut}, []byte("v"))
	if err == nil {
		t.Errorf("expected error for empty key")
	}

	// 2. Oversized key
	oversizedKey := make([]byte, binary.MaxKeyLen+1)
	err = sl.Insert(binary.InternalKey{UserKey: oversizedKey, SeqNum: 1, OpType: binary.OpTypePut}, []byte("v"))
	if err == nil {
		t.Errorf("expected error for oversized key")
	}

	// 3. Oversized value
	oversizedVal := make([]byte, binary.MaxValueLen+1)
	err = sl.Insert(sampleKey(t, "k2", 2, binary.OpTypePut), oversizedVal)
	if err == nil {
		t.Errorf("expected error for oversized value")
	}

	// 4. Invalid OpType
	err = sl.Insert(binary.InternalKey{UserKey: []byte("k3"), SeqNum: 3, OpType: binary.OpType(0xFF)}, []byte("v"))
	if err == nil {
		t.Errorf("expected error for invalid opType")
	}

	// 5. Invalid forced height
	err = sl.InsertWithHeightForTesting(sampleKey(t, "k4", 4, binary.OpTypePut), []byte("v"), memtable.MaxHeight+1)
	if err == nil {
		t.Errorf("expected error for height > MaxHeight")
	}

	// State must be 100% unchanged
	if sl.Len() != initCount {
		t.Errorf("Len() changed from %d to %d after failed inserts", initCount, sl.Len())
	}
	if sl.Height() != initHeight {
		t.Errorf("Height() changed from %d to %d after failed inserts", initHeight, sl.Height())
	}

	// Valid key must still be present and searchable
	got, err := sl.Search([]byte("valid-key"))
	if err != nil || string(got) != "valid-val" {
		t.Errorf("original entry corrupted: %q (err: %v)", got, err)
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed after failed inserts: %v", err)
	}
}

func TestSkipList_DeterministicHeightHierarchy(t *testing.T) {
	sl := memtable.NewSkipList()

	// Insert nodes with explicitly controlled heights via test seam
	// Keys: "k1" (height 1), "k2" (height 4), "k3" (height 2), "k4" (height 16)
	items := []struct {
		key    string
		height int
	}{
		{"k1", 1},
		{"k2", 4},
		{"k3", 2},
		{"k4", 16},
	}

	for _, item := range items {
		k := sampleKey(t, item.key, 1, binary.OpTypePut)
		if err := sl.InsertWithHeightForTesting(k, []byte("val-"+item.key), item.height); err != nil {
			t.Fatalf("insert %s with height %d failed: %v", item.key, item.height, err)
		}
	}

	if sl.Height() != 16 {
		t.Errorf("expected active list height 16, got %d", sl.Height())
	}

	// Verify exact node counts per level:
	// Level 0: 4 nodes ("k1", "k2", "k3", "k4")
	// Level 1: 3 nodes ("k2", "k3", "k4")
	// Level 2: 2 nodes ("k2", "k4")
	// Level 3: 2 nodes ("k2", "k4")
	// Levels 4..15: 1 node ("k4")
	expectedPerLevel := []int{
		0: 4, 1: 3, 2: 2, 3: 2,
		4: 1, 5: 1, 6: 1, 7: 1,
		8: 1, 9: 1, 10: 1, 11: 1,
		12: 1, 13: 1, 14: 1, 15: 1,
	}

	for lvl, expCount := range expectedPerLevel {
		count := sl.NodeCountAtLevelForTesting(lvl)
		if count != expCount {
			t.Errorf("level %d: expected %d nodes, got %d", lvl, expCount, count)
		}
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed: %v", err)
	}
}

func TestSkipList_AdversarialSequences(t *testing.T) {
	t.Run("SortedAscending", func(t *testing.T) {
		sl := memtable.NewSkipList()
		for i := 0; i < 200; i++ {
			k := sampleKey(t, fmt.Sprintf("key:%05d", i), 1, binary.OpTypePut)
			if err := sl.Insert(k, []byte(fmt.Sprintf("v-%d", i))); err != nil {
				t.Fatalf("insert %d failed: %v", i, err)
			}
		}
		if err := sl.ValidateStructureForTesting(); err != nil {
			t.Fatalf("sorted insert failed structure check: %v", err)
		}
		for i := 0; i < 200; i++ {
			got, err := sl.Search([]byte(fmt.Sprintf("key:%05d", i)))
			if err != nil || string(got) != fmt.Sprintf("v-%d", i) {
				t.Fatalf("search %d failed: %v", i, err)
			}
		}
	})

	t.Run("SortedDescending", func(t *testing.T) {
		sl := memtable.NewSkipList()
		for i := 199; i >= 0; i-- {
			k := sampleKey(t, fmt.Sprintf("key:%05d", i), 1, binary.OpTypePut)
			if err := sl.Insert(k, []byte(fmt.Sprintf("v-%d", i))); err != nil {
				t.Fatalf("insert %d failed: %v", i, err)
			}
		}
		if err := sl.ValidateStructureForTesting(); err != nil {
			t.Fatalf("reverse-sorted insert failed structure check: %v", err)
		}
		for i := 0; i < 200; i++ {
			got, err := sl.Search([]byte(fmt.Sprintf("key:%05d", i)))
			if err != nil || string(got) != fmt.Sprintf("v-%d", i) {
				t.Fatalf("search %d failed: %v", i, err)
			}
		}
	})

	t.Run("AlternatingExtremeKeys", func(t *testing.T) {
		sl := memtable.NewSkipList()
		for i := 0; i < 100; i++ {
			minKey := sampleKey(t, fmt.Sprintf("a:%04d", i), 1, binary.OpTypePut)
			maxKey := sampleKey(t, fmt.Sprintf("z:%04d", i), 1, binary.OpTypePut)
			if err := sl.Insert(minKey, []byte("min")); err != nil {
				t.Fatalf("insert min failed: %v", err)
			}
			if err := sl.Insert(maxKey, []byte("max")); err != nil {
				t.Fatalf("insert max failed: %v", err)
			}
		}
		if err := sl.ValidateStructureForTesting(); err != nil {
			t.Fatalf("alternating keys structure check failed: %v", err)
		}
	})

	t.Run("LongCommonPrefixes", func(t *testing.T) {
		sl := memtable.NewSkipList()
		longPrefix := bytes.Repeat([]byte("prefix.component.service.subsystem."), 10)
		for i := 0; i < 100; i++ {
			kBytes := append(longPrefix, []byte(fmt.Sprintf("%04d", i))...)
			k, _ := binary.NewInternalKey(kBytes, 1, binary.OpTypePut)
			if err := sl.Insert(k, []byte("val")); err != nil {
				t.Fatalf("insert prefix %d failed: %v", i, err)
			}
		}
		if err := sl.ValidateStructureForTesting(); err != nil {
			t.Fatalf("long prefix structure check failed: %v", err)
		}
	})
}

func TestSkipList_Boundaries(t *testing.T) {
	sl := memtable.NewSkipList()

	// Min valid key: 1 byte
	minK, _ := binary.NewInternalKey([]byte{0x01}, 0, binary.OpTypePut)
	if err := sl.Insert(minK, []byte("min-val")); err != nil {
		t.Fatalf("insert min key failed: %v", err)
	}

	// Max valid key: MaxKeyLen (65535 bytes)
	maxKeyBytes := bytes.Repeat([]byte("K"), binary.MaxKeyLen)
	maxK, err := binary.NewInternalKey(maxKeyBytes, math.MaxUint64, binary.OpTypePut)
	if err != nil {
		t.Fatalf("failed to create max key: %v", err)
	}
	if err := sl.Insert(maxK, []byte("max-val")); err != nil {
		t.Fatalf("insert max key failed: %v", err)
	}

	// Search min and max keys
	gotMin, err := sl.Search([]byte{0x01})
	if err != nil || string(gotMin) != "min-val" {
		t.Errorf("search min key failed: %v", err)
	}
	gotMax, err := sl.Search(maxKeyBytes)
	if err != nil || string(gotMax) != "max-val" {
		t.Errorf("search max key failed: %v", err)
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("boundary validation failed: %v", err)
	}
}

func TestSkipList_SearchEdgeCases(t *testing.T) {
	sl := memtable.NewSkipList()
	keys := []string{"m", "o"}
	for _, k := range keys {
		ikey := sampleKey(t, k, 1, binary.OpTypePut)
		if err := sl.Insert(ikey, []byte("val-"+k)); err != nil {
			t.Fatalf("insert %s failed: %v", k, err)
		}
	}

	// Key smaller than minimum ("a" < "m")
	got, err := sl.Search([]byte("a"))
	if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for key smaller than min, got %v (err: %v)", got, err)
	}

	// Key greater than maximum ("z" > "o")
	got, err = sl.Search([]byte("z"))
	if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for key greater than max, got %v (err: %v)", got, err)
	}

	// Key between existing keys ("n" between "m" and "o")
	got, err = sl.Search([]byte("n"))
	if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for key in gap, got %v (err: %v)", got, err)
	}
}

func TestSkipList_SentinelNeverParticipates(t *testing.T) {
	sl := memtable.NewSkipList()
	head := sl.HeadForTesting()
	if head == nil {
		t.Fatalf("head is nil")
	}
	if head.HeightForTesting() != memtable.MaxHeight {
		t.Errorf("head height %d != MaxHeight %d", head.HeightForTesting(), memtable.MaxHeight)
	}

	// Search for empty key returns validation error, never matches sentinel
	got, err := sl.Search(nil)
	if got != nil || err == nil {
		t.Errorf("expected validation error searching nil key, got %v (err: %v)", got, err)
	}
}

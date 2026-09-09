package memtable_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// TestSkipList_Invariant_01_StrictLevelOrdering verifies P03-S01-M02-INV-01:
// For every level, keys encountered through forward pointers are strictly ordered according to CompareInternalKey.
func TestSkipList_Invariant_01_StrictLevelOrdering(t *testing.T) {
	sl := memtable.NewSkipList()
	keys := []string{"orange", "apple", "banana", "grape", "mango", "peach", "plum"}
	for i, k := range keys {
		ikey, _ := binary.NewInternalKey([]byte(k), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(ikey, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	for lvl := 0; lvl < sl.Height(); lvl++ {
		nodes := sl.NodesAtLevelForTesting(lvl)
		for i := 0; i < len(nodes)-1; i++ {
			cmp := binary.CompareInternalKey(nodes[i].KeyForTesting(), nodes[i+1].KeyForTesting())
			if cmp >= 0 {
				t.Errorf("level %d violation: key %v >= next %v (cmp=%d)",
					lvl, nodes[i].KeyForTesting(), nodes[i+1].KeyForTesting(), cmp)
			}
		}
	}
}

// TestSkipList_Invariant_02_TowerHeightSufficiency verifies P03-S01-M02-INV-02:
// Every node reachable at level L has a tower height >= L+1.
func TestSkipList_Invariant_02_TowerHeightSufficiency(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 50; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("user:%03d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	for lvl := 0; lvl < sl.Height(); lvl++ {
		nodes := sl.NodesAtLevelForTesting(lvl)
		for _, n := range nodes {
			if n.HeightForTesting() < lvl+1 {
				t.Errorf("node %v at level %d has height %d < %d",
					n.KeyForTesting(), lvl, n.HeightForTesting(), lvl+1)
			}
		}
	}
}

// TestSkipList_Invariant_03_Acyclicity verifies P03-S01-M02-INV-03:
// No forward-pointer traversal contains a cycle.
func TestSkipList_Invariant_03_Acyclicity(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 100; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("item:%04d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	// Structural validator explicitly verifies acyclicity across all levels
	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Fatalf("cycle detected or structure validation failed: %v", err)
	}
}

// TestSkipList_Invariant_04_LowerLevelReachability verifies P03-S01-M02-INV-04:
// Every node reachable from the head at level L is reachable through a valid lower-level path.
func TestSkipList_Invariant_04_LowerLevelReachability(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 100; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("k:%03d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	l0Nodes := make(map[*memtable.SkipListNode]bool)
	for _, n := range sl.NodesAtLevelForTesting(0) {
		l0Nodes[n] = true
	}

	for lvl := 1; lvl < sl.Height(); lvl++ {
		for _, n := range sl.NodesAtLevelForTesting(lvl) {
			if !l0Nodes[n] {
				t.Errorf("node %v at level %d is not reachable at level 0", n.KeyForTesting(), lvl)
			}
		}
	}
}

// TestSkipList_Invariant_05_LevelSpan verifies P03-S01-M02-INV-05:
// A node appears at all levels from 0 through height-1 and never appears at a higher level.
func TestSkipList_Invariant_05_LevelSpan(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 100; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("k:%03d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	allNodes := sl.NodesAtLevelForTesting(0)
	for _, n := range allNodes {
		h := n.HeightForTesting()
		for lvl := 0; lvl < memtable.MaxHeight; lvl++ {
			nodesAtLvl := sl.NodesAtLevelForTesting(lvl)
			contains := false
			for _, item := range nodesAtLvl {
				if item == n {
					contains = true
					break
				}
			}
			if lvl < h && !contains {
				t.Errorf("node %v of height %d missing at level %d", n.KeyForTesting(), h, lvl)
			}
			if lvl >= h && contains {
				t.Errorf("node %v of height %d unexpectedly present at level %d", n.KeyForTesting(), h, lvl)
			}
		}
	}
}

// TestSkipList_Invariant_06_InsertionPreservesEntries verifies P03-S01-M02-INV-06:
// Insertion preserves all existing entries unless duplicate handling is explicitly defined.
func TestSkipList_Invariant_06_InsertionPreservesEntries(t *testing.T) {
	sl := memtable.NewSkipList()
	type entry struct {
		key binary.InternalKey
		val []byte
	}
	var inserted []entry

	for i := 0; i < 100; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("k:%03d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		val := []byte(fmt.Sprintf("val-%d", i))
		if err := sl.Insert(k, val); err != nil {
			t.Fatalf("insert %d failed: %v", i, err)
		}
		inserted = append(inserted, entry{key: k, val: val})

		// After each insert, verify every previously inserted entry remains searchable
		for _, prev := range inserted {
			got, err := sl.Search(prev.key.UserKey)
			if err != nil {
				t.Errorf("previously inserted key %s missing after inserting %d: %v", prev.key.UserKey, i, err)
			}
			if !bytes.Equal(got, prev.val) {
				t.Errorf("key %s value altered: got %s, want %s", prev.key.UserKey, got, prev.val)
			}
		}
	}
}

// TestSkipList_Invariant_07_SearchReturnsNewestVersion verifies P03-S01-M02-INV-07:
// Search returns the newest matching version according to descending sequence order.
func TestSkipList_Invariant_07_SearchReturnsNewestVersion(t *testing.T) {
	sl := memtable.NewSkipList()
	userKey := []byte("multi-version-key")

	for seq := uint64(1); seq <= 20; seq++ {
		k, _ := binary.NewInternalKey(userKey, binary.SeqNum(seq), binary.OpTypePut)
		val := []byte(fmt.Sprintf("v-%d", seq))
		if err := sl.Insert(k, val); err != nil {
			t.Fatalf("insert seq %d failed: %v", seq, err)
		}

		// Search immediately after each revision must return the newest revision (seq)
		got, err := sl.Search(userKey)
		if err != nil {
			t.Fatalf("search failed for seq %d: %v", seq, err)
		}
		expected := fmt.Sprintf("v-%d", seq)
		if string(got) != expected {
			t.Errorf("expected newest version %q, got %q", expected, string(got))
		}
	}
}

// TestSkipList_Invariant_08_SearchNonexistentDoesNotMutate verifies P03-S01-M02-INV-08:
// Searching for a nonexistent UserKey returns the correct not-found result without mutating the structure.
func TestSkipList_Invariant_08_SearchNonexistentDoesNotMutate(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 50; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("k:%03d", i*2)), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	beforeCount := sl.Len()
	beforeHeight := sl.Height()

	// Search for nonexistent odd keys
	for i := 0; i < 50; i++ {
		got, err := sl.Search([]byte(fmt.Sprintf("k:%03d", i*2+1)))
		if got != nil || !stdErrors.Is(err, errors.ErrKeyNotFound) {
			t.Errorf("expected ErrKeyNotFound for odd key %d, got %v (err: %v)", i*2+1, got, err)
		}
	}

	if sl.Len() != beforeCount {
		t.Errorf("Len() changed from %d to %d after nonexistent searches", beforeCount, sl.Len())
	}
	if sl.Height() != beforeHeight {
		t.Errorf("Height() changed from %d to %d after nonexistent searches", beforeHeight, sl.Height())
	}

	if err := sl.ValidateStructureForTesting(); err != nil {
		t.Errorf("structural validation failed after read-only searches: %v", err)
	}
}

// TestSkipList_Invariant_09_ForwardTraversalTerminatesAtNil verifies P03-S01-M02-INV-09:
// Forward traversal terminates at nil across all levels.
func TestSkipList_Invariant_09_ForwardTraversalTerminatesAtNil(t *testing.T) {
	sl := memtable.NewSkipList()
	for i := 0; i < 30; i++ {
		k, _ := binary.NewInternalKey([]byte(fmt.Sprintf("node:%02d", i)), binary.SeqNum(i+1), binary.OpTypePut)
		if err := sl.Insert(k, []byte("val")); err != nil {
			t.Fatalf("insert failed: %v", err)
		}
	}

	for lvl := 0; lvl < memtable.MaxHeight; lvl++ {
		curr, err := sl.HeadForTesting().ForwardAtForTesting(lvl)
		if err != nil {
			t.Fatalf("failed reading forward pointer at level %d: %v", lvl, err)
		}
		steps := 0
		for curr != nil {
			steps++
			if steps > 1000 {
				t.Fatalf("infinite loop at level %d: traversal failed to terminate at nil", lvl)
			}
			curr, _ = curr.ForwardAtForTesting(lvl)
		}
		// Traversal terminated at nil cleanly
	}
}

// TestSkipList_Invariant_10_StructureValidAfterArbitraryInserts verifies P03-S01-M02-INV-10:
// The SkipList remains structurally valid after arbitrary sequences of sequential insertions.
func TestSkipList_Invariant_10_StructureValidAfterArbitraryInserts(t *testing.T) {
	sl := memtable.NewSkipList()
	rng := memtable.NewPCG32(0xCAFE, 0xBEEF)

	for i := 0; i < 300; i++ {
		keyNum := rng.Uint32() % 50 // Collisions create multiple versions
		k, err := binary.NewInternalKey([]byte(fmt.Sprintf("key:%04d", keyNum)), binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			t.Fatalf("failed to create key: %v", err)
		}
		if err := sl.Insert(k, []byte(fmt.Sprintf("v-%d", i))); err != nil {
			t.Fatalf("insert %d failed: %v", i, err)
		}
		if (i+1)%50 == 0 {
			if err := sl.ValidateStructureForTesting(); err != nil {
				t.Fatalf("structural violation after %d inserts: %v", i+1, err)
			}
		}
	}
}

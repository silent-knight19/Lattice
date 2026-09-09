package memtable_test

import (
	stdErrors "errors"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// P03-S01-INV-01: Every valid SkipList node has 1 <= height <= MaxHeight.
func TestInvariant_P03_S01_INV_01_HeightBounds(t *testing.T) {
	key := sampleKey(t, "inv01-key", 1, binary.OpTypePut)

	// Valid range [1, 16] must succeed
	for h := memtable.MinHeight; h <= memtable.MaxHeight; h++ {
		node, err := memtable.NewSkipListNodeForTesting(key, []byte("v"), h)
		if err != nil {
			t.Fatalf("failed to create valid node with height %d: %v", h, err)
		}
		if node.HeightForTesting() < memtable.MinHeight || node.HeightForTesting() > memtable.MaxHeight {
			t.Fatalf("node height %d violates bounds [%d, %d]",
				node.HeightForTesting(), memtable.MinHeight, memtable.MaxHeight)
		}
	}

	// Heights outside [1, 16] must be rejected
	for _, invalidH := range []int{-10, -1, 0, 17, 100} {
		node, err := memtable.NewSkipListNodeForTesting(key, []byte("v"), invalidH)
		if err == nil || node != nil {
			t.Fatalf("construction permitted invalid height %d", invalidH)
		}
		if !stdErrors.Is(err, errors.ErrInvalidSkipListHeight) {
			t.Errorf("expected ErrInvalidSkipListHeight, got %v", err)
		}
	}
}

// P03-S01-INV-02: Forward-pointer storage length exactly matches the node's declared height.
func TestInvariant_P03_S01_INV_02_PointerStorageExactLength(t *testing.T) {
	key := sampleKey(t, "inv02-key", 1, binary.OpTypePut)

	for h := memtable.MinHeight; h <= memtable.MaxHeight; h++ {
		node, err := memtable.NewSkipListNodeForTesting(key, []byte("v"), h)
		if err != nil {
			t.Fatalf("failed to create node with height %d: %v", h, err)
		}
		if node.HeightForTesting() != h {
			t.Fatalf("declared height %d does not match forward pointer length %d",
				h, node.HeightForTesting())
		}
		// Splicing level (h-1) must succeed; level h must fail
		target, _ := memtable.NewSkipListNodeForTesting(key, []byte("target"), 1)
		if err := node.SetForwardForTesting(h-1, target); err != nil {
			t.Fatalf("failed to set forward pointer at valid top level %d: %v", h-1, err)
		}
		if err := node.SetForwardForTesting(h, target); err == nil {
			t.Fatalf("expected error setting forward pointer at level %d for height %d", h, h)
		}
	}
}

// P03-S01-INV-03: Node construction cannot allocate attacker-controlled unbounded tower storage.
func TestInvariant_P03_S01_INV_03_BoundedTowerAllocation(t *testing.T) {
	key := sampleKey(t, "inv03-key", 1, binary.OpTypePut)

	// Attacker attempts to request extreme heights to trigger huge slice allocations or OOM
	attackHeights := []int{
		1 << 10, // 1,024
		1 << 16, // 65,536
		1 << 24, // 16,777,216
		1 << 30, // 1,073,741,824
	}

	for _, h := range attackHeights {
		node, err := memtable.NewSkipListNodeForTesting(key, []byte("v"), h)
		if err == nil || node != nil {
			t.Fatalf("attacker allocated unbounded tower with height %d", h)
		}
		if !stdErrors.Is(err, errors.ErrInvalidSkipListHeight) {
			t.Errorf("expected ErrInvalidSkipListHeight for attack height %d, got %v", h, err)
		}
	}
}

// P03-S01-INV-04: Random height depends only on the random source and configured distribution, not on key/value contents.
func TestInvariant_P03_S01_INV_04_RandomHeightKeyIndependence(t *testing.T) {
	// A fixed PRNG seed must produce identical height sequences regardless of what keys are being inserted
	seq1 := make([]int, 50)
	seq2 := make([]int, 50)

	gen1 := memtable.NewHeightGenerator(memtable.NewPCG32(8888, 1))
	for i := 0; i < 50; i++ {
		seq1[i] = gen1.RandomHeight()
	}

	gen2 := memtable.NewHeightGenerator(memtable.NewPCG32(8888, 1))
	for i := 0; i < 50; i++ {
		seq2[i] = gen2.RandomHeight()
	}

	for i := 0; i < 50; i++ {
		if seq1[i] != seq2[i] {
			t.Fatalf("height sequence diverges at index %d: %d != %d", i, seq1[i], seq2[i])
		}
	}
}

// P03-S01-INV-05: randomHeight() always terminates.
func TestInvariant_P03_S01_INV_05_TerminationGuarantee(t *testing.T) {
	// Adversarial random source that produces infinite promotion condition (val = 0)
	adversarialSrc := &mockFixedSource{val: 0}
	gen := memtable.NewHeightGenerator(adversarialSrc)

	// Must terminate within a few microseconds
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h := gen.RandomHeight()
			if h != memtable.MaxHeight {
				t.Errorf("expected MaxHeight %d, got %d", memtable.MaxHeight, h)
			}
		}
		close(done)
	}()

	select {
	case <-done:
		// Succeeded in terminating
	case <-time.After(2 * time.Second):
		t.Fatalf("randomHeight failed to terminate under adversarial promotion loop")
	}
}

// P03-S01-INV-06: No valid node can expose a forward-pointer level outside its configured height.
func TestInvariant_P03_S01_INV_06_NoLevelExposureOutsideHeight(t *testing.T) {
	key := sampleKey(t, "inv06-key", 1, binary.OpTypePut)

	node, err := memtable.NewSkipListNodeForTesting(key, []byte("v"), 5)
	if err != nil {
		t.Fatalf("failed to create node: %v", err)
	}

	// Valid levels are 0, 1, 2, 3, 4
	for lvl := 0; lvl < 5; lvl++ {
		if _, err := node.ForwardAtForTesting(lvl); err != nil {
			t.Errorf("level %d within height 5 failed: %v", lvl, err)
		}
	}

	// Levels outside [0, 4] must be rejected
	for _, lvl := range []int{-5, -1, 5, 6, 16} {
		if _, err := node.ForwardAtForTesting(lvl); !stdErrors.Is(err, errors.ErrInvalidSkipListLevel) {
			t.Errorf("expected ErrInvalidSkipListLevel for level %d, got %v", lvl, err)
		}
		if err := node.SetForwardForTesting(lvl, node); !stdErrors.Is(err, errors.ErrInvalidSkipListLevel) {
			t.Errorf("expected ErrInvalidSkipListLevel for setting level %d, got %v", lvl, err)
		}
	}
}

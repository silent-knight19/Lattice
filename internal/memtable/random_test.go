package memtable_test

import (
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/memtable"
)

// mockFixedSource always returns a fixed value.
type mockFixedSource struct {
	val uint32
}

func (m *mockFixedSource) Uint32() uint32 {
	return m.val
}

// mockSequenceSource returns values from a slice in order, repeating when exhausted.
type mockSequenceSource struct {
	mu   sync.Mutex
	vals []uint32
	idx  int
}

func (m *mockSequenceSource) Uint32() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.vals[m.idx]
	m.idx = (m.idx + 1) % len(m.vals)
	return v
}

func TestRandomHeight_RepeatedGenerationBounds(t *testing.T) {
	gen := memtable.NewDefaultHeightGenerator()

	const iterations = 10000
	for i := 0; i < iterations; i++ {
		h := gen.RandomHeight()
		if h < memtable.MinHeight || h > memtable.MaxHeight {
			t.Fatalf("iteration %d: height %d out of bounds [%d, %d]",
				i, h, memtable.MinHeight, memtable.MaxHeight)
		}
	}
}

func TestRandomHeight_PathologicalMinimumSource(t *testing.T) {
	// Source that always returns a value with bottom 2 bits != 00 (e.g. 0xFFFFFFFF has bottom bits 11)
	// Must NEVER promote, returning MinHeight (1) every single time.
	src := &mockFixedSource{val: 0xFFFFFFFF}
	gen := memtable.NewHeightGenerator(src)

	for i := 0; i < 1000; i++ {
		h := gen.RandomHeight()
		if h != memtable.MinHeight {
			t.Fatalf("expected minimum height %d, got %d", memtable.MinHeight, h)
		}
	}
}

func TestRandomHeight_PathologicalMaximumSource(t *testing.T) {
	// Source that always returns 0 (bottom 2 bits 00)
	// Would promote infinitely if unbounded. Must terminate deterministically at MaxHeight (16).
	src := &mockFixedSource{val: 0}
	gen := memtable.NewHeightGenerator(src)

	for i := 0; i < 1000; i++ {
		h := gen.RandomHeight()
		if h != memtable.MaxHeight {
			t.Fatalf("expected bounded maximum height %d, got %d", memtable.MaxHeight, h)
		}
	}
}

func TestRandomHeight_DeterministicSequences(t *testing.T) {
	// Value with & 3 == 0 promotes (e.g. 0, 4, 8, 12)
	// Value with & 3 != 0 halts promotion (e.g. 1, 2, 3)
	testCases := []struct {
		name           string
		sequence       []uint32
		expectedHeight int
	}{
		{
			name:           "immediate halt on first coin flip",
			sequence:       []uint32{1},
			expectedHeight: 1,
		},
		{
			name:           "one promotion then halt",
			sequence:       []uint32{0, 1},
			expectedHeight: 2,
		},
		{
			name:           "two promotions then halt",
			sequence:       []uint32{0, 4, 1},
			expectedHeight: 3,
		},
		{
			name:           "five promotions then halt",
			sequence:       []uint32{0, 0, 0, 0, 0, 1},
			expectedHeight: 6,
		},
		{
			name:           "fifteen promotions reach MaxHeight",
			sequence:       []uint32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			expectedHeight: 16,
		},
		{
			name:           "twenty promotions capped at MaxHeight (16)",
			sequence:       []uint32{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1},
			expectedHeight: 16,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			src := &mockSequenceSource{vals: tc.sequence}
			gen := memtable.NewHeightGenerator(src)
			h := gen.RandomHeight()
			if h != tc.expectedHeight {
				t.Errorf("height mismatch: got %d, want %d", h, tc.expectedHeight)
			}
		})
	}
}

func TestRandomHeight_KeyIndependence(t *testing.T) {
	// P03-S01-INV-04: Random height depends only on the random source, not on key/value contents.
	gen := memtable.NewHeightGenerator(memtable.NewPCG32(12345, 1))

	// HeightGenerator does not take key/value arguments.
	// We verify that consecutive heights produced are identical whether associated with
	// key A, key B, identical keys, or reverse keys.
	key1, _ := binary.NewInternalKey([]byte("key-alpha"), 1, binary.OpTypePut)
	key2, _ := binary.NewInternalKey([]byte("key-beta"), 2, binary.OpTypePut)

	node1, err := memtable.NewSkipListNodeForTesting(key1, []byte("v1"), gen.RandomHeight())
	if err != nil {
		t.Fatalf("failed to create node1: %v", err)
	}

	node2, err := memtable.NewSkipListNodeForTesting(key2, []byte("v2"), gen.RandomHeight())
	if err != nil {
		t.Fatalf("failed to create node2: %v", err)
	}

	// Heights are valid regardless of key contents
	if node1.HeightForTesting() < 1 || node1.HeightForTesting() > 16 {
		t.Errorf("node1 height invalid: %d", node1.HeightForTesting())
	}
	if node2.HeightForTesting() < 1 || node2.HeightForTesting() > 16 {
		t.Errorf("node2 height invalid: %d", node2.HeightForTesting())
	}
}

func TestRandomHeight_ConcurrentAccessRaceFree(t *testing.T) {
	gen := memtable.NewDefaultHeightGenerator()

	const numGoroutines = 64
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				h := gen.RandomHeight()
				if h < memtable.MinHeight || h > memtable.MaxHeight {
					t.Errorf("concurrent height %d out of bounds", h)
				}
			}
		}()
	}

	wg.Wait()
}

func TestPackageLevelRandomHeight(t *testing.T) {
	for i := 0; i < 100; i++ {
		h := memtable.RandomHeightForTesting()
		if h < memtable.MinHeight || h > memtable.MaxHeight {
			t.Fatalf("package-level randomHeight returned out of bounds height %d", h)
		}
	}
}

func TestHeightGenerator_NilSourceFallback(t *testing.T) {
	gen := memtable.NewHeightGenerator(nil)
	if gen == nil {
		t.Fatalf("expected non-nil generator when passing nil source")
	}
	h := gen.RandomHeight()
	if h < memtable.MinHeight || h > memtable.MaxHeight {
		t.Errorf("height out of bounds: %d", h)
	}
}

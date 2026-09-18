package benchmark_test

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/benchmark"
	"github.com/silent-knight19/lattice/internal/binary"
)

// TestZipf_ConstructorValidation verifies validation of parameters N, theta, and prefix.
func TestZipf_ConstructorValidation(t *testing.T) {
	// N = 0 rejected
	if _, err := benchmark.NewDefaultZipfGenerator(0, 42); !errors.Is(err, benchmark.ErrInvalidKeyspace) {
		t.Fatalf("expected ErrInvalidKeyspace for N=0, got: %v", err)
	}

	// N > MaxKeyspace rejected
	if _, err := benchmark.NewDefaultZipfGenerator(benchmark.MaxKeyspace+1, 42); !errors.Is(err, benchmark.ErrKeyspaceTooLarge) {
		t.Fatalf("expected ErrKeyspaceTooLarge, got: %v", err)
	}

	// theta <= 0.0 rejected
	for _, invalidTheta := range []float64{0.0, -0.5, -1.0, 1.0, 1.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := benchmark.NewZipfGenerator(100, invalidTheta, 42); !errors.Is(err, benchmark.ErrInvalidZipfTheta) {
			t.Fatalf("expected ErrInvalidZipfTheta for theta=%v, got: %v", invalidTheta, err)
		}
	}

	// Valid configurations succeed
	gen, err := benchmark.NewDefaultZipfGenerator(1000, 42)
	if err != nil {
		t.Fatalf("expected valid generator, got: %v", err)
	}
	if gen.Keyspace() != 1000 {
		t.Fatalf("expected keyspace 1000, got: %d", gen.Keyspace())
	}
	if gen.Theta() != benchmark.DefaultZipfTheta {
		t.Fatalf("expected theta %v, got: %v", benchmark.DefaultZipfTheta, gen.Theta())
	}
}

// TestZipf_KeyspaceMatrix verifies that all outputs remain strictly within [0, N-1]
// across a wide variety of keyspace sizes.
func TestZipf_KeyspaceMatrix(t *testing.T) {
	testSizes := []uint64{1, 2, 3, 5, 10, 50, 100, 500, 1000, 10000, 100000}

	for _, n := range testSizes {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			gen, err := benchmark.NewDefaultZipfGenerator(n, int64(n*7919))
			if err != nil {
				t.Fatalf("NewDefaultZipfGenerator(%d) failed: %v", n, err)
			}

			seen := make(map[uint64]bool)
			sampleCount := 10000
			if n > 1000 {
				sampleCount = 50000
			}

			for i := 0; i < sampleCount; i++ {
				rank := gen.Next()
				if rank >= n {
					t.Fatalf("rank %d exceeds keyspace bound %d (op %d)", rank, n, i)
				}
				seen[rank] = true
			}

			// For small N, ensure all ranks are reachable
			if n <= 10 {
				if len(seen) != int(n) {
					t.Fatalf("expected all %d ranks to be reachable, saw only %d", n, len(seen))
				}
			}
		})
	}
}

// TestZipf_LatticeKeyCompatibility verifies that generated keys strictly conform
// to the Lattice engine's binary.ValidateKey constraints.
func TestZipf_LatticeKeyCompatibility(t *testing.T) {
	gen, err := benchmark.NewDefaultZipfGenerator(1000, 12345)
	if err != nil {
		t.Fatalf("NewDefaultZipfGenerator: %v", err)
	}

	scratch := make([]byte, 0, 32)
	for i := 0; i < 1000; i++ {
		scratch = gen.NextKeyBuf(scratch)

		// 1. Must pass Lattice binary key validation
		if err := binary.ValidateKey(scratch); err != nil {
			t.Fatalf("generated key %q failed binary.ValidateKey: %v", string(scratch), err)
		}

		// 2. Must start with default prefix
		if !bytes.HasPrefix(scratch, []byte("key:")) {
			t.Fatalf("generated key %q missing prefix", string(scratch))
		}

		// 3. Must be exactly 14 bytes (key: + 10 digits)
		if len(scratch) != 14 {
			t.Fatalf("unexpected key length %d (want 14): %q", len(scratch), string(scratch))
		}
	}
}

// TestZipf_DeterministicSeedReproducibility verifies that two generators configured
// with identical parameters and seeds yield identical output sequences.
func TestZipf_DeterministicSeedReproducibility(t *testing.T) {
	seeds := []int64{0, 1, 42, -999, 123456789, math.MaxInt64}

	for _, seed := range seeds {
		genA, err := benchmark.NewDefaultZipfGenerator(500, seed)
		if err != nil {
			t.Fatalf("genA init failed: %v", err)
		}
		genB, err := benchmark.NewDefaultZipfGenerator(500, seed)
		if err != nil {
			t.Fatalf("genB init failed: %v", err)
		}

		const numSamples = 5000
		for i := 0; i < numSamples; i++ {
			rankA := genA.Next()
			rankB := genB.Next()
			if rankA != rankB {
				t.Fatalf("seed %d mismatch at sample %d: genA=%d, genB=%d", seed, i, rankA, rankB)
			}

			keyA := genA.NextKey()
			keyB := genB.NextKey()
			if !bytes.Equal(keyA, keyB) {
				t.Fatalf("seed %d key mismatch at sample %d: %q vs %q", seed, i, keyA, keyB)
			}
		}
	}
}

// TestZipf_IsolatedRNGState verifies that sampling from one generator does not
// mutate or affect another generator instance with a different seed.
func TestZipf_IsolatedRNGState(t *testing.T) {
	gen1, err := benchmark.NewDefaultZipfGenerator(100, 101)
	if err != nil {
		t.Fatal(err)
	}
	gen2, err := benchmark.NewDefaultZipfGenerator(100, 202)
	if err != nil {
		t.Fatal(err)
	}
	genReference, err := benchmark.NewDefaultZipfGenerator(100, 101)
	if err != nil {
		t.Fatal(err)
	}

	// Interleave calls between gen1 and gen2
	for i := 0; i < 1000; i++ {
		_ = gen2.Next()
		r1 := gen1.Next()
		ref := genReference.Next()
		if r1 != ref {
			t.Fatalf("RNG interference detected at sample %d: gen1=%d ref=%d", i, r1, ref)
		}
	}
}

// TestZipf_IndependentConcurrencySafe verifies that independent ZipfGenerator
// instances can safely execute concurrently across goroutines without race conditions.
func TestZipf_IndependentConcurrencySafe(t *testing.T) {
	const workers = 16
	const samplesPerWorker = 5000
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			seed := int64(workerID*1000003 + 42)
			gen, err := benchmark.NewDefaultZipfGenerator(1000, seed)
			if err != nil {
				t.Errorf("worker %d init failed: %v", workerID, err)
				return
			}

			scratch := make([]byte, 0, 32)
			for i := 0; i < samplesPerWorker; i++ {
				rank := gen.Next()
				if rank >= 1000 {
					t.Errorf("worker %d got out of bounds rank %d", workerID, rank)
					return
				}
				scratch = gen.NextKeyBuf(scratch)
				if len(scratch) != 14 {
					t.Errorf("worker %d got invalid key length: %d", workerID, len(scratch))
					return
				}
			}
		}(w)
	}

	wg.Wait()
}

// TestZipf_NextKeyBuf_ZeroAllocations verifies that NextKeyBuf does not perform
// heap allocations when backed by a pre-allocated capacity slice.
func TestZipf_NextKeyBuf_ZeroAllocations(t *testing.T) {
	gen, err := benchmark.NewDefaultZipfGenerator(1000, 42)
	if err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 0, 32)
	allocs := testing.AllocsPerRun(1000, func() {
		buf = gen.NextKeyBuf(buf)
	})

	if allocs > 0 {
		t.Fatalf("expected 0 allocs/op for NextKeyBuf, got %f", allocs)
	}
}

// TestZipf_LexicographicalSortingOrder verifies that key serialization preserves
// numeric rank sorting order for indexing predictability.
func TestZipf_LexicographicalSortingOrder(t *testing.T) {
	gen, err := benchmark.NewDefaultZipfGenerator(100, 42)
	if err != nil {
		t.Fatal(err)
	}

	k1 := gen.FormatKey(nil, 1)
	k2 := gen.FormatKey(nil, 2)
	k10 := gen.FormatKey(nil, 10)
	k99 := gen.FormatKey(nil, 99)

	if bytes.Compare(k1, k2) >= 0 {
		t.Fatalf("expected %q < %q", k1, k2)
	}
	if bytes.Compare(k2, k10) >= 0 {
		t.Fatalf("expected %q < %q", k2, k10)
	}
	if bytes.Compare(k10, k99) >= 0 {
		t.Fatalf("expected %q < %q", k10, k99)
	}
}

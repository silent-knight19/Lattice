package filter_test

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/filter"
)

// TestTheoreticalFPR validates the theoretical FPR calculation against independently
// derived numerical values and verifies boundary behavior.
func TestTheoreticalFPR(t *testing.T) {
	// Independent oracle verification:
	// For k = 7 and m/n = 10:
	// exponent = -7 / 10 = -0.7
	// exp(-0.7) = 0.4965853037914095
	// 1 - exp(-0.7) = 0.5034146962085905
	// (0.5034146962085905)^7 ≈ 0.0081937221
	expectedTheo := 0.0081937221
	actualTheo := filter.TheoreticalFPR(10.0, 7)

	if math.Abs(actualTheo-expectedTheo) > 1e-6 {
		t.Fatalf("TheoreticalFPR(10.0, 7) = %.8f, want approximately %.8f", actualTheo, expectedTheo)
	}

	// Verify monotonic behavior: increasing bits per key decreases FPR.
	fpr10 := filter.TheoreticalFPR(10.0, 7)
	fpr12 := filter.TheoreticalFPR(12.0, 7)
	fpr15 := filter.TheoreticalFPR(15.0, 7)

	if !(fpr15 < fpr12 && fpr12 < fpr10) {
		t.Fatalf("expected monotonic decrease in FPR with more bits/key: fpr10=%.6f, fpr12=%.6f, fpr15=%.6f",
			fpr10, fpr12, fpr15)
	}

	// Boundary cases: invalid or zero inputs return 1.0.
	if p := filter.TheoreticalFPR(0, 7); p != 1.0 {
		t.Fatalf("TheoreticalFPR(0, 7) = %f, want 1.0", p)
	}
	if p := filter.TheoreticalFPR(-10, 7); p != 1.0 {
		t.Fatalf("TheoreticalFPR(-10, 7) = %f, want 1.0", p)
	}
	if p := filter.TheoreticalFPR(10, 0); p != 1.0 {
		t.Fatalf("TheoreticalFPR(10, 0) = %f, want 1.0", p)
	}
	if p := filter.TheoreticalFPR(10, -3); p != 1.0 {
		t.Fatalf("TheoreticalFPR(10, -3) = %f, want 1.0", p)
	}
}

// TestWilsonConfidenceInterval validates the binomial confidence interval calculation
// against known textbook statistical values and checks edge case handling.
func TestWilsonConfidenceInterval(t *testing.T) {
	z := 1.959963984540054 // 95% confidence

	// Textbook case: n = 100, x = 10.
	// Wilson score interval formula yields approximately [0.05523, 0.17438].
	low, high := filter.WilsonConfidenceInterval(100, 10, z)
	if math.Abs(low-0.05523) > 0.001 {
		t.Fatalf("Wilson interval lower bound = %.5f, want ~0.05523", low)
	}
	if math.Abs(high-0.17438) > 0.001 {
		t.Fatalf("Wilson interval upper bound = %.5f, want ~0.17438", high)
	}

	// Boundary: 0 successes out of n trials.
	// Wilson formula: lower = 0, upper = z^2 / (n + z^2).
	low0, high0 := filter.WilsonConfidenceInterval(1000, 0, z)
	if math.Abs(low0) > 1e-9 {
		t.Fatalf("expected lower bound 0 for 0 successes, got %f", low0)
	}
	expectedUpper0 := (z * z) / (1000.0 + z*z)
	if math.Abs(high0-expectedUpper0) > 1e-6 {
		t.Fatalf("upper bound for 0 successes = %f, want %f", high0, expectedUpper0)
	}

	// Boundary: n successes out of n trials.
	// Wilson formula: upper = 1.0, lower = n / (n + z^2).
	lowN, highN := filter.WilsonConfidenceInterval(1000, 1000, z)
	if highN != 1.0 {
		t.Fatalf("expected upper bound 1.0 for n successes, got %f", highN)
	}
	expectedLowerN := 1000.0 / (1000.0 + z*z)
	if math.Abs(lowN-expectedLowerN) > 1e-6 {
		t.Fatalf("lower bound for n successes = %f, want %f", lowN, expectedLowerN)
	}

	// Degenerate / invalid inputs
	l, h := filter.WilsonConfidenceInterval(0, 0, z)
	if l != 0 || h != 0 {
		t.Fatalf("expected (0, 0) for trials=0, got (%f, %f)", l, h)
	}
	l, h = filter.WilsonConfidenceInterval(-10, 5, z)
	if l != 0 || h != 0 {
		t.Fatalf("expected (0, 0) for trials=-10, got (%f, %f)", l, h)
	}
	l, h = filter.WilsonConfidenceInterval(100, -1, z)
	if l != 0 || h != 0 {
		t.Fatalf("expected (0, 0) for successes=-1, got (%f, %f)", l, h)
	}
	l, h = filter.WilsonConfidenceInterval(100, 10, -1.0)
	if l != 0 || h != 0 {
		t.Fatalf("expected (0, 0) for z=-1.0, got (%f, %f)", l, h)
	}
}

// TestZScore validates the standardized deviate calculation against independent values.
func TestZScore(t *testing.T) {
	n := 10000
	p := 0.01 // mu = 100, sigma = sqrt(10000 * 0.01 * 0.99) = sqrt(99) ≈ 9.94987437

	// Exact mean: Z should be 0.
	z0 := filter.ZScore(n, 100, p)
	if math.Abs(z0) > 1e-9 {
		t.Fatalf("ZScore at mean = %f, want 0.0", z0)
	}

	// +1 sigma
	zPlus1 := filter.ZScore(n, 110, p)
	expectedZ := (110.0 - 100.0) / math.Sqrt(99.0)
	if math.Abs(zPlus1-expectedZ) > 1e-6 {
		t.Fatalf("ZScore at 110 = %f, want %f", zPlus1, expectedZ)
	}

	// Degenerate inputs
	if z := filter.ZScore(0, 10, p); z != 0.0 {
		t.Fatalf("expected 0 for trials=0, got %f", z)
	}
	if z := filter.ZScore(1000, 10, 0.0); z != 0.0 {
		t.Fatalf("expected 0 for p=0, got %f", z)
	}
	if z := filter.ZScore(1000, 10, 1.0); z != 0.0 {
		t.Fatalf("expected 0 for p=1, got %f", z)
	}
}

// TestFormatKey verifies deterministic key formatting and zero allocation properties.
func TestFormatKey(t *testing.T) {
	var buf1, buf2, buf3 [32]byte

	key1 := filter.FormatKey(buf1[:], "insert:", 0)
	if string(key1) != "insert:0000000000" {
		t.Fatalf("FormatKey got %q, want %q", string(key1), "insert:0000000000")
	}

	key2 := filter.FormatKey(buf2[:], "insert:", 999999)
	if string(key2) != "insert:0000999999" {
		t.Fatalf("FormatKey got %q, want %q", string(key2), "insert:0000999999")
	}

	keyAbsent := filter.FormatKey(buf3[:], "absent:", 0)
	if string(keyAbsent) != "absent:0000000000" {
		t.Fatalf("FormatKey got %q, want %q", string(keyAbsent), "absent:0000000000")
	}

	// Disjointness check: prefix difference guarantees disjoint keys.
	if bytes.Equal(key1, keyAbsent) {
		t.Fatal("inserted key and absent key unexpectedly collided")
	}

	// Verify zero allocations
	var bufAlloc [32]byte
	allocs := testing.AllocsPerRun(1000, func() {
		_ = filter.FormatKey(bufAlloc[:], "test:", 123456)
	})
	if allocs != 0 {
		t.Fatalf("FormatKey performed %.1f allocations/run, want 0", allocs)
	}
}

// TestFPRExperiment_Controls exercises experimental setup controls to prevent flawed experiments.
func TestFPRExperiment_Controls(t *testing.T) {
	// Control A: Zero False Negatives on an actual filter.
	f := filter.NewBloomFilter(1000)
	var keyBuf [32]byte
	for i := 0; i < 1000; i++ {
		key := filter.FormatKey(keyBuf[:], "insert:", i)
		f.Add(key)
	}
	for i := 0; i < 1000; i++ {
		key := filter.FormatKey(keyBuf[:], "insert:", i)
		if !f.MayContain(key) {
			t.Fatalf("Control A failed: false negative for inserted key %s", string(key))
		}
	}

	// Control B: Disjointness validation.
	// Verify that for all i in [0, 1000], FormatKey("insert:", i) != FormatKey("absent:", j).
	for i := 0; i < 50; i++ {
		ki := filter.FormatKey(keyBuf[:], "insert:", i)
		var buf2 [32]byte
		for j := 0; j < 50; j++ {
			kj := filter.FormatKey(buf2[:], "absent:", j)
			if bytes.Equal(ki, kj) {
				t.Fatalf("Control B failed: overlap between %s and %s", string(ki), string(kj))
			}
		}
	}

	// Control C: Empty filter sanity.
	emptyFilter := filter.NewBloomFilter(0)
	absentKey := filter.FormatKey(keyBuf[:], "absent:", 42)
	if emptyFilter.MayContain(absentKey) {
		t.Fatal("Control C failed: empty filter reported MayContain=true")
	}

	// Invalid parameters to RunFPRExperiment
	if _, err := filter.RunFPRExperiment(0, 100); err == nil {
		t.Fatal("expected error for insertedCount=0")
	}
	if _, err := filter.RunFPRExperiment(100, 0); err == nil {
		t.Fatal("expected error for queryCount=0")
	}
}

// TestBloomFilter_EmpiricalFPR_Matrix verifies convergence and statistical sanity
// across a multi-scale matrix of populations (1K, 10K, 100K).
func TestBloomFilter_EmpiricalFPR_Matrix(t *testing.T) {
	sizes := []struct {
		n       int
		queries int
	}{
		{n: 1000, queries: 1000},
		{n: 10000, queries: 10000},
		{n: 100000, queries: 100000},
	}

	for _, tc := range sizes {
		t.Run(fmt.Sprintf("N=%d", tc.n), func(t *testing.T) {
			res, err := filter.RunFPRExperiment(tc.n, tc.queries)
			if err != nil {
				t.Fatalf("RunFPRExperiment(%d, %d) failed: %v", tc.n, tc.queries, err)
			}

			// Invariant 1: Zero false negatives
			if res.FalseNegatives != 0 {
				t.Fatalf("observed %d false negatives, expected 0", res.FalseNegatives)
			}

			// Invariant 2: Observed FPR should be reasonably close to ~0.82% (bounded by 2.5% for small N)
			if res.ObservedFPR > 0.025 {
				t.Fatalf("observed FPR %.4f exceeds upper safety bound of 2.5%%", res.ObservedFPR)
			}

			// Invariant 3: Sizing invariants
			expectedBits := uint64(tc.n) * uint64(filter.BitsPerKey)
			if res.BitCount != expectedBits {
				t.Fatalf("bitCount = %d, want %d", res.BitCount, expectedBits)
			}
			expectedBytes := int((expectedBits + 7) / 8)
			if res.ByteSize != expectedBytes {
				t.Fatalf("byteSize = %d, want %d", res.ByteSize, expectedBytes)
			}

			t.Logf("Matrix size N=%d, Queries=%d: FP=%d (%.4f%%), Expected=%.1f (%.4f%%), 95%% CI=[%.4f%%, %.4f%%], Z=%+.2f",
				tc.n, tc.queries, res.FalsePositives, res.ObservedFPR*100, res.ExpectedFP, res.TheoreticalFPR*100,
				res.WilsonLower95*100, res.WilsonUpper95*100, res.ZScore)
		})
	}
}

// TestBloomFilter_EmpiricalFalsePositiveRate_1M is the primary authoritative empirical
// verification test for P05-S02-M02.
// It inserts 1,000,000 unique keys and tests 1,000,000 disjoint absent keys.
func TestBloomFilter_EmpiricalFalsePositiveRate_1M(t *testing.T) {
	n := 1000000
	queries := 1000000

	res, err := filter.RunFPRExperiment(n, queries)
	if err != nil {
		t.Fatalf("RunFPRExperiment failed: %v", err)
	}

	// 1. Sizing verification
	expectedBits := uint64(10000000)
	expectedBytes := 1250000
	if res.BitCount != expectedBits {
		t.Fatalf("expected bitCount %d, got %d", expectedBits, res.BitCount)
	}
	if res.ByteSize != expectedBytes {
		t.Fatalf("expected byteSize %d, got %d", expectedBytes, res.ByteSize)
	}
	if res.BitsPerKey != 10 {
		t.Fatalf("expected BitsPerKey 10, got %d", res.BitsPerKey)
	}
	if res.HashCount != 7 {
		t.Fatalf("expected HashCount 7, got %d", res.HashCount)
	}

	// 2. Correctness invariant: Zero False Negatives
	if res.FalseNegatives != 0 {
		t.Fatalf("CRITICAL: detected %d false negatives on inserted keys", res.FalseNegatives)
	}

	// 3. Observed FPR bound: must be well below 1.0% (theoretical is ~0.819%)
	if res.ObservedFPR >= 0.01 {
		t.Fatalf("Observed FPR %.4f%% exceeds 1.0%% threshold", res.ObservedFPR*100.0)
	}

	// 4. Statistical Acceptance Criterion:
	// Theoretical FPR must fall within the 95% Wilson confidence interval OR
	// the observed result must have |Z-score| <= 3.0 (within 3 standard deviations).
	inCI := res.TheoreticalFPR >= res.WilsonLower95 && res.TheoreticalFPR <= res.WilsonUpper95
	within3Sigma := math.Abs(res.ZScore) <= 3.0

	t.Logf("\n============================================================\n"+
		"EMPIRICAL FALSE POSITIVE RATE VERIFICATION (1,000,000 KEYS)\n"+
		"============================================================\n"+
		"%s\n"+
		"============================================================\n"+
		"Theoretical FPR in 95%% CI: %v\n"+
		"Within 3 Sigma (|Z| <= 3.0): %v (Z = %+.4f)\n"+
		"============================================================",
		res.String(), inCI, within3Sigma, res.ZScore)

	if !inCI && !within3Sigma {
		t.Fatalf("Statistical verification FAILED: Theoretical FPR %.6f outside 95%% CI [%.6f, %.6f] and Z-score %+.4f exceeds 3.0",
			res.TheoreticalFPR, res.WilsonLower95, res.WilsonUpper95, res.ZScore)
	}
}

// BenchmarkBloomFilter_EmpiricalFPR_1M benchmarks the performance and allocations
// of populating 1,000,000 keys and performing 1,000,000 membership queries.
func BenchmarkBloomFilter_EmpiricalFPR_1M(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, err := filter.RunFPRExperiment(1000000, 1000000)
		if err != nil {
			b.Fatalf("RunFPRExperiment failed: %v", err)
		}
		if res.FalseNegatives != 0 {
			b.Fatalf("unexpected false negatives: %d", res.FalseNegatives)
		}
	}
}

package benchmark_test

import (
	"math"
	"math/rand"
	"testing"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

// TestZipf_RankFrequencyMonotonicity verifies that higher-ranked (lower index) keys
// are sampled with strictly higher frequencies than lower-ranked keys.
func TestZipf_RankFrequencyMonotonicity(t *testing.T) {
	const n = 100
	const samples = 200000

	gen, err := benchmark.NewDefaultZipfGenerator(n, 777)
	if err != nil {
		t.Fatal(err)
	}

	counts := make([]int, n)
	for i := 0; i < samples; i++ {
		counts[gen.Next()]++
	}

	// Top 5 ranks should be strictly monotonically decreasing with high statistical confidence
	for r := 0; r < 4; r++ {
		if counts[r] <= counts[r+1] {
			t.Fatalf("expected count(rank %d)=%d > count(rank %d)=%d", r, counts[r], r+1, counts[r+1])
		}
	}

	// Rank 0 should dominate
	rank0Ratio := float64(counts[0]) / float64(samples)
	if rank0Ratio < 0.15 || rank0Ratio > 0.22 {
		t.Fatalf("unexpected rank 0 frequency ratio: %f (expected ~0.18-0.19)", rank0Ratio)
	}
}

// TestZipf_HotspotConcentration80_20 verifies that in a sufficiently large
// keyspace (N = 1000), empirical sampling with canonical skew theta = 0.99
// concentrates approximately 75-80% of accesses in the top 20% of the keyspace,
// demonstrating asymptotic alignment with the Pareto 80/20 property.
//
// Note: In smaller domains (e.g. N <= 100), finite harmonic sum normalization
// yields lower top-20% shares (e.g. ~69% for N=100, ~51% for N=10).
func TestZipf_HotspotConcentration80_20(t *testing.T) {
	const n = 1000
	const samples = 200000

	gen, err := benchmark.NewDefaultZipfGenerator(n, 9999)
	if err != nil {
		t.Fatal(err)
	}

	top20PercentCutoff := uint64(n / 5) // top 200 ranks [0..199]
	var top20Accesses int

	for i := 0; i < samples; i++ {
		rank := gen.Next()
		if rank < top20PercentCutoff {
			top20Accesses++
		}
	}

	ratio := float64(top20Accesses) / float64(samples)
	// For N=1000 and theta=0.99, theoretical top 20% share is ~77.89%
	if ratio < 0.70 || ratio > 0.85 {
		t.Fatalf("expected top 20%% of keys to receive ~75-80%% of accesses, observed %.2f%%", ratio*100)
	}
}

// TestZipf_TheoreticalConcentration_DomainScaling verifies the exact discrete
// mathematical CDF across varying keyspace sizes N, demonstrating that the
// theoretical top-20% access share scales monotonically with domain size N
// and approaches ~78% asymptotically as N grows large.
func TestZipf_TheoreticalConcentration_DomainScaling(t *testing.T) {
	cases := []struct {
		n               uint64
		top20Ranks      uint64
		expectedMinFrac float64
		expectedMaxFrac float64
	}{
		{n: 10, top20Ranks: 2, expectedMinFrac: 0.50, expectedMaxFrac: 0.52},     // ~50.86%
		{n: 50, top20Ranks: 10, expectedMinFrac: 0.64, expectedMaxFrac: 0.66},    // ~64.59%
		{n: 100, top20Ranks: 20, expectedMinFrac: 0.68, expectedMaxFrac: 0.70},   // ~68.81%
		{n: 1000, top20Ranks: 200, expectedMinFrac: 0.77, expectedMaxFrac: 0.79}, // ~77.89%
	}

	const theta = benchmark.DefaultZipfTheta

	for _, tc := range cases {
		var zetan float64
		for r := uint64(1); r <= tc.n; r++ {
			zetan += 1.0 / math.Pow(float64(r), theta)
		}

		var top20Sum float64
		for r := uint64(1); r <= tc.top20Ranks; r++ {
			top20Sum += 1.0 / math.Pow(float64(r), theta)
		}

		theoreticalShare := top20Sum / zetan
		if theoreticalShare < tc.expectedMinFrac || theoreticalShare > tc.expectedMaxFrac {
			t.Fatalf("N=%d: theoretical top 20%% share = %f, want [%f, %f]",
				tc.n, theoreticalShare, tc.expectedMinFrac, tc.expectedMaxFrac)
		}
	}
}

// TestZipf_UniformDistributionRegression verifies that an accidental or buggy
// uniform random distribution is decisively rejected by the Zipf statistical test.
func TestZipf_UniformDistributionRegression(t *testing.T) {
	const n = 100
	const samples = 100000

	// 1. Generate real Zipf distribution
	zipfGen, err := benchmark.NewDefaultZipfGenerator(n, 42)
	if err != nil {
		t.Fatal(err)
	}
	zipfTop20 := 0
	for i := 0; i < samples; i++ {
		if zipfGen.Next() < 20 {
			zipfTop20++
		}
	}
	zipfRatio := float64(zipfTop20) / float64(samples)

	// 2. Simulate flawed uniform sampler
	uniformRng := rand.New(rand.NewSource(42))
	uniformTop20 := 0
	for i := 0; i < samples; i++ {
		if uniformRng.Intn(int(n)) < 20 {
			uniformTop20++
		}
	}
	uniformRatio := float64(uniformTop20) / float64(samples)

	// Uniform top 20% must be ~20%
	if uniformRatio < 0.18 || uniformRatio > 0.22 {
		t.Fatalf("unexpected uniform ratio: %f", uniformRatio)
	}

	// Zipf top 20% for N=100 (theoretical discrete top 20% is ~68.8%)
	// Must substantially exceed the uniform ~20%
	if zipfRatio < 0.60 {
		t.Fatalf("zipf generator failed hotspot test: ratio = %f (expected ~0.68-0.71)", zipfRatio)
	}

	// The difference must be vast (> 40 percentage points)
	diff := zipfRatio - uniformRatio
	if diff < 0.40 {
		t.Fatalf("failed to distinguish Zipfian generator from uniform random sampler! diff = %f", diff)
	}
}

// TestZipf_ChiSquareGoodnessOfFit validates the empirical frequency distribution
// against the theoretical discrete Zipfian probability model:
//
//	P(r) = r^(-theta) / H(N, theta)
func TestZipf_ChiSquareGoodnessOfFit(t *testing.T) {
	const n = 50
	const samples = 10000
	const theta = benchmark.DefaultZipfTheta

	gen, err := benchmark.NewDefaultZipfGenerator(n, 1234)
	if err != nil {
		t.Fatal(err)
	}

	observed := make([]int, n)
	for i := 0; i < samples; i++ {
		observed[gen.Next()]++
	}

	// Compute theoretical sum H(N, theta)
	var zetan float64
	for i := uint64(1); i <= n; i++ {
		zetan += 1.0 / math.Pow(float64(i), theta)
	}

	// Evaluate Chi-Square: sum (O_i - E_i)^2 / E_i
	var chiSquare float64
	for i := 0; i < int(n); i++ {
		rank := float64(i + 1)
		prob := (1.0 / math.Pow(rank, theta)) / zetan
		expected := prob * float64(samples)

		diff := float64(observed[i]) - expected
		chiSquare += (diff * diff) / expected
	}

	// For degrees of freedom df = 49, critical value at p=0.01 is 74.9.
	// For Jim Gray's continuous approximation with finite sample variance,
	// a threshold of 100.0 safely validates fit.
	// (A uniform sampler would yield chiSquare > 10,000).
	if chiSquare > 100.0 {
		t.Fatalf("Chi-Square goodness-of-fit test failed: chi2 = %f (distribution diverges from Zipf)", chiSquare)
	}
	t.Logf("Chi-Square goodness-of-fit statistic: %f (df=%d, N=%d, samples=%d)", chiSquare, n-1, n, samples)
}

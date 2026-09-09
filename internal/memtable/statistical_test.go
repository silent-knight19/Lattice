package memtable_test

import (
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/memtable"
)

func TestGeometricHeight_StatisticalDistribution_100K(t *testing.T) {
	// Statistical Test over 100,000 generated heights.
	//
	// Mathematical Formulation:
	// Let N = 100,000.
	// Parameter p = 0.25 (DefaultPromotionProbability), Lmax = 16 (MaxHeight).
	//
	// Theoretical Probability Mass Function (PMF):
	// For 1 <= h < Lmax:
	//   P(H = h) = (1 - p) * p^(h-1) = 0.75 * (0.25)^(h-1)
	// For h = Lmax:
	//   P(H = Lmax) = p^(Lmax-1) = (0.25)^15 ≈ 9.313e-10
	//
	// Expected Frequencies for N = 100,000:
	//   h = 1:  100,000 * 0.75000000 = 75,000.00
	//   h = 2:  100,000 * 0.18750000 = 18,750.00
	//   h = 3:  100,000 * 0.04687500 =  4,687.50
	//   h = 4:  100,000 * 0.01171875 =  1,171.875
	//   h = 5:  100,000 * 0.00292969 =    292.969
	//   h = 6:  100,000 * 0.00073242 =     73.242
	//   h = 7:  100,000 * 0.00018311 =     18.311
	//   h >= 8: 100,000 * (0.25)^7    =      6.104
	//
	// Cochran's Criterion for Chi-Square Test:
	// To ensure test validity, all expected cell counts must be >= 1, and at least 80%
	// must be >= 5. Combining all rare buckets h >= 8 yields E = 6.104 > 5, satisfying
	// Cochran's criterion across all 8 bins.
	//
	// Degrees of Freedom: df = 8 - 1 = 7.
	// Critical value at alpha = 0.001 significance: chi2_crit ≈ 24.322.

	const sampleSize = 100000
	const p = memtable.DefaultPromotionProbability // 0.25

	// Seeded PRNG for deterministic, reproducible CI execution
	src := memtable.NewPCG32(42, 54)
	gen := memtable.NewHeightGenerator(src)

	observed := make(map[int]int)
	for i := 0; i < sampleSize; i++ {
		h := gen.RandomHeight()
		if h < memtable.MinHeight || h > memtable.MaxHeight {
			t.Fatalf("height %d out of bounds [%d, %d]", h, memtable.MinHeight, memtable.MaxHeight)
		}
		observed[h]++
	}

	// Calculate expected counts for bins 1..7 and aggregate bin 8+
	expected := make([]float64, 8) // index 0 unused, 1..7 for h=1..7, 7 for h>=8
	expectedP := make([]float64, 8)

	for h := 1; h <= 7; h++ {
		prob := (1.0 - p) * math.Pow(p, float64(h-1))
		expectedP[h] = prob
		expected[h] = float64(sampleSize) * prob
	}

	// Bin 8+: P(H >= 8) = p^7
	prob8Plus := math.Pow(p, 7.0)
	expected8Plus := float64(sampleSize) * prob8Plus

	// Aggregate observed counts for h >= 8
	observed8Plus := 0
	for h := 8; h <= memtable.MaxHeight; h++ {
		observed8Plus += observed[h]
	}

	t.Logf("=== 100,000 Sample SkipList Height Distribution ===")
	t.Logf("%-6s | %-10s | %-12s | %-10s | %-10s", "Height", "Observed", "Expected", "Expected %", "Observed %")
	t.Logf("------------------------------------------------------------------")

	for h := 1; h <= 7; h++ {
		obs := observed[h]
		exp := expected[h]
		obsPct := (float64(obs) / float64(sampleSize)) * 100.0
		expPct := expectedP[h] * 100.0
		t.Logf("%-6d | %-10d | %-12.2f | %9.4f%% | %9.4f%%", h, obs, exp, expPct, obsPct)
	}
	obs8PlusPct := (float64(observed8Plus) / float64(sampleSize)) * 100.0
	exp8PlusPct := prob8Plus * 100.0
	t.Logf("%-6s | %-10d | %-12.2f | %9.4f%% | %9.4f%%", ">= 8", observed8Plus, expected8Plus, exp8PlusPct, obs8PlusPct)

	// 1. Pearson's Chi-Square Goodness-of-Fit Test
	var chiSquare float64
	for h := 1; h <= 7; h++ {
		obs := float64(observed[h])
		exp := expected[h]
		diff := obs - exp
		chiSquare += (diff * diff) / exp
	}
	diff8Plus := float64(observed8Plus) - expected8Plus
	chiSquare += (diff8Plus * diff8Plus) / expected8Plus

	t.Logf("Pearson's Chi-Square Statistic: %.4f (df = 7, critical value at alpha=0.001 is 24.322)", chiSquare)

	// Critical threshold at alpha = 0.001 for df = 7
	const chiSquareCritical = 24.322
	if chiSquare > chiSquareCritical {
		t.Fatalf("observed distribution failed Chi-Square goodness-of-fit: chi2 = %.4f > %.4f",
			chiSquare, chiSquareCritical)
	}

	// 2. Binomial Confidence Interval Verification for Dominant Buckets (h=1, 2, 3)
	// For each bucket, the observed count must lie within a 4-sigma envelope
	// sigma = sqrt(N * p * (1 - p)). Under normal approximation, P(|Z| > 4) ≈ 0.000063.
	for h := 1; h <= 3; h++ {
		expProb := expectedP[h]
		sigma := math.Sqrt(float64(sampleSize) * expProb * (1.0 - expProb))
		margin := 4.0 * sigma

		obs := float64(observed[h])
		exp := expected[h]
		lowerBound := exp - margin
		upperBound := exp + margin

		t.Logf("Bucket h=%d: Observed=%d, Expected=%.1f, 4-Sigma Range=[%.1f, %.1f]",
			h, observed[h], exp, lowerBound, upperBound)

		if obs < lowerBound || obs > upperBound {
			t.Errorf("bucket h=%d observed count %d outside 4-sigma envelope [%.1f, %.1f]",
				h, observed[h], lowerBound, upperBound)
		}
	}
}

func TestGeometricHeight_MultipleSeedsConsistency(t *testing.T) {
	// Verify that the Chi-Square goodness-of-fit passes across 5 distinct PRNG seeds.
	const sampleSize = 50000
	const p = memtable.DefaultPromotionProbability

	seeds := []uint64{101, 2024, 77777, 999999, 133742}

	for _, seed := range seeds {
		src := memtable.NewPCG32(seed, 1)
		gen := memtable.NewHeightGenerator(src)

		observed := make(map[int]int)
		for i := 0; i < sampleSize; i++ {
			observed[gen.RandomHeight()]++
		}

		var chiSquare float64
		for h := 1; h <= 6; h++ {
			exp := float64(sampleSize) * (1.0 - p) * math.Pow(p, float64(h-1))
			obs := float64(observed[h])
			diff := obs - exp
			chiSquare += (diff * diff) / exp
		}
		// Bucket 7+
		exp7Plus := float64(sampleSize) * math.Pow(p, 6.0)
		obs7Plus := 0
		for h := 7; h <= memtable.MaxHeight; h++ {
			obs7Plus += observed[h]
		}
		diff7Plus := float64(obs7Plus) - exp7Plus
		chiSquare += (diff7Plus * diff7Plus) / exp7Plus

		// Critical value for df = 6 at alpha = 0.001 is 22.458
		if chiSquare > 22.458 {
			t.Errorf("seed %d: chi-square %.4f exceeds critical value 22.458", seed, chiSquare)
		}
	}
}

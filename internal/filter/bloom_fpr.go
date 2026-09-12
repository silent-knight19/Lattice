package filter

import (
	"fmt"
	"math"
	"time"
)

// StandardNormalZ95 is the critical value for a two-tailed 95% confidence interval
// under the standard normal distribution (Z_{0.975} ≈ 1.959963984540054).
const StandardNormalZ95 = 1.959963984540054

// TheoreticalFPR computes the theoretical false positive probability of a standard
// Bloom filter under the standard asymptotic approximation:
//
//	p = (1 - e^(-k * n / m))^k = (1 - e^(-k / bitsPerKey))^k
//
// Parameters:
//   - bitsPerKey: the ratio m / n (e.g. 10.0 in Lattice).
//   - hashCount: the number of hash functions k (e.g. 7 in Lattice).
//
// Return Contract:
//   - If bitsPerKey <= 0 or hashCount <= 0: returns 1.0 (empty or degenerate filter).
//   - Otherwise: returns p in (0.0, 1.0).
func TheoreticalFPR(bitsPerKey float64, hashCount int) float64 {
	if bitsPerKey <= 0 || hashCount <= 0 {
		return 1.0
	}
	exponent := -float64(hashCount) / bitsPerKey
	pSingleProbe := 1.0 - math.Exp(exponent)
	return math.Pow(pSingleProbe, float64(hashCount))
}

// WilsonConfidenceInterval computes the two-tailed Wilson score confidence interval
// for a binomial proportion p given sample trials n and successes x at confidence level z.
//
// The Wilson score interval is preferred over the normal approximation because it
// maintains nominal coverage near the boundaries (p near 0 or 1) and never produces
// intervals outside [0, 1].
//
// Mathematical Formulation:
//
//	p_hat = x / n
//	denominator = 1 + z^2 / n
//	center = p_hat + z^2 / (2n)
//	margin = z * sqrt( (p_hat * (1 - p_hat) / n) + (z^2 / (4n^2)) )
//	lower = (center - margin) / denominator
//	upper = (center + margin) / denominator
//
// Return Contract:
//   - If trials <= 0 or successes < 0: returns (0.0, 0.0).
//   - Clamps the returned interval to [0.0, 1.0].
func WilsonConfidenceInterval(trials int, successes int, z float64) (float64, float64) {
	if trials <= 0 || successes < 0 || z <= 0 {
		return 0.0, 0.0
	}
	if successes > trials {
		successes = trials
	}

	n := float64(trials)
	x := float64(successes)
	pHat := x / n
	z2 := z * z

	denominator := 1.0 + z2/n
	center := pHat + z2/(2.0*n)
	margin := z * math.Sqrt((pHat*(1.0-pHat)/n)+(z2/(4.0*n*n)))

	lower := (center - margin) / denominator
	upper := (center + margin) / denominator

	if successes == 0 || lower < 0.0 {
		lower = 0.0
	}
	if successes == trials || upper > 1.0 {
		upper = 1.0
	}

	return lower, upper
}

// ZScore computes the standardized test statistic (Z-score) for an observed number of
// successes x out of trials n, tested against a theoretical probability pTheoretical:
//
//	mu = n * pTheoretical
//	sigma = sqrt(n * pTheoretical * (1 - pTheoretical))
//	Z = (x - mu) / sigma
//
// Return Contract:
//   - If trials <= 0 or pTheoretical <= 0 or pTheoretical >= 1: returns 0.0.
func ZScore(trials int, successes int, pTheoretical float64) float64 {
	if trials <= 0 || pTheoretical <= 0 || pTheoretical >= 1.0 {
		return 0.0
	}
	n := float64(trials)
	mu := n * pTheoretical
	sigma := math.Sqrt(n * pTheoretical * (1.0 - pTheoretical))
	if sigma == 0.0 {
		return 0.0
	}
	return (float64(successes) - mu) / sigma
}

// FormatKey formats a deterministic key with prefix and a 10-digit zero-padded id into dst.
// dst must have capacity of at least len(prefix) + 10 bytes.
// This function executes with zero heap allocations.
func FormatKey(dst []byte, prefix string, id int) []byte {
	n := copy(dst, prefix)
	val := id
	if val < 0 {
		val = -val
	}
	for i := 9; i >= 0; i-- {
		dst[n+i] = byte('0' + (val % 10))
		val /= 10
	}
	return dst[:n+10]
}

// FPRExperimentResult encapsulates the full empirical and theoretical measurement
// of a Bloom filter false positive rate verification experiment.
type FPRExperimentResult struct {
	InsertedCount  int           // Total keys inserted into the filter
	QueryCount     int           // Total disjoint absent keys queried
	BitsPerKey     int           // Configured bits per key (10)
	HashCount      int           // Configured hash probe count (7)
	BitCount       uint64        // Exact allocated bit count in bitset
	ByteSize       int           // Exact allocated byte count in bitset
	FalsePositives int           // Number of absent keys incorrectly reported as present
	TrueNegatives  int           // Number of absent keys correctly reported as absent
	FalseNegatives int           // Number of inserted keys incorrectly reported as absent (MUST BE 0)
	ObservedFPR    float64       // FalsePositives / QueryCount
	TheoreticalFPR float64       // (1 - e^(-k/10))^k
	ExpectedFP     float64       // QueryCount * TheoreticalFPR
	WilsonLower95  float64       // Lower bound of 95% Wilson score confidence interval
	WilsonUpper95  float64       // Upper bound of 95% Wilson score confidence interval
	ZScore         float64       // Standardized deviate (Observed - Expected) / sigma
	InsertDuration time.Duration // Time spent inserting keys
	QueryDuration  time.Duration // Time spent querying absent keys
}

// String formats the experiment result into a comprehensive diagnostic summary.
func (r *FPRExperimentResult) String() string {
	return fmt.Sprintf(
		"FPRExperimentResult:\n"+
			"  Inserted Keys:     %d\n"+
			"  Absent Queries:    %d\n"+
			"  Bits Per Key:      %d\n"+
			"  Hash Functions:    %d\n"+
			"  Bit Count (m):     %d\n"+
			"  Byte Size:         %d bytes\n"+
			"  False Positives:   %d\n"+
			"  True Negatives:    %d\n"+
			"  False Negatives:   %d (must be 0)\n"+
			"  Observed FPR:      %.6f (%.4f%%)\n"+
			"  Theoretical FPR:   %.6f (%.4f%%)\n"+
			"  Expected FP:       %.1f\n"+
			"  95%% Wilson CI:     [%.6f, %.6f] ([%.4f%%, %.4f%%])\n"+
			"  Z-Score:           %+.4f\n"+
			"  Insert Duration:   %v\n"+
			"  Query Duration:    %v",
		r.InsertedCount,
		r.QueryCount,
		r.BitsPerKey,
		r.HashCount,
		r.BitCount,
		r.ByteSize,
		r.FalsePositives,
		r.TrueNegatives,
		r.FalseNegatives,
		r.ObservedFPR,
		r.ObservedFPR*100.0,
		r.TheoreticalFPR,
		r.TheoreticalFPR*100.0,
		r.ExpectedFP,
		r.WilsonLower95,
		r.WilsonUpper95,
		r.WilsonLower95*100.0,
		r.WilsonUpper95*100.0,
		r.ZScore,
		r.InsertDuration,
		r.QueryDuration,
	)
}

// RunFPRExperiment executes a deterministic empirical false-positive rate experiment
// using streaming stack-allocated keys to maintain minimal memory overhead.
//
// Key Population Strategy:
//   - Inserted Keys: "insert:0000000000" ... "insert:0000000000" + (insertedCount - 1)
//   - Absent Queries: "absent:0000000000" ... "absent:0000000000" + (queryCount - 1)
//   - Disjointness: Prefix divergence ensures Inserted ∩ Absent = ∅.
//
// Control Verification:
//   - Zero False Negatives: Checks up to 10,000 inserted keys to verify 0 false negatives.
//
// Memory & Performance:
//   - Uses local stack buffers ([32]byte) for key formatting.
//   - Allocates only the BloomFilter itself.
func RunFPRExperiment(insertedCount int, queryCount int) (*FPRExperimentResult, error) {
	if insertedCount <= 0 {
		return nil, fmt.Errorf("insertedCount must be positive, got %d", insertedCount)
	}
	if queryCount <= 0 {
		return nil, fmt.Errorf("queryCount must be positive, got %d", queryCount)
	}

	filter := NewBloomFilter(insertedCount)
	if filter == nil {
		return nil, fmt.Errorf("failed to allocate BloomFilter for %d keys", insertedCount)
	}

	var keyBuf [32]byte

	// 1. Insert Population
	insertStart := time.Now()
	for i := 0; i < insertedCount; i++ {
		key := FormatKey(keyBuf[:], "insert:", i)
		filter.Add(key)
	}
	insertDuration := time.Since(insertStart)

	// 2. Control A: Verify Zero False Negatives on a sample of inserted keys
	controlSamples := insertedCount
	if controlSamples > 10000 {
		controlSamples = 10000
	}
	falseNegatives := 0
	for i := 0; i < controlSamples; i++ {
		key := FormatKey(keyBuf[:], "insert:", i)
		if !filter.MayContain(key) {
			falseNegatives++
		}
	}

	// 3. Query Disjoint Absent Population
	falsePositives := 0
	trueNegatives := 0

	queryStart := time.Now()
	for i := 0; i < queryCount; i++ {
		key := FormatKey(keyBuf[:], "absent:", i)
		if filter.MayContain(key) {
			falsePositives++
		} else {
			trueNegatives++
		}
	}
	queryDuration := time.Since(queryStart)

	// 4. Statistical Analysis
	pTheo := TheoreticalFPR(float64(BitsPerKey), HashFunctions)
	obsFPR := float64(falsePositives) / float64(queryCount)
	expectedFP := float64(queryCount) * pTheo
	wLower, wUpper := WilsonConfidenceInterval(queryCount, falsePositives, StandardNormalZ95)
	z := ZScore(queryCount, falsePositives, pTheo)

	return &FPRExperimentResult{
		InsertedCount:  insertedCount,
		QueryCount:     queryCount,
		BitsPerKey:     BitsPerKey,
		HashCount:      HashFunctions,
		BitCount:       filter.BitCount(),
		ByteSize:       filter.ByteSize(),
		FalsePositives: falsePositives,
		TrueNegatives:  trueNegatives,
		FalseNegatives: falseNegatives,
		ObservedFPR:    obsFPR,
		TheoreticalFPR: pTheo,
		ExpectedFP:     expectedFP,
		WilsonLower95:  wLower,
		WilsonUpper95:  wUpper,
		ZScore:         z,
		InsertDuration: insertDuration,
		QueryDuration:  queryDuration,
	}, nil
}

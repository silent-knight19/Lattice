package benchmark

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"
)

const (
	// subBucketBits defines the number of bits allocated to linear sub-buckets
	// within each power-of-two octave (2^7 = 128 sub-buckets).
	// This guarantees a maximum relative quantization error <= 1/128 = 0.78125% (< 1.0%)
	// across the entire trackable range.
	subBucketBits = 7

	// subBucketCount is the number of sub-buckets per octave (128).
	subBucketCount = 1 << subBucketBits

	// subBucketMask is the bitmask for extracting the sub-bucket offset (127).
	subBucketMask = subBucketCount - 1

	// maxOctave is the highest power-of-two octave explicitly tracked.
	// Octave 43 covers [2^43, 2^44 - 1] = [8.796e12, 17.592e12 - 1] ns (~4.88 hours),
	// which comfortably exceeds the benchmark requirement of 3,600 seconds (1 hour).
	maxOctave = 43

	// numRegularBuckets is the number of regular linear and logarithmic buckets:
	// 1 base octave [0, 127] + (maxOctave - subBucketBits + 1) octaves = 38 * 128 = 4,864 buckets.
	numRegularBuckets = (maxOctave - subBucketBits + 2) * subBucketCount

	// overflowBucketIndex is the index of the single overflow bucket capturing
	// latencies >= 2^44 ns (index 4,864).
	overflowBucketIndex = numRegularBuckets

	// totalBuckets is the total size of the fixed bucket array (4,865 buckets).
	totalBuckets = numRegularBuckets + 1

	// maxTrackableNs is the highest latency trackable in regular buckets:
	// 2^44 - 1 = 17,592,186,044,415 ns (~4.88 hours).
	maxTrackableNs = (int64(1) << (maxOctave + 1)) - 1
)

const (
	// MaxTrackableLatency is the upper boundary of regular logarithmic sub-bucket coverage
	// (~4.88 hours). Latencies >= MaxTrackableLatency are safely captured in the overflow
	// bucket while preserving exact Max.
	MaxTrackableLatency = time.Duration(maxTrackableNs)

	// SubBucketCount is the number of sub-buckets per octave (128), providing <= 0.78% relative error.
	SubBucketCount = subBucketCount

	// TotalBuckets is the fixed number of buckets in LatencyHistogram (4,865).
	TotalBuckets = totalBuckets
)

var (
	// ErrEmptyHistogram indicates that a percentile query was attempted on a histogram with zero observations.
	ErrEmptyHistogram = errors.New("benchmark: histogram is empty")

	// ErrInvalidPercentile indicates that the requested quantile p is outside [0.0, 1.0] or is NaN.
	ErrInvalidPercentile = errors.New("benchmark: percentile must be in range [0.0, 1.0]")

	// ErrNilHistogram indicates that a nil histogram pointer was passed to Merge.
	ErrNilHistogram = errors.New("benchmark: cannot merge nil histogram")
)

// LatencyHistogram records operation durations into a fixed-size array of logarithmic
// sub-buckets, providing high-resolution percentile queries (P50, P90, P99, P99.9) and exact
// extrema (Min, Max) with zero memory allocations on the recording hot path and bounded O(1) memory.
//
// Mathematical Model:
// - Latencies in [0, 127] ns have exact 1-nanosecond resolution (128 linear buckets).
// - Latencies in [128, 2^44 - 1] ns (~4.88 hours) are grouped into base-2 octaves [2^k, 2^(k+1)-1].
//   Each octave is subdivided into 128 equal sub-buckets. The bucket width in octave k is
//   2^(k-7) ns, ensuring that the maximum relative quantization error (bucket width / value)
//   never exceeds 1/128 = 0.78125% (< 1.0%).
// - Latencies >= 2^44 ns are captured in an overflow bucket. Exact Max is tracked independently.
//
// Percentile Semantics:
// Percentiles use the standard discrete nearest-rank convention: rank = ceil(p * Count).
// The value returned is the upper bound of the matching bucket, guaranteeing the conservative
// SLA/SLO property: at least p*100% of recorded operations completed in <= the returned latency.
//
// Concurrency Model:
// LatencyHistogram is intentionally NOT concurrency-safe and requires no internal locking.
// In high-throughput multi-threaded benchmark runners (such as cmd/lattice-bench), each worker
// goroutine should maintain its own independent LatencyHistogram instance. After the run,
// worker histograms are aggregated into a single report via Merge(other).
//
// Zero Value:
// The zero value of LatencyHistogram is initialized, valid, and immediately ready for use.
type LatencyHistogram struct {
	count          uint64
	totalNs        uint64
	minNs          int64
	maxNs          int64
	totalOverflown bool
	buckets        [totalBuckets]uint64
}

// NewLatencyHistogram constructs and returns a new empty LatencyHistogram.
func NewLatencyHistogram() *LatencyHistogram {
	return &LatencyHistogram{}
}

// Record records an elapsed time.Duration observation.
// Negative durations are discarded to prevent state corruption.
// This method executes in O(1) expected time, is lock-free, and performs 0 heap allocations.
func (h *LatencyHistogram) Record(d time.Duration) {
	h.RecordNano(d.Nanoseconds())
}

// RecordNano records an integer nanosecond latency observation.
// Negative values are discarded to prevent state corruption.
// This method executes in O(1) expected time, is lock-free, and performs 0 heap allocations.
func (h *LatencyHistogram) RecordNano(ns int64) {
	if ns < 0 {
		return
	}

	if h.count == 0 {
		h.minNs = ns
		h.maxNs = ns
	} else {
		if ns < h.minNs {
			h.minNs = ns
		}
		if ns > h.maxNs {
			h.maxNs = ns
		}
	}
	h.count++

	if !h.totalOverflown {
		uns := uint64(ns)
		if math.MaxUint64-h.totalNs < uns {
			h.totalNs = math.MaxUint64
			h.totalOverflown = true
		} else {
			h.totalNs += uns
		}
	}

	b := valueToBucket(ns)
	h.buckets[b]++
}

// Count returns the total number of valid recorded observations.
func (h *LatencyHistogram) Count() uint64 {
	return h.count
}

// Min returns the exact minimum observed latency.
// Returns 0 if the histogram is empty.
func (h *LatencyHistogram) Min() time.Duration {
	if h.count == 0 {
		return 0
	}
	return time.Duration(h.minNs)
}

// Max returns the exact maximum observed latency.
// Returns 0 if the histogram is empty.
func (h *LatencyHistogram) Max() time.Duration {
	if h.count == 0 {
		return 0
	}
	return time.Duration(h.maxNs)
}

// Mean returns the arithmetic mean duration of all recorded observations.
// Returns 0 if the histogram is empty.
// If the cumulative nanosecond sum overflew uint64, it computes a safe approximation from bucket midpoints.
func (h *LatencyHistogram) Mean() time.Duration {
	if h.count == 0 {
		return 0
	}
	if !h.totalOverflown {
		return time.Duration(h.totalNs / h.count)
	}

	// Fallback when cumulative nanoseconds exceeded MaxUint64: estimate from bucket midpoints
	var weightedSum float64
	for i := 0; i < totalBuckets; i++ {
		c := h.buckets[i]
		if c == 0 {
			continue
		}
		var mid float64
		if i == 0 {
			mid = 0
		} else if i < subBucketCount {
			mid = float64(i)
		} else if i >= overflowBucketIndex {
			mid = float64(h.maxNs)
		} else {
			u := bucketToUpper(i)
			group := i / subBucketCount
			msb := group + subBucketBits - 1
			shift := msb - subBucketBits
			l := (int64(1) << msb) + (int64(i%subBucketCount) << shift)
			mid = float64(l+u) / 2.0
		}
		weightedSum += float64(c) * mid
	}
	meanNs := weightedSum / float64(h.count)
	if meanNs > float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(int64(meanNs))
}

// Percentile computes the latency value at quantile p in [0.0, 1.0].
// Returns ErrEmptyHistogram if count == 0.
// Returns ErrInvalidPercentile if p < 0, p > 1, or p is NaN.
// Quantile 0.0 returns exact Min(); quantile 1.0 returns exact Max().
func (h *LatencyHistogram) Percentile(p float64) (time.Duration, error) {
	if math.IsNaN(p) || p < 0.0 || p > 1.0 {
		return 0, fmt.Errorf("%w: quantile %v must be in range [0.0, 1.0]", ErrInvalidPercentile, p)
	}
	if h.count == 0 {
		return 0, ErrEmptyHistogram
	}
	if p == 0.0 {
		return time.Duration(h.minNs), nil
	}
	if p == 1.0 {
		return time.Duration(h.maxNs), nil
	}

	targetRank := uint64(math.Ceil(p * float64(h.count)))
	if targetRank == 0 {
		targetRank = 1
	}
	if targetRank > h.count {
		targetRank = h.count
	}

	var accum uint64
	for i := 0; i < totalBuckets; i++ {
		accum += h.buckets[i]
		if accum >= targetRank {
			if i >= overflowBucketIndex {
				return time.Duration(h.maxNs), nil
			}
			upper := bucketToUpper(i)
			if upper > h.maxNs {
				upper = h.maxNs
			}
			return time.Duration(upper), nil
		}
	}
	return time.Duration(h.maxNs), nil
}

// P50 returns the 50th percentile (median) latency. Returns 0 if empty.
func (h *LatencyHistogram) P50() time.Duration {
	d, _ := h.Percentile(0.50)
	return d
}

// P90 returns the 90th percentile latency. Returns 0 if empty.
func (h *LatencyHistogram) P90() time.Duration {
	d, _ := h.Percentile(0.90)
	return d
}

// P99 returns the 99th percentile latency. Returns 0 if empty.
func (h *LatencyHistogram) P99() time.Duration {
	d, _ := h.Percentile(0.99)
	return d
}

// P999 returns the 99.9th percentile latency. Returns 0 if empty.
func (h *LatencyHistogram) P999() time.Duration {
	d, _ := h.Percentile(0.999)
	return d
}

// Reset clears all counters, buckets, and extrema, restoring the histogram
// to its initial empty state without performing any heap allocations.
func (h *LatencyHistogram) Reset() {
	*h = LatencyHistogram{}
}

// Snapshot returns an independent copy of h.
// Modifying h after Snapshot() does not affect the returned snapshot.
func (h *LatencyHistogram) Snapshot() *LatencyHistogram {
	cp := *h
	return &cp
}

// Merge combines the counts, buckets, and extrema from other into h.
// If other is nil, it returns ErrNilHistogram.
// If other is empty, Merge is a no-op.
// If h is empty, Merge copies other into h.
func (h *LatencyHistogram) Merge(other *LatencyHistogram) error {
	if other == nil {
		return ErrNilHistogram
	}
	if other.count == 0 {
		return nil
	}
	if h.count == 0 {
		*h = *other
		return nil
	}

	h.count += other.count
	if other.minNs < h.minNs {
		h.minNs = other.minNs
	}
	if other.maxNs > h.maxNs {
		h.maxNs = other.maxNs
	}

	if h.totalOverflown || other.totalOverflown || math.MaxUint64-h.totalNs < other.totalNs {
		h.totalNs = math.MaxUint64
		h.totalOverflown = true
	} else {
		h.totalNs += other.totalNs
	}

	for i := 0; i < totalBuckets; i++ {
		h.buckets[i] += other.buckets[i]
	}
	return nil
}

// valueToBucket maps a positive nanosecond latency to its bucket index in [0, totalBuckets-1].
func valueToBucket(ns int64) int {
	if ns <= 0 {
		return 0
	}
	if ns < subBucketCount {
		return int(ns)
	}
	u := uint64(ns)
	msb := 63 - bits.LeadingZeros64(u)
	if msb > maxOctave {
		return overflowBucketIndex
	}
	group := msb - subBucketBits + 1
	offset := (u >> (msb - subBucketBits)) & subBucketMask
	return group*subBucketCount + int(offset)
}

// bucketToUpper computes the upper boundary latency for regular bucket b.
// For bucket b in [0, 127], upper == b.
// For b in [128, numRegularBuckets-1], upper is the exact inclusive upper bound of bucket b.
// For b >= overflowBucketIndex, it returns maxTrackableNs + 1.
func bucketToUpper(b int) int64 {
	if b <= 0 {
		return 0
	}
	if b < subBucketCount {
		return int64(b)
	}
	if b >= overflowBucketIndex {
		return maxTrackableNs + 1
	}
	group := b / subBucketCount
	offset := b % subBucketCount
	msb := group + subBucketBits - 1
	shift := msb - subBucketBits
	lower := (int64(1) << msb) + (int64(offset) << shift)
	upper := lower + (int64(1) << shift) - 1
	return upper
}

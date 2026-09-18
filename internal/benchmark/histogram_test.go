package benchmark

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// TestHistogram_EmptyState verifies API behavior when no observations have been recorded.
func TestHistogram_EmptyState(t *testing.T) {
	h := NewLatencyHistogram()

	if h.Count() != 0 {
		t.Fatalf("expected count 0, got %d", h.Count())
	}
	if h.Min() != 0 {
		t.Fatalf("expected min 0, got %v", h.Min())
	}
	if h.Max() != 0 {
		t.Fatalf("expected max 0, got %v", h.Max())
	}
	if h.Mean() != 0 {
		t.Fatalf("expected mean 0, got %v", h.Mean())
	}
	if h.P50() != 0 {
		t.Fatalf("expected P50 0, got %v", h.P50())
	}
	if h.P90() != 0 {
		t.Fatalf("expected P90 0, got %v", h.P90())
	}
	if h.P99() != 0 {
		t.Fatalf("expected P99 0, got %v", h.P99())
	}
	if h.P999() != 0 {
		t.Fatalf("expected P999 0, got %v", h.P999())
	}

	for _, p := range []float64{0.0, 0.5, 0.99, 1.0} {
		val, err := h.Percentile(p)
		if !errors.Is(err, ErrEmptyHistogram) {
			t.Fatalf("expected ErrEmptyHistogram for p=%v, got err=%v val=%v", p, err, val)
		}
	}
}

// TestHistogram_InvalidPercentile verifies that invalid quantile requests are rejected.
func TestHistogram_InvalidPercentile(t *testing.T) {
	h := NewLatencyHistogram()
	h.Record(100 * time.Microsecond)

	invalidQuantiles := []float64{
		-0.0001, -1.0, 1.00001, 1.5, 2.0, math.NaN(), math.Inf(1), math.Inf(-1),
	}

	for _, p := range invalidQuantiles {
		val, err := h.Percentile(p)
		if !errors.Is(err, ErrInvalidPercentile) {
			t.Fatalf("expected ErrInvalidPercentile for p=%v, got val=%v err=%v", p, val, err)
		}
	}
}

// TestHistogram_SingleObservation verifies statistics for a single recorded value.
func TestHistogram_SingleObservation(t *testing.T) {
	h := NewLatencyHistogram()
	const target = 500 * time.Microsecond
	h.Record(target)

	if h.Count() != 1 {
		t.Fatalf("expected count 1, got %d", h.Count())
	}
	if h.Min() != target {
		t.Fatalf("expected min %v, got %v", target, h.Min())
	}
	if h.Max() != target {
		t.Fatalf("expected max %v, got %v", target, h.Max())
	}
	if h.Mean() != target {
		t.Fatalf("expected mean %v, got %v", target, h.Mean())
	}

	p0, err := h.Percentile(0.0)
	if err != nil || p0 != target {
		t.Fatalf("Percentile(0.0) = %v, err=%v; want %v", p0, err, target)
	}

	p1, err := h.Percentile(1.0)
	if err != nil || p1 != target {
		t.Fatalf("Percentile(1.0) = %v, err=%v; want %v", p1, err, target)
	}

	p50 := h.P50()
	if p50 < target {
		t.Fatalf("P50 (%v) < target (%v) violates conservative upper-bound SLA guarantee", p50, target)
	}
	relErr := float64(p50-target) / float64(target)
	if relErr > 1.0/float64(SubBucketCount) {
		t.Fatalf("relative error %f exceeds 1/%d bound", relErr, SubBucketCount)
	}
}

// TestHistogram_ZeroDuration verifies handling of zero-duration observations.
func TestHistogram_ZeroDuration(t *testing.T) {
	h := NewLatencyHistogram()
	h.Record(0)

	if h.Count() != 1 {
		t.Fatalf("expected count 1, got %d", h.Count())
	}
	if h.Min() != 0 {
		t.Fatalf("expected min 0, got %v", h.Min())
	}
	if h.Max() != 0 {
		t.Fatalf("expected max 0, got %v", h.Max())
	}
	if h.Mean() != 0 {
		t.Fatalf("expected mean 0, got %v", h.Mean())
	}

	p0, err := h.Percentile(0.0)
	if err != nil || p0 != 0 {
		t.Fatalf("Percentile(0.0) = %v, err=%v", p0, err)
	}
	p50, err := h.Percentile(0.5)
	if err != nil || p50 != 0 {
		t.Fatalf("Percentile(0.5) = %v, err=%v", p50, err)
	}
	p1, err := h.Percentile(1.0)
	if err != nil || p1 != 0 {
		t.Fatalf("Percentile(1.0) = %v, err=%v", p1, err)
	}
}

// TestHistogram_NegativeDuration_Dropped verifies that negative durations are safely discarded.
func TestHistogram_NegativeDuration_Dropped(t *testing.T) {
	h := NewLatencyHistogram()
	h.Record(-10 * time.Millisecond)
	h.RecordNano(-1)
	h.RecordNano(math.MinInt64)

	if h.Count() != 0 {
		t.Fatalf("expected count 0 after negative inputs, got %d", h.Count())
	}
	if h.Min() != 0 || h.Max() != 0 || h.Mean() != 0 {
		t.Fatalf("state corrupted by negative input: min=%v max=%v mean=%v", h.Min(), h.Max(), h.Mean())
	}

	// Recording valid data after negative data behaves normally
	h.Record(50 * time.Microsecond)
	if h.Count() != 1 || h.Min() != 50*time.Microsecond || h.Max() != 50*time.Microsecond {
		t.Fatalf("unexpected state after subsequent valid record: count=%d min=%v max=%v", h.Count(), h.Min(), h.Max())
	}
}

// TestHistogram_ExtremeAndOverflowDurations verifies safe handling of durations
// exceeding MaxTrackableLatency up to MaxInt64.
func TestHistogram_ExtremeAndOverflowDurations(t *testing.T) {
	h := NewLatencyHistogram()

	extremeValues := []int64{
		int64(MaxTrackableLatency) + 1,
		int64(10 * time.Hour),
		int64(100 * time.Hour),
		math.MaxInt64,
	}

	for _, v := range extremeValues {
		h.RecordNano(v)
	}

	if h.Count() != uint64(len(extremeValues)) {
		t.Fatalf("expected count %d, got %d", len(extremeValues), h.Count())
	}
	if h.Max() != time.Duration(math.MaxInt64) {
		t.Fatalf("expected exact max MaxInt64, got %v", h.Max())
	}

	p99 := h.P99()
	if p99 > h.Max() {
		t.Fatalf("P99 (%v) > Max (%v)", p99, h.Max())
	}
}

// TestHistogram_ExactExtremaAndMean verifies calculation of Min, Max, and Mean.
func TestHistogram_ExactExtremaAndMean(t *testing.T) {
	h := NewLatencyHistogram()
	samples := []time.Duration{
		100 * time.Nanosecond,
		200 * time.Nanosecond,
		300 * time.Nanosecond,
		400 * time.Nanosecond,
		500 * time.Nanosecond,
	}

	for _, s := range samples {
		h.Record(s)
	}

	if h.Min() != 100*time.Nanosecond {
		t.Fatalf("expected min 100ns, got %v", h.Min())
	}
	if h.Max() != 500*time.Nanosecond {
		t.Fatalf("expected max 500ns, got %v", h.Max())
	}
	if h.Mean() != 300*time.Nanosecond {
		t.Fatalf("expected mean 300ns, got %v", h.Mean())
	}
}

// TestHistogram_Reset verifies resetting the histogram to its clean empty state.
func TestHistogram_Reset(t *testing.T) {
	h := NewLatencyHistogram()
	for i := 1; i <= 1000; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}

	if h.Count() != 1000 {
		t.Fatalf("expected count 1000, got %d", h.Count())
	}

	h.Reset()

	if h.Count() != 0 || h.Min() != 0 || h.Max() != 0 || h.Mean() != 0 {
		t.Fatalf("reset failed to clear state: count=%d min=%v max=%v mean=%v", h.Count(), h.Min(), h.Max(), h.Mean())
	}

	// Verify new recording works after reset
	h.Record(42 * time.Microsecond)
	if h.Count() != 1 || h.Min() != 42*time.Microsecond || h.Max() != 42*time.Microsecond {
		t.Fatalf("recording after reset failed: count=%d min=%v max=%v", h.Count(), h.Min(), h.Max())
	}
}

// TestHistogram_Snapshot verifies that Snapshot returns an independent copy.
func TestHistogram_Snapshot(t *testing.T) {
	h := NewLatencyHistogram()
	for i := 1; i <= 100; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}

	snap := h.Snapshot()
	if snap.Count() != 100 {
		t.Fatalf("expected snapshot count 100, got %d", snap.Count())
	}

	// Mutate original
	for i := 101; i <= 200; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}

	if h.Count() != 200 {
		t.Fatalf("expected h count 200, got %d", h.Count())
	}
	if snap.Count() != 100 {
		t.Fatalf("snapshot mutated after original modified! count=%d", snap.Count())
	}
}

// TestHistogram_Merge verifies combining multiple histogram instances.
func TestHistogram_Merge(t *testing.T) {
	t.Run("NilArgument", func(t *testing.T) {
		h := NewLatencyHistogram()
		if err := h.Merge(nil); !errors.Is(err, ErrNilHistogram) {
			t.Fatalf("expected ErrNilHistogram, got %v", err)
		}
	})

	t.Run("EmptyIntoEmpty", func(t *testing.T) {
		h1 := NewLatencyHistogram()
		h2 := NewLatencyHistogram()
		if err := h1.Merge(h2); err != nil {
			t.Fatalf("unexpected merge error: %v", err)
		}
		if h1.Count() != 0 {
			t.Fatalf("expected count 0, got %d", h1.Count())
		}
	})

	t.Run("PopulatedIntoEmpty", func(t *testing.T) {
		h1 := NewLatencyHistogram()
		h2 := NewLatencyHistogram()
		h2.Record(10 * time.Millisecond)
		if err := h1.Merge(h2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h1.Count() != 1 || h1.Min() != 10*time.Millisecond || h1.Max() != 10*time.Millisecond {
			t.Fatalf("unexpected merged state: count=%d min=%v max=%v", h1.Count(), h1.Min(), h1.Max())
		}
	})

	t.Run("EmptyIntoPopulated", func(t *testing.T) {
		h1 := NewLatencyHistogram()
		h1.Record(10 * time.Millisecond)
		h2 := NewLatencyHistogram()
		if err := h1.Merge(h2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h1.Count() != 1 || h1.Min() != 10*time.Millisecond || h1.Max() != 10*time.Millisecond {
			t.Fatalf("unexpected merged state: count=%d min=%v max=%v", h1.Count(), h1.Min(), h1.Max())
		}
	})

	t.Run("MultiWorkerAggregationVsSequential", func(t *testing.T) {
		const numWorkers = 16
		const samplesPerWorker = 5000
		totalSamples := numWorkers * samplesPerWorker

		workers := make([]*LatencyHistogram, numWorkers)
		for w := 0; w < numWorkers; w++ {
			workers[w] = NewLatencyHistogram()
		}

		sequential := NewLatencyHistogram()

		rng := rand.New(rand.NewSource(12345))
		for i := 0; i < totalSamples; i++ {
			d := time.Duration(rng.Int63n(10_000_000_000)) // 0 to 10 seconds
			w := i % numWorkers
			workers[w].Record(d)
			sequential.Record(d)
		}

		aggregate := NewLatencyHistogram()
		for _, w := range workers {
			if err := aggregate.Merge(w); err != nil {
				t.Fatalf("merge failed: %v", err)
			}
		}

		if aggregate.Count() != sequential.Count() {
			t.Fatalf("count mismatch: aggregate=%d sequential=%d", aggregate.Count(), sequential.Count())
		}
		if aggregate.Min() != sequential.Min() {
			t.Fatalf("min mismatch: aggregate=%v sequential=%v", aggregate.Min(), sequential.Min())
		}
		if aggregate.Max() != sequential.Max() {
			t.Fatalf("max mismatch: aggregate=%v sequential=%v", aggregate.Max(), sequential.Max())
		}
		if aggregate.Mean() != sequential.Mean() {
			t.Fatalf("mean mismatch: aggregate=%v sequential=%v", aggregate.Mean(), sequential.Mean())
		}
		if aggregate.P50() != sequential.P50() {
			t.Fatalf("P50 mismatch: aggregate=%v sequential=%v", aggregate.P50(), sequential.P50())
		}
		if aggregate.P90() != sequential.P90() {
			t.Fatalf("P90 mismatch: aggregate=%v sequential=%v", aggregate.P90(), sequential.P90())
		}
		if aggregate.P99() != sequential.P99() {
			t.Fatalf("P99 mismatch: aggregate=%v sequential=%v", aggregate.P99(), sequential.P99())
		}
		if aggregate.P999() != sequential.P999() {
			t.Fatalf("P999 mismatch: aggregate=%v sequential=%v", aggregate.P999(), sequential.P999())
		}
	})
}

// TestHistogram_DifferentialReference validates histogram percentiles against an
// exact sorted-slice reference across various sample sizes and distributions.
func TestHistogram_DifferentialReference(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	testSizes := []int{100, 1000, 10000, 50000}
	quantiles := []float64{0.01, 0.05, 0.10, 0.50, 0.90, 0.95, 0.99, 0.999}

	maxAllowedRelativeError := 1.0 / float64(SubBucketCount) // 1/128 = 0.0078125 <= 0.782%

	for _, size := range testSizes {
		h := NewLatencyHistogram()
		raw := make([]int64, size)

		for i := 0; i < size; i++ {
			var ns int64
			r := rng.Float64()
			switch {
			case r < 0.50:
				// Fast in-memory operations: 100ns to 50us
				ns = int64(100 + rng.Intn(50000))
			case r < 0.90:
				// Disk/WAL operations: 50us to 10ms
				ns = int64(50000 + rng.Intn(10000000))
			default:
				// Compaction stalls & GC outliers: 10ms to 5s
				ns = int64(10000000 + rng.Intn(5000000000))
			}
			raw[i] = ns
			h.RecordNano(ns)
		}

		sort.Slice(raw, func(i, j int) bool { return raw[i] < raw[j] })

		// Assert exact Min and Max
		if h.Min() != time.Duration(raw[0]) {
			t.Fatalf("size %d: min mismatch: h=%v raw[0]=%v", size, h.Min(), raw[0])
		}
		if h.Max() != time.Duration(raw[size-1]) {
			t.Fatalf("size %d: max mismatch: h=%v raw[-1]=%v", size, h.Max(), raw[size-1])
		}

		for _, q := range quantiles {
			hp, err := h.Percentile(q)
			if err != nil {
				t.Fatalf("size %d, q %v: unexpected error: %v", size, q, err)
			}

			rank := int(math.Ceil(q * float64(size)))
			if rank < 1 {
				rank = 1
			}
			if rank > size {
				rank = size
			}
			exactRef := time.Duration(raw[rank-1])

			// 1. Conservative SLA guarantee: hp >= exactRef
			if hp < exactRef {
				t.Fatalf("size %d, q %v: SLA violated! hp (%v) < exactRef (%v)", size, q, hp, exactRef)
			}

			// 2. Maximum relative quantization error bound <= 1/128
			if exactRef > 0 {
				relErr := float64(hp-exactRef) / float64(exactRef)
				if relErr > maxAllowedRelativeError+1e-9 {
					t.Fatalf("size %d, q %v: relative error %f exceeds bound %f (hp=%v exactRef=%v)",
						size, q, relErr, maxAllowedRelativeError, hp, exactRef)
				}
			}
		}
	}
}

// TestHistogram_BoundaryValues validates bucket transitions around powers of two
// and sub-bucket boundaries.
func TestHistogram_BoundaryValues(t *testing.T) {
	h := NewLatencyHistogram()

	testBoundaries := []int64{
		0, 1, 2, 3, 126, 127, 128, 129, 255, 256, 257,
		511, 512, 513, 1023, 1024, 1025,
		1_000_000, 1_000_001,
		1_000_000_000, 1_000_000_001,
		int64(MaxTrackableLatency) - 1,
		int64(MaxTrackableLatency),
		int64(MaxTrackableLatency) + 1,
	}

	for _, v := range testBoundaries {
		h.RecordNano(v)
	}

	if h.Count() != uint64(len(testBoundaries)) {
		t.Fatalf("expected count %d, got %d", len(testBoundaries), h.Count())
	}

	// Verify monotonic quantile ordering
	p50 := h.P50()
	p90 := h.P90()
	p99 := h.P99()
	p999 := h.P999()

	if !(h.Min() <= p50 && p50 <= p90 && p90 <= p99 && p99 <= p999 && p999 <= h.Max()) {
		t.Fatalf("monotonicity invariant violated: Min=%v P50=%v P90=%v P99=%v P99.9=%v Max=%v",
			h.Min(), p50, p90, p99, p999, h.Max())
	}
}

// TestHistogram_P999TailResolution verifies that the 99.9th percentile outlier is accurately
// captured in the presence of 99.9% fast operations.
func TestHistogram_P999TailResolution(t *testing.T) {
	h := NewLatencyHistogram()
	const total = 100_000
	const fastLatency = 200 * time.Microsecond
	const tailLatency = 50 * time.Millisecond

	// 99,850 fast operations (99.85%)
	for i := 0; i < 99_850; i++ {
		h.Record(fastLatency)
	}

	// 150 tail operations (0.15%)
	for i := 0; i < 150; i++ {
		h.Record(tailLatency)
	}

	p50 := h.P50()
	p90 := h.P90()
	p99 := h.P99()
	p999 := h.P999()

	// P50, P90, P99 must reflect the fast operations
	if p50 < fastLatency || p50 > fastLatency+5*time.Microsecond {
		t.Fatalf("unexpected P50: %v (want ~%v)", p50, fastLatency)
	}
	if p90 < fastLatency || p90 > fastLatency+5*time.Microsecond {
		t.Fatalf("unexpected P90: %v (want ~%v)", p90, fastLatency)
	}
	if p99 < fastLatency || p99 > fastLatency+5*time.Microsecond {
		t.Fatalf("unexpected P99: %v (want ~%v)", p99, fastLatency)
	}

	// P99.9 must transition into the tail
	if p999 < tailLatency {
		t.Fatalf("P99.9 (%v) failed to capture tail latency (%v)", p999, tailLatency)
	}
	relErr := float64(p999-tailLatency) / float64(tailLatency)
	if relErr > 1.0/float64(SubBucketCount) {
		t.Fatalf("P99.9 relative error %f exceeds bound", relErr)
	}
}

// TestHistogram_ConcurrentWorkersWithMerge simulates the intended M03 concurrency model:
// 32 goroutines recording into independent per-worker histograms without locks,
// then aggregating via Merge.
func TestHistogram_ConcurrentWorkersWithMerge(t *testing.T) {
	const workers = 32
	const samplesPerWorker = 5000
	var wg sync.WaitGroup

	workerHists := make([]*LatencyHistogram, workers)

	for w := 0; w < workers; w++ {
		workerHists[w] = NewLatencyHistogram()
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			hist := workerHists[workerID]
			rng := rand.New(rand.NewSource(int64(workerID*10007 + 42)))
			for i := 0; i < samplesPerWorker; i++ {
				d := time.Duration(rng.Int63n(1_000_000_000))
				hist.Record(d)
			}
		}(w)
	}

	wg.Wait()

	aggregate := NewLatencyHistogram()
	for _, wh := range workerHists {
		if err := aggregate.Merge(wh); err != nil {
			t.Fatalf("merge failed: %v", err)
		}
	}

	if aggregate.Count() != workers*samplesPerWorker {
		t.Fatalf("expected count %d, got %d", workers*samplesPerWorker, aggregate.Count())
	}
}

// TestHistogram_StructSize verifies the memory footprint of LatencyHistogram.
func TestHistogram_StructSize(t *testing.T) {
	size := unsafe.Sizeof(LatencyHistogram{})
	t.Logf("LatencyHistogram struct size: %d bytes (%.2f KB)", size, float64(size)/1024.0)

	// Bounded memory guarantee: must be strictly <= 64 KB
	const maxPermittedSize = 64 * 1024
	if size > maxPermittedSize {
		t.Fatalf("struct size %d exceeds maximum permitted %d bytes", size, maxPermittedSize)
	}
}

// FuzzHistogramRecord fuzzes latency recording with arbitrary integer values.
func FuzzHistogramRecord(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(128))
	f.Add(int64(1000000))
	f.Add(int64(-10))
	f.Add(int64(-1))
	f.Add(int64(math.MaxInt64))
	f.Add(int64(math.MinInt64))

	f.Fuzz(func(t *testing.T, val int64) {
		h := NewLatencyHistogram()
		h.RecordNano(val)

		if val < 0 {
			if h.Count() != 0 {
				t.Fatalf("negative val %d recorded! count=%d", val, h.Count())
			}
			return
		}

		if h.Count() != 1 {
			t.Fatalf("expected count 1 for val %d, got %d", val, h.Count())
		}
		if h.Min() != time.Duration(val) || h.Max() != time.Duration(val) {
			t.Fatalf("extrema mismatch for val %d: min=%v max=%v", val, h.Min(), h.Max())
		}

		p50 := h.P50()
		p99 := h.P99()
		if p50 > p99 {
			t.Fatalf("monotonicity violated: P50=%v > P99=%v", p50, p99)
		}
	})
}

// TestHistogram_Mean_NormalExact verifies that Mean() provides exact integer arithmetic
// for un-overflowed workloads.
func TestHistogram_Mean_NormalExact(t *testing.T) {
	h := NewLatencyHistogram()
	samples := []int64{100, 250, 400, 850, 1200}
	var total int64
	for _, s := range samples {
		h.RecordNano(s)
		total += s
	}

	expectedMean := time.Duration(total / int64(len(samples)))
	if h.Mean() != expectedMean {
		t.Fatalf("expected exact mean %v, got %v", expectedMean, h.Mean())
	}
	if h.totalOverflown {
		t.Fatalf("totalOverflown unexpectedly true")
	}
}

// TestHistogram_Mean_NearOverflow verifies behavior when cumulative total is near math.MaxUint64.
func TestHistogram_Mean_NearOverflow(t *testing.T) {
	h := NewLatencyHistogram()

	// 2 * MaxInt64 = 2 * (2^63 - 1) = 2^64 - 2 = MaxUint64 - 1
	h.RecordNano(math.MaxInt64)
	h.RecordNano(math.MaxInt64)

	if h.Count() != 2 {
		t.Fatalf("expected count 2, got %d", h.Count())
	}
	if h.totalOverflown {
		t.Fatalf("totalOverflown unexpectedly true at MaxUint64 - 1")
	}
	// Exact division: (2^64 - 2) / 2 = 2^63 - 1 = MaxInt64
	if h.Mean() != time.Duration(math.MaxInt64) {
		t.Fatalf("expected exact mean MaxInt64, got %v", h.Mean())
	}
}

// TestHistogram_Mean_OverflowTriggering verifies deterministic, non-wrapping behavior
// when cumulative total crosses math.MaxUint64.
func TestHistogram_Mean_OverflowTriggering(t *testing.T) {
	h := NewLatencyHistogram()

	// 2 * MaxInt64 = MaxUint64 - 1
	h.RecordNano(math.MaxInt64)
	h.RecordNano(math.MaxInt64)
	// Third record triggers totalNs overflow
	h.RecordNano(2)

	if h.Count() != 3 {
		t.Fatalf("expected count 3, got %d", h.Count())
	}
	if !h.totalOverflown {
		t.Fatalf("expected totalOverflown true after crossing MaxUint64")
	}
	if h.totalNs != math.MaxUint64 {
		t.Fatalf("expected totalNs saturated at MaxUint64, got %d", h.totalNs)
	}

	mean := h.Mean()
	if mean <= 0 {
		t.Fatalf("mean wrapped to non-positive value: %v", mean)
	}
	if mean > h.Max() {
		t.Fatalf("mean %v exceeds max %v", mean, h.Max())
	}

	// Verify determinism across multiple queries
	mean2 := h.Mean()
	if mean != mean2 {
		t.Fatalf("non-deterministic mean: %v vs %v", mean, mean2)
	}
}

// TestHistogram_Mean_MixedOverflowBucket verifies SEC-P13-M02-001:
// Distinct values in the overflow bucket (5h, 10h, 50h) are not all treated as equal to maxNs.
func TestHistogram_Mean_MixedOverflowBucket(t *testing.T) {
	h := NewLatencyHistogram()

	v1 := 5 * time.Hour
	v2 := 10 * time.Hour
	v3 := 50 * time.Hour

	h.Record(v1)
	h.Record(v2)
	h.Record(v3)

	if h.buckets[overflowBucketIndex] != 3 {
		t.Fatalf("expected 3 observations in overflow bucket, got %d", h.buckets[overflowBucketIndex])
	}
	if h.Max() != v3 {
		t.Fatalf("expected Max 50h, got %v", h.Max())
	}

	// In the normal path, mean is exact: (5 + 10 + 50) / 3 = 65h / 3 = 21h40m
	expectedExactMean := (v1 + v2 + v3) / 3
	if h.Mean() != expectedExactMean {
		t.Fatalf("normal path: expected %v, got %v", expectedExactMean, h.Mean())
	}

	// Now force fallback path by setting totalOverflown = true
	h.totalOverflown = true

	meanFallback := h.Mean()

	// The old defect treated all 3 observations as maxNs (50h), returning 50h.
	// The remediated implementation uses overflowTotalNs, returning exact 21h40m.
	if meanFallback == v3 {
		t.Fatalf("SEC-P13-M02-001 defect reproduced: Mean fallback treated all overflow observations as maxNs (%v)", v3)
	}
	if meanFallback != expectedExactMean {
		t.Fatalf("fallback with preserved overflowTotalNs: expected %v, got %v", expectedExactMean, meanFallback)
	}

	// Also test extreme fallback when overflowTotalNs is unavailable (e.g. overflowOverflown = true)
	h.overflowOverflown = true
	extremeMean := h.Mean()
	if extremeMean == v3 {
		t.Fatalf("extreme fallback treated all observations as maxNs (%v)", v3)
	}
	if extremeMean < v1 || extremeMean > v3 {
		t.Fatalf("extreme fallback %v outside [%v, %v]", extremeMean, v1, v3)
	}
}

// TestHistogram_Mean_RegularBucketOverflowBounded verifies that when totalOverflown is true,
// regular bucket midpoint estimation has bounded relative error <= 1/256 (~0.39%).
func TestHistogram_Mean_RegularBucketOverflowBounded(t *testing.T) {
	h := NewLatencyHistogram()

	// Record values in a regular octave (e.g. 1ms to 2ms)
	var sum time.Duration
	const count = 1000
	for i := 1; i <= count; i++ {
		d := time.Duration(1_000_000+i*1000) * time.Nanosecond
		h.Record(d)
		sum += d
	}
	exactMean := sum / count

	// Force fallback
	h.totalOverflown = true
	approxMean := h.Mean()

	relErr := math.Abs(float64(approxMean-exactMean)) / float64(exactMean)
	// Regular bucket relative error bound is <= 1/256 = 0.00390625 (~0.39%)
	const maxAllowedRelErr = 1.0 / 256.0
	if relErr > maxAllowedRelErr {
		t.Fatalf("regular bucket fallback relative error %f exceeds bound %f (approx=%v exact=%v)",
			relErr, maxAllowedRelErr, approxMean, exactMean)
	}
}

// TestHistogram_Merge_SaturatedCount verifies SEC-P13-M02-002:
// Merging near math.MaxUint64 saturates count and sets countOverflown rather than wrapping.
func TestHistogram_Merge_SaturatedCount(t *testing.T) {
	h1 := NewLatencyHistogram()
	h2 := NewLatencyHistogram()

	h1.count = math.MaxUint64 - 5
	h1.buckets[10] = math.MaxUint64 - 5
	h2.count = 10
	h2.buckets[10] = 10

	if err := h1.Merge(h2); err != nil {
		t.Fatalf("unexpected merge error: %v", err)
	}

	if h1.Count() != math.MaxUint64 {
		t.Fatalf("expected count saturated at MaxUint64, got %d", h1.Count())
	}
	if !h1.countOverflown {
		t.Fatalf("expected countOverflown true")
	}
	if h1.buckets[10] != math.MaxUint64 {
		t.Fatalf("expected bucket[10] saturated at MaxUint64, got %d", h1.buckets[10])
	}
}

// TestHistogram_Merge_SaturatedBucket verifies SEC-P13-M02-002:
// Merging near math.MaxUint64 saturates individual bucket counts rather than wrapping.
func TestHistogram_Merge_SaturatedBucket(t *testing.T) {
	h1 := NewLatencyHistogram()
	h2 := NewLatencyHistogram()

	h1.count = 100
	h1.buckets[200] = math.MaxUint64 - 3
	h2.count = 100
	h2.buckets[200] = 5

	if err := h1.Merge(h2); err != nil {
		t.Fatalf("unexpected merge error: %v", err)
	}

	if h1.buckets[200] != math.MaxUint64 {
		t.Fatalf("expected bucket[200] saturated at MaxUint64, got %d", h1.buckets[200])
	}
}

// TestHistogram_Merge_RepeatedMergesNoWrap verifies SEC-P13-M02-002:
// Repeated merges on saturated counters never wrap back toward zero.
func TestHistogram_Merge_RepeatedMergesNoWrap(t *testing.T) {
	h1 := NewLatencyHistogram()
	h1.count = math.MaxUint64
	h1.countOverflown = true
	h1.buckets[42] = math.MaxUint64

	h2 := NewLatencyHistogram()
	h2.count = 1000
	h2.buckets[42] = 1000

	for i := 0; i < 10; i++ {
		if err := h1.Merge(h2); err != nil {
			t.Fatalf("iteration %d: merge failed: %v", i, err)
		}
		if h1.Count() != math.MaxUint64 {
			t.Fatalf("iteration %d: count wrapped! got %d", i, h1.Count())
		}
		if h1.buckets[42] != math.MaxUint64 {
			t.Fatalf("iteration %d: bucket wrapped! got %d", i, h1.buckets[42])
		}
	}
}

// TestHistogram_Merge_MeanInteraction verifies that merging saturated or overflown histograms
// produces consistent, bounded, non-negative Mean statistics.
func TestHistogram_Merge_MeanInteraction(t *testing.T) {
	h1 := NewLatencyHistogram()
	h1.count = 500
	h1.totalNs = math.MaxUint64
	h1.totalOverflown = true
	h1.minNs = int64(100 * time.Microsecond)
	h1.maxNs = int64(500 * time.Millisecond)
	h1.buckets[100] = 500

	h2 := NewLatencyHistogram()
	h2.Record(10 * time.Millisecond)

	if err := h1.Merge(h2); err != nil {
		t.Fatalf("merge failed: %v", err)
	}

	if !h1.totalOverflown {
		t.Fatalf("expected totalOverflown true after merge")
	}

	mean := h1.Mean()
	if mean <= 0 {
		t.Fatalf("expected positive mean after merge, got %v", mean)
	}
	if mean > h1.Max() {
		t.Fatalf("mean %v > max %v", mean, h1.Max())
	}
}

// TestHistogram_Merge_CommutativityAndAssociativity verifies algebraic consistency
// across both normal and saturated operations.
func TestHistogram_Merge_CommutativityAndAssociativity(t *testing.T) {
	makeHist := func(seed int64) *LatencyHistogram {
		h := NewLatencyHistogram()
		rng := rand.New(rand.NewSource(seed))
		for i := 0; i < 500; i++ {
			h.Record(time.Duration(rng.Int63n(100_000_000)) * time.Nanosecond)
		}
		return h
	}

	// Commutativity: A + B == B + A
	{
		hA1 := makeHist(1)
		hB1 := makeHist(2)
		hA2 := makeHist(1)
		hB2 := makeHist(2)

		_ = hA1.Merge(hB1)
		_ = hB2.Merge(hA2)

		if hA1.Count() != hB2.Count() || hA1.Min() != hB2.Min() || hA1.Max() != hB2.Max() || hA1.Mean() != hB2.Mean() {
			t.Fatalf("commutativity failed: A+B != B+A")
		}
		if hA1.P50() != hB2.P50() || hA1.P90() != hB2.P90() || hA1.P99() != hB2.P99() {
			t.Fatalf("percentiles non-commutative: A+B != B+A")
		}
	}

	// Associativity: (A + B) + C == A + (B + C)
	{
		hA1 := makeHist(10)
		hB1 := makeHist(20)
		hC1 := makeHist(30)

		hA2 := makeHist(10)
		hB2 := makeHist(20)
		hC2 := makeHist(30)

		// (A + B) + C
		_ = hA1.Merge(hB1)
		_ = hA1.Merge(hC1)

		// A + (B + C)
		_ = hB2.Merge(hC2)
		_ = hA2.Merge(hB2)

		if hA1.Count() != hA2.Count() || hA1.Min() != hA2.Min() || hA1.Max() != hA2.Max() || hA1.Mean() != hA2.Mean() {
			t.Fatalf("associativity failed: (A+B)+C != A+(B+C)")
		}
		if hA1.P50() != hA2.P50() || hA1.P99() != hA2.P99() {
			t.Fatalf("percentiles non-associative: (A+B)+C != A+(B+C)")
		}
	}
}

// TestHistogram_Reset_ClearsAllOverflowState verifies Reset clears all overflow flags and counters.
func TestHistogram_Reset_ClearsAllOverflowState(t *testing.T) {
	h := NewLatencyHistogram()
	h.count = math.MaxUint64
	h.totalNs = math.MaxUint64
	h.overflowTotalNs = 12345
	h.totalOverflown = true
	h.countOverflown = true
	h.overflowOverflown = true

	h.Reset()

	if h.Count() != 0 || h.totalNs != 0 || h.overflowTotalNs != 0 {
		t.Fatalf("Reset did not clear counters: count=%d totalNs=%d overflowTotalNs=%d",
			h.Count(), h.totalNs, h.overflowTotalNs)
	}
	if h.totalOverflown || h.countOverflown || h.overflowOverflown {
		t.Fatalf("Reset did not clear overflow flags")
	}

	// Normal recording after reset works exactly
	h.Record(500 * time.Microsecond)
	if h.Count() != 1 || h.Mean() != 500*time.Microsecond || h.totalOverflown {
		t.Fatalf("recording after reset failed: count=%d mean=%v totalOverflown=%v",
			h.Count(), h.Mean(), h.totalOverflown)
	}
}

// TestHistogram_Snapshot_PreservesAllOverflowState verifies Snapshot copies all overflow flags and counters.
func TestHistogram_Snapshot_PreservesAllOverflowState(t *testing.T) {
	h := NewLatencyHistogram()
	h.count = 100
	h.totalOverflown = true
	h.countOverflown = true
	h.overflowOverflown = true
	h.overflowTotalNs = 99999

	snap := h.Snapshot()
	if !snap.totalOverflown || !snap.countOverflown || !snap.overflowOverflown || snap.overflowTotalNs != 99999 {
		t.Fatalf("Snapshot failed to preserve overflow fields: %+v", snap)
	}

	// Mutating original does not mutate snapshot
	h.totalOverflown = false
	h.overflowTotalNs = 0
	if !snap.totalOverflown || snap.overflowTotalNs != 99999 {
		t.Fatalf("Snapshot mutated after original modified")
	}
}

// FuzzHistogramMerge fuzzes combining two histograms with arbitrary latency distributions.
func FuzzHistogramMerge(f *testing.F) {
	f.Add(int64(0), int64(100), int64(1000), int64(50000))
	f.Add(int64(-5), int64(0), int64(math.MaxInt64), int64(1))
	f.Add(int64(128), int64(256), int64(1024), int64(4096))

	f.Fuzz(func(t *testing.T, a1, a2, b1, b2 int64) {
		h1 := NewLatencyHistogram()
		h2 := NewLatencyHistogram()

		h1.RecordNano(a1)
		h1.RecordNano(a2)
		h2.RecordNano(b1)
		h2.RecordNano(b2)

		initialCount1 := h1.Count()
		initialCount2 := h2.Count()

		if err := h1.Merge(h2); err != nil {
			t.Fatalf("merge failed: %v", err)
		}

		if h1.Count() != initialCount1+initialCount2 {
			t.Fatalf("count mismatch: expected %d, got %d", initialCount1+initialCount2, h1.Count())
		}

		if h1.Count() > 0 {
			if h1.Min() > h1.Max() {
				t.Fatalf("min %v > max %v", h1.Min(), h1.Max())
			}
			p50 := h1.P50()
			p99 := h1.P99()
			if p50 > p99 {
				t.Fatalf("monotonicity violated: P50=%v > P99=%v", p50, p99)
			}
			if h1.Mean() < 0 {
				t.Fatalf("negative mean: %v", h1.Mean())
			}
		}
	})
}

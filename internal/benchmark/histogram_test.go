package benchmark_test

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

// TestHistogram_EmptyState verifies API behavior when no observations have been recorded.
func TestHistogram_EmptyState(t *testing.T) {
	h := benchmark.NewLatencyHistogram()

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
		if !errors.Is(err, benchmark.ErrEmptyHistogram) {
			t.Fatalf("expected ErrEmptyHistogram for p=%v, got err=%v val=%v", p, err, val)
		}
	}
}

// TestHistogram_InvalidPercentile verifies that invalid quantile requests are rejected.
func TestHistogram_InvalidPercentile(t *testing.T) {
	h := benchmark.NewLatencyHistogram()
	h.Record(100 * time.Microsecond)

	invalidQuantiles := []float64{
		-0.0001, -1.0, 1.00001, 1.5, 2.0, math.NaN(), math.Inf(1), math.Inf(-1),
	}

	for _, p := range invalidQuantiles {
		val, err := h.Percentile(p)
		if !errors.Is(err, benchmark.ErrInvalidPercentile) {
			t.Fatalf("expected ErrInvalidPercentile for p=%v, got val=%v err=%v", p, val, err)
		}
	}
}

// TestHistogram_SingleObservation verifies statistics for a single recorded value.
func TestHistogram_SingleObservation(t *testing.T) {
	h := benchmark.NewLatencyHistogram()
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
	if relErr > 1.0/float64(benchmark.SubBucketCount) {
		t.Fatalf("relative error %f exceeds 1/%d bound", relErr, benchmark.SubBucketCount)
	}
}

// TestHistogram_ZeroDuration verifies handling of zero-duration observations.
func TestHistogram_ZeroDuration(t *testing.T) {
	h := benchmark.NewLatencyHistogram()
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
	h := benchmark.NewLatencyHistogram()
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
	h := benchmark.NewLatencyHistogram()

	extremeValues := []int64{
		int64(benchmark.MaxTrackableLatency) + 1,
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
	h := benchmark.NewLatencyHistogram()
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
	h := benchmark.NewLatencyHistogram()
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
	h := benchmark.NewLatencyHistogram()
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
		h := benchmark.NewLatencyHistogram()
		if err := h.Merge(nil); !errors.Is(err, benchmark.ErrNilHistogram) {
			t.Fatalf("expected ErrNilHistogram, got %v", err)
		}
	})

	t.Run("EmptyIntoEmpty", func(t *testing.T) {
		h1 := benchmark.NewLatencyHistogram()
		h2 := benchmark.NewLatencyHistogram()
		if err := h1.Merge(h2); err != nil {
			t.Fatalf("unexpected merge error: %v", err)
		}
		if h1.Count() != 0 {
			t.Fatalf("expected count 0, got %d", h1.Count())
		}
	})

	t.Run("PopulatedIntoEmpty", func(t *testing.T) {
		h1 := benchmark.NewLatencyHistogram()
		h2 := benchmark.NewLatencyHistogram()
		h2.Record(10 * time.Millisecond)
		if err := h1.Merge(h2); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h1.Count() != 1 || h1.Min() != 10*time.Millisecond || h1.Max() != 10*time.Millisecond {
			t.Fatalf("unexpected merged state: count=%d min=%v max=%v", h1.Count(), h1.Min(), h1.Max())
		}
	})

	t.Run("EmptyIntoPopulated", func(t *testing.T) {
		h1 := benchmark.NewLatencyHistogram()
		h1.Record(10 * time.Millisecond)
		h2 := benchmark.NewLatencyHistogram()
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

		workers := make([]*benchmark.LatencyHistogram, numWorkers)
		for w := 0; w < numWorkers; w++ {
			workers[w] = benchmark.NewLatencyHistogram()
		}

		sequential := benchmark.NewLatencyHistogram()

		rng := rand.New(rand.NewSource(12345))
		for i := 0; i < totalSamples; i++ {
			d := time.Duration(rng.Int63n(10_000_000_000)) // 0 to 10 seconds
			w := i % numWorkers
			workers[w].Record(d)
			sequential.Record(d)
		}

		aggregate := benchmark.NewLatencyHistogram()
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

	maxAllowedRelativeError := 1.0 / float64(benchmark.SubBucketCount) // 1/128 = 0.0078125 <= 0.782%

	for _, size := range testSizes {
		h := benchmark.NewLatencyHistogram()
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
	h := benchmark.NewLatencyHistogram()

	testBoundaries := []int64{
		0, 1, 2, 3, 126, 127, 128, 129, 255, 256, 257,
		511, 512, 513, 1023, 1024, 1025,
		1_000_000, 1_000_001,
		1_000_000_000, 1_000_000_001,
		int64(benchmark.MaxTrackableLatency) - 1,
		int64(benchmark.MaxTrackableLatency),
		int64(benchmark.MaxTrackableLatency) + 1,
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
	h := benchmark.NewLatencyHistogram()
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
	if relErr > 1.0/float64(benchmark.SubBucketCount) {
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

	workerHists := make([]*benchmark.LatencyHistogram, workers)

	for w := 0; w < workers; w++ {
		workerHists[w] = benchmark.NewLatencyHistogram()
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

	aggregate := benchmark.NewLatencyHistogram()
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
	size := unsafe.Sizeof(benchmark.LatencyHistogram{})
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
		h := benchmark.NewLatencyHistogram()
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

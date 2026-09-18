package benchmark_test

import (
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

// BenchmarkHistogram_Record measures the latency and allocations of Record(time.Duration).
func BenchmarkHistogram_Record(b *testing.B) {
	h := benchmark.NewLatencyHistogram()
	d := 250 * time.Microsecond

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		h.Record(d)
	}
}

// BenchmarkHistogram_RecordNano measures the latency and allocations of RecordNano(int64).
func BenchmarkHistogram_RecordNano(b *testing.B) {
	h := benchmark.NewLatencyHistogram()
	const ns = 250_000

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		h.RecordNano(ns)
	}
}

// BenchmarkHistogram_Percentile measures query throughput across a populated histogram.
func BenchmarkHistogram_Percentile(b *testing.B) {
	h := benchmark.NewLatencyHistogram()
	for i := 1; i <= 100_000; i++ {
		h.Record(time.Duration(i*100) * time.Nanosecond)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = h.P99()
	}
}

// BenchmarkHistogram_Merge measures aggregation throughput between two populated histograms.
func BenchmarkHistogram_Merge(b *testing.B) {
	h1 := benchmark.NewLatencyHistogram()
	h2 := benchmark.NewLatencyHistogram()
	for i := 1; i <= 50_000; i++ {
		h1.Record(time.Duration(i*100) * time.Nanosecond)
		h2.Record(time.Duration(i*200) * time.Nanosecond)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = h1.Merge(h2)
	}
}

// BenchmarkHistogram_Reset measures the overhead of clearing histogram state between benchmark phases.
func BenchmarkHistogram_Reset(b *testing.B) {
	h := benchmark.NewLatencyHistogram()
	for i := 1; i <= 10_000; i++ {
		h.Record(time.Duration(i*100) * time.Nanosecond)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		h.Reset()
	}
}

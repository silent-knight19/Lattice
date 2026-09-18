// Package benchmark provides workload generation primitives, key distribution
// samplers, high-resolution latency tracking, and client load generation
// harnesses for the Lattice storage engine.
//
// Key Distribution Semantics:
// Workload generators such as ZipfGenerator model skewed key access
// patterns following a finite discrete Zipfian (power-law) distribution:
//
//	P(rank = r) = r^(-theta) / H(N, theta)
//
// where theta in (0.0, 1.0) controls skew and H(N, theta) is the generalized
// harmonic number normalizing over the finite keyspace N items.
//
// Asymptotic vs. Finite-Domain 80/20 Concentration:
// Under canonical skew theta = 0.99, the distribution exhibits strong non-uniformity
// modeling access hotspots. For large keyspaces (N >= 1,000), the top 20% of ranks
// receive ~75-80% of operations, approximating the Pareto 80/20 rule. In smaller
// keyspaces, however, discrete harmonic normalization concentrates less mass in the
// top 20% (e.g. ~51% at N=10, ~65% at N=50, ~69% at N=100). Benchmark harnesses
// must compute theoretical expectations from the finite normalized distribution rather
// than inferring a fixed universal cache-hit ratio solely from theta = 0.99.
//
// Latency Tracking & Histogram Semantics:
// Latency tracking is provided by LatencyHistogram, a high-resolution, logarithmic
// sub-bucket histogram designed for benchmark performance profiling without GC distortion.
// Latency values are partitioned into power-of-two octaves with 128 linear sub-buckets per
// octave, maintaining a provable relative quantization error <= 1/128 (0.78125% < 1.0%)
// over a dynamic range spanning 1 ns to ~4.88 hours. Recording executes in O(1) time
// with 0 heap allocations.
//
// Concurrency Model:
// Workload generators (ZipfGenerator) and latency collectors (LatencyHistogram) in this package
// are single-consumer primitives intentionally designed without internal locking.
// To achieve maximum throughput and avoid mutex contention on the benchmark hot path,
// multi-threaded benchmark runners must instantiate one generator and one histogram per
// concurrent worker goroutine. Worker histograms are aggregated post-run via Merge.
package benchmark

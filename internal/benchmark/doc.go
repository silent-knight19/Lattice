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
// Concurrency Model:
// Unless explicitly documented otherwise, workload generators in this package
// (including ZipfGenerator) are single-consumer primitives backed by isolated,
// non-thread-safe PRNG sources. To achieve maximum throughput and avoid mutex
// contention on the workload generation hot path, multi-threaded benchmark
// runners must instantiate one generator per concurrent worker goroutine.
package benchmark

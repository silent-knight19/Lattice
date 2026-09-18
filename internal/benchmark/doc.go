// Package benchmark provides workload generation primitives, key distribution
// samplers, high-resolution latency tracking, and client load generation
// harnesses for the Lattice storage engine.
//
// Concurrency Model:
// Unless explicitly documented otherwise, workload generators in this package
// (including ZipfGenerator) are single-consumer primitives backed by isolated,
// non-thread-safe PRNG sources. To achieve maximum throughput and avoid mutex
// contention on the workload generation hot path, multi-threaded benchmark
// runners must instantiate one generator per concurrent worker goroutine.
package benchmark

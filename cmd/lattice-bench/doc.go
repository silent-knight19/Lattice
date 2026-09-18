// Package main implements the high-concurrency standalone benchmarking harness
// and latency profiler for the Lattice distributed storage engine.
//
// Purpose:
// cmd/lattice-bench simulates realistic database client workloads against a
// running Lattice node, empirical throughput (ops/sec), and high-resolution
// operation latency percentiles (P50, P90, P99, P99.9, Min, Mean, Max) across
// configurable concurrency levels and access patterns.
//
// Workload Model:
// Supported workload profiles include:
//   - read: 100% GET point-lookup operations.
//   - write: 100% PUT key-value mutation operations.
//   - mixed: Mixed operations governed by --read-ratio (default 0.8 for 80% GET / 20% PUT).
//
// In mixed workloads, each worker executes independent Bernoulli trials using an
// isolated PRNG to prevent artificial lockstep convoy synchronization across workers.
//
// Concurrency & Connection Architecture:
// The harness employs bounded concurrency (1 to 1,024 workers). Each worker goroutine
// maintains a single, dedicated, persistent TCP connection to the target server,
// eliminating connection establishment overhead during the timed measurement window.
// Wire communication uses Lattice's binary protocol (internal/transport), ensuring
// strict FIFO request/response sequencing per connection with TCP_NODELAY enabled.
// If a connection drops, the worker safely initiates reconnection with bounded exponential
// backoff (10ms to 1s) without panicking on nil client states or retrying failed operations.
// All network socket deadlines strictly respect min(now + timeout, context deadline),
// preventing benchmark duration overruns on stalled servers.
//
// Keyspace Pre-Population & Dataset Integrity:
// To ensure valid read benchmarks without cache-hit survivorship bias or false StatusKeyNotFound
// misses, pre-population guarantees that the entire configured keyspace is populated
// (default 10,000 keys matching DefaultPopulateCap) before timed execution begins. Partial
// pre-population for read and mixed workloads is strictly rejected.
//
// Latency Measurement Semantics:
// Measured latency encompasses the client-observed round-trip interval: from immediately
// before the request frame is written to the wire until the complete response frame has
// been read, decoded, verified (SeqID and CRC32-IEEE), and operation status evaluated.
// Connection establishment, keyspace pre-population, key generation, and histogram
// recording are strictly excluded from the measured latency window. Failed operations
// are recorded in error counters and isolated from success latency percentiles.
//
// Relationship to M01 (Zipfian Key Distribution):
// Keys are generated using internal/benchmark.ZipfGenerator, modeling access hotspots
// with canonical skew theta = 0.99 over a finite keyspace (default 10,000 keys).
// Uniform distribution is also supported. Each worker owns an independent generator
// seeded deterministically to guarantee reproducible, zero-contention key generation.
//
// Relationship to M02 (High-Resolution Latency Histogram):
// Latency observations are captured into worker-local internal/benchmark.LatencyHistogram
// structures with zero heap allocations on the recording hot path (RecordNano). Each
// worker maintains independent GET and PUT histograms. Upon benchmark completion,
// worker histograms are aggregated into global histograms via Merge() for final report
// generation.
package main

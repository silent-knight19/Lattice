# Lattice

> **A High-Performance Distributed Key-Value Storage Engine Being Built from the Ground Up**

[![Status: Phases 00–12 Complete](https://img.shields.io/badge/Status-Phases%2000--12%20Complete-brightgreen.svg)](#)
[![Design Spec](https://img.shields.io/badge/Docs-Architecture%20Spec-blue.svg)](docs/architecture-spec.md)
[![Implementation Plan](https://img.shields.io/badge/Docs-Implementation%20Plan-green.svg)](docs/implementation-plan.md)
[![Interview Knowledge](https://img.shields.io/badge/Docs-Interview%20Defense-purple.svg)](docs/interview-knowledge.md)
[![Known Limitations](https://img.shields.io/badge/Docs-Known%20Limitations-orange.svg)](docs/known-limitations.md)
[![Threat Model](https://img.shields.io/badge/Docs-Threat%20Model-red.svg)](docs/threat-model.md)
[![Language](https://img.shields.io/badge/Language-Go%201.22+-00ADD8.svg)](https://go.dev)
[![Architecture](https://img.shields.io/badge/Storage-LSM--Tree%20%2B%20Raft-orange.svg)](#)

---

## Overview

**Lattice** is a ground-up distributed key-value storage engine being developed in Go. It does not wrap SQLite, RocksDB, LevelDB, Redis, or etcd. Every subsystem—from append-only write-ahead logging (WAL) and custom binary wire protocols to probabilistic Bloom filtering, k-way leveled merge compaction, and Raft distributed consensus—is designed and implemented from core systems primitives.

The comprehensive system architecture and 184-step micro-phase execution blueprint are complete. Implementation is proceeding incrementally following strict engineering discipline: test-driven micro-steps, race detection (`-race`), continuous security threat reviews, empirical benchmark validation, and interview-oriented documentation tracking.

---

## Architecture Design Targets

The system design specifies the following architectural targets (currently undergoing incremental implementation):

* **Storage Engine**: Log-Structured Merge-Tree (LSM-Tree) with write-optimized sequential disk I/O.
* **Durability & WAL Target**: Pre-allocated append-only binary logs with CRC32-IEEE checksumming and cooperative **Group Commit** batching (`fdatasync()` barriers).
* **In-Memory MemTable**: Probabilistic concurrent **SkipList** with lock-free reads and monotonic 64-bit sequence numbers.
* **Persistent SSTable Format**: Immutable binary files with 4KB Data Blocks, prefix compression, 16-record restart points, Two-Level Block Indexes, and 48-byte fixed Footers.
* **Probabilistic Filtering**: Murmur3-based **Bloom filters** (10 bits/key, 7 hash functions) targeting $\ge 99\%$ cold read disk avoidance.
* **Compaction Strategy**: Leveled compaction ($L_0$ overlapping, $L_1..L_N$ non-overlapping partitioned runs with a $10\times$ multiplier) driven by k-way merge iterators.
* **Concurrency Model**: Version-pinned snapshot reads (`VersionSet` with atomic reference counting) ensuring reads never block on background flushes or compactions.
* **Wire Protocol**: Custom length-prefixed binary framing protocol over TLS 1.3 / mutual TLS (mTLS) sockets for both client data plane and Raft peer consensus transport (with loopback plaintext fallback for local development) with frame-bomb protections and concurrent request pipelining. Client-facing listeners strictly wrap `GetConfigForClient` to eliminate dynamic authentication/trust bypasses, mandate TLS 1.3+ and mutual TLS, pin trusted ClientCAs, preserve PKI separation, and enforce post-handshake cryptographic verification before any application request processing. For non-loopback Raft peers, Lattice strictly owns the TLS authentication boundary. Custom transport injection (`RawDialFunc`, `DialFunc`) cannot replace or bypass Lattice's TLS 1.3, mTLS, peer trust-anchor validation, certificate-role validation, or NodeID binding. Custom dialers act strictly as low-level raw network transport seams; returning a pre-secured or unauthenticated `*tls.Conn` on non-loopback peers is rejected fail-closed. Lattice wraps the raw transport with its own manager-owned TLS 1.3 configuration, intercepts and wraps `GetConfigForClient` callbacks to prevent dynamic trust or auth weakening, executes the TLS handshake against the trusted peer CA pool, validates cryptographic trust chains (`len(VerifiedChains) > 0`), enforces peer role (`OU = Lattice Raft Peer`) and expected remote NodeID binding, and asserts security admission criteria before the connection can ever reach `PeerStateConnected`.
* **Distributed Consensus (V1.1 Target)**: Single-group **Raft** consensus supporting leader election, randomized heartbeat timers, quorum replication ($Q = \lfloor N/2 \rfloor + 1$), and linearizable reads via the `ReadIndex` protocol.

---

## System Topology Blueprint

```
+-----------------------------------------------------------------------------------+
|                                  LATTICE SYSTEM                                   |
|                                                                                   |
|  +--------------------+        +--------------------+        +-----------------+  |
|  |    TCP Clients     |  --->  |    Wire Protocol   |  --->  | Engine Dispatch |  |
|  |  (CLI / Bench / SDK|        |  (Binary Framing)  |        | (Group Commit)  |  |
|  +--------------------+        +--------------------+        +--------+--------+  |
|                                                                       |           |
|         +-------------------------------------------------------------+           |
|         |                                                                         |
|         v                                                             v           |
|  +--------------+   Buffered Appends   +--------------+         +--------------+  |
|  |  Write-Ahead | ===================> | Page Cache / |         | Active       |  |
|  |  Log (WAL)   |   `fdatasync()`      | Disk Drive   |         | MemTable     |  |
|  +--------------+                      +--------------+         | (SkipList)   |  |
|                                                                 +-------+------+  |
|                                                                         | Swap    |
|                                                                         v         |
|  +--------------------+   Flushes via Merge   +--------------------------------+  |
|  |   Leveled SSTables | <==================== | Immutable MemTable Pipeline    |  |
|  | (L0..LN, Bloom, Idx|                       +--------------------------------+  |
|  +---------+----------+                                                           |
|            |                                                                      |
|            +============== k-way Merging Compactor (Background)                   |
+-----------------------------------------------------------------------------------+
```

---

## Reproducible Empirical Benchmark

In adherence to our Evidence Classification Rule (Tier 3: Measured Results), the performance metrics below reflect client-observed latencies and achieved throughput directly measured against a real, running Lattice daemon under clean standalone conditions.

### Benchmark Environment

* **CPU**: Apple M4 (10 cores: 10 physical)
* **RAM**: 16 GB Unified Memory
* **Operating System**: macOS 27.0 (Darwin 27.0.0, `darwin/arm64`)
* **Storage / Filesystem**: Apple APFS on internal NVMe SSD
* **Go Runtime**: `go1.27.1 darwin/arm64`
* **Daemon Topology**: Standalone single-node daemon (`127.0.0.1:9099`)
* **Network Boundary**: Local loopback TCP socket (client and server co-located)
* **Software Revision**: Git commit `dfdbccae2d567ddec219435885e10378644664a2` (main)

### Benchmark Workload Configuration

* **Workload Mix**: Mixed read/write (target 80% GET / 20% PUT)
* **Concurrency**: 64 persistent worker TCP connections (goroutines)
* **Timed Measurement Interval**: 60 seconds
* **Access Distribution**: Zipfian power-law skew ($\theta = 0.99$, canonical YCSB profile)
* **Keyspace Domain**: 10,000 keys (`key:0000000000` through `key:0000009999`)
* **Value Payload Size**: 256 bytes per PUT operation
* **PRNG Base Seed**: 42 (deterministic worker seeds: `seed + i*10007 + 1`)
* **Pre-population**: 10,000 keys sequentially populated before measurement interval

### Exact Reproduction Command

```bash
# 1. Start clean standalone daemon in dedicated directory
./lattice --data-dir=/tmp/lattice-bench-data --port=9099 --metrics-address=127.0.0.1:9100

# 2. Execute canonical 60-second benchmark (in separate shell)
./lattice-bench \
  --address=127.0.0.1:9099 \
  --workload=mixed \
  --read-ratio=0.8 \
  --concurrency=64 \
  --duration=60s \
  --key-distribution=zipfian \
  --keys=10000 \
  --val-size=256 \
  --seed=42 \
  --populate=true \
  --populate-keys=10000
```

### Measured Results (Representative Median Trial)

| Metric                                |        GET (Read) |        PUT (Write) |          Combined Workload |
| :------------------------------------ | ----------------: | -----------------: | -------------------------: |
| **P50 Latency (Median)**        |  **4.05ms** | **232.78ms** |                         — |
| **P90 Latency**                 |            8.09ms |           244.32ms |                         — |
| **P99 Latency (Tail)**          | **15.99ms** | **254.80ms** |                         — |
| **P99.9 Latency**               |           20.19ms |           264.24ms |                         — |
| **Min Latency**                 |            0.59ms |             4.22ms |                         — |
| **Mean Latency**                |            5.25ms |           232.03ms |                         — |
| **Max Latency**                 |           38.94ms |           271.08ms |                         — |
| **Successful Operations**       |        60,840 ops |         15,143 ops |       **75,983 ops** |
| **Achieved Operation Ratio**    |            80.07% |             19.93% |                    100.00% |
| **Achieved Throughput**         |                — |                 — | **1,266.36 ops/sec** |
| **Timed Measurement Duration**  |                — |                 — |   **60.001 seconds** |
| **Application Errors**          |                 0 |                  0 |         **0 errors** |
| **Boundary Cancellation Drops** |                — |                 — |               64 requests* |

*\*Note on Boundary Cancellation Drops: To enforce strict 60-second duration bounds without overrun, the harness applies context deadlines to in-flight TCP requests when the 60.000s measurement timer expires. With 64 concurrent workers, the 64 requests in-flight at $T = 60.001\text{s}$ are cancelled at the socket boundary. Zero network drops or protocol errors occurred during active serving.*

### Multi-Trial Repeatability

To verify measurement consistency and eliminate one-off anomalies, three independent 60-second trials were executed with fresh daemon instances and empty database directories:

| Trial                      | Measured Duration | Successful Ops |     Throughput | GET P50 | GET P99 |  PUT P50 |  PUT P99 | Achieved Read Ratio | Boundary Drops |
| :------------------------- | ----------------: | -------------: | -------------: | ------: | ------: | -------: | -------: | ------------------: | -------------: |
| **Trial 1 (Median)** |           60.001s |         75,983 | 1,266.36 ops/s |  4.05ms | 15.99ms | 232.78ms | 254.80ms |              80.07% |             64 |
| **Trial 2**          |           60.002s |         74,000 | 1,233.29 ops/s |  4.08ms | 16.19ms | 233.83ms | 394.26ms |              80.09% |             64 |
| **Trial 3**          |           60.002s |         76,258 | 1,270.93 ops/s |  4.05ms | 15.14ms | 232.78ms | 254.80ms |              80.06% |             64 |

*Repeatability Analysis*: Across three independent trials on clean hardware, achieved throughput varied by $< 3.0\%$ (1,233.29 to 1,270.93 ops/sec). GET P50 latency varied by $< 0.8\%$ (4.05ms to 4.08ms) and PUT P50 latency varied by $< 0.5\%$ (232.78ms to 233.83ms).

### Measurement Methodology & Latency Dynamics

1. **Client-Observed Boundary**: Latency timing starts immediately before `transport.WriteRequest` and completes immediately after `transport.ReadResponse` and header verification ($t_{\text{latency}} = t_{\text{resp}} - t_{\text{req}}$). Key generation, Zipfian PRNG sampling, and terminal formatting execute outside the timed interval.
2. **Logarithmic Sub-Bucket Histogram**: Latency samples are collected into fixed-size logarithmic sub-bucket histograms (`internal/benchmark/histogram.go`) using 128 linear sub-buckets per power-of-two octave, bounding relative quantization error to $\le 0.78125\%$ without hot-path allocations. Percentiles follow discrete nearest-rank semantics returning bucket upper bounds.
3. **Pre-population Isolation**: 10,000 keys are sequentially pre-populated before worker connections are established. Pre-population elapsed time (~37–39s) is strictly excluded from timed measurement duration and throughput calculations.
4. **Physical Write Durability Queuing**: Standalone daemon writes execute durable write-ahead logging via `AppendSync()` (`fdatasync()` barrier) on physical NVMe storage under `Engine.mu.Lock()`. With 64 concurrent workers, each physical disk flush (~3.5–4.5ms) is serialized to preserve crash-recovery sequence ordering, resulting in queued write wait times of $\approx 64 \times 3.6\text{ms} \approx 232\text{ms}$ P50 latency. Read operations acquiring `Engine.mu.RLock()` queue briefly behind pending write locks, producing a 4.05ms median GET latency.

---

## Roadmap & Verified Implementation Status

*In adherence to our Project Truth Rule, completion checkmarks (`[x]`) are awarded only after code is written, tested under `-race`, verified against invariants, and reviewed.*

* [x] **Architecture Specification** — Completed in [`docs/architecture-spec.md`](docs/architecture-spec.md)
* [x] **Micro-Phase Implementation Planning (184 Steps)** — Completed in [`docs/implementation-plan.md`](docs/implementation-plan.md)
* [x] **Interview Knowledge Base & Defense Guide** — Completed in [`docs/interview-knowledge.md`](docs/interview-knowledge.md)
* [x] **Architecture Decision Records (ADR 001–008)** — Completed in [`docs/decisions/`](docs/decisions/)
* [x] **Living Known Limitations Document** — Completed in [`docs/known-limitations.md`](docs/known-limitations.md)
* [x] **System Security Threat Model** — Completed in [`docs/threat-model.md`](docs/threat-model.md)
* [x] **Phase 00: Repository & Engineering Foundations** — Completed (Audit: Pass with Remediations)
* [x] **Phase 01: Core Storage Primitives & Binary Encodings** — Completed (Audit: Pass with Remediations)
* [x] **Phase 02: Write-Ahead Log (WAL) & Durability Subsystem** — Completed (Audit: Pass with Remediations)
* [x] **Phase 03: In-Memory MemTable & Concurrent SkipList** — Completed (Audit: Pass with Remediations)
* [x] **Phase 04: Persistent SSTable Subsystem** — Completed (Audit: Pass with Remediations)
* [x] **Phase 05: Probabilistic Bloom Filter Subsystem** — Completed (Audit: Pass with Remediations)
* [x] **Phase 06: Manifest Log & VersionSet Management** — Completed (Audit: Pass with Remediations)
* [x] **Phase 07: Crash Recovery & Integrity Verification** — Completed (Audit: Pass with Remediations)
* [x] **Phase 08: Leveled Compaction Subsystem** — Completed (Audit: Pass with Remediations)
* [x] **Phase 09: Sharded LRU Read Block Cache** — Completed (Audit: Pass with Remediations)
* [x] **Phase 10: Single-Node Storage Engine Integration** — Completed (Audit: Pass with Remediations)
* [x] **Phase 11: TCP Binary Wire Protocol & Networking** — Completed (Audit: Pass with Remediations)
* [x] **Phase 12: CLI, Interactive REPL & Forensic Diagnostics** — Completed (Audit: Pass with Remediations)
* [ ] **Phase 13: Benchmarking Suite & Performance Profiling**
* [ ] **Phase 14: Distributed Cluster Foundations & Node Topology**
* [ ] **Phase 15: Raft Consensus Engine (V1.1)**
* [ ] **Phase 16: Distributed State Machine Replication**
* [ ] **Phase 17: Linearizable Reads (ReadIndex Protocol)**
* [ ] **Phase 18: Fault Injection & Chaos Testing Suite**
* [ ] **Phase 19: Comprehensive Security Hardening**
* [ ] **Phase 20: Production Hardening & Operational Observability**
* [ ] **Phase 21: Resume & Technical Interview Portfolio Validation**

---

## Core Engineering Documentation

* [Full Architecture & System Design Specification (48 Sections & Diagrams)](docs/architecture-spec.md)
* [Micro-Phase Implementation Plan (184 Verifiable Micro-Steps)](docs/implementation-plan.md)
* [Technical Interview Knowledge Base & Defense Guide](docs/interview-knowledge.md)
* [Architecture Decision Records (ADRs)](docs/decisions/)
* [Known Limitations & Architectural Boundaries](docs/known-limitations.md)
* [System Security Threat Model](docs/threat-model.md)

# Lattice: Micro-Phase Implementation Plan & Execution Blueprint

* **Document Version**: 1.0.0-EXEC-PLAN
* **Source Architecture Specification**: [`docs/architecture-spec.md`](architecture-spec.md)
* **Status**: Approved for Execution
* **Methodology**: Test-Driven, Micro-Incremental, Security-First Systems Engineering

---

---

## Table of Contents

1. [Architectural Consistency Audit & Integrity Verification](#1-architectural-consistency-audit--integrity-verification)
2. [Evidence Classification System](#2-evidence-classification-system)
3. [Project Truth & Documentation Accuracy Rules](#3-project-truth--documentation-accuracy-rules)
4. [Project-Wide Definition of Done (DoD)](#4-project-wide-definition-of-done-dod)
5. [Invariant-Driven Testing Framework](#5-invariant-driven-testing-framework)
6. [Early Subsystem Failure Testing Requirements](#6-early-subsystem-failure-testing-requirements)
7. [Differential Testing Methodology](#7-differential-testing-methodology)
8. [Correctness-First Optimization Policy](#8-correctness-first-optimization-policy)
9. [Human Review Gates](#9-human-review-gates)
10. [Benchmark Reproducibility & Baseline/Regression Model](#10-benchmark-reproducibility--baselineregression-model)
11. [Resume Evidence Rule](#11-resume-evidence-rule)
12. [Dependency & Supply-Chain Awareness Policy](#12-dependency--supply-chain-awareness-policy)
13. [Resume Buzzword Prohibition Rule](#13-resume-buzzword-prohibition-rule)
14. [Claude Code 13-Step Execution Protocol](#14-claude-code-13-step-execution-protocol)
15. [Mandatory Implementation Response Format](#15-mandatory-implementation-response-format)
16. [Current Execution State](#16-current-execution-state)
17. [Git Workflow & Commit Cadence](#17-git-workflow--commit-cadence)
18. [Resume Signal Tracking Matrix](#18-resume-signal-tracking-matrix)
19. [Implementation Hierarchy & Roadmap Overview (184 Micro-Phases)](#19-implementation-hierarchy--roadmap-overview-184-micro-phases)

---

# 1. Architectural Consistency Audit & Integrity Verification

A strict line-by-line audit of [`docs/architecture-spec.md`](architecture-spec.md) was conducted prior to authoring this implementation plan.

### Audit Findings

1. **WAL Group Commit vs MemTable Insertion Invariant**:
   * *Analysis*: If multiple writers enqueue tasks, the leader flushes and syncs to disk before inserting into the MemTable. If the WAL sync fails, no records are inserted into the MemTable, ensuring disk and memory never diverge. Verified consistent.
2. **SSTable Footer Fixed Offset Calculation**:
   * *Analysis*: The SSTable footer is exactly 48 bytes (`2 * 16B` block handles + `8B` padding + `8B` magic). The footer is read at `file_size - 48`. Verified consistent.
3. **Compaction Tombstone Purging Invariant**:
   * *Analysis*: A tombstone cannot be purged during compaction if older revisions of the key exist in lower levels ($L_{target+1}..L_N$). The plan includes explicit key-range scanning checks across deeper levels before dropping tombstones. Verified consistent.
4. **VersionSet Concurrency & File Unlinking**:
   * *Analysis*: Readers pin an immutable `Version` via atomic reference counting (`atomic.AddInt32(&v.refCount, 1)`). Background compaction installs new versions via atomic pointer swap. SSTables are physically unlinked only when `v.refCount == 0`. Verified consistent.
5. **Wire Protocol Frame Bomb Defense**:
   * *Analysis*: Maximum frame size is constrained to 5MB. Inbound lengths are validated prior to memory allocation. Verified consistent.

### Audit Conclusion
**No blocking architectural inconsistencies, circular dependencies, or impossible invariants exist.** The architecture specification is completely viable for sequential micro-step implementation.

---

# 2. Evidence Classification System

To maintain absolute technical credibility, every technical statement, performance claim, and algorithmic property across Lattice must be categorized into one of four distinct tiers:

```
+-------------------------------------------------------------------------------+
|                        EVIDENCE CLASSIFICATION TIERS                          |
+-------------------------------------------------------------------------------+
| Tier 1: Design Target         | Aspirational engineering goal for development |
| Tier 2: Theoretical Property   | Mathematically or architecturally provable    |
| Tier 3: Measured Result       | Empirically benchmarked on specific hardware  |
| Tier 4: Observed Limitation   | Empirically discovered bottleneck or boundary |
+-------------------------------------------------------------------------------+
```

### Governing Rules
1. **Design Target $\ne$ Measured Result**: A claim such as "$\ge 80,000 \text{ writes/sec}$" or "$\le 1.5\text{s recovery}$" is classified strictly as a **Design Target**. It must **never** be cited as an achieved capability until an automated benchmark executes on real hardware and records verifiable evidence.
2. **Theoretical Properties**: Concepts like "Bloom filter false-positive probability $\approx 0.82\%$", "SkipList expected complexity $O(\log N)$", or "Leveled compaction space amplification $\le 1.33\times$" are classified as **Theoretical Properties**. When tested, they must be validated against empirical distributions.
3. **Measured Results**: Can only be stated after running reproducible benchmarks capturing the full hardware/OS tuple (CPU, RAM, NVMe model, OS kernel, Go version, Git commit, workload parameters).
4. **Observed Limitations**: Any bottleneck or degraded edge case discovered during execution must be immediately documented in [`docs/known-limitations.md`](known-limitations.md).

---

# 3. Project Truth & Documentation Accuracy Rules

> **Core Axiom**: Documentation must never be more advanced than reality when describing implementation status.

1. The architecture specification (`docs/architecture-spec.md`) defines target blueprints and future goals.
2. The implementation plan (`docs/implementation-plan.md`) defines roadmap tasks.
3. **Public Status Integrity**: The root `README.md`, resume summaries, and portfolio notes must only display completion checkmarks (`[x]`) and "implemented" descriptors for micro-phases that have met the full Definition of Done.
4. **Zero Vanity Claims**: Never advertise "Raft linearizability", "Zero data loss", or "High performance" before the respective verification tests execute.

---

# 4. Project-Wide Definition of Done (DoD)

A micro-phase is **NOT COMPLETE** merely because the code compiles. Every micro-phase must satisfy the following checklist before being marked `Completed`:

```
   Scope Implemented
          ↓
   Targeted Unit Tests Pass
          ↓
   Regression Suite Clean
          ↓
   Race Detector Clean (`go test -race ./...`)
          ↓
   Static Analysis Clean (`go vet`, `golangci-lint`)
          ↓
   Security Checked (Boundaries, Path Traversal, Buffers)
          ↓
   Invariants & Failure Cases Explicitly Verified
          ↓
   Documentation & ADRs Updated (if applicable)
          ↓
   Interview Knowledge Base Updated
          ↓
   Git Checkpoint Committed
          ↓
   Human Review Gate Passed (if at subsystem boundary)
```

*Note on Optimizations*: Do not require zero allocations or 100% line coverage universally. Prioritize meaningful correctness, error-path handling, and failure-mode resilience.

---

# 5. Invariant-Driven Testing Framework

Before implementing any subsystem, its non-negotiable correctness invariants must be established. Tests must intentionally attempt to violate these invariants.

| Subsystem | Invariant Statement | Why It Matters | How It Could Be Violated | Detection Test | Failure Behavior |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **WAL** | An acknowledged durable write must be recoverable after crash. | Prevents silent data loss. | Dropping `fdatasync()` or truncating valid records. | Crash injection + WAL replay. | Fatal engine halt if corrupted; clean truncate if torn tail. |
| **MemTable** | Keys remain monotonically ordered at all SkipList levels. | Range scans and binary lookups depend on sort order. | Race condition during concurrent forward pointer splicing. | Concurrent multi-threaded stress test under `-race`. | Linter/race detector panic; test assertion failure. |
| **SSTable** | Records within data blocks are strictly ordered; restart points match. | Binary search within 4KB block fails if order is violated. | Bug in prefix compression restart point offset calculation. | Round-trip block builder and sequential iterator check. | Block decoding returns `ErrCorruptedBlock`. |
| **Compaction** | Newer record revisions must always supersede older revisions. | Prevents stale data overwriting recent updates. | Bug in min-heap priority queue comparison logic. | K-way merge test with identical keys across 4 files. | Iterator assertion fails. |
| **Tombstones** | A tombstone cannot be dropped while an older revision exists at a deeper level. | Prevents deleted keys from silently reappearing ("ghost key resurrect"). | Compactor purges tombstone at $L_1$ without checking $L_2..L_N$. | Ghost key resurrect test: insert key, flush, delete, compact $L0 \to L1$. | Test detects resurrect; halts compaction with error. |
| **VersionSet** | Readers never observe partially installed metadata or unlinked files. | Prevents nil-pointer panics or corrupted query reads. | Compaction unlinks SSTable file descriptor while reader is active. | Version-pinned concurrency stress test. | Operating system returns `EBADF` or panic. |
| **Raft** | A committed log entry cannot be replaced by a conflicting entry. | Fundamental consensus safety (State Machine Safety). | Leader overwrites log without quorum confirmation. | Jepsen-style network partition test with split votes. | State machine comparison fails. |

---

# 6. Early Subsystem Failure Testing Requirements

Failure testing must **not** be postponed until Phase 18. Each subsystem includes immediate failure tests in its own micro-phases:

* **WAL Subsystem (`Phase 02`)**: Tests truncated tail records, random bit-flip checksum corruptions, simulated process termination mid-write (`P02-S01-M03`, `P02-S03-M01`).
* **SSTable Subsystem (`Phase 04`)**: Tests invalid footer magic, out-of-bounds block handles, corrupted block CRC32s, truncated SSTable files (`P04-S02-M02`, `P04-S03-M02`).
* **Manifest Subsystem (`Phase 06` & `Phase 07`)**: Tests torn `VersionEdit` records, missing referenced SSTable files on disk, crash during `CURRENT` pointer swap (`P06-S02-M01`, `P07-S01-M02`).
* **Concurrency Subsystem (`Phase 03` & `Phase 10`)**: Tests overlapping readers/writers, race detector validations under 64 goroutines, write backpressure stalls (`P03-S02-M01`, `P10-S01-M03`).
* **Networking Subsystem (`Phase 11`)**: Tests frame-bomb oversized payloads, truncated byte frames, invalid opcodes, slow connection timeouts (`P11-S01-M01`).
* **Raft Consensus (`Phase 15`)**: Tests candidate rejection on stale terms, log divergence recovery, follower reconnection catch-up (`P15-S02-M01`, `P15-S03-M02`).

---

# 7. Differential Testing Methodology

For complex subsystems, a simple, obviously correct reference model serves as a **correctness oracle**:

```
      Random Pseudorandom Operation Sequence (10,000 Ops)
                      │
           ┌──────────┴──────────┐
           ▼                     ▼
┌─────────────────────┐   ┌─────────────────────┐
│   Reference Model   │   │    Lattice Engine   │
│ (In-Memory Go Map + │   │ (SkipList / WAL /   │
│ Mutex / Simple Log) │   │ SSTable Compaction) │
└─────────────────────┘   └─────────────────────┘
           │                     │
           └──────────┬──────────┘
                      ▼
        Compare Final State & Outputs
```

### Planned Differential Targets
1. **MemTable SkipList (`P03`)**: Validated against `map[string][]byte` protected by a global `sync.RWMutex`.
2. **K-Way Merge Compaction (`P08`)**: Validated against an in-memory slice sort and deduplication reference implementation.
3. **Storage Engine CRUD (`P10`)**: 100,000 random operations compared against a reference key-value model.

---

# 8. Correctness-First Optimization Policy

We strictly enforce the systems engineering optimization hierarchy:

$$\text{Correctness} \longrightarrow \text{Determinism} \longrightarrow \text{Testability} \longrightarrow \text{Observability} \longrightarrow \text{Performance} \longrightarrow \text{Micro-optimization}$$

### Rules
1. **No Speculative Optimization**: Do not introduce `unsafe`, lock-free algorithms, or manual memory tricks without a baseline benchmark demonstrating a real bottleneck.
2. **Allocation Justification**: Do not introduce `sync.Pool` or zero-copy slicing unless Go `pprof` heap profiles show that GC allocation is degrading throughput or tail latency.
3. **Optimization Documentation Template**:
   ```
   Problem Measured  : <Data from pprof / benchmark baseline>
   Baseline Metric   : <e.g., 42,000 ops/sec, 14 allocs/op>
   Optimization Made : <Description of algorithmic or memory change>
   Result After      : <e.g., 78,000 ops/sec, 2 allocs/op>
   Trade-off Incurred: <Increased code complexity, memory retention>
   ```

---

# 9. Human Review Gates

Implementation must pause for human review and sign-off at these 16 critical subsystem boundaries:

| Gate | Subsystem Boundary | Human Review & Verification Requirements |
| :--- | :--- | :--- |
| **Gate 00** | Foundations (`P00`) | Review project layout, error model, linter cleanliness. |
| **Gate 01** | Primitives (`P01`) | Review endianness, varint codec safety, CRC32 test vectors. |
| **Gate 02** | WAL Subsystem (`P02`) | Review Group Commit concurrency, torn write truncation, `fdatasync()` safety. |
| **Gate 03** | MemTable (`P03`) | Review SkipList lock-free read traversal, `-race` cleanliness, memory accounting. |
| **Gate 04** | SSTables (`P04`) | Inspect raw SSTable hex/block dumps, verify sparse index binary search. |
| **Gate 05** | Bloom Filters (`P05`)| Validate empirical false positive measurements against mathematical model. |
| **Gate 06** | Manifest (`P06`) | Review `VersionSet` ref-counting, atomic `CURRENT` write-rename. |
| **Gate 07** | Recovery (`P07`) | Test simulated process crash recovery; verify zero lost acknowledged writes. |
| **Gate 08** | Compaction (`P08`)| Audit tombstone purge safety logic; verify non-overlapping leveled invariant. |
| **Gate 09** | Block Cache (`P09`)| Review 16-shard partition hashing and 64-byte cache-line padding. |
| **Gate 10** | Engine (`P10`) | Verify end-to-end CRUD integration, write pacing, graceful shutdown. |
| **Gate 11** | Networking (`P11`)| Review frame-bomb limits, socket read timeouts, TCP client connection pooling. |
| **Gate 12** | Diagnostics (`P12`)| Test interactive CLI REPL and forensic SSTable/WAL inspection tools. |
| **Gate 13** | Benchmarking (`P13`)| Audit benchmark reproducibility, Zipfian generator, latency histogram capture. |
| **Gate 14** | Raft Core (`P15`) | Review randomized election timers, term stepping, quorum commit math. |
| **Gate 15** | Linearizability (`P17`)| Audit ReadIndex implementation; verify stale read prevention under partition. |

---

# 10. Benchmark Reproducibility & Baseline/Regression Model

Every benchmark result cited in documentation or interview notes must be accompanied by the full environmental metadata:

```
[LATTICE BENCHMARK RUN LOG]
Timestamp             : 2026-09-06T11:42:00Z
Git Commit            : <exact 40-char commit hash>
Hardware CPU          : Apple M3 Pro / AMD EPYC 7763 (Cores / Threads)
System RAM            : 36 GB Unified / 64 GB DDR4
Storage Media         : Apple NVMe SSD / Samsung 980 PRO 1TB NVMe
Operating System      : macOS 14.5 / Linux 6.5.0-generic
Go Version            : go1.22.4 darwin/arm64
Compiler Flags        : -trimpath -gcflags=all="-N -l" (or optimized)
Dataset Size          : 10,000,000 keys (approx. 2.4 GB raw)
Key / Value Size      : 16-byte key / 256-byte value
Workload Profile      : 80% Read / 20% Write (Zipfian s=0.99)
Client Concurrency    : 64 concurrent goroutines
Test Duration         : 60.00 seconds
Cache State           : Cold start / Warm (256MB cache capacity)
Compaction State      : Background compactor active
Benchmark Command     : ./lattice-bench --workload=mixed --concurrency=64 --duration=60s
Measured Throughput   : 124,198 ops/sec
Latency Percentiles   : P50: 0.21ms | P95: 0.68ms | P99: 1.82ms | P99.9: 4.10ms
```

---

# 11. Resume Evidence Rule

To guarantee that resume claims are bulletproof and withstand deep technical scrutiny by senior FAANG interviewers:

A feature may **only** be listed as an achievement on a resume if it meets all 5 criteria:
1. **Implemented**: Production code is fully written and merged into `main`.
2. **Tested**: Comprehensive unit, regression, and race-detection tests pass.
3. **Invariants Proven**: Negative and chaos tests have verified core system invariants.
4. **Empirically Measured**: Quantitative claims (e.g. throughput, latency, recovery time) have recorded, reproducible benchmark evidence matching Section 10.
5. **Interview Defensible**: The human engineer can explain the code, trade-offs, failure modes, and low-level mechanics without relying on generic talking points.

---

# 12. Dependency & Supply-Chain Awareness Policy

* **Default Rule**: **Zero External Dependencies**. The storage engine core, WAL, MemTable, SSTables, Bloom filters, and TCP networking rely strictly on the Go standard library (`os`, `sync`, `net`, `hash/crc32`, `container/heap`, `log/slog`, `syscall`).
* **Permissible Exceptions**: Only highly specialized, widely audited packages (such as `golang.org/x/sys/unix` for platform-specific POSIX flags, or Google's `snappy` for block compression) may be introduced, subject to written justification in an ADR.

---

# 13. Resume Buzzword Prohibition Rule

Technologies must **never** be added solely for resume hype. Lattice explicitly prohibits introducing:
* Kubernetes operators, Kafka, Redis sidecars, or gRPC wrappers.
* `io_uring`, SIMD, or complex lock-free algorithms unless a real benchmark demonstrates that standard library primitives are a blocking bottleneck.
* Every proposed architectural addition must document: Problem $\to$ Limitation $\to$ Proposed Solution $\to$ Alternatives $\to$ Measured Justification $\to$ Trade-offs.

---

# 14. Claude Code 13-Step Execution Protocol

For **every** single micro-phase execution request, Claude Code must follow this strict loop:

1. **Read Core Specs**: Inspect `docs/architecture-spec.md`, `docs/implementation-plan.md`, `docs/interview-knowledge.md`.
2. **Identify Target Micro-Phase**: State the exact ID (`Pxx-Sxx-Mxx`) and single objective.
3. **Inspect Repository**: Check git status and existing files.
4. **Implement Scope**: Write code **only** for the designated micro-phase.
5. **Run Targeted Tests**: Execute unit tests verifying the change.
6. **Run Regression Tests**: Execute full test suite.
7. **Run Race Detector**: Execute `go test -race ./...`.
8. **Perform Security Review**: Audit boundaries, permissions, and memory bounds.
9. **Verify Invariants**: Assert subsystem invariants are preserved.
10. **Update Documentation**: Update docstrings, ADRs, or known limitations.
11. **Update Interview Knowledge**: Add real lessons, questions, and trade-offs.
12. **Report Results**: Provide report strictly adhering to the mandatory format.
13. **STOP**: Halt execution and wait for human review.

---

# 15. Mandatory Implementation Response Format

Every future micro-phase implementation response from Claude Code must use this format:

```markdown
## Target Micro-Phase
- ID: Pxx-Sxx-Mxx
- Objective: <Single concise goal>
- Dependencies: <Preceding micro-phases>

## Changes Made
- Exact files created or modified with specific line/function details.

## Invariants Maintained
- Concrete statement of preserved system invariants.

## Tests Executed
- Commands run and explicit test outputs.

## Security Review
- Specific attack surfaces analyzed and mitigations implemented.

## Failure Cases Tested
- Specific corruptions, edge cases, or error paths verified.

## Performance & Allocations
- Allocation profile or benchmark notes (if applicable).

## Documentation Updated
- Documents and docstrings modified.

## Interview Alignment
- Core concepts the human engineer must explain in an interview.

## Known Limitations Discovered
- Any newly discovered boundaries (added to `docs/known-limitations.md`).

## Git Checkpoint
- Suggested commit command with standard commit message.

## Status
- Completed / Blocked / Review Required.
```

---

# 16. Current Execution State

```
Current Major Phase           : Phase 04 — Persistent SSTable Subsystem (COMPLETE)
Current Sub-Phase             : Sub-Phase 04.3 — SSTable File Writer & Reader (COMPLETE)
Current Micro-Phase           : P04-S03-M02 — SSTable Block Reader & Sparse Index Binary Search (COMPLETE)
Phase 01 Status               : COMPLETE (Sub-Phases 01.1 & 01.2 Complete)
Phase 02 Status               : COMPLETE (Sub-Phases 02.1, 02.2, 02.3, 02.4 Complete)
Phase 03 Status               : COMPLETE (Sub-Phases 03.1, 03.2, 03.3 Complete)
Phase 04 Status               : COMPLETE (Sub-Phases 04.1, 04.2, 04.3 Complete)
Previous Completed Phase      : Phase 04 — Persistent SSTable Subsystem
Previous Completed Micro-Phase: P04-S03-M02 — SSTable Block Reader & Sparse Index Binary Search
Next Planned Micro-Phase      : P05-S01-M01 — Bloom Filter Parameter Calculator & Bitset Allocator
Phase 00 Final Audit          : Completed — PASS WITH REMEDIATIONS
Phase 01 Final Audit          : Completed — PASS WITH REMEDIATIONS
Security Audit Track State    : Active
  - SEC-01 — Security Audit Foundations & Attack-Surface Inventory (COMPLETE)
  - SEC-02 — Static Security Audit & Dependency/Secret/Configuration Analysis (COMPLETE)
  - SEC-03 — WAL / Filesystem / Storage Dynamic Security Audit (COMPLETE)
  - SEC-04 — In-Memory Concurrent Engine & SkipList Security Audit (COMPLETE)
  - SEC-P03 — Independent Security Audit Through Phase 03 Scope (COMPLETE)
  - Next Security Phase: SEC-05 — Network / Protocol / Parser / Fuzz Security Audit (PLANNED)
  - SEC-06 through SEC-09 (PLANNED)
Blocking Issues               : None
Tests Passing                 : `go test -race ./...` (All test suites passing, 0 race conditions), `golangci-lint run ./...` clean (0 issues), `go mod verify` passed, Linux & Windows cross-platform verified
Security Review Status        : Complete & Verified (SEC-01 foundations established; SEC-02 static audit verified; SEC-03 dynamic persistence audit completed; SEC-04 in-memory engine audit completed; SEC-P03 independent adversarial audit completed; P04-S01-M01 audited; P04-S01-M02 audited; P04-S02-M01 audited; P04-S02-M02 audited; P04-S03-M01 audited; P04-S03-M02 audited with 0 vulnerabilities, bounded ReadAt disk reads, defensive value copies, CRC32 block verification, bounds-checked restart offset arithmetic, and clean error discrimination)
Interview Knowledge Status    : Updated with Sections 30 through 35 containing deep systems interview questions and answers across prefix compression, restart points, block trailers, sparse two-level indexing, block handles, fixed 48-byte footers, sequential SSTable file writer, and SSTable point read/seek architecture
Git Commit                    : feat(sstable): [P04-S03-M02] implement SSTable block reader and point lookup
```

---

# 16.1 Continuous Security Track Roadmap

In parallel with the feature development roadmap (Phase 00–21), Lattice maintains a dedicated, non-disruptive Continuous Security Audit Track:

| Security Milestone | Scope & Objectives | Status |
| :--- | :--- | :--- |
| **SEC-01** | **Security Audit Foundations & Attack-Surface Inventory**: Trust boundaries, attack surface catalog, 12-vector threat model, security invariants register, security audit data model, baseline audit report. | **COMPLETE** |
| **SEC-02** | **Static Security Audit & Dependency/Secret/Config Analysis**: Go AST analyzers (`SECURITY-001` through `SECURITY-012`), conservative secret scanner (`SECURITY-008`), supply-chain dependency analyzer (`SECURITY-DEP-001`), configuration auditor (`SECURITY-CFG-001`), auditable suppressions, automated markdown/JSON reporting. | **COMPLETE** |
| **SEC-03** | **WAL / Filesystem / Storage Security Audit**: Dynamic filesystem fault injection, torn write fuzzing, descriptor permission verification. | **COMPLETE** |
| **SEC-04** | **In-Memory Concurrent Engine & SkipList Security Audit**: Multi-version key ordering, slice mutation immutability, bounded allocation limits, comparator fuzzing, SkipList/MemTable design target invariants. | **COMPLETE** |
| **SEC-05** | **Network / Protocol / Parser / Fuzz Security Audit**: Wire protocol frame fuzzing, Slowloris defenses, payload ceiling enforcement (post-Phase 11). | Planned |
| **SEC-06** | **Authentication / Authorization / Security Boundary Audit**: Mutual TLS (mTLS), cluster node identity verification, RBAC permissions (post-Phase 14). | Planned |
| **SEC-07** | **Corruption / Fault Injection / Adversarial Recovery Audit**: Random bit-rot mutation testing, split-brain consensus recovery, crash-during-flush validation. | Planned |
| **SEC-08** | **Security Regression Suite / CI Security Gates**: Automated PR blocking on new security findings, deterministic finding tracking. | Planned |
| **SEC-09** | **Final Penetration-Style Audit / Release Security Certification**: End-to-end red team assessment, cryptographic verification, formal release sign-off. | Planned |


---

# 17. Git Workflow & Commit Cadence

* **Branching Model**: Trunk-based development on `main`.
* **Commit Message Format**:
  ```
  feat(subsystem): [PXX-SXX-MXX] Short descriptive summary

  - Detail change 1
  - Detail change 2
  - Invariants verified: <invariants>
  - Tests: <test commands executed>
  ```
* **Cadence**: Exactly **one git commit per micro-phase** upon satisfying the Definition of Done.

---

# 18. Resume Signal Tracking Matrix

| Feature / Subsystem | Technical Signal | Benchmark Evidence Required | Test Evidence Required | Interview Depth | Resume Inclusion |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Custom Binary Wire Protocol** | High | Throughput (req/s), Frame decode CPU time | Frame-bomb rejection, CRC corruption | High | Yes |
| **WAL Group Commit** | Very High | Ops/sec vs Strict fsync (IOPS multiplier) | Torn write crash recovery, kill -9 | Very High | Yes |
| **Concurrent SkipList MemTable** | High | Concurrent read/write latency under load | Race detector (`-race`), concurrent stress | High | Yes |
| **SSTable Block Index & Prefix Compression** | High | Compression ratio, binary search seek time | Hex dump inspection, restart point tests | High | Yes |
| **Murmur3 Bloom Filter** | High | Negative query disk avoidance (>99%) | False-positive rate vs mathematical model | Medium | Yes |
| **VersionSet & MANIFEST Replay** | Very High | Startup recovery latency on 10k SSTables | Crash-during-flush injection, orphan cleanup | Very High | Yes |
| **Leveled Compactor (k-way merge)** | Extremely High | Write amplification, compaction throughput | Ghost key resurrect tests, heap sort invariant | Extremely High | Yes |
| **Sharded LRU Block Cache** | High | Hit ratio, mutex contention profiling | Cache eviction correctness, false-sharing padding | High | Yes |
| **Single-Group Raft Consensus** | Extremely High | Leader election time, replication lag | Network partition (split-brain), drop/replay | Extremely High | Yes |
| **Linearizable ReadIndex Protocol** | Extremely High | Read latency vs consensus round-trips | Stale read prevention during partition | Extremely High | Yes |

---

# 19. Implementation Hierarchy & Roadmap Overview (184 Micro-Phases)

The project is structured into **22 Major Phases (P00–P21)**, **68 Sub-Phases**, and **184 Micro-Phases**.

```
P00: Repository & Engineering Foundations (8 Micro-Phases)
P01: Core Storage Primitives & Binary Encodings (8 Micro-Phases)
P02: Write-Ahead Log (WAL) & Durability Subsystem (12 Micro-Phases)
P03: In-Memory MemTable & Concurrent SkipList (10 Micro-Phases)
P04: Persistent SSTable Subsystem (14 Micro-Phases)
P05: Probabilistic Bloom Filter Subsystem (8 Micro-Phases)
P06: Manifest Log & VersionSet Management (10 Micro-Phases)
P07: Crash Recovery & Integrity Verification (8 Micro-Phases)
P08: Leveled Compaction Subsystem (14 Micro-Phases)
P09: Sharded LRU Read Block Cache (8 Micro-Phases)
P10: Single-Node Storage Engine Integration (10 Micro-Phases)
P11: TCP Binary Wire Protocol & Networking Subsystem (10 Micro-Phases)
P12: CLI, Interactive REPL & Forensic Diagnostics (8 Micro-Phases)
P13: Benchmarking Suite & Performance Profiling (8 Micro-Phases)
P14: Distributed Cluster Foundations & Node Topology (8 Micro-Phases)
P15: Raft Consensus Engine (14 Micro-Phases)
P16: Distributed State Machine Replication (8 Micro-Phases)
P17: Linearizable Reads (ReadIndex Protocol) (6 Micro-Phases)
P18: Fault Injection & Chaos Testing Suite (6 Micro-Phases)
P19: Comprehensive Security Hardening (6 Micro-Phases)
P20: Production Hardening & Operational Observability (6 Micro-Phases)
P21: Resume & Technical Interview Portfolio Validation (4 Micro-Phases)
-------------------------------------------------------------------------
TOTAL: 184 Discrete, Testable Micro-Phases
```

---

# Phase 00: Repository & Engineering Foundations

* **Major Objective**: Establish the Go workspace, project layout, CI checks, error model, and logging foundation.
* **Dependencies**: None.
* **Risks**: Permissive file permissions, untyped error models, external dependency bloat.

### Sub-Phase 00.1: Repository Scaffolding & Tooling
* **P00-S01-M01: Initialize Go Module & Root Metadata**
  * *Status*: **Completed**
  * *Objective*: Create `go.mod` specifying Go 1.22+, initialize root project metadata.
  * *Preconditions*: Empty git repository.
  * *Changes*: Create `go.mod`, `.gitignore`, `.editorconfig`.
  * *Invariants*: Zero external dependencies in `go.mod`.
  * *Tests*: `go mod verify` succeeds.
  * *Security*: `.gitignore` excludes binary artifacts, credentials, `.tmp` files.
  * *Completion*: `go.mod` valid and verified.
* **P00-S01-M02: Canonical Go Project Layout Scaffolding**
  * *Status*: Completed
  * *Objective*: Create the standard Go directory layout (`cmd/`, `internal/`, `pkg/`).
  * *Preconditions*: P00-S01-M01.
  * *Packages Created*: 17 packages established matching Section 42 of architecture spec:
    - CLI entrypoints: `cmd/lattice` (server daemon), `cmd/lattice-cli` (interactive CLI), `cmd/lattice-bench` (microbenchmark driver) with `doc.go` and minimal `main.go`.
    - Internal storage & distributed subsystems: `internal/binary`, `internal/cache`, `internal/compaction`, `internal/engine`, `internal/errors`, `internal/filter`, `internal/memtable`, `internal/metrics`, `internal/raft`, `internal/sstable`, `internal/transport`, `internal/version`, `internal/wal` with canonical `doc.go`.
    - Public client SDK: `pkg/client` with canonical `doc.go`.
  * *Invariants*:
    - Internal packages live strictly under `internal/` preventing external consumer imports (compiler-enforced boundary).
    - Public client API isolated under `pkg/client`.
    - Zero database logic implemented; purely structural scaffolding.
    - Zero import cycles (no inter-package imports introduced).
    - Zero external dependencies.
  * *Verification Performed*:
    - `go build ./...` (Exit 0, compiles all 3 CLI entrypoints and packages)
    - `go vet ./...` (Exit 0, static analysis clean across all 17 packages)
    - `go test ./...` (Exit 0, recognized and verified all 17 packages)
    - `gofmt -l .` (Clean, zero unformatted Go files)
    - `go mod verify` (Exit 0, all modules verified hermetic)
  * *Issues Discovered & Fixed*:
    - Unanchored `.gitignore` rules (`lattice`, `lattice-cli`, `lattice-bench`, `wal/`) matched directories anywhere in the hierarchy, ignoring `cmd/lattice/` and `internal/wal/`. Anchored with leading slashes (`/lattice`, `/wal/`, etc.) in `.gitignore`.
    - Go requires `func main() {}` in `package main` packages to satisfy `go build ./...`; added minimal entrypoints in `cmd/*/main.go`.
  * *Evidence Classification*:
    - *Design Target*: Strict architectural separation of CLI binaries, internal storage engine layers, and public client interface.
    - *Theoretical Property*: Go compiler-enforced package isolation via `internal/` path token.
    - *Measured Result*: 17 packages compile and pass `go build`, `go vet`, and `go test` with zero warnings or errors.
    - *Observed Limitation*: Git does not track empty directories; valid Go files (`doc.go`, `main.go`) are required for the toolchain to recognize packages.
  * *Completion*: Complete canonical package scaffolding established, validated, and documented.
  * *Next Micro-Phase*: P00-S01-M03 — Static Analysis & Linter Configuration.
* **P00-S01-M03: Static Analysis & Linter Configuration**
  * *Status*: Completed
  * *Objective*: Establish `.golangci.yml` enforcing strict linting, error checks, and formatting.
  * *Preconditions*: P00-S01-M02.
  * *Tooling & Configuration*:
    - Created `.golangci.yml` adhering to `golangci-lint` v2 schema (`version: "2"`).
    - Enabled core high-signal linters: `govet`, `errcheck`, `staticcheck`, `ineffassign`, `unused`, `errorlint`, `nolintlint`.
    - Enabled `gofmt` under `formatters`.
    - Strict error enforcement: configured `errcheck` with `check-type-assertions: true` to prevent unhandled interface type conversions from panicking at runtime.
    - Error wrapping validation: configured `errorlint` with `errorf: true`, `asserts: true`, and `comparison: true` to enforce `errors.Is`/`errors.As` over direct `==` comparisons.
    - Suppression auditing: configured `nolintlint` with `require-specific: true` and `require-explanation: true` to bar blanket `//nolint` annotations.
    - Intentional exclusions: disabled `shadow` (standard variable shadowing in Go) and `fieldalignment` (struct padding; database engines intentionally arrange fields for cache-line isolation or binary layout rather than naive size-packing).
  * *Invariants*: Unhandled errors cause linter failure; zero hidden findings (`max-issues-per-linter: 0`, `max-same-issues: 0`).
  * *Verification Performed*:
    - `golangci-lint config verify` (Exit 0, JSON schema valid)
    - `golangci-lint run ./...` (Exit 0, 0 issues across all 17 packages)
    - Tested negative cases: unhandled error (`errcheck`), ineffectual assignment (`ineffassign`), blanket nolint (`nolintlint`), bad formatting (`gofmt`), and raw error comparison (`errorlint`) all correctly failed with exit code 1.
    - `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l .`, `go mod verify` (all Exit 0).
  * *Issues Discovered & Fixed*:
    - `golangci-lint` v2.x requires `version: "2"` at configuration root; omitting it causes exit code 3 (`unsupported version of the configuration`).
    - In v2 schema, formatters like `gofmt` are configured under a separate `formatters` section rather than `linters`.
  * *Evidence Classification*:
    - *Design Target*: Zero-noise, high-signal static analysis enforcing storage durability invariants (unhandled errors) and defensive error comparison (`errors.Is`).
    - *Theoretical Property*: AST-based static analysis detects unchecked return values and type assertions at build time before code reaches runtime.
    - *Measured Result*: `golangci-lint run ./...` executed against all 17 packages reporting 0 issues; negative tests proved failures on unhandled errors, malformed nolint, and bad formatting.
    - *Observed Limitation*: `govet` `fieldalignment` flags intentional cache-line padding and struct field order designed for hardware concurrency; disabled to prevent anti-patterns in low-level systems code.
  * *Completion*: Static analysis and linting foundation established, tested, and verified.
  * *Next Micro-Phase*: P00-S02-M01 — Domain Error Types & Sentinel Definitions.

### Sub-Phase 00.2: Core Error Architecture
* **P00-S02-M01: Domain Error Types & Sentinel Definitions**
  * *Status*: Completed
  * *Objective*: Define structured domain errors in `internal/errors`.
  * *Preconditions*: P00-S01-M03.
  * *Domain Errors Defined*:
    - Sentinels in `internal/errors/errors.go`: `ErrKeyNotFound`, `ErrEmptyKey`, `ErrKeyTooLarge`, `ErrValueTooLarge`, `ErrChecksumMismatch`, `ErrTornWrite`, `ErrCompactionRunning`.
    - Contextual typed errors: `KeyTooLargeError`, `ValueTooLargeError`, `ChecksumMismatchError`, `TornWriteError`, each implementing `Error() string` and `Is(target error) bool` matching its corresponding sentinel.
  * *Invariants*:
    - All domain errors implement `error` and participate in `errors.Is()` / `errors.As()`.
    - Leaf package isolation: `internal/errors` has zero internal dependencies to prevent import cycles.
    - Zero data leakage: raw key or value bytes are never formatted into error strings; only numeric sizes, limits, CRC32 checksums, and log offsets are logged.
  * *Tests*: Added `internal/errors/errors_test.go` verifying:
    - Sentinel identity and error message string checks.
    - Single and multi-level `%w` wrapping with `errors.Is()`.
    - Typed error `errors.Is()` matching and negative matches.
    - `errors.As()` field extraction (`KeySize`, `MaxSize`, `Expected`, `Actual`, `Offset`, `Reason`).
    - Negative extraction and cross-sentinel mismatch guarantees.
  * *Verification Performed*:
    - `go test -race -v ./...` (PASS, 0 race conditions, 8 test suites passing)
    - `go vet ./...` (Exit 0, static analysis clean)
    - `golangci-lint run ./...` (Exit 0, 0 issues)
    - `gofmt -l .` (Clean, zero unformatted files)
    - `go mod verify` (Exit 0, hermetic modules verified)
  * *Evidence Classification*:
    - *Design Target*: Minimal, cohesive domain error vocabulary allowing callers to differentiate operational conditions (missing key, size violations, torn write recovery).
    - *Theoretical Property*: `errors.Is` recursively unwraps errors and queries `Is(error) bool`, allowing typed structs to match sentinels without pointer equality.
    - *Measured Result*: 8 unit test suites in `internal/errors` passed under race detector (`-race`) in 1.585s with 0 linter warnings.
    - *Observed Limitation*: Leaf package `internal/errors` intentionally aliases standard library `import stdErrors "errors"` to avoid package namespace collision.
  * *Completion*: Domain error foundation established, tested, and verified.
  * *Next Micro-Phase*: P00-S02-M02 — Internal Logging Foundation.
* **P00-S02-M02: Internal Logging Foundation**
  * *Objective*: Implement lightweight structured logger in `internal/logger` wrapping Go's standard library `log/slog`.
  * *Preconditions*: P00-S02-M01.
  * *Changes*:
    - Created `internal/logger/doc.go` documenting zero dependencies, structured JSON/Text formats, subsystem scoping (`WithComponent`), privacy by design, concurrency safety, and testability.
    - Created `internal/logger/logger.go` with:
      - `Level` enum (`LevelDebug`, `LevelInfo`, `LevelWarn`, `LevelError`) and string mapping.
      - `Format` enum (`FormatJSON`, `FormatText`).
      - `Logger` interface with context methods (`DebugContext`, `InfoContext`, `WarnContext`, `ErrorContext`), attributes (`With`), and subsystem scoping (`WithComponent`).
      - Automated sensitive key redaction (`[REDACTED]`) via `ReplaceAttr` covering 13 baseline keywords (`password`, `secret`, `token`, `auth`, `authorization`, `api_key`, `apikey`, `private_key`, `credential`, `credentials`, `access_token`, `refresh_token`), compound/stem variants, and custom configured keys.
      - `Redactable` interface (`Redact() any`) for custom domain struct scrubbing.
      - `Err(err error) slog.Attr` for idiomatic error attribute integration.
      - `nopHandler` and `NewNop()` for zero-allocation silent logging in benchmarks.
  * *Security*: Automated defense-in-depth redaction in `ReplaceAttr`: sensitive attribute keys strictly take precedence over custom `Redactable` values (immediately replacing with `[REDACTED]` without evaluating `Redact()`), while non-sensitive keys evaluate `Redactable` with reflection nil guards and panic recovery.
  * *Tests Added*: `internal/logger/logger_test.go` covering:
    - Log level filtering (`LevelInfo` suppresses `Debug`).
    - JSON formatting and field structure validation (`time`, `level`, `msg`, attributes).
    - Text formatting for CLI/development.
    - Component scoping (`WithComponent("wal")`) and chaining.
    - Domain error integration via `Err(err)` with standard and domain error types.
    - Sensitive field redaction (default keys, casing variants, custom keys).
    - `Redactable` interface custom struct scrubbing.
    - `NewNop` zero-overhead drop behavior.
    - Contextual logging (`InfoContext`, etc.).
    - High-concurrency stress testing (100 goroutines logging 5,000 total records under `go test -race`).
  * *Verification Performed*:
    - `go build ./...` (PASS, 0 errors)
    - `go vet ./...` (PASS, 0 warnings)
    - `go test -race -v ./...` (PASS, 0 race conditions, 12 logger test suites passing)
    - `golangci-lint run ./...` (Exit 0, 0 issues)
    - `gofmt -l .` (Clean, zero unformatted files)
    - `go mod verify` (Exit 0, hermetic modules verified)
  * *Evidence Classification*:
    - *Design Target*: Lightweight structured logging adhering strictly to standard library with zero external dependencies and machine-parsable JSON lines.
    - *Theoretical Property*: Standard library `log/slog` handlers serialize writes to their underlying `io.Writer`, guaranteeing thread-safe record emission without garbled lines.
    - *Measured Result*: 100 concurrent goroutines writing 5,000 log events executed cleanly under `go test -race` with 0 data races and 100% JSON parsing and redaction integrity.
    - *Observed Limitation*: Runtime log rotation and remote log streaming are intentionally deferred to future operational phases; logging currently writes to configured `io.Writer` streams.
  * *Completion*: Logging foundation established, tested, and verified.
  * *Next Micro-Phase*: P01-S01-M01 — Big-Endian Fixed Integer Encoding & Decoding.

### Phase 00 Final Security Audit & Closeout Summary
* **Final Audit Result**: Completed — **PASS WITH REMEDIATIONS**
* **Vulnerabilities Discovered & Remediated**:
  1. *Typed-Nil Redactable Panic*: Interface nil check trap in `internal/logger/logger.go` causing nil pointer dereference on typed nil pointers implementing `Redactable`. Fixed via reflection nil detection and panic recovery in `safeRedact()`.
  2. *Nil Receiver Panics in Typed Domain Errors*: Calling `.Error()` on typed nil pointers of `KeyTooLargeError`, `ValueTooLargeError`, `ChecksumMismatchError`, and `TornWriteError` panicked with nil pointer dereference. Fixed via explicit `if e == nil` receiver guards returning sentinel messages.
  3. *Compound Key Redaction Bypass*: Sensitive keys like `db_password`, `client_secret`, `auth_token`, `session_token`, `api-key`, and `private-key` escaped exact-match map lookups. Fixed via hyphen normalization and stem pattern matching in `isSensitiveKey()`.
  4. *Sensitive Key vs Redactable Precedence Ambiguity*: Evaluating `a.Value.Any().(Redactable)` before `isSensitiveKey` permitted buggy or hostile `Redact()` implementations to leak secrets under sensitive keys. Remediated by enforcing strict precedence: sensitive keys immediately replace with `[REDACTED]` without invoking `Redact()`.
* **Permanent Regression Tests**: Added `TestNilReceiverTypedErrors` in `internal/errors/errors_test.go` and 12 security regression test suites in `internal/logger/logger_test.go`.
* **Phase 00 Verification Status**: All test suites passing under `go test -race -count=1 ./...`, `golangci-lint run ./...` reporting 0 issues, and `go mod verify` clean.
* **Phase 00 Status**: **COMPLETED & SEALED**. Phase 01 is ready to begin at `P01-S01-M01`.

---

# Phase 01: Core Storage Primitives & Binary Encodings

* **Major Objective**: Define zero-allocation byte representations, variable-length integer encoders, and CRC32 checksum pipelines.
* **Dependencies**: Phase 00.
* **Risks**: Endianness mismatches, integer overflow during varint decoding.

### Sub-Phase 01.1: Binary Encoding Primitives
* **P01-S01-M01: Big-Endian Fixed Integer Encoding & Decoding**
  * *Status*: **COMPLETE**
  * *Objective*: Implement zero-allocation uint16, uint32, uint64 encoders/decoders in `internal/binary`.
  * *Functions Implemented*:
    - `PutUint16(buf []byte, v uint16)`: Encodes 2-byte Big-Endian uint16 into `buf[0..1]`.
    - `GetUint16(buf []byte) uint16`: Decodes 2-byte Big-Endian uint16 from `buf[0..1]`.
    - `PutUint32(buf []byte, v uint32)`: Encodes 4-byte Big-Endian uint32 into `buf[0..3]`.
    - `GetUint32(buf []byte) uint32`: Decodes 4-byte Big-Endian uint32 from `buf[0..3]`.
    - `PutUint64(buf []byte, v uint64)`: Encodes 8-byte Big-Endian uint64 into `buf[0..7]`.
    - `GetUint64(buf []byte) uint64`: Decodes 8-byte Big-Endian uint64 from `buf[0..7]`.
  * *Invariants Verified*:
    - Invariant 1: Strict Big-Endian (Network Byte Order) byte layout across all integer widths.
    - Invariant 2: Round-trip identity `Get(Put(x)) == x` across all representable values.
    - Invariant 3: Zero side-effects on trailing bytes in oversized buffers (verified via canary bytes).
    - Invariant 4: No torn/partial writes on undersized buffers due to early BCE bounds checks (`_ = buf[width-1]`).
    - Invariant 5: Zero heap allocations empirically verified (`0 B/op`, `0 allocs/op`).
    - Invariant 6: Architecture-independent serialization (pure shifts/masks, identical output across CPU architectures).
  * *Tests Added* (`internal/binary/endian_test.go`):
    - Exact byte sequence tests against known vectors (`0`, `1`, `127`, `128`, `255`, `256`, `65535`, `65536`, `math.MaxUint16`, `math.MaxUint32`, `math.MaxUint64`, alternating bit patterns `0xAA..`, `0x55..`, high-bit `0x80..`).
    - Oversized buffer canary tests verifying isolation of subsequent indices.
    - Boundary panic tests (`nil`, empty, length `< required`) proving deterministic panics and absence of torn writes.
    - Differential testing against standard library `encoding/binary.BigEndian` across 10,000 property iterations per width.
    - Native Go fuzz testing (`FuzzUint16`, `FuzzUint32`, `FuzzUint64`) executing >3.8M iterations with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkPutUint16-10`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetUint16-10`: 0.24 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkPutUint32-10`: 0.25 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetUint32-10`: 0.25 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkPutUint64-10`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetUint64-10`: 0.25 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkRoundTripUint64-10`: 0.83 ns/op, 0 B/op, 0 allocs/op
  * *Security Review*: Verified absence of unsafe pointer manipulations, arithmetic overflows, or slice memory bleeding. Early bounds check triggers panic before writing, guaranteeing that malformed buffer lengths cannot result in corrupted partial writes.
  * *Evidence Classification*:
    - Design Target: Zero heap allocations, sub-nanosecond integer codecs.
    - Theoretical Property: Big-Endian network byte order guarantees platform-independent serialization.
    - Measured Result: 0 B/op, 0 allocs/op, ~0.23-0.25 ns/op, 100% statement coverage.
    - Documented Limitation: Fixed-width encoding always consumes 2, 4, or 8 bytes regardless of numeric magnitude (variable-width compression deferred to P01-S01-M02).
  * *Completion*: Unit tests, differential suites, and fuzz tests passing with 100% code coverage.
  * *Next Micro-Phase*: P01-S01-M02 — Unsigned Variable-Length Integer (Varint) Codec.
* **P01-S01-M02: Unsigned Variable-Length Integer (Varint) Codec**
  * *Status*: **COMPLETE**
  * *Objective*: Implement 7-bit varint codec for disk offsets and lengths.
  * *Functions Implemented*:
    - `PutVarint64(buf []byte, v uint64) int`: Encodes a uint64 into `buf` using canonical 7-bit varint encoding.
    - `GetVarint64(buf []byte) (uint64, int, error)`: Decodes a uint64 from `buf`, returning bytes consumed and error.
    - `VarintLen(v uint64) int`: Helper computing exact bytes required for encoding `v` ($1 \le n \le 10$).
    - `const MaxVarintLen64 = 10`: Maximum byte length for a 64-bit varint.
  * *Invariants Verified*:
    - Invariant 1: Values $\le 127$ consume exactly 1 byte.
    - Invariant 2: Canonical encodings strictly use the minimum required number of bytes (monotonic growth at boundaries 128, 16384, etc.).
    - Invariant 3: No valid uint64 value requires more than 10 bytes.
    - Invariant 4: Overflow enforcement: Malformed varints exceeding 10 bytes or with invalid payload bits in the 10th byte ($b > 1$) return `ErrVarintOverflow`.
    - Invariant 5: Truncation enforcement: Empty buffers or buffers terminating while continuation bit $0\text{x}80$ is set return `ErrVarintTruncated`.
    - Invariant 6: Anti-DoS Bounded Execution: Decoder loop executes at most 10 iterations regardless of input buffer size (verified against 1,000,000-byte attack streams).
    - Invariant 7: Anti-Tear Protection: `PutVarint64` verifies `len(buf) >= VarintLen(v)` upfront via `_ = buf[needed-1]`, preventing partial writes on undersized buffers.
    - Invariant 8: Zero heap allocations empirically verified across all value widths (`0 B/op`, `0 allocs/op`).
  * *Tests Added* (`internal/binary/varint_test.go`):
    - Exact byte sequence tests against known vectors (`0`, `1`, `127`, `128`, `129`, `255`, `256`, `16383`, `16384`, `2097151`, `2097152`, `268435455`, `268435456`, `math.MaxUint32`, $1 \ll 32$, $(1 \ll 63) - 1$, $1 \ll 63$, `math.MaxUint64`).
    - Oversized buffer canary tests verifying isolation of trailing bytes.
    - Negative boundary panic tests verifying anti-tear protection on undersized buffers.
    - Comprehensive truncation matrix (`nil`, empty, 1, 2, 3, and 9-byte continuation chains).
    - Overflow matrix (10th-byte continuation bit set, 10th-byte payload $> 1$, 11-byte to 15-byte chains).
    - Varint Bomb DoS bounded execution test (1,000,000 bytes of `0x80` rejected in sub-microsecond time).
    - Non-canonical overlong compatibility test.
    - Differential property testing against `encoding/binary.PutUvarint` and `Uvarint` (10,000 iterations).
    - Native Go fuzz testing (`FuzzGetVarint64` and `FuzzRoundTripVarint64`) executing >2.56M iterations with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkPutVarint64_1Byte`: 0.25 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkPutVarint64_2Bytes`: 0.73 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkPutVarint64_5Bytes`: 1.66 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkPutVarint64_10Bytes`: 2.60 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetVarint64_1Byte`: 0.78 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetVarint64_2Bytes`: 1.49 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetVarint64_5Bytes`: 2.74 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkGetVarint64_10Bytes`: 4.33 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkRoundTripVarint64`: 4.07 ns/op, 0 B/op, 0 allocs/op
  * *Security Review*: Verified that varint decoding work is strictly bounded to $\le 10$ iterations, preventing CPU exhaustion from malformed streams. Verified that 10th-byte validation prevents uint64 silent wrap-around on overflow.
  * *Evidence Classification*:
    - Design Target: Zero heap allocations, strict bounded loop execution.
    - Theoretical Property: 7-bit varints encode $\le 127$ in 1 byte and $2^{64}-1$ in at most 10 bytes.
    - Measured Result: 0 B/op, 0 allocs/op, ~0.25–4.3 ns/op latency, 100% statement coverage, 2.56M+ fuzz executions with 0 crashes.
    - Observed Limitation: Decoder accepts valid non-canonical encodings for compatibility with standard encoders, though Lattice's encoder strictly emits canonical minimal forms.
  * *Completion*: Unit tests, differential suites, and fuzz tests passing with 100% code coverage.
  * *Next Micro-Phase*: P01-S01-M03 — CRC32-IEEE Checksum Wrapper.
* **P01-S01-M03: CRC32-IEEE Checksum Wrapper**
  * *Status*: **COMPLETE**
  * *Objective*: Implement high-performance CRC32 calculator with hardware acceleration (`hash/crc32`).
  * *Functions Implemented*:
    - `Checksum(data []byte) uint32`: Computes CRC32-IEEE checksum over `data`.
    - `Verify(data []byte, expected uint32) bool`: Computes checksum and verifies exact match against `expected`.
  * *Invariants Verified*:
    - Invariant 1: Standard IEEE 802.3 polynomial (`0xEDB88320`).
    - Invariant 2: Canonical vector `"123456789"` strictly yields `0xCBF43926`.
    - Invariant 3: `nil` and empty byte slices strictly yield `0x00000000`.
    - Invariant 4: Non-mutating contract: neither `Checksum` nor `Verify` alters any byte of input data.
    - Invariant 5: Concurrency-safety: zero shared mutable state, verified across 100 concurrent goroutines under `go test -race`.
    - Invariant 6: Zero heap allocations empirically verified across all input payload sizes (`0 B/op`, `0 allocs/op`).
    - Invariant 7: Deterministic: returns identical checksums across heterogeneous CPU architectures.
  * *Tests Added* (`internal/binary/crc_test.go`):
    - Authoritative known vectors (`nil`, `""`, `"123456789"`, `"a"`, `"abc"`, `"message digest"`, `"The quick brown fox jumps over the lazy dog"`, `"Lattice"`, all-zero, all-0xFF, sequential bytes `0x00..0xFF`).
    - Independent bit-by-bit software simulation reference oracle (`referenceCRC32IEEE`) cross-verifying without standard library circularity.
    - Differential randomized property testing against independent oracle and `hash/crc32.ChecksumIEEE` across 19 lengths (0 to 4096 bytes) and 1,900 iterations.
    - Data isolation canary test verifying input buffers are untouched.
    - Single-bit corruption detection across 10 sample byte offsets in a 1KB block, flipping all 8 bits.
    - Multi-byte corruption, byte-swap, truncation, and trailing byte append detection.
    - Concurrency test with 100 parallel goroutines and 500 iterations each.
    - Native Go fuzzing (`FuzzChecksum`) executing ~3,000,000 iterations with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkChecksum_Empty`: 3.88 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkChecksum_64B`: 4.63 ns/op (13.8 GB/s), 0 B/op, 0 allocs/op
    - `BenchmarkChecksum_1KB`: 79.69 ns/op (12.8 GB/s), 0 B/op, 0 allocs/op
    - `BenchmarkChecksum_4KB`: 343.9 ns/op (11.9 GB/s), 0 B/op, 0 allocs/op
    - `BenchmarkChecksum_64KB`: 5,605 ns/op (11.7 GB/s), 0 B/op, 0 allocs/op
    - `BenchmarkChecksum_1MB`: 89,590 ns/op (11.7 GB/s), 0 B/op, 0 allocs/op
    - `BenchmarkVerify_4KB`: 349.9 ns/op (11.7 GB/s), 0 B/op, 0 allocs/op
  * *Security Review*: Confirmed CRC32 is strictly an error-detection code for accidental corruption (media bit rot, torn writes), not a cryptographic hash. It provides no authentication or collision resistance against malicious tampering. Non-mutating and concurrency-safe with zero allocations.
  * *Evidence Classification*:
    - Design Target: Zero heap allocations, boolean verify contract, concurrency safety.
    - Theoretical Property: CRC32-IEEE polynomial detects all odd numbers of bit errors, all double bit errors for block sizes within Hamming distance limits, and any single burst error of length $\le 32$ bits.
    - Measured Result: 0 B/op, 0 allocs/op, ~11.7–13.8 GB/s throughput, 100% statement coverage, ~3.0M fuzz executions with 0 crashes.
    - Observed Limitation: Non-cryptographic; an attacker with write access to both payload and checksum can trivially forge a valid CRC32.
  * *Completion*: Unit tests, differential suites, and fuzz tests passing with 100% code coverage.
  * *Next Micro-Phase*: P01-S02-M01 — Key & Value Boundary Constraints & Validation.

### Sub-Phase 01.2: Database Key & Value Models
* **P01-S02-M01: Key & Value Boundary Constraints & Validation**
  * *Status*: **COMPLETE**
  * *Objective*: Implement validation functions enforcing $1 \le \text{KeyLen} \le 65,535$ and $0 \le \text{ValLen} \le 4\text{MB}$.
  * *Functions & Constants Implemented*:
    - `ValidateKey(key []byte) error`: Enforces $1 \le \text{len(key)} \le 65,535$ bytes. Returns `ErrEmptyKey` for len 0, `*KeyTooLargeError` for len $> 65,535$, and `nil` for valid keys.
    - `ValidateValue(val []byte) error`: Enforces $0 \le \text{len(val)} \le 4,194,304$ bytes (4 MiB). Returns `*ValueTooLargeError` for len $> 4,194,304$, and `nil` for valid values (including empty/nil).
    - `MinKeyLen = 1`, `MaxKeyLen = 65535`, `MaxKeyBytes = 65535`.
    - `MinValueLen = 0`, `MaxValueLen = 4194304`, `MaxValueBytes = 4194304`.
  * *Invariants Verified*:
    - Invariant 1: Key lower bound: empty/nil keys strictly return `ErrEmptyKey`.
    - Invariant 2: Key upper bound: keys $> 65,535$ bytes return `*KeyTooLargeError` matching `ErrKeyTooLarge` via `errors.Is`.
    - Invariant 3: Value lower bound: zero-length and nil values are valid valueless markers (returns `nil`).
    - Invariant 4: Value upper bound: values $> 4,194,304$ bytes return `*ValueTooLargeError` matching `ErrValueTooLarge` via `errors.Is`.
    - Invariant 5: $O(1)$ complexity: inspects only slice header length; never scans, hashes, or copies payload.
    - Invariant 6: Input immutability: neither `ValidateKey` nor `ValidateValue` modifies any byte of input.
    - Invariant 7: Binary safety: handles arbitrary bytes (null bytes, 0xFF) transparently.
    - Invariant 8: UTF-8 multibyte byte-length invariant: enforces raw byte count, not character/rune count.
    - Invariant 9: Zero heap allocations on valid inputs (`0 B/op`, `0 allocs/op`).
  * *Tests Added* (`internal/binary/validate_test.go`):
    - Table-driven boundary tests ($max-1, max, max+1$) for keys (65,534B, 65,535B, 65,536B) and values (4,194,303B, 4,194,304B, 4,194,305B).
    - Nil and empty slice tests for both keys and values.
    - Error compatibility tests: `errors.Is` and `errors.As` extraction of `KeySize`, `ValueSize`, and `MaxSize`.
    - Input immutability canary test.
    - Binary safety tests (null bytes, 0xFF, control characters).
    - Multibyte UTF-8 byte-length test verifying 4-byte runes are bounded by byte size.
    - Native Go fuzzing (`FuzzValidateKey`, `FuzzValidateValue`) executing >5.84M iterations with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkValidateKey_Empty`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateKey_16B`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateKey_1KB`: 0.22 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateKey_65535B`: 0.22 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateValue_Empty`: 0.22 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateValue_1KB`: 0.22 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateValue_1MB`: 0.22 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkValidateValue_4MB`: 0.22 ns/op, 0 B/op, 0 allocs/op
  * *Security Review*: Verified $O(1)$ constant-time admission check eliminates memory exhaustion and CPU scanning vectors. Confirmed typed errors omit raw payload data, preventing sensitive credential leakage in log streams.
  * *Evidence Classification*:
    - Design Target: Zero heap allocations on valid paths, $O(1)$ admission without payload copies.
    - Theoretical Property: Slice header length check is strictly $O(1)$ independent of payload size.
    - Measured Result: 0 B/op, 0 allocs/op, ~0.22-0.23 ns/op across all payload sizes (16B to 4MB), 100% statement coverage, >5.84M fuzz executions with 0 crashes.
    - Observed Limitation: Validation is purely structural; it does not verify semantic schema constraints (which belongs to user applications).
  * *Completion*: Unit tests, boundary suites, and fuzz tests passing with 100% code coverage.
  * *Next Micro-Phase*: P01-S02-M02 — Operation Type & Sequence Number Abstractions.
* **P01-S02-M02: Operation Type & Sequence Number Abstractions**
  * *Status*: **COMPLETE**
  * *Objective*: Define `OpType byte` (`0x00 = INVALID`, `0x01 = PUT`, `0x02 = DELETE/TOMBSTONE`) and monotonic `SeqNum uint64`.
  * *Functions & Types Implemented*:
    - `type OpType byte`: Constants `OpTypeInvalid = 0x00`, `OpTypePut = 0x01`, `OpTypeDelete = 0x02`, `OpTypeTombstone = OpTypeDelete`.
    - `(op OpType) Valid() bool`: Reports true only for `OpTypePut` and `OpTypeDelete`.
    - `(op OpType) Validate() error`: Returns `nil` for valid operations, `*errors.InvalidOpTypeError` for invalid operations.
    - `(op OpType) String() string`: Returns `"PUT"`, `"DELETE"`, or `"UNKNOWN(0x..)"`.
    - `ParseOpType(b byte) (OpType, error)`: Parses and validates raw byte into `OpType`.
    - `type SeqNum uint64`: Constants `MinSeqNum = 0`, `MaxSeqNum = math.MaxUint64` ($18,446,744,073,709,551,615$).
    - `(s SeqNum) Next() (SeqNum, error)`: Returns $s + 1$, or `*errors.SeqNumOverflowError` on `MaxSeqNum`.
    - `(s SeqNum) String() string`: Decimal string representation via `strconv.FormatUint`.
    - Sentinel & Typed Errors: `ErrInvalidOpType`, `ErrSeqNumOverflow`, `InvalidOpTypeError`, `SeqNumOverflowError` in `internal/errors`.
  * *Invariants Verified*:
    - Invariant 1: `OpType` boundary: exactly `0x01` and `0x02` are valid; all other 254 byte values (`0x00`, `0x03`–`0xFF`) are invalid and rejected.
    - Invariant 2: `OpType` zero-value semantics: `OpType(0x00)` is `OpTypeInvalid`, fails `Valid()`, and returns `ErrInvalidOpType`.
    - Invariant 3: `SeqNum` width: strictly 64-bit unsigned integer (`uint64`), spanning $[0, 2^{64}-1]$.
    - Invariant 4: `SeqNum` overflow protection: `MaxSeqNum.Next()` returns `ErrSeqNumOverflow` without silent wraparound to 0.
    - Invariant 5: `SeqNum` ordering: strict total ordering natively supported by Go relational operators (`<`, `<=`, `>`, `>=`).
    - Invariant 6: Type safety: distinct Go types prevent accidental interchange between opcodes, sequence numbers, lengths, and raw bytes.
    - Invariant 7: Zero heap allocations on valid paths: `0 B/op`, `0 allocs/op` for `Valid`, `Validate`, `ParseOpType`, and `Next`.
  * *Tests Added* (`internal/binary/types_test.go`, `internal/errors/errors_test.go`):
    - Exhaustive 256-byte loop testing `Valid()` and `Validate()`.
    - Zero-value `OpType` tests.
    - `ParseOpType` table-driven tests with error extraction via `errors.Is` and `errors.As`.
    - `SeqNum` boundary progression: 0, 1, 42, 1000, 1000000, `MaxSeqNum - 1`, `MaxSeqNum`.
    - `SeqNum` strict total ordering and descending sort verification.
    - Fuzz testing: `FuzzParseOpType` (2.58M executions) and `FuzzSeqNumNext` (2.92M executions) with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkOpType_Valid`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkOpType_Validate_Valid`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkOpType_String_Put`: 0.24 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkParseOpType_Valid`: 0.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkSeqNum_Next_Valid`: 0.22 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkSeqNum_String`: 11.82 ns/op, 16 B/op, 1 allocs/op
  * *Security Review*: Strict byte-level validation rejects unrecognized opcodes from untrusted network/disk inputs. 64-bit sequence overflow protection eliminates silent wraparound corruption.
  * *Evidence Classification*:
    - Design Target: Strongly typed database primitives with zero-allocation validation and overflow protection.
    - Theoretical Property: 64-bit unsigned sequence numbers provide $1.84 \times 10^{19}$ distinct states, lasting $>584,000$ years at 1M writes/sec.
    - Measured Result: 0 B/op, 0 allocs/op on all valid paths, 100% statement coverage in `internal/binary`, >5.50M fuzz executions with 0 crashes.
    - Observed Limitation: `SeqNum` type abstraction does not itself allocate or assign sequence numbers; monotonic sequence allocation is coordinated by higher-level storage managers (WAL & engine).
  * *Completion*: Unit tests, boundary suites, and fuzz tests passing with 100% code coverage.
  * *Next Micro-Phase*: P01-S02-M03 — InternalKey Data Model & Comparator.
* **P01-S02-M03: InternalKey Data Model & Comparator**
  * *Status*: **COMPLETE**
  * *Objective*: Define `InternalKey` struct (`UserKey []byte`, `SeqNum SeqNum`, `OpType OpType`) and bidirectional binary encoding/decoding.
  * *Functions & Types Implemented*:
    - `type InternalKey struct { UserKey []byte; SeqNum SeqNum; OpType OpType }`
    - `InternalKeyTrailerLen = 9` (8 bytes SeqNum + 1 byte OpType)
    - `NewInternalKey(userKey []byte, seqNum SeqNum, opType OpType) (InternalKey, error)`: Validates inputs via `ValidateKey` and `opType.Validate()`, making an owned copy of `userKey`.
    - `(k InternalKey) Clone() InternalKey`: Returns deep copy of InternalKey with cloned UserKey slice.
    - `(k InternalKey) String() string`: Human-readable diagnostic formatting with `%q` escaped key.
    - `(k InternalKey) Equal(other InternalKey) bool`: Reports whether `CompareInternalKey(k, other) == 0`.
    - `(k InternalKey) Compare(other InternalKey) int`: Receiver method calling `CompareInternalKey(k, other)`.
    - `CompareInternalKey(a, b InternalKey) int`: Canonical comparator (UserKey ascending $\to$ SeqNum descending $\to$ OpType descending).
    - `AppendInternalKey(dst []byte, key InternalKey) []byte`: Appends UserKey + 8-byte Big-Endian SeqNum + 1-byte OpType.
    - `EncodeInternalKey(key InternalKey) []byte`: Serializes into newly allocated byte slice.
    - `DecodeInternalKey(data []byte) (InternalKey, error)`: Deserializes, validates, and returns owned InternalKey.
    - Sentinel error `ErrInternalKeyTruncated` in `internal/errors`.
  * *Invariants Verified*:
    - Invariant 1: Canonical ordering: UserKey ascending (raw byte lexicographical order) $\to$ SeqNum descending (higher sequence numbers sort before lower ones) $\to$ OpType descending (DELETE sorts before PUT).
    - Invariant 2: Mathematical comparator laws: reflexivity (`Compare(x, x) == 0`), antisymmetry (`sign(Compare(x, y)) == -sign(Compare(y, x))`), transitivity (if $x < y$ and $y < z \implies x < z$).
    - Invariant 3: LSM newest-first search invariant: for any given user key, higher sequence numbers sort earlier, guaranteeing that point lookups and iterators encounter the newest revision first.
    - Invariant 4: Ownership & immutability: constructor `NewInternalKey` and `DecodeInternalKey` make owned defensive copies, isolating internal state from caller buffer mutations. Direct struct initialization enables zero-allocation borrowed usage for hot internal search loops.
    - Invariant 5: Zero heap allocations on comparison and append paths (`0 B/op`, `0 allocs/op`).
    - Invariant 6: Binary layout: $N + 9$ bytes (`[ UserKey | SeqNum (8B Big-Endian) | OpType (1B) ]`), correctly bounded by $10 \le \text{len} \le 65,544$ bytes.
  * *Tests Added* (`internal/binary/internalkey_test.go`, `internal/errors/errors_test.go`):
    - Construction and boundary validation (1B, typical, 65,535B, empty, oversized, invalid OpType).
    - Defensive copy caller mutation protection test.
    - Comparator ordering matrix (UserKey ascending, SeqNum descending, OpType descending).
    - Mathematical comparator laws across sample keys and 5,000 randomized triples.
    - Sorting integration with `sort.Slice` verifying newest versions emerge first.
    - Binary round-trip encoding/decoding and error handling (truncated, oversized, corrupted opcode).
    - Native Go fuzzing: `FuzzCompareInternalKey` (2.92M iterations) and `FuzzInternalKeyRoundTrip` (2.89M iterations) with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkCompareInternalKey_16B`: 2.49 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkCompareInternalKey_1KB`: 24.72 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkCompareInternalKey_64KB`: 1541 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkCompareInternalKey_SameKey_DifferentSeq`: 2.23 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkAppendInternalKey`: 3.02 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkDecodeInternalKey`: 8.13 ns/op, 24 B/op, 1 allocs/op
  * *Security Review*: Defensive slice copying eliminates caller-aliased memory mutation exploits. Three-way comparator guarantees strict weak ordering, preventing sorting panics or infinite binary search loops.
  * *Evidence Classification*:
    - Design Target: Deterministic multi-version key representation and allocation-free comparator.
    - Theoretical Property: Comparator provides a strict weak ordering (total ordering when all 3 fields match), satisfying the prerequisites of binary search and priority queues.
    - Measured Result: 0 B/op, 0 allocs/op for comparator, 100% statement coverage in `internal/binary`, >5.82M fuzz executions with 0 crashes.
    - Observed Limitation: UserKey comparison complexity is $O(\min(len_a, len_b))$; sequence and opcode tie-breaking is $O(1)$.
  * *Completion*: Unit tests, boundary suites, fuzz suites, and benchmarks passing with 100% code coverage. Sub-Phase 01.2 and Phase 01 COMPLETE.
  * *Next Micro-Phase*: P02-S01-M01 — WAL Record Header & Framing Definition.

---

# Phase 02: Write-Ahead Log (WAL) & Durability Subsystem

* **Major Objective**: Implement append-only binary WAL logging, CRC32 verification, segment rotation, and cooperative Group Commit.
* **Dependencies**: Phase 01.
* **Risks**: Torn writes, un-synced page cache data loss, disk space exhaustion.

### Sub-Phase 02.1: WAL Binary Record Layout & Serialization
* **P02-S01-M01: WAL Record Header & Framing Definition**
  * *Status*: **COMPLETE**
  * *Objective*: Implement struct and serialization for 21-byte WAL header (`CRC32`, `Type`, `SeqNum`, `Timestamp`).
  * *Data Structures & Functions Implemented*:
    - `HeaderSize = 21`, `RecordHeaderLen = 21`: Fixed-size physical binary header constants.
    - `RecordType byte`: 1-byte operation/framing marker (`RecordTypeInvalid=0x00`, `RecordTypePut=0x01`, `RecordTypeDelete=0x02`, `RecordTypeBatchStart=0x03`, `RecordTypeBatchCommit=0x04`).
    - `(t RecordType) Valid() bool`, `(t RecordType) Validate() error`, `(t RecordType) String() string`.
    - `(t RecordType) OpType() (binary.OpType, error)`: Typed bridge converting WAL operation types to storage engine primitives.
    - `ParseRecordType(b byte) (RecordType, error)`: Safe parser rejecting uninitialized or unknown bytes.
    - `RecordHeader`: Struct containing `CRC uint32`, `Type RecordType`, `SeqNum binary.SeqNum`, `Timestamp uint64`.
    - `EncodeHeader(buf []byte, h RecordHeader)`: Serializes 21-byte header with early BCE bounds check (`_ = buf[20]`).
    - `AppendHeader(dst []byte, h RecordHeader) []byte`: Appends 21-byte serialized header to slice with zero allocations on sufficient capacity.
    - `DecodeHeader(buf []byte) (RecordHeader, error)`: Deserializes 21-byte header, returning `ErrHeaderTruncated` if undersized or `*InvalidRecordTypeError` if corrupt.
  * *Invariants Verified*:
    - Invariant 1: Header size is strictly fixed at 21 bytes (`CRC32` 4B + `Type` 1B + `SeqNum` 8B + `Timestamp` 8B).
    - Invariant 2: Strict Big-Endian integer serialization using `internal/binary` primitives.
    - Invariant 3: Zero-tear bounds check: `EncodeHeader` triggers runtime bounds panic before mutating undersized buffers (`_ = buf[HeaderSize-1]`).
    - Invariant 4: Oversized buffer isolation: bytes beyond index 20 are untouched during encode and ignored during decode.
    - Invariant 5: Zero heap allocations empirically verified (`0 B/op`, `0 allocs/op`).
  * *Tests Added* (`internal/wal/record_test.go`):
    - Constant identity and exact physical byte offset alignment against known hex vectors.
    - Round-trip serialization across edge cases (min/max SeqNum, max uint64 timestamp, all RecordType variants).
    - Truncated buffer tests (lengths 0..20) verifying `ErrHeaderTruncated`.
    - Corrupt RecordType tests (0x00, 0x05, 0xFF) verifying `ErrInvalidRecordType` and `*InvalidRecordTypeError`.
    - Anti-tear panic tests ensuring zero bytes are mutated on undersized writes.
    - High-concurrency race tests across 64 goroutines and 1,000 iterations each.
    - Native Go fuzz testing (`FuzzRecordHeaderCodec`) executing >3.2M iterations with 0 crashes.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkEncodeHeader-10`: 0.99 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkDecodeHeader-10`: 1.48 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkAppendHeader-10`: 1.36 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkRecordType_Validate-10`: 0.23 ns/op, 0 B/op, 0 allocs/op
  * *Security Review*: Verified rejection of uninitialized memory (0x00 is invalid). Early bounds check prevents buffer bleeding and torn writes. All integer fields serialized with fixed-width big-endian routines without unsafe memory manipulation.
* **P02-S01-M02: WAL Full Record Serializer & Deserializer**
  * *Objective*: Implement full physical record serialization and stream-safe deserialization (`Header (21B) || KeyLen (2B) || KeyBytes || ValLen (4B) || ValBytes`).
  * *Changes*:
    - Defined `Record` struct with `CRC uint32`, `Type RecordType`, `SeqNum binary.SeqNum`, `Timestamp uint64`, `Key []byte`, `Value []byte`.
    - Implemented `(r Record) Validate() error` enforcing type rules: PUT (1B..64KB key, 0..4MB val), DELETE (1B..64KB key, 0B val tombstone), BATCH_START / BATCH_COMMIT (0B key, 0B val markers).
    - Implemented `(r Record) Equal(other Record) bool`, `(r Record) Header() RecordHeader`, and `(r Record) String() string` (redacting raw payload data).
    - Implemented `AppendRecord(dst []byte, record Record) ([]byte, error)`: zero-allocation record append when capacity is available.
    - Implemented `EncodeRecord(record Record) ([]byte, error)`: exactly one heap allocation.
    - Implemented `DecodeRecord(r io.Reader) (Record, error)`: stream-safe deserializer with hostile-reader defenses, partial I/O handling, anti-DoS length checks before allocation, and streaming CRC32-IEEE verification.
    - Added `ErrInvalidRecordPayload` sentinel and `InvalidRecordPayloadError` struct in `internal/errors`.
  * *Invariants & Safety Properties*:
    - **Physical Layout**: `[CRC32 (4B) | RecordType (1B) | SeqNum (8B) | Timestamp (8B) | KeyLen (2B) | Key (Var) | ValLen (4B) | Val (Var)]`.
    - **CRC Coverage**: CRC32-IEEE computed across all bytes following the 4-byte CRC field: `RecordType || SeqNum || Timestamp || KeyLen || Key || ValLen || Val`.
    - **Anti-DoS Early Allocation Bounding**: 16-bit key length and 32-bit value length validated against authoritative storage limits (`binary.MaxKeyLen = 65,535` and `binary.MaxValueLen = 4,194,304`) BEFORE allocating memory buffers, preventing resource exhaustion attacks.
    - **Memory Ownership**: Decoded slices are newly allocated and strictly owned by the returned `Record`, guaranteeing zero aliasing with reader buffers or future decode calls. Callers' input slices are never mutated during encoding.
  * *Tests Performed*: Complete test suite covering requirements A through AB:
    - Minimal valid records (PUT 1B key / 0B val, BATCH 0B key / 0B val).
    - Boundary limits: Maximum key (65,535 bytes) and maximum value (4,194,304 bytes / 4 MiB).
    - Empty value handling (both `nil` and `[]byte{}`).
    - All four record types (PUT, DELETE tombstone, BATCH_START, BATCH_COMMIT).
    - Minimum and maximum sequence numbers (0 and `math.MaxUint64`).
    - Minimum and maximum timestamps (0 and `math.MaxUint64`).
    - Exact known CRC vector byte verification.
    - Single-byte bit-flip corruption across all 7 record fields (RecordType, SeqNum, Timestamp, KeyLength, Key, ValueLength, Value) verifying `ErrChecksumMismatch`.
    - Truncated headers (0 bytes clean `io.EOF`, 1..20 bytes `ErrHeaderTruncated` and `io.ErrUnexpectedEOF`).
    - Truncated keys (key length, key bytes) and truncated values (value length, value bytes) returning `io.ErrUnexpectedEOF`.
    - Invalid record types (0x00, 0x05, 0x7F, 0xFF) returning `*InvalidRecordTypeError`.
    - KeyLength above maximum returning `*KeyTooLargeError`.
    - ValueLength above maximum returning `*ValueTooLargeError`.
    - Malicious 32-bit lengths (`math.MaxUint32`, 2 GiB, 256 MiB) failing fast without memory allocation.
    - Stream fragmentation tests: arbitrary chunk sizes (1, 2, 3, 5, 7, 11, 17, 31 bytes), single-byte readers (1 byte/read), short readers returning fewer bytes than requested with nil error, and readers returning remaining bytes with `io.EOF` simultaneously.
    - Round-trip property tests across 500 randomized records.
    - Deterministic encoding: multiple encodes produce identical bytes.
    - Input immutability: mutating caller slices does not alter encoded stream.
    - Decoded slice ownership and aliasing independence.
    - Integer arithmetic boundary conditions.
    - Native Go fuzzing (`FuzzRecordCodec`) executing >2.92M iterations with 0 crashes, 0 panics, and 100% round-trip structural stability.
  * *Benchmark Results* (Apple M4, Darwin arm64, Go 1.24):
    - `BenchmarkAppendRecord_ReusedBuffer-10`: 20.10 ns/op, 0 B/op, 0 allocs/op
    - `BenchmarkEncodeRecord_Small-10`: 43.04 ns/op, 192 B/op, 1 allocs/op
    - `BenchmarkEncodeRecord_Large-10`: 9085 ns/op, 73728 B/op, 1 allocs/op
    - `BenchmarkDecodeRecord_Small-10`: 91.54 ns/op, 192 B/op, 5 allocs/op
    - `BenchmarkDecodeRecord_Large-10`: 9105 ns/op, 66592 B/op, 5 allocs/op
  * *Evidence Classification*:
    - **Design Target**: High-throughput binary streaming codec without unbounded memory allocation on corrupt or hostile inputs.
    - **Theoretical Property**: CRC32-IEEE covers all post-CRC bytes deterministically; zero allocation for AppendRecord with reused buffer; total memory isolation of decoded slices.
    - **Measured Result**: 20.10 ns/op (0 allocs) append, 43.04 ns/op encode, 91.54 ns/op decode; >2.92M fuzz iterations without panic; 98.9% statement coverage.
    - **Observed Limitation**: DecodeRecord allocates fresh memory slices for user key and value payloads to guarantee caller ownership without buffer aliasing.
  * *Completion*: Full record serialization, stream-safe deserialization, hostile input defenses, and test suites verified.
* **P02-S01-M03: WAL Corruption & Checksum Verification Tests**
  * *Objective*: Prove that corrupted bytes are intercepted across all fields of the physical record, verify CRC32-IEEE integrity, and validate the boundary between structural format rejection and checksum rejection.
  * *Changes*:
    - Created `internal/wal/corruption_test.go` implementing 7 comprehensive corruption and checksum verification test suites.
    - `TestCRCFieldCorruption`: Tested mutating each byte of the CRC field (offsets 0..3) across all 8 bit positions (32 tests). Proved that every mutation produces `ErrChecksumMismatch` carrying structured `Expected` and `Actual` CRC diagnostics.
    - `TestCRCScopeCoverage`: Independently verified CRC32-IEEE over `RecordType || SeqNum || Timestamp || KeyLength || Key || ValueLength || Value` using `binary.Checksum`. Confirmed stored CRC equals independent CRC, proved CRC field itself is strictly excluded, and proved mutating any field in the layout alters the checksum.
    - `TestExhaustiveSingleBitCorruption`: Deterministic exhaustive single-bit flip test across all 59 post-CRC payload bytes (472 bit flips). Proved 100% interception (419 checksum mismatches, 53 structural rejections, 0 silent accepts), and verified that bit restoration cleanly restores valid decoding.
    - `TestStructuralVsChecksumRejectionMatrix`: Mapped and verified boundary between early structural rejections (`ErrInvalidRecordType`, `ErrValueTooLarge`, `ErrInvalidRecordPayload`) and checksum rejections (`ErrChecksumMismatch`).
    - `TestCorruptionSemantics_ReversibilityAndCollisions`: Validated the 5 corruption semantics (mutation detected, original decodes, restoration recovers, 50 distinct random mutations do not collide into valid records, and 100-run verification determinism).
    - `TestDeterministicRandomizedCorruption`: 1,000 deterministic seeded randomized tests across all 4 record types (PUT, DELETE, BATCH_START, BATCH_COMMIT) with variable key/value sizes; 100% intercepted.
    - `TestCRCCollisionCaveat_TheoreticalLimits`: Documented and demonstrated difference between error detection and cryptographic integrity.
  * *Tests & Benchmarks*:
    - `go test -count=1 -race ./internal/wal/...`: PASS (49 suites)
    - `go test -count=1 -race ./...`: PASS across all packages
    - `FuzzRecordCodec`: >2.87M iterations in 5s with 0 crashes
    - `golangci-lint run ./...`: 0 issues
    - Code coverage: 99.4% of statements in `internal/wal`
  * *Evidence Classification*:
    - **Design Target**: 100% detection of bit rot, torn writes, and data corruption across WAL records without compromising structural format validation or anti-DoS bounds.
    - **Theoretical Property**: CRC32-IEEE guarantees detection of all single-bit flips and error bursts up to polynomial bounds; random collision probability is $2^{-32} \approx 2.33 \times 10^{-10}$; CRC is an error-detecting code, not a cryptographic authentication mechanism.
    - **Measured Result**: 472/472 exhaustive bit flips detected (419 checksum mismatches, 53 structural errors, 0 silent accepts); 32/32 CRC byte bit flips produced `ErrChecksumMismatch`; 1000/1000 randomized records intercepted; 99.4% statement coverage in `internal/wal`.
    - **Observed Limitation**: CRC32 does not prevent intentional record tampering where an attacker recomputes the checksum; cryptographic integrity would require an HMAC or signature layer.
  * *Completion*: WAL corruption and checksum verification suites complete and fully validated.

### Sub-Phase 02.2: WAL File Management & Append Operations
* **P02-S02-M01: WAL File Creator & Directory Initializer**
  * *Objective*: Safely initialize `<db_path>/wal/` directory with restrictive `0700` POSIX permissions, preventing unauthorized local access, avoiding TOCTOU races, rejecting symlinks, and maintaining idempotent convergence.
  * *Changes*:
    - Extended `internal/errors` with `ErrNotADirectory` sentinel and `NotADirectoryError{Path, Mode}` typed diagnostic error.
    - Implemented `internal/wal/dir.go` with constants `DirName = "wal"`, `DirMode = 0700`, path constructors `Dir(dbPath)` and `DirPath(dbPath)`, and directory initializer `InitDir(dbPath string) (string, error)`.
    - Enforced atomic directory creation via `os.Mkdir(walPath, DirMode)` to eliminate check-then-create TOCTOU races.
    - Implemented `os.Lstat` inspection on pre-existing paths, rejecting regular files, symlinks, and non-directory objects with `*errors.NotADirectoryError`.
    - Hardened existing-directory permission updates: opens the directory handle directly, verifies `os.SameFile(info, finfo)` to pin the exact directory inode, executes descriptor-based `f.Chmod(DirMode)` (`fchmod`) to prevent symlink-swap traversal attacks, and re-validates inode continuity post-chmod.
    - Added comprehensive unit, concurrency, failure mode, inode-preservation, and adversarial test suite in `internal/wal/dir_test.go` covering Scenarios A through R.
  * *Tests & Verification*:
    - `go test -count=1 -race ./internal/wal/...`: PASS (59 suites, including 10 directory suites)
    - `go test -count=1 -race ./...`: PASS across all packages
    - `golangci-lint run ./...`: 0 issues
    - Statement coverage: 98.7% in `internal/wal`, 100% in `internal/errors`
  * *Evidence Classification*:
    - **Design Target**: Secure, race-resistant, and idempotent initialization of `<db_path>/wal/` with `0700` POSIX mode (`rwx------`), protecting database logs against group/other local access.
    - **Theoretical Property**: Direct atomic `os.Mkdir` syscall eliminates check-then-create TOCTOU races; `os.Lstat` detects and rejects symlinks at the WAL path; descriptor-based `fchmod` and `os.SameFile` pin the verified directory inode and prevent symlink-substitution redirection during permission hardening; umask cannot add group/other permissions when `0700` is requested.
    - **Measured Result**: Verified 0700 mode on fresh creation; verified descriptor-based permission tightening on 0777 pre-existing directories; verified inode preservation across repeated and hardening calls; 100% error detection on regular files, symlinks, and non-existent parents; 50 goroutines safely converged without race or data corruption; pre-existing WAL files preserved without modification.
    - **Observed Limitation**: Unprivileged local attacks are mitigated provided the parent directory `dbPath` permissions are properly restricted; a fully privileged (root/superuser) host attacker can bypass all OS permission checks; on non-POSIX filesystems (e.g. Windows), directory permission bits do not reflect POSIX 0700 semantics; directory initialization creates `<db_path>/wal` but does not establish storage media crash durability until log files are flushed and synced.
  * *Completion*: WAL directory creator and initializer verified and complete.
* **P02-S02-M02: Synchronous WAL Appender (`Strict Sync`)**
  * *Objective*: Implement sequential file writer calling `file.Write()` followed by `fdatasync()` adhering to the Strict Sync durability contract.
  * *Changes*:
    - Added `ErrWriterClosed` sentinel error in `internal/errors/errors.go` and unit tests in `internal/errors/errors_test.go`.
    - Added `internal/wal/sync_linux.go` using Linux kernel `syscall.Fdatasync(int(f.Fd()))` for zero-dependency data page synchronization without syncing unchanged inode metadata.
    - Added `internal/wal/sync_fallback.go` falling back to `f.Sync()` on non-Linux platforms (Darwin, Windows) with documented platform limitations.
    - Implemented `WALWriter` in `internal/wal/writer.go`:
      - `const FileMode os.FileMode = 0600`: Restrictive POSIX mode for WAL segment files.
      - Canonical segment naming helpers `SegmentName(id uint64) string` (`wal_%012d.log`) and `SegmentPath(dbPath string, id uint64) string`.
      - `OpenWriter(path string) (*WALWriter, error)` and `OpenSegmentWriter(dbPath string, id uint64) (*WALWriter, error)` with `O_WRONLY | os.O_CREATE | os.O_APPEND`.
      - Pre-open symlink/directory rejection, post-open `os.SameFile` inode pinning, and descriptor cleanup on failure paths.
      - `(w *WALWriter) AppendSync(rec Record) error`: Validates record, serializes via `EncodeRecord`, executes write loop handling short writes, and flushes via `w.syncFn`.
      - Strict sync contract: returns `nil` if and only if all bytes were written AND `fdatasync` succeeded; fails fast if closed or on write/sync errors.
      - Concurrency safety: internal `sync.Mutex` serializes concurrent `AppendSync` calls, preventing record interleaving without process-global mutexes.
      - Non-destructive reopening: reopening existing segment files appends at EOF without truncation or overwriting prior records.
      - Preserves caller key and value byte memory immutability.
    - Implemented comprehensive test suite in `internal/wal/writer_test.go` covering all 17 required test groups.
  * *Tests & Verification*:
    - `go test -count=1 -race ./internal/wal/...`: PASS (77 test suites, 0 data races)
    - `go test -count=1 -race ./...`: PASS across all repository packages
    - `golangci-lint run ./...`: 0 issues
    - `GOOS=linux go vet ./...`: 0 issues (cross-compilation verified)
    - `GOOS=windows go vet ./...`: 0 issues (cross-compilation verified)
    - Statement coverage: 94.2% in `internal/wal`, 100% in `internal/errors`
  * *Evidence Classification*:
    - **Design Target**: Synchronous WAL appending where `AppendSync` guarantees that bytes leave OS buffers via `fdatasync` before returning success to callers, with zero record interleaving under concurrent calls.
    - **Theoretical Property**: On Linux with pre-allocated segments, `syscall.Fdatasync` flushes data blocks without forcing directory/inode metadata writes; `os.O_APPEND` guarantees appending at EOF across process restarts; per-writer `sync.Mutex` guarantees serialized atomic record frames.
    - **Measured Result**: 1,000 sequential records verified byte-for-byte with `DecodeRecord`; 50 concurrent goroutines appended without record interleaving or data races; injected sync failures deterministically returned errors without false success; short writes handled without busy looping; reopening existing segment files preserved existing records intact; file permissions verified at `0600`.
    - **Observed Limitation**: On Darwin (macOS) and Windows, `fdatasync(2)` is not available in the kernel; synchronization falls back to `f.Sync()`, which also flushes file inode metadata. Physical media durability depends on underlying storage device controller write caches and filesystem barrier semantics. Mid-write failures may leave a partial record tail on disk; cleanup is deferred to the startup recovery subsystem.
  * *Completion*: Synchronous appender passing all unit, concurrency, fault injection, and strict sync tests.
* **P02-S02-M03: Sequential WAL Reader & Log Iterator**
  * *Objective*: Implement `WALReader` streaming records sequentially from disk from offset 0 to clean EOF.
  * *Changes*: `internal/wal/reader.go` implementing `WALReader`, `OpenReader()`, `OpenSegmentReader()`, `Path()`, `Offset()`, `Close()`, and `Next() (Record, error)`; `internal/errors/errors.go` adding `ErrReaderClosed`.
  * *Invariants*: Forward-only deterministic iteration without full-file buffering; strict read-only operation (opened with `os.O_RDONLY`, never modifies or truncates the file); symlink rejection via `os.Lstat` and inode pinning via `os.SameFile`; `Offset()` starts at 0, advances only on successful record consumption by exact physical wire length (`MinRecordSize + len(key) + len(val)`); clean EOF returns `io.EOF`; partial/torn records at EOF return truncation errors (`ErrHeaderTruncated`, `io.ErrUnexpectedEOF`); corrupted records return checksum/structural errors (`ErrChecksumMismatch`, `ErrInvalidRecordType`); never skips or repairs corrupted entries; single-consumer model (not safe for concurrent use); idempotent `Close()`.
  * *Evidence*:
    - **Design Target**: Forward-only, streaming sequential reader verifying checksums and framing boundaries without loading entire WAL files into memory.
    - **Theoretical Property**: If every successful `Next()` consumes exactly one valid record, cumulative reader offset equals the sum of consumed physical record sizes (`MinRecordSize + len(key) + len(val)`), and stops cleanly at `io.EOF` or freezes at the exact byte offset of the first invalid/truncated byte.
    - **Measured Result**: 22 required test groups passed; 1,000 sequential records read with cumulative offset exactly equal to file size; clean EOF distinguished from torn tails (1..20 byte truncated headers, partial keys, partial values); middle corruption intercepted deterministically without skipping; read-only verification proved file size and SHA256 checksum remain identical before and after reading; `go test -count=1 -race ./...` clean; 91.8% statement coverage in `internal/wal`; `golangci-lint` clean (0 issues).
    - **Observed Limitation**: Reader classifies and stops on errors; it does not repair corruption or truncate torn tails. Truncation and recovery decisions belong strictly to Sub-Phase 02.3 (`P02-S03-M01`). Reader is not safe for concurrent `Next()` calls.
  * *Completion*: Complete and verified across all unit, corruption, torn-tail, offset accounting, and lifecycle test suites.

### Sub-Phase 02.3: Torn Write Handling & Log Rotation
* **P02-S03-M01: Torn Tail Write Detection & Safe Truncation**
  * *Objective*: Implement the WAL recovery primitive that detects incomplete/torn records at the end of a segment and safely truncates the segment in-place to the last valid record boundary.
  * *Changes*: `internal/wal/recovery.go` implementing `RecoveryResult`, `RecoverSegment()`, `RecoverSegmentByID()`, and `recoverSegmentWithSeams()`; `internal/wal/export_test.go` exposing test seams; `internal/wal/recovery_test.go` implementing 33 comprehensive test suites.
  * *Invariants*: Quiescent segment assumption (target segment is not actively being written to); valid prefix preservation (all complete valid records are preserved); clean EOF requires no mutation (`Truncated: false`); complete corrupt records (checksum mismatch or invalid type) fail closed without truncation; middle corruption fails closed without skipping; truncation size exactness; single-descriptor in-place mutation and inode pinning via `os.SameFile`; post-condition verified before return (size check + rewind replay to `io.EOF`).
  * *Evidence*:
    - **Design Target**: Deterministic torn-tail recovery primitive that classifies EOF partial writes, truncates only the uncommitted tail, and refuses to silently repair or skip middle corruption.
    - **Theoretical Property**: If every record in prefix $P$ is complete and checksum-valid, and the record at $P$ is incomplete due to premature EOF (`ErrHeaderTruncated` or `io.ErrUnexpectedEOF`), truncating the physical file to $\text{len}(P)$ and flushing via `f.Sync()` leaves the file containing exactly $P$ terminating at clean `io.EOF`.
    - **Measured Result**: 33 test suites passing; 0-byte file preserved; 1..20 byte headers truncated back; partial key/value lengths/payloads truncated back; complete bad CRC at EOF rejected without truncation; complete invalid type at EOF rejected without truncation; middle corruption halted without truncation; valid prefix SHA256 immutability proved; 500-record prefix with torn tail cleanly truncated; randomized seeded truncation tests verified; 0 data races; `golangci-lint` clean (0 issues).
    - **Observed Limitation**: Recovery assumes the target segment is quiescent (not concurrently appended to by a `WALWriter`); multi-segment recovery is deferred to engine startup orchestration (`P02-S03-M03`).
  * *Completion*: Complete and verified across all unit, corruption, boundary matrix, and lifecycle test suites.
* **P02-S03-M02: WAL Segment Rotation & Sequencing**
  * *Objective*: Introduce deterministic WAL segment rotation and sequencing (`RotatingWriter` / `WAL`), enforcing strict monotonic segment IDs (`wal_%012d.log`), single active writer ownership, pre-write boundary evaluation, oversized record handling, atomic creation (`os.O_EXCL`), and multi-segment numeric ordering.
  * *Changes*: `internal/wal/rotation.go` (`RotatingWriter`, `WAL`, `Options`, `OpenRotatingWriter`, `Open`, `RecordWireSize`, `ParseSegmentID`, `ListSegments`), `internal/wal/writer.go` (`CreateWriter`, `CreateSegmentWriter`, `Size`).
  * *Evidence*:
    - **Design Target**: Exactly one active writable segment at a time for the logical WAL writer; rotation triggers automatically before appending any record that would exceed configured segment size (default 64MB); segment IDs increment monotonically ($N \to N+1$); older segments remain closed and independently readable without modification; existing foreign files are never overwritten or truncated on rotation collision.
    - **Theoretical Property**: If every record is encoded with exact wire length $R$, evaluating $S > 0 \land S + R > M$ before writing guarantees no record is fragmented across segment boundaries. If $S = 0$ (empty segment), accepting an oversized record ($R > M$) prevents deadlock while ensuring all subsequent appends trigger rotation immediately. Creating segments with `os.O_EXCL | os.O_CREATE` guarantees kernel-level atomic fail-fast if a target file already exists, eliminating TOCTOU collision hazards.
    - **Measured Result**: 34 test suites passing in `rotation_test.go`; exact boundary threshold ($S + R == M$), one byte under ($S + R == M - 1$), and one byte over ($S + R == M + 1$) verified; atomic collision rejection with foreign file non-truncation verified; oversized record in empty segment verified; multi-segment continuous byte sequence verified; 10 concurrent goroutines performing concurrent appends and rotations under `-race` passing with 0 data races; `golangci-lint` clean (0 issues).
    - **Observed Limitation**: Multi-segment startup recovery orchestration (discovering active segment, reconciling uncommitted tail writes across multiple historical segments, and replaying records into MemTable on database boot) is deferred to `P02-S03-M03`.
  * *Completion*: Complete and verified across all lifecycle, boundary matrix, concurrency, and fault-injection test suites.

* **P02-S03-M03: WAL Recovery Coordinator & Multi-Segment Replay**
  * *Objective*: Coordinate multi-segment crash recovery during database startup, discovering historical WAL segments in strictly numeric order, executing torn-tail recovery on the latest active segment, verifying older sealed segments, validating global sequence monotonicity, and replaying operations into a replay sink interface.
  * *Changes*: `internal/wal/coordinator.go` (`RecoverWAL(dbPath string, sink ReplaySink) (RecoveryReport, error)`), `internal/wal/coordinator_test.go`, `internal/errors/errors.go` (`ErrSegmentGap`, `ErrDuplicateSegment`, `ErrSequenceOutOfOrder`, `SegmentGapError`, `DuplicateSegmentError`, `SequenceOutOfOrderError`).
  * *Invariants*: Replay order strictly follows ascending segment ID ($1 \to 2 \to 3 \dots$); segment ID gaps or duplicates fail closed; sealed historical segments ($1..N-1$) must end at clean `io.EOF`; only latest segment ($N$) may contain a recoverable torn tail; complete CRC or middle corruptions fail closed without mutation; sequence numbers must be strictly monotonically increasing ($SeqNum_k > SeqNum_{k-1}$); replay sink failures stop replay immediately and accurately report pre-failure records.
  * *Evidence Classification*:
    - **Design Target**: Discover and validate all WAL segments under `<db_path>/wal/` in strictly ascending numeric order; verify historical segments in read-only mode; repair latest-segment torn tails via safe in-place truncation (`RecoverSegment`); enforce global sequence monotonicity; stream records into `ReplaySink` without full-file in-memory buffering; provide deterministic, transparent recovery report.
    - **Theoretical Property**: Decoupling physical repair from logical replay guarantees on-disk consistency before state-machine modification. If physical truncation occurs on the latest segment and subsequent replay fails (e.g. sink error), the on-disk log remains cleanly truncated, ensuring startup recovery retry is completely idempotent. Verifying historical segments prior to mutating the latest segment guarantees that corrupt historical logs never trigger erroneous latest-segment truncation.
    - **Measured Result**: 39 test suites passing in `coordinator_test.go`; empty WAL directory, clean single-segment, multi-segment ascending order, segment ID gap rejection, latest torn-tail truncation, earlier torn-tail fail-closed, complete CRC corruption fail-closed, middle corruption fail-closed, invalid record type rejection, sequence monotonicity and regression rejection, duplicate sequence rejection, valid DELETE and BATCH marker preservation, replay sink failure propagation, symlink and directory masquerade rejection, 10-segment streaming test with bounded memory, and adversarial multi-segment test suites passing under `-race` with 0 data races; statement coverage 88.9% in `internal/wal`; `golangci-lint` clean (0 issues).
    - **Observed Limitation**: Replay sink is defined as an engine integration interface (`ReplaySink`); concrete in-memory MemTable replay storage is deferred to Phase 03 (`P03-S01`).
  * *Completion*: Complete and verified across all lifecycle, boundary matrix, sequence monotonicity, fail-closed corruption, and adversarial test suites.

### Sub-Phase 02.4: Group Commit Coalescing Pipeline
* **P02-S04-M01: Group Commit Queue & Write Task Types**
  * *Objective*: Define the core task data structures (`WriteTask`) and queue boundary (`WriteQueue` / `GroupCommitQueue`) for the WAL Group Commit subsystem, establishing strict durability completion semantics, defensive payload immutability, and thread-safe bounded FIFO queueing.
  * *Changes*:
    - `internal/errors/errors.go`: Added `ErrQueueClosed`, `ErrQueueFull`, `ErrQueueEmpty`, `ErrTaskAlreadyCompleted`, `ErrTaskAlreadyEnqueued`, `ErrNilTask`, `ErrInvalidQueueCapacity` sentinels, and `InvalidQueueCapacityError` structured error.
    - `internal/wal/task.go`: Implemented `WriteTask` with four-tier payload ownership (defensive copy at `NewWriteTask`, task-owned internal storage, defensive copy at public `Record()`, package-internal unexported `rawRecord()` for M02 executor), execution error tracking, channel-based completion signaling (`done chan struct{}`), idempotent `Complete(err error)`, `Wait() error`, `WaitContext(ctx) error`, atomic `markEnqueued()` flag.
    - `internal/wal/queue.go`: Implemented `WriteQueue` (`GroupCommitQueue`) bounded ring buffer with `sync.Mutex` and condition variables (`notEmpty`, `notFull`), `DefaultQueueCapacity = 1024`, `Enqueue`, `TryEnqueue`, `Dequeue`, `TryDequeue`, graceful `Close()` draining, and immediate `CloseWithError(err)` fail-safe unblocking.
    - `internal/wal/queue_test.go`: 25 comprehensive unit, lifecycle, concurrency, regression, and adversarial test suites covering 33 test scenarios.
  * *Invariants*:
    - Strict durability invariant: task completion represents hardware synchronization barrier (`fdatasync`), never merely enqueue, dequeue, or page cache write.
    - Payload immutability: defensive copy of caller key/value slices eliminates post-enqueue mutation hazards; public `Record()` returns independent defensive copies to prevent post-inspection mutations.
    - Bounded FIFO ordering: tasks dequeued strictly in submission order; positive capacity bounds RAM accumulation.
    - Zero hung waiters: `CloseWithError(err)` guarantees every accepted task receives an explicit completion outcome on shutdown.
  * *Evidence Classification*:
    - **Design Target**: Bounded FIFO queue (default 1,024 capacity) providing condition-variable backpressure; defensive copying of `Record` key/value payloads at construction and public inspection; channel-based synchronization barrier; idempotent completion preventing duplicate wakeups; safe uninitialized/nil receiver guards; package-private zero-copy `rawRecord()` accessor for M02 batch runner.
    - **Theoretical Property**: Decoupling write submission from physical disk synchronization allows multiple caller goroutines to queue writes concurrently while establishing a single synchronization barrier in the downstream executor. Defensive copying at both the construction and public accessor boundaries provides mathematical isolation against slice aliasing without requiring external memory synchronization. Ring buffer slot clearing (`buffer[head] = nil`) guarantees garbage collection of dequeued tasks.
    - **Measured Result**: 25 test suites passing in `queue_test.go` covering 33 scenarios; concurrent multi-producer (10 goroutines, 1,000 tasks) stress test passing cleanly under `go test -count=1 -race ./...` with 0 data races; statement coverage 90.9% in `internal/wal`; `golangci-lint` clean (0 issues); cross-compilation vet clean on Linux and Windows.
    - **Observed Limitation**: Task and queue boundaries exist, but the active group-commit leader loop, cooperative batch formation, timer coalescing, and background executor worker are deferred to `P02-S04-M02`.
  * *Completion*: Complete and verified across all lifecycle, boundary matrix, concurrency, and adversarial test suites.
* **P02-S04-M02: Group Commit Batch Runner & Cooperative fsync**
  * *Objective*: Dedicated batch runner coalescing up to 1,024 writes or 64 KiB into a single `fdatasync()` barrier, with strict FIFO preservation, single-sync amortization, error fan-out, and graceful lifecycle drain.
  * *Changes*: `BatchWriter` interface; `WALWriter.Append` and `WALWriter.Sync`; `RotatingWriter.Append` and `RotatingWriter.Sync`; `WriteQueue.DequeueBatch` and `WriteQueue.TryDequeueBatch` with dual limits (`MaxBatchTasks = 1024`, `MaxBatchBytes = 64 * 1024`) and singleton oversized fallback; `GroupCommitRunner` background event loop with `Start()`, `Stop()`, `Wait()`, stats tracking, error fan-out, and drain; sentinels `ErrRunnerRunning` and `ErrRunnerClosed`.
  * *Invariants*: Exactly one `Sync()` barrier per successful batch; tasks never completed before `Sync()` returns; partial append failure skips `Sync()` and fails all tasks; sync failure fails all tasks; batches never exceed 1,024 tasks or 64 KiB (except singleton oversized records); zero-copy `rawRecord()` hot-path access; clean shutdown without goroutine leaks or stuck tasks.
  * *Evidence Classification*:
    - **Design Target**: Batch runner continuously consuming `WriteTask`s from `WriteQueue` in strict FIFO order, coalescing up to 1,024 tasks or 64 KiB wire representation per batch, executing exactly one physical `Sync()` barrier per successful batch, fanning out success or failure to all tasks in the batch, and coordinating segment rotation under `RotatingWriter`.
    - **Theoretical Property**: Amortizes non-volatile disk synchronization latency across concurrent caller goroutines, converting thread concurrency into a throughput multiplier ($N / T_{sync}$) while bounding tail latency to $\le 2 \times T_{sync}$. Strict post-sync completion channels prevent data loss on sudden power disruption. Singleton oversized fallback mathematically eliminates queue deadlock for valid records $>64\text{ KiB}$. Safe subtraction bounds checks prevent 64-bit integer overflow. Package-private zero-copy `rawRecord()` eliminates heap churn on hot append paths.
    - **Measured Result**: 26 test scenarios in `runner_test.go` covering all execution paths and failure modes; 287/287 wal tests passing; 90.3% statement coverage in `internal/wal`; concurrent multi-producer stress test (50 goroutines, 500 tasks) passing cleanly under `go test -count=1 -race ./...` with 0 data races; single-sync barrier amortization deterministically verified via mock writer seam; end-to-end multi-segment rotation and crash recovery replay verified; `golangci-lint` clean (0 issues); cross-compilation vet clean on Linux and Windows.
    - **Observed Limitation**: Linger timeout delay (`linger_ms`) and dynamic adaptive batch sizing deferred to single-node engine integration (documented in `docs/known-limitations.md` #13).
  * *Completion*: Complete and verified across all lifecycle, boundary matrix, concurrency, and adversarial test suites. Phase 02 is now fully complete.

---

# Phase 03: In-Memory MemTable & Concurrent SkipList

* **Major Objective**: Implement the probabilistic concurrent SkipList supporting lock-free reads and exact memory accounting.
* **Dependencies**: Phase 01.
* **Risks**: Data races during concurrent forward pointer traversal, memory leaks.

### Sub-Phase 03.1: SkipList Node & Level Generation
* **P03-S01-M01: SkipList Node Memory Representation & Geometric Randomizer**
  * *Objective*: Define node struct with forward pointer slices and geometric height randomizer ($p=0.25, L_{max}=16$).
  * *Changes*: `internal/memtable/node.go` (`skipListNode`, `newSkipListNode`, `newSentinelNode`), `internal/memtable/random.go` (`RandomSource`, `PCG32`, `HeightGenerator`, `RandomHeight() int`, `randomHeight() int`), `internal/memtable/export_test.go`, `internal/errors/errors.go` (`ErrInvalidSkipListHeight`, `ErrInvalidSkipListLevel`, `InvalidSkipListHeightError`, `InvalidSkipListLevelError`).
  * *Invariants Enforced*:
    - `P03-S01-INV-01`: Every valid SkipList node has $1 \le \text{height} \le \text{MaxHeight}$ ($L_{max} = 16$).
    - `P03-S01-INV-02`: Forward-pointer storage length exactly matches the node's declared height (`len = cap = height`).
    - `P03-S01-INV-03`: Node construction cannot allocate attacker-controlled unbounded tower storage.
    - `P03-S01-INV-04`: Random height depends strictly on the random source and configured distribution, not on key/value contents.
    - `P03-S01-INV-05`: `randomHeight()` always terminates deterministically in at most 15 iterations.
    - `P03-S01-INV-06`: No valid node can expose a forward-pointer level outside its configured height.
  * *Theoretical Property*: Geometric distribution $P(H \ge n) = p^{n-1}$ with $p = 0.25$ provides an average of $\frac{1}{1-p} \approx 1.33$ pointers per node (saving $33.3\%$ pointer memory compared to $p = 0.5$) while guaranteeing $O(\log_{1/p} N) = O(\log N)$ expected search and insertion complexity across up to $4^{15} \approx 10^9$ keys.
  * *Measured Result*: 100,000 generated heights evaluated via Pearson's Chi-Square Goodness-of-Fit test yielding $\chi^2 = 5.1448$ ($df = 7$, well below critical threshold $\chi^2_{0.001} = 24.322$); dominant buckets verified within $4\sigma$ binomial confidence envelopes; 2,625,192 fuzz iterations executed with 0 panics; 98.5% statement coverage in `internal/memtable`; 0 data races under 64 goroutines in `go test -race`; `golangci-lint` clean (0 issues); cross-compilation vet clean on Linux and Windows.
  * *Observed Limitation*: Forward pointer mutations are currently unexported and package-private; concurrent atomic publication (`atomic.LoadPointer` / `atomic.StorePointer`) and multi-level traversal are scheduled for Sub-Phase 03.2 (`P03-S02-M01`).
  * *Completion*: Complete and verified across all boundary matrices, invariants, statistical distributions, and fuzz tests.
* **P03-S01-M02: Single-Threaded SkipList Insertion & Lookup**
  * *Objective*: Implement sequential `Insert(key binary.InternalKey, value []byte)` and point lookup `Search(userKey []byte) ([]byte, error)` over multi-version `InternalKey` entries anchored by a `MaxHeight` sentinel node.
  * *Invariants*:
    - P03-S01-M02-INV-01: For every level, keys encountered through forward pointers are strictly ordered according to `binary.CompareInternalKey` (UserKey ASC, SeqNum DESC, OpType DESC).
    - P03-S01-M02-INV-02: Every node reachable at level L has a tower height >= L+1.
    - P03-S01-M02-INV-03: No forward-pointer traversal contains a cycle.
    - P03-S01-M02-INV-04: Every node reachable from the head at level L is reachable through a valid lower-level path.
    - P03-S01-M02-INV-05: A node appears at all levels from 0 through height-1 and never appears at a higher level.
    - P03-S01-M02-INV-06: Insertion preserves all existing entries unless duplicate handling is explicitly defined (exact duplicates idempotently update value).
    - P03-S01-M02-INV-07: Search returns the newest matching version according to descending sequence order.
    - P03-S01-M02-INV-08: Searching for a nonexistent UserKey returns ErrKeyNotFound without mutating the structure.
    - P03-S01-M02-INV-09: Forward traversal terminates at nil across all levels.
    - P03-S01-M02-INV-10: The SkipList remains structurally valid after arbitrary sequences of sequential insertions.
  * *Theoretical Property*: Expected search and insertion complexity is $O(\log N)$ with expected traversal cost $\frac{1}{p} \log_{1/p} N = 4 \log_4 N$ node inspections. Zero heap allocations on the `Search` point lookup path.
  * *Measured Result*: 10,000 randomized insertions (with duplicate user keys exercising multi-versioning) executed and verified against an in-memory oracle model in 0.01s; 1,000 nonexistent key point lookups confirmed returning `ErrKeyNotFound`; 270,783 fuzz iterations completed in 3.0s with 0 panics and 0 invariant violations; 91.0% statement coverage in `internal/memtable`; 0 data races detected in `go test -race ./...`; `golangci-lint run ./...` clean (0 issues); `go vet` clean across Darwin, Linux, and Windows.
  * *Observed Limitation*: Forward pointer mutations are strictly sequential in this micro-phase; concurrent atomic publication (`atomic.StorePointer`), lock-free reader traversal (`atomic.LoadPointer`), and memory accounting are scheduled for Sub-Phase 03.2.
  * *Completion*: Complete and verified.

### Sub-Phase 03.2: Concurrent Traversal & Memory Accounting
* **P03-S02-M01: Lock-Free Read Traversal via Atomic Pointer Reads**
  * *Objective*: Implement lock-free concurrent read traversal (`SearchConcurrent(userKey []byte) ([]byte, error)`) across forward pointer express lanes via `atomic.Pointer[skipListNode]`, accompanied by exclusive writer synchronization (`sync.RWMutex`), atomic active-height observation (`atomic.Int32`), and bottom-up publication.
  * *Invariants*:
    - P03-S02-M01-INV-01: Every reader-visible forward-pointer load is atomic (`atomic.Pointer.Load`).
    - P03-S02-M01-INV-02: A published node is fully initialized before becoming reader-visible.
    - P03-S02-M01-INV-03: Published immutable node fields are never mutated non-atomically after publication; duplicate updates atomically swap value container.
    - P03-S02-M01-INV-04: Concurrent readers never require the writer mutation lock for forward traversal.
    - P03-S02-M01-INV-05: Concurrent active-height observation is race-free.
    - P03-S02-M01-INV-06: Concurrent reader traversal cannot observe a partially initialized node.
    - P03-S02-M01-INV-07: Concurrent insertion preserves canonical ordering across all levels.
    - P03-S02-M01-INV-08: Concurrent searches return only valid logical states (either valid old or valid new).
    - P03-S02-M01-INV-09: Concurrent traversal never creates or follows cycles.
    - P03-S02-M01-INV-10: After writer completion, `Search` and `SearchConcurrent` are semantically equivalent.
  * *Theoretical Property*: Reader point lookup requires expected $O(\log N)$ atomic pointer loads and zero locks on the writer mutation mutex. Traversal state allocates zero heap memory.
  * *Measured Result*: 16 reader goroutines + 1 writer goroutine concurrently executing 3,000 multi-version mutations and continuous point lookups under `go test -race` with 0 data races, verified against an independent concurrent test oracle; active-height growth ($1 \to 2 \to 8 \to 16$) verified race-free; 352,967 concurrent fuzz iterations completed in 3.0s (117,652 execs/sec) with 0 panics; 91.5% statement coverage in `internal/memtable`; `golangci-lint run ./...` clean (0 issues); `go vet` clean across Darwin, Linux, and Windows.
  * *Observed Limitation*: Write mutations remain serialized under `s.mu.Lock()`; concurrent multi-writer lock-free insertions are intentionally not implemented. Exact byte-level memory tracking (`MemTable.ByteSize()`) is deferred to `P03-S02-M02`.
  * *Completion*: Complete and verified.
* **P03-S02-M02: Exact Byte-Level Memory Accounting**
  * *Objective*: Track exact heap consumption of keys, values, node structs, and forward pointer arrays.
  * *Changes*: `internal/memtable/size.go` (`NodeStructSize`, `NodeValueStructSize`, `PointerSize`, `nodeMemoryBytes`, `valueMemoryBytes`, `safeAddUint64`, `safeSubUint64`), `internal/memtable/skiplist.go` (`SkipList.ByteSize() uint64`, `byteSize atomic.Uint64`, atomic accounting on fresh insert and duplicate replacement), `internal/memtable/export_test.go`, `internal/memtable/size_test.go`.
  * *Invariants*:
    - P03-S02-M02-SEC-INV-01: `ByteSize` never wraps due to unchecked addition (saturates safely at `math.MaxUint64`).
    - P03-S02-M02-SEC-INV-02: `ByteSize` never underflows due to subtraction (saturates safely at 0).
    - P03-S02-M02-SEC-INV-03: Failed insertion does not change `ByteSize` (failure atomicity).
    - P03-S02-M02-SEC-INV-04: Exact duplicate insertion does not change node count (`Len()`).
    - P03-S02-M02-SEC-INV-05: Exact duplicate replacement changes `ByteSize` strictly by the defined ownership delta ($\Delta = \text{newValBytes} - \text{oldValBytes}$).
    - P03-S02-M02-SEC-INV-06: `ByteSize()` is race-free under concurrent observation.
    - P03-S02-M02-SEC-INV-07: Every live owned allocation represented by the accounting model is counted exactly once.
    - P03-S02-M02-SEC-INV-08: No temporary test-only memory is included in production `ByteSize`.
  * *Theoretical Property*: `ByteSize()` is $O(1)$ time, zero heap allocations, safe for concurrent reader observation via `atomic.Uint64`. Accounting derives deterministically from struct layout (`unsafe.Sizeof`) and owned slice capacities, completely decoupled from non-deterministic `runtime.MemStats`.
  * *Measured Result*: Exact equality verified against an independent test oracle across deterministic cases (Heights 1..16, Keys 1..65,535B, Values 0..65,536B); incremental additive deltas ($A, B, C$) confirmed; duplicate delta updates (grow, shrink, shrink to 0, grow from 0) verified; 1 writer + 16 readers executing continuous mutations and `ByteSize()` observation under `go test -race` with 0 data races; 92.7% statement coverage in `internal/memtable`; `golangci-lint run ./...` clean (0 issues); `go vet` clean across Darwin, Linux, and Windows.
  * *Observed Limitation*: `ByteSize()` tracks Lattice-owned in-memory object heap allocations under the project's explicit accounting model; it intentionally excludes Go runtime allocator metadata, GC write barrier structures, allocator size-class slack, and OS-level process RSS.
  * *Completion*: Complete and verified.

### Sub-Phase 03.3: MemTable Iteration & Immutable Transition
* **P03-S03-M01: Forward Iterator Implementation**
  * *Objective*: Implement lock-free forward iterator (`Iterator`) over the SkipList's Level-0 canonical physical ordering supporting `Seek`, `SeekToFirst`, `SeekInternalKey`, `Next`, `Valid`, `Key`, `Value`, and `Close`.
  * *Invariants*:
    - `P03-S03-M01-INV-01`: For a static SkipList, repeated `Next()` traverses exactly the Level-0 sequence once from head to tail.
    - `P03-S03-M01-INV-02`: Keys returned by successive valid iterator positions are strictly non-decreasing according to canonical `CompareInternalKey`.
    - `P03-S03-M01-INV-03`: Iterator traversal terminates at `nil` and is guaranteed to be acyclic.
    - `P03-S03-M01-INV-04`: `Seek(target)` returns the first position satisfying `UserKey >= target`; for multi-version keys, it strictly lands on the newest revision.
    - `P03-S03-M01-INV-05`: `Key()` and `Value()` return defensive copies, strictly preventing caller mutation from corrupting engine memory.
    - `P03-S03-M01-INV-06`: Concurrent iteration and insertion is 100% race-free under the supported weakly-consistent live iterator model.
  * *Theoretical Property*: Traversal requires zero mutex locks and zero allocations per `Next()`. `Seek(userKey)` leverages express lanes to achieve expected $O(\log N)$ descent from active height down to Level 0. Physical sequence exposes tombstones (`OpTypeDelete` with `Value() == nil`) and historical versions to support LSM SSTable flushing and compaction.
  * *Measured Result*:
    - `BenchmarkIterator_SequentialScan_1000Keys`: 2,393 ns/op (~2.39 ns/node), 0 B/op, 0 allocs/op.
    - `BenchmarkIterator_SequentialScan_10000Keys`: 35,967 ns/op (~3.60 ns/node), 0 B/op, 0 allocs/op.
    - `BenchmarkIterator_Seek_Random_1000Keys`: 58.84 ns/op, 0 B/op, 0 allocs/op.
    - `BenchmarkIterator_Seek_Random_10000Keys`: 129.3 ns/op, 0 B/op, 0 allocs/op.
    - `BenchmarkIterator_Next_PerStep`: 3.002 ns/op, 0 B/op, 0 allocs/op.
    - Fuzz testing (`FuzzIterator`): >1,360,000 iterations executed with 0 failures.
    - Race detector (`go test -race ./internal/memtable`): PASSED (0 data races).
  * *Observed Limitation*: The iterator is a live, weakly-consistent iterator over a mutable SkipList, not a point-in-time snapshot iterator. Insertions ahead of the iterator's cursor are observed; insertions behind are not. Snapshot isolation will be introduced via `MemTable.Freeze()` (`P03-S03-M02`) and MVCC sequence number filtering.
  * *Completion*: Complete and verified.
* **P03-S03-M02: Atomic MemTable Freeze & Immutable Transition**
  * *Objective*: Permanently transition the active SkipList / MemTable from `ACTIVE` to read-only `FROZEN` via `Freeze() bool` and `IsFrozen() bool`, rejecting all subsequent mutations with `ErrMemTableFrozen`.
  * *Invariants*:
    - `P03-S03-M02-INV-01`: Lifecycle state is monotonic `ACTIVE -> FROZEN`; once frozen, reopening is prohibited.
    - `P03-S03-M02-INV-02`: Once `Freeze` linearizes, no subsequent `Insert` can mutate structural links, heights, or values.
    - `P03-S03-M02-INV-03`: Every `Insert` that linearizes before `Freeze` is guaranteed to be represented in the frozen structure.
    - `P03-S03-M02-INV-04`: Every post-freeze rejected `Insert` leaves structure, `Len`, `Height`, and `ByteSize` completely unchanged.
    - `P03-S03-M02-INV-05`: Concurrent `Freeze` calls have one stable terminal outcome (idempotent, thread-safe).
    - `P03-S03-M02-INV-06`: `SearchConcurrent` remains lock-free and race-free across the `Freeze` transition.
    - `P03-S03-M02-INV-07`: Existing iterators can safely traverse the structure after `Freeze`.
    - `P03-S03-M02-INV-08`: After `Freeze`, all structural links and value states are permanently immutable (exact duplicate updates cannot swap value containers).
  * *Theoretical Property*: `Freeze()` is an $O(1)$ in-place state transition taking 0 heap allocations, synchronizing with serialized writers on `s.mu` to eliminate TOCTOU race windows. Lock-free readers and iterators continue without interruption or cache invalidation.
  * *Measured Result*:
    - `BenchmarkSkipList_Freeze_1K`: **12.16 ns/op**, 0 B/op, 0 allocs/op.
    - `BenchmarkSkipList_Freeze_10K`: **12.00 ns/op**, 0 B/op, 0 allocs/op.
    - `BenchmarkSkipList_Freeze_100K`: **12.02 ns/op**, 0 B/op, 0 allocs/op.
    - `BenchmarkSkipList_Freeze_IdempotentAlreadyFrozen`: **6.52 ns/op**, 0 B/op, 0 allocs/op.
    - Fuzz testing (`FuzzSkipList_FreezeLifecycle`): >1,630,000 iterations executed with 0 failures.
    - Race detector (`go test -race ./internal/memtable`): PASSED (0 data races).
  * *Observed Limitation*: `Freeze()` provides structural immutability of the physical MemTable for background SSTable flusher workers; logical MVCC point-in-time snapshot isolation (sequence-number filtering and tombstone masking) is applied at higher engine tiers (Phase 10).
  * *Completion*: Complete and verified.

---

# Phase 04: Persistent SSTable Subsystem

* **Major Objective**: Construct immutable binary SSTables with prefix-compressed data blocks, sparse indexes, and 48-byte footers.
* **Dependencies**: Phase 01, Phase 03.
* **Risks**: Corrupted block alignments, binary search seek failures.

### Sub-Phase 04.1: Data Block Construction & Prefix Compression
* **P04-S01-M01: Data Block Builder with Prefix Compression**
  * *Objective*: Implement `BlockBuilder` compressing consecutive sorted keys via shared prefix lengths with restart points every 16 records.
  * *Changes*:
    - Created `internal/sstable/block_builder.go`: implemented `BlockBuilder`, `NewBlockBuilder()`, `NewBlockBuilderWithInterval()`, `Add()`, `AddRaw()`, `Finish()`, `Reset()`, `RestartOffsets()`, `RestartCount()`, `EntryCount()`, `DataSize()`, `CurrentSizeEstimate()`, `IsEmpty()`, `Finished()`.
    - Added domain errors in `internal/errors/errors.go`: `ErrKeyOutOfOrder`, `ErrBlockFinished`, `ErrInvalidRestartInterval`, `ErrBlockOverflow`, and typed `KeyOutOfOrderError`.
    - Implemented independent test-side block parser oracle in `internal/sstable/block_builder_test.go`.
    - Added comprehensive fuzz targets in `internal/sstable/block_builder_fuzz_test.go`.
    - Added performance benchmarks in `internal/sstable/block_builder_bench_test.go`.
  * *Invariants Maintained*:
    - *Restart Group Invariant*: Restart points emitted every 16 entries (indexes 0, 16, 32, ...); every restart entry strictly enforces `SharedKeyLen = 0`.
    - *Canonical Ordering Invariant*: Keys must be supplied in strictly increasing canonical order (`UserKey` ASC, `SeqNum` DESC, `OpType` DESC); ordering inversions or exact duplicates are rejected with `ErrKeyOutOfOrder`.
    - *Failure Atomicity*: If `Add()` fails, builder state remains 100% unmodified with zero buffer mutation or metadata leakage.
    - *Caller Isolation*: Input keys and values are defensively copied; `Finish()` returns an owned slice isolated from external mutation.
  * *Measured Results*:
    - Micro-benchmarks (Apple M4):
      - `BenchmarkBlockBuilder_Add_1K`: ~38.1 µs total (38.1 ns/op per record).
      - `BenchmarkBlockBuilder_Add_10K`: ~441.9 µs total (44.2 ns/op per record).
      - `BenchmarkBlockBuilder_Finish`: 140.4 ns/op (1 allocation for defensive copy).
      - `BenchmarkBlockBuilder_SharedPrefix`: ~41.6 µs per 1K adds.
      - `BenchmarkBlockBuilder_NoSharedPrefix`: ~32.9 µs per 1K adds.
    - Empirical Compression Ratio: 1.81x (44.8% storage reduction) across 100 benchmark keys sharing 50-byte prefixes.
    - Fuzz Testing: >690,000 iterations of `FuzzBlockBuilder_ValidSequence` and >2,500,000 iterations of `FuzzBlockBuilder_AdversarialOrdering` with zero failures or crashes.
  * *Completion*: Complete and verified under `-race`.
* **P04-S01-M02: Restart Array & Block Trailer Serialization**
  * *Objective*: Append 32-bit restart point offsets and restart count to block tail; add CRC32-IEEE trailer.
  * *Changes*:
    - `internal/sstable/block_builder.go`:
      - Added constants `RestartOffsetSize = 4`, `RestartCountSize = 4`, `BlockTrailerSize = 4`.
      - Updated `BlockBuilder` with `finishedBuf []byte` for idempotent repeated `Finish()` calls.
      - Updated `Finish() []byte` to serialize:
        `[Entry Data] + [Restart Offsets (Big-Endian uint32 * N)] + [Restart Count (Big-Endian uint32)] + [CRC32-IEEE (Big-Endian uint32)]`.
      - Empty builder returns `[]byte{}` with zero allocations and enters sealed finished state.
      - CRC32-IEEE covers `[Entry Data || Restart Offsets || Restart Count]`, strictly excluding the CRC trailer field itself.
      - Updated `CurrentSizeEstimate()` to reflect entry data + restart array + restart count + CRC32 trailer.
      - Updated `DataSize()` to report entry data size prior to or excluding trailer.
      - Added capacity overflow guard checking entry data + projected trailer bytes against `math.MaxUint32`.
      - Updated `Reset()` to clear finished buffer and restore reusable state.
    - `internal/sstable/block_builder_test.go`:
      - Created independent reference oracle `parseFullBlock` and bit-by-bit software CRC32-IEEE oracle `referenceCRC32IEEE`.
      - Implemented hand-calculated exact byte fixtures:
        - Fixture A: 1 entry, 1 restart point.
        - Fixture B: 3 entries with prefix compression (72 bytes exact sequence).
        - Fixture C: 16 entries (boundary of first restart group, 288 bytes).
        - Fixture D: 17 entries (second restart point emitted at offset 276, 312 bytes).
        - Fixture E: Custom restart interval $k=4$ with 6 records (124 bytes).
        - Fixture F: Binary values with null bytes, 0xFF, and high-bit sequences.
        - Fixture G: InternalKey with tombstone / `OpTypeDelete`.
      - Implemented CRC32 test vector verification (`123456789 -> 0xCBF43926`).
      - Implemented single-bit corruption detection across entry data, restart offsets, restart count, and CRC trailer.
      - Implemented repeated `Finish()` idempotence and memory mutation isolation tests.
    - `internal/sstable/block_builder_bench_test.go`:
      - Added benchmarks: `BenchmarkBlockBuilder_Finish_Small`, `BenchmarkBlockBuilder_Finish_4KB`, `BenchmarkBlockBuilder_Finish_ManyRestarts`, `BenchmarkBlockBuilder_Reset_Rebuild`.
  * *Invariants*:
    - Block layout: `[Entry Data || Restart Offsets (uint32 * M) || Restart Count (uint32) || CRC32-IEEE (uint32)]`.
    - Big-Endian byte order for all fixed numeric metadata fields.
    - `restartOffsets[0] == 0` for non-empty blocks; strictly increasing monotonically; all offsets `< len(entryData)`.
    - `restartCount == len(restartOffsets)`.
    - CRC32-IEEE covers `[Entry Data || Restart Offsets || Restart Count]`.
    - Determinism: byte-for-byte identical output for identical records.
    - Failure atomicity: builder state unmodified on invalid inputs.
    - Caller isolation: defensive copy returned, mutations do not affect internal state.
  * *Tests & Benchmarks*:
    - Micro-benchmarks (Apple M4):
      - `BenchmarkBlockBuilder_Finish_Small`: ~542.9 ns/op (4.5 KB/op, 15 allocs/op).
      - `BenchmarkBlockBuilder_Finish_4KB`: ~3,772 ns/op (12.2 KB/op, 85 allocs/op).
      - `BenchmarkBlockBuilder_Finish_ManyRestarts`: ~3,451 ns/op (9.0 KB/op, 108 allocs/op).
      - `BenchmarkBlockBuilder_Reset_Rebuild`: ~651.7 ns/op (704 B/op, 21 allocs/op).
    - Fuzz Testing:
      - `FuzzBlockBuilder_ValidSequence`: >1,134,000 iterations in 10s with 0 failures.
      - `FuzzBlockBuilder_AdversarialOrdering`: >5,104,000 iterations in 10s with 0 failures.
    - Full suite passing under `go test -race ./...`.
  * *Security Review*:
    - Integer safety: 32-bit overflow checked before buffer append.
    - Checksum coverage: verified bit-by-bit; CRC field strictly excluded from its own digest.
    - Memory isolation: defensive copy verified by post-Finish buffer mutation test.
  * *Completion*: Complete and verified under `-race`.

### Sub-Phase 04.2: SSTable Index & Footer Design
* **P04-S02-M01: Sparse Two-Level Block Index Builder**
  * *Objective*: Record largest key and file offset/size handle for each emitted data block.
  * *Changes*:
    - Created `internal/sstable/block_handle.go`:
      - Defined `BlockHandle` struct (`Offset uint64`, `Size uint64`) with fixed 16-byte Big-Endian encoding (`BlockHandleSize = 16`).
      - Implemented `Encode() [16]byte`, `AppendTo(dst []byte) []byte`, `DecodeBlockHandle(src []byte) (BlockHandle, error)`, `Validate() error`, `ValidateAgainstFileSize(fileSize int64) error`.
    - Created `internal/sstable/index_builder.go`:
      - Defined `IndexEntry` (`LargestKey []byte`, `Handle BlockHandle`) with `Clone()`.
      - Implemented `IndexBuilder`: `NewIndexBuilder()`, `AddBlock(largestKey []byte, handle BlockHandle) error`, `AddBlockKey(key binary.InternalKey, handle BlockHandle) error`, `Finish() []byte`, `Reset()`, `EntryCount() int`, `IsEmpty() bool`, `Finished() bool`, `Entries() []IndexEntry`, `FindBlock(targetKey []byte) (BlockHandle, bool)`, `CurrentSizeEstimate() int`.
      - Defined `BlockIndex` and implemented independent binary reader `DecodeBlockIndex(data []byte) (*BlockIndex, error)`.
    - Added domain errors in `internal/errors/errors.go`: `ErrBlockHandleTruncated`, `ErrInvalidBlockHandle`, `ErrIndexFinished`, `ErrIndexBlockTruncated`, `ErrIndexBlockCorrupted`, and typed `InvalidBlockHandleError`, `IndexBlockCorruptedError`.
    - Created `internal/sstable/block_handle_test.go`: unit tests for round-trip encoding, byte layout, truncation, zero size, overflow, and file boundary validation.
    - Created `internal/sstable/index_builder_test.go`:
      - Verified sparse index construction across 50 data blocks generated with `BlockBuilder`.
      - Verified that all 50 handles accurately record the physical offsets and sizes of the emitted blocks.
      - Tested sparse index binary search (`FindBlock`), failure atomicity, caller isolation, repeated `Finish()` idempotence, and single-bit corruption detection.
    - Created `internal/sstable/index_builder_fuzz_test.go`:
      - Added `FuzzBlockHandle_Decode` (>6,073,000 iterations in 10s with 0 failures).
      - Added `FuzzBlockIndex_Decode` (>5,796,000 iterations in 10s with 0 failures).
    - Created `internal/sstable/index_builder_bench_test.go`: micro-benchmarks for adding 50 and 1,000 blocks, index serialization, and binary search.
  * *Invariants Maintained*:
    - *One Index Entry Per Block*: Exactly one index entry per data block, storing its largest key and physical `BlockHandle`.
    - *Canonical Ordering*: Keys added to the index builder must be strictly increasing; inversions or duplicates rejected with `KeyOutOfOrderError`.
    - *Handle Integrity*: Non-zero size (`Size > 0`) and valid offset bounds (`Offset + Size` does not overflow 64-bit address space).
    - *Failure Atomicity*: Failed additions leave builder state completely unmodified.
    - *Caller Isolation*: Keys and handles defensively copied on input; `Entries()` and `Finish()` return defensively copied data.
    - *CRC32 Integrity*: Serialized index block trailer includes 4-byte CRC32-IEEE covering all entry data, offsets, and entry count.
  * *Measured Results*:
    - Micro-benchmarks (Apple M4):
      - `BenchmarkIndexBuilder_AddBlock_50`: ~2,156 ns/op (43.1 ns per block handle added).
      - `BenchmarkIndexBuilder_AddBlock_1K`: ~49,089 ns/op (49.1 ns per block handle added across 1,000 blocks).
      - `BenchmarkIndexBuilder_Finish`: ~173.5 ns/op (2,304 B/op, 1 alloc/op for defensive copy).
      - `BenchmarkBlockIndex_FindBlock_BinarySearch`: ~256.5 ns/op for binary search over 1,000 blocks in RAM.
    - Fuzz Testing:
      - `FuzzBlockHandle_Decode`: >6,073,000 iterations in 10s with 0 failures.
      - `FuzzBlockIndex_Decode`: >5,796,000 iterations in 10s with 0 failures.
  * *Completion*: Complete and verified under `-race`.
* **P04-S02-M02: Fixed 48-Byte Footer Serializer & Parser**
  * *Objective*: Implement encoding and decoding of the fixed 48-byte SSTable file footer (`MetaIndexHandle [16B] + IndexHandle [16B] + Padding [8B] + Magic [8B]`).
  * *Binary Layout*:
    - `Offset 00..15`: `MetaIndexHandle` (8B Offset Big-Endian uint64, 8B Size Big-Endian uint64).
    - `Offset 16..31`: `IndexHandle` (8B Offset Big-Endian uint64, 8B Size Big-Endian uint64).
    - `Offset 32..39`: `Padding` (strictly 8 zero bytes `0x00` for canonical framing).
    - `Offset 40..47`: `Magic` (8B Big-Endian uint64 `0x4C41545453535401`, ASCII `"LATT_SST_1"`).
  * *Changes*:
    - `internal/sstable/footer.go`: `Footer` struct, `FooterSize = 48`, `FooterMagic = 0x4C41545453535401`, `Footer.Encode() [48]byte`, `Footer.AppendTo(dst []byte) []byte`, `Footer.Decode(src []byte) error`, `DecodeFooter(src []byte) (Footer, error)`, `Footer.Validate() error`, `Footer.ValidateAgainstFileSize(fileSize int64) error`.
    - `internal/errors/errors.go`: Added sentinels `ErrInvalidFooter`, `ErrInvalidFooterMagic`, `ErrFooterTruncated`, `ErrInvalidFooterSize`, `ErrInvalidFooterPadding`, and structured types `InvalidFooterMagicError`, `InvalidFooterPaddingError`, `InvalidFooterSizeError`.
  * *Invariants*:
    - Serialized footer length is strictly 48 bytes.
    - Decodes strictly when `len(src) == 48`. If `< 48`, returns `ErrFooterTruncated` wrapping `InvalidFooterSizeError`. If `> 48`, returns `ErrInvalidFooterSize` wrapping `InvalidFooterSizeError`.
    - Magic number at bytes `[40:48]` must equal `0x4C41545453535401`. Any bit flip or endianness error is rejected with `ErrInvalidFooterMagic`.
    - Reserved padding at bytes `[32:40]` must be strictly all zero (`0x00`). Any non-zero byte is rejected with `ErrInvalidFooterPadding`.
    - Handles must pass `BlockHandle.Validate()` (`Size > 0` and no uint64 overflow).
    - Physical file bounds validation via `ValidateAgainstFileSize(fileSize int64)` enforces `Offset + Size <= uint64(fileSize) - FooterSize` for both handles, ensuring handles cannot point into or overlap the footer anchored at `fileSize - 48`.
    - Zero heap allocation during encoding (`Encode()` returns stack-allocated `[48]byte`).
  * *Tests*:
    - Independent binary oracle test comparing against hand-crafted Big-Endian byte fixture.
    - Full round-trip tests with small/large offsets, sizes, and boundary values.
    - Comprehensive magic validation: 1-bit flips across all 64 bits, all-zeros, all-ones (0xFF), byte-reversed.
    - Comprehensive padding validation: 1-bit non-zero flags across all 8 padding bytes.
    - Truncation tests: lengths 0 through 47 bytes all safely rejected without panic.
    - Trailing bytes tests: lengths 49 through 96 bytes rejected.
    - Zero-value footer rejection (zero-size handles fail validation).
    - File boundary validation with handles exceeding `fileSize - 48`.
  * *Benchmarks* (Apple M4, darwin/arm64):
    - `BenchmarkFooter_Encode`: 3.315 ns/op (0 B/op, 0 allocs/op).
    - `BenchmarkFooter_AppendTo`: 4.936 ns/op (0 B/op, 0 allocs/op).
    - `BenchmarkFooter_Decode`: 3.765 ns/op (0 B/op, 0 allocs/op).
    - `BenchmarkFooter_RoundTrip`: 7.816 ns/op (0 B/op, 0 allocs/op).
  * *Fuzz Testing*:
    - `FuzzDecodeFooter`: 5,999,893 executions in 10s with 0 failures, 0 crashes, 0 hangs.
  * *Completion*: Complete and verified under `-race`, `golangci-lint`, Linux and Windows `go vet`.

### Sub-Phase 04.3: SSTable File Writer & Reader
* **P04-S03-M01: SSTable Sequential File Writer (`TableWriter`)**
  * *Objective*: Assemble data blocks, meta-index block, index block, and 48-byte footer into an immutable `.sst` file with atomic staging and durability barriers.
  * *Physical Layout*:
    - `Data Blocks 0..N-1`: 4KB default prefix-compressed records with restart offsets and CRC32 trailer.
    - `Meta Index Block`: 8 bytes in Phase 04 (`uint32(0)` entry count + `uint32` CRC32-IEEE checksum).
    - `Index Block`: Sparse Two-Level Block Index mapping largest keys to 16B `BlockHandle`s with tail offsets and CRC32 trailer.
    - `Footer`: Fixed 48-byte trailer (`MetaIndexHandle [16B] + IndexHandle [16B] + Padding [8B] + Magic [8B]`).
  * *Changes*:
    - `internal/sstable/table_writer.go`: `TableWriter`, `TableWriterOptions`, `DefaultTableWriterOptions()`, `SSTableMetadata`, `Iterator` interface, `NewTableWriter()`, `NewTableWriterWithFile()`, `Add()`, `AddRaw()`, `Build()`, `Finish()`, `Close()`, `BytesWritten()`, `EntryCount()`, `BlockCount()`, `EstimatedSize()`.
    - `internal/sstable/export_test.go`: Test seams for deterministic fault injection (`SetWriteFnForTesting`, `SetSyncFnForTesting`, `SetCloseFnForTesting`).
    - `internal/errors/errors.go`: Added `ErrTableWriterClosed`, `ErrTableWriterFinalized`, `ErrSSTableExists`.
  * *Invariants*:
    - Keys added in strictly increasing canonical order (`UserKey ASC, SeqNum DESC, OpType DESC`). Out-of-order keys rejected with `KeyOutOfOrderError`.
    - Atomic Staging: Writes stream to `.sst.tmp`. `Finish()` syncs file, closes descriptor, atomically renames to `.sst`, and syncs parent directory.
    - Failure Cleanup: Unfinalized close or disk error unlinks the `.tmp` file, preventing orphaned corrupt files.
    - Overwrite Protection: Pre-existing finalized `.sst` files cannot be overwritten (`ErrSSTableExists`).
    - Disjoint Regions: Data blocks, meta-index, index, and footer occupy non-overlapping physical file offsets.
    - Empty Table Semantics: 0 records produces a valid 64-byte file (8B meta + 8B index + 48B footer) satisfying all footer and file-bound constraints.
  * *Tests*:
    - Single-block SSTable verification with independent byte-by-byte inspection of every region and CRC.
    - Multi-block SSTable (>5 blocks) verifying contiguous offsets, monotone handles, and index searchability.
    - Empty SSTable producing valid 64-byte file.
    - Key ordering enforcement and duplicate key rejection.
    - Caller mutation isolation on key and value buffers.
    - Staging file creation, atomic rename, and abandoned close cleanup.
    - Pre-existing file conflict rejection.
    - Lifecycle state machine tests (repeated finish, add after finish/close, close after finish).
    - Fault injection tests: write failure, short write, sync failure, close failure.
    - Integration test with `memtable.SkipList.NewIterator()` via `TableWriter.Build(iter)`.
  * *Benchmarks* (Apple M4, darwin/arm64):
    - `BenchmarkTableWriter_Sequential_1K`: 8.63 ms/op (writing & fsyncing 1,000 records).
    - `BenchmarkTableWriter_Sequential_10K`: 10.17 ms/op (writing & fsyncing 10,000 records, ~1MB SSTable, ~1M keys/sec).
    - `BenchmarkTableWriter_Add_DirectFile`: 143.7 ns/op (~7M ops/sec in-memory buffering).
  * *Fuzz Testing*:
    - `FuzzTableWriter`: 1,474 full file lifecycle executions in 11s with 0 failures, 0 panics, 0 file leaks.
  * *Completion*: Complete and verified under `-race`, `golangci-lint`, Linux and Windows `go vet`.
* **P04-S03-M02: SSTable Block Reader & Sparse Index Binary Search**
  * *Status*: Complete
  * *Objective*: Open SSTable, read footer, load index block into RAM, binary search for target key's block handle, read candidate data block via positional `ReadAt`, decode prefix-compressed records, and perform exact point lookup.
  * *Architecture & Flow*:
    - Open file descriptor and stat physical file size.
    - Read fixed 48-byte footer anchored at `[fileSize - 48 : fileSize]`.
    - Decode and validate footer structure, format magic number (`0x4C41545453535401`), and 8-byte zero padding.
    - Validate footer block handles against physical file boundaries via `ValidateAgainstFileSize`.
    - Read sparse index block bytes from disk at `IndexHandle.Offset` for `IndexHandle.Size` bytes via bounded `ReadAt`.
    - Decode `BlockIndex` and retain index entries in RAM.
    - `Seek(userKey []byte)` executes two-level binary search:
      1. Sparse in-memory binary search via `BlockIndex.FindBlock`: finds first entry where `LargestKey >= targetKey`. If `targetKey > all LargestKeys`, returns `ErrKeyNotFound` with zero disk I/O.
      2. Validates candidate data block handle bounds against physical file size.
      3. Reads exactly `handle.Size` bytes from `handle.Offset` via positional `ReadAt`.
      4. Validates data block CRC32-IEEE checksum over `[entry data || restart offsets || restart count]`.
      5. Parses and validates restart count and strictly monotonic restart offsets.
      6. Binary-searches restart points to locate nearest restart interval.
      7. Scans prefix-compressed entries forward from restart offset, reconstructing full `InternalKey`s.
      8. Multi-version resolution: first encountered match is latest version (`SeqNum DESC`). If `OpType == OpTypePut`, returns owned defensive copy of value bytes. If `OpType == OpTypeDelete`, returns `ErrKeyNotFound` (tombstone). If scanned past key (`UserKey > target`), stops and returns `ErrKeyNotFound`.
  * *Invariants*:
    - Safe Concurrency: `TableReader` is safe for concurrent `Seek` calls across multiple goroutines using `sync.RWMutex.RLock()`.
    - Non-destructive I/O: Positional `ReadAt` leaves file offsets untouched.
    - Resource Safety: `Close()` releases file descriptor; subsequent operations return `ErrTableReaderClosed`; repeated `Close()` is idempotent.
    - Zero Corruption Masking: Corrupt magic, corrupt CRC32, truncated buffers, out-of-bounds restart offsets return explicit corruption errors; never misclassified as `ErrKeyNotFound`.
    - Memory Isolation: Returned value slices are owned defensive copies.
  * *Tests*:
    - Initialization: valid file, nonexistent file, empty file (0B), truncated file (<48B), empty finalized SSTable (64B), nil file rejection.
    - Footer validation: corrupted magic, corrupted padding, out-of-bounds index handle.
    - Index validation: corrupted index CRC32 checksum rejection.
    - Single-block point lookups: exact keys, first key, last key, absent key before first, absent key between entries, absent key after last, invalid key bounds.
    - Multi-block point lookups: 1,000 keys across multiple 4KB blocks with 100% correct values and absent probes.
    - Revisions and tombstones: multi-version PUT resolution (latest returned), tombstone deletion resolution (`ErrKeyNotFound` returned).
    - Data block corruption: corrupted CRC32 rejection, zero restart count rejection, truncated buffer rejection.
    - Lifecycle: `Close()`, repeated `Close()`, `Seek()` after close (`ErrTableReaderClosed`), nil receiver safety.
    - Concurrency: 20 concurrent goroutines querying 200 keys under `go test -race` with 0 race conditions.
    - Differential testing: reference model `map[string][]byte` compared against `TableReader.Seek` across 300 mixed PUT/DELETE operations with 100% match.
    - Large dataset verification: 100,000 keys in SSTable (770 data blocks, 3.19 MB) with 100,000 / 100,000 lookups verified 100.0% correct.
  * *Benchmarks* (Apple M4, darwin/arm64):
    - `BenchmarkTableReader_Seek_HotBlock-10`: 2,633,008 ops, 877.4 ns/op, 3,537 B/op, 5 allocs/op (~1.14M point lookups/sec).
    - `BenchmarkTableReader_Seek_Random-10`: 1,998,081 ops, 1,218 ns/op, 4,776 B/op, 15 allocs/op (~821K random reads/sec).
    - `BenchmarkTableReader_Seek_Missing-10`: 23,812,101 ops, 103.9 ns/op, 150 B/op, 12 allocs/op (~9.6M absent probes/sec with zero data block disk reads).
  * *Fuzz Testing*:
    - `FuzzSearchDataBlock`: 5,345,098 executions in 10s with 0 crashes, 0 hangs, 0 memory leaks.
    - `FuzzTableReader_Seek`: 4,879,733 executions in 10s with 0 crashes, 0 hangs.
    - `FuzzTableReader_CorruptedFile`: 520 file mutations in 11s with deterministic error handling and zero crashes.
  * *Completion*: Complete and verified under `-race`, `golangci-lint`, Linux and Windows `go vet`. Sub-Phase 04.3 and Phase 04 are fully COMPLETE.

---

# Phase 05: Probabilistic Bloom Filter Subsystem

* **Major Objective**: Implement Murmur3-based Bloom filters with 10 bits/key to eliminate $>99\%$ of cold read disk accesses.
* **Dependencies**: Phase 01.
* **Risks**: Mathematical sizing errors, hash collisions, endianness bugs in bitset serialization.

### Sub-Phase 05.1: Mathematical Modeling & Bitset Implementation
* **P05-S01-M01: Bloom Filter Parameter Calculator & Bitset Allocator**
  * *Objective*: Calculate optimal bitset length ($m = n \times 10$) and hash count ($k=7$).
  * *Changes*:
    - Created `internal/filter/bloom.go`: implemented `BloomFilter`, `OptimalBitsetSize()`, `NewBloomFilter()`, and accessors (`KeyCount()`, `BitCount()`, `HashCount()`, `ByteSize()`, `Bitset()`, `IsEmpty()`). Defined fixed project policy constants `BitsPerKey = 10`, `DefaultHashFunctions = 7`, `MaxBitsetBytes = 256 MiB`, and `MaxKeyCount = 209,715,200`.
    - Created `internal/filter/bloom_test.go`: table-driven tests for basic sizing across cardinalities (1 to 10,000,000 keys), byte-rounding boundaries ($\lceil m/8 \rceil$), zero input semantics ($n=0 \implies 0$ bits, 0 bytes), negative input semantics ($n < 0 \implies \text{nil}$), integer multiplication overflow guards, `MaxKeyCount` allocation ceiling, zero-initialization verification, and nil-receiver safety.
    - Created `internal/filter/bloom_bench_test.go`: constructor allocation micro-benchmarks across 100 to 1,000,000 keys and in-register sizing calculation.
    - Created `internal/filter/bloom_fuzz_test.go`: `FuzzOptimalBitsetSize` testing arithmetic invariants and `FuzzNewBloomFilter_Bounded` testing bounded physical allocation.
  * *Invariants Maintained*:
    - *Exact Mathematical Sizing*: Bit count strictly satisfies $m = n \times 10$; byte allocation strictly satisfies $\lceil m / 8 \rceil = (m + 7) / 8$.
    - *Fixed Policy Parameters*: `BitsPerKey == 10`, `HashFunctions == 7`.
    - *Arithmetic Overflow Safety*: Sizing guards against multiplication overflow; values exceeding $(math.MaxInt - 7) / 10$ fail closed returning `(0, 0, false)`.
    - *Fail-Closed Boundary Defense*: Negative key counts and counts exceeding `MaxKeyCount` safely return `nil`.
    - *Empty Set Consistency*: $n = 0$ yields a valid non-nil empty filter ($m = 0, \text{bytes} = 0, k = 7$).
    - *Deterministic Zero-Initialization*: All allocated bitset bytes are guaranteed `0x00` (no bits set).
  * *Measured Results*:
    - Micro-benchmarks (Apple M4, darwin/arm64):
      - `BenchmarkNewBloomFilter_100`: ~36.5 ns/op (176 B/op, 2 allocs/op).
      - `BenchmarkNewBloomFilter_1K`: ~114.6 ns/op (1,328 B/op, 2 allocs/op).
      - `BenchmarkNewBloomFilter_10K`: ~640.2 ns/op (13,616 B/op, 2 allocs/op).
      - `BenchmarkNewBloomFilter_100K`: ~4,874 ns/op (131,121 B/op, 2 allocs/op).
      - `BenchmarkNewBloomFilter_1M`: ~40,052 ns/op (1,253,424 B/op, 2 allocs/op).
      - `BenchmarkOptimalBitsetSize`: ~0.27 ns/op (0 B/op, 0 allocs/op).
    - Fuzz Testing:
      - `FuzzOptimalBitsetSize`: >3,181,000 executions with 0 failures.
      - `FuzzNewBloomFilter_Bounded`: >2,784,000 executions with 0 failures.
  * *Completion*: Complete and verified under `-race`, `go vet`, `golangci-lint`.
* **P05-S01-M02: Murmur3 Double-Hashing Implementation**
  * *Objective*: Implement Kirsch-Mitzenmacher optimization generating $k=7$ hash probes from two 64-bit hash values:
    $$g_i(x) = ((h_1(x) \bmod m) + i \cdot (h_2(x) \bmod m)) \bmod m \quad \text{for } i \in [0, 6]$$
  * *Changes*:
    - Created `internal/filter/murmur3.go`: implemented canonical `Murmur3_128(data []byte, seed uint64) (uint64, uint64)` based on Austin Appleby's SMHasher `MurmurHash3_x64_128` specification. Uses 16-byte body block loops, explicit little-endian 64-bit word reads (`binary.LittleEndian.Uint64`), canonical constants ($c_1 = \text{0x87c37b91114253d5}, c_2 = \text{0x4cf5ad432745937f}$), bit rotations via `math/bits.RotateLeft64`, full 1..15 byte tail branch coverage, and finalization mixers ($fmix64$). Defined `DefaultMurmur3Seed = 0`.
    - Modified `internal/filter/bloom.go`: added `(f *BloomFilter) Add(key []byte)` setting bits across $k=7$ probes, `(f *BloomFilter) MayContain(key []byte) bool` testing membership with fail-fast optimization, and `(f *BloomFilter) Probes(key []byte) []uint64` for testability and diagnostics.
    - Created `internal/filter/murmur3_test.go`: test vectors for empty/nil keys, short strings, standard pangram, canonical Austin Appleby SMHasher `VerificationTest` asserting `0x6384BA69` across 256 keys, tail coverage across all 16 tail lengths, large inputs up to 1 MB, slice capacity invariance, and zero-allocation assertions.
    - Modified `internal/filter/bloom_test.go`: added 10,000-key zero false negative verification, probe in-bounds checks, byte boundary bit positions, `Add` idempotence, insertion order independence, cross-filter storage isolation, empty filter semantics ($n=0$ safe no-op / returns false), nil receiver safety, nil/empty key support, bounded absent key sanity check (~0.70% observed rate vs ~0.82% theoretical), and zero-allocation assertions.
    - Modified `internal/filter/bloom_bench_test.go`: added microbenchmarks for Murmur3 (16B, 64B, 256B, 1KB, 4KB), `Add_10K`, `MayContain_Hit`, and `MayContain_Miss`.
    - Modified `internal/filter/bloom_fuzz_test.go`: added `FuzzMurmur3_128` and `FuzzBloomFilter_AddMayContain` property-based fuzz tests.
  * *Invariants Maintained*:
    - *Zero False Negatives*: For every key $k$ added via `Add(k)`, `MayContain(k)` is mathematically guaranteed to return `true`.
    - *Overflow-Safe Probing*: Probes are derived via modular reduction avoiding 64-bit unsigned integer wrap-around before the modulo operation.
    - *Probe Bounds*: Every generated probe satisfies $0 \le g_i(x) < m$ and maps to a byte index within `len(bitset)`.
    - *Fail-Fast Lookup*: `MayContain` halts on the first unset probe bit (averaging ~7.4 ns/op on misses).
    - *Zero Heap Allocations*: Hashing and membership operations execute with 0 B/op and 0 allocs/op.
    - *Concurrency Model*: Single-writer construction/population (`Add`), concurrent thread-safe reading (`MayContain`).
  * *Measured Results*:
    - Micro-benchmarks (Apple M4, darwin/arm64):
      - `BenchmarkMurmur3_16B`: ~3.29 ns/op (4,861 MB/s, 0 B/op, 0 allocs/op).
      - `BenchmarkMurmur3_64B`: ~7.86 ns/op (8,143 MB/s, 0 B/op, 0 allocs/op).
      - `BenchmarkMurmur3_256B`: ~26.97 ns/op (9,490 MB/s, 0 B/op, 0 allocs/op).
      - `BenchmarkMurmur3_1KB`: ~118.3 ns/op (8,658 MB/s, 0 B/op, 0 allocs/op).
      - `BenchmarkMurmur3_4KB`: ~479.3 ns/op (8,545 MB/s, 0 B/op, 0 allocs/op).
      - `BenchmarkBloomFilter_Add_10K`: ~14.35 ns/op (0 B/op, 0 allocs/op).
      - `BenchmarkBloomFilter_MayContain_Hit`: ~15.10 ns/op (0 B/op, 0 allocs/op).
      - `BenchmarkBloomFilter_MayContain_Miss`: ~7.40 ns/op (0 B/op, 0 allocs/op).
    - Fuzz Testing:
      - `FuzzMurmur3_128`: >1,536,000 executions with 0 failures.
      - `FuzzBloomFilter_AddMayContain`: >1,507,000 executions with 0 failures.
  * *Completion*: Complete and verified under `-race`, `go vet`, `golangci-lint`.

### Sub-Phase 05.2: Filter Block Serialization & Empirical Testing
* **P05-S02-M01: Filter Block Binary Serialization**
  * *Objective*: Establish persistent binary wire representation of the Bloom filter as an SSTable filter block, append $k$ count byte, and integrate into SSTable filter block and MetaIndex block.
  * *Binary Wire Format*:
    - `[Bitset Payload (N bytes, N = ceil(m/8))]`
    - `[BitCount m (8 bytes, Big-Endian uint64)]`
    - `[HashCount k (1 byte, uint8 = 7)]`
    - `[CRC32-IEEE Checksum (4 bytes, Big-Endian uint32)]`
    - Trailer size: 13 bytes (`FilterBlockTrailerSize = 13`).
    - Checksum covers `Bitset || BitCount || HashCount`.
    - Empty filter ($m=0, N=0$): exactly 13 bytes (`0B bitset || 8B 0x00 || 1B 0x07 || 4B CRC32`).
  * *Changes*:
    - `internal/errors`: Added `ErrFilterBlockTruncated`, `ErrFilterBlockCorrupted`, `ErrUnsupportedHashCount`, `ErrFilterFinished`, `FilterBlockCorruptedError`.
    - `internal/filter/bloom.go`: Added `FilterBlockTrailerSize = 13`, `FilterMetaKey = "filter.bloom"`, `(f *BloomFilter) Encode() []byte`, `EncodeFilterBlock(f *BloomFilter) []byte`, `DecodeFilterBlock(data []byte) (*BloomFilter, error)`.
    - `internal/filter/filter_builder.go`: Implemented `FilterBlockBuilder` with lifecycle (`NewFilterBlockBuilder`, `NewFilterBlockBuilderWithFilter`, `AddKey`, `Finish`, `Reset`, `Filter`, `AddedKeys`, `IsEmpty`, `CurrentSizeEstimate`).
    - `internal/sstable/meta_index.go`: Implemented `BuildMetaIndexBlock(entries map[string]BlockHandle) []byte`, `DecodeMetaIndexBlock(data []byte) (map[string]BlockHandle, error)`, `FindMetaIndexEntry(data []byte, key string) (BlockHandle, bool, error)`.
    - `internal/sstable/table_writer.go`: Wired `FilterBuilder *filter.FilterBlockBuilder` into `TableWriterOptions`; flushes Filter Block before MetaIndex block and records `FilterHandle` in `SSTableMetadata`.
    - `internal/sstable/table_reader.go`: Added `(r *TableReader) ReadFilterBlock() (*filter.BloomFilter, error)` for fail-closed disk retrieval and validation.
  * *Invariants & Security*:
    - Bounds validation: `len(data) <= MaxBitsetBytes + 13` enforced before allocation (guards against memory exhaustion DoS).
    - Exact probe preservation: Storing exact `bitCount` preserves modular reduction without false negatives on non-multiple-of-8 bitsets.
    - Fail-closed corruption handling: CRC32 mismatches, truncated buffers, bad $k \ne 7$, and length mismatches return explicit errors; never silently pretend keys are absent.
    - Physical region ordering: `Data Blocks` -> `Filter Block` -> `MetaIndex Block` -> `Index Block` -> `48-Byte Footer`. All regions strictly contiguous, non-overlapping, and physically bounded.
  * *Tests & Benchmarks*:
    - Exact-byte fixtures comparing against hand-calculated independent oracle.
    - Round-trip tests across $n \in \{0, 1, 2, 7, 8, 9, 10, 16, 50, 100, 1000, 5000\}$ verifying 100% membership equivalence and zero false negatives.
    - Determinism, idempotence, and ownership isolation tests.
    - Full corruption matrix: bit flips in CRC, payload, bitCount; invalid $k$; truncated buffers; bounds violations.
    - Fuzz testing: `FuzzFilterBlockCodec` verifying no panics on arbitrary malformed inputs.
    - SSTable integration tests: `TestSSTable_FilterBlockIntegration`, `TestSSTable_NoFilter_BackwardCompatibility`, `TestSSTable_CorruptedFilterBlockOnDisk`, `TestMetaIndexBlock_Codec`.
    - Microbenchmarks (Apple M4):
      - Serialize ($N=100$ keys): ~28.39 ns/op (144 B/op, 1 allocs/op)
      - Deserialize ($N=100$ keys): ~33.21 ns/op (176 B/op, 2 allocs/op)
      - Serialize ($N=1000$ keys): ~201.0 ns/op (1280 B/op, 1 allocs/op)
      - Deserialize ($N=1000$ keys): ~214.9 ns/op (1328 B/op, 2 allocs/op)
  * *Completion*: Complete and verified under `-race`, `go vet`, `golangci-lint`. P05-S02-M02 remains next planned micro-phase.
* **P05-S02-M02: Empirical False-Positive Rate Verification Benchmark**
  * *Objective*: Benchmark measuring false-positive rate across 1,000,000 non-existent keys.
  * *Tests*: Assert empirical false positive rate $p \le 0.01$ (under 1%).
  * *Completion*: Precision verified against mathematical model.

---

# Phase 06: Manifest Log & VersionSet Management

* **Major Objective**: Implement the append-only `MANIFEST` log recording atomic `VersionEdit` state transitions, paired with a `CURRENT` pointer.
* **Dependencies**: Phase 04.
* **Risks**: Corrupted manifest state, memory leak of orphaned versions.

### Sub-Phase 06.1: VersionEdit Protocol & Manifest Logging
* **P06-S01-M01: `VersionEdit` Binary Representation**
  * *Objective*: Define atomic edit struct recording `AddFile(level, meta)`, `DeleteFile(level, fileNum)`, `NextFileNum`, `LastSeqNum`.
  * *Changes*: `VersionEdit.Encode()`, `VersionEdit.Decode()`.
  * *Tests*: Test round-trip encoding of complex version edits.
  * *Completion*: VersionEdit codec verified.
* **P06-S01-M02: Append-Only MANIFEST Log Writer**
  * *Objective*: Append serialized `VersionEdit` records to `MANIFEST-000001` with CRC32 framing.
  * *Changes*: `ManifestWriter.LogEdit(edit VersionEdit) error`.
  * *Invariants*: Manifest updates are flushed via `fdatasync()`.
  * *Tests*: Append 50 edits; verify file contents and CRC integrity.
  * *Completion*: Manifest logging tested.

### Sub-Phase 06.2: CURRENT Pointer & VersionSet Invariants
* **P06-S02-M01: Atomic CURRENT Pointer File Swapper**
  * *Objective*: Write active manifest filename to `CURRENT.tmp` and swap atomically via `os.Rename()`.
  * *Changes*: `SetCurrentManifest(dir string, manifestNum uint64) error`.
  * *Tests*: Crash-safety test simulating power interruption during pointer write.
  * *Completion*: Atomic pointer swap verified.
* **P06-S02-M02: `VersionSet` & Version-Pinned Reference Counting**
  * *Objective*: Maintain linked list of active `Version` structs with atomic `Ref()` and `Unref()`.
  * *Changes*: `VersionSet.AppendVersion(v *Version)`, `Version.Ref()`, `Version.Unref()`.
  * *Invariants*: SSTable physical file descriptors are never closed while `v.refCount > 0`.
  * *Tests*: Multi-threaded test pinning versions while compactor simulates file deletions.
  * *Completion*: Reference counting verified race-clean.

---

# Phase 07: Crash Recovery & Integrity Verification

* **Major Objective**: Reconstruct complete database state upon startup from `CURRENT`, `MANIFEST`, and active WAL logs.
* **Dependencies**: Phase 02, Phase 04, Phase 06.
* **Risks**: Replay ordering bugs, failure to clean orphaned `.tmp` files.

### Sub-Phase 07.1: Manifest Replay & Version Reconstruction
* **P07-S01-M01: Boot Discovery & CURRENT Validation**
  * *Objective*: Scan data directory on startup, parse `CURRENT`, open active `MANIFEST`.
  * *Changes*: `Engine.RecoverManifest() (*Version, error)`.
  * *Tests*: Replay empty DB boot; replay populated DB boot.
  * *Completion*: Boot discovery verified.
* **P07-S01-M02: Sequential VersionEdit Replay Engine**
  * *Objective*: Replay `VersionEdit` records sequentially from manifest, reconstructing level arrays ($L_0..L_N$).
  * *Invariants*: If any referenced `.sst` file is missing from disk, abort with `ErrMissingSSTable`.
  * *Tests*: Replay 100 historical edits; verify reconstructed version matches expected level state.
  * *Completion*: Replay engine verified.

### Sub-Phase 07.2: WAL Replay & MemTable Restoration
* **P07-S02-M01: Uncommitted WAL Discovery & Replay**
  * *Objective*: Scan `/wal/` for logs newer than the manifest checkpoint, replay valid records into active MemTable.
  * *Changes*: `Engine.RecoverWAL() error`.
  * *Tests*: Write 500 records, crash process without flush; reboot and verify all 500 records present in MemTable.
  * *Completion*: WAL replay passing tests.
* **P07-S02-M02: Orphaned Temporary File Garbage Collector**
  * *Objective*: Scan directory on boot and remove unreferenced `.tmp` files left by interrupted compactions or flushes.
  * *Changes*: `Engine.CleanOrphanedFiles() error`.
  * *Tests*: Inject fake `.tmp` files; verify recovery safely purges them without touching valid SSTables.
  * *Completion*: Orphan GC verified.

---

# Phase 08: Leveled Compaction Subsystem

* **Major Objective**: Implement asynchronous Leveled Compaction ($L0 \to L1 \to L_N$) using a min-heap k-way merge iterator.
* **Dependencies**: Phase 04, Phase 06.
* **Risks**: Write stalls, ghost key resurrects caused by premature tombstone purging, disk full during merge.

### Sub-Phase 08.1: Compaction Scoring & Input Selection
* **P08-S01-M01: Level Compaction Score Heuristics**
  * *Objective*: Calculate score for $L0$ ($\text{FileCount} / 4$) and $L_1..L_N$ ($\text{TotalBytes} / \text{MaxBytes}$).
  * *Changes*: `Compactor.PickCompactionLevel() (int, float64)`.
  * *Invariants*: Level with score $\ge 1.0$ prioritized; $L0$ prioritized over deeper levels.
  * *Tests*: Unit test scoring formula across simulated level metadata.
  * *Completion*: Compaction scoring verified.
* **P08-S01-M02: Overlapping Key Range File Selector**
  * *Objective*: Pick file from $L_i$, calculate key range $[Key_{min}, Key_{max}]$, find all overlapping files in $L_{i+1}$.
  * *Changes*: `Compactor.GetOverlappingInputs(level int, f *SSTableMetadata) []*SSTableMetadata`.
  * *Tests*: Test key range intersection logic with non-overlapping and overlapping boundary cases.
  * *Completion*: File selection verified.

### Sub-Phase 08.2: K-Way Merge Sort & Tombstone Purging
* **P08-S02-M01: Min-Heap K-Way Merge Iterator**
  * *Objective*: Multi-file iterator using `container/heap` sorting by `Key` ascending, `SeqNum` descending.
  * *Changes*: `NewMergingIterator(iters []Iterator) *MergingIterator`.
  * *Invariants*: Yields newest record revision first; discards duplicate older revisions.
  * *Tests*: Merge 4 files containing overlapping keys and revisions; verify output strictly sorted and deduplicated.
  * *Completion*: K-way merge verified.
* **P08-S02-M02: Tombstone Purge Safety Invariant Enforcer**
  * *Objective*: Purge tombstone record if and only if key does not exist in any level deeper than target level.
  * *Changes*: `Compactor.CanDropTombstone(key []byte, targetLevel int) bool`.
  * *Tests*: Ghost key test: verify tombstone retained when older version exists in deeper level; verify tombstone dropped when no older version exists.
  * *Completion*: Tombstone purge safety verified.

### Sub-Phase 08.3: Compaction Execution & Manifest Commit
* **P08-S03-M01: Compaction Output SSTable Generation**
  * *Objective*: Stream merged records into new SSTable files partitioned at 2MB boundaries for $L \ge 1$.
  * *Changes*: `Compactor.Run() error`.
  * *Tests*: Execute simulated compaction; verify output files adhere to non-overlapping key range invariant.
  * *Completion*: Compaction file generation verified.
* **P08-S03-M02: Atomic Manifest Commit & Obsolete File Deletion**
  * *Objective*: Commit `VersionEdit` deleting input files and adding output files; unlink obsolete files once unpinned.
  * *Changes*: `VersionSet.LogAndApply(edit VersionEdit) error`.
  * *Tests*: Verify atomic transition and file unlinking.
  * *Completion*: Manifest commit tested.

---

# Phase 09: Sharded LRU Read Block Cache

* **Major Objective**: Implement 16-shard concurrent LRU cache for 4KB SSTable data blocks to maximize read throughput.
* **Dependencies**: Phase 01.
* **Risks**: Lock contention across CPU cores, memory leaks.

### Sub-Phase 09.1: LRU Doubly-Linked List & Sharding
* **P09-S01-M01: Cache Shard Mutex & Doubly-Linked List**
  * *Objective*: Implement single LRU cache shard using hash map and circular doubly-linked list.
  * *Changes*: `type lruShard struct`, `lruShard.Get()`, `lruShard.Put()`.
  * *Invariants*: Least recently accessed block evicted when capacity exceeded.
  * *Tests*: Insert 100 blocks with capacity 50; verify first 50 evicted in order.
  * *Completion*: Single shard verified.
* **P09-S01-M02: 16-Way Sharded Cache Partitioning with Cache-Line Padding**
  * *Objective*: Route block queries across 16 shards using high hash bits; pad shard structs to 64-byte boundaries.
  * *Changes*: `ShardedBlockCache.Get(sstableID uint64, offset uint64)`.
  * *Tests*: High-concurrency benchmark testing 64 goroutines reading cache simultaneously with zero false sharing.
  * *Completion*: Sharded cache verified.

---

# Phase 10: Single-Node Storage Engine Integration

* **Major Objective**: Wire together MemTable, WAL, SSTable reader, Compactor, Block Cache, and VersionSet into unified Engine interface.
* **Dependencies**: Phases 01 through 09.
* **Risks**: Write stalls, deadlocks between flusher and compactor.

### Sub-Phase 10.1: Engine Dispatcher & CRUD Workflows
* **P10-S01-M01: Unified `PUT`, `GET`, `DELETE` Engine API**
  * *Objective*: Expose top-level `Engine` struct implementing single-node operations.
  * *Changes*: `Engine.Put(k, v []byte)`, `Engine.Get(k []byte)`, `Engine.Delete(k []byte)`.
  * *Tests*: End-to-end CRUD test executing 50,000 operations across memory and disk.
  * *Completion*: Basic engine integration complete.
* **P10-S01-M02: Asynchronous Background Flusher Pipeline**
  * *Objective*: Monitor active MemTable size; when $>64\text{MB}$, atomically swap to `imm` and trigger background flush to $L0$.
  * *Changes*: `Engine.flushLoop()`.
  * *Invariants*: Writes continue to fresh active MemTable without blocking during flush I/O.
  * *Tests*: Ingest 200MB data; verify multiple $L0$ files generated on disk.
  * *Completion*: Flush pipeline verified.

### Sub-Phase 10.2: Write Backpressure & Graceful Shutdown
* **P10-S01-M03: Progressive Write Pacing & Stall Controller**
  * *Objective*: Throttle incoming writes when $L0$ file count exceeds 8; stall when $>12$.
  * *Changes*: `Engine.maybeDelayWrite()`.
  * *Tests*: Saturate engine with writes; verify write latency smoothly increases rather than crashing with disk exhaustion.
  * *Completion*: Write pacing verified.
* **P10-S01-M04: Clean Engine Shutdown & Resource Reclamation**
  * *Objective*: Flush active MemTable, wait for in-flight compactions to pause, sync manifest, close file descriptors.
  * *Changes*: `Engine.Close() error`.
  * *Tests*: Verify clean shutdown leaves zero uncommitted logs or open file leaks.
  * *Completion*: Clean shutdown verified.

---

# Phase 11: TCP Binary Wire Protocol & Networking Subsystem

* **Major Objective**: Implement high-throughput, length-prefixed binary framing protocol over raw TCP with frame-bomb protections.
* **Dependencies**: Phase 10.
* **Risks**: Unbounded memory allocation, slow client socket leaks, TCP head-of-line blocking.

### Sub-Phase 11.1: Frame Parsing & Protocol State Machine
* **P11-S01-M01: 18-Byte Header Decoder & CRC32 Validator**
  * *Objective*: Decode magic (`0x4C415454`), OpCode, SeqID, PayloadLen, and verify frame CRC32.
  * *Changes*: `FrameDecoder.DecodeHeader(r io.Reader) (*Header, error)`.
  * *Security*: Rejects unknown magic immediately; rejects `PayloadLength > 5MB` with `ErrFrameTooLarge`.
  * *Tests*: Test valid frames; test oversized frames; test CRC corruption.
  * *Completion*: Frame decoder verified.
* **P11-S01-M02: TCP Listener & Goroutine Connection Pool**
  * *Objective*: Accept TCP connections, assign dedicated reader goroutine, dispatch requests to engine.
  * *Changes*: `Server.Listen(addr string) error`.
  * *Tests*: Connect 100 concurrent TCP clients; execute ping-pong `PUT`/`GET` requests.
  * *Completion*: TCP server tested.

---

# Phase 12: CLI, Interactive REPL & Forensic Diagnostics

* **Major Objective**: Deliver `lattice` server binary, `lattice-cli` REPL, and `inspect-sstable` forensic tooling.
* **Dependencies**: Phase 11.

* **P12-S01-M01: Server Daemon CLI Entrypoint (`cmd/lattice`)**
  * *Objective*: Implement CLI flag parsing (`--config`, `--data-dir`, `--port`), signal handling (`SIGINT`, `SIGTERM`).
* **P12-S01-M02: Interactive REPL Client (`cmd/lattice-cli`)**
  * *Objective*: Interactive prompt supporting `PUT`, `GET`, `DELETE`, `EXISTS`, `STATS` commands with colored output.
* **P12-S01-M03: SSTable Forensic Inspection Tool (`lattice inspect-sstable`)**
  * *Objective*: Read raw SSTable file, print block counts, key ranges, Bloom filter stats, and restart points.
* **P12-S01-M04: WAL Forensic Dump Tool (`lattice dump-wal`)**
  * *Objective*: Decode and print every WAL record, sequence number, and CRC status for debugging corruptions.

---

# Phase 13: Benchmarking Suite & Performance Profiling

* **Major Objective**: Build standalone load generator (`cmd/lattice-bench`) measuring P50, P90, P99, P99.9 latencies.
* **Dependencies**: Phase 11.

* **P13-S01-M01: Zipfian Key Distribution Generator**
  * *Objective*: Generate non-uniform key access patterns ($s=0.99$) to simulate real-world read/write hotspots.
* **P13-S01-M02: High-Resolution Latency Histogram Collector**
  * *Objective*: Record nanosecond operation latencies into logarithmic buckets without GC allocation overhead.
* **P13-S01-M03: Standalone Benchmark Load Runner**
  * *Objective*: Multi-threaded client driver configurable by concurrency, read/write ratio, and duration.
* **P13-S01-M04: `pprof` CPU & Memory Profiling Integration**
  * *Objective*: Expose `/debug/pprof` endpoints on server; document profiling runbook.

---

# Phase 14: Distributed Cluster Foundations & Node Topology

* **Major Objective**: Lay the groundwork for Version 1.1 clustering: node identity, peer RPC framing, and cluster configuration.
* **Dependencies**: Phase 11.

* **P14-S01-M01: Node Identity & Cluster Configuration Model**
  * *Objective*: Parse cluster topology (Node IDs, peer IP:Port addresses) from config.
* **P14-S01-M02: Peer-to-Peer RPC Framing Protocol**
  * *Objective*: Implement binary frames for Raft RPCs (`RequestVote`, `AppendEntries`).
* **P14-S01-M03: Outbound Peer Connection Manager**
  * *Objective*: Maintain persistent TCP connection pools with automatic reconnect and keep-alive to all cluster peers.

---

# Phase 15: Raft Consensus Engine

* **Major Objective**: Implement full single-group Raft consensus: elections, randomized timers, quorum log replication.
* **Dependencies**: Phase 14.
* **Risks**: Split-vote deadlocks, term regression, uncommitted log overwrites.

### Sub-Phase 15.1: Persistent Raft State & Roles
* **P15-S01-M01: Persistent Raft State (`currentTerm`, `votedFor`, `log[]`)**
  * *Objective*: Persist Raft term and votes to disk before responding to RPCs.
* **P15-S01-M02: Role State Transitions (Follower $\leftrightarrow$ Candidate $\leftrightarrow$ Leader)**
  * *Objective*: Implement state machine governing node role transitions.

### Sub-Phase 15.2: Leader Election & Heartbeats
* **P15-S02-M01: Randomized Election Timer & `RequestVote` RPC**
  * *Objective*: Election timer ($150-300\text{ms}$); broadcast `RequestVote` when expired.
  * *Invariants*: Candidate must have log at least as up-to-date as voter to receive vote.
* **P15-S02-M02: Quorum Vote Counting & Leader Transition**
  * *Objective*: Transition to Leader upon receiving $\lfloor N/2 \rfloor + 1$ votes; send immediate heartbeats.
* **P15-S02-M03: Periodic Heartbeat Scheduler (`AppendEntries` Empty)**
  * *Objective*: Transmit heartbeats every $50\text{ms}$ to maintain leadership.

### Sub-Phase 15.3: Log Replication & Quorum Commit
* **P15-S03-M01: Proposal Ingestion & Log Append**
  * *Objective*: Leader appends client proposal to local log, sends `AppendEntries` with `prevLogIndex` and `prevLogTerm`.
* **P15-S03-M02: Log Matching Verification on Follower**
  * *Objective*: Follower verifies preceding log entry; rejects if mismatched; leader decrements `nextIndex`.
* **P15-S03-M03: Quorum Commit Index Advancement**
  * *Objective*: Leader advances `commitIndex` when majority of peers acknowledge match; signals state machine.

---

# Phase 16: Distributed State Machine Replication

* **Major Objective**: Connect Raft committed log stream to local LSM storage engine state machine.
* **Dependencies**: Phase 10, Phase 15.

* **P16-S01-M01: State Machine Apply Loop**
  * *Objective*: Consume committed Raft entries sequentially and apply them to local LSM engine.
* **P16-S01-M02: Client Proposal Routing & Follower Redirection**
  * *Objective*: Follower nodes intercept client write requests and return redirect response with Leader address.

---

# Phase 17: Linearizable Reads (ReadIndex Protocol)

* **Major Objective**: Eliminate stale reads under network partitions using the `ReadIndex` protocol.
* **Dependencies**: Phase 16.

* **P17-S01-M01: Leader Quorum Heartbeat Verification**
  * *Objective*: Record current `commitIndex`, broadcast heartbeat to confirm active majority leadership.
* **P17-S01-M02: State Machine Read Barrier Execution**
  * *Objective*: Wait until local state machine applies up to recorded `commitIndex`, then serve read.
  * *Tests*: Partition simulation verifying stale reads are never returned.

---

# Phase 18: Fault Injection & Chaos Testing Suite

* **Major Objective**: Programmatically inject process crashes, torn writes, and network partitions to prove fault tolerance.
* **Dependencies**: Phase 17.

* **P18-S01-M01: Jepsen-Style Network Partition Simulation**
  * *Objective*: Drop TCP packets between isolated leader and peers; assert zero split-brain writes committed.
* **P18-S01-M02: Abrupt `SIGKILL` Chaos Monkey Loop**
  * *Objective*: Continuously write data while sending random `kill -9` signals; assert zero acknowledged write loss.

---

# Phase 19: Comprehensive Security Hardening

* **Major Objective**: Eliminate memory leaks, buffer overflows, path traversal, and connection exhaustion vulnerabilities.
* **Dependencies**: Phase 11, Phase 18.

* **P19-S01-M01: Strict Path Traversal Sanitization**
  * *Objective*: Validate all file paths to prevent directory escaping attacks.
* **P19-S01-M02: Network Connection Limits & Slowloris Protection**
  * *Objective*: Enforce read/write connection deadlines and client connection limits ($4,096$).

---

# Phase 20: Production Hardening & Operational Observability

* **Major Objective**: Instrument Prometheus metrics, structured health checks, and diagnostics.
* **Dependencies**: Phase 19.

* **P20-S01-M01: Prometheus Metrics HTTP Endpoint (`:9100/metrics`)**
  * *Objective*: Export histograms for write/read latency, WAL bytes, compaction duration.
* **P20-S01-M02: Liveness & Readiness Probes**
  * *Objective*: HTTP endpoints reporting cluster health and disk storage thresholds.

---

# Phase 21: Resume & Technical Interview Portfolio Validation

* **Major Objective**: Final review of all benchmark claims, test logs, code cleanliness, and interview readiness.
* **Dependencies**: Phases 00 through 20.

* **P21-S01-M01: Benchmark Evidence Verification & Documentation**
  * *Objective*: Run reproducible 60-second benchmark on clean hardware; record P50/P99 latencies in README.
* **P21-S01-M02: Interview Defense Rehearsal & Knowledge Base Audit**
  * *Objective*: Complete final verification against [`docs/interview-knowledge.md`](interview-knowledge.md).

---

*End of Implementation Plan — Lattice v1.0.0-EXEC-PLAN*

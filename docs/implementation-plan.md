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
Current Major Phase           : Phase 02 — Write-Ahead Log (WAL) & Durability Subsystem
Current Sub-Phase             : Sub-Phase 02.1 — WAL Binary Record Layout & Serialization
Current Micro-Phase           : P02-S01-M02 — WAL Full Record Serializer & Deserializer
Phase 01 Status               : COMPLETE (Sub-Phases 01.1 & 01.2 Complete)
Previous Completed Phase      : Phase 01 — Core Storage Primitives & Binary Encodings
Previous Completed Micro-Phase: P02-S01-M01 — WAL Record Header & Framing Definition
Phase 00 Final Audit          : Completed — PASS WITH REMEDIATIONS
Phase 01 Final Audit          : Completed — PASS WITH REMEDIATIONS
Blocking Issues               : None
Tests Passing                 : `go test -race ./...` (14/14 error suites, 18/18 logger suites, 48/48 binary suites, 12/12 wal suites passing, 100% binary & wal coverage), `golangci-lint run ./...` clean (0 issues), `go mod verify` passed
Security Review Status        : Complete & Verified (Early BCE bounds checks prevent torn writes; invalid record type interception guards against uninitialized memory; >3.2M fuzz iterations passing with 0 crashes)
Interview Knowledge Status    : Updated with WAL physical 21-byte header layout, framing offsets, and zero-allocation encoding invariants
Git Commit                    : feat(wal): [P02-S01-M01] implement 21-byte WAL record header and framing
```

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
  * *Objective*: Serialize complete records (`Header + KeyLen + Key + ValLen + Val`).
  * *Changes*: `EncodeRecord(record Record) ([]byte, error)`, `DecodeRecord(r io.Reader) (Record, error)`.
  * *Invariants*: CRC32 is calculated over all bytes following the CRC field itself.
  * *Tests*: Encode and decode records of varying sizes; verify CRC matches.
  * *Completion*: Full record codec verified.
* **P02-S01-M03: WAL Corruption & Checksum Verification Tests**
  * *Objective*: Prove that corrupted bytes are intercepted.
  * *Changes*: Test suite mutating random bytes in serialized records and verifying `ErrChecksumMismatch`.
  * *Tests*: Single-bit flip tests across header, key, and value fields.
  * *Completion*: 100% of bit flips detected.

### Sub-Phase 02.2: WAL File Management & Append Operations
* **P02-S02-M01: WAL File Creator & Directory Initializer**
  * *Objective*: Safely initialize `<db_path>/wal/` directory with `0700` permissions.
  * *Security*: Strict POSIX permissions prevent other local users from reading WAL data.
  * *Tests*: Verify directory creation and permission bits on filesystem.
  * *Completion*: Directory management verified.
* **P02-S02-M02: Synchronous WAL Appender (`Strict Sync`)**
  * *Objective*: Implement sequential file writer calling `file.Write()` followed by `fdatasync()`.
  * *Changes*: `WALWriter.AppendSync(rec Record) error`.
  * *Invariants*: Method does not return until `fdatasync()` completes.
  * *Tests*: Append 1,000 records; verify file length and verify records replay cleanly.
  * *Completion*: Synchronous appender passing tests.
* **P02-S02-M03: Sequential WAL Reader & Log Iterator**
  * *Objective*: Implement `WALReader` streaming records from disk from offset 0 to EOF.
  * *Changes*: `WALReader.Next() (Record, error)`.
  * *Tests*: Read back sequential log; verify sequence number monotonicity.
  * *Completion*: Reader verified against multi-record log.

### Sub-Phase 02.3: Torn Write Handling & Log Rotation
* **P02-S03-M01: Torn Tail Write Detection & Safe Truncation**
  * *Objective*: Implement recovery logic that detects partial writes at the end of the file and truncates them.
  * *Changes*: `WALReader.RecoverAndTruncate() error`.
  * *Invariants*: Mid-log corruption returns fatal error; EOF partial write truncates cleanly.
  * *Tests*: Append partial record bytes to EOF; verify reader truncates and recovers preceding valid records.
  * *Completion*: Torn write test passing.
* **P02-S03-M02: WAL Segment Rotation & Pre-allocation**
  * *Objective*: Rotate WAL file when size exceeds 64MB; pre-allocate via `fallocate()`.
  * *Changes*: `WALWriter.Rotate() (*WALSegment, error)`.
  * *Tests*: Append records exceeding 64MB; verify new segment `wal_000000000002.log` created.
  * *Completion*: Segment rotation verified.

### Sub-Phase 02.4: Group Commit Coalescing Pipeline
* **P02-S04-M01: Group Commit Queue & Write Task Types**
  * *Objective*: Implement concurrency-safe task channel holding pending writes and completion channels.
  * *Changes*: `type writeTask struct`, `type groupCommitQueue struct`.
  * *Invariants*: Memory queue bounded to avoid unbounded RAM accumulation.
  * *Tests*: Multi-threaded enqueuing test under mock sync.
  * *Completion*: Queue tested under concurrent load.
* **P02-S04-M02: Group Commit Batch Runner & Cooperative fsync**
  * *Objective*: Dedicated batch runner coalescing up to 1,024 writes or 64KB into a single `fdatasync()`.
  * *Changes*: `groupCommitRunner()` event loop.
  * *Invariants*: If sync succeeds, all tasks in batch notified of success; on failure, all receive error.
  * *Tests*: 100 concurrent goroutines writing simultaneously; verify exactly 1 `fdatasync` per batch.
  * *Completion*: Group commit verified with race detector clean.

---

# Phase 03: In-Memory MemTable & Concurrent SkipList

* **Major Objective**: Implement the probabilistic concurrent SkipList supporting lock-free reads and exact memory accounting.
* **Dependencies**: Phase 01.
* **Risks**: Data races during concurrent forward pointer traversal, memory leaks.

### Sub-Phase 03.1: SkipList Node & Level Generation
* **P03-S01-M01: SkipList Node Memory Representation & Geometric Randomizer**
  * *Objective*: Define node struct with forward pointer slices and geometric height randomizer ($p=0.25, L_{max}=16$).
  * *Changes*: `newSkipListNode`, `randomHeight() int`.
  * *Tests*: Statistical test verifying height distribution matches geometric curve over $100,000$ iterations.
  * *Completion*: Height generator verified.
* **P03-S01-M02: Single-Threaded SkipList Insertion & Lookup**
  * *Objective*: Implement sequential `Insert(InternalKey, Value)` and `Search(UserKey)`.
  * *Invariants*: Nodes maintained in strict ascending sorted order.
  * *Tests*: Insert 10,000 random keys; verify all keys found; verify non-existent keys return nil.
  * *Completion*: Basic SkipList tested.

### Sub-Phase 03.2: Concurrent Traversal & Memory Accounting
* **P03-S02-M01: Lock-Free Read Traversal via Atomic Pointer Reads**
  * *Objective*: Use `atomic.LoadPointer` for traversing forward pointers, allowing readers to search without mutexes.
  * *Changes*: `SearchConcurrent(key []byte) (Value, bool)`.
  * *Invariants*: Readers never block writers; writers lock exclusively during pointer splicing.
  * *Tests*: 16 reader goroutines + 1 writer goroutine running simultaneously under `go test -race`.
  * *Completion*: Concurrent reads verified race-free.
* **P03-S02-M02: Exact Byte-Level Memory Accounting**
  * *Objective*: Track exact heap consumption of keys, values, node structs, and pointer arrays.
  * *Changes*: `MemTable.ByteSize() uint64`.
  * *Invariants*: `ByteSize()` updated atomically on each insertion.
  * *Tests*: Insert known sizes; verify reported `ByteSize()` matches expected memory overhead within 1%.
  * *Completion*: Memory accounting verified.

### Sub-Phase 03.3: MemTable Iteration & Immutable Transition
* **P03-S03-M01: Forward Iterator Implementation**
  * *Objective*: Implement `Iterator` interface (`Seek`, `Next`, `Valid`, `Key`, `Value`).
  * *Invariants*: Iterator yields records in ascending internal key order.
  * *Tests*: Iterate through populated MemTable; assert lexicographical monotonicity.
  * *Completion*: Iterator verified.
* **P03-S03-M02: Atomic MemTable Freeze & Immutable Transition**
  * *Objective*: Transition active MemTable to read-only `ImmutableMemTable`.
  * *Changes*: `MemTable.Freeze()`, insert attempts on frozen table return `ErrMemTableFrozen`.
  * *Tests*: Verify frozen table accepts no new writes but serves reads and iterations.
  * *Completion*: Freeze transition tested.

---

# Phase 04: Persistent SSTable Subsystem

* **Major Objective**: Construct immutable binary SSTables with prefix-compressed data blocks, sparse indexes, and 48-byte footers.
* **Dependencies**: Phase 01, Phase 03.
* **Risks**: Corrupted block alignments, binary search seek failures.

### Sub-Phase 04.1: Data Block Construction & Prefix Compression
* **P04-S01-M01: Data Block Builder with Prefix Compression**
  * *Objective*: Implement `BlockBuilder` compressing consecutive sorted keys via shared prefix lengths.
  * *Changes*: `BlockBuilder.Add(key, value []byte)`.
  * *Invariants*: Restart points emitted every 16 keys with `SharedLen = 0`.
  * *Tests*: Compress sequential keys (`user:1001`, `user:1002`); verify compression ratio $> 2\times$.
  * *Completion*: Block builder compression verified.
* **P04-S01-M02: Restart Array & Block Trailer Serialization**
  * *Objective*: Append 32-bit restart point offsets and restart count to block tail; add CRC32 trailer.
  * *Changes*: `BlockBuilder.Finish() []byte`.
  * *Invariants*: Block ends with restart array followed by 1-byte compression type and 4-byte CRC32.
  * *Tests*: Verify block trailer offset calculations.
  * *Completion*: Block serialization complete.

### Sub-Phase 04.2: SSTable Index & Footer Design
* **P04-S02-M01: Sparse Two-Level Block Index Builder**
  * *Objective*: Record largest key and file offset/size handle for each emitted data block.
  * *Changes*: `IndexBuilder.AddBlock(largestKey []byte, handle BlockHandle)`.
  * *Invariants*: Exactly one index entry per data block.
  * *Tests*: Build index across 50 data blocks; verify all handles point to correct offsets.
  * *Completion*: Index builder verified.
* **P04-S02-M02: Fixed 48-Byte Footer Serializer & Parser**
  * *Objective*: Implement encoding/decoding of 48-byte trailer (`MetaIndexHandle + IndexHandle + Padding + Magic`).
  * *Changes*: `Footer.Encode()`, `Footer.Decode()`.
  * *Invariants*: Magic equals `0x4C41545453535401`.
  * *Tests*: Validate round-trip footer encode/decode; test rejection of invalid magic numbers.
  * *Completion*: Footer codec verified.

### Sub-Phase 04.3: SSTable File Writer & Reader
* **P04-S03-M01: SSTable Sequential File Writer (`TableWriter`)**
  * *Objective*: Stream data blocks from MemTable iterator to `.sst.tmp` file; write filter, index, footer; `fdatasync()`.
  * *Changes*: `TableWriter.Build(iter MemTableIterator) (*SSTableMetadata, error)`.
  * *Invariants*: File synced to disk before renaming to final `.sst` name.
  * *Tests*: Flush a 4MB MemTable to SSTable; inspect binary layout.
  * *Completion*: SSTable writer passing tests.
* **P04-S03-M02: SSTable Block Reader & Sparse Index Binary Search**
  * *Objective*: Open SSTable, read footer, load index block into RAM, binary search for target key's block handle.
  * *Changes*: `TableReader.Seek(key []byte) ([]byte, error)`.
  * *Tests*: Point lookup across 100,000 keys in SSTable; verify 100% correct values.
  * *Completion*: SSTable reader verified.

---

# Phase 05: Probabilistic Bloom Filter Subsystem

* **Major Objective**: Implement Murmur3-based Bloom filters with 10 bits/key to eliminate $>99\%$ of cold read disk accesses.
* **Dependencies**: Phase 01.
* **Risks**: Mathematical sizing errors, hash collisions, endianness bugs in bitset serialization.

### Sub-Phase 05.1: Mathematical Modeling & Bitset Implementation
* **P05-S01-M01: Bloom Filter Parameter Calculator & Bitset Allocator**
  * *Objective*: Calculate optimal bitset length ($m = n \times 10$) and hash count ($k=7$).
  * *Changes*: `NewBloomFilter(keyCount int) *BloomFilter`.
  * *Tests*: Validate bitset memory sizing across various key counts ($100$ to $10,000,000$).
  * *Completion*: Sizing verified.
* **P05-S01-M02: Murmur3 Double-Hashing Implementation**
  * *Objective*: Implement Kirsch-Mitzenmacher optimization generating $k$ hashes from two 64-bit hash values:
    $$g_i(x) = h_1(x) + i \cdot h_2(x) \pmod{m}$$
  * *Changes*: `BloomFilter.Add(key []byte)`, `BloomFilter.MayContain(key []byte) bool`.
  * *Tests*: Insert 10,000 keys; verify `MayContain` returns true for all 10,000 keys (zero false negatives).
  * *Completion*: Membership testing verified.

### Sub-Phase 05.2: Filter Block Serialization & Empirical Testing
* **P05-S02-M01: Filter Block Binary Serialization**
  * *Objective*: Serialize Bloom bitset, append $k$ count byte, and integrate into SSTable filter block.
  * *Changes*: `FilterBlockBuilder.Finish() []byte`.
  * *Tests*: Round-trip filter serialization and deserialization.
  * *Completion*: Filter block codec verified.
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

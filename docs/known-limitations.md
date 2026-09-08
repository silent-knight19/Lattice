# Lattice: Known Limitations & Architectural Boundaries

* **Document Version**: 1.0.0-LIVING-DOC
* **Status**: Active Living Document
* **Policy**: Every limitation discovered through architecture design, implementation constraints, automated testing, or empirical benchmarking must be truthfully recorded here.

---

## Overview

A credible systems engineering project does not pretend to solve all problems simultaneously. Distributed storage engines involve fundamental trade-offs dictated by physics, operating systems, and computer science theorems (e.g., CAP theorem, PACELC theorem, RUM conjecture). 

This document tracks all **genuine architectural and operational limitations** of Lattice, their underlying causes, mitigations, and planned future evolutions.

---

## Living Limitations Register

### 1. Single-Group Raft Write Throughput Bottleneck
* **Limitation**: All write requests must be sequenced and committed by a single active Raft leader node.
* **Why It Exists**: Version 1.1 implements single-group consensus to guarantee strict linearizability without the immense complexity of distributed transaction managers (2PC) or multi-raft coordinator groups.
* **Impact**: Total cluster write throughput cannot exceed what a single node's NVMe drive and network interface can ingest ($~80\text{k}-100\text{k}$ writes/sec design target).
* **How It Was Detected**: Inherent to single-group consensus topology (Section 27 of `docs/architecture-spec.md`).
* **Current Mitigation**: Group Commit coalescing spreads `fdatasync()` overhead across up to 1,024 concurrent client operations per batch.
* **Future Solution**: Multi-Raft horizontal sharding with range partitioning (Post-V1 roadmap).
* **Dimensional Impact**:
  * Correctness: **None** (Linearizability strictly preserved).
  * Performance: **Moderate** write throughput ceiling under high-node clusters.
  * Scalability: **High** (Cannot scale write throughput horizontally by adding nodes).

---

### 2. Dataset Size Constrained by Single Node Disk Headroom
* **Limitation**: The entire active dataset must fit on the local disk volumes of the individual storage nodes.
* **Why It Exists**: Single-group Raft replicates the entire state machine to every cluster participant.
* **Impact**: Maximum database capacity is bounded by the smallest disk volume among active quorum replicas.
* **How It Was Detected**: Design invariant of replicated state machine architectures.
* **Current Mitigation**: SSTable prefix compression and 10 bits/key Bloom filters maximize data density.
* **Future Solution**: Multi-Raft partitioning where each shard holds a disjoint subset of the key range.
* **Dimensional Impact**:
  * Correctness: **None**.
  * Performance: **None**.
  * Scalability: **High** (Storage capacity bounded by single machine disk).

---

### 3. No Distributed Multi-Key ACID Transactions
* **Limitation**: Multi-key atomicity is supported only within a single `BATCH` executed on a single node or single Raft group. Distributed two-phase commit across independent partitions is not supported.
* **Why It Exists**: Distributed 2PC introduces coordinator failure modes, latency spikes, and blocking locks that are non-goals for this key-value engine.
* **Impact**: Applications requiring multi-key atomic transactions across distinct shards must implement coordination at the application tier.
* **How It Was Detected**: Explicitly excluded in Non-Goals (Section 4 of `docs/architecture-spec.md`).
* **Current Mitigation**: `BATCH` provides atomic all-or-nothing execution within the local storage engine and Raft log.
* **Future Solution**: Decentralized timestamp ordering or Percolator-style distributed transactions (Future Roadmap).
* **Dimensional Impact**:
  * Correctness: **None** (Single-key and batch operations remain strictly atomic).
  * Performance: **None**.
  * Scalability: **Low** (Fits standard distributed KV use cases like etcd, Consul).

---

### 4. Temporary Write Stalls During Major Compaction Spikes
* **Limitation**: Heavy write bursts that significantly outpace compaction speed will trigger progressive write pacing delays ($1\text{ms}-50\text{ms}$).
* **Why It Exists**: In Leveled Compaction, if $L_0$ accumulates too many overlapping files ($\ge 8$), point lookup performance collapses. Writes must be throttled to allow background compactor threads to merge files into $L_1$.
* **Impact**: High-percentile tail latency (P99 / P99.9) increases during prolonged maximum-throughput write bursts.
* **How It Was Detected**: Classic LSM-tree compaction dynamics (Section 46 of `docs/architecture-spec.md`).
* **Current Mitigation**: Progressive two-tier write pacing (1ms delay at 8 files, hard throttle at 12 files) prevents sudden latency cliffs.
* **Future Solution**: Dynamic compaction thread pools and partitioned sub-compaction runs.
* **Dimensional Impact**:
  * Correctness: **None** (Data integrity preserved).
  * Performance: **Moderate** tail latency impact under sustained write overload.
  * Scalability: **None**.

---

### 5. Memory Footprint Under Extremely Large Key Spans
* **Limitation**: Resident memory usage scales with the total number of SSTables because sparse block indexes and Bloom filters remain pinned in RAM.
* **Why It Exists**: To guarantee that cold point lookups do not incur multiple disk seeks, block indexes and Bloom filters are loaded into memory when an SSTable is opened.
* **Impact**: If a node stores hundreds of millions of tiny keys, the aggregate Bloom filter bitsets ($10\text{ bits/key}$) and index arrays can consume several gigabytes of RAM.
* **How It Was Detected**: Memory modeling of the LSM-tree hierarchy.
* **Current Mitigation**: Restart points every 16 records reduce index size by $>90\%$; 10 bits/key is an optimal balance for $<1\%$ false positives.
* **Future Solution**: Evictable two-level block index caching (loading index blocks into the block cache on demand).
* **Dimensional Impact**:
  * Correctness: **None**.
  * Performance: **None**.
  * Scalability: **Moderate** (RAM consumption scales with key count).

---

### 6. Read Amplification on Pure Cold Random Reads
* **Limitation**: If a requested key exists on disk but has not been accessed recently (misses the LRU block cache), reading it requires reading a 4KB block from physical NVMe storage.
* **Why It Exists**: LSM-trees partition data across levels rather than updating in-place. While Bloom filters prevent reading files that *do not* contain the key, retrieving an existing cold key requires an actual disk read.
* **Impact**: Cold random read throughput is bounded by physical NVMe random read IOPS ($~35\text{k}-50\text{k}$ ops/sec per drive).
* **How It Was Detected**: Hardware physical characteristics of solid-state storage.
* **Current Mitigation**: 16-shard LRU block cache keeps hot 4KB blocks in RAM without mutex contention.
* **Future Solution**: Asynchronous batched prefetching via Linux `io_uring`.
* **Dimensional Impact**:
  * Correctness: **None**.
  * Performance: **Bounded by physical storage hardware**.
  * Scalability: **None**.

---

### 7. Varint Non-Canonical (Overlong) Encoding Acceptance
* **Limitation**: The varint decoder (`GetVarint64`) accepts non-canonical (overlong) byte sequences representing values $\le 2^{64}-1$ up to 10 bytes (e.g., `0` encoded as `[0x80, 0x00]`), rather than rejecting all non-minimal encodings.
* **Why It Exists**: To maintain broad binary compatibility with standard library encoders (`encoding/binary.PutUvarint`) and third-party tools that may emit non-canonical representations.
* **Impact**: Two distinct byte sequences can decode to the same `uint64` value.
* **How It Was Detected**: Architectural audit of Phase 01 binary primitives.
* **Current Mitigation**: Lattice's encoder (`PutVarint64`) strictly emits minimal canonical forms; decoder strictly enforces the 10-byte bound and rejects integer overflow (`b > 1` on 10th byte).
* **Future Solution**: Provide a strict-mode decoder (`GetCanonicalVarint64`) if cryptographic or deterministic hashing requirements demand strict canonical representations.
* **Dimensional Impact**:
  * Correctness: **None** (Values decode mathematically correctly).
  * Performance: **None**.
  * Scalability: **None**.

---

### 8. InternalKey Zero-Allocation Borrowing Requires Caller Mutation Discipline
* **Limitation**: Direct initialization of `InternalKey` struct literals (`InternalKey{UserKey: ...}`) borrows the caller's slice without allocation, bypassing `NewInternalKey`'s defensive copy.
* **Why It Exists**: High-performance inner loops (e.g., point lookups and range scans across MemTable and SSTable blocks) cannot tolerate heap allocations per comparison.
* **Impact**: If a caller mutates the borrowed `UserKey` slice after passing it to an in-memory component, internal index ordering can be corrupted.
* **How It Was Detected**: Phase 01 memory safety and ownership audit.
* **Current Mitigation**: `NewInternalKey` and `DecodeInternalKey` defensively copy slices on ingest; borrowed struct literals are restricted by convention to read-only comparator probes.
* **Future Solution**: Continue enforcing ownership boundaries across package boundaries in engine and MemTable abstractions.
* **Dimensional Impact**:
  * Correctness: **High if misused** (Caller must respect read-only convention).
  * Performance: **Optimal** (Enables $0\text{ B/op}$ read path).
  * Scalability: **None**.

---

### 9. Scalar Sequence Number Monotonicity vs Global Atomic Allocation
* **Limitation**: `SeqNum.Next()` provides local incrementation with overflow detection (`ErrSeqNumOverflow`), but does not coordinate sequence numbers across concurrent goroutines.
* **Why It Exists**: `SeqNum` is a value-type primitive (`type SeqNum uint64`) in `internal/binary`, designed to be lightweight, zero-allocation, and register-passed without lock overhead.
* **Impact**: Calling `Next()` on a local `SeqNum` value cannot be used directly as a thread-safe sequence generator.
* **How It Was Detected**: Architectural consistency audit between representation primitives and concurrency models.
* **Current Mitigation**: Documented distinction between representation type and stateful allocator.
* **Future Solution**: Phase 02 WAL writer introduces atomic global sequence coordinator (`atomic.Uint64`).
* **Dimensional Impact**:
  * Correctness: **None** (Enforced at architectural layer).
  * Performance: **Optimal**.
  * Scalability: **None**.

---

### 10. Key-Based Redaction Heuristic for Unrecognized Non-Sensitive Keys
* **Limitation**: Key-based log redaction automatically masks attributes matching known sensitive keywords or configured custom keys (and sensitive keys take strict precedence over custom `Redactable` values). However, if arbitrary credentials or secrets are logged under an innocuous, unrecognized key name (e.g. `description`, `misc_data`) as plain string/scalar types without implementing `Redactable`, the logger cannot infer semantic sensitivity.
* **Why It Exists**: `slog` operates on key-value pairs without deep natural language processing or arbitrary secret sniffing in hot logging paths, which would destroy throughput.
* **Impact**: Plain primitive secrets passed under non-sensitive keys bypass automated masking unless the type implements `Redactable` or the key is added to `Config.RedactedKeys`.
* **How It Was Detected**: Security contract analysis of `internal/logger`.
* **Current Mitigation**: Strict sensitive-key precedence over `Redactable` prevents bypass when keys are classified as sensitive; domain structs carrying sensitive data implement `Redactable` to scrub themselves regardless of key name; custom keys can be configured via `Config.RedactedKeys`.
* **Future Solution**: Provide opt-in regex-based value scrubber for diagnostic environments where strict audit compliance is required.
* **Dimensional Impact**:
  * Correctness: **None** (Explicit contracts are enforced).
  * Performance: **Optimal** ($O(1)$ keyword and stem matching).
  * Scalability: **None**.

---

### 11. Single-Segment Recovery Assumes Quiescent Segment Access
* **Limitation**: `RecoverSegment` assumes the target WAL segment file is quiescent and not being concurrently appended to by an active `WALWriter`.
* **Why It Exists**: `P02-S03-M01` implements single-segment torn-tail detection and in-place truncation. Concurrent writes during recovery could cause race conditions where in-flight writes are misidentified as torn tails or truncated during append.
* **Impact**: Recovery must be executed during startup prior to writer initialization, or against an inactive/sealed segment.
* **How It Was Detected**: Recovery concurrency and lifecycle modeling.
* **Current Mitigation**: Documented architectural invariant; engine startup sequence executes segment recovery before active writers are spawned.
* **Future Solution**: Sub-Phase 02.3 M02/M03 segment rotation and engine startup coordinator manage writer lifecycle and ensure segment quiescence before recovery execution.
* **Dimensional Impact**:
  * Correctness: **High if violated** (Caller must guarantee segment quiescence during recovery).
  * Performance: **Optimal** (No locking overhead during offline recovery).
  * Scalability: **None**.

### 12. Concrete MemTable In-Memory Replay Target Deferred to Phase 03
* **Limitation**: `RecoverWAL` orchestrates multi-segment discovery, historical validation, latest-segment torn-tail repair, sequence monotonicity enforcement, and streaming replay into a `ReplaySink` interface. It does not provide the concrete in-memory SkipList or MemTable storage engine.
* **Why It Exists**: In accordance with the micro-phase hierarchy, Phase 02 establishes durable logging and recovery orchestration, while in-memory concurrent indexing (`MemTable`) is implemented in Phase 03.
* **Impact**: Callers can fully recover, validate, repair, and iterate historical WAL segments using `RecoverWAL` with any custom `ReplaySink` callback, but applying records directly to an active database MemTable requires the Phase 03 implementation.
* **How It Was Detected**: Subsystem boundary separation.
* **Current Mitigation**: Minimal `ReplaySink` interface (`Apply(Record) error`) decouples log recovery orchestration from in-memory index mechanics, allowing full verification without MemTable dependencies.
* **Future Solution**: Phase 03 implements the concurrent SkipList MemTable, which implements or integrates with `ReplaySink` during engine startup boot.
* **Dimensional Impact**:
  * Correctness: **None** (Recovery coordinator invariants and streaming replay are fully verified).
  * Performance: **Optimal** (Streaming verification avoids intermediate memory accumulation).
  * Scalability: **None**.

---

### 13. Group Commit Queue Boundary Established Without Coalescing Runner
* **Limitation**: `WriteTask` and `WriteQueue` define the task representation, bounded capacity, FIFO ordering, defensive payload ownership, and completion semantics for group commit, but do not yet include the active group-commit leader loop, cooperative batch coalescing, timer-based batch formation, or background execution worker.
* **Why It Exists**: `P02-S04-M01` defines the core data structures and queue boundaries in isolation to establish deterministic task lifecycle and durability completion contracts before introducing the multi-threaded coalescing pipeline in `P02-S04-M02`.
* **Impact**: Callers can enqueue, dequeue, and complete write tasks using `WriteQueue`, but automatic coalescing of multiple tasks into a single batch and single `fdatasync` barrier is deferred to the batch runner implementation in `P02-S04-M02`.
* **How It Was Detected**: Architectural phase separation.
* **Current Mitigation**: Fully verified bounded queue with condition variable backpressure, graceful drain on `Close()`, and immediate error propagation via `CloseWithError(err)` guarantees zero hung waiters.
* **Future Solution**: `P02-S04-M02` implements `groupCommitRunner`, cooperative batch formation, and single fsync synchronization barrier across grouped tasks.
* **Dimensional Impact**:
  * Correctness: **None** (Durability and queue lifecycle invariants strictly verified).
  * Performance: **Expected** (Single-fsync coalescing throughput improvements deferred to M02).
  * Scalability: **None**.

---

*End of Known Limitations — To be updated continuously throughout implementation.*


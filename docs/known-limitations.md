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

### 13. Group Commit Batch Runner Implemented Without Linger Timeout Coalescing
* **Limitation**: `GroupCommitRunner` executes on-demand FIFO queue draining up to 1,024 tasks or 64 KiB, but does not yet implement a configurable timer linger delay (`linger_ms`) or dynamic adaptive batch sizing based on observed queue backlog.
* **Why It Exists**: `P02-S04-M02` implements the core batch runner, dual batch limits, single-`fdatasync` synchronization barrier, error fan-out, and graceful lifecycle management. Introducing speculative timer delays or dynamic heuristics before the baseline coalescing pipeline was verified would add non-deterministic timing jitter and latency overhead to low-concurrency workloads.
* **Impact**: Under purely serial, single-threaded write workloads with zero queue backlog, each task is dequeued immediately and forms a singleton batch with its own `Sync()` barrier. Throughput scales naturally under concurrent load when pending writes accumulate during disk sync.
* **How It Was Detected**: Architectural design and performance profiling of cooperative group commit pipelines.
* **Current Mitigation**: `TryDequeueBatch` drains all currently enqueued tasks in a single lock acquisition. When the runner is blocked in `Sync()`, concurrent producers accumulate in the queue, automatically forming dense batches for the subsequent iteration without artificial timer sleeps.
* **Future Solution**: Introduce optional microsecond-level linger timeout (`LingerTimeout`) and adaptive batch sizing during single-node engine integration (Phase 10).
* **Dimensional Impact**:
  * Correctness: **None** (Durability and serializability strictly preserved).
  * Performance: **Optimal under concurrency**; serial single-threaded latency bounded by raw hardware fsync speed.
  * Scalability: **Optimal** (Amortizes up to 1,024 tasks per sync).

---

### 14. Static Security Audit, Storage Dynamic Audit (SEC-03), and In-Memory Engine Audit (SEC-04) Verified
* **Limitation**: Static analysis (`SEC-01`, `SEC-02`), persistence/storage dynamic auditing (`SEC-03`), and in-memory multi-version engine auditing (`SEC-04`) are fully complete and verified. However, dynamic network protocol fuzzing, distributed consensus fault injection, and multi-node Byzantine testing remain deferred until their respective subsystems are implemented.
* **Why It Exists**: In accordance with the security roadmap hierarchy, storage persistence (`SEC-03`) and in-memory core ordering/bounds primitives (`SEC-04`) are audited against implemented code. Network transport (Phase 11) and Raft consensus (Phase 13) do not yet exist in the codebase.
* **Impact**: Storage, WAL, filesystem races, torn writes, malformed framing, permission boundaries, and in-memory InternalKey multi-version ordering/bounds are rigorously verified under adversarial tests. Network and cluster-level dynamic testing will activate when those subsystems are built.
* **How It Was Detected**: Security roadmap staging and architecture boundaries.
* **Current Mitigation**: Comprehensive dynamic test suites in `internal/wal` (`sec03_*_test.go`), `internal/binary` (`sec04_*_test.go`), native Go fuzzing, fault injection seams, and race detection.
* **Future Solution**: Execute `SEC-05` (network protocol fuzzing) and `SEC-06` (distributed consensus chaos testing) once networking and Raft are implemented.
* **Dimensional Impact**:
  * Correctness: **None** (Existing subsystems verified).
  * Performance: **None**.
  * Scalability: **None**.

---

### 15. Directory Metadata Fsync and Cross-Platform ACL Handling
* **Limitation**: Individual WAL segment files are strictly synchronized via `fdatasync()` upon append and rotation, but the containing parent directory is not explicitly directory-fsynced on Unix filesystems. Furthermore, on Windows, POSIX permissions (`0700` / `0600`) map to standard file attributes rather than fine-grained Windows NT Access Control Lists (ACLs).
* **Why It Exists**: In modern journaling filesystems (ext4, XFS, APFS), file creation and append operations with `O_CREAT | O_APPEND` followed by `fdatasync` ensure file data and required inode size metadata reach disk. Explicit directory `fsync()` incurs substantial synchronous metadata lock contention across concurrent rotations. On Windows, the Go standard library maps file modes to read-only bits without native security descriptor manipulation.
* **Impact**: In the catastrophic event of a sudden host power cut at the exact microsecond of segment rotation on a non-journaled filesystem, directory entry pointers could lag behind allocated inodes. On multi-user Windows servers without restricted parent directories, local users might read segment files if parent directory inheritance is permissive.
* **How It Was Detected**: Identified as Hardening Opportunities `SEC-03-HARD-02` and `SEC-03-HARD-03` during the SEC-03 dynamic security audit.
* **Current Mitigation**: Segment creation uses `os.O_EXCL | os.O_CREATE`, verifying inode identity via `os.SameFile` and hardening permissions via `fchmod` on the open descriptor. Startup recovery discovers segments by numeric filename sorting and fails closed upon sequence gaps.
* **Future Solution**: Introduce optional directory fsync configuration for enterprise deployments and native Windows security descriptor inheritance in Phase 10.
* **Dimensional Impact**:
  * Correctness: **None** (Recovery fails closed on any discrepancy).
  * Performance: **Optimal** (Avoids synchronous directory lock stalls).
  * Scalability: **None**.

---

### 16. In-Memory InternalKey Direct Initialization vs Boundary Copying (SEC-04-HARD-01)
* **Limitation**: `InternalKey` struct fields (`UserKey []byte`) are exported to allow zero-allocation slice borrowing inside internal engine hot loops (such as SSTable block decoders and memtable traversals). Direct instantiation (`InternalKey{UserKey: slice}`) borrows the caller's slice without copying.
* **Why It Exists**: High-performance LSM engines require zero heap allocations along read and compaction hot paths. Forcing defensive copies at every internal struct initialization would multiply garbage collection overhead by $O(N)$ across billions of record comparisons.
* **Impact**: If an external or untrusted caller constructs an `InternalKey` via direct struct initialization rather than the canonical constructor `binary.NewInternalKey()`, later mutations to the caller slice will mutate the `InternalKey`'s referenced key.
* **How It Was Detected**: Identified as Hardening Opportunity `SEC-04-HARD-01` during the SEC-04 in-memory engine security audit.
* **Current Mitigation**: `binary.NewInternalKey()` and `binary.DecodeInternalKey()` strictly perform defensive copies (`copy(make([]byte, len(k)), k)`). `ik.Clone()` provides deep copy isolation. Storage engine entry points enforce `NewInternalKey()`.
* **Future Solution**: When implementing the Phase 03 MemTable, enforce that `MemTable.Put()` always performs a defensive copy or allocates into an append-only arena before linking nodes into the SkipList.
* **Dimensional Impact**:
  * Correctness: **None** when public constructors are utilized.
  * Performance: **Optimal** (Enables zero-allocation internal engine comparisons).
  * Scalability: **None**.

---

### 17. Single Serialized Writer Model & Verified Lock-Free Reader Architecture
* **Limitation**: In `P03-S02-M01` and `P03-S02-M02`, lock-free reader traversal (`SearchConcurrent`), exact byte-level memory tracking (`ByteSize()`), and bottom-up atomic publication are fully implemented and verified. However, write mutations remain strictly serialized under an exclusive mutex (`s.mu.Lock()`). Concurrent multi-writer lock-free insertions are intentionally not implemented.
* **Why It Exists**: In modern LSM storage engines (e.g. LevelDB, RocksDB, Pebble), multiple concurrent writers incur extreme CAS retry overhead and complex split/splice lock-free protocols that provide minimal throughput benefit over serialized append-oriented MemTables fed by a WAL Group Commit pipeline. Serializing structural SkipList mutations under a mutex while keeping readers 100% lock-free represents the optimal production engineering balance.
* **Impact**: Total ingestion rate into a single active MemTable is bounded by single-core pointer splicing throughput (~2-5 million writes/sec in memory). Multiple concurrent readers scale linearly across CPU cores without mutex contention.
* **How It Was Detected**: Architectural design established in Phase 03 Roadmap and ADR-003.
* **Current Mitigation**: Serialized writer mutex guarantees structural failure atomicity and zero CAS retry storms; lock-free readers execute without acquiring locks.
* **Future Solution**: MemTable freezing and multi-version forward iterators will be implemented in Sub-Phase 03.3.
* **Dimensional Impact**:
  * Correctness: **None** (Linearizability and race freedom strictly preserved).
  * Performance: **Optimal** (Readers never stall on writers; writers avoid CAS loops).
  * Scalability: **High** read scalability; write scalability governed by WAL group commit.

---

### 18. Lattice-Owned Object Accounting Boundary vs Go Runtime Allocator Metadata and Process RSS
* **Limitation**: `ByteSize()` deterministically tracks heap memory directly attributable to Lattice-owned objects (`skipListNode` struct headers, cloned `UserKey` backing arrays, variable-height forward pointer towers, `nodeValue` containers, and value backing arrays). It deliberately does not track Go runtime allocator internal metadata (`mheap`, `mcentral`, `mspan`), size-class rounding slack (e.g. a 72-byte struct allocated from Go's 80-byte size class), GC write barrier state, goroutine stacks, runtime heap fragmentation, or operating system Resident Set Size (RSS).
* **Why It Exists**: `ByteSize()` must be deterministic, reproducible, arithmetic-safe, and independent of Go runtime GC cycles, allocator cache flushes, or operating system page management. Using `runtime.MemStats.HeapAlloc` would make byte accounting non-deterministic, noisy across unrelated goroutines, and unsuited for predictable MemTable flush threshold decisions.
* **Impact**: Total physical process resident memory (`RSS`) reported by OS tools (e.g., `ps`, `top`) will exceed `ByteSize()` due to runtime overhead, garbage collector retention curves, and OS page alignment.
* **How It Was Detected**: P03-S02-M02 memory accounting architectural specification.
* **Current Mitigation**: The accounting model explicitly derives bounds from `unsafe.Sizeof` and allocated slice capacities, ensuring exact equality against independent mathematical models and predictable flushing triggers.
* **Future Solution**: Phase 10 engine metrics will export both logical `ByteSize()` and physical OS RSS (`runtime.MemStats.Sys`, `HeapInuse`) for operational telemetry.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic, testable, and failure-atomic).
  * Performance: **Optimal** ($O(1)$ atomic counter load with zero allocations).
  * Scalability: **Optimal**.

### 19. Weakly-Consistent Live SkipList Iterator Traversal vs Snapshot Isolation
* **Limitation**: The SkipList forward iterator (`Iterator`, introduced in `P03-S03-M01`) operates directly over the live, mutable SkipList via lock-free atomic pointer reads along Level 0. It is a weakly-consistent live iterator, not a point-in-time snapshot iterator.
* **Why It Exists**: SkipList Level-0 links are updated concurrently by serialized writers. Providing true multi-version snapshot isolation at the iterator level requires either freezing the MemTable (`P03-S03-M02`) or maintaining an active snapshot sequence number barrier with MVCC record visibility filtering. Before MemTable freeze transitions exist, the physical SkipList iterator reflects live mutations.
* **Impact**: An iterator traversing while concurrent writers append nodes will observe newly inserted nodes if they fall ahead of the iterator's current position (`UserKey >= current.UserKey`). Nodes inserted behind the iterator's current position will not be observed. Historical revisions and tombstones (`OpTypeDelete`) are physically yielded in canonical internal order (`UserKey ASC`, `SeqNum DESC`, `OpType DESC`) rather than logically filtered.
* **How It Was Detected**: Design specification and semantic definition in `P03-S03-M01`.
* **Current Mitigation**: The iterator contract is explicitly documented as live and weakly consistent. Traversal is 100% race-free under `go test -race` due to atomic pointer loads. Defensive copying (`Key()` and `Value()`) guarantees callers cannot corrupt internal memory.
* **Future Solution**: In `P03-S03-M02`, `MemTable.Freeze()` will transition active MemTables to read-only immutable tables, providing static snapshot guarantees. In Phase 06/10, engine-level snapshot iterators will bind a read sequence number to mask uncommitted or future versions.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Guarantees monotonicity, acyclicity, and memory isolation without false claims of snapshot isolation).
  * Performance: **Optimal** (Zero locks acquired during iteration; sub-3ns Next step).
  * Scalability: **Optimal** (Arbitrary concurrent iterators scale linearly across CPU cores).

### 20. Structural In-Place Immutability vs Full MVCC Point-In-Time Snapshot Isolation
* **Limitation**: `SkipList.Freeze()` (introduced in `P03-S03-M02`) provides permanent *structural immutability* for the in-memory MemTable, but does not provide logical MVCC snapshot isolation (such as point-in-time sequence-number visibility filtering or tombstone masking).
* **Why It Exists**: In an LSM-tree architecture, the frozen MemTable is the exact physical unit passed to background SSTable flusher workers and compaction mergers. Flusher workers strictly require physical visibility of all sequence revisions and tombstones (`OpTypeDelete`) so deletions and overwrites are persisted to L0 SSTables. Logical filtering at the MemTable tier would break LSM compaction dynamics.
* **Impact**: Iterating over a frozen MemTable (`it := frozenSL.NewIterator()`) yields a static, immutable view of all physical records stored up to the `Freeze()` linearization point. However, multiple revisions of the same UserKey and raw tombstones remain visible until upper-layer engine snapshot iterators (Phase 10) apply MVCC sequence masking.
* **How It Was Detected**: Architectural specification in `P03-S03-M02`.
* **Current Mitigation**: The separation of concerns is explicitly maintained: `Freeze()` guarantees structural and value immutability (zero post-freeze mutations, failure-atomic rejection returning `ErrMemTableFrozen`).
* **Future Solution**: Phase 06/10 will implement `DB.NewSnapshot()` and `SnapshotIterator`, which bind a read sequence number $S_{\text{read}}$ to mask newer revisions and tombstones.
* **Dimensional Impact**:
  * Correctness: **Optimal** (LSM flusher contracts strictly preserved).
  * Performance: **Optimal** ($O(1)$ in-place transition with zero data copying).
  * Scalability: **Optimal**.

---

### 21. Data Block Prefix Compression Sizing Sensitivity to Key Redundancy and Random Seeks
* **Limitation**: Prefix compression inside `BlockBuilder` (P04-S01-M01) achieves high compression ratios ($>1.8\times$) only when consecutive keys share common byte prefixes (e.g. structured keys such as `tenant:100:user:001`, `tenant:100:user:002` or multi-version keys with descending sequence numbers). For high-entropy random keys (e.g. raw UUIDs, SHA-256 hashes), common prefix length drops to 0, resulting in zero compression and small varint length framing overhead (1–2 bytes per record). Furthermore, prefix compression couples records between restart points: point lookup within a block cannot jump directly to an arbitrary entry without linear delta reconstruction from the preceding restart point.
* **Why It Exists**: LSM-tree data blocks optimize for sequential streaming and high data density. Resetting prefixes every $k=16$ entries establishes restart points that limit point-lookup delta scanning to at most 15 records while retaining high space reduction.
* **Impact**: Blocks containing random keys will not compress via prefix delta encoding. Point lookup latency within a 4KB block includes scanning up to 15 key suffixes from the nearest restart point.
* **How It Was Detected**: P04-S01-M01 prefix compression modeling and empirical benchmarks.
* **Current Mitigation**: The restart interval defaults to 16, striking a balanced trade-off between compression ratio and internal block binary search cost.
* **Future Solution**: Phase 04.2 will add two-level sparse indexing, and Phase 05 will add Bloom filters to bypass non-matching SSTable blocks entirely, minimizing random seek penalties.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Binary-safe, byte-for-byte exact reconstruction).
  * Performance: **High** sequential compression; $O(k)$ scan bounded by restart interval.
  * Scalability: **Optimal**.

---

### 22. SSTable Data Block Checksum Scope and Reader Decoupling (P04-S01-M02)
* **Limitation**: In `BlockBuilder` (P04-S01-M02), the serialized block trailer includes a 32-bit CRC32-IEEE checksum covering `[Entry Data || Restart Offsets || Restart Count]`. While CRC32-IEEE reliably detects random hardware bit flips, network corruption, torn writes, and storage decay, it is **not a cryptographic digest or message authentication code (MAC)**. It does not provide cryptographic integrity or protection against malicious adversarial tampering where an attacker with write access can recalculate the CRC. Furthermore, M02 implements block-level construction and trailer serialization; block seeking, binary searching restart points, and block decoding remain decoupled and deferred to the reader implementation in P04-S03-M02.
* **Why It Exists**: Storage engines prioritize high-throughput serialization with zero-allocation CRC calculation in NVMe data paths over heavy cryptographic hashing (e.g. SHA-256). In-memory block reading and sparse index lookup are separate architectural responsibilities cleanly isolated from writer serialization.
* **Impact**: Blocks can detect corruption and torn writes via CRC mismatch but cannot authenticate against active adversaries with disk write access. Point lookup within written blocks requires the reader subsystem (P04-S03-M02).
* **How It Was Detected**: Architectural analysis of P04-S01-M02 block serialization and security review.
* **Current Mitigation**: Strict verification of Big-Endian uint32 layout, restart offset monotonicity, capacity overflow guards, and independent CRC validation.
* **Future Solution**: P04-S03-M02 will implement `TableReader` with binary search across restart points. At the system boundary, optional encryption/signing layers (e.g. TLS on wire, LUKS/dm-crypt on disk) provide cryptographic protection if required.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Self-contained, deterministically verifiable trailer).
  * Performance: **Optimal** (Zero-allocation CRC32-IEEE computation).
  * Scalability: **Optimal**.

---

### 23. Sparse Block Index RAM Residency & SSTable Reader Decoupling (P04-S02-M01)
* **Limitation**: `IndexBuilder` and `BlockIndex` (P04-S02-M01) implement construction, binary serialization, CRC validation, and in-memory binary search across 4KB data blocks. In this micro-phase, the sparse index is held in contiguous memory in RAM once decoded. While sparse indexing reduces the index size by >97.5% compared to dense indexing (holding ~1 entry per 4KB data block), extremely large SSTables (e.g. hundreds of gigabytes per file) would still require several megabytes of RAM per SSTable if the index block is held permanently memory-resident without eviction. Furthermore, M01 is decoupled from file-level I/O; table-level reading (`TableReader`), footer parsing, and reading data blocks from disk are deferred to P04-S02-M02 and P04-S03-M02.
* **Why It Exists**: Following the micro-phase engineering discipline, index building and decoding are cleanly isolated from the 48-byte footer (P04-S02-M02) and file-level disk streaming/lookup (P04-S03-M01/M02).
* **Impact**: Full SSTable point lookups from disk require subsequent micro-phases (P04-S02-M02 footer and P04-S03-M02 reader).
* **How It Was Detected**: Architectural design and memory profiling of P04-S02-M01 sparse index structures.
* **Current Mitigation**: Strict `BlockHandle` validation against physical file size (`ValidateAgainstFileSize`) to prevent out-of-bounds reads, compact 16-byte fixed handles, and 32-bit offset indexing.
* **Future Solution**: P04-S02-M02 will implement the 48-byte footer pointing to the index block; P04-S03-M02 will integrate the reader; and Phase 10 will add evictable index block caching in the LRU block cache if needed for memory-constrained environments.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic, verified under race detector and fuzzing).
  * Performance: **Optimal** (~256 ns binary search across 1,000 blocks in RAM).
  * Scalability: **Optimal** for standard LSM SSTables (up to 64MB-2GB per SSTable).

### 24. Fixed SSTable Footer and TableReader Decoupling (P04-S02-M02)
* **Limitation**: `Footer` (P04-S02-M02) implements the fixed 48-byte binary serialization, deserialization, zero-padding verification, and magic number validation anchoring the SSTable. In this micro-phase, the footer codec operates in-memory on byte slices and fixed byte arrays. End-to-end disk persistence (appending the footer to an actual `.sst` file on disk) and file-level bootstrapping (reading `file_size - 48` from an OS file descriptor to initialize `TableReader`) are deferred to Sub-Phase 04.3 (`P04-S03-M01` for `TableWriter` and `P04-S03-M02` for `TableReader`). Furthermore, while `ValidateAgainstFileSize(fileSize)` guarantees handles do not extend beyond `fileSize - 48`, validating the footer alone does not guarantee the integrity of the data blocks or index blocks pointed to by the handles until those blocks are independently fetched and their CRC32 checksum trailers are verified.
* **Why It Exists**: Following the micro-phase engineering discipline, binary structure codecs are cleanly isolated from file system I/O, streaming abstractions, and full table lifecycle management.
* **Impact**: End-to-end SSTable reading and point lookups from disk require subsequent micro-phases (P04-S03-M01 writer and P04-S03-M02 reader).
* **How It Was Detected**: Architectural analysis of P04-S02-M02 footer codec and integration boundaries.
* **Current Mitigation**: Comprehensive unit tests, independent binary oracle fixtures, fuzz testing (>5.9M iterations with 0 crashes), zero heap allocation execution, and explicit file boundary validation (`ValidateAgainstFileSize`).
* **Future Solution**: Sub-Phase 04.3 (`P04-S03-M01` TableWriter and `P04-S03-M02` TableReader) will integrate the footer into end-to-end file creation and querying workflows.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Strict 48-byte framing, byte-for-byte exact independent oracle match, 0-padding invariant).
  * Performance: **Optimal** (Zero heap allocations, ~3.3 ns encode, ~3.7 ns decode).
  * Scalability: **Optimal** (Handles address up to $2^{64}-1$ bytes).

### 25. SSTable TableWriter Persistence and TableReader Decoupling (P04-S03-M01)
* **Limitation**: `TableWriter` (P04-S03-M01) implements full persistent file assembly, streaming prefix-compressed data blocks, writing the 8-byte meta-index block, serializing the sparse index block, appending the 48-byte footer, and guaranteeing crash-safe durability via `.tmp` staging files, `file.Sync()`, atomic rename, and directory syncing. However, reading, decoding, seeking within blocks, and querying SSTable files from disk (`TableReader.Seek`) remain decoupled and deferred to `P04-S03-M02`. Additionally, Bloom filter generation is deferred to Phase 05; in Phase 04, `TableWriter` emits a canonical 8-byte empty MetaIndex block (`0` count + CRC32-IEEE) satisfying footer non-zero handle constraints while reserving the structure for filter integration.
* **Why It Exists**: Following the micro-phase engineering discipline, persistence assembly on the write path is cleanly isolated from random-access block seeking and query execution on the read path.
* **Impact**: End-to-end point lookups (`Seek`) and reading data blocks directly from disk require the subsequent reader micro-phase (P04-S03-M02).
* **How It Was Detected**: Architectural design and micro-phase boundaries of Phase 04 Sub-Phase 04.3.
* **Current Mitigation**: Comprehensive forensic tests validating every byte offset and block CRC directly from disk, atomic staging and cleanup, and fuzz testing (1,474 full file cycles).
* **Future Solution**: Sub-Phase 04.3 (`P04-S03-M02`) will implement `TableReader` with sparse index binary search, and Phase 05 will implement Bloom filter generation.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Atomic staging, directory sync, pre-existing file protection).
  * Performance: **Optimal** (Over 1,000,000 keys/sec persistent write throughput; ~143.7 ns in-memory ingestion).
  * Scalability: **Optimal** (Streaming append pipeline scales to multi-gigabyte SSTables).

### 26. SSTable Point Reads Lack Bloom Filter Acceleration and Block Cache Buffering (P04-S03-M02)
* **Limitation**: In `TableReader` (P04-S03-M02), point lookup (`Seek`) operates directly on a single finalized SSTable file on disk. In this phase:
  1. *No Bloom Filter Acceleration*: Reads do not yet evaluate a Bloom filter before issuing disk reads. If a requested key is absent but falls within the key span of a data block ($S_i \le K \le L_i$), `TableReader` must read the candidate data block from disk to confirm absence. Bloom filter generation and filtering are deferred to Phase 05.
  2. *No In-Memory Block Cache*: Repeated `Seek()` operations against keys in the same data block read the block from disk via `ReadAt` on each call rather than reusing a cached uncompressed block from an LRU block cache. Block cache integration is deferred to Phase 10.
  3. *Single-Table Scope*: `TableReader` queries exactly one `.sst` file; multi-level searching, version set snapshots, manifest tracking, and LSM compaction merges are deferred to Phases 06 and 07.
  4. *Point Lookup Only*: `TableReader` implements exact point lookup via `Seek()`; bidirectional range iteration (`Iterator`) across blocks and tables is deferred to future engine phases.
* **Why It Exists**: Following the micro-phase engineering discipline, the persistent reader, sparse index search, and prefix-compressed data block decoder are cleanly implemented and validated in isolation before composing with probabilistic filters, caching layers, and multi-SSTable version sets.
* **Impact**: Cold absent reads falling within block key ranges require 1 disk seek. Repeated reads incur disk I/O latency unless cached by OS page cache.
* **How It Was Detected**: Architectural design and micro-phase boundaries of Phase 04 Sub-Phase 04.3.
* **Current Mitigation**: The in-memory sparse `BlockIndex` immediately rejects any target key greater than all largest keys with zero data block disk I/O (~103.9 ns/op). Restart point binary search bounds forward scans to at most 16 records.
* **Future Solution**: Phase 05 will implement Bloom filters eliminating $>99\%$ of cold read disk I/Os; Phase 06 will integrate multi-SSTable `VersionSet` lookups; and Phase 10 will add an LRU block cache.
* **Dimensional Impact**:
  * Correctness: **Optimal** (100% verified point lookup accuracy across 100,000 keys; strict multi-version and tombstone resolution; corruption protection).
  * Performance: **High** (~877.4 ns hot block seek, ~1,218 ns random seek, ~103.9 ns missing key rejection).
  * Scalability: **Optimal** (Stateless `ReadAt` scales concurrently across multiple goroutines).

### 27. Phase 04 Security Hardening and Deliberate Architectural Boundaries
* **Limitation**: Following the Phase 04 security remediation audit:
  1. *Filesystem Security Baseline*: SSTable files are created with `0600` permissions and directories with `0700`, aligning with the WAL durability and confidentiality model.
  2. *Secure Staging Lifecycle*: Staging files use randomized temporary paths created via `os.CreateTemp` with `O_CREATE|O_EXCL` semantics; symlinks are rejected and atomic publication fails closed with `ErrSSTableExists` if the destination path appears concurrently.
  3. *Sparse Index Key Separation*: `BlockIndex.FindBlock` and `IndexBuilder.FindBlock` explicitly compare bare `targetUserKey` slices against `entry.UserKey()`, eliminating heuristic type guessing and preventing false `ErrKeyNotFound` errors for binary keys.
  4. *Representation Boundary Decoupling*: `MaxUserKeyLen` is bounded at 65,535 bytes, while `MaxEncodedInternalKeyLen` is bounded at 65,544 bytes ($65,535 + 9$).
  5. *Default Metadata Redaction*: `InternalKey.String()` is safe by default and returns redacted metadata. Explicit `DebugString()` is available for forensic inspection.
  6. *In-Scope Boundary Reminder*: Consistent with the project charter, Phase 04 provides single-table immutability, point lookups, and crash-safe storage. Bloom filters (Phase 05), multi-table VersionSet reads (Phase 06), compaction (Phase 07), LRU block cache (Phase 10), network transport (Phase 11), and distributed consensus (Phase 12) remain strictly deferred.
* **Why It Exists**: Security hardening must reinforce existing invariants without artificially expanding the architectural scope beyond the completed Phase 04 boundary.
* **Impact**: Zero security regressions; full binary key fidelity up to 64KB.
* **How It Was Detected**: Comprehensive adversarial security audit through Phase 04 and regression verification.
* **Current Mitigation**: Strict regression test suite in `sec04_remediation_test.go`, verified clean under `-race`, `go vet`, and `golangci-lint`.
* **Future Solution**: Future phases will inherit these hardened filesystem, serialization, and comparison primitives.
* **Dimensional Impact**:
  * Correctness: **Optimal** (100% test pass, zero type confusion, crash-safe publication).
  * Performance: **Optimal** (Zero-overhead type-separated comparisons).
  * Scalability: **Optimal**.

---

### 28. Bloom Filter Parameter Calculator & Bitset Allocator Decoupling (P05-S01-M01)
* **Limitation**: `BloomFilter` (P05-S01-M01) implemented foundational mathematical parameter sizing and zero-initialized physical bitset allocation for $m = n \times 10$ bits and $k = 7$ hash functions, but membership operations (`Add`, `MayContain`) were decoupled.
* **Resolution**: Resolved in P05-S01-M02 with the implementation of canonical `Murmur3_128` hashing, Kirsch–Mitzenmacher double hashing, `Add`, and `MayContain` with zero false negatives verified across 10,000 keys.

---

### 29. Bloom Filter Membership Serialization & SSTable Integration Decoupling (P05-S01-M02)
* **Limitation**: `BloomFilter` (P05-S01-M02) implements in-memory Murmur3 128-bit double-hashing, Kirsch–Mitzenmacher probe calculation, `Add`, and `MayContain` with zero false negatives. However, filter block binary serialization (`FilterBlockBuilder`), persistent storage format, SSTable reader/writer integration, and formal empirical false-positive rate benchmarking over 1,000,000 keys are decoupled and deferred to Sub-Phase 05.2 (P05-S02-M01 and P05-S02-M02). Additionally, `Bitset()` returns a direct slice to the underlying buffer; external modification of this slice will corrupt filter invariants without synchronization (intended concurrency model: single-writer population, concurrent immutable querying).
* **Why It Exists**: Following micro-phase engineering discipline, low-level hashing and probabilistic membership invariants are thoroughly tested and fuzzed independently from block framing and on-disk SSTable file layouts.
* **Impact**: In-memory Bloom filters cannot yet be persisted into SSTable files or read by `TableReader`.
* **How It Was Detected**: Architectural design and micro-phase scoping of Phase 05.
* **Current Mitigation**: Comprehensive in-memory tests, SMHasher verification test (`0x6384BA69`), property-based fuzz tests (>3.0M executions with 0 failures), bounded sanity check, zero-allocation enforcement (0 B/op on `Add` and `MayContain`).
* **Future Solution**: Sub-Phase 05.2 will implement `FilterBlockBuilder` serialization (M01) and empirical 1M key false-positive rate benchmark (M02). Phase 06+ will integrate filter blocks into `TableWriter` and `TableReader`.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Zero false negatives on inserted keys, SMHasher test passed, overflow-safe modular probing).
  * Performance: **Optimal** (~14.3 ns/op `Add`, ~7.4 ns/op `MayContain` miss, ~15.1 ns/op hit, 0 allocs/op).
  * Scalability: **Optimal** (Supports up to ~209.7 million keys per filter block).

---

*End of Known Limitations — To be updated continuously throughout implementation.*


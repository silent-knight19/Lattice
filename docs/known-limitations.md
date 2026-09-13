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
* **Current Mitigation**: Lattice's encoder (`PutVarint64`) strictly emits minimal canonical forms; decoder strictly enforces the 10-byte bound and rejects integer overflow (`b > 1` on 10th byte). IND-H-005 proof in `sec_ind_h005_test.go` verifies the audit's exact 1000×`0xFF` flood terminates promptly with `ErrVarintOverflow` (wall-clock guarded), plus continuation-flood lengths 1–64 and 32-bit floods. IND-L-001 proof in `sec_ind_l001_test.go` pins the approved dual policy: lenient decode accepts overlong forms with correct values while `GetVarint64Canonical` rejects them, with round-trip agreement on canonical inputs. PDF IND-008 proof in `sec_pdf_ind008_test.go` verifies 32-bit codec symmetry (`decode(encode(x)) == x`) across all int32/uint32 boundaries with canonical agreement, plus truncation fail-closed.
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
* **Current Mitigation**: `TryDequeueBatch` drains all currently enqueued tasks in a single lock acquisition. When the runner is blocked in `Sync()`, concurrent producers accumulate in the queue, automatically forming dense batches for the subsequent iteration without artificial timer sleeps. IND-C-003 proof in `sec_ind_c003_test.go` verifies sync-barrier failure fans out as `*BatchSyncError` to every waiter (zero false durable acks) and poisons the writer fail-closed.
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
* **Current Mitigation**: Comprehensive dynamic test suites in `internal/wal` (`sec03_*_test.go`), `internal/binary` (`sec04_*_test.go`), native Go fuzzing, fault injection seams, and race detection. IND-M-004 proof in `sec_ind_m004_test.go` verifies truncated headers/bodies fail closed (`ErrHeaderTruncated`/`ErrUnexpectedEOF`) while 1-byte-fragmented valid records still decode exactly.
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
* **Current Mitigation**: Strict `BlockHandle` validation against physical file size (`ValidateAgainstFileSize`) to prevent out-of-bounds reads, compact 16-byte fixed handles, and 32-bit offset indexing. IND-H-001 proof in `sec_ind_h001_test.go` verifies the audit's exact wrapping scenario (offset=MaxUint64-10, size=20) is rejected with `ErrInvalidBlockHandle` by the overflow guard, plus the exact-fit boundary matrix.
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
* **Current Mitigation**: Comprehensive unit tests, independent binary oracle fixtures, fuzz testing (>5.9M iterations with 0 crashes), zero heap allocation execution, and explicit file boundary validation (`ValidateAgainstFileSize`). IND-H-007 proof in `sec_ind_h007_test.go` verifies 0/1/15/16/47-byte files are rejected with `ErrInvalidFooterSize` (panic-guarded) and bad-magic 48-byte files fail closed.
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
* **Current Mitigation**: Comprehensive forensic tests validating every byte offset and block CRC directly from disk, atomic staging and cleanup, and fuzz testing (1,474 full file cycles). IND-H-002 proof in `sec_ind_h002_test.go` verifies single-file `link(2)+unlink` publication leaves zero `.tmp_*` orphans on success, on injected link failure, and when a destination appears mid-flush (fail-closed `ErrSSTableExists`, victim bytes preserved).
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
* **Current Mitigation**: The in-memory sparse `BlockIndex` immediately rejects any target key greater than all largest keys with zero data block disk I/O (~103.9 ns/op). Restart point binary search bounds forward scans to at most 16 records. IND-M-003 proof in `sec_ind_m003_test.go` corrupts a real flushed data block (zero restart count, non-zero first offset with re-fixed CRC, stale-CRC torn write) proving each class fails closed with `ErrDataBlockCorrupted`/`ErrChecksumMismatch` under a panic guard.
* **Future Solution**: Phase 05 will implement Bloom filters eliminating $>99\%$ of cold read disk I/Os; Phase 06 will integrate multi-SSTable `VersionSet` lookups; and Phase 10 will add an LRU block cache.
* **Dimensional Impact**:
  * Correctness: **Optimal** (100% verified point lookup accuracy across 100,000 keys; strict multi-version and tombstone resolution; corruption protection).
  * Performance: **High** (~877.4 ns hot block seek, ~1,218 ns random seek, ~103.9 ns missing key rejection).
  * Scalability: **Optimal** (Stateless `ReadAt` scales concurrently across multiple goroutines).

### 27. Phase 04 Security Hardening and Deliberate Architectural Boundaries
* **Limitation**: Following the Phase 04 security remediation audit:
  1. *Filesystem Security Baseline*: SSTable files are created with `0600` permissions and directories with `0700`, aligning with the WAL durability and confidentiality model.
  2. *Secure Staging Lifecycle*: Staging files use randomized temporary paths created via `os.CreateTemp` with `O_CREATE|O_EXCL` semantics; symlinks are rejected and atomic publication fails closed with `ErrSSTableExists` if the destination path appears concurrently. IND-H-006 proof in `sec_ind_h006_test.go` verifies predictable-name squats (regular or symlink) are ignored and rapid symlink churn during 10 flushes causes no stall, redirect, or victim modification.
  3. *Sparse Index Key Separation*: `BlockIndex.FindBlock` and `IndexBuilder.FindBlock` explicitly compare bare `targetUserKey` slices against `entry.UserKey()`, eliminating heuristic type guessing and preventing false `ErrKeyNotFound` errors for binary keys.
  4. *Representation Boundary Decoupling*: `MaxUserKeyLen` is bounded at 65,535 bytes, while `MaxEncodedInternalKeyLen` is bounded at 65,544 bytes ($65,535 + 9$).
  5. *Default Metadata Redaction*: `InternalKey.String()` is safe by default and returns redacted metadata. Explicit `DebugString()` is available for forensic inspection.
  6. *In-Scope Boundary Reminder*: Consistent with the project charter, Phase 04 provides single-table immutability, point lookups, and crash-safe storage. Bloom filters (Phase 05), multi-table VersionSet reads (Phase 06), compaction (Phase 07), LRU block cache (Phase 10), network transport (Phase 11), and distributed consensus (Phase 12) remain strictly deferred.
  7. *Filter Block Allocation Caps (PDF IND-007)*: `DecodeFilterBlock` enforces `MaxBitsetBytes` (256 MiB) before any allocation, pins `k == 7`, verifies CRC first, and requires declared bit-count to match actual length. Proof in `sec_pdf_ind007_test.go`: the audit's exact 1-billion-bit bomb plus `MaxUint64` wraparounds are rejected promptly (wall-clock guarded), with truncation/CRC/count/length hardening matrix and a no-false-negative round-trip control.
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

* **Resolution**: Resolved in P05-S02-M01 with the implementation of `FilterBlockBuilder`, persistent Filter Block binary serialization (`[Bitset] [BitCount 8B] [k 1B] [CRC32 4B]`), SSTable MetaIndex block integration (`"filter.bloom" -> FilterHandle`), and reader extraction via `TableReader.ReadFilterBlock()`.

---

### 30. SSTable Point Lookup Bloom Filter Bypass & Empirical 1M FPR Validation Boundary (P05-S02-M01)
* **Limitation**: In `P05-S02-M01`, active point-lookup (`TableReader.Seek`) does not yet query the in-memory Bloom filter before reading data blocks, and formal empirical validation over 1,000,000 keys was deferred.
* **Resolution**: The empirical 1,000,000-key false positive verification benchmark was implemented and validated in P05-S02-M02 (8,186 / 1,000,000 FP = 0.8186%, 95% Wilson CI [0.8011%, 0.8365%], zero false negatives). Wiring the in-memory Bloom filter into `TableReader.Seek` point lookups remains intentionally decoupled for subsequent reader optimization phases.

---

### 31. Bloom Filter Empirical Benchmark Boundaries & Population Scope (P05-S02-M02)
* **Limitation**: In `P05-S02-M02`:
  1. *Deterministic Synthetic Key Distribution*: The empirical verification harness generates keys deterministically via stack-allocated strings (`"insert:%010d"` and `"absent:%010d"`). While MurmurHash3_x64_128 achieves uniform avalanche behavior across these inputs, empirical validation reflects this deterministic population rather than worst-case adversarial hash-collision attacks.
  2. *Point-Lookup Decoupling Preserved*: Strictly adhering to single micro-phase discipline, P05-S02-M02 measures the probabilistic correctness and statistical variation of the existing Bloom filter substrate without modifying `TableReader.Seek` point-lookup short-circuiting or SSTable file formats.
  3. *Fixed Policy Constraint*: The empirical validation measures the project's invariant policy ($m/n = 10, k = 7$). Dynamic tuning of bits per key or probe counts per SSTable is not supported by current design.
* **Why It Exists**: Verification phases must isolate measurement from architectural feature creep. Validating the statistical consistency of the Bernoulli model ($E[FP] \approx 8,193.7, \text{Observed}=8,186, Z=-0.0857$) on an isolated 1M key population proves the mathematical correctness of Murmur3 Kirsch–Mitzenmacher double hashing prior to modifying reader data-block seek paths.
* **Impact**: The Bloom filter implementation is empirically proven to produce $p \le 0.01$ (measured $0.8186\%$) with zero false negatives. `TableReader.Seek` will short-circuit block reads once wired into the reader in future optimization phases.
* **How It Was Detected**: P05-S02-M02 empirical benchmark harness (`TestBloomFilter_EmpiricalFalsePositiveRate_1M`, `BenchmarkBloomFilter_EmpiricalFPR_1M`).
* **Current Mitigation**: Unit tests verify theoretical calculations, Wilson score intervals, and zero false negatives on all inserted samples; benchmarks demonstrate 100% reproducible execution in ~72.1 ms with 4 total heap allocations.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Observed FPR 0.8186% falls cleanly within 95% Wilson score confidence interval [0.8011%, 0.8365%]; zero false negatives).
  * Performance: **Optimal** (~72.1 ms for 1M inserts + 1M queries, >18.5M ops/sec, zero allocations in inner loops).
  * Scalability: **Optimal** (Streaming stack formatting prevents unbounded heap memory consumption during multi-million key evaluations).

---

### 32. Append-Only MANIFEST Log Persistence Scope & Replay Boundary (P06-S01-M02)
* **Limitation**: In `P06-S01-M02`, `ManifestWriter` implements the durable append-only write path for CRC32-framed `VersionEdit` records with `fdatasync()` synchronization, but the active manifest pointer (`CURRENT`), multi-version reference counting (`VersionSet`), and startup recovery replay engine are intentionally excluded and reserved for subsequent phases.
* **Why It Exists**: Hard scope boundary enforcement. Atomic pointer swaps (`CURRENT` via `P06-S02-M01`), `VersionSet` reference counting (`P06-S02-M02`), and recovery log replay (`P07-S01`) build directly on top of this established append-only log primitive.
* **Impact**: MANIFEST files are durably written and framed with CRC32 integrity checks, but state reconstruction upon database startup will be completed in Phase 07.
* **Current Mitigation**: Comprehensive independent test oracle and corruption test suite in `manifest_writer_test.go` verifying record framing, CRC32 protection, truncation detection, and bit-rot rejection across 50-edit sequences and restart reopens. IND-C-002 proof in `sec_ind_c002_test.go` verifies the writer performs no rotation (no Stat → Remove → Rename path): decoy `MANIFEST.old/.tmp/.bak` symlinks are never followed or replaced and symlink-at-managed-path opens fail closed. IND-M-005 proof in `sec_ind_m005_test.go` verifies the log is always a complete CRC-valid watermark-ordered prefix across close/reopen, and that barrier failure poisons fail-closed with the record still fully framed (replay converges by idempotent re-application from the watermark; see `ManifestWriter` contract item 10).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Exact-byte fixtures, 100% test pass, poison state machine).
  * Performance: **Optimal** (~1,059 ns/op framing CPU latency; hardware NVMe flush dominates at ~3.6 ms).
  * Scalability: **Optimal** (Append-only O(1) writes with 64-bit offset safety).

---

### 33. CURRENT Pointer Staging Scope & Recovery Decoupling (P06-S02-M01)
* **Limitation**: In `P06-S02-M01`, `SetCurrentManifest` establishes the atomic and crash-safe filesystem pointer swapping mechanism (`CURRENT.tmp` $\to$ `CURRENT`) with directory synchronization, but active manifest discovery, pointer parsing upon startup, and `VersionSet` version tracking are intentionally excluded and reserved for subsequent phases (`P06-S02-M02` and `P07-S01-M01`).
* **Why It Exists**: Hard scope boundary enforcement. The storage engine requires an atomic, uncorruptible pointer primitive on disk before building high-level version reference counting (`VersionSet`) and crash recovery replay engines (`P07-S01`).
* **Impact**: `CURRENT` can be safely created and atomically replaced on disk without risk of torn pointers, but automatic boot discovery and manifest replay will be implemented in Phase 07.
* **Current Mitigation**: Comprehensive test suite in `current_test.go` simulating power interruptions across all failure points (write failure, short write, sync failure, close failure, rename failure, directory sync failure) proving that prior valid `CURRENT` files remain strictly intact. `sec_ind_c001_test.go` (`TestINDC001_*`) proves the IND-C-001 ordering invariant (file-sync → rename → exactly-once parent dir-sync) and the dir-sync-failure contract (`ErrCurrentDirectorySync`, renamed pointer preserved, no staging residue).
* **Durability Modes**: Default file barrier is `fdatasync` on Linux (`f.Sync` fallback elsewhere) for lower latency; `version.SetCurrentStrictSync(true)` opts into a full `f.Sync`/`fsync` file barrier at higher I/O cost. The parent-directory barrier always runs after rename in both modes.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Atomic rename atomicity guarantees zero torn pointers).
  * Performance: **Optimal** (~118 µs without sync; ~8.53 ms with double hardware barrier: file `fdatasync` + directory `fsync`).
  * Scalability: **Optimal** (Tiny 16-byte text pointer).

---

### 34. CURRENT Reader Bounded Allocation & Recovery Decoupling (P06-S02-M02)
* **Limitation**: In `P06-S02-M02`, `ReadCurrentManifest` safely locates, inspects, and strictly parses the active manifest sequence number from `CURRENT`, but intentionally does not open or inspect the referenced `MANIFEST-NNNNNN` file, does not check if the referenced manifest exists on disk, does not replay records, and does not reconstruct `Version` or `VersionSet` states.
* **Why It Exists**: Strict architectural separation of concerns. `ReadCurrentManifest` is a low-level metadata reader primitive. Higher-level lifecycle orchestration (`VersionSet` tracking in `P06-S02-M03` and full crash recovery in `Phase 07`) builds atop this primitive without coupling file parsing to storage engine startup logic.
* **Impact**: Callers receive the validated sequential manifest sequence number (`uint64`), but must rely on subsequent phase components to replay delta logs.
* **Current Mitigation**: Strict parser validation enforces the exact canonical format `MANIFEST-%06d\n` (rejecting whitespace, non-digits, CRLF, overflow, or manifest 0), while bounded 31-byte stack buffering ensures zero heap allocation and immunity to resource-exhaustion attacks. Post-open `os.SameFile` verification with bounded retry ensures race-free reads under concurrent atomic `os.Rename` operations.
* **Dimensional Impact**:
  * Correctness: **Optimal** (2.35M fuzz executions with 0 crashes; fail-closed on any non-canonical formatting).
  * Performance: **Optimal** (~58.8 ns/op pure CPU parsing; ~12.2 µs/op full OS filesystem pipeline).
  * Scalability: **Optimal** (Fixed 31-byte stack buffer; 0 heap allocations in parser).

---

### 35. VersionSet In-Memory Lifecycle Scope & Physical Reclamation Decoupling (P06-S02-M03)
* **Limitation**: In `P06-S02-M03`, `Version` and `VersionSet` establish the in-memory metadata snapshot ownership layer with atomic reference counting (`Ref()`, `Unref()`, `TryRef()`) and an active circular doubly-linked version chain, but manifest log replaying (`Phase 07`), `VersionEdit` delta application to construct versions from disk, compaction scoring (`Phase 08`), and physical SSTable file unlinking are deliberately excluded.
* **Why It Exists**: Strict architectural separation of concerns. Reference counting establishes the lifetime and pinning boundary for immutable metadata in RAM. Physical SSTable deletion is a dangerous disk-level operation that must only occur after the storage engine's compaction coordinator verifies that no live Version references the file.
* **Impact**: Versions can be created, installed into `VersionSet`, pinned by concurrent readers, superseded by newer versions, and safely finalized (unlinked and cleared from memory) when reference counts hit 0, but disk recovery and physical file deletion will be wired in Phases 07 and 08.
* **Current Mitigation**: Strict atomic compare-and-swap state transitions prevent resurrection from 0 and underflow on double Unref, while defensive cloning in `NewVersion` guarantees complete immutability. Compactor retention is simulated in unit tests proving that obsolete resources remain retained while older versions are pinned by readers. IND-H-003 proof in `sec_ind_h003_test.go` replays the audit's use-after-free interleaving (pin → supersede → use) proving the pinned version survives with exact refcount accounting and exactly-once cleanup, plus a pin-vs-install race under `-race`. IND-H-004 proof in `sec_ind_h004_test.go` shows no iterator-invalidation callback is needed because the pin is the guard: held `Files()` snapshots stay fully decodable across supersede, are defensively copied, and unpinned versions reclaim deterministically exactly once (never dangling). IND-H-008 proof in `sec_ind_h008_test.go` hand-encodes the audit's malicious wire bytes (smallest=100 > largest=50) proving decode rejects with `ErrInvalidSeqNumRange` and nil edit, with a valid control proving the harness.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Lock-free atomic pinning; 0 data races under `-race`).
  * Performance: **Optimal** (~7.8 ns/op `Ref`/`Unref`; ~7.8 ns/op `Current()` pinning; 0 heap allocs).
  * Scalability: **Optimal** (O(1) active chain updates; circular linked list bounds dead version retention).

---

### 36. MetaIndex Parser Integer-Overflow / Slice-Bounds Panic Vulnerability (SEC-001 / F-001) — REMEDIATED
* **Limitation**: In prior implementations through Phase 05/06, `DecodeMetaIndexBlock` (`internal/sstable/meta_index.go`) permitted an attacker-controlled `keyLen` (up to $2^{64}-1$) to participate in unchecked 64-bit unsigned arithmetic: `expectedLen := uint64(varintLen) + keyLen + BlockHandleSize`. In unsigned 64-bit integer arithmetic, addition wraps modulo $2^{64}$. A crafted entry where `varintLen=10` and `keyLen = 2^64 - 6` wrapped modulo $2^{64}$ to equal `20`, matching `len(entrySlice) == 20` and bypassing entry length validation. The subsequent conversion `int(keyLen)` evaluated to `-6` on 64-bit architectures, causing an invalid slice expression (`entrySlice[10:4]`) and triggering an unhandled Go runtime slice-bounds panic (`panic: slice bounds out of range [10:4]`). When a client or background reader invoked `TableReader.ReadFilterBlock() -> FindMetaIndexEntry()`, a maliciously crafted or bit-flipped SSTable could crash the entire database process.
* **Root Cause**:
  1. Lack of domain validation on decoded varint `keyLen` prior to arithmetic.
  2. Performing addition with untrusted operands (`varintLen + keyLen + BlockHandleSize`) vulnerable to unsigned 64-bit integer wraparound.
  3. Converting untrusted `uint64` to `int` prior to bounding the value against the underlying slice buffer length.
* **Remediation**:
  1. *Immediate Domain Validation*: Immediately after decoding the varint, `keyLen` is validated against domain boundaries: rejecting `keyLen == 0 || keyLen > binary.MaxEncodedInternalKeyLen` (65,536 bytes) with `*errors.IndexBlockCorruptedError`.
  2. *Buffer Minimum Threshold*: Explicitly verifies `len(entrySlice) >= varintLen + BlockHandleSize` before performing any length math.
  3. *Overflow-Safe Subtraction Bounds*: Validates available payload via safe subtraction: `remainingForKey := uint64(len(entrySlice) - minEntryLen)` and asserts `keyLen == remainingForKey`.
  4. *Safe Bounded Integer Conversion*: Conversion `int(keyLen)` is executed only after proving `keyLen` is bounded within $[1, \text{len}(entrySlice) - minEntryLen]$, guaranteeing safe slicing.
  5. *Fail-Closed Error Taxonomy*: Rejection returns structured `*errors.IndexBlockCorruptedError` without panicking, preserving fail-closed error contracts.
* **Regression Coverage & Evidence**:
  - *Audit PoC Regression*: `TestSecurity_Remediation_SEC_001_IntegerOverflowPanicPoC` proves the exact arithmetic exploit vector (`keyLen = 2^64 - 6` with `entryLen = 20`) which previously caused `panic: slice bounds out of range [10:4]` now safely fails closed with `*errors.IndexBlockCorruptedError` and zero panics.
  - *Boundary Matrix*: `TestSecurity_Remediation_SEC_001_BoundaryMatrix` covers `keyLen = 0`, `MaxUint64`, `MaxUint64 - 1`, `MaxUint64 - 6`, `MaxUint64 - 7`, maximum valid length, one above maximum, buffer mismatches, truncated buffers, and malformed block handles.
  - *Adversarial Mutation Suite*: `TestSecurity_Remediation_SEC_001_MutationTesting` verifies single-byte mutations over varint headers, entry lengths, and block handle trailers.
  - *Real End-to-End File Path*: `TestSecurity_Remediation_SEC_001_TableReader_ReadFilterBlockPath` constructs an on-disk `.sst` table containing the crafted MetaIndex block and verifies `TableReader.ReadFilterBlock` fails closed with corruption errors and zero panics.
  - *Fuzz Testing*: `FuzzMetaIndexBlock_Decode` fed raw arbitrary bytes directly into `DecodeMetaIndexBlock` for >7.3 million executions with 0 crashes.
* **Scope Note**: This remediation specifically resolves vulnerability SEC-001 / F-001 in `DecodeMetaIndexBlock`. It does not claim that all conceivable SSTable parser vulnerabilities are eliminated.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Malformed metadata fails closed with structured corruption error).
  * Performance: **Optimal** (Eliminated redundant arithmetic; zero additional heap allocations).
  * Security: **Optimal** (Process-crashing DoS panic vector eliminated).

---

### 37. Concurrent CURRENT Writer Staging Collision & Race Vulnerability (SEC-002 / F-002) — REMEDIATED
* **Limitation**: In prior implementations through Phase 06, `SetCurrentManifest` (`internal/version/current.go`) used a shared static staging path `CURRENT.tmp` without writer serialization. When multiple in-process goroutines concurrently invoked `SetCurrentManifest` for the same database directory, a time-of-check-to-time-of-use (TOCTOU) staging collision occurred: Writer A created `CURRENT.tmp` and began writing; Writer B entered, observed `CURRENT.tmp`, treated it as an orphaned crash artifact, and unlinked it; Writer B then created a new `CURRENT.tmp` inode at that path. Writer A subsequently finished writing to its unlinked file descriptor and executed `os.Rename(CURRENT.tmp, CURRENT)`, promoting Writer B's partially written or differently sequenced temporary file into `CURRENT`. This resulted in corrupted manifest sequence pointers, broken recovery invariant states, or premature pointer swaps.
* **Root Cause**:
  1. Static staging path `CURRENT.tmp` shared concurrently without writer serialization.
  2. Stale-file cleanup step in `SetCurrentManifest` blindly unlinking active temporary files created by concurrent in-flight writers.
  3. Decoupling temporary file creation from the atomic rename across overlapping execution lifetimes.
* **Remediation**:
  1. *Directory-Scoped In-Process Writer Serialization*: Introduced `currentLockRegistry` with reference-counted per-directory synchronization (`currentDirLock`). Concurrent invocations of `SetCurrentManifest` for the same canonical database directory are strictly serialized.
  2. *Canonical Path Normalization*: Keyed by `canonicalDirKey`, which evaluates `filepath.Clean`, `filepath.Abs`, and physical symlink targets via `filepath.EvalSymlinks`, guaranteeing that relative paths, absolute paths, and symlink aliases map to the identical synchronization lock.
  3. *Zero-Leak Lifecycle*: Uses reference-counting (`refCount`); when active and pending callers conclude, the directory key is automatically removed from the registry map, guaranteeing zero memory leaks.
  4. *Atomic Section Scope*: The directory lock covers the entire staging lifecycle: stale temporary file inspection, cleanup, exclusive file creation (`O_WRONLY|O_CREATE|O_EXCL`), inode pinning (`os.SameFile`), content serialization, `fdatasync`, close, `os.Rename`, and parent directory sync.
  5. *Directory Isolation*: Unrelated database directories acquire disjoint locks and proceed fully in parallel without cross-database serialization bottlenecks.
* **Regression Coverage & Evidence**:
  - *Deterministic Interleaving Race*: `TestSetCurrentManifest_ConcurrentWriters_DeterministicRace` forces Writer A into staging (`CURRENT.tmp` created, paused in write), launches Writer B concurrently, verifies Writer B is strictly blocked, asserts Writer A's staging inode is NOT unlinked or replaced (`os.SameFile`), and unblocks Writer A to verify clean sequential progression.
  - *High-Contention Stress*: `TestSetCurrentManifest_ConcurrentWriters_Stress` runs 16 concurrent workers executing 320 parallel updates, verifying 100% success, zero corrupted reads, and zero active locks remaining.
  - *Concurrent Writers + Readers*: `TestSetCurrentManifest_ConcurrentWriters_WithReaders` runs 8 writers and 8 readers under `-race`, proving readers never observe partial, empty, or torn bytes.
  - *Directory Isolation*: `TestSetCurrentManifest_DirectoryIsolation` proves writers on Dir 1 do not block writers on Dir 2.
  - *Registry Lifecycle*: `TestCurrentLockRegistry_Lifecycle` verifies canonical path resolution, acquire/release, and idempotent release (`sync.Once`).
  - *Benchmark Stability*: `BenchmarkSetCurrentManifest_ConcurrentWriters` demonstrates ~130 µs/op non-sync throughput with lock acquisition and 0 data races.
* **Remaining Scope Boundary**:
  - Remediates in-process concurrency between goroutines. Cross-process concurrency (multiple independent OS processes modifying the same directory) is not protected by in-process Go synchronization and relies on single-process database locking (e.g. `flock`) scheduled for engine initialization in Phase 10.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Concurrent CURRENT updates preserve strict atomic replacement and inode integrity).
  * Performance: **Optimal** (Lock acquisition overhead is sub-microsecond; directory sync and disk I/O dominate latency).
  * Security: **Optimal** (Eliminated persistent CURRENT corruption and recovery failure vector).

---

### 38. Unsafe `VersionSet.ActiveVersions()` Lifetime Semantics (SEC-003 / F-003) — REMEDIATED
* **Limitation**: In prior implementations through Phase 06, `ActiveVersions()` (`internal/version/version_set.go`) traversed the active circular doubly-linked version chain under `vs.mu.RLock()` and appended raw `*Version` pointers without incrementing their reference counts. Once `vs.mu.RUnlock()` was dropped, the returned pointers were not owned by the caller. If another goroutine called `AppendVersion()`, the VersionSet released its ownership reference to superseded versions (`oldCurrent.Unref()`). When a superseded version had no outstanding reader pins from `Current()`, its reference count transitioned to zero, triggering immediate finalization (`finalize()`, which nils out `v.levels`) and unlinking from the chain while the caller was still holding and inspecting the returned `*Version` pointer. Attempting to inspect metadata could result in nil-pointer or empty slice access, racing with cleanup, and attempting to retain the version via `v.Ref()` panicked due to non-resurrection enforcement (`refCount <= 0`).
* **Root Cause**:
  1. `ActiveVersions()` returning unpinned pointers, decoupling slice membership from reference-counted object lifetime.
  2. Failure to transfer a caller-owned reference prior to dropping the structural read lock `vs.mu`.
  3. Window between `oldCurrent.Unref()` and `finalize()` unlinking allowing dead versions (`refCount == 0`) to be observed in the chain.
* **Remediation**:
  1. *Pin-Before-Unlock Pattern*: While holding `vs.mu.RLock()`, `ActiveVersions()` walks the active chain and attempts atomic pinning on each node via `cur.TryRef()`.
  2. *Caller Ownership Contract*: Callers now receive pinned `*Version` references and own one reference per returned element. Callers MUST call `Unref()` exactly once on every returned version when finished.
  3. *Dead Node Filtering & Non-Resurrection*: If `cur.TryRef()` returns `false` (because `refCount <= 0` pending unlinking in `finalize()`), the node is omitted from the result, preventing resurrection of finalized versions.
  4. *ActiveCount Synchronization*: `ActiveCount()` verifies `cur.refCount.Load() > 0`, ensuring only live versions are counted.
* **Regression Coverage & Evidence**:
  - *Ownership Contract*: `TestVersionSet_ActiveVersions_OwnershipContract` verifies all returned pointers are pinned (`refCount` incremented), remain alive and readable after being superseded and after reader pins are dropped, and clean up cleanly when unreferenced.
  - *Deterministic Lifetime Race*: `TestVersionSet_ActiveVersions_DeterministicLifetimeRace` reproduces F-003 by publishing 10 replacement versions while a caller retains a snapshot, proving the snapshot remains alive and metadata intact until caller `Unref()`.
  - *Finalization Protection*: `TestVersionSet_ActiveVersions_FinalizationProtection` verifies `finalize()` cannot run while an active snapshot pin exists, and confirms `Ref()` panics on dead versions while `TryRef()` safely fails.
  - *Concurrent Stress*: `TestVersionSet_ActiveVersions_ConcurrentStress` runs concurrent appenders, active readers, and current readers under `-race`, verifying 0 data races, 0 panics, and 0 memory leaks.
  - *Randomized Lifecycle*: `TestVersionSet_ActiveVersions_RandomizedLifecycle` exercises repeated cycles of version creation, snapshot retention, and out-of-order unrefs.
  - *Benchmarks*: `BenchmarkVersionSet_ActiveVersions_Single` (12.08 ns/op, 8 B/op) and `BenchmarkVersionSet_ActiveVersions_Multiple` (26.62 ns/op, 64 B/op) confirm negligible atomic overhead.
* **Remaining Scope Boundary**:
  - Remediates `ActiveVersions()` lifetime semantics. Does not modify `VersionEdit` validation (F-004) or refcount overflow handling (F-005).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Callers have guaranteed object lifetimes for all inspected version snapshots).
  * Performance: **Optimal** (~12-26 ns/op per call, sub-nanosecond per version pin).
  * Security: **Optimal** (Eliminated use-after-lifetime and panic race vectors).

---

### 39. VersionEdit AddFile Accepts Structurally & Semantically Invalid FileMetadata (SEC-004 / F-004) — REMEDIATED
* **Limitation**: In prior implementations through Phase 06, `VersionEdit.AddFile()` and `DecodeVersionEdit()` validated only high-level scalar bounds (`level < NumLevels`, `len(key) <= MaxEncodedInternalKeyLen`). They failed to validate semantic invariants required of finalized SSTables:
  1. `FileNum == 0` was accepted, admitting unassigned/sentinel file numbers into the manifest and VersionSet.
  2. `FileSize == 0` was accepted, allowing zero-byte non-existent or truncated SSTables to participate in compaction calculations and query routing.
  3. `SmallestSeqNum > LargestSeqNum` was accepted, allowing reversed sequence ranges to corrupt multi-version visibility calculations.
  4. `SmallestKey` and `LargestKey` were treated as arbitrary opaque bytes and not validated as serialized `binary.InternalKey`s (allowing truncated keys, missing trailers, and invalid `OpType` kinds like 0x00 or > 0x02 to enter the VersionSet).
  5. Backwards key ranges (`SmallestKey > LargestKey` under the canonical storage engine comparator `binary.CompareInternalKey`) were admitted without check.
  6. Corrupt or crafted manifest records could bypass programmatic checks and reconstruct invalid `FileMetadata` directly into active versions.
* **Root Cause**:
  1. Conflating *syntactic wire-format validity* (varints decode, lengths fit within buffer) with *semantic domain validity* (keys conform to InternalKey invariants, keys sort monotonically, file numbers are valid).
  2. Lack of a unified admission validation boundary between programmatic construction (`AddFile`) and manifest reconstruction (`DecodeVersionEdit`).
* **Remediation**:
  1. *Canonical Admission Validator (`ValidateFileMetadata`)*: Centralized semantic validation in `ValidateFileMetadata(level uint32, meta FileMetadata) error` enforcing all 7 domain invariants:
     - Level Invariant: `0 <= level < NumLevels` (returns `*errors.InvalidLevelError`).
     - File Number Invariant: `meta.FileNum > 0` (returns `errors.ErrInvalidFileNum`).
     - File Size Invariant: `meta.FileSize > 0` (returns `errors.ErrInvalidFileSize`).
     - Sequence Range Invariant: `meta.SmallestSeqNum <= meta.LargestSeqNum` (returns `errors.ErrInvalidSeqNumRange`).
     - Key Structural Invariant: Both `SmallestKey` and `LargestKey` validated via canonical `binary.ValidateEncodedInternalKey` (checks 10-byte minimum trailer, valid UserKey length [1, 65535], valid OpType Put or Delete).
     - Key Range Ordering Invariant: Unpacks InternalKeys zero-allocation and verifies `binary.CompareInternalKey(ikSmall, ikLarge) <= 0` under canonical storage engine ordering (returns `errors.ErrInvalidKeyRange`). Allows `SmallestKey == LargestKey` for single-entry SSTables.
  2. *Parity Across Admission Boundaries*: Both programmatic `AddFile` and manifest decoding `DecodeVersionEdit` (`TagAddFile` handler) call `ValidateFileMetadata` before admitting any entry. Direct decoding of invalid payloads fails closed immediately.
  3. *Atomicity Guarantee*: If validation fails, `AddFile` returns the error and leaves `edit.addedFiles` completely unmodified (`NumAddedFiles()` invariant preserved).
  4. *Defensive Cloning*: Retains independent cloned byte slices for `SmallestKey` and `LargestKey` on admission and on read (`AddedFiles()`), preventing external mutation of internal metadata state.
* **Regression Coverage & Evidence**:
  - *Valid Metadata*: `TestSEC004_ValidMetadata` verifies standard levels (0..6), minimum valid internal keys (1-byte user key), equal key boundary cases, reverse-sequence ordering for identical user keys, and max key sizes.
  - *Invalid Scalars*: `TestSEC004_InvalidScalars` tests rejection of `level >= 7`, `FileNum == 0`, `FileSize == 0`, and `SmallestSeqNum > LargestSeqNum`.
  - *Invalid Keys*: `TestSEC004_InvalidKeys` tests rejection of nil/empty keys, truncated keys (< 10 bytes), oversized keys (> 65,544 bytes), and invalid `OpType`s.
  - *Key Range Ordering*: `TestSEC004_KeyRangeOrdering` tests user-key backwards ranges, sequence ordering inversions for identical user keys, op-type inversions, and identical single-entry keys.
  - *Atomicity*: `TestSEC004_Atomicity` verifies zero partial admission or modification of existing entries upon validation error.
  - *Defensive Copying*: `TestSEC004_DefensiveCopying` verifies isolation against caller mutation before and after addition.
  - *Decoder Rejection*: `TestSEC004_DecoderRejection` constructs raw TLV payloads and verifies `DecodeVersionEdit` rejects all invalid metadata with matching sentinels without panic.
  - *Adversarial Matrix*: `TestSEC004_AdversarialTable` exercises the complete matrix of scalar and compound violations.
  - *Admission Parity*: `TestSEC004_AdmissionParity` confirms exact failure parity between programmatic `AddFile` and wire-level `DecodeVersionEdit`.
  - *Fuzz Testing*: `FuzzDecodeVersionEdit` ran 1,424,975 executions in 11s with 0 crashes, verifying that every admitted file in any successfully decoded edit satisfies all 7 metadata invariants.
* **Remaining Scope Boundary**:
  - Remediates `VersionEdit` metadata validation (F-004). Does not modify refcount overflow handling (F-005), TableWriter symlink checks (F-006), or TableReader path checks (F-007).
* **Dimensional Impact**:
  * Correctness: **Optimal** (VersionSet and manifest replay state are guaranteed to only contain semantically valid SSTable metadata).
  * Performance: **Optimal** (Zero-allocation InternalKey decoding during validation; benchmarks show negligible nanosecond-scale validation cost).
  * Security: **Optimal** (Eliminated corrupt manifest replay, invalid compaction range panic, and state corruption vectors).

---

### 40. Version Reference Count Overflow at MaxInt32 (SEC-005 / F-005) — REMEDIATED
* **Limitation**: In prior implementations through Phase 06, `Version.TryRef()` and `Version.Ref()` (`internal/version/version.go`) used an atomic compare-and-swap loop that checked only `cur <= 0`. It failed to check the upper boundary `cur == math.MaxInt32` before computing `cur + 1`. In Go, 32-bit signed integer addition wraps around modulo $2^{32}$ without runtime errors or panics. If `refCount` reached `math.MaxInt32` (2,147,483,647), a subsequent `TryRef()` or `Ref()` incremented `cur + 1` to `math.MinInt32` (-2,147,483,648). This negative value permanently corrupted the Version lifecycle:
  1. Subsequent calls to `TryRef()` observed `cur <= 0` and returned `false`, falsely treating a live Version as dead.
  2. Subsequent calls to `Ref()` panicked with `"cannot Ref dead Version: reference count is zero or negative"`.
  3. Subsequent calls to `Unref()` observed `cur <= 0` and panicked with `"Version refCount underflow: double Unref"`.
  4. Finalization and active chain unlinking could never execute, stranding obsolete SSTables on disk permanently.
* **Root Cause**:
  1. Conflating *atomic thread-safety* (CAS prevents lost updates) with *arithmetic domain safety* (CAS does not prevent signed integer overflow).
  2. Computing `cur + 1` before proving that `cur < math.MaxInt32`.
* **Remediation**:
  1. *Pre-Computation Upper-Bound Guard*: In both `Version.TryRef()` and `Version.Ref()`, the upper boundary `cur == math.MaxInt32` is explicitly evaluated inside the CAS loop *before* any addition occurs:
     ```go
     for {
         cur := v.refCount.Load()
         if cur <= 0 || cur == math.MaxInt32 {
             return false
         }
         if v.refCount.CompareAndSwap(cur, cur+1) {
             return true
         }
     }
     ```
     Because `cur < math.MaxInt32` is proven prior to addition, `cur + 1` is mathematically guaranteed to remain within `[2, math.MaxInt32]`, completely eliminating the overflow vector.
  2. *Exact Accounting & Anti-Saturation*: Rejects reference acquisition once capacity is reached (`TryRef() == false`), strictly preserving 1-to-1 ownership accounting rather than saturating or clamping.
  3. *Distinguishable Failure Modes*: `Ref()` panics with `"cannot Ref Version: reference count exhausted at MaxInt32"` when `cur == math.MaxInt32`, clearly distinguishing capacity exhaustion from dead Version resurrection (`cur <= 0`).
  4. *Preserved MaxInt32 Liveness & Underflow Defense*: A Version at `math.MaxInt32` remains a valid live object; `Unref()` successfully transitions `MaxInt32 -> MaxInt32 - 1`. Underflow protection at `cur <= 0` remains strictly enforced.
* **Regression Coverage & Evidence**:
  - *TryRef at MaxInt32*: `TestSEC005_TryRef_AtMaxInt32` verifies clean rejection (`false`), zero mutation, and no overflow.
  - *Ref at MaxInt32*: `TestSEC005_Ref_AtMaxInt32` verifies explicit exhaustion panic without reporting a dead Version.
  - *One Below Maximum*: `TestSEC005_OneBelowMaximum` verifies transition `MaxInt32 - 1 -> MaxInt32` succeeds, and next attempt immediately fails.
  - *Maximum Release*: `TestSEC005_MaxRelease` verifies `Unref()` from `MaxInt32 -> MaxInt32 - 1` succeeds without panic.
  - *Boundary Round Trip*: `TestSEC005_BoundaryRoundTrip` tests complete cycle `MaxInt32 - 2 -> -1 -> Max -> reject -> -1 -> -2`.
  - *Dead Version Distinction*: `TestSEC005_DeadVersionDistinction` confirms distinct panic messages for dead vs exhausted versions.
  - *Underflow Regression*: `TestSEC005_UnderflowRegression` confirms double-Unref panics and finalization runs exactly once.
  - *Concurrent Maximum-Collision*: `TestSEC005_ConcurrentCollision` launches 50 concurrent goroutines against `MaxInt32 - 1`; exactly 1 acquires the reference, 49 fail cleanly, and final count is exactly `MaxInt32`.
  - *Concurrent Stress*: `TestSEC005_ConcurrentStress` exercises 3,200 mixed TryRef/Unref operations near the maximum boundary under `-race` with 0 races and exact count retention.
  - *Arithmetic Safety Property*: `TestSEC005_Property_ArithmeticSafety` validates mathematical invariants across all representative states.
   - *Benchmarks*: `BenchmarkVersion_RefUnref` confirms 8.05 ns/op, 0 B/op, 0 allocs/op (zero measurable overhead).
   - *IND-M-001 proof*: `sec_ind_m001_test.go` verifies 100k balanced Ref/Unref cycles never drift or wrap negative, and the MaxInt64 cap rejects (Ref panics, TryRef refuses) without wraparound while staying live under Unref.
* **Remaining Scope Boundary**:
  - Remediates Version refcount overflow (F-005). Does not modify TableWriter symlink checks (F-006) or TableReader path checks (F-007).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Mathematical proof that refCount stays within valid [0, MaxInt32] domain).
  * Performance: **Optimal** (Single branch in existing CAS loop, ~8 ns/op, zero allocations).
  * Security: **Optimal** (Eliminated negative counter corruption, permanent resource leaks, and spurious underflow panics).

---

### 41. TableWriter Parent-Directory Symlink / TOCTOU Race (SEC-006 / F-006) — REMEDIATED
* **Limitation**: In prior implementations through Phase 06, `TableWriter` (`internal/sstable/table_writer.go`) operated exclusively on string pathnames for staging file creation, atomic publication, directory synchronization, and failure cleanup. The parent directory was referenced by `filepath.Dir(dstPath)` — a string re-resolved on every filesystem operation. Between `NewTableWriter` initialization and `Finish()` publication, an attacker or concurrent process could replace the parent directory path with a symlink or different directory object, redirect intermediate path components, or exploit cleanup operations targeting swapped directories.
* **Root Cause**:
  1. Treating pathname strings as stable filesystem identity references (violating TOCTOU invariant).
  2. Re-resolving `filepath.Dir(w.dstPath)` at publication and sync time instead of pinning directory object identity.
  3. Not validating intermediate path components for symlink injection.
* **Remediation**:
  1. *Parent Directory Descriptor Pinning*: `NewTableWriter` opens the parent directory, captures `parentDirFile *os.File` and `parentDirStat os.FileInfo`, validates `IsDir()` and `os.SameFile(parentStat, parentLstat)`.
  2. *Intermediate Path Component Validation*: `validatePathNoSymlinks()` inspects every existing path component, rejecting unpermitted symlinks. macOS system symlinks (`/var`, `/tmp`, `/etc`) whitelisted.
  3. *Pre-Publication Identity Re-Verification*: Before `linkFn`, re-verifies `os.SameFile(w.parentDirStat, curParentStat)` and intermediate components. Mismatches fail closed with `ErrParentDirectorySwapped`.
  4. *Pinned Directory Sync*: Calls `w.parentDirFile.Sync()` directly on the pinned descriptor.
  5. *Safe Cleanup*: `cleanupStaging()` verifies parent directory identity before `os.Remove`, refusing to delete files in swapped directories.
* **Regression Coverage**: 11 targeted test cases including parent symlink rejection, parent directory replacement detection, symlink swap detection, intermediate symlink redirect, failure cleanup isolation, pinned directory sync verification, concurrent replacement race (15 iterations), and 9-scenario object identity matrix. All pass under `-race` detector.
* **Remaining Scope Boundary**:
  - Remediates TableWriter parent directory TOCTOU (F-006). Does not modify TableReader path checks (F-007).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Every security-sensitive operation refers to pinned directory object identity).
  * Performance: **Optimal** (One additional `os.Open` + `Stat` at construction; two `Lstat` + `SameFile` at publication).
  * Security: **Optimal** (Eliminated parent directory TOCTOU, intermediate symlink redirect, and cleanup deletion in swapped directory vectors).

---

### 42. TableReader Path / Symlink TOCTOU (SEC-007 / F-007) — REMEDIATED
* **Limitation**: In prior implementations through Phase 06, `TableReader` (`internal/sstable/table_reader.go`) opened SSTable files via naive `os.Open(path)` followed by `file.Stat()`. Standard `os.Open()` automatically follows symbolic links, so if `path` or any intermediate directory was replaced with a symlink to an attacker-controlled file, `file.Stat()` inspected the target of the symlink (a regular file), causing `TableReader` to silently consume attacker-controlled bytes. Furthermore, between a caller selecting `path` and the reader finishing validation, an attacker could substitute the underlying file object (different inode) at the same pathname.
* **Root Cause**:
  1. Relying on standard `os.Open(path)` which automatically resolves symlinks without verifying destination or ancestor symlink policy.
  2. Performing validation without correlating the opened descriptor's inode against the pathname's pre- and post-open filesystem objects.
  3. Lack of intermediate path component validation to prevent directory-level symlink redirection.
* **Remediation**:
  1. *Pre-Open Path Inspection*: `os.Lstat(path)` verifies before open that `path` is an existing regular file, not a symlink (`ModeSymlink == 0`), and not a directory (`!IsDir()`).
  2. *Intermediate Component Validation*: `validatePathNoSymlinks(filepath.Dir(path))` walks ancestor path components from root down to parent directory, rejecting unpermitted symlinks with `ErrParentDirectorySymlink` (whitelisting Darwin system prefixes `/var`, `/tmp`, `/etc`).
  3. *Secure Open with Guaranteed Cleanup*: Opens via `openFileFn(path)` (`os.Open`). A deferred cleanup block guarantees `file.Close()` on all subsequent validation failure paths, preventing descriptor leaks.
  4. *Descriptor-Based Validation*: `fstat, err := file.Stat()` validates `!fstat.IsDir()` and `fstat.Mode().IsRegular()` directly on the opened file descriptor (`fstat`).
  5. *Post-Open Path Re-Inspection*: `os.Lstat(path)` re-inspects the pathname to verify it was not swapped with a symlink during or immediately after the open operation.
  6. *Inode Pinning & Object Identity Invariance*: Both `os.SameFile(fstat, lstatBefore)` and `os.SameFile(fstat, lstatAfter)` must hold. This guarantees the opened descriptor references the exact inode observed before and after opening, failing closed with `ErrSSTableObjectChanged` or `ErrSSTableSymlink` if any substitution occurs.
  7. *Ancestor Re-Verification*: `validatePathNoSymlinks` re-verifies ancestor hierarchy post-open.
  8. *Descriptor-Centric Immutable Reads*: Once initialized, `TableReader` executes all point lookups (`Seek`), filter reads (`ReadFilterBlock`), and index reads strictly via positional `file.ReadAt` on the pinned descriptor. The pathname is never re-opened or re-consulted, ensuring subsequent disk modifications cannot redirect reader reads.
  9. *Sentinels*: Added `ErrSSTableSymlink` and `ErrSSTableObjectChanged` to `internal/errors`.
* **Regression Coverage & Evidence**:
  - *Normal Regular SSTable*: `TestSEC007_NormalRegularSSTable_Accepted` verifies standard point lookups and filter reads succeed.
  - *Destination Symlink Rejection*: `TestSEC007_PathIsSymlink_Rejected` verifies destination symlink is rejected with `ErrSSTableSymlink` and target file is preserved.
  - *Intermediate Symlink Rejection*: `TestSEC007_IntermediateComponentSymlink_Rejected` verifies intermediate directory symlinks fail closed with `ErrParentDirectorySymlink`.
  - *Pre-Open Replacement*: `TestSEC007_FileReplacedBeforeOpen_Detected` verifies pre-open substitution fails closed.
  - *Post-Open Descriptor Authoritative*: `TestSEC007_FileReplacedAfterOpen_DescriptorAuthoritative` proves that deleting/replacing the file on disk while the reader is open DOES NOT affect reader lookups (descriptor continues reading original payload).
  - *Inode Substitution Detection*: `TestSEC007_SamePathDifferentInode_Detected` verifies post-open file swap fails closed with `ErrSSTableObjectChanged`.
  - *Post-Open Symlink Swap*: `TestSEC007_PostOpenSymlinkSwap_Detected` verifies post-open symlink swap fails closed with `ErrSSTableSymlink`.
  - *Directory Rejection*: `TestSEC007_OpenedObjectIsDirectory_Rejected` verifies directories are rejected wrapping `ErrNotADirectory`.
  - *Non-Regular Rejection*: `TestSEC007_OpenedObjectIsNonRegular_Rejected` verifies named pipes (FIFOs) are rejected before open.
  - *Missing File*: `TestSEC007_MissingFile_OrdinaryNotFound` preserves `os.IsNotExist`.
  - *Permission Failure*: `TestSEC007_PermissionFailure_Preserved` preserves `os.IsPermission`.
  - *Corruption Handlers*: `TestSEC007_MalformedSSTable_CorruptionPreserved` verifies truncated footers and bad magic are rejected cleanly.
   - *Descriptor Leak Prevention*: `TestSEC007_DescriptorLeakPrevention` asserts 0 leaked descriptors across all failure paths. IND-M-002 proof in `sec_ind_m002_test.go` asserts `/dev/fd` count is unchanged across 400 validation-failed opens and 50 post-open-hook-failed opens.
  - *Concurrent Path Mutation*: `TestSEC007_ConcurrentPathMutation` runs concurrent goroutines swapping path between valid file, symlink, and evil payload while multiple readers query, confirming 0 races and 0 bad reads.
  - *Object Identity Matrix*: `TestSEC007_ObjectIdentityMatrix` verifies canonical absolute, relative, redundant separators, leaf symlinks, and intermediate symlinks.
  - *Fuzz Testing*: `FuzzNewTableReader_Paths` executed 1.47M+ iterations with 0 crashes, 0 panics, 0 descriptor leaks.
  - Full repository test suite passes with `-race` (0 data races).
* **Remaining Scope Boundary**:
  - All identified security findings F-001 through F-007 from the Phase 00–06 security audit are now fully remediated and verified.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Reader reads strictly through the pinned descriptor; pathname mutation cannot redirect reads).
  * Performance: **Optimal** (Nanosecond overhead for `Lstat` checks at open time; zero overhead during `Seek` reads).
  * Security: **Optimal** (Eliminated symlink following, intermediate path redirection, and TOCTOU inode substitution vectors).

---

### 43. Boot Discovery Scope & Sequential Replay Decoupling (P07-S01-M01)
* **Limitation**: In `P07-S01-M01`, `DiscoverActiveManifest` establishes the secure boot discovery boundary that inspects the database directory, strictly parses the authoritative `CURRENT` pointer, resolves the active `MANIFEST-%06d` path, and safely opens the active MANIFEST regular file with symlink and TOCTOU defenses, but sequential `VersionEdit` record decoding, level array reconstruction ($L_0..L_6$), missing SSTable file detection (`ErrMissingSSTable`), and WAL replay into MemTable are intentionally excluded and reserved for subsequent micro-phases (`P07-S01-M02` and `P07-S02-M01`).
* **Why It Exists**: Hard scope boundary enforcement and architectural separation of concerns. Startup recovery requires a verified, uncompromised descriptor to the active MANIFEST before state machine replay can begin. Coupling file discovery with record decoding would entangle descriptor lifecycle safety with logical state reconstruction.
* **Impact**: Callers receive a verified `*DiscoveredManifest` with exclusive descriptor ownership (`File *os.File`), physical file size, and authoritative manifest sequence number, but must pass this descriptor to the sequential replay engine (`P07-S01-M02`) to reconstruct `Version` state.
* **Current Mitigation**: Strict canonical parsing via `ReadCurrentManifest` rejects malformed or non-canonical pointers without fallback. Pre-open `os.Lstat` rejects symlinks, directories, and non-regular files; `openFileNoFollow` prevents symlink traversal; and post-open `os.SameFile` verification ensures the descriptor matches the inspected disk inode before and after opening. Failure paths guarantee descriptor closure via `defer`.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Zero fallback on corrupted `CURRENT`; zero state mutation; fail-closed on any ambiguous filesystem state).
  * Performance: **Optimal** (~27.26 µs/op across full OS `lstat`/`open`/`fstat` pipeline; zero heap buffering of MANIFEST contents).
  * Security: **Optimal** (Descriptor identity invariance eliminates TOCTOU substitution attacks).

---

### 44. Sequential VersionEdit Replay Scope & WAL Replay Decoupling (P07-S01-M02)
* **Limitation**: In `P07-S01-M02`, `ReplayManifest` sequentially replays CRC-framed `VersionEdit` records from the pinned MANIFEST descriptor, reconstructs immutable `Version` level arrays ($L_0..L_6$) and monotonic scalars (`NextFileNum`, `LastSeqNum`), and validates that all final referenced SSTable files physically exist on disk as regular files (failing closed with `ErrMissingSSTable` if missing), but scanning `/wal/` for logs newer than the manifest checkpoint and replaying uncommitted WAL records into active MemTable are intentionally excluded and reserved for Sub-Phase 07.2 (`P07-S02-M01`).
* **Why It Exists**: Hard scope boundary enforcement and architectural separation of concerns. Startup recovery establishes durable LSM-tree level structure from the MANIFEST before replaying volatile, uncommitted write log records into memory. Coupling manifest replay with WAL scanning would violate single-responsibility boundaries and create race hazards between version state installation and memtable recovery.
* **Impact**: Callers receive a reconstructed `*ReplayResult` containing an immutable `*Version` (refCount = 1), updated `NextFileNum`, and `LastSeqNum`, but the engine's active MemTable is not populated until WAL recovery runs in `P07-S02-M01`.
* **Current Mitigation**: `ReplayManifest` operates on an isolated in-memory reconstruction builder, ensuring zero state mutation or partial `VersionSet` publication if replay fails. Bounded 8-byte framing checks, untrusted length verification against `MaxVersionEditBytes` before memory allocation, CRC32-IEEE verification, and read-only physical SSTable checks ensure fail-closed crash recovery without altering persistent disk state.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic state reconstruction; strictly enforces non-overlapping key ranges on $L_1..L_6$ and regular-file existence on all referenced SSTables).
  * Performance: **Optimal** (Streams records sequentially with bounded memory; replayed 100 historical edits in 101.2 µs/op and 1,000 edits in 710.1 µs/op).
  * Security: **Optimal** (Bounds untrusted length before allocation; borrowed descriptor eliminates TOCTOU reopening races; zero disk mutation on failure).

---

*End of Known Limitations — To be updated continuously throughout implementation.*



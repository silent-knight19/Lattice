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

### 45. Uncommitted WAL Recovery Scope & Orphaned File GC Decoupling (P07-S02-M01) — REMEDIATED
* **Limitation**: In `P07-S02-M01`, `Engine.RecoverWAL()` discovered and replayed uncommitted WAL records newer than the durable MANIFEST sequence checkpoint (`LastSeqNum`) into a fresh active MemTable with strict duplicate prevention and atomic batch handling, but scanning the database directory for orphaned `.tmp` staging files left by interrupted flushes or compactions was decoupled and reserved for `P07-S02-M02` (`Engine.CleanOrphanedFiles()`).
* **Remediation**: `P07-S02-M02` implemented `Engine.CleanOrphanedFiles()` and `CleanOrphanedFilesDir()`, which scans the database directory for unreferenced crash-window temporary staging files (`.tmp_<name>.sst_<random>`) and safely unlinks them while strictly defending against symlinks, directory masquerades, and path traversal, leaving live persistent files byte-identical.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Exact sequence-aware MemTable reconstruction; zero duplicate replay; fail-closed on corrupt logs).
  * Performance: **Optimal** (~4.2 µs/record replayed, linear scaling, bounded memory allocation).
  * Security: **Optimal** (CRC32 verified before filtering; streaming log consumption prevents DoS buffer exhaustion).

---

### 46. Orphaned Temporary File GC Scope & Leveled Compaction Decoupling (P07-S02-M02)
* **Limitation**: In `P07-S02-M02`, `Engine.CleanOrphanedFiles()` safely purges crash-window temporary staging files from the database directory, completing Phase 07 (Crash Recovery & Integrity Verification). However, background Leveled Compaction ($L_0 \to L_1 \to L_N$), compaction scoring heuristics, overlapping-input file selection, and k-way merge sorting are intentionally excluded and reserved for Phase 08.
* **Why It Exists**: Hard scope boundary enforcement and architectural separation of concerns. Startup recovery establishes durable LSM-tree consistency, restores active MemTable state, and purges staging residue before the database is opened for concurrent operations. Background compaction is a continuous maintenance subsystem that operates concurrently with client read/write workloads in Phase 08.
* **Impact**: Database startup recovery is 100% complete and self-contained; automatic background level compaction begins implementation in Phase 08.
* **Current Mitigation**: Startup recovery runs boot discovery, manifest replay, WAL recovery, and orphan temporary file cleanup in a strictly ordered, idempotent pipeline before client writes begin.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Conservative allowlist deletion; zero false positives; symlinks never followed).
  * Performance: **Optimal** (Sub-millisecond to ~35ms scan time even on 10,000 files; bounded non-recursive directory scan).
  * Security: **Optimal** (Parent directory pinning prevents TOCTOU directory swaps; external victim targets preserved byte-identical).

---

### 47. Phase 07 Security Remediation & Filesystem Boundaries (P07-SEC-REMED)
* **Remediation & Architectural Hardening**:
  Following the exhaustive Phase 07 security and correctness audit, six confirmed security and lifecycle findings (`SEC-P07-01` through `SEC-P07-06`) were remediated:
  1. **Recovery Lifecycle Isolation (`SEC-P07-01`)**: Implemented explicit deterministic four-state lifecycle gate (`NOT_RECOVERING`, `RECOVERING`, `RECOVERED`, `CLOSED`). Prohibits concurrent mutations (`Put`, `Delete`, `Get` return `ErrRecoveryInProgress` while recovering). Rejects recovery calls if the engine already contains live in-memory mutations (`ErrRecoveryInvalidState`), preventing silent state overwrite. Rejects double recovery after successful completion (`ErrRecoveryAlreadyComplete`). Aborts publication cleanly if `Close()` races with recovery, releasing the reconstructed Version without publishing into a closed engine. Preserves `nextSeqNum` as the sequence watermark with `nextSeqNum.Add(1)` allocation.
  2. **Cleaner Descriptor-Relative Deletion (`SEC-P07-02`)**: Replaced pathname-based deletion in orphan cleanup with descriptor-relative `unlinkat` anchored to the opened directory descriptor on Unix-like platforms (Darwin raw syscall 472, Linux `SYS_UNLINKAT`), eliminating parent pathname replacement TOCTOU races. Preserves the full multi-tier allowlist, direct-child check, and symlink rejection layers.
  3. **CURRENT Writer Parent Descriptor Pinning (`SEC-P07-03`)**: Hardened `SetCurrentManifest` to anchor temporary file creation and atomic replacement directly to the validated parent directory file descriptor (`openat`/`createTempAt` and `renameat`). Replaces naive post-rename path checking with descriptor-relative operations.
  4. **Version Reference Ownership in Recovery (`SEC-P07-04`)**: Added `VersionSet.HasCurrent()` to check presence without leaking pinned caller references. Explicitly unrefs the reconstructed Version if `AppendVersion` fails or if recovery is aborted, maintaining exact bijective reference counting.
  5. **MANIFEST Replay Resource Budgets (`SEC-P07-05`)**: Introduced explicit, configurable replay budgets: `MaxManifestReplayBytes` (64 MiB), `MaxManifestReplayRecords` (100,000 edits), and `MaxManifestLiveFiles` (100,000 live files). Bounded header decoding checks remaining stream bytes before allocating payload buffers. Live file count is validated during `applyEdit` before map allocations, preventing memory exhaustion while permitting valid add/delete churn.
  6. **Cleaner Directory-Sync Visibility (`SEC-P07-06`)**: Updated `CleanOrphanReport` with `DirectorySyncFailed` and `SyncError`. If physical unlinks succeed but directory synchronization fails, `FilesCleaned` preserves the accurate deletion count while reporting the durability error explicitly via `report.Error()`.
* **Platform-Dependent Filesystem Guarantees**:
  - *Unix (Darwin / Linux)*: Native descriptor-relative system calls (`unlinkat`, `renameat`, `openat`) provide race-free directory operations anchored to verified file descriptors.
  - *Windows*: Standard Go `os` pathname operations are utilized as fallback. On Windows platforms, directory-relative unlink/rename syscalls are not exposed by the standard Go runtime without external packages; atomic directory pinning is narrower and relies on file locks and pre-open inspection.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Exact sequence watermark semantics preserved; version reference accounting balanced).
  * Performance: **Optimal** (Descriptor-relative syscalls eliminate repeated pathname resolution; zero heap allocation on bounded checks).
  * Security: **Optimal** (TOCTOU parent substitution eliminated on Unix; replay DoS bounded before allocation; lifecycle mutations serialized).

---

### 48. Phase 07 Audit 2 Security Hardening: Metadata, WAL Bounds & SSTable Lifecycle (P07-SEC-REMED-2)
* **Remediation & Architectural Boundaries**:
  Following the second independent security audit of Phase 07, ten confirmed security and correctness findings (`P07-SEC-001` through `P07-SEC-014`) were remediated:
  1. **Missing MANIFEST Fail-Closed (`P07-SEC-001`)**: `Engine.RecoverWAL()` strictly differentiates `ErrCurrentNotFound` (an uninitialized, fresh database directory) from `ErrManifestNotFound` (an existing `CURRENT` pointing to a missing `MANIFEST` file). If `CURRENT` references a missing `MANIFEST`, recovery fails closed with `ErrManifestNotFound` rather than treating the directory as uninitialized or resetting checkpoints to 0.
  2. **Orphan Finalized SSTable Crash-Window & File-Number Safety (`P07-SEC-004`, `P07-SEC-006`)**: During recovery, `Engine` initializes its `nextFileNum` allocator to the maximum of: the authoritative MANIFEST watermark, the highest numbered physical `.sst` file on disk + 1, and 1. This guarantees that uncommitted SSTables written and linked prior to a crash can never collide with subsequent TableWriter allocations.
  3. **Cleaner Startup Denial of Service Prevention (`P07-SEC-005`)**: The orphan file cleaner strictly refuses unsafe entries (symlinks, directories, permission-denied files) without deleting them, but records refusal diagnostics in `lastCleanerReport` instead of converting individual safe refusals into fatal boot errors. Startup fails only on critical filesystem-level failures (e.g. inability to open the DB directory).
  4. **WAL Recovery Bounded Batch Buffering (`P07-SEC-007`)**: Uncommitted batch records between `BATCH_START` and `BATCH_COMMIT` are buffered with strict pre-allocation limits: `MaxRecoveryBatchRecords = 10,000` records and `MaxRecoveryBatchBytes = 64 MiB`. Memory accounting includes key, value, and overhead bytes before append, failing closed with `ErrRecoveryBatchLimitExceeded` without publishing partial batches.
  5. **SSTable Physical File Size Validation (`P07-SEC-008`)**: During MANIFEST replay, all referenced `.sst` files are verified via `os.Lstat` to ensure their physical disk size exactly equals `FileMetadata.FileSize`. Mismatches fail closed with `ErrSSTableSizeMismatch`.
  6. **MANIFEST Scalar Regression Detection (`P07-SEC-011`)**: `versionBuilder.applyEdit()` strictly enforces monotonicity on `NextFileNum` and `LastSeqNum`. Decreasing scalar values in subsequent `VersionEdit` records are rejected as corruption with `ErrCorruptedVersionEdit`.
  7. **ReplayError Diagnostic Provenance (`P07-SEC-012`)**: Post-replay validation errors (such as missing SSTables, file size mismatches, or level range overlaps) accurately report the originating `RecordIndex` and stream `Offset` of the `AddFile` edit that introduced the file.
  8. **Initial WAL Segment ID Validation (`P07-SEC-013`)**: `ValidateSegmentContinuity()` requires that non-empty WAL segment chains begin with segment ID 1 (`ids[0] == 1`). Chains with missing initial segments fail closed with `SegmentGapError`.
  9. **Sequence Number Contract Alignment (`P07-SEC-014`)**: Documentation has been aligned with runtime semantics: `nextSeqNum` represents the sequence watermark, and runtime write mutations allocate sequence numbers via `nextSeqNum.Add(1)`.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Missing authoritative metadata fails closed; file allocations are collision-free; WAL batches are strictly bounded).
  * Performance: **Optimal** (Pre-allocation bounds prevent heap exhaustion; O(1) atomic file-number allocation).
  * Security: **Optimal** (Adversarial symlink or permission squats cannot cause denial of service; corrupted metadata is immediately rejected).

---

### 49. SSTable Sequential Streaming Iterator Traversal & Merge Decoupling (P08-S01-M02)
* **Limitation & Architectural Boundaries**:
  In `P08-S01-M02`:
  1. *Single-Table Traversal Only*: `TableIterator` operates strictly over a single physical SSTable file. Multi-file min-heap k-way merge sorting (`P08-S02-M01`) is intentionally decoupled.
  2. *Tombstone Preservation*: Tombstones (`OpTypeDelete`) are yielded verbatim as standard records with `Value() == nil`. Tombstone safety checking and elimination across deeper levels is deferred to `P08-S02-M02`.
  3. *Zero Storage Modifications*: The iterator performs purely read-only streaming traversal over immutable SSTable files. It creates no new SSTables, writes no MANIFEST edits, installs no new Versions, and deletes no files.
  4. *Ownership Model*: `NewTableIterator` borrows an existing `*TableReader` (closing the iterator releases block buffers without closing the underlying reader descriptor). `OpenTableIterator` opens and owns its `*TableReader`, guaranteeing the underlying file descriptor is closed when the iterator is closed.
  5. *Key/Value Lifetime Contract*: `Key()` returns an owned defensive copy (`currKey.Clone()`) and `Value()` returns an owned copy, ensuring callers, priority queues, and concurrent consumers never experience buffer invalidation or mutational aliasing across subsequent `Next()` steps.
* **Why It Exists**:
  Single-responsibility micro-phase discipline. Implementing a rock-solid, memory-bounded, streaming single-table iterator before building the k-way merge heap isolates block decoding and corruption detection from merge scheduling.
* **Impact**:
  Streaming memory boundedness is guaranteed ($O(1)$ uncompressed block in RAM). Corruption, truncation, CRC mismatch, and out-of-order keys fail closed deterministically without polluting subsequent compaction stages.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Monotonic canonical `binary.CompareInternalKey` ordering verified intra-block and inter-block; corruption never produces false EOF).
  * Performance: **Optimal** (~95 ns per record scan, ~8.2M records/sec throughput on Apple M4, zero heap allocations for internal block tracking).
  * Security: **Optimal** (Descriptor-centric ReadAt inherited from hardened TableReader; no symlink or path reopening vulnerabilities).

---

### 50. Min-Heap K-Way Merge Iterator & Compaction Scope Boundaries (P08-S02-M01)
* **Limitation & Architectural Boundaries**:
  In `P08-S02-M01`:
  1. *Merge-Layer Deduplication vs Tombstone Purging Boundary*: `MergingIterator` deduplicates older revisions of identical `UserKey`s (emitting the newest revision with highest `SeqNum`), but emits winning tombstones (`OpTypeDelete`) verbatim with `Value() == nil`. It does NOT determine whether a tombstone is globally safe to drop across deeper LSM levels; tombstone purge safety is strictly isolated to `P08-S02-M02` (`CanDropTombstone`).
  2. *Zero Output Generation*: `MergingIterator` is a streaming record consumer and producer. It writes no new SSTables to disk, generates no index blocks, and flushes no data blocks (`P08-S03-M01`).
  3. *Zero Metadata / Manifest Mutations*: No `VersionEdit` records are created or committed, no Manifest log writes occur, and no active `Version` or `VersionSet` state is modified (`P08-S03-M02`).
  4. *Deterministic Tie-Breaking*: When two child iterators present identical canonical `InternalKey`s, the record from the lower child index wins deterministically, ensuring repeatable heap ordering independent of pointer addresses or memory layout.
  5. *Streaming Memory Boundedness*: The heap retains at most one active record per live child iterator ($O(N)$ for $N$ child streams). No unbounded record accumulation occurs.
* **Why It Exists**:
  Modular separation of concerns in LSM storage engine compaction. Decoupling multi-stream k-way heap ordering from physical SSTable writing and tombstone eradication ensures the merge algorithm can be exhaustively tested with fuzzing and differential verification in isolation.
* **Impact**:
  Streaming memory boundedness and strictly increasing canonical key ordering are guaranteed. Corrupted child streams fail closed immediately without masking errors as EOF.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against independent differential scanner over 2,500 randomized runs and 437k+ fuzz executions; 0 crashes/violations).
  * Performance: **Optimal** (~848 ns per emitted record; 32-way merge of 320,000 records completes in ~31 ms / ~10.3M records/sec).
  * Security: **Optimal** (Safe child ownership and idempotent Close; zero filesystem mutation or descriptor leaks).

---

### 51. Tombstone Purge Safety Invariant Enforcer & Compaction Execution Decoupling (P08-S02-M02)
* **Limitation & Architectural Boundaries**:
  In `P08-S02-M02`:
  1. *Safety Decision vs Physical Output Generation*: `Compactor.CanDropTombstone` is strictly a pure read-only boolean safety gate. It does NOT write output SSTables, omit records from physical files, or rewrite disk blocks (`P08-S03-M01`).
  2. *Exact vs Conservative Semantics*: In default metadata-only mode (Model A), any candidate file in a deeper level whose user-key range overlaps `userKey` conservatively prevents dropping the tombstone (`false`). While this may occasionally preserve a tombstone whose key happens to fall into a range hole between records in a deeper SSTable, it guarantees zero I/O overhead and provable safety against ghost-key resurrection. In Model C hybrid mode, configuring a `TableOpener` verifies physical presence via targeted index seek.
  3. *Zero Metadata / Manifest Mutations*: No `VersionEdit` records are committed, no Manifest log writes occur, and no active `VersionSet` pointers are updated (`P08-S03-M02`).
  4. *Atomic Version Refcounting*: `Compactor` pins its `*version.Version` snapshot via `v.TryRef()` upon creation and releases it via `v.Unref()` upon `Close()`. The pinned Version cannot be reclaimed or invalidated while `Compactor` is active.
* **Why It Exists**:
  Modular separation of concerns. Proving the tombstone purge safety invariant in isolation ensures that ghost-key resurrection risks are thoroughly eliminated and validated by differential tests and fuzzing before coupling to the physical compaction output generation engine.
* **Impact**:
  Guaranteed absence of ghost-key resurrection. If key existence in deeper levels cannot be disproven with mathematical certainty, the tombstone is preserved.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against independent differential scanner over 2,500 randomized runs and 898k+ fuzz executions; 0 crashes/violations).
  * Performance: **Optimal** (~3.7 ns for bottom-level targets, ~1.28 µs for metadata scans, zero allocations on hot paths).
  * Security: **Optimal** (Version refcount pinning prevents use-after-free; descriptor-safe TableReader integration).


---

### 52. Compaction Output SSTable Generation, 2 MiB Partitioning, & Manifest Visibility Boundaries (P08-S03-M01)
* **Limitation & Architectural Boundaries**:
  In `P08-S03-M01`:
  1. *Physical Output vs Manifest Visibility*: `BuildCompactionOutput` and `Compactor.BuildOutput` safely write, sync, hard-link (`os.Link`), and validate completed SSTables on disk. However, these generated `.sst` files are NOT yet part of the active database `Version` or visible to reader queries until the subsequent micro-phase (`P08-S03-M02`) records the corresponding `VersionEdit` in the active `MANIFEST` log and advances the active `VersionSet`.
  2. *2 MiB Target Partitioning & Boundary Policy*: The target partition size (2 MiB = 2,097,152 bytes) is an architectural partitioning target, not a strict mathematical upper bound. When an active SSTable is non-empty (`EntryCount > 0`), it is finalized before adding an incoming record if the current writer-visible size estimate (`TableWriter.EstimatedSize()`) plus estimated record overhead exceeds 2 MiB.
  3. *Oversized Single Record Policy*: A single record whose serialized representation exceeds 2 MiB by itself is written into an empty SSTable and finalized immediately upon the arrival of the next record. This guarantees continuous forward progress, prevents infinite partitioning loops, and ensures single records are never split.
  4. *Tombstone Omission Policy*: Physical omission of tombstones (`OpTypeDelete`) is strictly governed by `TombstoneSafetyChecker.CanDropTombstone(userKey, targetLevel)`. A tombstone is physically omitted from compaction output if and only if it is mathematically proven that no older revision of the user key exists in levels deeper than the compaction target level. Unsafe tombstones and non-tombstones are preserved verbatim in exact canonical order (`UserKey ASC, SeqNum DESC, OpType DESC`).
  5. *Zero Obsolete File Deletion*: Input SSTables consumed by the compaction merge iterator are not unlinked or deleted from disk in this micro-phase. Obsolete file deletion and unpinning are deferred to `P08-S03-M02` and `P08-S03-M03`.
  6. *Fail-Closed Cleanup*: If an error occurs during writing, block flush, or post-finalization `TableReader` validation, all open temporary staging files and finalized SSTables created during that run are unlinked from disk before returning the error, preventing orphaned file accumulation.
* **Why It Exists**:
  Systems engineering micro-phase discipline. Decoupling physical SSTable creation, atomic staging, and 2 MiB boundary partitioning from manifest commit and file reclamation ensures that filesystem failure modes, key ordering invariants, and tombstone omission paths can be verified in isolation.
* **Impact**:
  Guaranteed deterministic partitioning, zero record loss, physical omission of droppable tombstones, and complete verification via post-write `TableReader` round-trips before manifest installation.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against independent differential scanner across 1,000 randomized iterations, 22 acceptance tests A-T, and native Go fuzz campaign; 0 mismatches/crashes).
  * Performance: **Optimal** (~77 ms for 100K records / ~140 MB/s; ~167 MB/s on large partitioned workloads; ~480 MB/s on 90% tombstone omission workloads).
  * Security: **Optimal** (Atomic staging `.tmp_*`, hard-link `link(2)` without TOCTOU rename overwrites, strict file mode enforcement 0600, monotonic file number allocation).

### 53. Atomic Manifest Commit, Version Installation, & Obsolete SSTable Reclamation Boundaries (P08-S03-M02)
* **Limitation & Architectural Boundaries**:
  In `P08-S03-M02`:
  1. *LogAndApply Lifecycle & Ordering Invariant*: State transitions follow the strict deterministic ordering:
     `validate edit -> derive next Version -> persist to MANIFEST -> hardware durability barrier (fdatasync) -> atomic publication to VersionSet -> release superseded Version -> safe obsolete SSTable cleanup`.
     A successful return guarantees that the edit has achieved physical non-volatile durability on disk before in-memory publication occurs.
  2. *Fail-Closed Manifest Durability Barrier*: If appending or synchronizing the manifest record fails, the active ManifestWriter enters an unrecoverable poisoned state, the uninstalled candidate Version snapshot is safely discarded, and the live database `Current` pointer remains completely unmodified on the preceding Version.
  3. *Concurrency & Lock Decoupling*: Commit state transitions are strictly serialized by `vs.applyMu` to eliminate stale-base updates or lost concurrent edits. Readers acquiring pinned snapshots via `vs.Current()` or `vs.ActiveVersions()` hold `vs.mu.RLock()` for pointer copying and are never blocked across slow disk I/O operations.
  4. *Reference-Counted Obsolete File Deletion*: Physical SSTables deleted by committed `VersionEdit`s are NOT immediately unlinked if older `Version` snapshots are pinned by active reader queries. Obsolete SSTables are safely reclaimed if and only if no Version in the active chain references them (upon `Version.refCount` transitioning from 1 to 0 during `finalize()`).
  5. *Non-Rollback Cleanup Semantics*: A failed obsolete-file unlink operation does not roll back or corrupt an already-published `Version`. The database's logical snapshot remains committed and valid, while cleanup failures are recorded as diagnostic errors and retained for subsequent reclamation passes.
  6. *Recovery Parity Guarantee*: Sequential MANIFEST replay via `ReplayManifest` reconstructs the exact same level hierarchy, file metadata descriptors, and monotonic scalar watermarks as normal runtime `LogAndApply`.
  7. *Subsystem Scope Boundaries*: This micro-phase implements atomic manifest commit and obsolete file reclamation. Background compaction worker scheduling, automatic compaction triggering, block cache integration (Phase 09), and top-level engine orchestration (Phase 10) are deferred to subsequent phases.
* **Why It Exists**:
  Core ACID atomicity and durability contract for LSM-tree metadata. Guaranteeing that in-memory version state never advances beyond durable on-disk manifest state ensures that process crashes, power failures, or I/O faults can never cause recovery divergence.
* **Impact**:
  Guaranteed durability, serialized atomic version transitions, zero stale-base corruption, zero premature file deletion under reader concurrency, and 100% replay parity.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against Acceptance Matrix A-V, 2,000-iteration randomized differential test suite vs independent reference model, and native Go fuzz target `FuzzLogAndApply`; 0 mismatches/crashes).
  * Performance: **Optimal** (~13.3 µs in-memory version derivation; ~3.7 ms per durable manifest sync; zero reader lock contention during manifest I/O).
  * Security: **Optimal** (Path traversal defense, symlink rejection before unlinking, parent directory inode pinning, monotonic watermarks, fail-closed poison state machine).

### 54. LRU Block Cache Shard Defensive Copying and Single-Shard Boundary (P09-S01-M01)
* **Limitation & Architectural Boundaries**:
  In `P09-S01-M01`:
  1. *Defensive Buffer Copying on Ingest and Retrieval*: `LRUShard.Put` and `LRUShard.Get` defensively clone block slices using `bytes.Clone`. For a 4 KB data block, `Get` on hit incurs one heap allocation (`4096 B/op`, ~336 ns/op).
  2. *Single-Shard Contention Ceiling*: `LRUShard` manages recency and map lookup under an exclusive `sync.Mutex`. While optimal for low-to-medium concurrency, high-core concurrent readers hitting the same shard will experience lock contention until 16-way sharding is introduced in `P09-S01-M02`.
  3. *Unconnected Subsystem Boundary*: `LRUShard` is implemented as an independent storage building block in `internal/cache/`. Integration with `TableReader.Seek` and `Engine.Get` remains deferred to Phase 10.
* **Why It Exists**:
  Security-first memory isolation. In Go, returning raw `[]byte` slice pointers from an internal cache node creates catastrophic cross-request data corruption if a caller mutates the returned slice. Defensive copying guarantees 100% data integrity and eliminates aliasing vulnerabilities without manual C-style reference counting.
* **Impact**:
  Guaranteed panic-free, thread-safe LRU caching with zero data corruption. Read hits allocate one buffer copy; cache misses execute with zero heap allocations (0 B/op, ~8.5 ns/op).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against Acceptance Matrix A-S, 15,000-operation differential test suite vs independent slice-based reference model, and 1.89M+ native Go fuzz executions with zero invariant violations).
  * Performance: **Optimal for single shard** (~336 ns/op hit, ~8.5 ns/op miss, ~332 ns/op update, ~344 ns/op eviction).
  * Scalability: **Bounded by single mutex** (16-way sharded partitioning with 64-byte hardware cache-line padding is implemented in `P09-S01-M02`).

### 55. 16-Way Sharded Cache Partitioning, Hardware Cache-Line Padding, and Shard-Local LRU Recency Boundaries (P09-S01-M02)
* **Limitation & Architectural Boundaries**:
  In `P09-S01-M02`:
  1. *Shard-Local vs Global Recency Ordering*: The sharded cache routes keys deterministically across 16 independent `LRUShard` instances. Consequently, LRU eviction and recency order are local to each shard. There is no global recency order across all 16 shards (i.e. an entry accessed in shard 0 does not update the relative recency of entries in shard 1). This is standard across production LSM storage engines (LevelDB, RocksDB, Pebble) to avoid global lock contention.
  2. *Model A Capacity Distribution & Integer Remainder*: Global cache capacity $C$ is distributed evenly across 16 shards (`base = C / 16`). Non-divisible capacities distribute the remainder `rem = C % 16` by adding 1 unit of capacity to the first `rem` shards (shards 0 through `rem - 1`). The sum of shard capacities is strictly equal to $C$, preventing memory amplification.
  3. *Hardware Cache-Line Padding (128-Byte Stride)*: Shards are embedded in a `paddedShard` struct with 48 bytes of trailing padding, creating a stride of 128 bytes ($2 \times 64\text{B}$). This guarantees that adjacent shard mutexes can never share a single 64-byte hardware cache line, eliminating CPU cache-line bouncing and false sharing during multi-core concurrent reads and writes.
  4. *Zero Global Lock on Hot Path*: Read and write operations (`Get`, `Put`, `Peek`, `Contains`, `Remove`) compute the target shard index via `Murmur3_128` and lock only that shard. Aggregate methods (`Len`, `Capacity`, `Clear`) touch shards sequentially without cross-shard nested locking, preventing deadlock cycles.
  5. *Subsystem Integration*: Integrated with `TableReader.ReadBlock` in Sub-Phase 09.2 (`P09-S01-M03`); top-level engine-wide cache coordination across SSTables remains scheduled for Phase 10.
* **Why It Exists**:
  Architectural trade-off prioritizing multi-core scalability over exact global recency. Maintaining a single global LRU list across 16 shards would require a global coordination mutex or complex distributed coordination, which would defeat the primary purpose of sharding.
* **Impact**:
  Near-linear multi-core read scaling with zero lock contention across different shards and zero false sharing between adjacent shard mutexes.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against comprehensive acceptance matrix, 15,000-operation differential test suite vs independent reference model, and 3.49M+ native Go fuzz executions; 0 mismatches/crashes).
  * Performance: **Optimal** (~498 ns/op under 64 concurrent workers vs ~785 ns/op on single shard, a 36.5% latency reduction; single-threaded hit ~341 ns/op, miss ~12.6 ns/op).
  * Scalability: **High** (Scales smoothly to 64+ concurrent goroutines with zero data races).

---

### 56. SSTable Read Path Block Cache Integration, Infallible Bounds Pre-Validation, and Canonical Data Block Caching (P09-S01-M03)
* **Limitation & Architectural Boundaries**:
  In `P09-S01-M03`:
  1. *Handle Bounds Pre-Validation Invariant*: Candidate `BlockHandle` bounds (`Size > 0`, `Size <= MaxDataBlockSize`, `Offset + Size <= FileSize - FooterSize`) are validated strictly BEFORE inspecting `BlockCache`. A cache hit can NEVER bypass structural handle bounds validation.
  2. *Canonical Data Block Caching Unit*: The cache unit is the complete validated data block payload (`[entries || restart offsets || restart count || CRC32]`). Because Lattice data block compression is prefix delta encoding, caching the validated block payload preserves zero-copy restart point binary searching while avoiding redundant physical disk reads and CRC32 recalculations on subsequent hits.
  3. *Fail-Closed Non-Caching on Corruption*: Data blocks failing minimum trailer bounds (>= 8 bytes), CRC32-IEEE checksum verification, or restart array validation return `ChecksumMismatchError` or `DataBlockCorruptedError` immediately and are NEVER inserted into the cache. The cache stores exclusively proven, valid blocks.
  4. *Immutable SSTable Lifetime Assumption*: Cache entries are keyed by physical identity `(FileNum, BlockOffset)`. Because SSTables in Lattice are immutable once published, cached blocks remain valid for the lifetime of that physical table. Obsolete SSTables unlinked by compaction remain harmlessly in cache until naturally evicted by shard LRU policy.
  5. *Zero Single-Flight Request Coalescing*: Concurrent cold misses for the same block execute concurrent disk reads before populating the cache. Single-flight request coalescing is intentionally out-of-scope for this micro-phase.
  6. *Engine-Wide Integration Boundary*: `TableReader.ReadBlock` is fully integrated with `BlockCache`. Wiring a shared `ShardedBlockCache` into `Engine` options across all active `TableReader` instances across compaction and flush pipelines is scheduled for Phase 10.
* **Why It Exists**:
  Security-first read-path integration. Enforcing bounds checks prior to cache queries prevents bounds-bypass attacks, and caching only post-checksum-validated blocks guarantees corrupt disk state never masquerades as valid data in RAM.
* **Impact**:
  Warm reads avoid physical disk reads entirely (178.1x read reduction in differential tests), `ReadBlock` latency drops from 345.7 ns to 55.98 ns (6.2x speedup), `Seek` drops from 532.4 ns to 225.1 ns (2.4x speedup), and 64-worker concurrent read throughput scales by 7.5x (160.0 ns/op vs 1193 ns/op).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Verified against 10-scenario acceptance matrix, 5,000-op differential suite vs direct reader with 0 mismatches, and 1.85M+ native Go fuzz executions).
  * Performance: **Optimal** (6.2x faster `ReadBlock`, 2.4x faster `Seek`, 7.5x faster concurrent reads).
  * Security: **Optimal** (Handle bounds enforced before cache access; corrupted blocks fail closed and never enter cache; cross-SSTable key isolation enforced).

---

### 57. Unified Engine CRUD Scope, Transient Reader Model, and Deferred Flush/Backpressure/Shutdown (P10-S01-M01)
* **Limitation & Architectural Boundaries**:
  In `P10-S01-M01`:
  1. *Thin Orchestration Only*: `Engine` owns `activeMem`/`immMems` bookkeeping, `nextSeqNum`/`nextFileNum` watermarks, lifecycle state, and backpressure accounting. It coordinates WAL, `VersionSet`, transient `TableReader`s, and one shared `ShardedCache`; it maintains no `[]SSTable`, `map[key]value`, second WAL/cache/`VersionSet`, and performs no raw file I/O, manifest writes, or compaction output construction.
  2. *WAL-Optional Durability*: `Put`/`Delete` allocate one sequence, `AppendSync` when a WAL is configured (success only after WAL barrier + MemTable insert), else memory-only for unit tests. `Open()` recovers via existing `RecoverWAL` before opening `RotatingWriter` to preserve quiescence.
  3. *Tombstone-Aware Lookup*: `Get` uses `Iterator.Seek` OpType (not `SearchConcurrent`/`Seek` alone) so a tombstone at any layer stops the search; L0 is searched newest `FileNum` first, L1.. via decoded key-range prune. Storage errors never map to `NotFound`.
  4. *Transient Readers*: Persistent reads open a short-lived `TableReader` per candidate file with the shared cache and close it before returning. No persistent reader pool/registry is introduced; concurrent `Get`s never share a reader lifetime.
  5. *Explicitly Deferred*: Background flush loop + `imm` drain (M02), L0-count pacing/stall (M03), and flush-on-close/compaction-drain/manifest-sync shutdown (M04) are not implemented. `Close()` only freezes, closes WAL, and closes backpressure. Dual `Engine`/`VersionSet` watermarks and WAL-less bulk-test mode (50k acceptance uses SSTables without per-op fsync; durability proven by small WAL tests) remain until M02 reconciles flush allocation.
* **Why It Exists**:
  Single-micro-phase discipline: prove durable CRUD + versioned reads + cache integration without building background pipelines, stall controllers, or shutdown coordinators.
* **Impact**:
  50k mixed CRUD verified (20k PUT / 20k GET / 10k DELETE, 0 mismatches); WAL durability + reopen/update/delete-across-restart verified separately; transient opens cost one footer+index read per candidate file per miss (acceptable for M01 correctness-first scope).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Tombstone shadowing, L0 newest-wins, corruption-not-NotFound, defensive copies, Version pin/unpin verified under `-race` + 4.2M fuzz execs).
  * Performance: **Adequate** (Bulk 50k in ~1.6s WAL-less; WAL-backed writes bounded by fsync; no global Engine lock across persistent I/O on reads).
  * Security: **Optimal** (No WAL-success-before-sync, no tombstone bypass, no released-Version use, no shared-reader close race, no cache-bounds bypass, no buffer aliasing, no corruption masking).

---

### 58. Asynchronous Flusher Scope, Single-Worker Model, and Deferred Pacing/Shutdown (P10-S01-M02)
* **Limitation & Architectural Boundaries**:
  In `P10-S01-M02`:
  1. *Single Worker, FIFO Queue*: One `flushLoop` goroutine drains `immMems` oldest-first with a coalesced `flushCh` (cap 1, no per-trigger goroutine, no busy poll). Multiple rotations append (no overwrite); the worker never holds `Engine.mu` during `TableWriter`/`Finish`/`LogAndApply` I/O.
  2. *Threshold*: Single authoritative `memtable.MaxMemTableSize` (64 MiB) via `ByteSize()`; rotation when `>=` threshold after success plus existing `ErrMemTableFull` rotate-and-retry. `SetFlushThresholdForTesting` exists only for fast tests; the 200MB acceptance uses the real 64MB threshold.
  3. *Manifest Durability*: `ensureManifestWriter` creates/opens the active MANIFEST and points `VersionSet` at it; each flush publishes `AddFile(L0)` + `NextFileNum(fileNum+1)` + `LastSeqNum(maxFlushedSeq)` via `LogAndApply` (manifest sync barrier). WAL segments are intentionally retained (no truncation/GC); recovery skips `Seq <= checkpoint` and serves flushed keys from L0.
  4. *Failure Retention*: Writer/`Finish`/`Add`/`LogAndApply` failures retain the imm, record `FlushError`, publish nothing, and break to avoid tight retry (next rotation signal retries oldest-first). Post-`Finish` publish failures may leave an orphan SSTable on disk (ignored by replay, FileNum bumped past it).
  5. *Minimal Close*: `Close()` signals `stopCh`, waits for in-flight `flushOne` (no full drain/flush-on-close), then freezes and closes WAL/manifest/backpressure. Remaining imm stays memory-readable and WAL-durable.
  6. *Explicitly Deferred*: L0-count pacing/stall (M03) and full shutdown sequencing with compaction coordination (M04) are not implemented; unbounded imm growth under sustained flush failure is bounded only by future backpressure.
* **Why It Exists**:
  Correctness-first background persistence: prove async rotation/flush/publication/retirement without building stall controllers or shutdown coordinators.
* **Impact**:
  200MB ingest yields 3 L0 files with full 50k-key validation; blocked-flush test proves fresh writes complete while I/O is stalled; empty imm flushes publish nothing.
* **Dimensional Impact**:
  * Correctness: **Optimal** (No visibility gap, seq/tombstone preservation, unique FileNums, orphan-safe failures verified under `-race` + fuzz).
  * Performance: **Adequate** (200MB in ~54s with real SSTable+manifest I/O; hot-path threshold check O(1); dormant worker when idle).
  * Security: **Optimal** (No lock-held I/O, no imm overwrite, no double publish, no FileNum reuse, no silent background errors, no WAL premature disposal).

---

### 59. L0 Write Pacing and Stall Scope, Polling Relief, and Deferred Shutdown (P10-S01-M03)
* **Limitation & Architectural Boundaries**:
  In `P10-S01-M03`:
  1. *Authoritative Pressure*: Pacing reads `VersionSet.Current().NumFiles(0)` per gate (pinned only for the read, never across sleeps); no duplicate L0 counter exists. `SetL0CountOverrideForTesting` is test-only.
  2. *Exact Thresholds*: L0 ≤ 8 normal (no timer); 9→1ms, 10→5ms, 11→15ms, 12→30ms single pacing sleep; L0 > 12 stalls in bounded 20ms polls until L0 ≤ 12 (release hysteresis = roadmap boundary; no extra bands). Constants only: no overflow, no unbounded sleep.
  3. *Placement*: Gate runs after key/value validation, before memory backpressure/`Acquire`, sequence allocation, WAL, and MemTable work — stalled writers reserve no seqs and create no internal copies (caller goroutine waits; no pending-write queue, no per-writer goroutine). `Get` is never gated.
  4. *Relief*: The controller never deletes files; compaction (or VersionSet publication as compaction would publish) reduces L0 and writers observe it on the next poll. No compaction kick, no forced compaction, no second compaction system.
  5. *Lifecycle*: Stall/pacing selects honor ctx cancellation, `stopCh`, and `closed`; `Close()` terminates waiters with `ErrWriterClosed` (no M04 drain). Stall itself returns no new error type.
  6. *Explicitly Deferred*: Full graceful shutdown sequencing with compaction coordination (M04). Flush-failure state (`FlushError`) is orthogonal: pacing neither masks nor clears it.
* **Why It Exists**:
  Self-protection under L0 pressure with deterministic, reviewable policy: progressive latency instead of disk-exhaustion crash, without lock-held sleeps or waiter amplification.
* **Impact**:
  No-pressure Put ≈ 616ns/op; L0=12 paced ≈ 31ms/op; stall holds caller goroutines in 20ms slices (no spin, no retained internal buffers beyond call args).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Boundaries 8/12/13, monotonic pacing, atomic same-key/delete application after relief, real-L0 + relief acceptance verified under `-race` + 26.7k fuzz execs).
  * Performance: **Adequate** (Zero timer at ≤8; one timer per paced write; stall polls bounded).
  * Security: **Optimal** (No lock retention while waiting, no unbounded queue/goroutines, no delay amplification, reads unaffected, close/cancel terminate waits).

---

### 60. Shutdown Drain Scope, Single-Winner Close, and External-Compaction Contract (P10-S01-M04)
* **Limitation & Architectural Boundaries**:
  In `P10-S01-M04`:
  1. *Single-Winner Close*: First `Close()` drives RUNNING→CLOSING→CLOSED; concurrent closers wait on `closeDone` and receive the identical remembered error (nil on success). No double channel close, no double final flush, no duplicate publication.
  2. *Drain, Not Background*: After joining the exited worker, `Close` rotates a non-empty active table and flushes every queued generation oldest-first synchronously through `flushOne` (original seqs, same allocator, `LogAndApply`). No Engine mutex is held across this I/O. Engines whose worker never started (memory-only / never Opened) skip the drain, preserving prior behavior.
  3. *Failure Retention*: A failed generation stops the drain with the error recorded (`FlushError`) and state retained; resources are still all closed (best-effort) and the terminal error is remembered. WAL is never truncated by shutdown, so a fresh Engine recovers every accepted mutation past a failed Close.
  4. *External Compaction*: No Engine-owned compactor exists. In-flight external `LogAndApply` racing `Close` is serialized by existing `applyMu`/manifest locks (fails closed either way; never panics or corrupts). Callers must quiesce external publishers before `Close` for guaranteed inclusion; concurrent Engine-owned publishing is impossible by construction (worker joined before drain).
  5. *Reads Stay Open*: `Get` is never gated by lifecycle (established M01 contract); after a successful drain it serves from published SSTables.
  6. *Cache*: The shared block cache is left untouched (memory-only; may be shared outside Engine).
* **Why It Exists**:
  Deterministic end-of-life: every accepted write ends WAL-durable plus SSTable-published with a synced manifest, with no leaked goroutines, descriptors, or retained MemTables, and a database reopenable by a fresh Engine.
* **Impact**:
  Successful Close leaves queue empty, one terminal error slot, zero Engine goroutines; Open/Close ×10 stress shows no goroutine growth and exact cumulative state.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Drain ordering, tombstone/update preservation across reopen, failure-state recovery verified under `-race` + lifecycle fuzz).
  * Performance: **Adequate** (Empty Open+Close ≈12ms dominated by creation fsyncs; 100-key drain adds ≈0.1ms).
  * Security: **Optimal** (No close-under-worker, no send-on-closed channel, no lost durability on failed Close, no immortal stalled writers).

---

### 61. Wire Protocol Codec Framing & Storage Engine Boundary (P11-S01-M01)
* **Limitation & Architectural Boundaries**:
  In `P11-S01-M01`:
  1. *Pure Framing Codec*: Implements the length-prefixed binary wire protocol foundation (`internal/transport`), 18-byte fixed header (`0x4C415454`), 4-byte CRC32-IEEE checksum trailer, and typed Request/Response codec without network listeners or socket management (scheduled for `P11-S01-M02`).
  2. *Pre-Allocation Frame Bounding*: Strict $5\text{MB}$ payload ceiling enforced upon header decode *before* memory allocation. Claims $>5\text{MB}$ fail closed with `ErrFrameTooLarge` and zero payload allocation.
  3. *Unspecified Wire Contract Resolution*:
     - `OP_PUT`: Encoded as `[ KeyLen (2B uint16 Big-Endian) | Key (KeyLen B) | Value (remainder B) ]`. Eliminates redundant value length headers and guarantees consistency with total payload length.
     - `OP_GET`, `OP_DELETE`, `OP_EXISTS`: Encoded as `[ Key (PayloadLength B) ]` directly utilizing the frame payload length.
     - `OP_BATCH`: Encoded as `[ Count (4B uint32) | Entries... ]` with entries storing `[ OpType (1B) | KeyLen (2B) | Key | ValLen (4B) | Val ]`. Bounded to $\le 1024$ ops and $\le 5\text{MB}$. Storage-level atomic multi-operation execution remains dependent on a future Engine `WriteBatch` API.
     - `OP_STATS`: Validated with zero payload; unexpected bytes rejected.
     - `Response`: Symmetrically framed with 18-byte header where byte 5 is `StatusCode` (`StatusOk=0x00`, `StatusKeyNotFound=0x01`, `StatusError=0x02`, `StatusInvalidRequest=0x03`, `StatusThrottled=0x04`, `StatusServerClosed=0x05`), echoing `SeqID` and `OpCode` with a 4-byte CRC32 trailer. Success GET carries raw value; EXISTS carries 1-byte boolean; failures carry diagnostic message string.
  4. *Buffer Ownership*: `DecodeRequest` and `DecodeResponse` strictly return independent, defensive slice copies of keys, values, and batches. Source frame buffers may be safely recycled without corrupting decoded structs.
* **Why It Exists**:
  Separates transport serialization and adversarial framing defense from socket lifecycle and storage engine mechanics.
* **Impact**:
  Decouples the wire codec completely from storage engine internals; guarantees bounded allocations under malicious payloads; eliminates slice aliasing races.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Binary transparency, exact round-trip, bit-flip detection, stream fragmentation safety verified under `-race` and fuzzing).
  * Performance: **Optimal** (Zero-allocation header decode and incremental CRC32 update; single contiguous write framing).
  * Security: **Optimal** (Pre-allocation $5\text{MB}$ ceiling, integer overflow checks, fail-closed unknown opcodes, defensive buffer copies).

### 62. TCP Server Connection Lifecycle & Engine Request Dispatch (P11-S01-M02) — HARDENED
* **Limitation & Architectural Boundaries**:
  In `P11-S01-M02` and Post-Audit Hardening:
  1. *Plaintext Transport Remote Exposure Prevention (SEC-P11-001)*: The TCP server runs unauthenticated plaintext binary wire traffic. To prevent accidental exposure on untrusted networks, the server strictly restricts binding to loopback addresses (`127.0.0.1`, `localhost`, `::1`) by default. Any attempt to bind to a wildcard (`0.0.0.0`, `:port`) or public IP without explicit opt-in fails immediately with `ErrInsecureTransport`. Setting `ServerConfig.InsecureTransport = true` is required to bind non-loopback interfaces (e.g. for container networking behind TLS proxies or private networks). TLS 1.3 / mTLS is scheduled for Phase 19.
  2. *Garbage Payload Allocation Defense via Pooled Buffers (SEC-P11-002)*: To mitigate heap allocation amplification attacks where an adversary streams valid-length headers ($\le 5\text{MB}$) followed by corrupted payload bytes or mismatched CRC32 trailers, frame reading utilizes sized `sync.Pool` buffers (64KB and 5MB). Corrupted frames, truncated streams, or CRC checksum failures return the rented buffer to the pool immediately, producing zero net heap allocations on invalid frames.
  3. *Synchronized Multi-Caller Graceful Shutdown (SEC-P11-003)*: Concurrent invocations of `Server.Shutdown(ctx)` are coordinated via a dedicated `shutdownDone` channel. While the single-winner caller triggers listener close and waits for active connections to drain, all concurrent callers wait on `shutdownDone` (or context cancellation). No caller returns success before all connection goroutines have exited and all sockets are closed.
  4. *Listener Backoff on Temporary Network Errors (GAP-P11-001)*: The accept loop detects transient network errors (`net.Error.Temporary()`, such as `EMFILE`/`ENFILE` file descriptor exhaustion) and applies exponential backoff (5ms to 1s) rather than tightly spinning CPU cycles.
  5. *Supported vs Unsupported Engine Operations*:
     - `OP_PUT`, `OP_GET`, `OP_DELETE` dispatch directly to Phase 10 `Engine.Put`, `Engine.Get`, and `Engine.Delete`.
     - `OP_EXISTS`, `OP_BATCH`, `OP_STATS` are rejected deterministically with `StatusInvalidRequest` and explanatory diagnostics, adhering strictly to the principle of not fabricating unexposed storage engine functionality.
  6. *Slowloris & Resource Defense*:
     - Reading frames enforces separate deadlines: `IdleTimeout` (default 60s) for inactivity between requests, `HeaderTimeout` (default 5s) for frame header completion, and `PayloadTimeout` (default 10s) for payload completion.
     - `WriteTimeout` (default 5s) prevents stalled clients from holding server goroutines blocked indefinitely during response transmission.
     - `MaxConnections` (default 1024) caps simultaneous accepted connections to mitigate OS file descriptor and goroutine exhaustion.
  7. *Error Sanitization & Sequence Independence*:
     - Internal storage errors returned across the wire are sanitized to `"internal storage error"`, preventing leakage of filesystem paths, SSTable filenames, or WAL structures.
     - Wire `SeqID` is preserved strictly as a transport-level request correlation identifier; it is never mapped to or confused with internal Engine MVCC sequence numbers.
  8. *Pipelining & Advanced Concurrency*:
     - Connections process requests sequentially (one request read -> dispatched -> response written -> next request read). Out-of-order request pipelining and connection multiplexing are not implemented.
* **Why It Exists**:
  Provides a bounded, DoS-resistant TCP server boundary above the single-node storage engine without violating storage invariants or introducing speculative networking complexity.
* **Impact**:
  Decouples transport goroutines from storage engine internals; guarantees clean graceful shutdown without goroutine or socket leaks; isolates network client timeouts from storage durability.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Sequential request-response correlation, exact binary transparency, clean error mappings, concurrent shutdown synchronization verified under `-race`).
  * Performance: **Optimal** (Direct non-blocking dispatch, buffer pooling on invalid/corrupt payloads, minimal leaf lock contention).
  * Security: **Optimal** (Default loopback-only binding enforcement, garbage payload heap exhaustion protection, Slowloris defense, connection limits, sanitized error diagnostics).

### 63. Server Daemon Entrypoint Lifecycle & Configuration Boundaries (P12-S01-M01)
* **Limitation & Architectural Boundaries**:
  In `P12-S01-M01`:
  1. *Daemon CLI vs Storage Subsystem Separation*: `cmd/lattice` acts exclusively as an operational process supervisor and orchestration entrypoint. It parses CLI flags, loads configuration files, traps OS signals (`SIGINT`, `SIGTERM`), boots the Phase 10 `Engine`, binds the Phase 11 `Server`, and enforces strict two-stage graceful shutdown. It does NOT bypass the Engine, perform direct WAL manipulations, inspect raw SSTables, or modify Manifest pointers.
  2. *Strict Ordered Two-Stage Graceful Shutdown*: Shutdown enforces strict hierarchical ordering: `Server.Shutdown(ctx)` halts network connection ingestion and drains active client requests before `Engine.Close()` is invoked. `Engine.Close()` then drains immutable memtable queues, executes a final synchronous flush to an L0 SSTable, syncs the `MANIFEST` and `WAL`, and closes underlying descriptors. Reversing this order or closing the Engine while requests are active is mathematically prevented.
  3. *Loopback Transport Policy Preservation*: The daemon inherits and strictly enforces Phase 11's loopback security policy. Attempting to configure or bind a non-loopback address (e.g. `0.0.0.0`) without explicitly providing `--insecure-transport` fails closed at configuration validation with `errors.ErrInsecureTransport` before creating the database directory or opening storage.
  4. *Configuration Precedence & Bounded Parsing*: Configuration follows strict precedence: `Compiled Defaults` $\to$ `Configuration File` $\to$ `CLI Flags`. Configuration files (JSON or key-value) are limited to 1 MiB to prevent memory exhaustion DoS, and directories or non-regular files passed to `--config` fail fast.
  5. *Excluded Diagnostic & Client Tooling*: Interactive REPL client (`cmd/lattice-cli`), SSTable forensic inspection (`inspect-sstable`), and WAL forensic dumping (`dump-wal`) are explicitly excluded and reserved for subsequent micro-phases (`P12-S01-M02` through `P12-S01-M04`).
* **Why It Exists**:
  Single-responsibility micro-phase discipline. Delivering a robust, signal-aware server entrypoint that coordinates existing Engine and Server primitives establishes the live daemon foundation before building client and forensic diagnostic tooling.
* **Impact**:
  Provides a production-grade, testable daemon binary (`lattice`) with zero data loss on graceful termination, clean signal handling, and zero leaked goroutines or file descriptors.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic lifecycle state machine; real TCP integration tests prove data durability across process restarts).
  * Performance: **Optimal** (Negligible CLI initialization overhead, zero runtime CPU cost on hot paths).
  * Security: **Optimal** (Early loopback security enforcement, bounded configuration file reads, directory path sanitization, safe absorption of repeated shutdown signals).

### 64. Interactive REPL Client Boundaries & Protocol Transparency (P12-S01-M02)
* **Limitation & Architectural Boundaries**:
  In `P12-S01-M02`:
  1. *Client-Side Network Boundary Only*: `cmd/lattice-cli` is strictly a network client interacting with the running Lattice server over TCP using the Phase 11 binary wire protocol. It does NOT open database files, access WALs, inspect SSTables, or alter Manifest state directly.
  2. *Strict Protocol Fidelity (No Client-Side Operation Fabrication)*: Commands `PUT`, `GET`, and `DELETE` execute against active storage engine operations. For `EXISTS` and `STATS`, the client transmits the real wire opcodes (`OpExists`, `OpStats`) to the server and transparently renders the server's authoritative response (`StatusInvalidRequest: unsupported operation...`). The client does NOT fabricate boolean values via client-side `GET` emulation or synthesize fake statistics.
  3. *Zero Unbounded Buffer Allocation*: Command input is bounded to 5 MiB (`MaxInputLineSize`) using bounded line readers to prevent memory exhaustion DoS.
  4. *Binary Escaping & Distinguishable Empty Values*: Unprintable binary bytes in GET responses are safely escaped as `\xHH` inside quotes to protect terminal emulators from control sequence corruption. Zero-length values (`""`) are distinctly rendered from non-existent keys (`NOT FOUND`).
  5. *No Automatic Mutation Retries*: In the event of a connection failure or timeout during `PUT` or `DELETE`, the client does NOT automatically reconnect and retry the operation, preventing duplicate mutation hazards and adhering to the server's authoritative storage semantics.
  6. *Excluded Diagnostic Forensics*: Raw SSTable forensic inspection (`inspect-sstable`) and WAL record dumping (`dump-wal`) remain excluded and deferred to `P12-S01-M03` and `P12-S01-M04`.
* **Why It Exists**:
  Preserves clear separation between client presentation and storage engine invariants; enforces defensive programming at user terminal boundaries; maintains protocol transparency.
* **Impact**:
  Provides a secure, binary-safe, interactive and scriptable terminal interface (`lattice-cli`) with deterministic exit codes, clean EOF/quit handling, and zero memory leaks.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Transparent response rendering, sequence ID correlation, distinction between empty values and not-found keys).
  * Performance: **Optimal** (Streaming bounded tokenization, zero unnecessary buffer allocations).
  * Security: **Optimal** (5 MiB input bounds, no command injection/shell invocation, non-TTY color suppression, no credential leakage via shell history).

### 65. SSTable Forensic Inspection Tool Boundaries & Read-Only Invariants (P12-S01-M03)
* **Limitation & Architectural Boundaries**:
  In `P12-S01-M03`:
  1. *Strict Read-Only Guarantee*: `lattice inspect-sstable` opens target files strictly with `O_RDONLY`. It never repairs, rewrites, compacts, deletes, syncs, or truncates inspected SSTables. Byte-for-byte SHA-256 immutability is verified across inspections.
  2. *Single-Artifact Scope*: Operates strictly on the single file path provided by the user. Automatic data directory recursion, manifest-driven table resolution, and bulk database verification are excluded.
  3. *Independent Diagnostic Tooling*: Does not initialize the `Engine`, `VersionSet`, `MemTable`, `WAL`, or `BlockCache`. Operates directly on the raw on-disk SSTable structures (`Footer`, `BlockIndex`, `MetaIndex`, `BloomFilter`, `DataBlocks`).
  4. *Defensive Memory Bounds & Anti-DoS*: Rejects block sizes exceeding `MaxDataBlockSize` (8 MiB) and `MaxIndexBlockSize` (8 MiB), restart counts exceeding `MaxRestartCount` (65,536), and bit counts exceeding `MaxBitsetBytes` (256 MiB) prior to memory allocation.
  5. *Binary Output Safety*: All key bytes in forensic output are escaped with `\xHH` when non-printable, preventing terminal control sequence manipulation attacks.
  6. *Completed Diagnostic Forensics*: WAL forensic dumping (`dump-wal`) is implemented in `P12-S01-M04`.
* **Why It Exists**:
  Guarantees that diagnostic and forensic inspection can be safely performed on suspect or corrupted SSTable files without triggering unwanted mutations, engine side-effects, or terminal crashes.
* **Impact**:
  Provides a secure, deterministic, offline diagnostic tool for inspecting physical SSTables and diagnosing corruptions.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Exact physical format decoding; comprehensive CRC, magic, padding, and restart point validation).
  * Performance: **Optimal** (Targeted positional `ReadAt` reads; minimal memory allocation).
  * Security: **Optimal** (Read-only immutability, integer overflow protection on all block handle offsets, bounded memory allocations, terminal injection protection).

### 66. WAL Forensic Dump Tool Boundaries & Read-Only Invariants (P12-S01-M04)
* **Limitation & Architectural Boundaries**:
  In `P12-S01-M04`:
  1. *Strict Read-Only Guarantee*: `lattice dump-wal` opens target files strictly via `wal.OpenReader` with `O_RDONLY` and `openFileNoFollow`. It never repairs, rewrites, truncates, syncs, or modifies WAL segment files. Byte-for-byte SHA-256 immutability is verified across repeated inspections.
  2. *Single-Segment Scope*: Operates strictly on the single WAL segment file path supplied by the user. Automatic directory scanning, segment coordination, and multi-segment log stitching are excluded.
  3. *Independent Diagnostic Tooling*: Does not initialize the `Engine`, `VersionSet`, `MemTable`, `BlockCache`, or background coordinators. Operates directly on the raw WAL segment stream via authoritative low-level decoding routines.
  4. *Streaming O(1) Memory Footprint*: Each record is decoded, validated, formatted to stdout, and discarded sequentially. No full-segment buffers or unbounded record slices are retained in RAM.
  5. *Bounded Length Protections & Anti-DoS*: Enforces `MaxKeyLen` (65,535 bytes), `MaxValueLen` (4 MiB), and `MaxRecordLength` (4,259,866 bytes) prior to payload allocation, preventing integer overflow and memory exhaustion from malicious or corrupt length fields.
  6. *Binary Output Safety*: All key bytes and preview values are escaped via `FormatBytes` (`\xHH` escaping), preventing terminal control sequence manipulation or ANSI escape attacks. Value previews in verbose mode are capped at 64 bytes.
  7. *Excluded WAL Mutation & Replay*: WAL rewriting, log truncation, automatic corruption repair, transaction replay, and live crash recovery are strictly excluded from the forensic dumper.
* **Why It Exists**:
  Ensures that incident triage, corruption analysis, and forensic inspection of suspect WAL segments can be performed with zero risk of mutating the persistent log, replaying corrupt operations, or destabilizing the operating environment.
* **Impact**:
  Provides a secure, deterministic, offline forensic utility for dumping WAL segments, analyzing sequence numbers, validating checksums, and pinpointing torn writes.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Exact physical framing verification, distinction between clean EOF and torn tails, authoritative CRC32 checking).
  * Performance: **Optimal** (Single-pass sequential streaming, $O(1)$ memory consumption, minimal allocations).
  * Security: **Optimal** (Read-only immutability, TOCTOU inode pinning, bounded payload allocations, terminal injection prevention).

### 67. Security Remediation for Phase 12 CLI and Forensic Handlers (SEC-P12-001, SEC-P12-002)
* **Limitation & Architectural Boundaries**:
  Post-Phase 12 security audit hardening resolved:
  1. *FIFO Pre-Open Validation (`SEC-P12-001`)*: `InspectSSTable` performs pre-open inspection via `os.Stat(cleanPath)` before calling `os.Open()`, immediately rejecting non-regular files (FIFOs, sockets, device nodes) and eliminating kernel open blocking when targeted at named pipes without writers.
  2. *Strict Root Subcommand Validation (`SEC-P12-002`)*: `ParseFlags` validates that trailing positional argument slice `fs.Args()` is strictly empty. Unrecognized subcommands or misspelled arguments (e.g. `lattice dump_wal`) fail fast with a configuration error instead of defaulting to daemon server startup.
  3. *Bare Carriage Return Sanitization*: `FormatValue` in `lattice-cli` classifies bare carriage returns (`\r` not followed by `\n`) as non-printable, preventing terminal line rewriting attacks while maintaining clean support for CRLF newlines.
* **Why It Exists**:
  Protects local operators and automation pipelines from unintentional hangs, terminal spoofing, or unintended daemon background execution caused by syntax typos or named pipes.
* **Impact**:
  Strengthens defensive boundaries across all CLI tools (`cmd/lattice` and `cmd/lattice-cli`).
* **Dimensional Impact**:
  * Correctness: **Optimal** (Fail-fast rejection on unknown commands and non-regular files).
  * Performance: **Optimal** (Zero open blocking; minimal stat overhead).
  * Security: **Optimal** (Mitigates local DoS from FIFOs, port occupation from typos, and terminal spoofing from bare CR).

### 68. Zipfian Key Distribution Generator Scope and Mathematical Model (P13-S01-M01)
* **Limitation & Architectural Boundaries**:
  `ZipfGenerator` in `internal/benchmark/zipf.go` provides non-uniform access pattern generation following a discrete Zipfian (power-law) distribution with skew parameter $\theta = 0.99$.
  1. *Mathematical Model & Standard Library Incompatibility*: Go's standard library `math/rand.NewZipf` requires $s > 1$ and returns `nil` for $s \le 1$. Lattice implements the Jim Gray et al. (SIGMOD 1994) / YCSB (Cooper et al., 2010) finite Zipfian inversion algorithm, operating over a finite discrete domain $r \in [1, N]$ ($i \in [0, N-1]$) with generalized harmonic normalization $H_{N, \theta} = \sum_{j=1}^N j^{-\theta}$.
  2. *Single-Consumer Concurrency Model*: `ZipfGenerator` is backed by an isolated `*rand.Rand` PRNG source and is intentionally not thread-safe. Concurrent benchmark drivers (Phase 13 M03) must instantiate one generator per worker goroutine to achieve zero-contention, lock-free workload generation.
  3. *Zero-Allocation Hot Path*: `NextKeyBuf` formats keys into a caller-supplied slice with $0\text{ allocs/op}$ and $0\text{ B/op}$. `NextKey` provides an allocating alternative.
  4. *Decoupled Subsystems*: Latency tracking (P13-S01-M02), TCP client load running (P13-S01-M03), and `pprof` profiling endpoints (P13-S01-M04) are strictly decoupled and reserved for subsequent micro-phases.
* **Why It Exists**:
  Provides an efficient, deterministic, $O(1)$ sampling primitive modeling real-world access hotspots (80/20 rule) without introducing memory distortion, GC pressure, or mutex contention into benchmark measurements.
* **Impact**:
  Enables reproducible, non-uniform workload benchmarking across arbitrary keyspaces up to $10^9$ keys.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Strict bounds $r \in [0, N-1]$, seed reproducibility, Chi-Square goodness-of-fit validation).
  * Performance: **Optimal** (~27-36 ns/op sampling, ~46 ns/op zero-allocation key formatting).
  * Security: **Optimal** (Isolated RNG state, bounded keyspace, no filesystem or network side effects).

### 69. Benchmark Driver Buffer Memory Scaling Under Extreme Concurrency (SEC-P13-M03-POST-001)
* **Limitation & Architectural Boundaries**:
  In `cmd/lattice-bench`:
  1. *Per-Worker Independent Buffers*: To prevent cross-thread synchronization overhead and eliminate cache ping-pong on the benchmark driver's hot measurement path, each worker goroutine allocates its own dedicated write value payload buffer (`valBuf := make([]byte, cfg.ValSize)`) and connection I/O buffers.
  2. *Extreme Concurrency Footprint*: Under default benchmark settings (`--concurrency 16`, `--val-size 128`), aggregate buffer memory is negligible (~4.8 KB). However, if an operator configures extreme concurrency alongside massive value sizes (e.g. `--concurrency 1024` with `--val-size 4194304` [4 MiB maximum payload]), the driver process will allocate approximately 4.096 GiB of RAM purely for worker value buffers plus TCP socket buffers.
  3. *Zero Cross-Worker Sharing Invariant*: Worker value buffers are deliberately unshared to preserve strict thread isolation, deterministic CPU cache locality, and zero lock contention during latency measurement.
  4. *Unchanged Runner Source*: The benchmark runner implementation (`cmd/lattice-bench/runner.go`) remains unmodified in this micro-phase.
* **Why It Exists**:
  Micro-benchmarking harness design requires isolating worker goroutines from shared state to avoid measurement distortion. Sharing buffers across workers would introduce mutex contention or atomic synchronization, which artificially skews latency percentiles (P99/P99.9).
* **Impact**:
  Operators configuring high concurrency ($C \ge 512$) with large value payloads ($V \ge 1\text{ MiB}$) should ensure the host running `lattice-bench` has sufficient physical RAM to avoid OS out-of-memory (OOM) killer intervention.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic per-worker isolation, no torn buffers).
  * Performance: **Optimal** (0 allocation on hot path, 0 lock contention between workers).
  * Scalability: **Memory bounded linearly** by $C \times V$.

---

### 70. `pprof` Profiling Diagnostics Server Boundaries & Security Model (P13-S01-M04)
* **Limitation & Architectural Boundaries**:
  In `cmd/lattice` (`P13-S01-M04`):
  1. *Strict Loopback-Only Policy*: The pprof HTTP diagnostics server (`--pprof-address`) strictly enforces loopback binding (`127.0.0.1`, `localhost`, `[::1]`). Binding to wildcards (`0.0.0.0`, `::`) or external network interfaces is rejected during config validation and server instantiation with a fail-closed error.
  2. *Immunity to Insecure Transport Opt-In*: The `--insecure-transport` CLI flag (which permits unencrypted plaintext TCP on the binary storage port) does NOT bypass or relax the loopback restriction on the pprof HTTP server.
  3. *Complete Data Plane Isolation*: Pprof is served over a dedicated HTTP listener on a separate port. It is never multiplexed over the binary TCP database protocol port (`9099`).
  4. *Dedicated ServeMux*: Endpoints are registered on an isolated `http.NewServeMux()`, completely preventing exposure via `http.DefaultServeMux`.
  5. *Unauthenticated Local Diagnostics*: The HTTP diagnostics server does not implement authentication (basic auth, bearer tokens) or TLS, as it is designed exclusively for local or SSH-tunneled operator diagnostics. Remote operators must establish an encrypted SSH port-forwarding tunnel (`ssh -L 6060:127.0.0.1:6060`) to access endpoints on remote nodes.
  6. *Opt-In by Default*: Pprof is disabled by default (`PprofAddress = ""`). Zero HTTP ports or listeners are created unless explicitly opted into via configuration or CLI flag.
* **Why It Exists**:
  Runtime profiling endpoints (heap dumps, CPU profiling, goroutine stacks) expose internal process memory and state. Enforcing loopback-only binding ensures that diagnostic utilities cannot be remotely accessed or weaponized for reconnaissance or denial-of-service over public networks.
* **Impact**:
  Provides safe, production-grade profiling and observability without exposing unauthenticated HTTP endpoints to external networks.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Full standard Go runtime/pprof endpoint parity).
  * Performance: **Zero overhead when disabled**; ~1-3% CPU overhead only during active CPU sampling.
  * Security: **Fail-closed loopback enforcement**, complete transport isolation, zero risk of remote information disclosure.

### 71. Static Peer Topology Address Canonicalization & Syntactic Aliasing Boundary (P14-S01-M01)
* **Limitation & Architectural Boundaries**:
  In `internal/cluster` (`P14-S01-M01`):
  1. *Syntactic / Structural Canonicalization Only*: Peer endpoint validation and duplicate detection operate strictly syntactically (`net.SplitHostPort`, port range checks $1..65535$, IP normalization via `net.ParseIP`, lowercase host conversion). Zero network I/O or DNS lookups (`net.LookupHost`) are performed during configuration parsing.
  2. *Hostname Aliasing Boundary*: Because DNS resolution is deliberately avoided during configuration validation to guarantee fast, deterministic, offline-capable startup and eliminate cold-boot deadlock, aliases that resolve to the same underlying IP address (e.g., `127.0.0.1:9098` vs `localhost:9098`, or `node1.internal:9098` vs `10.0.0.1:9098`) cannot be proven equivalent syntactically. If an operator configures two peers where one uses an IP and the other uses an unresolved hostname alias pointing to that IP, syntactic validation cannot detect this duplication at config parse time.
  3. *Static Membership Invariant*: Dynamic topology reconfiguration (`AddPeer`, `RemovePeer`, `SetPeer`) is explicitly prohibited in M01; cluster topology is frozen and immutable after initial validation. Dynamic membership transitions require future Raft consensus protocols (Phase 15).
  4. *Bounded Cluster Size*: Cluster topology is constrained to $N \le 256$ peers (`MaxClusterSize`) to prevent memory amplification and resource exhaustion from malicious configuration files.
* **Why It Exists**:
  Network calls and DNS dependencies during configuration parsing introduce startup latency, non-deterministic failures during network partitions, and break hermetic offline test suites. Syntactic validation provides microsecond verification and eliminates external dependencies during process boot.
* **Impact**:
  Operators configuring static clusters should maintain consistency in address notation across cluster nodes (e.g., exclusively using canonical IP addresses or consistent FQDN hostnames). Duplicate detection at the transport socket level will fail closed if two connections collide during transport initialization in subsequent phases.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic syntactic validation, zero DNS side effects).
  * Performance: **Optimal** (Sub-millisecond configuration parsing).
  * Security: **Optimal** (Bounds cluster size to 256, rejects wildcards and malformed ports, zero network attack surface during parsing).

### 72. Stateless Peer RPC Framing Boundaries & Transport Lifecycle Decoupling (P14-S01-M02)
* **Limitation & Architectural Boundaries**:
  In `internal/transport` (`P14-S01-M02`):
  1. *Stateless Codec Scope*: The peer RPC codec provides binary serialization and deserialization for Raft messages (`RequestVote`, `AppendEntries`, responses) with strict structural validation, pre-allocation length bounds, and CRC32-IEEE integrity verification. It intentionally does not establish network connections (`net.Dial`, `net.Listen`), connection pools, keep-alive loops, or reconnect backoffs (deferred to P14-S01-M03).
  2. *Replay Cache Decoupling*: Peer RPC frames carry a 64-bit cryptographic `Nonce` field and frame-level `SeqID` metadata, but the codec maintains no in-memory nonce cache or sliding replay window. Stateful replay detection and connection-scoped sequence verification require authenticated session tracking and are deferred to the peer connection manager (P14-S01-M03) and consensus engine (Phase 15).
  3. *Fixed Wire Layout & Zero Dynamic Negotiation*: The framing protocol uses a fixed, deterministic binary layout (`Magic = 0x4C415454`, fixed 18-byte header, 4-byte CRC32 trailer, fixed payload offsets). Dynamic version negotiation is intentionally omitted; protocol compatibility is enforced through strict opcode namespace partitioning (`0x81..0x84` for peer, `0x01..0x06` for client) and fail-closed rejection of unknown message types.
* **Why It Exists**:
  Separation of concerns: Framing codecs must remain purely deterministic, memory-bounded, and stateless. Coupling wire framing with transport networking, TLS cryptographic sessions, or state-machine replay caches introduces lock contention and architectural fragility.
* **Impact**:
  Callers consuming M02 framing must not assume raw peer frames provide cryptographic confidentiality, mutual authentication, or replay immunity until integrated with the authenticated mTLS transport layer in P14-S01-M03.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Deterministic byte layout, zero ambiguous states).
  * Performance: **Optimal** (~7-17 ns single message encode/decode, zero heap allocation on fixed headers).
### 73. Outbound Peer Connection Lifecycle Scope & Security/mTLS Abstraction Boundary (P14-S01-M03)
* **Limitation & Architectural Boundaries**:
  In `internal/transport` (`P14-S01-M03`):
  1. *Injectable Dialing & mTLS PKI Decoupling*: `PeerConnectionManager` manages socket lifecycle, bounded exponential reconnect, TCP keep-alive, write serialization, and framed message consumption. Network connection establishment is abstracted via `DialFunc func(ctx context.Context, addr string) (net.Conn, error)`. Universal mutual TLS (mTLS) PKI generation, certificate authority (CA) lifecycle, certificate issuance, and rotation are intentionally excluded from this micro-phase and reserved for subsequent security hardening (Phase 19). Local development and offline unit tests operate over loopback TCP using this injectable abstraction without representing unencrypted transport as production-ready.
  2. *Transport TCP Keep-Alive vs Consensus Heartbeat Decoupling*: The manager configures transport-level TCP keep-alive (`SO_KEEPALIVE` and `SetKeepAlivePeriod`) for detecting dead sockets during idle periods. It intentionally does NOT implement or transmit application-level PING, PONG, or HEARTBEAT opcodes. Consensus heartbeats (empty `AppendEntries` RPCs) belong strictly to the Raft consensus engine in Phase 15.
  3. *Static Membership Invariant*: The manager constructs connection supervisors strictly from immutable `cluster.Topology.RemotePeers()`. Dynamic cluster reconfiguration (`AddPeer`, `RemovePeer`, joint consensus) is not supported in M03; topology modifications require constructing a new topology and manager in Phase 15.
  4. *Consensus Agnostic Transport*: The manager transports generic `*Frame` envelopes. It does not parse, evaluate, or interpret Raft terms, candidate IDs, vote grants, log matching indexes, or state-machine commands.
* **Why It Exists**:
  Separation of concerns: Network transport lifecycle (reconnect loops, backoff timers, socket buffer serialization) must remain decoupled from consensus state machines and cryptographic PKI lifecycles. Coupling transport with Raft elections or PKI infrastructure creates circular dependencies and prevents deterministic offline testing.
* **Impact**:
  Callers receive a robust, race-free, persistent outbound framing transport layer. Higher-level Raft consensus protocols (Phase 15) consume this manager to exchange `RequestVote` and `AppendEntries` frames reliably across cluster nodes.
### 74. Phase 14 Adversarial Security Audit & Transport Hardening Boundaries (SEC-P14-001)
* **Limitation & Architectural Boundaries**:
  Comprehensive adversarial security review and remediation across Phase 14 (`P14-S01-M01`, `P14-S01-M02`, `P14-S01-M03`) established the following security boundaries:
  1. *Plaintext Transport Public Network Restriction*: The default peer connection manager enforces loopback-only peer transport by default. Any attempt to dial a remote non-loopback address (`192.168.x.x`, public IPs, external hostnames) over unencrypted cleartext TCP without setting `PeerConnectionConfig.InsecureTransport = true` fails closed with `errors.ErrInsecureTransport`. Universal mutual TLS (mTLS) for multi-host production deployments is scheduled for Phase 19.
  2. *Bounded Sliding-Window Replay Defense*: `peerSupervisor` maintains an in-memory `peerReplayFilter` per peer with bounded capacity (`DefaultReplayWindowSize = 4096`, `DefaultMaxNoncesTracked = 4096`). Incoming frames with duplicate sequence IDs, stale sequence IDs outside the 4096-window, or duplicate cryptographic nonces on `RequestVote` / `AppendEntries` requests are dropped fail-closed before reaching consensus callbacks. Replay filters are bounded in memory and persist across connection reconnects to defeat reconnect-and-replay attacks.
  3. *Thread-Safe Frame Serialization*: `EncodeFrame` treats caller-supplied `*Frame` structures as strictly read-only. In-place mutations of `Magic`, `PayloadLength`, and `CRC` on the caller's struct were eliminated. Concurrently broadcasting a single `*Frame` pointer across multiple peer connections (e.g. during Raft log replication) is guaranteed data-race-free under ThreadSanitizer.
  4. *Socket Partial-Write Completion Loop*: `EncodeFrame` executes an atomic write loop guaranteeing that frames are either transmitted completely or return an explicit I/O error (`io.ErrShortWrite`). Partial writes are never reported as successful deliveries.
  5. *Lifecycle Start/Close Synchronization*: `PeerConnectionManager.Start` and `Close` are synchronized via `lifecycleMu`, preventing `sync.WaitGroup` misuse, race panics, and orphaned supervisor goroutines during concurrent startup and shutdown.
  6. *RFC 1123 Hostname Sanitization*: Address canonicalization strictly enforces RFC 1123 label boundaries (1..63 chars, alphanumeric start/end, `[a-z0-9-]` only), normalizes trailing FQDN dots before IP parsing to eliminate wildcard bypasses, and rejects all-numeric top-level domains.
  7. *Non-Overflowing Exponential Backoff*: Reconnect backoff calculation uses iterative doubling with saturation guards, guaranteeing `0 < delay <= ReconnectMax` for any arbitrary failure count up to `math.MaxInt`.
* **Why It Exists**:
  Protects the distributed storage cluster from protocol desynchronization, packet replay, race crashes, and unauthorized plaintext exposure before consensus logic (Phase 15) is layered on top.
* **Impact**:
  Provides a robust, race-free, bounded transport foundation that fail-closes against adversarial inputs.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Exact write loop, replay filter, lifecycle mutex).
  * Performance: **Optimal** ($O(1)$ replay tracking, zero memory leaks, ~100s 50x race pass).
  * Security: **Fail-closed default transport**, cryptographic nonces via `crypto/rand`, RFC 1123 validation.

---

### 75. Phase 15 Raft Scope & Model Boundaries (P15-S01 — P15-S03 + Hardening)
* **Limitation & Architectural Boundaries**:
  The single-group Raft core (`internal/raft`: persistent state, elections, heartbeats, proposal ingestion, follower replication, leader quorum commitment) is implemented and hardened, with the following deliberate boundaries:
  1. *Volatile `commitIndex`*: The leader's commit index is in-memory derived state, reset to 0 on restart and rebuilt by subsequent quorum activity (a re-elected leader's current-term no-op entry recommits the inherited prefix without client traffic). It is never persisted and never implies state-machine application.
  2. *No follower commit tracking / application*: Leaders transmit `LeaderCommit`, but followers neither store nor apply it; there is no `lastApplied`, no state-machine execution, and no client commit acknowledgement. This is Phase 16 work.
  3. *Commitment ≠ application*: A committed entry is guaranteed present on future leaders (leader completeness) but is not yet executed against any state machine; client-visible linearizability additionally requires Phase 16 (apply) and Phase 17 (ReadIndex).
  4. *Crash-fault peer model*: Follower `MatchIndex` reports are trusted within the configured membership (monotonic, plausibility-bounded by the leader's own log length). Byzantine peers that forge responses are outside the threat model.
  5. *No request/response correlation state*: `matchIndex`/`nextIndex` are updated from response contents with monotonicity and term-session gating rather than per-request tracking; delayed or reordered same-term responses converge (a dipped `nextIndex` heals on the next success) and can never corrupt commit state.
  6. *Truncate-then-append crash window*: Follower conflict replacement chains the existing durable `TruncateSuffix` and `Append` operations. An interruption between them leaves a valid durable prefix that leader retry re-matches; no false acknowledgement is possible. No new transactional storage primitive was introduced.
  7. *Static membership*: Topology is immutable after construction; no joint consensus, snapshots, log compaction, or membership changes.
  8. *Production mTLS*: Peer transport runs over the Phase 14 loopback-guarded framing; universal mTLS remains Phase 19 work.
* **Why It Exists**:
  Phase 15 delivers a defensible consensus core with minimal, auditable mechanisms. Persistence-backed commit indexes, application pipelines, and Byzantine defenses belong to later phases with their own failure models.
* **Impact**:
  Operators must not treat `CommitIndex()` as an application-level durability signal across restarts, and must not expose uncommitted or committed-but-unapplied entries to clients as applied state.
* **Dimensional Impact**:
  * Correctness: **Optimal within scope** (Raft safety invariants hold over the replicated log; state-machine safety applies once Phase 16 applies entries).
  * Performance: **Optimal** (commit scan is O(log) per response; replication batches are frame-bounded).
  * Scalability: **None** (single-group topology unchanged).

---

### 76. Phase 16 State Machine Apply Loop Scope & Model Boundaries (P16-S01-M01)
* **Limitation & Architectural Boundaries**:
  The state machine apply loop (`internal/raft/apply.go`, `internal/raft/command.go`) establishes sequential, deterministic application of committed Raft entries to the local LSM storage engine (`internal/engine`), with the following deliberate boundaries:
  1. *Volatile `lastApplied` & Restart Replay*: `lastApplied` is an in-memory monotonic watermark owned exclusively by the apply loop. It is not persisted to disk in M01. On restart, application progress resets to 0 (or restored point), and committed entries are replayed sequentially. For current CRUD primitives (`Put` with overwrite, `Delete`), re-application is naturally idempotent. Non-idempotent primitives (e.g. increments, list appends) are non-goals for this micro-phase and will require deduplication mechanisms in later phases.
  2. *Strict Failure Halt (No Skip)*: If an entry fails state-machine application (e.g., corrupted command payload, disk exhaustion, closed engine), the apply loop halts immediately and sets `n.ApplyError()`. `lastApplied` does not advance. The loop never skips failed entry $N$ to process $N+1$. Recovery requires operator intervention or node restart with a corrected engine state.
  3. *Consensus Coordinates vs. LSM Sequence Numbers*: Raft `LogIndex` is a consensus coordinate; LSM `SeqNum` is an internal engine MVCC tag. They are never conflated. Raft consensus no-ops (`PeerEntryNoop`) advance `lastApplied` to maintain Raft log alignment but produce zero LSM mutations.
  4. *Client Proposal Routing Deferred*: P16-S01-M01 implements only the background apply loop. Client request interception, leader proposal forwarding, and follower redirection belong to P16-S01-M02.
  5. *Linearizable Reads Deferred*: Reading state machine data directly without consensus verification does not guarantee linearizability; lease-based or consensus-verified reads (`ReadIndex`) belong to Phase 17.
* **Why It Exists**:
  Preserves clear separation of concerns between consensus commitment and local state machine durability without prematurely introducing distributed client routing or snapshotting.
* **Impact**:
  Replication and state machine apply are fully decoupled from client wire protocol changes.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Strict contiguous index order $N, N+1, \dots$; invariant $0 \le \text{lastApplied} \le \text{commitIndex} \le \text{lastLogIndex}$ enforced; halted on error).
  * Performance: **Streaming batch processing** (bounded `DefaultApplyBatchSize` chunks avoid unbounded memory allocation during commit catchup).
  * Scalability: **None** (Single-group consensus).

---

### 77. Phase 16 Client Proposal Routing & Follower Redirection Scope & Model Boundaries (P16-S01-M02)
* **Limitation & Architectural Boundaries**:
  The client proposal router (`internal/transport/server.go`, `internal/raft/router.go`) bridges the external TCP client data-plane write path (`OpPut`, `OpDelete`) to the Raft consensus subsystem, enforcing consensus-first mutation and follower redirection with the following boundaries:
  1. *Proposal Acceptance vs. Quorum Commit vs. State Machine Apply*:
     `Node.Propose()` durably appends the canonical client mutation command to the leader's local Raft log and returns `StatusOK`.
     **Critical Distinction**: In P16-S01-M02, proposal acceptance guarantees that the entry is durably persisted to the active leader's local log. It does *not* block waiting for quorum replication across peer nodes, nor does it block waiting for the P16-S01-M01 apply loop to execute the command against the local LSM engine. In a single-node cluster ($N=1$), `refreshCommitIndex()` commits the entry immediately on append, waking the apply loop. In multi-node clusters ($N > 1$), asynchronous peer replication drives commit advancement and subsequent application. Synchronous quorum commit waiting and linearizable read verification (`ReadIndex`) belong to Phase 17.
  2. *Follower Redirection via Trusted Topology*:
     Non-leader nodes (followers and candidates) strictly intercept client write requests and return `StatusNotLeader` (0x06).
     Followers never mutate their local LSM engine directly and never propose log entries locally.
     If a leader is known, the redirection response includes the leader's NodeID and network address resolved strictly from the immutable `cluster.Topology`.
     The system never relies on client-provided addresses or unverified network endpoints, eliminating SSRF and open-redirect vectors.
  3. *Candidate & Unknown Leader Handling*:
     If a node is in the `Candidate` role, or is a `Follower` with no known leader (`LeaderIDNil` = 0), or if the known leader has no configured address in the topology, the router returns `StatusNotLeader` with an informative message indicating election in progress or unknown leader. Clients are expected to apply exponential backoff and retry.
  4. *Preserved Standalone / Local Engine Mode*:
     For single-process deployments and standalone tests operating without Raft consensus, `transport.Server` maintains backward compatibility: when `ProposalRouter` is nil, client writes are dispatched directly to `Engine.Put` and `Engine.Delete`. Consensus mode vs. standalone mode is strictly explicit via `ServerConfig.ProposalRouter` / `Server.SetProposalRouter`.
  5. *Retry and Deduplication Semantics*:
     Lattice does not implement a client duplicate-request filtering table in M02. If a client transmits a proposal, receives a network timeout, and retransmits, multiple identical entries may be appended to the Raft log.
     Because Lattice `Put` (key-value overwrite) and `Delete` (tombstone) are naturally idempotent operations in the LSM state machine, sequential replay of duplicate writes produces deterministic, identical final state. Non-idempotent operations (such as atomic counters or list appends) will require a dedicated request deduplication table in future phases.
  6. *Client Sequence ID Preservation*:
     Every client response strictly preserves the client's request `SeqID` across all outcomes: leader success, follower redirect, unknown leader, validation errors, and internal proposal failures. Raft log indexes and terms are strictly decoupled from client sequence IDs.
* **Why It Exists**:
  Prevents client writes from bypassing consensus on replicated nodes while maintaining decoupled transport and Raft abstractions, zero import cycles, and 100% wire-protocol compatibility.
* **Impact**:
  Replicated nodes cannot suffer state divergence caused by direct local engine writes. Followers reliably guide clients to the current leader.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Consensus-first write invariant strictly enforced; direct engine bypass eliminated; zero races during leadership transitions).
  * Performance: **Minimal overhead** (Local append and async replication; zero network hop on leader writes).
  * Scalability: **Single-group leader write bottleneck** (As documented in Limitation 1).


---

### 78. Phase 16 Security Remediation & Hardening (P16-SEC)
* **Limitation & Architectural Boundaries**:
  An exhaustive security audit and independent review across `P16-S01-M01` (Apply Loop) and `P16-S01-M02` (Proposal Routing) established critical hardening boundaries against consensus bypass, leadership TOCTOU races, unbounded memory allocation, and unauthenticated redirects:
  1. *Cluster-Mode Fail-Open Bypass Elimination (F01 / F09)*:
     - `transport.ServerConfig` introduces an explicit `ClusterMode bool` flag, captured immutably in `Server.clusterMode` upon initialization.
     - In cluster mode, if the consensus router is unavailable (`s.router == nil`), `s.dispatch()` strictly rejects `OpPut` and `OpDelete` with `StatusError` ("cluster mode active but consensus router unavailable").
     - Direct fallback to `s.engine` occurs *only* when `!s.clusterMode` (standalone local engine mode).
     - Dynamically clearing or mutating `s.router` via `SetProposalRouter(nil)` cannot enable direct engine writes in cluster mode.
  2. *Leadership TOCTOU & Phantom Acknowledgement Defense (F02 / F10 / GAP A)*:
     - `Node` tracks a monotonically increasing `leaderEpoch uint64`, incremented under lock on all role transitions and stepdowns (`becomeLeaderLocked`, `campaignLocked`, `BecomeFollower`, `ObserveHigherTerm`, `StepDownSameTerm`, `HandleRequestVote`, `HandleRequestVoteResponse`, `HandleAppendEntries`, `HandleAppendEntriesResponse`, `Close`).
     - `ProposeWithContext()` takes an atomic snapshot of `leaderEpoch` prior to disk I/O. Following durable `Storage.Append()`, `leaderEpoch` is re-verified under lock.
     - If the node stepped down or lost leadership during the append via any vector (higher-term heartbeat, same-term dual leader discovery, vote request/response), client acknowledgement is suppressed with `ErrRaftInvalidRoleTransition`. The appended entry remains in the log and will be cleanly truncated or committed by the subsequent leader, preventing split-brain phantom ACKs.
  3. *Context Disambiguation & Post-Append Ambiguity (F03 / GAP B)*:
     - Pure client context cancellation or deadline expiration is strictly decoupled from leadership loss (`ErrRaftInvalidRoleTransition`).
     - Context expiration before admission, during mutex acquisition, or before append returns pure `ctx.Err()`, causing `RouteWrite()` to return `StatusThrottled` (0x04) without triggering a misleading `StatusNotLeader` redirect.
     - *Intentional Semantic Ambiguity*: If context expires after durable `Storage.Append()`, the entry is durably written to the local log, but client acknowledgement returns `StatusThrottled`. The distinction between `durably appended locally`, `quorum committed`, and `applied to state machine` is strictly preserved.
  4. *Apply Batch Size Upper Bounding (F04)*:
     - `StartApplyLoop()` enforces a hard maximum batch size: `MaxApplyBatchSize = 4096`. Values $\le 0$ default to `DefaultApplyBatchSize = 64`, and values exceeding 4096 are clamped.
     - Prevents adversarial or misconfigured batch limits from causing unbounded heap allocations during commit catch-up.
  5. *Strict Entry Type & Canonical Control Validation (F05)*:
     - `applySingleEntry()` enforces that `PeerEntryNoop` entries must carry strictly empty payload (`len(Data) == 0`). Non-empty noops are rejected with `ErrRaftCorruptedState`.
     - Reserved `PeerEntryConfiguration` entries with non-empty payload are rejected fail-closed, preventing silent consumption of unsupported configuration commands.
  6. *Storage Result Bounds & Contiguity Verification (F06)*:
     - `drainCommittedEntries()` defensively verifies the count and index bounds of entries returned by `Storage.Entries(from, to)`.
     - Specifically validates: `len(entries) <= requestedCount`, `entries[0].Index == from`, `lastReturnedIdx < to`, and `lastReturnedIdx <= commitIndex`. Any anomaly immediately halts the apply loop with `ErrRaftCorruptedState`.
  7. *Bounded Apply Loop Shutdown Timeout (F07)*:
     - `stopApplyLoop()` introduces a bounded termination wait (`DefaultApplyShutdownTimeout = 10s`).
     - If the underlying state machine or LSM engine blocks indefinitely during apply, `Node.Close()` logs the timeout and proceeds, preventing permanent hangs on server shutdown.
  8. *Header Injection & Open Redirect Sanitization (F08)*:
     - `ParseRedirectMessage()` and redirect payload parsers reject messages containing ASCII control characters (0x00–0x1F, 0x7F), preventing response splitting, log injection, or format string exploits.
  9. *Production Daemon End-to-End Wiring (GAP C)*:
     - `cmd/lattice/daemon.go` wires `raft.Storage`, `raft.Node`, `raft.ProposalRouter`, and `StartApplyLoop` into the production server lifecycle.
     - Standalone mode continues to mutate `engine.Engine` directly when cluster flags are absent.
     - Cluster mode initializes Raft consensus, routes client writes exclusively through `ProposalRouter`, and halts fail-closed if consensus components are absent.
* **Why It Exists**:
  Guarantees fail-closed consensus invariants across distributed failure modes, hostile client requests, uncoordinated leadership transitions, and daemon lifecycles.
* **Impact**:
  Nodes operating in cluster mode cannot silently bifurcate state machine state from Raft consensus state.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Linearization of proposals preserved across stepdowns; zero fail-open bypass; complete epoch fencing).
  * Security: **Audited & Hardened** (Fail-closed invariants, bounded buffers, sanitized wire outputs, context disambiguation).
  * Performance: **Minimal overhead** (Epoch checks and batch clamping operate in $O(1)$ time).

### 79. Phase 17 Leader Quorum Heartbeat Verification Scope & Invariants (P17-S01-M01)
* **Limitation & Architectural Boundaries**:
  The ReadIndex primitive (`Node.ReadIndex(ctx context.Context) (ReadIndexResult, error)` in `internal/raft/node.go`) establishes the safe Raft-side read position and confirms active current-term leadership across a majority of the cluster before returning, with the following deliberate boundaries:
  1. *Raft-Side Primitive Only (No State Machine Barrier in M01)*:
     `Node.ReadIndex` establishes that the node is the legitimate leader with confirmed current-term majority authority and returns the safe committed position (`ReadIndexResult{Index: commitIndex, Term: term}`). It does **not** wait for `lastApplied >= ReadIndex`, nor does it perform state-machine read dispatch or alter client `GET` request handling. State-machine read barriers and client-facing linearizable GET execution belong exclusively to P17-S01-M02.
  2. *Strict Quorum Semantics*:
     Quorum is evaluated using the cluster's canonical rule: $\text{quorumSize} = \lfloor N/2 \rfloor + 1$.
     - In single-node clusters ($N=1$), the leader alone constitutes a majority ($\text{quorumSize}=1$) and returns immediately without network I/O or waiting.
     - In multi-node clusters ($N > 1$), the leader broadcasts empty `AppendEntries` heartbeat probes bearing a round-unique 64-bit cryptographic nonce to all remote peers and awaits current-term confirmation from a majority ($1 + \text{acks} \ge \text{quorumSize}$).
  3. *Stale-Response & Round Correlation Defense*:
     Followers echo `req.Nonce` in `AppendEntriesResponse` (extended backwards-compatibly to 25 bytes on the wire when `Nonce != 0`). Responses are matched in $O(1)$ against active read rounds. Responses with mismatched nonces, legacy zero nonces, older terms, or older leadership epochs are strictly ignored and cannot advance quorum confirmation.
  4. *Continuous Leadership & Epoch Verification*:
     The linearization point captures `commitIndex` atomically with verified `role == RoleLeader`, continuous `currTerm`, and unchanged `leaderEpoch`. Any concurrent stepdown, higher-term observation, or node closure aborts all pending read rounds fail-closed with `ErrRaftInvalidRoleTransition` or `ErrRaftStateClosed`.
  5. *Context Error Preservation*:
     Context cancellation or timeout during quorum wait immediately unregisters the active round and returns pure context errors (`context.Canceled` or `context.DeadlineExceeded`), preventing spurious not-leader redirects.
* **Why It Exists**:
  Prevents stale reads under asymmetric network partitions (where an isolated former leader might otherwise serve reads with obsolete data) by requiring explicit, fresh proof of leadership from a majority.
* **Impact**:
  The Raft consensus layer now provides a verifiable, race-free `ReadIndex` primitive ready for consumption by the P17-S01-M02 state-machine read barrier.
* **Dimensional Impact**:
  * Correctness: **Optimal** (Atomic linearization point, round-unique nonces, leadership epoch fencing, zero TOCTOU windows).
  * Security: **Audited & Hardened** (Fail-closed on stepdown, unknown peers rejected, bounded memory with automatic round deregistration).
  * Performance: **Low overhead** (Single round of heartbeat probes; single-node clusters execute with zero network delay).

---

*End of Known Limitations — To be updated continuously throughout implementation.*


# ADR-007: Version-Pinned Snapshot Isolation & Concurrency Model

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Concurrency Core Team
* **Technical Invariants Affected**: Read/Write Concurrency, Compaction Interleaving, File Descriptor Lifecycle

---

## 1. Context
The database engine must handle concurrent multi-threaded client reads and writes while background workers continuously flush MemTables to disk and execute multi-megabyte compaction merges.

## 2. Problem
Using a coarse reader-writer mutex (`sync.RWMutex`) over the entire engine creates severe lock contention. Long-running compaction file merges would block incoming reads, causing catastrophic tail latency spikes. Conversely, uncoordinated concurrent reads risk reading partially written files or crashing when compaction deletes files from under an active reader.

## 3. Decision
We adopt **Version-Pinned Snapshot Isolation**:
1. All physical metadata (active SSTable sets) is encapsulated in an immutable `Version` struct.
2. Reads pin the active `Version` via an atomic reference counter: `atomic.AddInt32(&version.refCount, 1)`.
3. Flushes and compactions create new files and install a new `Version` via an atomic pointer swap (`atomic.StorePointer`).
4. Active readers continue accessing their pinned, immutable files without holding any locks during disk I/O.
5. When a reader finishes, it calls `version.Unref()`. When `refCount == 0`, obsolete files are safely unlinked.

## 4. Alternatives Considered
1. **Global Engine RWMutex**: Simple, but compaction and flushes block all queries.
2. **Channel-Based Actor Event Loop**: Single coordinator goroutine; serializes all operations, bottlenecking multi-core CPU read scalability.

## 5. Reasoning
* **Zero Lock Contention on Disk Reads**: Readers never block background workers, and background workers never block readers.
* **Point-in-Time Consistency**: A reader observes a completely consistent snapshot of the database state throughout its query execution.

## 6. Trade-offs
* **Temporary Disk Space Retention**: If a slow reader pins an old `Version`, obsolete SSTables replaced by compaction cannot be deleted from disk until the slow reader completes its query.

## 7. Consequences & Mitigations
* We track reader lease durations; if a reader holds a pinned version for $>60\text{ seconds}$, a warning metric is emitted.

## 8. Evidence Classification
* **Theoretical Property**: Lock-free, non-blocking reads during background disk flushes and compactions.
* **Test Verification**: Concurrency tests with 64 reader goroutines and continuous background compaction running under `go test -race`.

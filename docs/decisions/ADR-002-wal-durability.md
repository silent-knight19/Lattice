# ADR-002: Group Commit & Durability Policy for the Write-Ahead Log

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Storage Internals Core Team
* **Technical Invariants Affected**: WAL Write Path, `fdatasync()` Cadence, Tail Latency

---

## 1. Context
A database must survive sudden process crashes and power outages without losing acknowledged transactions. Durability requires ensuring bytes leave the operating system page cache and reach non-volatile flash storage media via kernel barrier system calls (`fdatasync()`).

## 2. Problem
On modern NVMe solid-state storage, a synchronous `fdatasync()` system call takes approximately $0.1\text{ms} - 0.8\text{ms}$. Calling `fdatasync()` on every individual client write operation limits database throughput to disk IOPS (typically $1,200 - 5,000$ writes/sec), creating a massive bottleneck for multi-threaded workloads.

## 3. Decision
We implement **Cooperative Group Commit** as the default durability policy, supported by configurable Strict Sync and Periodic Sync modes. A dedicated batch leader drains pending writes from a bounded concurrent queue, writes them contiguously to the active WAL segment, and issues a single `fdatasync()` barrier for the entire batch.

## 4. Alternatives Considered
1. **Strict Sync Per Write**: Calling `fdatasync()` synchronously on every single `PUT`.
2. **Asynchronous / Periodic Flush**: Writing to the OS page cache and relying on the Linux kernel `pdflush` or a 1-second ticker to sync dirty pages.

## 5. Reasoning
* **Throughput Scaling**: Group Commit amortizes the physical I/O barrier cost across hundreds of concurrent client goroutines.
* **Strict Crash Guarantees**: A client request only receives a success acknowledgement after its batch has been committed to disk by `fdatasync()`. No acknowledged data is lost on sudden power failure.
* **Flaw of Async Flush**: Asynchronous flushing loses up to 1 second of acknowledged user transactions on power loss, violating ACID durability.

## 6. Trade-offs
* **Micro-Latency Addition**: Low-concurrency workloads incur a micro-batching wait timer ($\le 2\text{ms}$) to allow write coalescing.
* **Implementation Complexity**: Requires coordination logic between the batch leader and waiting follower channels, including panic recovery handlers.

## 7. Consequences & Mitigations
* If the batch leader panics during disk I/O, a `defer ... recover()` block intercepts the panic, broadcasts an error to all waiting followers, and triggers an orderly engine shutdown.

## 8. Evidence Classification
* **Design Target**: Scale throughput from $2,500 \text{ writes/sec}$ (strict sync) to $\ge 80,000 \text{ writes/sec}$ (group commit).
* **Observed Limitation**: Single-threaded, synchronous sequential writes will observe a P99 latency bounded by the $2\text{ms}$ batch timer.

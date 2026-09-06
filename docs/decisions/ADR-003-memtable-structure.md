# ADR-003: Selection of Probabilistic SkipList for In-Memory MemTable

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Concurrency Core Team
* **Technical Invariants Affected**: MemTable Read Path, Concurrency Synchronization, Memory Overhead

---

## 1. Context
The MemTable provides in-memory buffering for incoming writes while serving live point lookups and range scans before data flushes to disk. It must support high-concurrency multi-threaded access.

## 2. Problem
Balanced search trees (such as AVL or Red-Black trees) require balancing rotations when keys are inserted. In a concurrent environment, pointer rotations touch multiple parent, child, and sibling nodes simultaneously. Protecting these multi-node mutations requires coarse-grained locking, which stalls concurrent readers and throttles multi-core CPU scalability.

## 3. Decision
We select a **Probabilistic SkipList** with height generation governed by a geometric coin-flip distribution ($p=0.25, L_{max}=16$). Writers acquire an internal mutex, while readers traverse forward pointers completely lock-free using atomic pointer reads (`atomic.LoadPointer`).

## 4. Alternatives Considered
1. **Concurrent Red-Black Tree**: Strict balance, but requires heavy reader-writer mutexes or complex software transactional memory.
2. **Adaptive Radix Tree (ART)**: High performance, but significantly higher implementation complexity and memory alignment edge cases.
3. **Partitioned Concurrent Hash Map**: Ultra-fast point lookups, but incapable of ordered range scans required by LSM-trees.

## 5. Reasoning
* **Lock-Free Read Safety**: SkipList nodes are never moved, rotated, or rebalanced after insertion. Forward pointers are modified using compare-and-swap or bottom-up pointer assignment, guaranteeing readers always observe a monotonically consistent list without locks.
* **Algorithmic Simplicity**: Node heights are determined probabilistically at allocation time, eliminating rotation invariants.

## 6. Trade-offs
* **Pointer Memory Overhead**: Each node contains a slice of forward pointers. With $p=0.25$, the expected average number of pointers per node is $\frac{1}{1-p} \approx 1.33$.
* **Worst-Case Search Time**: Worst-case search complexity is $O(N)$ if the randomizer degrades, though the probability of severe degradation across $N=100,000$ keys is $< 10^{-12}$.

## 7. Consequences & Mitigations
* We seed the PRNG securely and track exact memory allocations (including pointer slices) to guarantee MemTable size tracking remains accurate down to the byte.

## 8. Evidence Classification
* **Theoretical Property**: Expected $O(\log N)$ search, insertion, and deletion complexity.
* **Test Verification**: Unit tests verify geometric height distribution over 100,000 insertions; race detector verifies lock-free reads under 16 concurrent threads.

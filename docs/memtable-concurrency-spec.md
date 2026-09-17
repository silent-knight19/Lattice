# Lattice MemTable & Concurrent SkipList Specification

**Document ID:** SPEC-MEMTABLE-CONCURRENCY-001  
**Status:** NORMATIVE  
**Engine Subsystem:** In-Memory Storage & Indexing (`internal/memtable`)  
**Phase Origin:** Phase 03 ("In-Memory MemTable & Concurrent SkipList")  
**Security Level:** Trust Boundary 2 (Storage & File I/O) / Trust Boundary 1 (In-Memory Protection)

---

## 1. Scope and Architectural Objectives

This document establishes the authoritative concurrency model, memory-ordering contract, probabilistic height distribution, memory accounting invariants, and freeze lifecycle for the in-memory MemTable subsystem in Lattice.

The MemTable acts as the primary in-memory write buffer and active index for all live, unflushed keys. It satisfies the following architectural guarantees:
1. **Single-Writer / Lock-Free Multi-Reader Concurrency:** Write operations are serialized by an exclusive mutex, while point lookups (`SearchConcurrent`) and Level-0 forward iterators traverse the multi-level express lanes with zero mutex acquisition, utilizing sequentially consistent atomic pointer operations.
2. **Safe Publication (No Publish-Before-Init):** Readers never observe uninitialized keys, nil values, or torn pointers.
3. **Rigorous Heap Accounting & Anti-DoS Ceilings:** Every node accounts for its struct header, key backing array, tower pointer slice, and value container. Hard memory and entry-count caps prevent heap-exhaustion attacks before allocation occurs.
4. **Adversarial RNG Resistance:** Height generation is decoupled from key and value contents, using a system-entropy-seeded PCG32 generator capped at a hard maximum height ($L_{\max} = 16$).
5. **Deterministic Freeze Linearization:** Freezing provides a sharp linearization boundary with zero copying, transitioning the table to permanent immutability.

---

## 2. Concurrency & Memory-Ordering Model

### 2.1 Role Separation & Locking Disciplines

| Operation | Synchronization Mechanism | Lock Type | Allocations | Complexity |
|---|---|---|---|---|
| `Insert` (New Entry) | `s.mu.Lock()` | Exclusive Write Lock | $1$ node + tower + key/value copies | $O(\log N)$ expected |
| `Insert` (Exact Duplicate Update) | `s.mu.Lock()` + `node.value.Store()` | Exclusive Write Lock | $1$ `nodeValue` container | $O(\log N)$ search, $O(1)$ swap |
| `SearchConcurrent` (Point Lookup) | Lock-Free (Atomic Pointer Loads) | **Zero Locks** | $1$ defensive value clone | $O(\log N)$ expected |
| `NewIterator` (Create Iterator) | `s.activeIterators.Add(1)` | Atomic Counter | $1$ `Iterator` struct | $O(1)$ |
| `Iterator.Next` / `Seek` | `it.sl.mu.RLock()` + `it.mu.Lock()` | Read Lock on List, Mutex on Iterator | $0$ allocations | $O(1)$ next, $O(\log N)$ seek |
| `Freeze` (Transition to Immutable) | `s.mu.Lock()` + `s.frozen.Store(true)` | Exclusive Write Lock | $0$ allocations | $O(1)$ |

### 2.2 Safe Publication Invariants

To eliminate publish-before-init races under lock-free concurrent readers:
1. **Private Construction:** The writer fully allocates the `skipListNode` struct, sets its immutable `key` (defensively cloned), stores the `nodeValue` pointer, and initializes its `forward` pointer slice with targets determined during predecessor search.
2. **Bottom-Up Splicing:** Predecessor forward pointers are updated using `atomic.Pointer.Store` strictly from **Level 0 up to Level $H-1$**.
   - Because readers traverse from Level $H_{\text{active}}-1$ down to Level 0, a reader observing the new node at any upper level is guaranteed that all lower-level express lanes are already spliced.
3. **Active Height Monotonicity:** If the new node's height $H$ exceeds the current `activeHeight`, the height is expanded using `atomic.Int32.Store`. Readers reading `activeHeight` via `atomic.Int32.Load` observe a monotonically non-decreasing level count.

```text
Writer Thread:
  1. Allocate node N (height=3) in private memory
  2. Copy Key and Value into N
  3. Populate N.forward[0..2] with successor pointers
  4. atomic.Store(&pred[0].forward[0], N)  <-- Spliced Level 0
  5. atomic.Store(&pred[1].forward[1], N)  <-- Spliced Level 1
  6. atomic.Store(&pred[2].forward[2], N)  <-- Spliced Level 2
  7. If height > activeHeight: atomic.Store(&activeHeight, height)

Reader Thread:
  1. h = atomic.Load(&activeHeight)
  2. Traverse pred -> curr via atomic.Load(&curr.forward[level])
  3. Invariant: N is fully initialized before any forward pointer references it.
```

### 2.3 Exact Duplicate InternalKey Updates

When an insertion matches an existing entry with the exact same `(UserKey, SeqNum, OpType)`:
- The node structure, key, tower height, and forward pointers remain completely untouched.
- The writer allocates a new `nodeValue` container with a defensive copy of the new payload.
- The writer executes `node.value.Store(newValueContainer)`.
- Concurrent readers reading `node.value.Load()` atomically observe either the previous or new value container without torn reads or data races.
- The `byteSize` counter is adjusted by the delta: `newValBytes - oldValBytes`.

---

## 3. Probabilistic Height Generation & Invariants

SkipList tower heights follow a geometric distribution with promotion parameter $p = 0.25$ and hard ceiling $L_{\max} = 16$.

### 3.1 Mathematical Properties

- **Parameter:** $p = 0.25$ (probability of advancing to the next level is $1/4$).
- **Probability Distribution:**
  $$\Pr(\text{Height} \ge h) = p^{h-1} = \left(\frac{1}{4}\right)^{h-1} \quad \text{for } 1 \le h \le 16$$
- **Average Node Height:**
  $$\mathbb{E}[\text{Height}] = \sum_{h=1}^{16} h \cdot \Pr(\text{Height} = h) = \frac{1}{1 - p} = \frac{4}{3} \approx 1.333 \text{ pointers/node}$$
- **Expected Search Complexity:** $O\left(\frac{1}{p} \log_{1/p} N\right) = O(\log_4 N)$ comparisons.
- **Maximum Addressable Capacity:** $L_{\max} = 16$ accommodates up to $4^{15} \approx 1,073,741,824$ (1 billion) keys with optimal $O(\log N)$ performance.

### 3.2 PRNG Architecture & Adversarial Hardening

```text
                    +--------------------------------+
                    |       crypto/rand (OS CSPRNG)   |
                    +--------------------------------+
                                    |
                          64-bit Entropy Seed
                                    |
                                    v
                    +--------------------------------+
                    |   PCG32 (PCG-XSH-RR Generator) |
                    +--------------------------------+
                                    |
                             32-bit Output
                                    |
                                    v
                    +--------------------------------+
                    |  Promotion Gate: (val & 3 == 0) |
                    +--------------------------------+
                                    |
                    Clamped to [MinHeight=1, MaxHeight=16]
```

- **Algorithm:** PCG-XSH-RR (`PCG32`), 64-bit internal state with 32-bit output.
- **Entropy Seeding:** `initialEntropySeed()` reads 8 bytes from `crypto/rand` at startup. If the operating system CSPRNG is unavailable, it falls back to `time.Now().UnixNano()`.
- **Deterministic Loop Termination:** The height generation loop checks `height < MaxHeight && (src.Uint32() & 3 == 0)`. The loop terminates deterministically in at most $16 - 1 = 15$ iterations, immune to infinite loops even under hostile PRNG implementations.
- **Adversarial Decoupling:** Height generation depends strictly on the PRNG state. User key and value contents have **zero influence** on node height, preventing an attacker from crafting keys to artificially force maximal tower heights.

---

## 4. Memory Accounting & Anti-DoS Ceilings

### 4.1 Exact Byte Accounting (Model A)

The `ByteSize()` method tracks the exact heap bytes directly owned by user-record structures.

$$\text{EntryBytes} = \text{NodeStructSize} + \text{len}(\text{UserKey}) + (\text{Height} \times \text{PointerSize}) + \text{ValueBytes}$$

Where:
- On 64-bit platforms:
  - `NodeStructSize = 72` bytes (`InternalKey` 40B + `atomic.Pointer` 8B + `forward` slice header 24B).
  - `PointerSize = 8` bytes per forward pointer in the tower.
  - `NodeValueStructSize = 24` bytes (`data []byte` slice header).
- On 32-bit platforms:
  - `NodeStructSize = 40` bytes.
  - `PointerSize = 4` bytes.
  - `NodeValueStructSize = 12` bytes.

### 4.2 Hard Resource Ceilings

| Parameter | Constant | Value | Enforcement Point | Error Returned |
|---|---|---|---|---|
| **Max MemTable Bytes** | `MaxMemTableSize` | $64\text{ MiB}$ ($67,108,864$ bytes) | Pre-allocation check in `Insert` | `*errors.MemTableFullError` |
| **Max MemTable Entries** | `MaxMemTableEntries` | $1,000,000$ records | Pre-allocation check in `Insert` | `*errors.MemTableFullError` |
| **Max Key Size** | `binary.MaxKeyLen` | $65,535$ bytes ($64\text{ KB} - 1$) | Input validation in `Insert` | `*errors.KeyTooLargeError` |
| **Max Value Size** | `binary.MaxValueLen` | $4,194,304$ bytes ($4\text{ MB}$) | Input validation in `Insert` | `*errors.ValueTooLargeError` |

**Pre-Allocation Failure Atomicity:** When an incoming write would cause `ByteSize() + EntryBytes > MaxMemTableSize` or `count >= MaxMemTableEntries`, `Insert` returns `*errors.MemTableFullError` immediately. **Zero heap memory is allocated**, neutralizing allocation bombs.

---

## 5. Freeze Lifecycle & Backpressure Coordination

### 5.1 Freeze State Transition

```text
       +---------------------------------------------+
       |             ACTIVE MemTable                 |
       |  - Accepting Inserts                        |
       |  - Lock-free SearchConcurrent               |
       |  - Forward Iterators (weakly consistent)    |
       +---------------------------------------------+
                              |
                     sl.Freeze() under mu.Lock()
                              |
                              v
       +---------------------------------------------+
       |             FROZEN MemTable                 |
       |  - Inserts REJECTED (ErrMemTableFrozen)     |
       |  - Values and Towers Permanently Immutable  |
       |  - Lock-free SearchConcurrent continues     |
       |  - Forward Iterators (strictly immutable)   |
       +---------------------------------------------+
                              |
                     Flushed to SSTable (Phase 04)
                              |
                              v
       +---------------------------------------------+
       |             DISCARDED / GC'd                |
       +---------------------------------------------+
```

1. **Linearization:** `Freeze()` acquires `s.mu.Lock()`.
   - Inserts that acquired `s.mu.Lock()` prior to `Freeze()` complete and are included in the frozen table.
   - Inserts arriving after `Freeze()` observe `s.frozen.Load() == true` and are rejected with `errors.ErrMemTableFrozen`.
2. **Idempotence:** Calling `Freeze()` multiple times returns `true` on the initial transition and `false` on subsequent calls.
3. **Zero Copy:** The data structure is frozen in place without allocating a copy.

### 5.2 Engine-Level Backpressure & Flushing

At the storage engine layer (`internal/engine`):
1. **Rotation Trigger:** When `activeMem.Insert` returns `ErrMemTableFull` (or when size reaches threshold), the engine:
   - Freezes `activeMem`.
   - Appends it to the immutable MemTable slice (`immMems`).
   - Allocates a fresh active `SkipList`.
   - Retries the write in the fresh `SkipList`.
   - Signals the background flush worker (`e.signalFlush()`).
2. **Write Pacing & Stall:**
   - The engine tracks total memory across active and immutable tables:
     $$\text{TotalMemory} = \text{activeMem.ByteSize}() + \sum_{i} \text{immMem}_i.\text{ByteSize}()$$
   - Before sequence allocation or WAL append, `e.backpressure.Acquire(ctx, estBytes)` gates incoming writes.
   - If flush falls behind and immutable tables accumulate, `Acquire` injects microsecond pacing delays.
   - If memory exceeds the hard engine limit, `Acquire` stalls writes until a background flush completes, preventing Out-Of-Memory (OOM) panics.

---

## 6. Forward Iterator Consistency Contract

1. **Physical Order:** Iterators yield entries along Level 0 in strictly ascending canonical order:
   - `UserKey` ASC (lexicographical)
   - `SeqNum` DESC (newest revision first)
   - `OpType` DESC (`DELETE` tombstone before `PUT`)
2. **Weak Consistency over Active Tables:** Iterators over an active MemTable observe entries inserted ahead of the iterator's cursor. Entries inserted behind the cursor are not observed.
3. **Strict Immutability over Frozen Tables:** Iterators over a frozen MemTable observe an exact, immutable snapshot of all committed records at freeze time.
4. **Defensive Clones:** `it.Key()` and `it.Value()` return defensive copies of internal slices, preventing external caller mutations from affecting the MemTable.
5. **Thread Safety:** The `Iterator` struct protects its internal cursor and state with an internal `sync.RWMutex`, preventing data races if `Next()` and `Close()` are called concurrently.

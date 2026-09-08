# SEC-04: In-Memory Storage & MemTable Threat Model

**Subsystem**: In-Memory Storage Engine (`internal/memtable/`, `internal/binary/`, `internal/engine/`)  
**Security Phase**: SEC-04 — In-Memory Concurrent Engine & SkipList Security Audit  
**Status**: ACTIVE THREAT MODEL  
**Date**: September 2026

---

## 1. Subsystem Architecture & Boundary Context

The in-memory storage subsystem sits between the Write-Ahead Log (WAL) durability boundary and the persistent SSTable storage subsystem. In Lattice's Log-Structured Merge-Tree (LSM) architecture:

```
[ Client Request ]
       │
       ▼
[ Write-Ahead Log (WAL) ] ──(Strict fdatasync / Group Commit)──► [ Durable Disk ]
       │
       ▼ (Sequential Replay / Active Append)
[ Active MemTable (SkipList) ] ◄── Concurrent Lock-Free Reads (Search / Iterator)
       │
       ▼ (Threshold: 64 MB / Flush Command)
[ Immutable MemTable ]
       │
       ▼ (Background Compactor)
[ SSTable Subsystem ] ──► [ Level 0 SSTables on Disk ]
```

### 1.1 Implemented vs. Roadmapped Subsystem State

In accordance with the Security Evidence Discipline Rule (**Code is Authoritative**):

| Component | Implementation State | Active Code Location | Audited in SEC-04 |
| :--- | :---: | :--- | :---: |
| **InternalKey Multi-Version Comparator** | **IMPLEMENTED** | `internal/binary/internalkey.go` | **YES** (Dynamic & Fuzz) |
| **Key & Value Bounds Enforcement** | **IMPLEMENTED** | `internal/binary/validate.go` | **YES** (Dynamic & Fuzz) |
| **Trailer & Wire Serialization** | **IMPLEMENTED** | `internal/binary/internalkey.go` | **YES** (Dynamic & Fuzz) |
| **Operation Types & Sequence Enums** | **IMPLEMENTED** | `internal/binary/types.go` | **YES** (Dynamic & Fuzz) |
| **Domain Errors & Sentinels** | **IMPLEMENTED** | `internal/errors/errors.go` | **YES** (Dynamic) |
| **Concurrent SkipList Data Structure** | **DESIGN TARGET** | Planned for Phase 03 (`P03-S01`) | Evaluated as Target Contract |
| **MemTable Struct & Allocator** | **DESIGN TARGET** | Planned for Phase 03 (`P03-S02`) | Evaluated as Target Contract |
| **Forward & Reverse Iterators** | **DESIGN TARGET** | Planned for Phase 03 (`P03-S03`) | Evaluated as Target Contract |
| **Atomic Freeze State Machine** | **DESIGN TARGET** | Planned for Phase 03 (`P03-S03`) | Evaluated as Target Contract |

---

## 2. Threat Actors & Attacker Capabilities

SEC-04 models the following threat capabilities operating against the in-memory engine:

### 2.1 Attacker Profile 1: Hostile Client / Concurrent Writer
- **Capability**: Can submit arbitrary write requests (`PUT`, `DELETE`), arbitrary key lengths ($0 \dots \infty$), arbitrary value lengths ($0 \dots \infty$), duplicate keys, identical keys with distinct sequence numbers, out-of-order sequence numbers, and pathological binary byte sequences.
- **Objective**: Trigger unbounded heap allocation (OOM crash), corrupt in-memory sort order, cause race conditions, or alias internally stored byte slices.

### 2.2 Attacker Profile 2: Hostile Client / Concurrent Reader
- **Capability**: Can execute concurrent point lookups (`Get`), initiate multiple iterators, retain iterators across background flush transitions, and mutate returned byte slices.
- **Objective**: Trigger race conditions during concurrent pointer traversal, read uncommitted or stale data, cause use-after-free or dangling pointer dereferences, or alter stored state by mutating returned slices.

### 2.3 Attacker Profile 3: Memory Exhaustion / Denial of Service Actor
- **Capability**: Repeatedly updates the exact same key to flood multi-version histories, creates millions of microscopic keys to maximize SkipList pointer overhead, or submits maximum-size payloads ($4\text{ MB}$ values).
- **Objective**: Exhaust physical server RAM, induce excessive GC pauses, or crash the database process via the OS Out-Of-Memory (OOM) killer.

---

## 3. Attack Scenarios & Analysis

### Scenario 1: Uncontrolled Memory Allocation via Declared Lengths
- **Attack Vector**: Submitting keys or values exceeding architectural thresholds ($65,535\text{ bytes}$ for keys, $4\text{ MB}$ for values).
- **Defense Invariant**: `ValidateKey` and `ValidateValue` enforce hard limits prior to slice copying or node allocation. Rejections return structured `KeyTooLargeError` and `ValueTooLargeError` without allocating heap memory.

### Scenario 2: Memory Aliasing & Caller Slice Mutation
- **Attack Vector**: Caller calls `NewInternalKey(userKey, seq, op)` or inserts into MemTable, and subsequently mutates `userKey[0] = 'X'`. If internal structures retain the caller's slice directly without defensive copying, internal stored state is silently mutated out-of-band.
- **Defense Invariant**: `NewInternalKey` and `Clone()` must perform independent deep copies of `userKey`. Callers must never be able to alter stored keys after submission.

### Scenario 3: Returned Memory Mutation (Egress Aliasing)
- **Attack Vector**: Caller performs `Get(key)` or reads `iter.Value()`, receiving a byte slice pointing directly into the SkipList node's internal backing array. Caller mutates `value[0] = 0xFF`, corrupting stored data for all subsequent readers.
- **Defense Invariant**: The retrieval API must return an independent defensive copy of the value bytes or document an explicit, immutable zero-copy buffer ownership protocol.

### Scenario 4: Comparator Evasion & Sequence Inversion
- **Attack Vector**: Crafting keys that exploit subtle comparator bugs (e.g. signed vs unsigned byte comparison, improper tie-breaking between PUT and DELETE, or sequence number comparison inverted).
- **Defense Invariant**: `CompareInternalKey` must implement a strict weak ordering:
  1. `UserKey` ascending: unsigned byte-by-byte lexicographical comparison (`bytes.Compare`).
  2. `SeqNum` descending: newer sequence numbers sort strictly before older ones ($SeqNum_A > SeqNum_B \implies -1$).
  3. `OpType` descending: tie-breaker ensuring `OpTypeDelete` (0x02) sorts before `OpTypePut` (0x01) if sequence numbers match.

### Scenario 5: Deletion Resurrection & Version Shadowing
- **Attack Vector**: An attacker updates key $K$ at $SeqNum=10$ with a tombstone (`DELETE`), but comparator flaws cause an older version ($SeqNum=5$) to be returned during lookups.
- **Defense Invariant**: Because $SeqNum$ is sorted descending, point lookups scanning the SkipList encounter the newest version first. If the newest version is a tombstone, the engine must return `ErrKeyNotFound` immediately without inspecting older versions.

### Scenario 6: Concurrent Pointer Publication (Data Races)
- **Attack Vector**: A writer inserts a new SkipList node with multiple forward pointers (height $H$). If forward pointers are spliced into the list before the node's key, value, and lower-level pointers are completely initialized, concurrent readers reading without locks (`atomic.LoadPointer`) observe uninitialized or garbage memory.
- **Defense Invariant**: SkipList node fields must be fully written and published using atomic pointer stores with release semantics (`atomic.StorePointer`). Readers must use atomic load primitives.

### Scenario 7: Iterator Invalidation during Table Freeze
- **Attack Vector**: An active iterator is scanning the MemTable when the table reaches $64\text{ MB}$ and transitions to `ImmutableMemTable` for background flushing to disk. If the active table is freed, closed, or overwritten, the iterator crashes.
- **Defense Invariant**: MemTables must utilize atomic reference counting or pointer retention. An immutable MemTable remains readable until all active iterators are closed and the SSTable flush completes.

### Scenario 8: Pathological Random Seed & Height Degeneration
- **Attack Vector**: If the height generator randomizer is biased or exploitable, an attacker might force all nodes to height 1 (degenerating the SkipList into an $O(N)$ linked list, causing quadratic search times and CPU starvation DoS) or force all nodes to height $L_{max}$ (wasting pointer memory).
- **Defense Invariant**: Node height generation must strictly adhere to geometric distribution ($p=0.25, L_{max}=16$). The expected search complexity remains $O(\log N)$.

---

## 4. Security Invariant Matrix

| ID | Invariant Statement | Enforcement Mechanism | Status |
| :--- | :--- | :--- | :---: |
| **SEC-MEM-INV-01** | External mutable buffers cannot corrupt stored InternalKey state. | Defensive deep copying in `NewInternalKey` and `Clone()`. | **MEASURED RESULT** |
| **SEC-MEM-INV-02** | InternalKey ordering strictly satisfies canonical multi-version comparison. | Three-tier comparator in `CompareInternalKey`. | **MEASURED RESULT** |
| **SEC-MEM-INV-03** | Key and value sizes cannot exceed configured memory bounds. | Pre-allocation validation in `ValidateKey` and `ValidateValue`. | **MEASURED RESULT** |
| **SEC-MEM-INV-04** | InternalKey wire trailer encoding is strictly 9 bytes and unambiguous. | Fixed 8-byte big-endian SeqNum + 1-byte OpType trailer. | **MEASURED RESULT** |
| **SEC-MEM-INV-05** | SkipList structure contains no cycles and maintains monotonic level ordering. | Forward pointer splicing invariant. | **DESIGN TARGET** |
| **SEC-MEM-INV-06** | Concurrent operations cannot expose partially initialized nodes. | Atomic pointer publication (`atomic.StorePointer`). | **DESIGN TARGET** |
| **SEC-MEM-INV-07** | Deletion tombstones permanently shadow older record revisions. | SeqNum descending ordering in point lookup traversal. | **DESIGN TARGET** |
| **SEC-MEM-INV-08** | Iterators never dereference invalid memory across lifecycle freeze events. | Immutable MemTable refcounting and read-only pinning. | **DESIGN TARGET** |
| **SEC-MEM-INV-09** | Sequence number overflow cannot cause silent wrap to 0. | `SeqNum.Next()` bounds check with `ErrSeqNumOverflow`. | **MEASURED RESULT** |
| **SEC-MEM-INV-10** | MemTable freeze transitions are atomic and reject subsequent writes. | Atomic state transition returning `ErrMemTableFrozen`. | **DESIGN TARGET** |

---

## 5. Audit Scope & Evidence Classification Strategy

1. **Active Code Verification**: Every implemented component in `internal/binary` (`InternalKey`, comparator, validation, codecs, OpType, SeqNum) is subjected to comprehensive dynamic adversarial test suites, race detection, and fuzz testing.
2. **Unimplemented Code Discipline**: All components not yet implemented on the product roadmap (SkipList struct, MemTable struct, MemTable iterator, MemTable freeze) are classified strictly as `DESIGN TARGET` and `NOT APPLICABLE`. Zero mock structures or fake tests are fabricated.

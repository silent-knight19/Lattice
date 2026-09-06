# ADR-005: Adoption of Leveled Compaction Over Size-Tiered Compaction

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Storage Internals Core Team
* **Technical Invariants Affected**: Compaction Lifecycle, Space Amplification, Read Amplification

---

## 1. Context
In an append-only LSM storage engine, updates and deletions accumulate over time. A background compaction subsystem must periodically merge sorted runs to purge obsolete revisions, reclaim disk space, and bound the number of files checked during reads.

## 2. Problem
Choosing the wrong compaction strategy can cause runaway read degradation (checking hundreds of files per query) or catastrophic space amplification (requiring 50% free disk space just to execute a merge).

## 3. Decision
We implement **Leveled Compaction** ($L_0..L_N$) with a $10\times$ size factor multiplier:
* $L_0$: Flushed MemTables; keys may overlap across files.
* $L_1..L_N$: Strictly partitioned, **non-overlapping** key ranges. $L_1 = 10\text{MB}, L_2 = 100\text{MB}, L_3 = 1\text{GB}, \dots$

## 4. Alternatives Considered
1. **Size-Tiered Compaction (STCS)**: Merging files of similar size into larger files (Cassandra default).
2. **Time-Window Compaction**: Merging files by timestamp boundaries (specialized for time-series).

## 5. Reasoning
* **Bounded Read Amplification**: Because files in levels $L \ge 1$ have strictly non-overlapping key boundaries, point lookups inspect **at most one** SSTable per level. For a 100GB database with 4 levels, at most 4 SSTables are inspected.
* **Low Space Amplification**: Leveled Compaction merges small files incrementally (e.g. one 2MB file at a time), keeping space amplification low (~$1.11 - 1.33\times$). Size-Tiered compaction requires up to $50\%$ spare disk capacity during major merges.

## 6. Trade-offs
* **Higher Write Amplification**: Data is repeatedly re-read and re-written as it cascades down levels $L_1 \to L_N$, resulting in write amplification factors of $10-25\times$.

## 7. Consequences & Mitigations
* **Write Stall Mitigation**: If write traffic outpaces compaction and $L_0$ exceeds 8 files, the engine injects progressive write delays ($1\text{ms}-50\text{ms}$) to prevent unbounded $L_0$ accumulation.

## 8. Evidence Classification
* **Theoretical Property**: Space amplification bounded to $\le 1.33\times$; point lookup disk reads bounded to $\text{Count}(L_0) + \text{LevelCount}$.
* **Observed Limitation**: Write amplification is higher than Size-Tiered compaction on pure write-only workloads.

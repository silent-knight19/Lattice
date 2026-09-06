# ADR-001: Selection of Log-Structured Merge-Tree (LSM-Tree) Storage Architecture

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Distributed Systems Core Team
* **Technical Invariants Affected**: Write Path, Disk I/O Profile, Compaction Lifecycle

---

## 1. Context
Lattice requires a persistent, high-throughput storage engine capable of ingesting arbitrary key-value pairs on modern solid-state storage (NVMe SSDs). Modern database workloads exhibit high write volumes and point queries.

## 2. Problem
Traditional storage engines frequently utilize in-place update structures like $B^+$ Trees (e.g. SQLite, PostgreSQL, MySQL InnoDB). On flash media, updating records in-place requires reading, modifying, and writing back full 8KB/16KB disk pages for small (50-byte) records, generating severe random I/O, heavy write amplification, and premature flash drive wear.

## 3. Decision
We select a **Log-Structured Merge-Tree (LSM-Tree)** architecture composed of an append-only Write-Ahead Log (WAL), an in-memory SkipList MemTable, immutable on-disk Sorted String Tables (SSTables), and background Leveled Compaction.

## 4. Alternatives Considered
1. **$B^+$ Tree with Buffer Pool Manager**: In-place page updates with LRU page eviction and write-ahead dirty page flushing.
2. **Bitcask Append-Only Log with In-Memory Hash Index**: Append-only log with an in-memory `key -> file_offset` hash table.

## 5. Reasoning
* **Sequential I/O Dominance**: LSM-trees convert all random writes into sequential writes, utilizing maximum NVMe disk write bandwidth.
* **Range Scan Capability**: Unlike Bitcask (which cannot perform efficient range queries because keys are not sorted on disk), SSTables preserve global lexicographical sort order.
* **Memory Boundedness**: Unlike Bitcask (where all keys must permanently fit in RAM), an LSM-tree with sparse block indexes allows datasets to exceed RAM by orders of magnitude.

## 6. Trade-offs
* **Increased Read Amplification**: A point lookup may need to check multiple levels if the key is not in the MemTable.
* **Write Amplification During Compaction**: Re-merging files across levels consumes background I/O bandwidth.

## 7. Consequences & Mitigations
* **Mitigation for Read Amplification**: In-memory Murmur3 Bloom filters (10 bits/key) eliminate $>99\%$ of cold read disk I/O; an explicit 16-shard LRU block cache caches decompressed 4KB data blocks.
* **Mitigation for Write Amplification**: Leveled Compaction with a $10\times$ multiplier bounds space amplification to $\le 1.33\times$.

## 8. Evidence Classification
* **Theoretical Property**: $O(1)$ amortized sequential write complexity; $O(\log N)$ point read complexity bounded by level count $L$.
* **Design Target**: $\ge 80,000 \text{ writes/sec}$ on modern NVMe hardware (to be validated via benchmark).

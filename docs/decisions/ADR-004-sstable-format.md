# ADR-004: Binary SSTable File Format with Two-Level Indexing and Prefix Compression

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Storage Internals Core Team
* **Technical Invariants Affected**: On-Disk Format, Read Path, Block Indexing

---

## 1. Context
SSTables store immutable sorted runs of key-value data on persistent disk. The format directly dictates disk space efficiency, CPU decompression overhead, and the latency of point lookups and range scans.

## 2. Problem
Storing individual uncompressed records on disk produces severe bloat. Conversely, compressing an entire multi-megabyte SSTable into a single file makes point lookups slow, as reading a 100-byte record would require decompressing the entire file into memory.

## 3. Decision
We adopt a **Block-Structured SSTable Layout**:
1. Sorted records are partitioned into fixed-size **4KB Data Blocks**.
2. Within each 4KB block, records use **Prefix Compression** relative to preceding keys, with restart points every 16 keys.
3. A **Two-Level Sparse Index** records only the largest key of each data block and its offset/size handle.
4. A fixed **48-Byte Footer** at the end of the file anchors the index handle and terminates with an 8-byte magic number (`0x4C41545453535401`).

## 4. Alternatives Considered
1. **Single Monolithic Compressed File**: Max compression ratio, but atrocious random read latency.
2. **Dense Indexing (Index Every Key)**: Fast lookups, but the index consumes $>30\%$ of total disk space and cannot remain RAM-resident.

## 5. Reasoning
* **Fast Point Lookups**: To find a key, the reader binary searches the RAM-resident index block, loads exactly one 4KB data block via `pread()`, and binary searches within that block via the restart array.
* **Prefix Compression Efficiency**: Keys in real databases frequently share long prefixes (e.g. `user:10001:profile`). Resetting prefixes every 16 records allows binary search within 4KB without decompressing from byte 0.

## 6. Trade-offs
* **Internal CPU Overhead**: Binary searching inside a prefix-compressed block requires decoding key deltas.
* **Block Alignment Padding**: Data blocks are aligned to boundary thresholds, causing minor internal fragmentation ($\le 4\text{KB}$ per file).

## 7. Consequences & Mitigations
* Corrupted block offsets are mitigated by validating that every handle's `Offset + Size` does not exceed the total physical file size before issuing disk reads.

## 8. Evidence Classification
* **Theoretical Property**: Reduces in-memory index size by $>90\%$ compared to dense indexing; bounds disk I/O to exactly one 4KB block read per SSTable.

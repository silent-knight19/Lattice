# Lattice SSTable (Sorted String Table) Format Specification

**Document ID:** SPEC-SSTABLE-FORMAT-001  
**Status:** NORMATIVE  
**Engine Subsystem:** Storage & Persistent File Format (`internal/sstable`)  
**Phase Origin:** Phase 04 (Persistent SSTable Subsystem)  
**Security Level:** Trust Boundary 2 (Storage & File I/O)

---

## 1. Scope and Architectural Objectives

This document establishes the authoritative, byte-level on-disk specification for Sorted String Table (SSTable) files in Lattice. SSTables represent immutable, ordered, randomly-addressable on-disk representations of key-value data flushed from MemTables or produced during background compactions.

The format satisfies the following non-negotiable architectural and security invariants:
1. **Immutable Random Addressability:** Files are written strictly sequentially during construction, sealed with an atomic publication protocol, and accessed exclusively via positional `ReadAt` operations on an immutable pinned file descriptor.
2. **Fixed-Size Reverse-Anchored Footer:** Every SSTable terminates with an exact 48-byte fixed footer located at `file_size - 48`. Readers parse the footer first, validate format magic and zero-padding, and discover offsets to sparse block indexes and metadata regions.
3. **Hardware-Accelerated CRC32-IEEE Verification:** Every data block, sparse index block, MetaIndex block, and filter block contains an independent CRC32-IEEE checksum trailer. All decoders verify checksums **prior to parsing or slice extraction** (fail-closed integrity).
4. **Prefix Compression with Periodic Restarts:** Data blocks leverage prefix compression across consecutive keys, with periodic uncompressed restart points (default every 16 keys) permitting binary search within individual blocks.
5. **Two-Level Sparse Block Indexing:** In-memory lookups perform a binary search over an in-memory sparse index to identify the single candidate data block, achieving $O(\log N)$ point lookups with minimal disk I/O.
6. **Strict Anti-DoS Allocator Ceilings:** Every block handle, key length, value length, and restart count is validated against architectural hard ceilings prior to heap allocation, neutralizing decompression bombs and memory exhaustion vectors.
7. **Platform-Independent Big-Endian Wire Order:** All multi-byte numeric quantities (block handles, restart offsets, entry counts, checksums, magic words) are serialized in standard Network Byte Order (Big-Endian).

---

## 2. High-Level File Organization

A finalized Lattice SSTable file strictly adheres to the following sequential region layout:

```text
+-------------------------------------------------------------------+
| Data Block 0                                                      |
+-------------------------------------------------------------------+
| Data Block 1                                                      |
+-------------------------------------------------------------------+
| ...                                                               |
+-------------------------------------------------------------------+
| Data Block N-1                                                    |
+-------------------------------------------------------------------+
| Filter Block (Optional; Phase 05 Bloom Filter)                    |
+-------------------------------------------------------------------+
| MetaIndex Block (Pointers to Filter / Auxiliary Metadata Blocks)  |
+-------------------------------------------------------------------+
| Sparse Block Index (Pointers to all Data Blocks)                  |
+-------------------------------------------------------------------+
| Footer (Fixed 48 Bytes, Anchored at file_size - 48)               |
+-------------------------------------------------------------------+
```

Region ordering guarantees:
- Data blocks are written in strictly increasing key order from offset 0.
- Auxiliary metadata blocks (such as the Bloom filter block) follow data blocks.
- The `MetaIndex Block` records the location of metadata blocks (keyed by metadata identifier, e.g. `"filter.bloom"`).
- The `Sparse Block Index` records the location of every data block (keyed by the block's largest InternalKey).
- The `Footer` terminates the file, referencing both `MetaIndexHandle` and `IndexHandle`.

---

## 3. Fixed 48-Byte Footer Specification

The footer is anchored at the exact physical end of the file: bytes `[file_size - 48 : file_size]`.

### 3.1 Binary Wire Layout

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 MetaIndex Handle Offset (8 bytes)             |  Offset 0..7
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 MetaIndex Handle Size   (8 bytes)             |  Offset 8..15
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 Index Handle Offset     (8 bytes)             |  Offset 16..23
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 Index Handle Size       (8 bytes)             |  Offset 24..31
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                 Padding Bytes (8 bytes, All 0x00)             |  Offset 32..39
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|         Magic Number: 0x4C41545453535401 ("LATT_SST_1")      |  Offset 40..47
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

### 3.2 Field Definitions

| Offset | Field | Width | Type | Endianness | Description |
|---|---|---|---|---|---|
| `0..7` | `MetaIndexOffset` | 8 bytes | `uint64` | Big-Endian | Physical file offset of the MetaIndex block. |
| `8..15` | `MetaIndexSize` | 8 bytes | `uint64` | Big-Endian | Physical byte size of the MetaIndex block (including CRC trailer). |
| `16..23` | `IndexOffset` | 8 bytes | `uint64` | Big-Endian | Physical file offset of the sparse Block Index. |
| `24..31` | `IndexSize` | 8 bytes | `uint64` | Big-Endian | Physical byte size of the sparse Block Index (including CRC trailer). |
| `32..39` | `Padding` | 8 bytes | `[8]byte` | Raw | Reserved canonical framing. **Must be strictly 8 zero bytes (`0x00`).** |
| `40..47` | `Magic` | 8 bytes | `uint64` | Big-Endian | Format identifier: `0x4C41545453535401` (`"LATT_SST_1"` in ASCII). |

### 3.3 Decoder Validation Invariants
1. File size must be $\ge 48$ bytes (`ErrInvalidFooterSize`).
2. Magic number must exactly match `0x4C41545453535401` (`ErrInvalidFooterMagic`).
3. Padding bytes must be all zero (`ErrInvalidFooterPadding`).
4. Both handles must satisfy $Size > 0$, $Offset + Size \le \text{file\_size} - 48$, and not overflow `uint64` (`ErrInvalidBlockHandle`).
5. Handles must not overlap each other or exceed the 8 MiB maximum block size.

---

## 4. Physical Block Handle Encoding

A `BlockHandle` represents a fixed 16-byte physical pointer to a contiguous block on disk:

```text
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      Offset (uint64, 8 bytes)                 |  Offset 0..7
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      Size   (uint64, 8 bytes)                 |  Offset 8..15
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

- **Offset:** Byte position from start of file (`0 \le \text{Offset} \le \text{math.MaxInt64}`).
- **Size:** Total block byte length on disk, including trailer and checksum (`1 \le \text{Size} \le \text{MaxDataBlockSize}`).

---

## 5. Data Block Format

Data blocks store user key-value entries with prefix compression and periodic restart points.

### 5.1 Block Layout

```text
+-----------------------------------------------------------+
| Entry Data Region                                         |
|   Record 0 (Restart Point 0: SharedKeyLen = 0)            |
|   Record 1                                                |
|   ...                                                     |
|   Record 16 (Restart Point 1: SharedKeyLen = 0)           |
|   ...                                                     |
|   Record N-1                                              |
+-----------------------------------------------------------+
| Restart Array (uint32 * M, Big-Endian)                    |
|   Restart Offset 0 (uint32, must be 0)                    |
|   Restart Offset 1 (uint32)                               |
|   ...                                                     |
|   Restart Offset M-1 (uint32)                             |
+-----------------------------------------------------------+
| Restart Count (uint32 Big-Endian, 4 bytes)                |
+-----------------------------------------------------------+
| CRC32-IEEE Checksum (uint32 Big-Endian, 4 bytes)          |
+-----------------------------------------------------------+
```

### 5.2 Record Entry Wire Layout

Each record within the Entry Data Region is serialized as:
1. `SharedKeyLen` (`varint64`): Length of key prefix shared with immediately preceding key. At restart points, this is strictly `0`.
2. `UnsharedKeyLen` (`varint64`): Length of unique key suffix following the shared prefix.
3. `ValueLen` (`varint64`): Length of value payload in bytes.
4. `KeyDeltaBytes` (`UnsharedKeyLen` bytes): Raw unshared suffix bytes of the canonical `InternalKey`.
5. `ValueBytes` (`ValueLen` bytes): Raw value payload bytes.

### 5.3 Restart Array & Trailer

- **Restart Offsets:** Array of 4-byte Big-Endian integers representing byte offsets within the block where restart points begin.
  - Invariant 1: `restartOffsets[0] == 0` for any non-empty block.
  - Invariant 2: `restartOffsets[i] < restartOffsets[i+1]` (strictly monotonic).
  - Invariant 3: `restartOffsets[last] < entry_region_end`.
- **Restart Count:** 4-byte Big-Endian `uint32` indicating the number of restart offsets ($M$). Constrained to `1 \le M \le 65,536` (`MaxRestartCount`).
- **CRC32-IEEE:** 4-byte Big-Endian checksum computed across `[EntryData || RestartOffsets || RestartCount]`. Excludes the CRC field itself.

---

## 6. Sparse Two-Level Block Index Format

The sparse block index maps the largest canonical `InternalKey` of each data block to its physical `BlockHandle`.

### 6.1 Index Block Layout

```text
+-----------------------------------------------------------+
| Index Entry Data Region                                   |
|   Entry 0: KeyLen (varint), KeyBytes, BlockHandle (16B)   |
|   Entry 1: KeyLen (varint), KeyBytes, BlockHandle (16B)   |
|   ...                                                     |
|   Entry N-1                                               |
+-----------------------------------------------------------+
| Entry Offsets Region (uint32 * N, Big-Endian)             |
|   Offset 0 (uint32, must be 0)                            |
|   ...                                                     |
|   Offset N-1 (uint32)                                     |
+-----------------------------------------------------------+
| Entry Count (uint32 Big-Endian, 4 bytes)                  |
+-----------------------------------------------------------+
| CRC32-IEEE  (uint32 Big-Endian, 4 bytes)                  |
+-----------------------------------------------------------+
```

### 6.2 Index Entry Properties
- **Determinism:** Entries are appended in strictly increasing key order matching data block emissions.
- **Fail-Closed Validation:** Entry count is validated against buffer capacity before allocation:
  $$\text{entryCount} \le \frac{\text{len(block)}}{\text{BlockHandleSize} + 1}$$
- **Binary Search:** The offsets region enables direct $O(\log N)$ in-memory binary search over separator keys without linear block scanning.

---

## 7. MetaIndex Block Format

The MetaIndex block records mappings between metadata subsystem keys and auxiliary metadata block handles.

### 7.1 Canonical Metadata Keys
- `"filter.bloom"`: Block handle pointing to the table's Bloom filter block (Phase 05).
- Future metadata blocks (e.g. `"stats.keys"`, `"compact.info"`) follow the identical mapping scheme.

### 7.2 MetaIndex Layout
The MetaIndex block shares the exact structural framing as the sparse index:
```text
+-----------------------------------------------------------+
| Entry Data: KeyLen (varint) + KeyBytes + BlockHandle(16B) |
+-----------------------------------------------------------+
| Entry Offsets: uint32 * N Big-Endian                      |
+-----------------------------------------------------------+
| Entry Count: uint32 Big-Endian (4 bytes)                  |
+-----------------------------------------------------------+
| CRC32-IEEE: uint32 Big-Endian (4 bytes)                   |
+-----------------------------------------------------------+
```

### 7.3 Empty Representation
When an SSTable contains no metadata blocks (e.g. no filter), the MetaIndex block is an exact 8-byte block:
- `EntryCount = 0x00000000` (4 bytes)
- `CRC32-IEEE = 0x82A17977` (4 bytes over the 4 zero bytes)
This maintains 100% backward compatibility without changing the fixed 48-byte footer layout.

---

## 8. Bloom Filter Block Format (Phase 05)

When Bloom filtering is enabled, the filter block is emitted immediately following data blocks and before the MetaIndex block:

```text
+-----------------------------------------------------------+
| Bitset Payload (M bytes)                                  |
+-----------------------------------------------------------+
| BitCount (uint64 Big-Endian, 8 bytes)                     |
+-----------------------------------------------------------+
| HashCount k (uint8, 1 byte)                               |
+-----------------------------------------------------------+
| CRC32-IEEE Checksum (uint32 Big-Endian, 4 bytes)          |
+-----------------------------------------------------------+
```

- **Bitset Payload:** Filter bit array formatted via Murmur3-128 double-hashing ($10\text{ bits/key}$, $k=7$).
- **Trailer:** Total 13 bytes (`BitCount` 8B + `k` 1B + `CRC32` 4B).
- **Anti-DoS Bound:** Bitset size is bounded to $\le 256\text{ MiB}$ (`MaxBitsetBytes`).
- **Normative Specification:** The authoritative standalone wire format specification is documented in [`docs/bloom-filter-format.md`](file:///Users/sachinkumarsingh/Projectss/Lattice/docs/bloom-filter-format.md).

---

## 9. Architectural Size Ceilings and Hard Limits

| Metric | Constant | Value | Purpose |
|---|---|---|---|
| **Footer Size** | `FooterSize` | 48 bytes | Fixed trailer size at tail of file |
| **Block Handle Size** | `BlockHandleSize` | 16 bytes | 8B offset + 8B size |
| **Target Data Block Size** | `TargetBlockSize` | 4,096 bytes (4 KB) | Target uncompressed block flush threshold |
| **Max Data Block Size** | `MaxDataBlockSize` | 8,388,608 bytes (8 MiB) | Maximum allowed data block size |
| **Max Index Block Size** | `MaxIndexBlockSize` | 8,388,608 bytes (8 MiB) | Maximum allowed index block size |
| **Max Physical Block Size** | `MaxBlockSize` | 67,108,864 bytes (64 MiB) | Hard ceiling for any physical block |
| **Max Restart Count** | `MaxRestartCount` | 65,536 | Maximum restart points per data block |
| **Max Key Length** | `binary.MaxKeyLen` | 65,535 bytes (64 KB - 1) | User key size limit |
| **Max Value Length** | `binary.MaxValueLen`| 4,194,304 bytes (4 MB) | Value payload size limit |
| **Max Filter Bitset** | `MaxBitsetBytes` | 268,435,456 bytes (256 MiB)| Maximum filter bitset size |

---

## 10. Atomic Publication and Filesystem Safety Contract

To ensure crash consistency, durability, and symlink safety (Trust Boundary 2):
1. **Isolated Staging:** SSTable writes occur in a private temporary file created via `os.CreateTemp` in the target directory with strict `0600` permissions.
2. **Inode Pinning:** During construction, the file descriptor is statted (`fstat`) to verify it is a regular file with `Perm & 0077 == 0`.
3. **Parent Directory Pinning:** The parent directory is opened and pinned via a directory descriptor with symlink validation (`validatePathNoSymlinks`).
4. **Zero-Overwrite Publication:** Publication uses `os.Link(stagingPath, finalPath)` to atomically publish the SSTable. If `finalPath` already exists, it returns `os.ErrExist` (`ErrSSTableExists`). `os.Rename` fallback is strictly prohibited to prevent silent overwrites.
5. **Durable Directory Sync:** Prior to returning from `Finalize()`, the parent directory descriptor is explicitly synced (`SyncDir`) to guarantee the new directory entry is committed to persistent storage.

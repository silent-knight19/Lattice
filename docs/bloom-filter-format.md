# Lattice Bloom Filter Block Format Specification

**Document ID:** SPEC-BLOOM-FILTER-001  
**Status:** NORMATIVE  
**Engine Subsystem:** Storage & Probabilistic Indexing (`internal/filter`)  
**Phase Origin:** Phase 05 (Probabilistic Bloom Filter Subsystem)  
**Security Level:** Trust Boundary 2 (Storage & File I/O)

---

## 1. Scope and Architectural Objectives

This document establishes the authoritative, byte-level on-disk specification for Bloom filter blocks in Lattice. In Lattice's Log-Structured Merge-tree (LSM) storage engine, Bloom filter blocks serve as auxiliary probabilistic indices embedded within SSTable files. By testing set membership entirely in memory prior to searching SSTable block indexes and reading physical data blocks from disk, the Bloom filter eliminates unnecessary disk I/O for non-existent keys.

The format satisfies the following non-negotiable architectural and security invariants:
1. **Zero False Negatives:** For any key inserted into the filter via `Add(key)`, `MayContain(key)` is mathematically guaranteed to return `true`.
2. **Deterministic Empirical FPR (< 1%):** Sizing policy of 10 bits per key ($m/n = 10$) combined with $k = 7$ hash functions produces a theoretical false positive probability of $p \approx 0.00819$ (below 1%), targeting $\ge 99\%$ cold read disk avoidance.
3. **Hardware-Accelerated CRC32-IEEE Verification:** Every filter block terminates with a 4-byte CRC32-IEEE checksum computed over the bitset payload and metadata fields. Checksums are verified **prior to parsing or bitset allocation** (fail-closed integrity).
4. **Kirsch-Mitzenmacher Double-Hashing:** 7 discrete hash probes are generated from a single 128-bit MurmurHash3 pass using modular double hashing, preventing integer overflow and executing in $O(k)$ time with zero heap allocations.
5. **Strict Anti-DoS Memory Ceilings:** The maximum bitset capacity is capped at 256 MiB (`MaxBitsetBytes`), accommodating up to ~209.7 million keys per table while precluding allocation bombs.
6. **Immutable Shared Hashing Contract:** Both writer (`FilterBlockBuilder`) and reader (`TableReader.ReadFilterBlock`) share an identical canonical definition of the hash function, default seed (`0`), and probe count ($k = 7$). Decoder strictly rejects desynced parameters.

---

## 2. Mathematical Formulation & Sizing Policy

### 2.1 Sizing Arithmetic
For an expected key cardinality of $n$ keys:
- **Bits allocated ($m$):**
  $$m = n \times \text{BitsPerKey} = n \times 10$$
- **Bytes allocated ($N$):**
  $$N = \lceil m / 8 \rceil = \lfloor (m + 7) / 8 \rfloor$$
- **Hash function count ($k$):**
  $$k = 7$$

### 2.2 Theoretical False Positive Probability
The false positive probability $p$ after inserting $n$ keys into an $m$-bit filter with $k$ hash functions is:
$$p \approx \left(1 - e^{-k \cdot n / m}\right)^k = \left(1 - e^{-7/10}\right)^7 \approx (1 - 0.496585)^7 \approx (0.503415)^7 \approx 0.00819 \quad (\approx 0.82\%)$$

Empirical validation across 1,000,000 queries in `bloom_fpr_test.go` confirms actual false positive rates conform to $0.0082 \pm 0.0005$.

---

## 3. Binary Wire Framing Layout

A finalized Bloom filter block consists of a variable-length **Bitset Payload** followed by a fixed **13-byte Metadata & Checksum Trailer**:

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
|                   Bitset Payload (N bytes)                    |
|                N = ceil(BitCount / 8) bytes                   |
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      BitCount (uint64)                        |  Trailer Offset 0..7
|                 (Total Bit Capacity m, 8 bytes)               |  Big-Endian
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
| HashCount (k) |                                               |  Trailer Offset 8 (1 byte)
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      CRC32-IEEE Checksum                      |  Trailer Offset 9..12
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

Total wire length: $N + 13$ bytes (`len(bitset) + FilterBlockTrailerSize`).

### 3.1 Field Specifications

| Offset | Field | Width | Data Type | Endianness | Description |
|---|---|---|---|---|---|
| `0..N-1` | `Bitset` | $N$ bytes | `[]byte` | Raw | Physical bitset array storing probe bits. |
| `N..N+7` | `BitCount` | 8 bytes | `uint64` | Big-Endian | Total bit capacity $m$. Must equal $n \times 10$ for non-empty filters. |
| `N+8` | `HashCount` | 1 byte | `uint8` | N/A | Number of hash probes $k$. **Must strictly equal `0x07`.** |
| `N+9..N+12` | `CRC32` | 4 bytes | `uint32` | Big-Endian | CRC32-IEEE checksum computed across all preceding bytes (`0..N+8`). |

### 3.2 Canonical Empty Filter (13 Bytes Fixed)
When an SSTable contains zero keys or Bloom filtering is disabled, the filter block serializes to exactly 13 bytes:
- `BitCount = 0x0000000000000000` (8 zero bytes)
- `HashCount = 0x07` (1 byte, $k = 7$)
- `CRC32 = 0x8797D7DC` (4 bytes Big-Endian CRC32 over the 9 metadata bytes)

---

## 4. Hash Function & Probe Generation Specification

### 4.1 Hash Algorithm
Hash values are computed using **MurmurHash3_x64_128** with the canonical seed:
```go
const DefaultMurmur3Seed uint64 = 0
```
Input: arbitrary byte slice (`[]byte`). Output: two 64-bit unsigned integers `(h1, h2)`.

### 4.2 Kirsch-Mitzenmacher Modular Double-Hashing
For key $x$ and bitset capacity $m = \text{BitCount}$:
1. Compute 128-bit hash:
   $$(h_1, h_2) = \text{Murmur3\_128}(x, 0)$$
2. Compute modulo reductions:
   $$h_{1\text{mod}} = h_1 \pmod m, \quad h_{2\text{mod}} = h_2 \pmod m$$
3. For probe index $i \in [0, k-1]$ (where $k = 7$):
   $$\text{probe}_i = (h_{1\text{mod}} + i \cdot h_{2\text{mod}}) \pmod m$$
4. Set or test target bit:
   $$\text{byteIdx} = \lfloor \text{probe}_i / 8 \rfloor, \quad \text{bitMask} = 1 \ll (\text{probe}_i \bmod 8)$$

This modular reduction formulation guarantees exact mathematical values in $\mathbb{Z}/m\mathbb{Z}$ and eliminates 64-bit arithmetic addition or multiplication overflow prior to modulo reduction.

---

## 5. Security & Decoder Validation Invariants

Any decoder parsing a filter block from persistent storage (`DecodeFilterBlock`) must enforce the following fail-closed validation pipeline:

1. **Minimum Length Validation:**
   If `len(data) < 13`, reject with `errors.ErrFilterBlockTruncated`.
2. **Maximum Length Anti-DoS Cap:**
   If `len(data) > MaxBitsetBytes + 13` ($256\text{ MiB} + 13\text{ B}$), reject with `errors.ErrFilterBlockCorrupted` **prior to buffer allocation**.
3. **CRC32-IEEE Integrity Verification:**
   Compute CRC32-IEEE over `data[:len(data)-4]` and compare with trailing 4-byte `CRC32`. On mismatch, reject with `*errors.ChecksumMismatchError`.
4. **Strict HashCount Invariant ($k = 7$):**
   Read byte at `len(data)-5`. If $k \ne 7$, reject immediately with `errors.ErrUnsupportedHashCount`. Rejects hostile $k=0$ (bypass) and $k > 7$ (CPU exhaustion).
5. **BitCount Upper Bounds & Arithmetic Wrap Protection:**
   Read 8-byte `uint64` Big-Endian at `len(data)-13`. Reject with `errors.ErrFilterBlockCorrupted` if:
   - `bitCount > MaxKeyCount * BitsPerKey` ($2,097,152,000$ bits)
   - `bitCount > MaxBitsetBytes * 8`
   - `bitCount > math.MaxUint64 - 7`
6. **Bitset Byte Length Exact Agreement:**
   Compute expected bytes:
   $$\text{expectedBytes} = \begin{cases} 0 & \text{if } \text{bitCount} = 0 \\ \lfloor (\text{bitCount} + 7) / 8 \rfloor & \text{if } \text{bitCount} > 0 \end{cases}$$
   Verify $\text{actualBitsetBytes} = \text{len(data)} - 13 == \text{expectedBytes}$. If mismatched, reject with `errors.ErrFilterBlockCorrupted`.
7. **Defensive Allocation & Ownership:**
   Allocates an owned byte slice of length $\text{expectedBytes}$ and copies payload bytes. Re-encoding or external buffer mutations do not alias or corrupt internal state.

---

## 6. Architectural Limits & Constants

| Constant | Value | Description |
|---|---|---|
| `BitsPerKey` | `10` | Mathematical sizing policy ($m/n = 10$) |
| `DefaultHashFunctions` | `7` | Probe count ($k = 7$) |
| `DefaultMurmur3Seed` | `0` | Canonical Murmur3 64-bit seed |
| `FilterBlockTrailerSize`| `13` bytes | Fixed trailer (8B BitCount + 1B k + 4B CRC32) |
| `MaxBitsetBytes` | `268,435,456` bytes (256 MiB) | Maximum allowed physical bitset byte allocation |
| `MaxKeyCount` | `209,715,200` keys | Theoretical maximum key capacity per table |
| `FilterMetaKey` | `"filter.bloom"` | SSTable MetaIndex block entry key |

---

## 7. SSTable Integration Lifecycle

1. **Table Construction (`TableWriter`):**
   - If Bloom filtering is enabled, keys added via `TableWriter.Add` are passed to `FilterBlockBuilder.AddKey(key.UserKey)`.
   - In `TableWriter.Finish()`, the filter block is finalized via `builder.Finish()`, written sequentially after data blocks, and its `BlockHandle` is registered in the SSTable `MetaIndex` block under `"filter.bloom"`.
2. **Table Read Path (`TableReader`):**
   - `TableReader.ReadFilterBlock()` discovers the filter handle via `FindMetaIndexEntry(metaBuf, "filter.bloom")`.
   - Validates handle bounds against physical file boundaries.
   - Reads filter block bytes from disk and decodes via `filter.DecodeFilterBlock`.
   - **Thread-Safe Caching (SEC-P04-003):** Decoded `*filter.BloomFilter` is cached in `TableReader.cachedFilter` under `sync.RWMutex`, eliminating repeated disk reads and reallocations on subsequent queries.

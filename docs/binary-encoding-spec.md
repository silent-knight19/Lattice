# Lattice Binary Encoding Specification

**Document Version:** 1.0.0  
**Status:** Normative Specification  
**Governing Phase:** Phase 01 ("Core Storage Primitives & Binary Encodings")  
**Trust Boundary:** Trust Boundary 2 (Storage & File I/O) / Trust Boundary 1 (Transport Layer)  

---

## 1. Overview & Architectural Invariants

Every persistent and on-wire data structure in Lattice — Write-Ahead Log (WAL) records, SSTable data and index blocks, MANIFEST version edit records, Bloom filters, and TCP client/server frames — is built from the foundational primitives defined in `internal/binary`.

### Core Engineering Invariants
1. **Zero External Dependencies**: All encoding and decoding logic is implemented purely in Go stdlib (`encoding/binary`, `hash/crc32`, `math/bits`).
2. **Deterministic Canonical Encodings**: Equivalent logical values must always produce identical physical byte representations. Malleable or non-canonical encodings are rejected at decode time.
3. **Fail-Closed Verification**: Checksum, length, and bounds violations return typed errors (e.g., `ErrChecksumMismatch`, `ErrVarintOverflow`, `ErrVarintTruncated`) and immediately halt decoding without falling back or returning partial data.
4. **Strict Bounds Checking**: Decoders strictly limit loop iterations and buffer consumption to mathematical maximums (e.g., 10 bytes for 64-bit varints), providing provable immunity against unbounded allocations and DoS bombs.

---

## 2. Fixed-Width Integer Encoding (Big-Endian)

Lattice standardizes on **Network Byte Order (Big-Endian)** for all fixed-width integer fields in file headers, footers, checksum fields, sequence numbers, and transport frames.

### 2.1 Specification

| Type | Bit Width | Byte Width | Canonical Encoding | Standard Function |
|---|---|---|---|---|
| `uint16` | 16 bits | 2 bytes | `[B1 B0]` (MSB first) | `binary.PutUint16`, `binary.DecodeUint16` |
| `uint32` | 32 bits | 4 bytes | `[B3 B2 B1 B0]` (MSB first) | `binary.PutUint32`, `binary.DecodeUint32` |
| `uint64` | 64 bits | 8 bytes | `[B7 B6 B5 B4 B3 B2 B1 B0]` | `binary.PutUint64`, `binary.DecodeUint64` |

### 2.2 Byte Order Diagram

For a 32-bit integer `0x12345678`:

```text
Offset:    +0   +1   +2   +3
Byte:     0x12 0x34 0x56 0x78
           ^^             ^^
          MSB            LSB
```

### 2.3 Buffer Contract & Safety
- **Bounds Pre-Check**: All `PutUintXX` and `DecodeUintXX` functions assert buffer boundaries before read or write operations. If `len(buf) < size`, decoders return `errors.ErrBufferUnderflow` or panic before partial writes.

---

## 3. Variable-Length Unsigned Integer (Varint) Encoding

Lattice uses **unsigned 7-bit variable-length encoding (LEB128-style varint)** for compact length and counter prefixes in SSTable data blocks, index handles, and sparse metadata.

### 3.1 Encoding Rules

1. Each byte carries **7 payload bits** (bits 0–6) and **1 continuation bit** (bit 7, mask `0x80`).
2. Lower-order 7-bit groups are serialized first (**Little-Endian group ordering**).
3. The continuation bit (`0x80`) is set on every byte except the terminal byte:
   - `byte & 0x80 != 0`: Subsequent bytes follow.
   - `byte & 0x80 == 0`: Final byte of the varint.
4. **Bounds Enforcement**:
   - `uint64`: Maximum 10 bytes (`MaxVarintLen64 = 10`).
   - `uint32`: Maximum 5 bytes (`MaxVarintLen32 = 5`).

### 3.2 Terminal Byte & Overflow Semantics

To prevent shift overflow and illegal representations:

- **64-bit varints (`GetVarint64`)**:
  - The first 9 bytes contribute `9 * 7 = 63` bits.
  - The 10th byte can contribute **at most 1 bit** (the 64th bit).
  - Therefore, for the 10th byte `b`:
    - `b & 0x80 != 0` (continuation bit set): **ILLEGAL** (requests an 11th byte).
    - `b & 0x7E != 0` (bits 1–6 set): **ILLEGAL** (represents values $\ge 2^{64}$).
    - Any violation returns `errors.ErrVarintOverflow`.
- **32-bit varints (`GetVarint32`)**:
  - The first 4 bytes contribute `4 * 7 = 28` bits.
  - The 5th byte can contribute **at most 4 bits** ($32 - 28 = 4$).
  - If the 5th byte has `b > 0x0F`, it returns `errors.ErrVarintOverflow`.

### 3.3 Canonical Minimal-Byte Representation

Standard varint allows overlong encodings (e.g. `0` encoded as `[0x80, 0x00]`). In security-critical contexts where cryptographic non-malleability is required:
- `GetVarint64Canonical` and `GetVarint32Canonical` assert:
  $$\text{VarintLen}(\text{decodedValue}) == \text{bytesConsumed}$$
- If an overlong representation is encountered, decoding fails closed with `errors.ErrVarintNonCanonical`.

### 3.4 Varint Concrete Representation Table

| Value | Decoded (Hex) | Encoded Bytes (Hex) | Length |
|---|---|---|---|
| $0$ | `0x00` | `00` | 1 byte |
| $1$ | `0x01` | `01` | 1 byte |
| $127$ | `0x7F` | `7F` | 1 byte |
| $128$ | `0x80` | `80 01` | 2 bytes |
| $16,383$ | `0x3FFF` | `FF 7F` | 2 bytes |
| $16,384$ | `0x4000` | `80 80 01` | 3 bytes |
| $2^{32}-1$ | `0xFFFFFFFF` | `FF FF FF FF 0F` | 5 bytes |
| $2^{64}-1$ | `0xFFFFFFFFFFFFFFFF` | `FF FF FF FF FF FF FF FF FF 01` | 10 bytes |

---

## 4. CRC32-IEEE Checksum Specification

Lattice uses **CRC32-IEEE** for corruption detection across all storage files.

### 4.1 Algorithm Parameters
- **Standard**: IEEE 802.3
- **Polynomial**: `0xEDB88320` (reversed / bit-reflected representation of `0x04C11DB7`)
- **Initial Value**: `0xFFFFFFFF`
- **Final XOR**: `0xFFFFFFFF`
- **Deterministic**: Guaranteed byte-for-byte equivalence across all supported architectures (`amd64`, `arm64`).

### 4.2 Coverage Range by Subsystem

| Subsystem | Storage Artifact | Coverage Range | Checksum Location |
|---|---|---|---|
| **WAL** | Log Record | Complete record header & payload: `[SeqNum(8B) \| RecordType(1B) \| Length(4B) \| Payload(NB)]` | Trailing 4-byte Big-Endian field |
| **SSTable** | Data / Index Block | Complete serialized block payload (uncompressed or compressed) | Trailing 4-byte CRC32 in block trailer |
| **SSTable** | 48-byte Footer | Encoded footer handles | Embedded validation check |
| **MANIFEST** | Version Edit Record | Complete serialized record: `[RecordLen(4B) \| RecordPayload(NB)]` | Trailing 4-byte Big-Endian field |

### 4.3 API Contract & Fail-Closed Enforcement

The primitive layer in `internal/binary` provides two verification functions:
- `Checksum(data []byte) uint32`: Computes the CRC32-IEEE checksum without allocation.
- `Verify(data []byte, expected uint32) bool`: Returns boolean equality check.
- `VerifyChecksum(data []byte, expected uint32) error`: Strictly enforces fail-closed verification. If mismatched, returns `*errors.ChecksumMismatchError` (satisfying `errors.Is(err, errors.ErrChecksumMismatch)`) containing structured `Expected` and `Actual` diagnostic fields.

---

## 5. InternalKey Binary Layout Specification

Lattice stores and orders data records via `InternalKey`, which embeds the user key, logical sequence number, and operation type (mutation vs tombstone deletion).

### 5.1 Physical Memory & On-Disk Layout

```text
+------------------------------------+--------------------------+
|          UserKey                   |          Tag             |
|          (N Bytes)                 |        (8 Bytes)         |
+------------------------------------+--------------------------+
 0                                    N                        N+8
```

Total physical size: $\text{len}(\text{UserKey}) + 8$ bytes.

### 5.2 8-Byte Tag Decomposition

The trailing 8-byte Tag packs the sequence number and operation type:

```text
Bits 63 .................................................... 8 | Bits 7 ... 0
+-------------------------------------------------------------+--------------+
|                     SeqNum (56 bits)                        | OpType (8B)  |
+-------------------------------------------------------------+--------------+
```

1. **`SeqNum` (56 bits)**:
   - Maximum value: $2^{56}-1 = 72,057,594,037,927,935$.
   - Monotonically increasing logical timestamp assigned on write.
2. **`OpType` (8 bits)**:
   - `0x01` (`OpTypePut`): Active key-value record.
   - `0x00` (`OpTypeDelete`): Tombstone record representing deletion.
   - Any other value is rejected with `ErrInvalidOpType`.
3. **Tag Encoding**:
   $$\text{Tag} = (\text{SeqNum} \ll 8) \mid \text{uint64}(\text{OpType})$$
   Stored on disk as an 8-byte Big-Endian unsigned integer.

### 5.3 Canonical Ordering Rules

InternalKeys must strictly sort by:
1. **`UserKey` ASC**: Lexicographical ascending order (`bytes.Compare`).
2. **`SeqNum` DESC**: Descending order (newer writes appear before older writes for the same user key).
3. **`OpType` DESC**: If sequence numbers match (e.g. within the same atomic batch), `OpTypePut` ($1$) takes precedence over `OpTypeDelete` ($0$).

This guarantees that a sequential scan encountering a user key always sees its most recent state first.

---

## 6. Conformance Test Vectors

Implementations must conform to the following reference vectors:

```text
Test Vector 1: Fixed Integers (Big-Endian)
- Value 0x1234 (uint16) -> [0x12, 0x34]
- Value 0x12345678 (uint32) -> [0x12, 0x34, 0x56, 0x78]
- Value 0x0102030405060708 (uint64) -> [0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08]

Test Vector 2: CRC32-IEEE
- Input: "123456789" -> CRC32: 0xCBF43926
- Input: "" (empty)  -> CRC32: 0x00000000
- Input: "The quick brown fox jumps over the lazy dog" -> CRC32: 0x414FA339

Test Vector 3: InternalKey Encoding
- UserKey: "apple" (len 5)
- SeqNum: 100
- OpType: OpTypePut (0x01)
- Encoded Bytes (13 bytes):
  [0x61, 0x70, 0x70, 0x6C, 0x65, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x64, 0x01]
```

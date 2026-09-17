# Lattice Write-Ahead Log (WAL) Record Format Specification

**Document ID:** SPEC-WAL-RECORD-001  
**Status:** NORMATIVE  
**Engine Subsystem:** Storage & Durability (`internal/wal`)  
**Phase Origin:** Phase 02 (Write-Ahead Log & Durability Subsystem)  
**Security Level:** Trust Boundary 2 (Storage & File I/O)

---

## 1. Scope and Architectural Objectives

This document establishes the authoritative, byte-level on-disk contract for Write-Ahead Log (WAL) records in Lattice. Every mutating operation (single-key `PUT`, tombstone `DELETE`, and atomic write batches) is persisted to an active WAL segment prior to acknowledgement to caller threads.

The format satisfies the following non-negotiable invariants:
1. **Hardware-Accelerated CRC32-IEEE Fail-Closed Verification:** Bit-rot, hardware corruption, and adversarial bit flips are deterministically detected.
2. **Crash & Torn-Tail Recovery Safety:** Partial tail writes caused by unbuffered power loss or kernel panics are cleanly truncated at EOF, while mid-log corruptions halt recovery immediately.
3. **Anti-DoS Memory Ceilings:** 32-bit length fields are validated against strict ceilings prior to buffer allocation, preventing allocation bombs.
4. **Platform-Independent Endianness:** All integer fields are encoded in standard Network Byte Order (Big-Endian).
5. **Zero-Allocation Streaming:** Decoders can validate framing headers without heap allocations.

---

## 2. Binary Wire Framing Layout

Every WAL record consists of a fixed **21-byte Header** followed by a variable-length **Payload** framed by explicit length fields.

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      CRC32-IEEE Checksum                      |  Offset 0..3 (4 bytes)
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|  RecordType   |                                               |  Offset 4 (1 byte)
+-+-+-+-+-+-+-+-+                                               +
|                     Sequence Number (SeqNum)                  |  Offset 5..12 (8 bytes)
+                               +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                               |                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+                               +
|                     Timestamp (Unix Nanos)                    |  Offset 13..20 (8 bytes)
+                               +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                               |          KeyLength            |  Offset 21..22 (2 bytes)
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          KeyBytes...                          |  Offset 23..23+KeyLen-1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                         ValueLength                           |  Offset 23+KeyLen.. (4 bytes)
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                         ValueBytes...                         |  Offset 27+KeyLen..
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

---

## 3. Detailed Field Specifications

### 3.1 Header Fields (Fixed 21 Bytes)

| Offset | Field | Width | Data Type | Endianness | Description |
|---|---|---|---|---|---|
| `0..3` | `CRC32` | 4 bytes | `uint32` | Big-Endian | Hardware-accelerated CRC32-IEEE checksum computed across all bytes following this field (offsets `4` through end of record). |
| `4` | `RecordType` | 1 byte | `uint8` | N/A | Framing operation type. Valid values: `0x01` (`PUT`), `0x02` (`DELETE`), `0x03` (`BATCH_START`), `0x04` (`BATCH_COMMIT`). |
| `5..12` | `SeqNum` | 8 bytes | `uint64` | Big-Endian | Strictly monotonic database sequence number assigned at engine commit time. |
| `13..20` | `Timestamp` | 8 bytes | `uint64` | Big-Endian | Unix epoch timestamp in nanoseconds when the sequence number was assigned. |

### 3.2 Payload Fields (Variable Length)

| Offset | Field | Width | Data Type | Endianness | Description |
|---|---|---|---|---|---|
| `21..22` | `KeyLength` | 2 bytes | `uint16` | Big-Endian | Length of the user key in bytes. Constrained to `1..65,535` for `PUT`/`DELETE`; must be `0` for batch markers. |
| `23..23+KeyLen-1` | `KeyBytes` | `KeyLen` | `[]byte` | Raw | Raw user key bytes. |
| `23+KeyLen..26+KeyLen` | `ValueLength` | 4 bytes | `uint32` | Big-Endian | Length of the value payload in bytes. Constrained to `0..4,194,304` (4 MB) for `PUT`; must be `0` for `DELETE` and batch markers. |
| `27+KeyLen..end` | `ValueBytes` | `ValLen` | `[]byte` | Raw | Raw value payload bytes. |

---

## 4. Size Ceilings and Invariants

| Metric | Constant / Expression | Value | Rationale |
|---|---|---|---|
| **Header Size** | `HeaderSize` | 21 bytes | Minimum framing header containing CRC, type, seq, and timestamp. |
| **Minimum Record Size** | `MinRecordSize` | 27 bytes | Header (21B) + `KeyLength` (2B) + `ValueLength` (4B) (e.g. batch markers). |
| **Maximum Key Length** | `binary.MaxKeyLen` | 65,535 bytes (64 KB - 1) | Prevents runaway index tree fan-out. |
| **Maximum Value Length** | `binary.MaxValueLen` | 4,194,304 bytes (4 MB) | Protects process RAM from oversized payload attacks (Threat 1 / VULN-002). |
| **Maximum Record Length** | `MaxRecordLength` | 4,259,866 bytes (~4.06 MB) | Strict ceiling: `MinRecordSize` + `MaxKeyLen` + `MaxValueLen`. |

Any decoded record advertising `KeyLength > 65535`, `ValueLength > 4194304`, or total wire length exceeding `MaxRecordLength` is rejected immediately **prior to memory allocation** with `*errors.KeyTooLargeError`, `*errors.ValueTooLargeError`, or `*errors.InvalidRecordPayloadError`.

---

## 5. Record Type Enumeration

| Value (Hex) | Constant | Supported Keys | Supported Values | Semantic Meaning |
|---|---|---|---|---|
| `0x00` | `RecordTypeInvalid` | None | None | Uninitialized memory, zero-fill, or corruption. Decoders reject with `*errors.InvalidRecordTypeError`. |
| `0x01` | `RecordTypePut` | `1..65,535` bytes | `0..4,194,304` bytes | Key insertion or update. |
| `0x02` | `RecordTypeDelete` | `1..65,535` bytes | Exactly `0` bytes | Tombstone deletion. Non-zero `ValueLength` causes rejection with `*errors.InvalidRecordPayloadError`. |
| `0x03` | `RecordTypeBatchStart` | Exactly `0` bytes | Exactly `0` bytes | Opens an atomic batch sequence. Key and Value lengths must be 0. |
| `0x04` | `RecordTypeBatchCommit` | Exactly `0` bytes | Exactly `0` bytes | Commits the preceding atomic batch sequence. Key and Value lengths must be 0. |

---

## 6. Checksum Coverage & Algorithm

- **Algorithm:** CRC32-IEEE (Polynomial `0xEDB88320`), standard Go `hash/crc32.IEEETable`.
- **Coverage Range:** Exact byte slice `buf[4 : WireLength]` spanning:
  1. `RecordType` (1 byte)
  2. `SeqNum` (8 bytes)
  3. `Timestamp` (8 bytes)
  4. `KeyLength` (2 bytes)
  5. `KeyBytes` (`KeyLen` bytes)
  6. `ValueLength` (4 bytes)
  7. `ValueBytes` (`ValLen` bytes)
- **Zero-Allocation Verification:** Decoders update the running CRC incrementally as stream chunks are verified via `crc32.Update`, eliminating heap buffering.
- **Fail-Closed Mismatch:** If computed CRC != header CRC, `*errors.ChecksumMismatchError` is returned immediately.

---

## 7. Crash Recovery & Error Semantics

During engine startup recovery (`internal/wal.RecoveryCoordinator`):
1. **Clean EOF:** If reader encounters `io.EOF` at an exact record boundary (0 bytes read), recovery terminates cleanly.
2. **Torn Write at Tail:** If the final segment terminates mid-header (`1..20` bytes read) or mid-payload before complete record consumption:
   - The torn fragment is discarded.
   - The segment file is truncated to the offset of the last fully committed valid record.
   - Truncation state is preserved across restarts.
3. **Mid-Log Corruption:** If bit-rot or checksum mismatch is detected before EOF, recovery halts immediately with `errors.ErrChecksumMismatch`. The engine refuses to start, preventing silent data loss or corrupted state replay.

---

## 8. Conformance Hex Examples

### Example 1: `PUT` Record
- `Type`: `0x01` (`PUT`)
- `SeqNum`: `1` (`0x0000000000000001`)
- `Timestamp`: `1000` (`0x00000000000003E8`)
- `Key`: `"foo"` (`0x66 0x6F 0x6F`, length `3`)
- `Value`: `"bar"` (`0x62 0x61 0x72`, length `3`)
- Total Wire Size: $21 + 2 + 3 + 4 + 3 = 33$ bytes.

```text
Offsets (Hex):
00..03: [CRC32 Checksum (4B)]
04    : 01                                              (PUT)
05..0C: 00 00 00 00 00 00 00 01                         (SeqNum = 1)
0D..14: 00 00 00 00 00 00 03 E8                         (Timestamp = 1000)
15..16: 00 03                                           (KeyLength = 3)
17..19: 66 6F 6F                                        ("foo")
1A..1D: 00 00 00 03                                     (ValueLength = 3)
1E..20: 62 61 72                                        ("bar")
```

### Example 2: `DELETE` Tombstone
- `Type`: `0x02` (`DELETE`)
- `SeqNum`: `2` (`0x0000000000000002`)
- `Timestamp`: `1001` (`0x00000000000003E9`)
- `Key`: `"foo"` (length `3`)
- `Value`: Empty (length `0`)
- Total Wire Size: $21 + 2 + 3 + 4 + 0 = 30$ bytes.

```text
Offsets (Hex):
00..03: [CRC32 Checksum (4B)]
04    : 02                                              (DELETE)
05..0C: 00 00 00 00 00 00 00 02                         (SeqNum = 2)
0D..14: 00 00 00 00 00 00 03 E9                         (Timestamp = 1001)
15..16: 00 03                                           (KeyLength = 3)
17..19: 66 6F 6F                                        ("foo")
1A..1D: 00 00 00 00                                     (ValueLength = 0)
```

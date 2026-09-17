# Lattice MANIFEST Log, VersionEdit, and CURRENT Format Specification

**Document ID:** SPEC-MANIFEST-FORMAT-001  
**Status:** NORMATIVE  
**Engine Subsystem:** Storage & Version Management (`internal/version`)  
**Phase Origin:** Phase 06 (Manifest Log & VersionSet Management)  
**Security Level:** Trust Boundary 2 (Storage & File I/O)

---

## 1. Scope and Architectural Objectives

This document establishes the authoritative, byte-level on-disk specification for the `MANIFEST` log, `VersionEdit` binary records, and the `CURRENT` pointer in Lattice. The manifest subsystem coordinates crash-consistent, atomic transitions of the engine's LSM-tree version state across multiple SSTables, MemTable flushes, and background compactions.

The format satisfies the following non-negotiable architectural and security invariants:
1. **Per-Record Hardware Durability:** Every `VersionEdit` appended to the active `MANIFEST` file is framed with a hardware-accelerated CRC32-IEEE checksum and synchronized to non-volatile storage via `fdatasync()` before acknowledgement.
2. **Append-Only Immutability:** The active `MANIFEST` is opened strictly in `O_APPEND` mode with `0600` permissions. Existing records are never overwritten, modified, or truncated in place.
3. **Atomic Pointer Swapping:** The active manifest filename is discovered via the `CURRENT` file, which is updated strictly through an atomic `CURRENT.tmp` $\to$ `fdatasync` $\to$ `os.Rename` $\to$ directory `fsync` lifecycle.
4. **Deterministic TLV Binary Framing:** `VersionEdit` records are serialized using canonical Tag-Length-Value (TLV) encoding with sorted, deterministic tag sequences.
5. **Strict Anti-DoS Allocation Limits:** Record lengths, field payload lengths, and collection cardinalities are enforced prior to allocation, preventing memory exhaustion attacks.
6. **Forward Compatibility:** Unrecognized TLV tags with structurally valid lengths are skipped safely, permitting backward-compatible version evolution.

---

## 2. Global File Structure & Organization

In Lattice's directory layout (`/var/lib/lattice/`):
```text
/var/lib/lattice/
├── CURRENT            <- 16-byte ASCII pointer naming active manifest ("MANIFEST-000001\n")
├── CURRENT.tmp        <- Staging file for atomic pointer replacement
├── MANIFEST-000001    <- Active append-only manifest log recording VersionEdits
├── MANIFEST-000002    <- Successor manifest produced during manifest rotation
├── 000001.sst         <- Immutable SSTable files referenced by FileMetadata
└── ...
```

---

## 3. MANIFEST Record Binary Wire Framing

Every entry in a `MANIFEST` file consists of an **8-byte Framing Header** followed by a variable-length **VersionEdit Payload**:

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                      CRC32-IEEE Checksum                      |  Offset 0..3 (4 bytes)
|                     (Big-Endian uint32)                       |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                     PayloadLength (N bytes)                   |  Offset 4..7 (4 bytes)
|                     (Big-Endian uint32)                       |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                                                               |
|                 VersionEdit Payload (N bytes)                 |  Offset 8..8+N-1
|                                                               |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

### 3.1 Field Definitions

| Offset | Field | Width | Data Type | Endianness | Description |
|---|---|---|---|---|---|
| `0..3` | `CRC32` | 4 bytes | `uint32` | Big-Endian | CRC32-IEEE checksum computed across `record[4:]` (`PayloadLength` + `VersionEdit Payload`). |
| `4..7` | `PayloadLength` | 4 bytes | `uint32` | Big-Endian | Exact byte length $N$ of the following `VersionEdit` binary payload. |
| `8..8+N-1` | `Payload` | $N$ bytes | `[]byte` | Raw | Serialized `VersionEdit` record. |

### 3.2 Checksum Coverage Invariant
The CRC32-IEEE checksum covers **both** the 4-byte `PayloadLength` field and the $N$-byte payload:
$$\text{CRC32} = \text{ChecksumIEEE}(\text{record}[4 : 8 + N])$$
This defends against both payload bit-rot and header length corruption.

### 3.3 Record Size Bounds
- **Minimum Record Size:** 9 bytes (8-byte header + 1-byte minimal VersionEdit format byte `0x01`).
- **Maximum Record Size:** $8 + 16\text{ MiB} = 16,777,224$ bytes (`ManifestHeaderSize + MaxVersionEditBytes`). Any record advertising a length beyond this threshold is rejected prior to buffer allocation.

---

## 4. VersionEdit Binary Representation & TLV Protocol

A `VersionEdit` encodes an atomic state transition delta (new SSTables added, obsolete SSTables removed, updated sequence numbers, or next file number allocator states).

### 4.1 Wire Layout

```text
+-------------------+---------------------------------------------------+
| Format Byte (1B)  | Sequence of TLV Fields in Canonical Order...       |
| 0x01 (V1)         | [ Tag (varint) | Length (varint) | Payload... ]   |
+-------------------+---------------------------------------------------+
```

- **Offset 0:** Format version byte. Must strictly equal `0x01` (`VersionEditFormatV1`).
- **Offsets 1..end:** Sequence of TLV fields:
  - `Tag`: unsigned 64-bit varint.
  - `Length`: unsigned 64-bit varint specifying payload byte length ($\le 1\text{ MiB}$).
  - `Payload`: raw field bytes.

### 4.2 Canonical Tag Definitions

| Tag Value | Constant | Payload Structure | Canonical Ordering Rules |
|---|---|---|---|
| `1` | `TagNextFileNum` | `[ NextFileNum (varint64) ]` | First field if set. Rejects duplicates. |
| `2` | `TagLastSeqNum` | `[ LastSeqNum (varint64) ]` | Second field if set. Rejects duplicates. |
| `3` | `TagDeleteFile` | `[ Level (varint64) \| FileNum (varint64) ]` | Sorted by `(Level ASC, FileNum ASC)`. |
| `4` | `TagAddFile` | `[ Level (varint64) \| FileMetadata ]` | Sorted by `(Level ASC, FileNum ASC)`. |

### 4.3 FileMetadata Wire Encoding (TagAddFile)
The `TagAddFile` payload encodes the physical identity and key boundaries of a committed SSTable:

```text
+---------------------------------------------------------------+
| Level           : varint64                                    |
+---------------------------------------------------------------+
| FileNum         : varint64 (must be > 0)                      |
+---------------------------------------------------------------+
| FileSize        : varint64 (must be > 0)                      |
+---------------------------------------------------------------+
| SmallestSeqNum  : varint64                                    |
+---------------------------------------------------------------+
| LargestSeqNum   : varint64 (must be >= SmallestSeqNum)        |
+---------------------------------------------------------------+
| SmallestKeyLen  : varint64                                    |
+---------------------------------------------------------------+
| SmallestKey     : raw bytes (encoded binary.InternalKey)      |
+---------------------------------------------------------------+
| LargestKeyLen   : varint64                                    |
+---------------------------------------------------------------+
| LargestKey      : raw bytes (encoded binary.InternalKey)      |
+---------------------------------------------------------------+
```

### 4.4 Semantic Invariants
1. **LSM Level Range:** $0 \le \text{Level} \le 6$ (`NumLevels = 7`). Levels $\ge 7$ trigger `InvalidLevelError`.
2. **File Number Sentinels:** `FileNum = 0` is strictly reserved and rejected with `ErrInvalidFileNum`.
3. **Mutual Exclusion:** A file cannot be both added and deleted at the same level within the same edit.
4. **Key Range Disjointness (Levels 1..6):** For non-zero levels, all added SSTables must have non-overlapping, strictly monotonic key ranges:
   $$\text{CompareInternalKey}(\text{prev.LargestKey}, \text{curr.SmallestKey}) < 0$$
5. **Sequence Number Bounds:** If `LastSeqNum` is set, every added file must satisfy $\text{LargestSeqNum} \le \text{LastSeqNum}$.
6. **Presence vs. Zero-Value:** Scalars (`NextFileNum`, `LastSeqNum`) explicitly track presence bits (`hasNextFileNum`, `hasLastSeqNum`). A value of `0` is distinct from an omitted field.
7. **Forward Compatibility:** If the decoder encounters an unrecognized tag $T > 4$, it safely skips `Length` bytes without error, allowing future extensions.

---

## 5. CURRENT Pointer File Specification

The `CURRENT` file acts as the atomic root pointer to the active `MANIFEST` log.

### 5.1 Format & Size Bounds
- **Canonical Content:** `"MANIFEST-%06d\n"`
- **Exact Size:** 16 bytes for standard 6-digit manifest IDs (e.g. `"MANIFEST-000001\n"`: `4d414e49464553542d3030303030310a`).
- **Permitted Range:** $16 \le \text{len(CURRENT)} \le 30$ bytes. Files smaller than 16 bytes or larger than 30 bytes fail closed immediately (`ErrCurrentCorrupted`).
- **Strict Syntax:** No leading/trailing spaces, no tabs, no CRLF (`\r\n`), no superfluous leading zeros (e.g. `MANIFEST-0000001\n` is rejected).

### 5.2 Atomic Write & Durability Protocol
To ensure crash consistency and prevent torn reads during power failure:
```text
Step 1: Write "MANIFEST-%06d\n" to CURRENT.tmp (0600)
Step 2: fdatasync(CURRENT.tmp)
Step 3: Close(CURRENT.tmp)
Step 4: os.Rename(CURRENT.tmp, CURRENT)  <-- Atomic POSIX replacement
Step 5: syncDir(parentDir)               <-- Directory dentry fsync
```
- **Prior Pointer Safety:** If any failure occurs prior to Step 4, `CURRENT.tmp` is unlinked and the existing `CURRENT` file remains unmodified.
- **Reader Isolation:** Readers read up to 31 bytes via positional `Read` on an open file descriptor, verifying inode identity via `os.SameFile` to defend against TOCTOU substitution.

---

## 6. Manifest Rotation Protocol (IND-C-002 Specification)

As the database runs, `MANIFEST` accumulates historical `VersionEdit` records. To bound recovery time and reclaim disk space, Lattice prescribes the following normative manifest rotation protocol:

### 6.1 Rotation Invariants
1. **Prohibition of Remove-Then-Rename:** The rotation protocol **must never** execute `os.Remove(target) -> os.Rename(tmp, target)`. A remove-then-rename sequence leaves a transient window where the target does not exist and allows symlink substitution attacks.
2. **Exclusive File Creation:** New manifest files are created with `O_WRONLY | O_CREATE | O_EXCL | O_APPEND` with permissions `0600`. If the destination already exists, rotation fails closed with `ErrManifestExists`.
3. **Atomic Root Pointer Hand-off:** The active manifest identity changes **only** when `CURRENT` is atomically renamed and directory entries are synced.

### 6.2 Step-by-Step Rotation Sequence
1. **Snapshot Generation:** Generate a consolidated `VersionEdit` capturing the complete current state of the active `Version` (all active SSTables across levels 0..6, current `NextFileNum`, and `LastSeqNum`).
2. **Next Manifest Allocation:** Allocate sequential manifest number $M_{\text{next}} = M_{\text{current}} + 1$.
3. **Exclusive Manifest Creation:** Create `MANIFEST-<M_next>` via `CreateManifestWriter` (`O_EXCL`, `0600`).
4. **Consolidated LogEdit:** Append the consolidated snapshot edit to `MANIFEST-<M_next>` and synchronize via `fdatasync()`.
5. **Atomic CURRENT Update:** Invoke `SetCurrentManifest(dir, M_next)`:
   - Stage to `CURRENT.tmp`
   - `fdatasync(CURRENT.tmp)`
   - Atomic `os.Rename(CURRENT.tmp, CURRENT)`
   - Parent directory `syncDir(dir)`
6. **Obsolete Manifest Unlink:** Once `CURRENT` safely points to $M_{\text{next}}$, the prior manifest file `MANIFEST-<M_current>` is marked obsolete and scheduled for safe background deletion by the file garbage collector.

---

## 7. Security and Integrity Reference Matrix

| Threat Class | Mitigation | Enforcement Mechanism |
|---|---|---|
| **Bit-rot & Corrupted Edits** | Hardware CRC32-IEEE framing | `ManifestWriter.LogEdit`, `ManifestReader` verify CRC before parsing |
| **Torn Tail on Crash** | Bounded reading & EOF discipline | Recovery truncates clean EOF torn records; halts on mid-log corruption |
| **Allocation Bomb (DoS)** | Hard ceilings | `MaxVersionEditBytes = 16 MiB`, `MaxFieldPayloadLen = 1 MiB`, `MaxAddFiles = 10,000` |
| **Symlink Hijack / TOCTOU** | Inode pinning & strict permissions | `openFileNoFollow`, `os.SameFile`, `0600` permissions, ancestor verification |
| **Partial Pointer Read** | Atomic rename & length bounds | `os.Rename(CURRENT.tmp, CURRENT)`, `[16, 30]` byte bounds |
| **Orphaned Version Resurrect** | Atomic refcounting | `Version.Ref()` panics on dead versions; final `Unref()` unlinks from chain |

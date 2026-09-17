# Crash Recovery & Integrity Verification Specification

**Specification Version:** 1.0.0  
**Status:** NORMATIVE  
**Last Updated:** 2026-09-17  
**Subsystem:** Crash Recovery & Startup Reconstruction (`internal/version`, `internal/engine`, `internal/wal`)  

---

## 1. Overview & Architectural Scope

This specification defines the normative crash recovery and state reconstruction protocol for the Lattice storage engine (Phase 07). 

Lattice implements a multi-tier crash-consistency and durability model combining an append-only Write-Ahead Log (WAL), a sequential MANIFEST version log, an atomic `CURRENT` pointer file, and immutable on-disk SSTables. Upon startup or following an unexpected process crash, power loss, or operating system termination, Lattice reconstructs the engine's exact, linearizable state through an idempotent, fail-closed recovery pipeline.

```
+---------------------------------------------------------------------------------------------------+
|                                     LATTICE RECOVERY PIPELINE                                     |
+---------------------------------------------------------------------------------------------------+
|                                                                                                   |
|  [ Step 1: Boot Discovery ]                                                                       |
|  Inspect DB directory -> Validate & Parse CURRENT -> Pin authoritative MANIFEST descriptor        |
|                                                                                                   |
|  [ Step 2: Manifest Replay ]                                                                      |
|  Stream VersionEdits -> Enforce Replay Budgets -> Verify SSTable Presence & Size -> Build Version |
|                                                                                                   |
|  [ Step 3: WAL Replay ]                                                                           |
|  Scan WAL Segments (1..N) -> Filter (SeqNum <= Checkpoint) -> Replay Uncommitted (SeqNum > Ckpt)  |
|                                                                                                   |
|  [ Step 4: Orphan File Cleanup ]                                                                  |
|  Scan DB directory -> Clean valid staging artifacts (.tmp_*.sst_*) -> Preserve all persistent     |
|                                                                                                   |
|  [ Step 5: Atomic State Publication ]                                                             |
|  Advance SeqNum Watermark -> Advance FileNum Watermark -> Publish Active MemTable & VersionSet   |
|                                                                                                   |
+---------------------------------------------------------------------------------------------------+
```

---

## 2. Step 1: Boot Discovery & `CURRENT` Validation Protocol

Entrypoint: `version.DiscoverActiveManifest(dir string) (*version.DiscoveredManifest, error)`

### 2.1 Directory Validation & Symlink Refusal
1. The target database directory path is cleaned via `filepath.Clean(dir)`.
2. The directory is inspected via `os.Lstat(cleanDir)` (without following symlinks).
3. If the directory does not exist, `DiscoverActiveManifest` returns `errors.ErrCurrentNotFound` (matching `os.ErrNotExist`), allowing fresh database initialization.
4. If `cleanDir` is a symbolic link (`Mode()&os.ModeSymlink != 0`), discovery fails closed with `os.ErrInvalid`.
5. If `cleanDir` is a regular file or special device, discovery fails closed with `*errors.NotADirectoryError`.

### 2.2 Canonical `CURRENT` Parsing & Bounds
The `CURRENT` file establishes the single authoritative manifest sequence number.
1. `ReadCurrentManifest(dir)` opens and reads `CURRENT` using bounded stack allocation (at most 31 bytes).
2. The file length must reside strictly in the inclusive range `[16, 30]` bytes.
3. The content must strictly match the syntax:
   ```
   MANIFEST-%06d\n
   ```
   - Must start with prefix `MANIFEST-`.
   - Must terminate with exactly one newline (`\n`). Carriage returns (`\r`), multiple newlines, or trailing characters are rejected.
   - The manifest number must be valid decimal digits, strictly positive (`> 0`), without redundant leading zeros beyond the 6-digit canonical width, and must not overflow `uint64`.
4. Any syntactic or semantic error in `CURRENT` fails closed. Lattice **never** falls back to scanning or guessing other `MANIFEST-*` files in the directory.

### 2.3 Symlink Defense, Double Inode Pinning & Descriptors
To eliminate Time-of-Check to Time-of-Use (TOCTOU) substitution attacks:
1. The target path is computed via `ManifestPath(dir, manifestNum)`.
2. Containment is verified: `filepath.Dir(cleanManifestPath) == cleanDir` (preventing path traversal).
3. **Pre-Open Inspection**: `os.Lstat(cleanManifestPath)` validates the target is a regular file (not a symlink, directory, or FIFO).
4. **Symlink-Safe Open**: The file is opened with `O_RDONLY | O_NOFOLLOW` (`openFileNoFollow`).
5. **Descriptor Stat**: `fstat, err := file.Stat()` verifies the open descriptor is a regular file.
6. **Post-Open Inspection**: `lstatAfter, err := os.Lstat(cleanManifestPath)` re-inspects the path.
7. **Double Inode Pinning**: The implementation asserts:
   ```go
   os.SameFile(fstat, lstatBefore) && os.SameFile(fstat, lstatAfter)
   ```
8. **Parent Invariance**: Re-verifies `os.SameFile(dirInfo, parentLstatAfter)` to detect directory swap attacks.
9. Exclusive descriptor ownership is encapsulated in `*DiscoveredManifest`. The descriptor is pinned and passed directly to the replay stage without reopening by path.

---

## 3. Step 2: MANIFEST Replay & Version Reconstruction Protocol

Entrypoint: `version.ReplayManifest(discovered *version.DiscoveredManifest) (*version.ReplayResult, error)`

### 3.1 Streaming Framing & CRC32 Verification
1. Manifest records are read sequentially starting from physical byte offset `0`.
2. Each record is framed by an 8-byte header:
   ```
   +-----------------------+--------------------------+---------------------------+
   | CRC32-IEEE (4 Bytes)  |  PayloadLength (4 Bytes) |  VersionEdit Payload (N)  |
   | Big-Endian            |  Big-Endian              |  TLV Encoded              |
   +-----------------------+--------------------------+---------------------------+
   ```
3. Header bounds: `PayloadLength` must be between `1` and `16 MiB` (`MaxManifestRecordPayload = 16 * 1024 * 1024`).
4. **CRC32-IEEE Checksum**: Computed over `record[4:]` (`PayloadLength` + `Payload`). Mismatches fail closed immediately with `errors.ErrChecksumMismatch`.
5. Clean EOF between complete records terminates the replay loop successfully.
6. Any torn header, partial payload, or corrupted framing byte fails closed with `*version.ReplayError`.

### 3.2 Anti-DoS Resource Ceilings
Replay enforces strict runtime resource ceilings to prevent malicious or unbounded memory exhaustion:
- `MaxManifestReplayBytes` = **64 MiB**: Maximum physical stream bytes processed.
- `MaxManifestReplayRecords` = **100,000**: Maximum `VersionEdit` records decoded.
- `MaxManifestLiveFiles` = **100,000**: Maximum active SSTables permitted across all levels ($L_0..L_6$).
Exceeding any ceiling aborts recovery fail-closed with `*errors.ManifestReplayLimitError`.

### 3.3 State Reconstruction & Semantic Invariants
State accumulation is performed in an isolated in-memory `versionBuilder`:
1. **Deletions (`DeletedFiles`)**: Processed idempotently (`delete(levels[lvl], fileNum)`). Deleting a non-existent file is a safe no-op.
2. **Additions (`AddedFiles`)**:
   - Level index must satisfy `0 <= Level <= 6`.
   - Enforces **Single-Level Identity**: an active file number cannot exist across multiple levels simultaneously.
   - Enforces metadata validity: non-zero `FileNum`, non-zero `FileSize`, valid `SmallestKey` and `LargestKey`, and `SmallestKey <= LargestKey`.
3. **Monotonic Scalars**:
   - `NextFileNum` progression must be strictly non-regressive.
   - `LastSeqNum` progression must be strictly non-regressive.
4. **Physical SSTable Existence & Size Verification**:
   - For every active SSTable present in the final reconstructed version, `validatePhysicalSSTables` executes `os.Lstat` on `<dbPath>/%06d.sst`.
   - Missing SSTable -> fails closed with `errors.ErrMissingSSTable`.
   - Symlink SSTable -> fails closed with `errors.ErrSSTableSymlink`.
   - Directory / non-regular file -> fails closed with `errors.ErrNotADirectory`.
   - Physical size mismatch against `FileMetadata.FileSize` -> fails closed with `*errors.SSTableSizeMismatchError`.
   - Diagnostic provenance (`RecordIndex` and byte `Offset` of the originating `AddFile`) is reported in `*version.ReplayError`.
5. Historically deleted SSTables are **not** required to exist on disk.

---

## 4. Step 3: WAL Replay & MemTable Restoration Protocol

Entrypoint: `(e *Engine) RecoverWAL() error`

### 4.1 Segment Discovery & Continuity
1. The engine scans the `<dbPath>/wal/` directory for segment files conforming to `wal_%06d.log`.
2. Segments are sorted and replayed in ascending numeric order ($1..N$).
3. **Continuity**: The initial segment must be ID `1` (`ValidateSegmentContinuity`). Any missing segment ID (e.g. segments 1, 3 present) fails closed with `*errors.SegmentGapError`.

### 4.2 Checkpoint Filtering & Sequence Monotonicity
1. The durable sequence checkpoint is obtained from `ReplayResult.LastSeqNum` (or `0` if clean fresh DB).
2. **Filtering Contract**:
   - WAL records with `SeqNum <= checkpoint` are **skipped**. They are already durable in immutable SSTables referenced by the MANIFEST.
   - WAL records with `SeqNum > checkpoint` are replayed into a fresh, private in-memory MemTable.
3. Every WAL record framing and CRC32-Castagnoli checksum are physically validated before checkpoint comparison.
4. Global sequence monotonicity is strictly enforced across all segments. Sequence regressions fail closed with `*errors.SequenceOutOfOrderError`.

### 4.3 Batch Atomicity & Resource Ceilings
1. Atomic batches are delimited by `BATCH_START` and `BATCH_COMMIT`.
2. Bounded batch buffer limits:
   - `MaxRecoveryBatchRecords` = **10,000**
   - `MaxRecoveryBatchBytes` = **64 MiB**
3. Batches exceeding limits fail closed with `*errors.RecoveryBatchLimitError` without partial publication.
4. If an uncommitted batch is truncated at EOF in the active segment, it is safely dropped.

### 4.4 Torn-Tail Truncation vs Historical Corruption
- **Active (Latest) Segment**: A torn write or incomplete record at EOF is safely truncated back to the last valid record barrier.
- **Historical (Sealed) Segments**: Any corruption, torn write, or framing defect in a historical segment fails closed without truncation or mutation.

---

## 5. Step 4: Orphan-File Handling Policy

Lattice enforces an unambiguous, three-tier policy governing files detected on disk during startup recovery:

| File Category | Identification Pattern | Recovery Policy | Rationale |
|---|---|---|---|
| **Tier A: Staging Artifacts** | `.tmp_<name>.sst_<random>` | **Safely Unlinked** via descriptor-relative `unlinkat` | Crash-window remnants from interrupted `TableWriter` flushes or compactions. |
| **Tier B: Unreferenced SSTables** | `%06d.sst` (not in active MANIFEST) | **Preserved on Disk**; Allocator advances watermark past them | Uncommitted SSTables from interrupted compaction/flush. Preserved to prevent data loss; allocator sets `nextFileNum = max(manifest, maxPhysical + 1)` to eliminate collisions. |
| **Tier C: Unknown / Foreign Files** | `unknown.tmp`, `backup.tmp`, directories, symlinks | **Strictly Preserved**; Never modified or unlinked | Prevents arbitrary file deletion, symlink traversal attacks, and operator data destruction. |

### 5.1 Staging Artifact Deletion Rules (`IsOrphanStagingFile`)
A file is eligible for deletion **only if** its name strictly matches all conditions:
1. Starts with exact prefix `.tmp_`.
2. Contains intermediate substring `.sst_`.
3. Base name between `.tmp_` and `.sst_` is non-empty and contains no directory separators (`/`, `\`), null bytes, or directory traversals (`.`, `..`).
4. Suffix after `.sst_` is non-empty, consisting only of alphanumeric, hyphen, or underscore characters.
5. Pathological files (`CURRENT`, `CURRENT.tmp`, `MANIFEST-*`, `wal`, `*.sst`) are **never** deletion candidates.
6. Deletion is executed using `unlinkat` anchored to the open parent directory file descriptor (`os.SameFile` pinned), preventing directory substitution TOCTOU attacks.
7. Deletion errors (permissions, locks) are recorded in `CleanOrphanReport` and do **not** cause engine startup denial-of-service (`P07-SEC-005`).

---

## 6. Step 5: Crash-Window Contracts & Durability Ordering

### 6.1 Manifest Append vs. `CURRENT` Swap Ordering (IND-M-005)
The engine executes metadata commits in strict sequential order:
```
1. Write SSTable files to staging (.tmp_*.sst_*)
2. fdatasync(staging)
3. Atomic hard link: os.Link(staging, targetSST)
4. fsync(parentDir)
5. Append VersionEdit to active MANIFEST
6. fdatasync(active MANIFEST)
7. [Optional: Rotate MANIFEST if size limit reached]
   a. Create MANIFEST-(N+1) exclusively (O_EXCL)
   b. Write base VersionEdit snapshot
   c. fdatasync(MANIFEST-(N+1))
   d. Write "MANIFEST-%06d\n" to CURRENT.tmp
   e. fdatasync(CURRENT.tmp)
   f. os.Rename(CURRENT.tmp, CURRENT)
   g. fsync(parentDir)
8. Unlink obsolete input SSTables
9. fsync(parentDir)
```

### 6.2 Crash-Window Analysis & Safety
- **Crash before Step 6 (Manifest Append)**: Replay reads the existing manifest; new SSTables remain on disk as Tier B unreferenced files. Replay ignores them, and startup advances `nextFileNum` beyond them.
- **Crash between Step 6 and Step 7 (Manifest durable, CURRENT not swapped)**: Replay reads the existing `CURRENT` pointing to the previous manifest (or previous state). If the edit was appended to the active manifest, replay applies it.
- **Crash during Step 7f (`CURRENT` rename)**: `os.Rename` is atomic on POSIX filesystems. `CURRENT` either points to the old manifest or the new manifest; never a partial or corrupted pointer.
- **Crash before Step 8 (Old files deleted)**: Both old and new files exist. The reconstructed version contains only the files specified by the durable manifest. Old files are preserved on disk without resurrecting data.

---

## 7. Recovery Idempotency & In-Memory State Publication

### 7.1 Pure In-Memory Reconstruction Guarantee
1. `DiscoverActiveManifest` mutates **0 bytes** on disk (`P07-S01-M01-INV-05`).
2. `ReplayManifest` mutates **0 bytes** on disk (`P07-S01-M02-INV-09`).
3. `RecoverWAL` replays records into a private, in-memory SkipList `recoveryMem`. It mutates **0 bytes** on disk.
4. If a crash occurs *during* recovery, or if recovery encounters corrupted inputs and fails closed, the storage directory remains byte-for-byte identical to its pre-recovery state.

### 7.2 Deterministic Idempotency
Let $S_{disk}$ be the persistent storage state at startup.  
For any number of recovery executions $N \ge 1$:
$$Replay(S_{disk})_1 \equiv Replay(S_{disk})_2 \equiv \dots \equiv Replay(S_{disk})_N$$
Repeated recovery executions produce identical active MemTable contents, identical sequence watermarks, identical VersionSet hierarchies, and identical file allocation counters.

### 7.3 Atomic State Installation
State publication occurs atomically under `Engine.mu.Lock()`:
1. Active MemTable installed: `e.activeMem = recoveryMem`.
2. Immutable MemTables installed: `e.immMems = recoveryImm`.
3. Sequence counter advanced: `e.nextSeqNum.Store(max(checkpoint, wal.LastSeqNum) + 1)`.
4. File number counter advanced: `e.nextFileNum.Store(max(manifest.NextFileNum, maxPhysicalSSTNum + 1))`.
5. Reconstructed `Version` installed into `e.vset`.
6. Lifecycle state transitioned to `engineStateRecovered`.

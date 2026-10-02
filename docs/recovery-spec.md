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
|  [ Step 4: Atomic State Publication (under Engine lock, state remains engineStateRecovering) ]     |
|  Advance SeqNum Watermark -> Advance FileNum Watermark -> Publish MemTable & VersionSet           |
|                                                                                                   |
|  [ Step 5: Safe Orphan File Cleanup ]                                                             |
|  Scan DB directory -> Clean valid staging artifacts (.tmp_*.sst_*) -> Preserve all persistent     |
|                                                                                                   |
|  [ Step 6: Mark Engine Recovered ]                                                                |
|  Verify clean directory state -> Transition state to engineStateRecovered                         |
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
1. The engine scans the `<dbPath>/wal/` directory for segment files conforming to `wal_%012d.log` (twelve-digit, zero-padded; see `SegmentName` in `internal/wal/rotation.go`). Attestation sidecars (`wal_%012d.log.att`) are skipped because they do not match that grammar.
2. Segments are sorted and replayed in ascending numeric order ($1..N$).
3. **Continuity**: The initial segment must be ID `1` (`ValidateSegmentContinuity`). Any missing segment ID (e.g. segments 1, 3 present) fails closed with `*errors.SegmentGapError`.

### 4.2 Checkpoint Filtering & Sequence Monotonicity
1. The durable sequence checkpoint is obtained from `ReplayResult.LastSeqNum` (or `0` if clean fresh DB).
2. **Filtering Contract**:
   - WAL records with `SeqNum <= checkpoint` are **skipped**. They are already durable in immutable SSTables referenced by the MANIFEST.
   - WAL records with `SeqNum > checkpoint` are replayed into a fresh, private in-memory MemTable.
3. Every WAL record's framing and **CRC32-IEEE** checksum (polynomial `0xEDB88320`, Go `hash/crc32.IEEETable`) are physically validated before checkpoint comparison.

   **Integrity, not authenticity.** CRC32-IEEE is an unkeyed 32-bit checksum. It reliably detects *accidental* corruption — bit rot, hardware faults, partial writes — and it is fail-closed for those. It provides **no** protection against a deliberate attacker holding segment write access: any field (`Type`, `SeqNum`, `Timestamp`, key, value) can be altered and a valid CRC recomputed in linear time. This is asserted by the project's own suite in `internal/wal/corruption_test.go`, which forges a record, recomputes its CRC, and shows it decodes cleanly. Detecting tampering therefore depends on segment attestation (`wal_%012d.log.att`, see §4.4.1) and filesystem permissions (`0600` files in a `0700` directory), never on the per-record checksum.
4. **Global sequence monotonicity is strictly enforced across all segments.** Sequence *regressions* fail closed with `*errors.SequenceOutOfOrderError`.

5. **Sequence *gaps* are intentionally NOT enforced.** A record whose `SeqNum` exceeds the previous record's by more than one is observed, counted, and accepted. `RecoveryReport` exposes `SequenceGaps` (number of discontinuities) and `SkippedSequenceNumbers` (total sequence numbers absent), and each event increments `lattice_wal_sequence_gaps_total`. Recovery never rejects on a gap.

   **Why this must stay non-enforcing.** Three code paths produce legitimate gaps, so requiring `SeqNum == prev + 1` would refuse to open healthy databases:

   | Source | Location |
   |---|---|
   | `Put` / `Delete` allocate a `SeqNum`, then fail to persist a record; `nextSeqNum` is never rolled back | `internal/engine/engine.go` (`allocSeqNumLocked` precedes `wal.AppendSync` on every write path) |
   | `Batch` reserves `N+2` sequence numbers up front and can fail mid-loop, leaving the remainder unpersisted | `internal/engine/engine.go` (batch append loop) |
   | Post-recovery the engine seeds `nextSeqNum` from `max(manifest checkpoint, WAL last SeqNum)`, so the next write legitimately jumps | `internal/engine/engine.go` recovery publication step |

   The third is decisive. When the MANIFEST checkpoint exceeds the WAL's highest `SeqNum` — which is exactly what happens once WAL segment GC removes segments already covered by a flush — every subsequent write starts above the WAL's last sequence number. Strict contiguity would then reject **every** database on **every** boot, permanently. Segment GC is already specified (`docs/architecture-spec.md` §WAL retention), so this is a scheduled landmine rather than a hypothetical.

   **What a gap does and does not mean.** A non-zero `SkippedSequenceNumbers` is an observability signal, not an integrity verdict. Whole-record *loss* from truncation is detected separately by segment attestation (§4.4.1); a gap in sequence numbering does not by itself indicate missing data.

   > **Do not "fix" this by enforcing `SeqNum == prev + 1`.** It will appear to work in testing (where the engine is the only writer and gaps are rare) and will brick every existing database in production. If contiguity is ever wanted, the allocator must first roll back `nextSeqNum` on every failure path *and* reconcile the manifest checkpoint against the WAL, which requires a migration story for existing databases.

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

#### 4.4.1 Segment Attestation

Per-record CRC32 cannot detect the loss of *whole* records. A segment tail removed exactly at a record boundary decodes as a clean `io.EOF`, so before attestation existed recovery reported success with silently missing acknowledged writes. To close that, every segment carries a sidecar `wal_%012d.log.att` recording what it held when it was sealed or last observed:

| offset | size | field |
|---|---|---|
| 0 | 4 | magic (`WATT`) |
| 4 | 2 | version |
| 6 | 2 | flags (bit 0 = sealed) |
| 8 | 8 | recordCount |
| 16 | 8 | finalOffset |
| 24 | 8 | lastSeqNum |
| 32 | 4 | CRC32-IEEE over bytes `[0:32)` |

Seal-time attestations are written by `RotatingWriter` when a segment rotates; the active segment is attested **unsealed** on `Close`, because `OpenRotatingWriter` reopens and appends to it.

Recovery decision table:

1. **Absent sidecar** → tolerated, counted in `lattice_wal_attestation_absent_total`. This is the normal state for any database created before attestation existed and for segments never sealed. It is explicitly **not** a verification pass, and it never fails recovery — otherwise every pre-existing database would be unopenable.
2. **Corrupt sidecar** (bad size, magic, version, flags, or CRC) → fails closed with `ErrAttestationCorrupted`. Ignoring it would re-open the hole attestation exists to close.
3. **Sealed segment diverged** → fails closed with `ErrAttestationMismatch`, with no truncation. A sealed segment is immutable by construction, so any divergence means whole records were lost or the file was modified out of band.
4. **Active segment shrank or lost records** → fails closed with `ErrAttestationMismatch`. The active segment may legitimately grow, so growth is accepted and the sidecar is refreshed after recovery.
5. **Matches** → pass.

The active segment's attestation is verified *before* `RecoverSegment` runs, so evidence of an earlier boundary-aligned truncation is not erased by this recovery pass's own torn-tail repair. The sidecar is then rewritten to the post-truncation baseline, so a legacy database upgrades itself on first recovery.

Because a sealed segment's attested length is final, a crash between sealing and writing the sidecar degrades to case 1 (absent), which is safe.

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

### 7.3 Atomic State Installation & Rollback
Preconditions and validations (including directory scanning for SSTables and staging artifacts, as well as `math.MaxUint64` overflow checks) are evaluated **prior to** mutating any live Engine state.
State publication occurs atomically under `Engine.mu.Lock()` with a pre-recovery rollback snapshot:
1. Capture rollback snapshot: `origActiveMem`, `origImmMems`, `origSeqNum`, `origFileNum`, `origBackpressure`, `origCleanerReport`, and initial `VersionSet` watermarks.
2. Active MemTable installed: `e.activeMem = recoveryMem`.
3. Immutable MemTables installed: `e.immMems = recoveryImm`.
4. Sequence counter advanced: `e.nextSeqNum.Store(max(checkpoint, wal.LastSeqNum))`. (The subsequent write increments via `Add(1)`).
5. File number counter advanced: `e.nextFileNum.Store(max(manifest.NextFileNum, maxPhysicalSSTNum + 1))` (scanning both SSTables and staging artifacts, preventing overflow).
6. Reconstructed `Version` installed into `e.vset`.
7. Backpressure controller synchronized with recovered memory byte count.
8. Engine lifecycle state remains `engineStateRecovering` throughout subsequent orphan staging cleanup, blocking concurrent mutations and point lookups.
9. **Atomic Rollback on Critical Failure**: If orphan staging cleanup encounters a critical error (such as directory synchronization failure or parent directory replacement) or if the Engine was closed concurrently, `rollbackPublishedRecovery` is invoked:
   - Restores `activeMem`, `immMems`, `nextSeqNum`, `nextFileNum`, `lastCleanerReport`, and backpressure usage.
   - Detaches the reconstructed Version via `vs.RollbackAppendedVersion`, unlinking it from the circular chain, restoring VersionSet watermarks, and dropping ownership references.
   - Resets Engine lifecycle state to `engineStateNotRecovering`.
   - Leaves the Engine in its exact pre-recovery state so subsequent recovery attempts succeed.
10. After orphan staging cleanup succeeds without critical directory sync error, lifecycle state transitions to `engineStateRecovered`.

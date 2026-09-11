# SEC-P04 Independent Security Re-Audit & Remediation Report

* **Repository**: `https://github.com/silent-knight19/Lattice`
* **Branch**: `main`
* **Audited Commit Baseline**: `ba954b688096f5ae13a8c431da48a60492380603`
* **Audit Scope**: Phase 00 through Phase 04 (Persistent SSTable Subsystem, WAL & Recovery, MemTable/SkipList, Binary Primitives, Errors, Logger)
* **Date**: September 2026
* **Status**: COMPLETE

---

## 1. Executive Summary

### Final Phase 04 Verdict: **PASS**

No confirmed Critical or High security vulnerabilities remain within the audited Phase 00–04 scope.

All four mandatory remediations—structural validation of SSTable index InternalKeys, enforced allocation boundaries on index and data blocks, strict rejection of insecure file modes, and WAL writer poisoning upon I/O or sync failures—have been successfully implemented, verified with comprehensive unit and integration tests, and subjected to fuzz testing and the Go race detector.

During the subsequent adversarial re-audit, three additional findings (one Medium, one Low, and one Informational) regarding data block parser key validation, value length bounding, and heuristic dead code removal were identified and remediated.

---

## 2. Scope & Boundaries

### In-Scope Components (Phases 00–04)
* **Binary Primitives & Codecs (`internal/binary`)**: `InternalKey`, `PutVarint64`, `GetVarint64`, `PutUint*`, `GetUint*`, `Checksum`, validation bounds (`ValidateKey`, `ValidateValue`), canonical comparators.
* **Write-Ahead Log (`internal/wal`)**: `WALWriter`, `Record` framing, checksums, sync lifecycle, poisoning state machine, recovery coordinator, directory scanning, and segment management.
* **In-Memory MemTable & SkipList (`internal/memtable`)**: `SkipList`, concurrent lock-free readers, atomic pointer publication, height generator (`PCG32`), freeze lifecycle, memory byte accounting.
* **Persistent SSTables (`internal/sstable`)**: Data blocks, restart point framing, prefix compression, sparse two-level index (`IndexBuilder`, `BlockIndex`), `BlockHandle`, `Footer`, `TableWriter` (atomic staging, directory sync, mode validation), `TableReader` (bounds-checked allocations, positional reads, search).
* **Cross-Cutting Support**: Structured errors (`internal/errors`), privacy-safe logger and redaction (`internal/logger`).

### Explicitly Excluded Future Components (Phase 05+)
* Bloom Filters (Phase 05)
* Block Cache / LRU Cache (Phase 05)
* Multi-SSTable Two-Level Iterator / Manifest (Phase 06)
* Leveled Compaction (Phase 07)
* Distributed Raft Consensus & Replication (Phase 08)
* Network Wire Protocol & Client Drivers (Phase 09+)

---

## 3. Findings Register

| ID | Severity | Component | File/Line | Problem | Impact | Evidence | Fix | Status |
|---|---|---|---|---|---|---|---|---|
| **FINDING-SEC04-01** | High | SSTable Index | `internal/sstable/index_builder.go:465` | Index entries permitted arbitrary bytes as `LargestKey` and heuristically guessed at runtime whether keys were `InternalKeys` or raw bytes. | Inconsistent trust boundary; type confusion; search ordering errors. | `IndexEntry.UserKey()` attempted decode and fell back to raw bytes; `compareIndexEntryKeys` heuristic. | Replaced with strict `binary.DecodeInternalKey` in `DecodeBlockIndex` and `IndexBuilder.AddBlock`. `IndexEntry` stores validated `InternalKey`. | **RESOLVED** |
| **FINDING-SEC04-02** | High | SSTable Reader | `internal/sstable/table_reader.go:128, 251` | File-supplied block handle sizes directly drove `make([]byte, size)` allocations up to file bounds, allowing memory exhaustion via sparse files. | Potential Denial of Service (OOM) on large sparse or corrupted SSTables. | Allocation `make([]byte, int(indexHandle.Size))` executed without explicit upper limit. | Introduced `MaxDataBlockSize = 8 MiB` and `MaxIndexBlockSize = 8 MiB`. Validated size overflow-safely before memory allocation. | **RESOLVED** |
| **FINDING-SEC04-03** | Medium | SSTable Writer | `internal/sstable/table_writer.go:162` | Configurable `FileMode` permitted callers to specify permissive modes (e.g. `0644`, `0666`, `0777`), bypassing the documented `0600` baseline. | Information disclosure; local unprivileged users reading database files. | Unit tests and constructors accepted `0644` without restriction. | Implemented `ValidateFileMode` strictly rejecting non-owner bits (`0077`) and execution bits (`0111`) with `ErrInsecureFileMode`. | **RESOLVED** |
| **FINDING-SEC04-04** | High | WAL Writer | `internal/wal/writer.go:142, 237` | Partial write or sync failure left `WALWriter` open and usable, risking subsequent appends after torn records (middle-of-WAL corruption). | WAL corruption; unrecoverable state where valid records follow a torn record. | `Append` returned I/O error but did not poison writer state; subsequent `Append` could succeed. | Added `poisoned` state machine in `WALWriter`. On write error, short write, or sync failure, writer transitions to poisoned and fails fast with `ErrWriterPoisoned`. | **RESOLVED** |
| **FINDING-SEC04-05** | Medium | SSTable Data Block | `internal/sstable/table_reader.go:371, 455` | `searchDataBlock` lacked structural `InternalKey` validation at restart points and linear scans, potentially accepting malformed or disordered keys. | Data corruption bypass; incorrect search termination or empty user key acceptance. | Slicing `entrySlice[hdrLen:hdrLen+int(unshared)-9]` without validating minimum length (10B) or user key validity. | Added `binary.DecodeInternalKey` validation for restart entries and reconstructed keys. Enforced monotonic ordering during scan. | **RESOLVED** |
| **FINDING-SEC04-06** | Low | SSTable Data Block | `internal/sstable/table_reader.go:364, 430` | Value length in data block entries was bounded only by block size (8 MiB) rather than `binary.MaxValueLen` (4 MiB). | Inconsistent validation boundary; memory over-allocation on corrupted records. | `valueLen` varint accepted up to block buffer capacity without checking `MaxValueLen`. | Enforced `valueLen <= binary.MaxValueLen` check before reading or copying value bytes. | **RESOLVED** |
| **FINDING-SEC04-07** | Informational | SSTable Index | `internal/sstable/index_builder.go:567` | Heuristic function `compareIndexEntryKeys` remained in codebase as dead code after index refactoring. | Dead code; developer confusion regarding index comparison semantics. | `compareIndexEntryKeys` flagged by `golangci-lint` (unused). | Removed function completely. | **RESOLVED** |

---

## 4. Remediation Details

### Remediation 1: Structurally Validated InternalKey Index Representation
* **Root Cause**: Index entries previously accepted arbitrary byte sequences as `LargestKey`. Methods such as `IndexEntry.UserKey()`, `FindBlock`, and `compareIndexEntryKeys` relied on heuristic trial decoding, treating failures as raw byte keys.
* **Implementation**:
  - `IndexEntry` struct was redesigned to explicitly store `Key binary.InternalKey`.
  - `IndexEntry.UserKey()` directly returns `e.Key.UserKey` without runtime heuristics or fallback decoding.
  - `IndexBuilder.AddBlock` decodes and validates `largestKey` via `binary.DecodeInternalKey` prior to any state mutation, ensuring failure atomicity and canonical LSM ordering (`binary.CompareInternalKey`).
  - `DecodeBlockIndex` strictly validates every serialized index entry using `binary.DecodeInternalKey`, ensuring that any successfully decoded index contains only valid, monotonic `InternalKeys`.
  - Search APIs (`FindBlock`, `FindBlockKey`, `FindBlockInternalKey`) enforce clean separation: `FindBlock` operates strictly on user keys, while `FindBlockKey` and `FindBlockInternalKey` operate strictly on structured internal keys.
* **Regression Tests**:
  - `TestSecurity_Remediation1_DecodeBlockIndex_MalformedKeys`: Verifies rejection of keys < 10 bytes, invalid `OpTypes`, empty user keys, and non-monotonic entries.
  - `TestSecurity_Remediation1_IndexBuilder_AddBlock_Validation`: Verifies failure atomicity and input rejection.
  - `TestSecurity_Remediation1_PropertyInvariant_DecodedKeysAreInternalKeys`: Invariant test proving all decoded index keys satisfy `InternalKey` properties.
  - `TestSecurity_Remediation1_FindBlock_Ordering`: Verifies multi-version ordering across point lookups.

### Remediation 2: Explicit Resource Limits for Block Allocations
* **Root Cause**: `TableReader` allocated byte buffers directly based on file-supplied `BlockHandle.Size` (e.g. `make([]byte, handle.Size)`), exposing memory exhaustion risks on large sparse files.
* **Implementation**:
  - Defined explicit security constants in `internal/sstable/block_builder.go`:
    ```go
    MaxDataBlockSize  uint64 = 8 * 1024 * 1024 // 8 MiB
    MaxIndexBlockSize uint64 = 8 * 1024 * 1024 // 8 MiB
    ```
  - `Footer.ValidateAgainstFileSize` rejects `IndexHandle.Size > MaxIndexBlockSize`.
  - `TableReader.NewTableReaderWithFile` and `TableReader.Seek` bounds-check handle sizes and file offsets overflow-safely *before* invoking `make([]byte, int(size))`.
  - Architecture-dependent integer limits (`math.MaxInt`) are explicitly verified to prevent negative or wrapped slice indices on 32-bit platforms.
* **Regression Tests**:
  - `TestSecurity_Remediation2_AllocationLimits`: Verifies that oversized index blocks (> 8 MiB), huge `math.MaxUint64` handles, and oversized data blocks (> 8 MiB) are rejected before heap allocations occur.

### Remediation 3: Filesystem Permission Baseline Enforcement
* **Root Cause**: `TableWriterOptions.FileMode` accepted arbitrary permission bits from callers, allowing insecure modes (e.g. `0644`, `0666`) to override the documented owner-only (`0600`) baseline.
* **Implementation**:
  - Added `ValidateFileMode(mode os.FileMode) error` in `internal/sstable/table_writer.go`:
    ```go
    func ValidateFileMode(mode os.FileMode) error {
        perm := mode.Perm()
        if perm&0077 != 0 || perm&0111 != 0 || perm&0400 == 0 {
            return &errors.InsecureFileModeError{RequestedMode: mode}
        }
        return nil
    }
    ```
  - Enforced across `NewTableWriter` and `NewTableWriterWithFile`. Insecure modes return `ErrInsecureFileMode` and immediately abort without leaving staging files.
* **Regression Tests**:
  - `TestSecurity_Remediation3_FilePermissions_Baseline`: Verifies that `0644`, `0666`, `0755`, `0777`, `0640`, `0604`, and `0700` are strictly rejected with `ErrInsecureFileMode`, while `0600` and `0400` succeed.

### Remediation 4: WAL Writer Poisoning on Write / Sync Failures
* **Root Cause**: An I/O error or short write in `WALWriter.Append()` or `AppendSync()` could leave the writer in an open state. Subsequent appends could succeed, placing valid records after a torn record and creating an unrecoverable corrupted middle-of-WAL state.
* **Implementation**:
  - Added `poisoned bool` and `poisonErr error` fields to `WALWriter`.
  - On write failure or short write in `appendLocked()`, or on fsync failure in `syncLocked()`, `w.poisoned = true` and `w.poisonErr = err`.
  - `Append`, `AppendSync`, `Sync`, and `Size` check `w.poisoned` under lock and fail fast with `*errors.WALWriterPoisonedError` (unwrapping to `ErrWriterPoisoned`).
  - `Close()` releases the underlying file descriptor, suppresses further sync operations, and propagates `ErrWriterPoisoned`.
* **Regression Tests**:
  - `TestSecurity_Remediation4_WALWriterPoisoning_PartialWrite`: Injects fault after partial byte write; asserts writer transitions to poisoned and rejects subsequent appends.
  - `TestSecurity_Remediation4_WALWriterPoisoning_SyncFailure`: Injects sync fault; asserts writer poisoning.
  - `TestSecurity_Remediation4_WALWriterPoisoning_ZeroByteError`: Injects immediate write error; verifies poisoning.
  - `TestSecurity_Remediation4_WALWriterPoisoning_RecoveryIntegration`: Asserts recovery truncates partial tail and fresh writer continues cleanly without corrupted middle records.
  - `TestSecurity_Remediation4_WALWriterPoisoning_ConcurrentRaces`: Runs concurrent appends during fault injection under `-race`.

---

## 5. Independent Second-Pass Audit Results

During the adversarial second pass across all Phase 00–04 subsystems, three additional improvements were identified and implemented:

1. **Data Block Parser InternalKey Validation (`FINDING-SEC04-05`)**:
   - `searchDataBlock` in `internal/sstable/table_reader.go` was updated to decode restart entries and reconstructed keys using `binary.DecodeInternalKey`.
   - Verified that all data block entries satisfy minimum length (10B), valid `OpType`, valid user key bounds, and strictly increasing monotonic ordering during linear forward scans.
2. **Data Block Value Length Cap (`FINDING-SEC04-06`)**:
   - `searchDataBlock` was hardened to verify `valueLen <= binary.MaxValueLen` (4 MiB), preventing corrupted blocks from triggering over-allocations.
3. **Dead Heuristic Code Removal (`FINDING-SEC04-07`)**:
   - Removed unused heuristic function `compareIndexEntryKeys` from `internal/sstable/index_builder.go`.

---

## 6. Verification Results

All verification commands were executed from the repository root:

```bash
# 1. Full Package Test Suite
go test ./...
# Result: ok (all packages passed)

# 2. Complete Race Detection Suite
go test -race ./...
# Result: ok (zero data races detected across entire codebase)

# 3. Static Analysis
go vet ./...
# Result: ok (zero issues)

# 4. Linter Analysis
golangci-lint run ./...
# Result: 0 issues

# 5. SSTable Fuzz Testing
go test -fuzz=FuzzBlockIndex_Decode -fuzztime=10s ./internal/sstable
# Result: PASS (4,836,125 iterations in 10s, 0 crashes)

go test -fuzz=FuzzTableReader_Seek -fuzztime=10s ./internal/sstable
# Result: PASS (4,789,966 iterations in 10s, 0 crashes)

go test -fuzz=FuzzDecodeFooter -fuzztime=5s ./internal/sstable
# Result: PASS (2,559,431 iterations in 5s, 0 crashes)

go test -fuzz=FuzzBlockHandle_Decode -fuzztime=5s ./internal/sstable
# Result: PASS (2,482,492 iterations in 5s, 0 crashes)

# 6. Binary & WAL Fuzz Testing
go test -fuzz=FuzzInternalKeyCodec -fuzztime=5s ./internal/binary
# Result: PASS (546,217 iterations in 5s, 0 crashes)

go test -fuzz=FuzzRecordCodec -fuzztime=5s ./internal/wal
# Result: PASS (1,438,833 iterations in 5s, 0 crashes)
```

---

## 7. Remaining Known Limitations (Phase 04 Boundary)

1. **Single-SSTable Scope**: Table reading in Phase 04 operates on single SSTable files. Multi-SSTable reconciliation, version sets, and point lookup fallback across levels belong to Phase 06.
2. **No Secondary Indexing**: SSTables index records strictly by canonical `InternalKey` (`UserKey ASC`, `SeqNum DESC`, `OpType DESC`).
3. **No Dynamic Block Caching**: Every point lookup in Phase 04 reads candidate data blocks via positional `ReadAt`. In-memory caching belongs to Phase 05 (LRU cache).
4. **No Bloom Filter Filtering**: In Phase 04, negative lookups inspect the sparse index and may perform one data block read before returning `ErrKeyNotFound`. Bloom filters belong to Phase 05.
5. **No Network Layer**: All storage engine operations are in-process. Distributed RPC, authentication, TLS, and wire framing belong to later phases.

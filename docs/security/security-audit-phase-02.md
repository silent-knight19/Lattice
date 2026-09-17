# Phase 02 Security Audit & Seal Report — Lattice

**Audit ID:** SEC-AUDIT-P02-SEAL-2026-09  
**Target Repository:** `github.com/silent-knight19/lattice`  
**Phase Audited:** Phase 02 ("Write-Ahead Log (WAL) & Durability Subsystem")  
**Audit Scope:** Write-ahead log record serialization, sequential reader & iterators, group commit batch queue, crash recovery coordinator, segment rotation, symlink-safe file creation, and parent directory pinning  
**Final Verdict:** 🟢 **PASS (Hermetically Sealed & Remediated)**

---

## 1. Executive Summary

Phase 02 delivered the Write-Ahead Log (WAL) and Durability Subsystem — the critical durability boundary for every committed transaction in Lattice.

An external security audit report (`SEC-AUDIT-P02-2026-09`) confirmed the scope re-scoping (establishing that Phase 02 is the WAL and durability subsystem, while Phase 03 is the MemTable and SkipList) and identified four findings. This seal document records the investigation, remediation, and automated test evidence resolving all four findings.

### Findings Summary & Resolution Matrix

| Finding ID | Severity | Title | Initial Classification | Final Status | Resolution Details |
|---|---|---|---|---|---|
| **SEC-P02-001** | Medium | WAL parent directory symlink pinning gap | True Positive (Nuanced) | **Remediated** | Hardened `OpenWriter`, `CreateWriter`, and `RotatingWriter` against parent directory symlink attacks and runtime directory displacement. Added ancestor verification (`isSystemSymlinkPrefix`), parent directory `os.Lstat` checks, and post-open/post-rotation `os.SameFile` inode pinning. Covered by `TestCreateWriter_ParentSymlink_Rejected`, `TestOpenWriter_ParentSymlink_Rejected`, and `TestRotatingWriter_ParentDirectorySwapped_FailsClosed`. |
| **SEC-P02-002** | Medium | No standalone WAL record format specification document | True Positive | **Remediated** | Authored normative specification document [`docs/wal-record-format.md`](../wal-record-format.md) detailing framing layout, 21-byte header, record type enumeration, CRC32-IEEE coverage range, Big-Endian encoding, and anti-DoS ceilings. Cross-referenced from [`docs/threat-model.md`](../threat-model.md) under Threat 3. |
| **SEC-P02-003** | Low | WAL writer poisoning error taxonomy leaks internal host paths | True Positive | **Remediated** | Updated `WALWriterPoisonedError.Error()` in `internal/errors/errors.go` to sanitize `e.Path` using `filepath.Base`, preventing leakage of host directory trees and user profiles. Verified by `TestWALWriterPoisonedError` in `internal/errors/errors_test.go`. |
| **SEC-P02-004** | Informational | Phase 02 audit artifact bundled with later phases; no standalone seal document | True Positive | **Remediated** | Created this standalone Phase 02 seal document (`docs/security/security-audit-phase-02.md`). |

---

## 2. Detailed Technical Cross-Checks & Remediations

### 2.1 Scope Re-Scoping Confirmation

The audit report explicitly re-scoped Phase 02 to the **Write-Ahead Log (WAL) & Durability Subsystem**, noting that the initial prompt mistakenly conflated Phase 02 with the MemTable / SkipList (which is Phase 03 in `docs/implementation-plan.md` §19 lines 389–393).

This audit seal confirms that Phase 02 covers:
- WAL record encoding & decoding (`internal/wal/record.go`)
- Checksum integrity and bit-rot corruption detection (`internal/wal/corruption_test.go`)
- Sequential WAL reader & log iterator (`internal/wal/reader.go`)
- High-throughput group commit batch runner (`internal/wal/queue.go`)
- Startup crash recovery and torn-write truncation (`internal/wal/recovery.go`)
- Multi-segment rotation, directory synchronization, and symlink protection (`internal/wal/rotation.go`, `dir.go`, `writer.go`).

### 2.2 SEC-P02-001: Parent Directory Symlink Pinning & Inode Invariance

* **Vulnerability Description:** While individual WAL segment files (`wal_*.log`) were opened with `O_NOFOLLOW`, the parent directory was not checked prior to segment creation. An unprivileged local attacker with write access to parent path components could plant a symlink at the WAL directory path, redirecting segment creation to an unauthorized location.
* **Remediation Implemented:**
  1. **`internal/wal/dir.go`:** Added `isSystemSymlinkPrefix` to distinguish legitimate Darwin macOS system symlinks (`/var`, `/tmp`, `/etc`) from unpermitted symlinks.
  2. **`internal/wal/writer.go`:** Updated `OpenWriter` and `CreateWriter`:
     - Inspects `parentDir := filepath.Dir(cleanPath)` via `os.Lstat`.
     - Strictly rejects any unpermitted symlink parent with `errors.ErrParentDirectorySymlink`.
     - Enforces that `parentDir` is a genuine directory (`errors.ErrNotADirectory`).
     - Post-creation/post-open verification: Re-stats `parentDir` and asserts `os.SameFile(pInfo, postParentInfo)`, eliminating TOCTOU directory swap windows.
  3. **`internal/wal/rotation.go`:** In `RotatingWriter`:
     - Captures authoritative `dirInfo` during initialization.
     - In `rotateLocked()`, re-inspects `walDir := Dir(rw.dbPath)` before creating segment $N+1$.
     - Rejects any symlink swap with `errors.ErrParentDirectorySymlink` and rejects any replaced inode with `errors.ErrParentDirectorySwapped`.
     - On any directory anomaly, rotation aborts immediately and the writer fails closed (`rw.active = nil`).
  4. **Regression Testing (`internal/wal/symlink_pinning_test.go`):**
     - `TestCreateWriter_ParentSymlink_Rejected`: Verifies that `CreateWriter` refuses to create files when the parent is a symlink.
     - `TestOpenWriter_ParentSymlink_Rejected`: Verifies `OpenWriter` rejects parent symlinks.
     - `TestRotatingWriter_ParentDirectorySwapped_FailsClosed`: Verifies that if `walDir` is displaced or replaced by a symlink during runtime, rotation fails closed and prevents writes to the foreign directory.

### 2.3 SEC-P02-002: Standalone WAL Record Format Specification

* **Defect Description:** The WAL on-disk byte contract was defined only in Go code (`record.go`) without a normative specification document.
* **Remediation Implemented:**
  - Created [`docs/wal-record-format.md`](../wal-record-format.md), establishing:
    - 21-byte Header layout: `[ CRC32 (4B) | RecordType (1B) | SeqNum (8B) | Timestamp (8B) ]`
    - Payload framing: `[ KeyLength (2B) | KeyBytes | ValueLength (4B) | ValueBytes ]`
    - Big-Endian encoding across all integer fields
    - CRC32-IEEE checksum covering offsets 4 through record end
    - Anti-DoS bounds: `MinRecordSize = 27`, `MaxKeyLen = 65,535`, `MaxValueLen = 4,194,304`, `MaxRecordLength = 4,259,866` bytes
    - Operational semantics for `PUT (0x01)`, `DELETE (0x02)`, `BATCH_START (0x03)`, `BATCH_COMMIT (0x04)`.
  - Cross-referenced from [`docs/threat-model.md`](../threat-model.md) under Threat 3.

### 2.4 SEC-P02-003: WAL Writer Poisoning Error Taxonomy Sanitization

* **Defect Description:** `WALWriterPoisonedError.Error()` formatted the raw absolute host filesystem path (`e.Path`), which could disclose internal server directory hierarchies and user account names if propagated to client errors.
* **Remediation Implemented:**
  - In `internal/errors/errors.go`:
    ```go
    func (e *WALWriterPoisonedError) Error() string {
        if e == nil {
            return ErrWriterPoisoned.Error()
        }
        name := filepath.Base(e.Path)
        if name == "" || name == "." {
            name = "wal segment"
        }
        if e.Reason != nil {
            return fmt.Sprintf("wal writer for %q is poisoned: %v", name, e.Reason)
        }
        return fmt.Sprintf("wal writer for %q is poisoned", name)
    }
    ```
  - Added unit test `TestWALWriterPoisonedError` in `internal/errors/errors_test.go` verifying that absolute directory paths are stripped and only the segment basename is displayed.

### 2.5 SEC-P02-004: Standalone Phase 02 Audit Seal Artifact

* **Defect Description:** Phase 02 lacked a standalone seal document in `docs/security/`, being bundled inside `docs/security-audit-phase-00-05.md`.
* **Remediation Implemented:** Authored this document (`docs/security/security-audit-phase-02.md`).

---

## 3. Threat Model & Invariant Conformance

| Threat / Control | Invariant | Phase 02 Status | Evidence |
|---|---|---|---|
| **Trust Boundary 2: Strict Permissions** | WAL directories must use `0700` POSIX mode; segment files must use `0600`. | ✅ **VERIFIED** | `wal.DirMode = 0700`, `wal.FileMode = 0600`. Verified in `dir_test.go` and `writer_test.go`. |
| **Trust Boundary 2: Symlink Pinning** | Intermediate directories and target files must be defended against symlink swaps and TOCTOU redirection. | ✅ **VERIFIED** | `O_NOFOLLOW` open flags + `os.SameFile` double inode pinning + `validatePathNoSymlinks` + parent directory `os.Lstat` inspection. |
| **Threat 3: Data Poisoning** | Every WAL record must verify a hardware CRC32-IEEE checksum before decoding or replay. | ✅ **VERIFIED** | Verified in `corruption_test.go` and `record_test.go`. Fail-closed on mismatch with `ErrChecksumMismatch`. |
| **Threat 3: Torn Write Recovery** | Partial writes at EOF caused by crash must truncate safely; mid-log corruptions must halt engine. | ✅ **VERIFIED** | `recovery_test.go` verifies torn tail truncation to last valid commit point. |
| **Anti-DoS Memory Ceilings** | Untrusted length headers must not trigger runaway heap allocations. | ✅ **VERIFIED** | `MaxRecordLength` (~4.06 MB) and `MaxValueLen` (4 MB) validated before buffer allocations. |

---

## 4. Automated Verification Results

All automated test suites, race detection runs, and fuzz seed corpus tests pass cleanly:

```text
=== RUN   TestCreateWriter_ParentSymlink_Rejected
--- PASS: TestCreateWriter_ParentSymlink_Rejected (0.00s)
=== RUN   TestOpenWriter_ParentSymlink_Rejected
--- PASS: TestOpenWriter_ParentSymlink_Rejected (0.00s)
=== RUN   TestRotatingWriter_ParentDirectorySwapped_FailsClosed
--- PASS: TestRotatingWriter_ParentDirectorySwapped_FailsClosed (0.01s)
=== RUN   TestWALWriterPoisonedError
--- PASS: TestWALWriterPoisonedError (0.00s)
=== RUN   TestCorruption_BitFlipsDetected
--- PASS: TestCorruption_BitFlipsDetected (0.02s)
=== RUN   TestRecovery_TornTailTruncated
--- PASS: TestRecovery_TornTailTruncated (0.01s)
PASS
ok      github.com/silent-knight19/lattice/internal/wal    17.750s
ok      github.com/silent-knight19/lattice/internal/errors  1.616s
```

Full repository race suite execution (`go test -short -race ./...`): **PASS, 0 races, 0 failures**.

---

## 5. Phase Gate Verdict

### 🟢 PASS (Hermetically Sealed & Remediated)

All four findings from `SEC-AUDIT-P02-2026-09` have been fully investigated and remediated:
1. Parent directory symlink pinning gap closed across `OpenWriter`, `CreateWriter`, and `RotatingWriter`.
2. Authoritative WAL record format specification created and cross-referenced in Threat Model.
3. Path disclosure in `WALWriterPoisonedError` eliminated.
4. Standalone Phase 02 audit seal documented.

Phase 02 foundations are sealed and cleared for Phase 03 (MemTable & Concurrent SkipList).

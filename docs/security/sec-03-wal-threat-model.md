# Lattice: WAL Filesystem Threat Model & Storage Security Analysis

* **Document Version**: 1.0.0-SEC03-WAL-THREAT
* **Security Milestone**: SEC-03 — WAL / Filesystem / Storage Dynamic Security Audit
* **Companion Documents**: [`docs/security/security-architecture.md`](security-architecture.md), [`docs/architecture-spec.md`](../architecture-spec.md)
* **Status**: Approved Security Threat Model

---

## 1. Storage & Filesystem Architecture Scope

The Write-Ahead Log (WAL) subsystem is the fundamental durability foundation of the Lattice storage engine. It interacts directly with the host operating system's POSIX filesystem layer. This threat model analyzes the security boundaries, path resolution lifecycle, inode pinning mechanics, permission models, and adversarial filesystem vectors across the Phase-02 implementation:

- Directory Management: `InitDir`, `Dir`, `DirPath`
- Segment Writers: `CreateWriter`, `CreateSegmentWriter`, `OpenWriter`, `OpenSegmentWriter`, `RotatingWriter`
- Segment Readers: `OpenReader`, `WALReader.Next`
- Crash Recovery & Truncation: `RecoverSegment`, `RecoverSegmentByID`, `RecoverWAL`

```
                                [Caller / Administrator]
                                           │
                                           │ dbPath (Attacker Influenced / External Input)
                                           ▼
                              ┌─────────────────────────┐
                              │    wal.InitDir(dbPath)  │
                              └────────────┬────────────┘
                                           │ Atomic Mkdir(0700) / Open / SameFile / fchmod
                                           ▼
┌─────────────────────────────────────────────────────────────────────────────────────────────────┐
│ Host POSIX Filesystem Perimeter: <db_path>/wal/ (Mode: 0700 rwx------)                           │
│                                                                                                 │
│  Segment Path Synthesis (Internal Typed uint64): fmt.Sprintf("wal_%012d.log", id)               │
│                                                                                                 │
│  Active Segment (N)              Historical Segment (N-1)         Historical Segment (1)        │
│  ┌───────────────────────────┐   ┌────────────────────────────┐   ┌───────────────────────────┐ │
│  │ wal_000000000002.log      │   │ wal_000000000001.log       │   │ ...                       │ │
│  │ Mode: 0600 (rw-------)    │   │ Mode: 0600 (rw-------)     │   │ Mode: 0600 (rw-------)    │ │
│  │ Flags: O_CREATE|O_EXCL    │   │ Sealed (Read-Only Replay)  │   │ Sealed (Read-Only Replay) │ │
│  │ Opened Descr: O_RDWR/O_WR │   │ Fails Closed on Any Error  │   │ Fails Closed on Any Error │ │
│  └───────────────────────────┘   └────────────────────────────┘   └───────────────────────────┘ │
└─────────────────────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Path Lifecycle & Trust Boundary Analysis

### 2.1 Attacker-Influenced vs. Internally Synthesized Paths

| Component / Function | Path Source | Trust Classification | Synthesis Method | Security Controls |
| :--- | :--- | :--- | :--- | :--- |
| `InitDir(dbPath)` | External Caller / CLI Flag | Semi-Trusted (Admin/Config) | `filepath.Join(dbPath, "wal")` | `filepath.Clean()`, empty path check (`os.ErrInvalid`), atomic creation |
| `CreateWriter(path)` | Internal Caller / Direct API | Untrusted if exposed to user | Exact input path | `filepath.Clean()`, `os.Lstat` pre-check, `O_EXCL` atomic creation, `os.SameFile` pinning |
| `OpenWriter(path)` | Internal Caller / Direct API | Untrusted if exposed to user | Exact input path | `filepath.Clean()`, `os.Lstat` pre-check, regular file verification, `os.SameFile` pinning |
| `OpenSegmentWriter(dbPath, id)` | Database root + typed `uint64` | Internally Synthesized | `filepath.Join(walDir, fmt.Sprintf("wal_%012d.log", id))` | Integer formatting prevents directory traversal (`../`) or special path injections |
| `RotatingWriter.rotateLocked()` | Internal State Machine | Internally Synthesized | `SegmentPath(rw.dbPath, rw.activeID+1)` | Strict sequence incrementation; bounded to `math.MaxUint64`; created via `CreateWriter` (`O_EXCL`) |
| `ListSegments(dbPath)` | Filesystem directory listing | Untrusted On-Disk Metadata | `os.ReadDir(walDir)` + `ParseSegmentID(name)` | Strict regex/digit parsing (`wal_%012d.log`), rejects non-digits, leading zeros, or invalid length |
| `RecoverSegment(path)` | Direct Path / Admin forensic | Semi-Trusted | Exact input path | `os.Lstat` pre-check, `O_RDWR` (no create/trunc), `os.SameFile` pinning, in-place descriptor mutation |
| `RecoverWAL(dbPath, sink)` | Database root | Semi-Trusted | Discovered through `ListSegments` | Contiguity validation ($S_k = S_{k-1} + 1$), monotonic sequence numbers ($Seq_k > Seq_{k-1}$) |

### 2.2 Symlink Resolution Points & Inode Pinning

A primary attack vector in local database systems is Time-of-Check to Time-of-Use (TOCTOU) symlink substitution. An unprivileged local attacker or compromised co-located process attempts to replace a database directory or segment file with a symbolic link pointing to a sensitive system file (e.g. `/etc/passwd`, `/etc/shadow`, or `/var/log`).

The Lattice WAL implementation deploys an Inode Pinning defense-in-depth model across every filesystem touchpoint:

1. **Pre-Open Inspection (`os.Lstat`)**:
   - `Lstat` specifically inspects path attributes without dereferencing symbolic links (`info.Mode() & os.ModeSymlink != 0`).
   - Any detected symlink aborts operation immediately with `os.ErrInvalid`.
2. **Atomic Open**:
   - For new segments: `os.OpenFile(cleanPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)`.
   - `O_EXCL` delegates mutual exclusion directly to the OS kernel: if the target already exists (as a regular file, special device, or pre-existing symlink), `open` fails fast with `os.ErrExist` without following the symlink.
3. **Descriptor Inode Pinning (`os.SameFile`)**:
   - Once the file descriptor is open, `f.Stat()` queries the kernel-level file descriptor status (equivalent to `fstat`).
   - A subsequent `os.Lstat(cleanPath)` queries the directory entry currently residing on disk.
   - `os.SameFile(finfo, postInfo)` compares the `sys.Dev` (device ID) and `sys.Ino` (inode number).
   - If an attacker deleted the file and substituted a symlink or alternate file during the microsecond window between `Lstat` and `open`, `os.SameFile` evaluates to `false`, and the writer immediately aborts with:
     ```
     wal: file <path> was replaced during open: invalid argument
     ```
   - The descriptor is closed immediately without writing a single byte.
4. **Descriptor-Based Mutations (`fchmod`, `f.Truncate`, `f.Sync`)**:
   - Permissions, physical truncations, and disk synchronizations are executed directly against the verified open file descriptor handle (`f.Chmod`, `f.Truncate`, `f.Sync`), completely eliminating pathname traversal races.

---

## 3. Adversarial Threat Scenarios & Evaluation

### 3.1 Pathname Substitution & Symlink Races
* **Threat**: Attacker creates a symlink at `<dbPath>/wal` pointing to `/etc` or `/var/run`.
* **Current Control**:
  - `wal.InitDir` executes `os.Lstat(walPath)` prior to directory reuse. If `ModeSymlink != 0`, it rejects initialization.
  - If the directory already exists with loose permissions (`0755` or `0777`), it opens the descriptor `f, _ := os.Open(walPath)` and executes `f.Chmod(0700)`, verifying `os.SameFile` between descriptor and disk entry.
* **Residual Risk**: Root database directory (`dbPath`) must be provided by database administrator.

### 3.2 Pre-Existing Malicious Segment Files (Collision Attack)
* **Threat**: Attacker creates a pre-existing unprivileged file or symlink at `<dbPath>/wal/wal_000000000002.log` prior to segment rotation, hoping that rotation will overwrite or append to an unauthorized target.
* **Current Control**:
  - `RotatingWriter` relies on `CreateWriter`, which specifies `os.O_EXCL`.
  - If segment $N+1$ already exists on disk, `CreateWriter` fails immediately with `os.ErrExist`.
  - The rotation fails safely: the active writer is sealed, the append fails closed, and zero bytes are written to the pre-existing collision target.

### 3.3 Historical Segment Tampering (Data Poisoning)
* **Threat**: Attacker mutates historical segment $N-1$ (modifying key/values or injecting corrupted records), hoping recovery will repair it like a torn write or skip it.
* **Current Control**:
  - `RecoverWAL` categorizes segments into Historical ($1..N-1$) and Active ($N$).
  - Historical segments are inspected strictly in read-only mode (`os.O_RDONLY`).
  - Safe truncation is **strictly prohibited** on historical segments. If a historical segment contains a torn write, checksum mismatch, or invalid framing, `RecoverWAL` aborts with fatal error, halting engine startup.

### 3.4 Premature Durability Acknowledgement (Dirty Sync Race)
* **Threat**: Concurrent tasks waiting on group commit receive success notification before physical `fdatasync` concludes. If the host crashes immediately, committed state is lost.
* **Current Control**:
  - In `GroupCommitRunner.executeBatch`, task channels (`close(task.done)`) are closed **strictly after** `writer.Sync()` returns `nil`.
  - If `writer.Sync()` fails, all tasks in the batch receive the sync failure error immediately.

### 3.5 Segment ID Wraparound
* **Threat**: Database runs continuously for years until segment IDs approach `math.MaxUint64`. If incremented naively, ID overflows to `0`, creating `wal_000000000000.log` or overwriting early segments.
* **Current Control**:
  - `RotatingWriter.appendLocked` checks `if rw.activeID < math.MaxUint64`.
  - If `activeID == math.MaxUint64`, rotation is forbidden; records are appended to the terminal segment.
  - `NextSegmentID` explicitly returns `errors.ErrSegmentIDOverflow`.

---

## 4. Summary of Filesystem Security Invariants

1. **DIR-INV-01**: The WAL directory must enforce POSIX `0700` (`rwx------`).
2. **DIR-INV-02**: An existing symlink at the WAL directory path must be rejected fail-closed.
3. **SEG-INV-01**: Segment files must be created with POSIX `0600` (`rw-------`) and `O_EXCL`.
4. **SEG-INV-02**: Descriptors must be verified against disk inodes via `os.SameFile` post-open.
5. **REC-INV-01**: Safe torn-tail truncation is permitted strictly on the highest active segment file.
6. **REC-INV-02**: Historical segment corruption must halt recovery with zero file mutation.
7. **ROT-INV-01**: Pre-existing collision targets during rotation must fail closed with `os.ErrExist`.
8. **ROT-INV-02**: Segment IDs must remain strictly positive ($> 0$) and never wrap around to 0.

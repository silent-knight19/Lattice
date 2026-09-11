# Lattice: System-Wide Security Threat Model & Attack Surface Analysis

* **Document Version**: 1.0.0-THREAT-MODEL
* **Companion Specification**: [`docs/architecture-spec.md`](architecture-spec.md)
* **Companion Execution Plan**: [`docs/implementation-plan.md`](implementation-plan.md)
* **Status**: Living Security Architecture Baseline

---

## 1. Threat Modeling Scope & Trust Boundaries

```
[Untrusted External Network]
            │
            ▼
═══════════════════════════════════════════ [TRUST BOUNDARY 1: TCP Network Layer]
  TCP Binary Protocol Listener (:9099)
  - Frame Parsing & Magic Validation
  - Max Payload Ceilings (5MB)
  - Connection Rate Limiting
═══════════════════════════════════════════
            │
            ▼
┌─────────────────────────────────────────┐
│        Lattice Process Core (RAM)       │
│  - Engine Dispatcher / Group Commit     │
│  - Active & Immutable MemTables         │
│  - Sharded LRU Block Cache              │
│  - VersionSet Active Snapshots          │
└─────────────────────────────────────────┘
            │
            ▼
═══════════════════════════════════════════ [TRUST BOUNDARY 2: Storage & File I/O]
  POSIX Filesystem Layer (`/var/lib/lattice`)
  - Strict File Permissions (0700/0600)
  - Path Whitelisting (No Path Traversal)
  - CRC32 Block & Log Verification
═══════════════════════════════════════════
            │
            ▼
┌─────────────────────────────────────────┐
│      Physical Non-Volatile Storage      │
│  - WAL Segments (`wal_*.log`)           │
│  - SSTables (`*.sst`)                   │
│  - Metadata (`MANIFEST-*`, `CURRENT`)   │
└─────────────────────────────────────────┘
            │
            ▼
═══════════════════════════════════════════ [TRUST BOUNDARY 3: Peer-to-Peer Raft RPC]
  Cluster Network Transport (:9098)
  - Node Identity Verification
  - Term Validation & Replay Defense
═══════════════════════════════════════════
```

---

## 2. Threat Register & Mitigation Matrix

### Threat 1: Network Frame-Bomb Memory Exhaustion (DoS)
* **Asset**: Host RAM and process stability.
* **Threat**: An attacker connects to the TCP port and transmits an 18-byte header claiming `PayloadLength = 2,147,483,648 (2GB)`. A naive server allocates a 2GB buffer, triggering an Out-Of-Memory (OOM) panic that kills the database.
* **Attack Surface**: Public/internal TCP listener (`:9099`).
* **Impact**: Critical (Total database service denial).
* **Likelihood**: High (Common network scanner and fuzzer vector).
* **Existing Mitigation**: Strict payload ceiling validation in `P11-S01-M01`. Any frame advertising `PayloadLength > 5,242,880 (5MB)` or `PayloadLength < 0` immediately triggers socket termination with zero heap allocation.
* **Automated Test**: Unit and fuzz tests transmitting oversized length values; assert connection closed instantly with memory allocations $= 0$.
* **Residual Risk**: Low.

---

### Threat 2: Path Traversal via Manipulated File Numbers / Table Identifiers
* **Asset**: Host filesystem, operating system files, and database integrity.
* **Threat**: Malicious input or crafted internal IDs containing `../` sequences attempt to write SSTables or read files outside the designated data directory (e.g. attempting to read or overwrite `/etc/passwd`).
* **Attack Surface**: CLI diagnostic commands, SSTable inspectors, internal file path generators.
* **Impact**: Critical (Arbitrary host file read or overwrite).
* **Likelihood**: Medium.
* **Existing Mitigation**: File numbers are strictly typed `uint64` integers. File paths are synthesized exclusively using `filepath.Join(dataDir, fmt.Sprintf("%06d.sst", fileNum))`. Raw string inputs from CLI diagnostics are checked against strict alphanumeric whitelists (`^[a-zA-Z0-9_-]+$`) and validated with `filepath.Clean()`.
* **Automated Test**: Unit tests supplying `../../etc/passwd` to file loaders; assert `ErrInvalidPath` returned.
* **Residual Risk**: Negligible (Mitigated by integer-synthesized paths and filepath cleaning).

---

### Threat 3: Data Poisoning & Torn Write Exploits
* **Asset**: Recovered database state after process crash or restart.
* **Threat**: Malicious actor or power failure alters bytes in historical WAL logs or SSTables, tricking the recovery engine into applying corrupted or unauthorized records.
* **Attack Surface**: Local storage media and persistent file recovery subsystem.
* **Impact**: High (Silent data corruption or state regression).
* **Likelihood**: Medium (Disk bit rot, firmware bugs, sudden power loss).
* **Existing Mitigation**: Every WAL record and every 4KB SSTable block contains a hardware-accelerated CRC32-IEEE checksum. During startup recovery, records are verified before processing. EOF torn writes are truncated safely; mid-log corruptions halt the engine with `ErrChecksumMismatch`.
* **Automated Test**: Mutation fuzzing tests flipping random bits in WAL and SSTable files; assert 100% detection.
* **Residual Risk**: Low (CRC32 detects all single, double, and burst errors up to 32 bits).

---

### Threat 4: Buffer Bleed & Cross-Tenant Memory Leakage
* **Asset**: User data confidentiality across independent client connections.
* **Threat**: To minimize GC overhead, Lattice recycles byte buffers via `sync.Pool`. If a buffer containing sensitive client data is recycled and handed to a subsequent connection without proper zeroing or length tracking, data from Client A could leak to Client B.
* **Attack Surface**: TCP connection handler and framing buffer pool.
* **Impact**: High (Data confidentiality breach).
* **Likelihood**: Low (Engineering implementation bug).
* **Existing Mitigation**: Buffer wrappers strictly track valid read/write slices (`buf[:length]`). Before returning buffers to `sync.Pool`, sensitive payload slices are explicitly zeroed out or reset to length zero.
* **Automated Test**: Concurrency test where concurrent clients write distinct pseudorandom byte patterns; verify zero cross-contamination.
* **Residual Risk**: Very Low.

---

### Threat 5: Slowloris Connection Exhaustion
* **Asset**: Available network socket file descriptors and server goroutines.
* **Threat**: An attacker opens thousands of TCP connections and transmits data at an excruciatingly slow rate (1 byte every 10 seconds), consuming connection slots and preventing legitimate clients from connecting.
* **Attack Surface**: TCP socket listener (`:9099`).
* **Impact**: High (Denial of service for legitimate clients).
* **Likelihood**: Medium.
* **Existing Mitigation**: Maximum concurrent connection cap ($4,096$). Inbound connections configure strict socket read/write deadlines (`net.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))`). Connections failing to deliver a complete frame within the deadline are aborted.
* **Automated Test**: Simulated slow client test sending partial headers; verify socket aborted after timeout.
* **Residual Risk**: Low.

---

### Threat 6: Rogue Peer Injection in Distributed Mode (V1.1)
* **Asset**: Raft consensus cluster membership and state machine integrity.
* **Threat**: An unauthorized machine connects to the internal Raft RPC port (`:9098`) and issues `RequestVote` or `AppendEntries` RPCs with fabricated terms to disrupt the cluster.
* **Attack Surface**: Raft peer-to-peer network transport.
* **Impact**: Critical (Cluster split-brain or consensus disruption).
* **Likelihood**: Low in isolated private networks; High in shared environments.
* **Existing Mitigation**: Strict configuration whitelisting of authorized peer node IDs and IP:Port endpoints. In production profiles, mutual TLS (mTLS) with client certificate verification ensures only authorized cluster nodes can establish Raft RPC sessions.
* **Automated Test**: Test injecting messages from an unregistered node ID; assert connection immediately rejected.
* **Residual Risk**: Low with mTLS enabled.

---

### Threat 7: Unrestricted Local Filesystem Permissions
* **Asset**: Persistent database files on disk (`/var/lib/lattice`).
* **Threat**: Database files created with permissive default umask (e.g. `0644` or `0755`) can be read or modified by other unprivileged local users on the host machine.
* **Attack Surface**: POSIX filesystem permissions.
* **Impact**: High (Local user reads sensitive key-value data).
* **Likelihood**: Medium.
* **Existing Mitigation**: Data directories are initialized with POSIX `0700` (`rwx------`) and files are created with `0600` (`rw-------`). TableWriter strictly validates caller-supplied `FileMode` using `ValidateFileMode`, rejecting any mode that grants group/other permissions (non-owner bits `0077`) or execution bits (`0111`) with `ErrInsecureFileMode`. Failing closed ensures no broad permissions (e.g. `0644`, `0666`, `0755`, `0777`) can bypass the security baseline.
* **Automated Test**: `TestSecurity_Remediation3_FilePermissions_Baseline` verifying rejection of insecure modes (`0644`, `0666`, `0755`, `0777`, `0640`, `0604`, `0700`) and acceptance of owner-only modes (`0600`, `0400`).
* **Residual Risk**: Low (Enforced via POSIX mode bits 0700/0600 and programmatic validation on host filesystem).

---

### Threat 8: Accidental Credential / Secret Leakage in Diagnostic Logs
* **Asset**: Authentication credentials, user passwords, session tokens, API keys, and private keys.
* **Threat**: Developers or internal subsystems log structs or context containing credentials under diagnostic log streams, persisting secrets to disk or transmitting them to central log aggregators. Furthermore, a buggy or hostile custom `Redact()` implementation could return an unscrubbed secret when logged under a sensitive key.
* **Attack Surface**: Structured logging sink (`slog.Logger`), log files, SIEM collectors.
* **Impact**: High (Confidentiality breach, privilege escalation).
* **Likelihood**: Medium (Common human error in production systems).
* **Existing Mitigation**: Automated sensitive-key redaction via `ReplaceAttr` covering baseline keywords and stem variants. Under the defense-in-depth policy, sensitive attribute keys take strict precedence over custom `Redactable` value transformations. If an attribute key is classified as sensitive, the logger replaces the entire value with `[REDACTED]` immediately without evaluating custom `Redact()` logic. For non-sensitive keys, `Redactable` values are evaluated with typed-nil detection and panic recovery.
* **Automated Test**: Unit and adversarial tests (`TestSensitiveKeyPrecedenceOverHostileRedactable`, `TestPanickingRedactablePrecedence`, `TestConcurrentRedactionPrecedence`) asserting that sensitive keys never invoke `Redact()`, never leak secrets, and never panic.
* **Residual Risk**: Low for sensitive keys; key-based redaction remains heuristic for arbitrary secrets logged under unrecognized or non-sensitive key names without `Redactable`.

---

### Threat 9: SSTable Staging File Hijacking, Symlink Attacks & TOCTOU Overwrites
* **Asset**: SSTable data integrity and destination file publication.
* **Threat**: Predictable staging file names (e.g. `<target>.tmp`) open opportunities for local unprivileged processes to pre-create symlinks pointing to sensitive files (e.g. `/etc/passwd`), cause file truncation via `O_TRUNC`, or hijack concurrent table flushes. Furthermore, race conditions between table creation and finalization could cause silent destination overwrites.
* **Attack Surface**: Filesystem staging path generation, file creation flags, and atomic publication rename.
* **Impact**: High (Unauthorized file overwrite, corrupted SSTables, silent data replacement).
* **Likelihood**: Medium.
* **Existing Mitigation**: Staging files are created in the same parent directory using `os.CreateTemp` with randomized prefixes and atomic `O_CREATE|O_EXCL` semantics with mode `0600`. The file descriptor is verified against `os.Lstat` using `os.SameFile` and regular file checks to defeat symlink hijacking. In `Finish()`, destination existence is re-verified via `os.Lstat` immediately prior to rename, failing closed with `ErrSSTableExists` rather than overwriting concurrently created tables.
* **Automated Test**: `TestSecurity_Remediation3_StagingFileHardening` verifying concurrent writer contention, destination overwrite prevention, and symlink rejection.
* **Residual Risk**: Negligible.

---

### Threat 10: Sparse Index Key Comparator Type Confusion
* **Asset**: SSTable point lookup correctness and sparse index block selection.
* **Threat**: Heuristic key comparators that attempt to decode arbitrary byte slices as `InternalKey`s can misidentify bare binary user keys ($\ge 10$ bytes ending in `0x01` or `0x02`) as versioned records. This truncates the user key, corrupts sparse index binary search, and produces false `ErrKeyNotFound` errors for valid data.
* **Attack Surface**: `BlockIndex.FindBlock`, `IndexBuilder.FindBlock`, and SSTable `Seek` path.
* **Impact**: High (Data unavailability, false negative lookups for valid keys).
* **Likelihood**: High for binary user keys.
* **Existing Mitigation**: The comparator explicitly distinguishes between operand representations. In the SSTable point lookup path, `BlockIndex.FindBlock(targetUserKey)` extracts `entry.UserKey()` from the index block's largest key and evaluates `bytes.Compare` against the bare target user key. Binary keys ending in `0x01`, `0x02`, or containing null bytes are never decoded as internal keys. Separate explicit methods (`FindBlockKey`, `FindBlockInternalKey`) handle internal key targets.
* **Automated Test**: `TestSecurity_Remediation1_SparseIndexTypeConfusion` verifying exact point lookups and boundary checks across binary keys ending in `0x01`, `0x02`, zero bytes, and trailer-like suffixes.
* **Residual Risk**: Negligible.

---

### Threat 11: SSTable Key Length Boundary Inconsistencies
* **Asset**: Maximum supported user key size (65,535 bytes) and memory allocation bounding.
* **Threat**: Applying user-key maximum lengths (`MaxKeyLen = 65,535`) to encoded `InternalKey`s (which include an additional 9-byte trailer) causes valid maximum-size user keys to be rejected during block index encoding or decoding, while failing to bound internal representations permits denial-of-service memory blowups.
* **Attack Surface**: `TableWriter.Add`, `IndexBuilder.AddBlock`, and `DecodeBlockIndex`.
* **Impact**: Medium (Rejection of valid maximum-boundary user keys; availability degradation).
* **Likelihood**: Medium for applications using 64KB keys.
* **Existing Mitigation**: Explicit separation of representation boundaries: `MaxUserKeyLen` (65,535 bytes) and `MaxEncodedInternalKeyLen` (65,544 bytes). `TableWriter.Add` validates user keys against `MaxUserKeyLen`, while `IndexBuilder` and `DecodeBlockIndex` validate index entries against `MaxEncodedInternalKeyLen`.
* **Automated Test**: `TestSecurity_Remediation4_MaxKeyLengthBoundary` asserting 65,535-byte user keys write, index, and read back correctly, while lengths exceeding ceilings are rejected.
* **Residual Risk**: Negligible.

---

### Threat 12: Cleartext Key Disclosure via Default String Formatting
* **Asset**: Sensitive data confidentiality in user keys.
* **Threat**: Generic string formatting (e.g. `fmt.Sprintf("%v", key)`) invoking `InternalKey.String()` formats raw user key bytes, inadvertently leaking sensitive tokens, passwords, or PII into standard error, third-party libraries, or unredacted logging sinks.
* **Attack Surface**: `InternalKey.String()`, `%v`, `%s`, and generic formatting callers.
* **Impact**: Low/Medium (Confidentiality leakage).
* **Likelihood**: Medium.
* **Existing Mitigation**: `InternalKey.String()` is safe by default and returns metadata-only redacted formatting (`InternalKey(len=%d, seq=%d, op=%s)`). Raw cleartext key formatting is restricted to explicitly invoked `DebugString()` calls intended for forensic analysis.
* **Automated Test**: `TestSecurity_Remediation6_InternalKeyRedaction` asserting default string representations omit raw payload bytes.
* **Residual Risk**: Negligible.

---

*End of Security Threat Model — Lattice v1.0.0-THREAT-MODEL*

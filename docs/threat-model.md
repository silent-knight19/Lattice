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
* **Residual Risk**: Zero (No user-supplied path concatenation permitted).

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
* **Existing Mitigation**: Data directories are initialized with POSIX `0700` (`rwx------`) and files are created with `0600` (`rw-------`). The engine checks and warns if permissions are compromised.
* **Automated Test**: Test verifying file mode bits upon creation.
* **Residual Risk**: Zero on dedicated host environments.

---

*End of Security Threat Model — Lattice v1.0.0-THREAT-MODEL*

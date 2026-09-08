# Lattice: Security Architecture, Attack Surface Inventory & Threat Model

* **Document Version**: 1.0.0-SECURITY-ARCH
* **Status**: Active Living Security Architecture Baseline
* **Scope**: Phase 00 through Phase 02 Subsystems (Core Binary Primitives, Logging, Errors, Write-Ahead Log, Group Commit)
* **Companion Specifications**: [`docs/architecture-spec.md`](../architecture-spec.md), [`docs/threat-model.md`](../threat-model.md), [`docs/implementation-plan.md`](../implementation-plan.md)

---

## 1. Security Architecture & Trust Boundaries

The Lattice storage engine enforces explicit trust boundaries between untrusted caller inputs, external filesystem state, background execution loops, and operator-visible outputs.

```
═══════════════════════════════════════════════════════════════════════════════
[BOUNDARY 1: Client / Network Boundary] - (FUTURE / NOT YET IMPLEMENTED)
- TCP Binary Protocol Listener (:9099)
- TLS 1.3 Termination, Framing Magic, Connection Rate Limiting
═══════════════════════════════════════════════════════════════════════════════
                                      │
                                      ▼
═══════════════════════════════════════════════════════════════════════════════
[BOUNDARY 2: Application / API Boundary] - (ACTIVE)
- Callers construct WriteTask / submit Record (Key []byte, Value []byte)
- Defensive memory deep-copying (Four-Tier Payload Ownership Model)
- Structural validation (KeyLen <= 65,535, ValueLen <= 4,194,304, Valid Type)
═══════════════════════════════════════════════════════════════════════════════
                                      │
                                      ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                      Lattice Process Memory (RAM)                           │
│  - Bounded FIFO Group Commit Queue (Default 1,024 slots)                    │
│  - Condition Variable Backpressure (notFull.Wait, notEmpty.Wait)            │
│  - Dedicated GroupCommitRunner Event Loop                                   │
│  - Single-Pass AST Caching & In-Memory Redaction Log Handler                │
└─────────────────────────────────────────────────────────────────────────────┘
                                      │
                                      ▼
═══════════════════════════════════════════════════════════════════════════════
[BOUNDARY 3: Storage & Filesystem Boundary] - (ACTIVE)
- Directory Initialization: 0700 (rwx------) with fchmod and os.SameFile pinning
- Segment Creation: 0600 (rw-------) with O_CREATE|O_WRONLY|O_EXCL
- File Path Synthesis: Canonical integer formatting ("wal_%016d.log")
- Hardware Synchronization: Linux fdatasync / platform Sync barrier
═══════════════════════════════════════════════════════════════════════════════
                                      │
                                      ▼
═══════════════════════════════════════════════════════════════════════════════
[BOUNDARY 4: WAL Recovery Boundary] - (ACTIVE)
- Startup Recovery Coordinator replaying untrusted on-disk bytes
- Strict Numeric Segment Sequencing (1, 2, 3...) with Gap Detection
- Dual CRC32-IEEE Verification & Early Allocation Ceilings
- Latest-Segment Torn-Tail Truncation vs Fail-Closed Historical Bit-Rot Halt
═══════════════════════════════════════════════════════════════════════════════
                                      │
                                      ▼
═══════════════════════════════════════════════════════════════════════════════
[BOUNDARY 5: Logging & Operator Boundary] - (ACTIVE)
- slog Handler with Automated Sensitive-Key Redaction
- Redactable Domain Interface Scrubbing
- Masked Audit Report Evidence Output
═══════════════════════════════════════════════════════════════════════════════
```

### Trust Boundary Register

| Boundary ID | Boundary Name | Input Source | Trust Level | Data Type | Validation Performed | Security-Sensitive Operations | Failure Behavior | Relevant Packages |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **TB-01** | Application / API Boundary | Calling application goroutines | Untrusted | `Record`, `WriteTask` (`[]byte` keys/values) | Key length $\le 65,535$, Value length $\le 4\text{ MiB}$, Valid RecordType | Memory allocation, task queueing | Reject immediately with structured error (`*errors.ValueTooLargeError`, `*errors.EmptyKeyError`) | `internal/binary`, `internal/wal` |
| **TB-02** | Client $\to$ Database Boundary | Remote network clients | Untrusted | TCP byte stream | *FUTURE / NOT YET IMPLEMENTED* | Connection accept, frame parsing | *FUTURE / NOT YET IMPLEMENTED* | `pkg/client`, `cmd/lattice` |
| **TB-03** | Network $\to$ Protocol Boundary | TCP sockets | Untrusted | Binary network frames | *FUTURE / NOT YET IMPLEMENTED* | Frame decoding, session dispatch | *FUTURE / NOT YET IMPLEMENTED* | `internal/transport` |
| **TB-04** | WAL $\to$ Recovery Boundary | Local on-disk log files | Untrusted (potential crash corruption, bit-rot, tampering) | Raw bytes from disk | Framing headers, CRC32 checksums, Monotonic `SeqNum` | In-place file truncation, state replay | Fail-closed panic/error on historical bit-rot; safe truncation strictly on latest active tail | `internal/wal` |
| **TB-05** | Disk / Filesystem Boundary | OS filesystem API | Semi-trusted (OS kernel / physical media) | Path strings, file descriptors | Canonical integer formatting, directory inode pinning via `os.SameFile` | `fchmod`, `open`, `write`, `fdatasync`, `truncate` | Return structured I/O error, fail batch closed | `internal/wal` |
| **TB-06** | Config / Environment Boundary | CLI flags, config files | Untrusted | Strings, file paths, numbers | Positive queue capacity, non-empty directory | Directory creation, buffer sizing | Fail initialization with `InvalidQueueCapacityError` | `internal/wal`, `internal/logger` |
| **TB-07** | Logging $\to$ Operator Boundary | Storage engine internals | Internal | Log attributes, record summaries | Key stem matching ("password", "secret", "token"), `Redactable` | Standard output, log file append | Automatically mask values with `[REDACTED]` | `internal/logger` |
| **TB-08** | Test/Debug $\to$ Prod Boundary | Test runners, harness | Untrusted / Adversarial | Fuzz mutations, synthetic corruption | Synthetic fixture isolation (`internal/security/testdata`) | Mock write seams, fault injection | Isolated to test binaries; excluded from production | `internal/security` |

---

## 2. Attack Surface Inventory

Inventory of currently reachable or security-sensitive interfaces across implemented subsystems.

| Component / Interface | Classification | Reachability | Description | Security Controls |
| :--- | :--- | :--- | :--- | :--- |
| `wal.NewWriteTask` | `INPUT`, `RESOURCE` | Reachable Today | Ingests caller key/value slices for asynchronous logging | Validates length ceilings, deep-copies slices to prevent aliasing mutations |
| `wal.WriteTask.Record()` | `OUTPUT` | Reachable Today | Exposes record copy to callers | Returns independent defensive copies; internal raw slices unexported |
| `wal.WriteQueue.Enqueue` | `RESOURCE`, `CONCURRENCY` | Reachable Today | Enqueues write task into bounded circular ring buffer | Condition variable backpressure; rejects when closed |
| `wal.GroupCommitRunner` | `EXECUTION`, `PRIVILEGE` | Reachable Today | Background event loop consuming queue and calling `Sync()` | Amortizes 1 sync per batch; fail-closed error fan-out; graceful drain |
| `wal.InitDir` | `STORAGE`, `PRIVILEGE` | Reachable Today | Initializes WAL directory on filesystem | Enforces POSIX `0700`; mitigates symlink TOCTOU via `fchmod` and `SameFile` |
| `wal.WALWriter.Append` | `STORAGE`, `RESOURCE` | Internal-only | Appends encoded record to active segment | Bounded 21-byte framing; Big-Endian encoding; CRC32 calculation |
| `wal.RotatingWriter.Append`| `STORAGE`, `PRIVILEGE` | Reachable Today | Coordinates segment rotation when file exceeds limit | Seals and syncs old segment before opening new `0600` segment |
| `wal.RecoverWAL` | `INPUT`, `STORAGE` | Reachable Today | Discovers, validates, truncates, and streams WAL logs | Strict numeric ordering; fail-closed gap detection; monotonic sequence checks |
| `binary.EncodeRecord` | `EXECUTION`, `RESOURCE` | Reachable Today | Serializes record into binary frame | Early bounds checks (`_ = buf[20]`); zero memory allocation when pre-allocated |
| `binary.DecodeRecord` | `INPUT`, `RESOURCE` | Reachable Today | Deserializes record from `io.Reader` | Early allocation bounding (`MaxKeyLen`, `MaxValueLen`); zero-allocation CRC replay |
| `logger.L().Info/Error` | `OUTPUT` | Reachable Today | Structured logging of engine events | Automated sensitive key redaction; `Redactable` interface support |
| `cmd/lattice` | `EXECUTION` | Future-facing | Main daemon entrypoint | Skinny main; currently stub |
| `pkg/client` | `INPUT` | Future-facing | Client driver library | Currently stub |
| `internal/transport` | `TRUST BOUNDARY` | Future-facing | TCP networking protocol listener | Not yet implemented (Phase 11) |

---

## 3. Threat Model

Database-specific threat model evaluating 12 adversary capabilities against Lattice persistent assets:

### 1. Attacker Submits Malformed Database Input
* **Attacker Capability**: Submits invalid record types (`0x00`, `0x05`), empty keys, or corrupt byte sequences.
* **Target Asset**: State machine integrity, process availability.
* **Relevant Surface**: `binary.DecodeRecord`, `wal.NewWriteTask`.
* **Security Objectives**: Fail-fast rejection without heap allocation or panics.
* **Current Controls**: `record.Validate()` strictly enforces `RecordType` enum (PUT, DELETE, BATCH_START, BATCH_COMMIT). Uninitialized `0x00` is explicitly `RecordTypeInvalid`.
* **Remaining Risk**: None in Phase 02; network ingress validation will be tested in Phase 11.

### 2. Attacker Submits Oversized Keys or Values
* **Attacker Capability**: Sends length header claiming 2GB or 4GB value length to trigger memory exhaustion.
* **Target Asset**: Host RAM, process availability (Linux OOM killer).
* **Relevant Surface**: `binary.DecodeRecord`, `wal.NewWriteTask`.
* **Security Objectives**: Reject payloads exceeding configured ceilings with zero memory allocation.
* **Current Controls**: `binary.MaxKeyLen = 65,535`, `binary.MaxValueLen = 4,194,304` (4 MiB). Decoders reject oversized lengths before executing `make([]byte, length)`.
* **Remaining Risk**: Negligible (enforced statically and dynamically).

### 3. Attacker Triggers Unusually High Request Rates
* **Attacker Capability**: Floods database with concurrent write operations.
* **Target Asset**: Process memory, thread pools, file descriptors.
* **Relevant Surface**: `wal.WriteQueue`.
* **Security Objectives**: Bounded memory consumption; natural backpressure pacing callers to physical disk IOPS.
* **Current Controls**: Bounded circular buffer (`DefaultQueueCapacity = 1024`). `Enqueue` blocks on condition variable; `TryEnqueue` returns `ErrQueueFull`.
* **Remaining Risk**: Client network connection rate limiting deferred to Phase 11.

### 4. Attacker Causes Malformed WAL Records
* **Attacker Capability**: Writes partial, corrupted, or bit-flipped records to WAL files.
* **Target Asset**: Crash recovery correctness, database persistence.
* **Relevant Surface**: `wal.WALReader`, `wal.RecoverSegment`, `wal.RecoverWAL`.
* **Security Objectives**: Deterministic detection via CRC32; safe truncation of torn tails; fail-closed halt on middle bit-rot.
* **Current Controls**: CRC32-IEEE checksums cover full payload; single-bit flips are 100% detected; un-synced EOF tails truncated safely; historical corruption halts startup.
* **Remaining Risk**: Non-cryptographic checksum (CRC32 does not defend against active cryptographic tampering). Cryptographic authentication deferred to threat model scope.

### 5. Attacker Influences Filesystem Paths
* **Attacker Capability**: Supplies path strings containing `../` traversal characters or symlinks.
* **Target Asset**: Host filesystem outside database directory.
* **Relevant Surface**: `wal.InitDir`, segment file naming.
* **Security Objectives**: Strict path confinement within `<db_path>/wal/`.
* **Current Controls**: Segment files are generated exclusively via integer formatting (`fmt.Sprintf("wal_%016d.log", id)`). Directory initialization verifies `os.SameFile` to prevent symlink substitution.
* **Remaining Risk**: Caller-supplied `dbPath` must be managed by database administrator.

### 6. Attacker Supplies Corrupted On-Disk State
* **Attacker Capability**: Injects non-monotonic sequence numbers or creates gaps in segment files.
* **Target Asset**: Database causal consistency and linearizability.
* **Relevant Surface**: `wal.RecoverWAL`.
* **Security Objectives**: Halt recovery immediately on ordering anomalies.
* **Current Controls**: Sequential replay enforces strict segment ID continuity ($S_{k} = S_{k-1} + 1$) and monotonic sequence numbering ($SeqNum_{k} > SeqNum_{k-1}$).
* **Remaining Risk**: None.

### 7. Attacker Controls Environment or Configuration Values
* **Attacker Capability**: Sets invalid or negative configuration options (e.g. negative queue capacity).
* **Target Asset**: Engine initialization and stability.
* **Relevant Surface**: `wal.NewWriteQueue`.
* **Security Objectives**: Reject non-positive or irrational configurations.
* **Current Controls**: Capacity $\le 0$ rejected with `InvalidQueueCapacityError`.
* **Remaining Risk**: Configuration file parsing to be expanded in Phase 10.

### 8. Attacker Observes Logs and Error Messages
* **Attacker Capability**: Inspects application standard output, syslog, or diagnostic error messages.
* **Target Asset**: Customer credential and data confidentiality.
* **Relevant Surface**: `internal/logger`, `internal/errors`.
* **Security Objectives**: Automated redaction of sensitive key names and secret literals.
* **Current Controls**: `internal/logger` masks keys matching "password", "token", "secret", "credential", "auth", "api_key". Domain types implement `Redactable`.
* **Remaining Risk**: Plain primitive values logged under obscure, non-standard key names (documented in `docs/known-limitations.md` #10).

### 9. Attacker Causes Concurrent Operations
* **Attacker Capability**: Spawns hundreds of concurrent goroutines calling write, close, and inspection APIs.
* **Target Asset**: Concurrency invariants, memory race freedom.
* **Relevant Surface**: `wal.WriteQueue`, `wal.WriteTask`, `wal.GroupCommitRunner`.
* **Security Objectives**: Zero data races, atomic lifecycle transitions, idempotent completions.
* **Current Controls**: Mutex and RWMutex guards, atomic lifecycle flags (`enqueued`, `closed`), dual condition variables. Verified race-free under `go test -race`.
* **Remaining Risk**: None in Phase 02.

### 10. Attacker Causes Process Restart or Crash at Adversarial Points
* **Attacker Capability**: Kills process mid-write, mid-batch, or during segment rotation.
* **Target Asset**: Durability guarantees, torn write resilience.
* **Relevant Surface**: `wal.RotatingWriter`, `wal.GroupCommitRunner`.
* **Security Objectives**: Never acknowledge commit before physical `Sync()`; clean crash recovery.
* **Current Controls**: Task completion channel closed strictly after `fdatasync()` returns `nil`. Rotation seals and syncs old segment before opening new segment.
* **Remaining Risk**: Hardware controller write cache volatility (mitigated by Linux `fdatasync` barrier).

### 11. Attacker Interacts with Future Network Interfaces
* **Attacker Capability**: Connects to TCP socket, sends frame bombs, slowloris attacks.
* **Target Asset**: Network listener, session manager.
* **Relevant Surface**: *FUTURE / NOT YET IMPLEMENTED* (`internal/transport`).
* **Security Objectives**: Enforce frame limits (5MB ceiling), connection rate limits, TLS 1.3.
* **Current Controls**: Documented architectural design targets.
* **Remaining Risk**: Implementation scheduled for Phase 11.

### 12. Attacker Attempts Resource Exhaustion
* **Attacker Capability**: Floods memory with un-synced tasks or oversized allocations.
* **Target Asset**: System memory and disk capacity.
* **Relevant Surface**: `wal.WriteQueue`, `wal.GroupCommitRunner`.
* **Security Objectives**: Bounded queues, dual batch limits ($\le 1,024$ tasks, $\le 64\text{ KiB}$).
* **Current Controls**: Dual hard limits; queue capacity bounds; singleton oversized record fallback.
* **Remaining Risk**: Disk full (`ENOSPC`) handled fail-closed.

---

## 4. Security Invariants Register

| Invariant ID | Security Property | Enforcing Code | Tests Proving Invariant | Remaining Gap |
| :--- | :--- | :--- | :--- | :--- |
| **SEC-INV-01** | No caller-controlled buffer may mutate asynchronous WAL task state | `internal/wal/task.go:NewWriteTask`, `Record()` | `queue_test.go:TestTaskPayloadOwnership`, `TestTaskRecordDoesNotExposeInternalKey` | None |
| **SEC-INV-02** | WAL corruption must fail closed rather than being silently skipped | `internal/wal/coordinator.go`, `reader.go` | `coordinator_test.go:TestCoordinator_MiddleSegmentCorruptHeaderFailsClosed`, `corruption_test.go` | None |
| **SEC-INV-03** | A malformed length field must never cause an uncontrolled memory allocation | `internal/wal/record.go:DecodeRecord`, `validate.go` | `record_test.go:TestDecodeRecord_ValueTooLargeRejectedEarly`, `validate_test.go` | None |
| **SEC-INV-04** | Sensitive values must not be emitted in plaintext logs | `internal/logger/logger.go:RedactingHandler` | `logger_test.go:TestSensitiveKeyRedaction`, `cross_phase_test.go` | Non-sensitive keys carrying secrets |
| **SEC-INV-05** | WAL directories must not be hijacked via symlink substitution races | `internal/wal/dir.go:InitDir` | `dir_test.go:TestInitDir_ExistingDirectory_LoosePermissionsHardened` | Root directory permissions |
| **SEC-INV-06** | Successful durability acknowledgement must not occur before physical synchronization | `internal/wal/runner.go:executeBatch` | `runner_test.go:TestRunner_SingleSyncAmortization`, `TestRunner_WriteFailureFailsAllTasks` | None |
| **SEC-INV-07** | Task queues must enforce positive, bounded capacities with condition variable backpressure | `internal/wal/queue.go:NewWriteQueue` | `queue_test.go:TestInvalidQueueCapacity`, `TestQueueCapacityBehaviorBounded` | None |
| **SEC-INV-08** | Recovery must enforce strict numeric segment continuity and sequence number monotonicity | `internal/wal/coordinator.go:validateSegmentContinuity` | `coordinator_test.go:TestCoordinator_MissingSegmentGapFailsClosed`, `TestCoordinator_SequenceRegressionFailsClosed` | None |

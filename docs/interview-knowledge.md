# Lattice: Technical Interview Knowledge Base & Defense Guide

* **Document Version**: 1.0.0-KNOWLEDGE-BASE
* **Companion Execution Blueprint**: [`docs/implementation-plan.md`](implementation-plan.md)
* **Companion Architecture Specification**: [`docs/architecture-spec.md`](architecture-spec.md)
* **Purpose**: Comprehensive, subsystem-by-subsystem knowledge tracking for technical interviews at top-tier systems engineering companies (Google, Meta, Amazon, Microsoft, Uber, Atlassian).

---

## Table of Contents

1. [Foundations, Binary Encodings & Memory Management](#1-foundations-binary-encodings--memory-management)
2. [Write-Ahead Log (WAL) & Durability Subsystem](#2-write-ahead-log-wal--durability-subsystem)
3. [In-Memory MemTable & Concurrent SkipList](#3-in-memory-memtable--concurrent-skiplist)
4. [Persistent SSTable File Formats & Block Layout](#4-persistent-sstable-file-formats--block-layout)
5. [Probabilistic Bloom Filter Subsystem](#5-probabilistic-bloom-filter-subsystem)
6. [Manifest Log & VersionSet Concurrency](#6-manifest-log--versionset-concurrency)
7. [Crash Recovery, Torn Writes & Startup Replay](#7-crash-recovery-torn-writes--startup-replay)
8. [Leveled Compaction & K-Way Merge Sort](#8-leveled-compaction--k-way-merge-sort)
9. [Sharded LRU Read Block Cache](#9-sharded-lru-read-block-cache)
10. [Single-Node Engine Integration & Backpressure](#10-single-node-engine-integration--backpressure)
11. [TCP Binary Wire Protocol & Networking](#11-tcp-binary-wire-protocol--networking)
12. [Distributed Foundations & Node Topology](#12-distributed-foundations--node-topology)
13. [Raft Consensus Engine (V1.1)](#13-raft-consensus-engine-v11)
14. [Distributed State Machine Replication](#14-distributed-state-machine-replication)
15. [Linearizable Reads & the ReadIndex Protocol](#15-linearizable-reads--the-readindex-protocol)
16. [Fault Injection & Chaos Engineering](#16-fault-injection--chaos-engineering)
17. [System-Wide Security & Threat Modeling](#17-system-wide-security--threat-modeling)
18. [Master Checklist: 30 "Did You Actually Build This?" Exposure Questions](#18-master-checklist-30-did-you-actually-build-this-exposure-questions)

---

# 1. Foundations, Binary Encodings & Memory Management

### Concepts I Must Personally Understand
* **Go Modules & Hermetic Builds**: A Go module is a collection of Go packages defined by a `go.mod` file at the root. Prior to Go 1.11, Go relied on `$GOPATH`, which lacked declarative dependency versioning, leading to fragile builds where external library changes broke local builds. `go.mod` establishes deterministic, reproducible compilation by recording the canonical module path and compiler toolchain version.
* **Minimal Version Selection (MVS)**: Unlike package managers in other ecosystems (e.g. npm or Cargo) that select the *maximum* or latest compatible version matching a semver range, Go's MVS algorithm selects the *oldest* version of a dependency that provides the required features. This guarantees that updating unrelated dependencies never silently pulls in untested major revisions, ensuring build determinism.
* **Checksum Database (`go.sum`)**: While `go.mod` records direct and indirect dependencies, `go.sum` contains cryptographic SHA-256 hashes of module zip files. Even if a remote Git repository alters or force-pushes a release tag, Go's checksum database halts the build with a verification mismatch.
* **Go Package Layout Architecture (`cmd/`, `internal/`, `pkg/`)**: The standard Go layout creates explicit separation of concerns:
  - `cmd/`: Contains entrypoint binaries (`package main`). Each subfolder (e.g., `cmd/lattice`, `cmd/lattice-cli`, `cmd/lattice-bench`) compiles to a distinct binary. Code in `cmd/` must remain minimal ("skinny main"), focusing strictly on flag parsing, signal handling, dependency wiring, and calling into internal subsystems.
  - `internal/`: Private application and storage engine libraries. The Go compiler enforces that no package outside the `github.com/silent-knight19/lattice` tree can import anything under `internal/`.
  - `pkg/`: Public library code intended for external consumption (e.g., `pkg/client`). Breaking changes here require major semantic version bumps.
* **`internal/` Package Semantics & Compiler Enforcement**: Introduced in Go 1.4, the Go compiler enforces that a package residing in `.../internal/foo` can only be imported by packages rooted in the directory immediately parent to `internal`. For example, external Go projects cannot do `import "github.com/silent-knight19/lattice/internal/wal"`. Attempting to do so fails at compile time with `use of internal package not allowed`. This guarantees that internal storage formats, locking mechanisms, and file layout structs remain private, eliminating accidental Hyrum's Law dependencies.
* **Command/Package Separation ("Skinny Main")**: A storage engine binary (`cmd/lattice`) must avoid embedding domain or concurrency logic in `main.go`. `main()` should only: parse CLI flags, read configuration files, initialize dependencies, register OS signal listeners (`SIGTERM`, `SIGINT`), and block until graceful shutdown completes. All engine orchestration belongs in `internal/engine`, which allows integration tests to instantiate the entire engine in-process without invoking CLI binaries.
* **Dependency Boundaries & Acyclic Directed Graph (DAG)**: Go strictly prohibits cyclic dependencies (`import cycle not allowed`). Package dependencies must form a strict Directed Acyclic Graph (DAG). Lower layers (e.g., `internal/binary`, `internal/errors`) are leaf nodes with zero dependencies on storage or engine packages. Mid-level subsystems (`internal/wal`, `internal/memtable`, `internal/sstable`) depend only on leaf utilities. Top-level orchestration (`internal/engine`) composes the storage subsystems, and `cmd/lattice` composes the engine with networking (`internal/transport`).
* **Package-Level API Design**: Go packages should be named for what they provide, not what they contain. Generic catch-all packages such as `util/`, `common/`, or `helpers/` are architectural anti-patterns in Go; they attract unrelated utilities, obscure package responsibilities, and inevitably cause cyclic imports. Interfaces should be defined where they are consumed (consumer-driven interfaces), not where they are implemented.
* **Import-Cycle Prevention**: In complex database engines, cyclic dependencies frequently threaten to arise (e.g., `MemTable` needs to trigger an asynchronous flush to `SSTable`, while `SSTable` compaction needs to query `MemTable` state). Cycles are prevented by: (1) defining shared domain errors and primitive types in leaf packages (`internal/errors`, `internal/binary`), (2) passing interfaces into constructors instead of concrete structs (dependency inversion), and (3) elevating coordination to parent orchestrators (`internal/engine`).
* **Why Package Boundaries Matter in a Storage Engine**: Database engines maintain delicate invariants spanning memory allocation, file synchronization (`fsync`), concurrent locking, and crash consistency. Leaky package boundaries permit developers to bypass invariants—such as mutating in-memory structures without first writing to the WAL, or reading SSTable block data without taking block cache reference counts. Compiler-enforced boundaries guarantee that subsystem invariants cannot be violated from outside the package.
* **`go vet` vs. Linters**: `go vet` is the official, conservative static analyzer bundled with the Go toolchain. It focuses strictly on high-probability compiler-adjacent bugs (bad printf formats, unreachable code, unkeyed composite literals, mutex copying, atomic 64-bit alignment) with an extremely low false-positive rate. In contrast, meta-linter runners like `golangci-lint` orchestrate dozens of specialized AST analyzers (`errcheck`, `staticcheck`, `errorlint`, `ineffassign`) that check for unchecked errors, error wrapping compliance, dead code, and style invariants far beyond `go vet`'s scope.
* **Static Analysis vs. Formatting**: Formatting (`gofmt`) operates strictly on AST syntax layout—tabs vs. spaces, brace placement, indentation, newline rules—without affecting program semantics. Static analysis inspects AST nodes, control-flow graphs (CFGs), and type tables to detect semantic bugs, memory safety issues, resource leaks, and concurrency hazards at compile/development time without executing the binary.
* **Why Compiler Correctness Is Not Enough in Systems Programming**: Go is a statically typed, memory-safe language, yet the compiler happily compiles code with catastrophic systems bugs:
  - Ignoring error returns from `file.Sync()` or `file.Close()` compiles with zero warnings, yet causes silent data loss upon sudden power loss or process crash.
  - Comparing wrapped errors with direct equality (`err == ErrKeyNotFound`) compiles, but fails at runtime if the error was formatted with `%w`.
  - Passing a struct containing a `sync.Mutex` by value compiles, but creates a detached copy that completely breaks mutual exclusion.
  - Type assertions `v := x.(string)` without the two-value comma-ok check compile, but trigger fatal runtime panics if an unexpected type is passed.
* **The Perils of Excessive Lint Rules ("Linter Fatigue")**: Enabling dozens of superficial, stylistic, or hyper-opinionated linters (e.g. `varnamelen`, `wsl`, `cyclop`, `godot`) creates massive noise, constant false positives, and code churn without delivering measurable correctness or performance value. High noise induces "linter fatigue", driving developers to either bypass checks with blanket suppressions or write convoluted, unidiomatic workarounds. Systems engines must optimize strictly for signal: linters must protect correctness, memory safety, and durability.
* **Enforcing Systems Durability Invariants via Static Analysis**: In a storage engine, every disk write, directory fsync, CRC checksum validation, and network frame decode returns an `error`. A single unhandled error can corrupt an entire database file. By configuring `errcheck` with `check-type-assertions: true`, we guarantee at development time that unhandled errors and unchecked interface conversions break the build before code can ever reach persistent storage.
* **Evaluating Development Tooling Dependencies**: Development tools (e.g. `golangci-lint`) must be vetted with the same supply-chain scrutiny as runtime libraries:
  - Tooling must be hermetic and isolated from the production `go.mod`, ensuring zero third-party dependencies bleed into the database binary.
  - Installation must use trusted, auditable sources (e.g. verified Homebrew core bottles) rather than unverified `curl | sh` scripts.
  - Tooling configuration must be pinned in version control (`.golangci.yml`) to guarantee identical analysis across local workstations and CI environments.
* **Distinguishing Correctness Bugs from Stylistic Heuristics**: Systems engineers must differentiate between true correctness defects and subjective compiler heuristics. For example, `govet`'s `fieldalignment` analyzer suggests rearranging struct fields to minimize memory padding. However, in low-level database engines, struct fields are often intentionally arranged to align with hardware cache lines (64 bytes) to prevent false sharing between concurrent threads, or to mirror on-disk binary layouts. Naively following `fieldalignment` destroys concurrency isolation; hence it is intentionally disabled.
* **Sentinel Errors vs. Contextual Typed Errors**:
  - *Sentinel errors* (e.g. `ErrKeyNotFound = errors.New("key not found")`) provide constant, pre-allocated identity. They are optimal for expected domain conditions where the error identity alone dictates control flow (e.g., return 404/not-found to client) without allocation overhead.
  - *Typed errors* (e.g. `KeyTooLargeError`, `ChecksumMismatchError`) are custom structs implementing the `error` interface. They carry structured diagnostic data (e.g., offsets, expected vs actual checksums, byte limits) essential for debugging, recovery, or logging.
* **Go 1.13+ Error Wrapping & `errors.Is` / `errors.As` Mechanics**:
  - In Go 1.13+, wrapping an error using `fmt.Errorf("...: %w", err)` creates an error chain linked by an `Unwrap() error` (or `Unwrap() []error` in Go 1.20+) method.
  - `errors.Is(err, target)` walks the chain using `Unwrap()`. At each node, it checks for equality or calls `Is(error) bool` if implemented on the error.
  - `errors.As(err, target)` walks the chain looking for an error assignable to the type pointed to by `target`.
  - By implementing `func (e *KeyTooLargeError) Is(target error) bool { return target == ErrKeyTooLarge }`, a custom typed error automatically satisfies `errors.Is(wrapped, ErrKeyTooLarge)` anywhere in the unwrap chain!
* **Why Direct Error Equality (`err == ...`) Is Dangerous**: In modern Go, functions regularly annotate errors with contextual subsystem names (`fmt.Errorf("sstable read block: %w", err)`). If caller code compares using direct equality (`err == ErrChecksumMismatch`), the comparison returns `false` because the wrapped wrapper struct is not identical to the original sentinel. Comparing with `errors.Is(err, ErrChecksumMismatch)` traverses the unwrap chain and correctly detects the root cause.
* **Storage Engine Error Taxonomy & Semantic Boundaries**:
  - A storage engine must clearly separate operational states from catastrophic data corruptions.
  - *Expected domain conditions* (`ErrKeyNotFound`, `ErrEmptyKey`): Handled routinely in normal client request lifecycles.
  - *Constraint violations* (`ErrKeyTooLarge`, `ErrValueTooLarge`): Rejected at the boundary to prevent memory or buffer exhaustion.
  - *Transient resource conflicts* (`ErrCompactionRunning`): Operational cues that tell schedulers or maintenance workers to retry later.
  - *Data integrity failures* (`ErrChecksumMismatch`): Indicates media corruption, bit rot, or tampering; requires read-repair or fail-stop.
  - *Recovery cues* (`ErrTornWrite`): Signals a clean tail interruption from ungraceful shutdown/power loss, authorizing automated log truncation during startup replay.
* **Privacy & Defensive Formatting in Error Types**: Database systems frequently handle sensitive data (passwords, auth tokens, personal identifiers). If a key or value exceeds size limits, formatting the raw payload into the error string (e.g. `fmt.Sprintf("invalid key %s", key)`) risks writing sensitive customer secrets into plain-text application or cluster logs. In Lattice, `KeyTooLargeError` and `ValueTooLargeError` deliberately log only numeric byte sizes and limits, never raw bytes.
* **Why Structured Logging Matters in Systems Software**: Traditional `fmt.Println` or unstructured text logging produces strings that require fragile regular expressions to parse in production log aggregators (e.g. Elasticsearch, Grafana Loki, Datadog). In high-throughput distributed database engines, logs must be machine-parsable JSON lines where timestamp, severity, subsystem (`component`), operation latency, node IDs, and sequence numbers are discrete, indexed key-value attributes. This enables automated alerting, real-time metrics extraction, and correlation across distributed cluster nodes.
* **Log Levels and Operational Semantics**:
  - `DEBUG`: Verbose diagnostics (e.g. SkipList node splits, block index binary searches, socket frame byte counts). Disabled in production by default to prevent buffer churn and I/O saturation.
  - `INFO`: Normal state transitions and lifecycle milestones (e.g. node joined cluster, WAL segment rotated, SSTable flush completed, MemTable frozen).
  - `WARN`: Non-critical operational anomalies that are handled automatically (e.g. transient network retry, slow disk sync, secondary replica lag).
  - `ERROR`: Actionable failures, corruptions, or operational aborts (e.g. CRC checksum mismatch, torn write during recovery, disk full, compaction worker crash).
* **Concurrency-Safe Logging & Mutex-Free Handlers**: In concurrent systems where hundreds of client connections, background flushers, and compaction workers log simultaneously, the logging system must be thread-safe. Standard library `log/slog` handlers (`slog.JSONHandler`) manage write synchronization internally around the underlying `io.Writer`, preventing interleaved or garbled JSON records across goroutines without requiring caller-side mutexes.
* **Logging Errors Without Destroying Error Semantics**: When recording errors in logs, naively calling `log.Info("op failed: " + err.Error())` converts the error into an unstructured string and divorces the error identity from the log event. Lattice provides `logger.Err(err error) slog.Attr`, which records the error under the canonical `"error"` key as an `slog.Attr`. This allows log pipelines to filter on the existence of error attributes, while call sites retain access to `errors.Is` and `errors.As` on the underlying Go error object.
* **Automated Log Privacy & Sensitive-Data Redaction**: Storage engines are the custodian of customer data. If user keys, authentication tokens, API credentials, or database values are logged during debug sessions, cluster logs become high-severity data leakage vectors. Lattice implements automated attribute replacement via `slog.HandlerOptions.ReplaceAttr`: any attribute whose key matches sensitive keywords (`password`, `secret`, `token`, `auth`, `api_key`, `credentials`, etc.) is automatically replaced with `"[REDACTED]"`. Furthermore, custom domain types can implement the `Redactable` interface (`Redact() any`) to safely scrub sensitive fields before serialization. Under the defense-in-depth policy, sensitive attribute keys take strict precedence over custom `Redactable` value transformations: if an attribute key is classified as sensitive, the logger replaces the entire value with `"[REDACTED]"` without invoking `Redact()`, ensuring that buggy or hostile custom methods cannot bypass redaction. Only non-sensitive keys evaluate `Redactable` values.
* **Logging Failure Handling & Preventing Recursive Loops**: A fundamental invariant in systems software is that logging must never cause a recursive failure loop. If a disk becomes full or an output pipe breaks, attempting to log the logger's write failure can trigger an infinite logging recursion that crashes the process. The logger must fail gracefully (e.g. dropping records or reporting through independent telemetry) without escalating into unhandled panics.
* **Why Logging Must Not Become a Hidden Global Bottleneck**: In LSM storage engines processing 100,000+ ops/sec, synchronous file logging in the critical write path (e.g. inside WAL append or SkipList insertion) causes catastrophic tail latency spikes because disk writes and JSON formatting steal CPU and block worker threads. Storage engines must log only lifecycle events at `INFO` in the foreground, leaving high-frequency operation tracing to sampling metrics or asynchronous telemetry buffers.
* **Endianness**: Big-Endian (Network Byte Order) stores the most significant byte at the lowest memory address; Little-Endian stores the least significant byte first. Network protocols and on-disk files must specify a canonical byte order to remain portable across CPU architectures.
* **Fixed-Width Binary Integer Encoding**: Storing scalar numeric identifiers (timestamps, CRC32 checksums, record sequence numbers, key/value byte lengths) as fixed-width binary representations (2, 4, or 8 bytes) rather than textual strings (e.g. JSON or CSV numbers). Fixed binary layout guarantees constant-time $O(1)$ parsing, bounded memory envelopes, and predictable disk offsets.
* **Bounds Check Elimination (BCE) & Anti-Tear Buffer Guards**: In Go, accessing slice elements (`buf[i]`) triggers compiler-generated runtime bounds checks that verify `i < len(buf)` before each read or write. In naive implementations:
  ```go
  buf[0] = byte(v >> 24)
  buf[1] = byte(v >> 16)
  buf[2] = byte(v >> 8)
  buf[3] = byte(v)
  ```
  The runtime checks bounds 4 consecutive times. Worse, if `len(buf) == 2`, the function writes `buf[0]` and `buf[1]` before panicking on `buf[2]`, leaving mutated, corrupted ("torn") memory behind. By inserting an early bounds check `_ = buf[3]` at the function entry:
  1. The runtime panics immediately *before* any slice bytes are mutated, guaranteeing zero partial writes on undersized buffers.
  2. The Go compiler's SSA optimizer proves that indices 0, 1, and 2 are strictly within bounds, eliminating all three subsequent branch checks and generating branch-free machine instructions.
* **Zero-Allocation Systems Design & Empirical Benchmark Auditing**: High-throughput storage engines cannot tolerate heap allocation on hot primitive paths. Passing scalar values and slice headers by value operates strictly in CPU registers and on the goroutine stack. Rather than assuming a function is allocation-free, systems engineers verify zero-allocation properties empirically using `testing.B` with `b.ReportAllocs()`, proving that benchmark runs report exactly `0 B/op` and `0 allocs/op`.
* **Differential Reference Testing Against Independent Oracles**: A common testing trap in binary serialization is testing only round-trip identity (`Get(Put(x)) == x`). If an author mistakenly implements Little-Endian in both `Put` and `Get`, the round-trip test passes with 100% success despite producing byte streams that violate the architecture specification. Robust verification requires *differential testing* against an independent correctness oracle (e.g., standard library `encoding/binary.BigEndian`) and fixed known byte arrays to ensure bit-level correctness.
* **Variable-Length Integers (Varints)**: 7-bit varints (LEB128 format) encode unsigned integers into a variable number of bytes to save disk space and network bandwidth. Each byte uses 7 bits for data and the high bit ($0\text{x}80$) as a continuation flag. An integer $\le 127$ uses 1 byte, while $2^{64}-1$ uses 10 bytes. In SSTables and WAL metadata where most block offsets, record lengths, and shared key prefixes are small, varints compress metadata by up to $75\%$ compared to fixed 64-bit integers.
* **The 10th-Byte Constraint in 64-Bit Varints**: In a 7-bit varint, 9 bytes provide $9 \times 7 = 63$ bits. To reach 64 bits, a 10th byte is required, but it can only supply **1 bit** (the 64th bit, index 63). Therefore, for the 10th byte:
  - Payload bits 1..6 cannot be set (`b & 0x7E == 0`), because setting them represents values $\ge 2^{64}$, overflowing `uint64`.
  - The continuation bit cannot be set (`b & 0x80 == 0`), because that would request an 11th byte, which is illegal for a 64-bit integer.
  - In code, both conditions are caught in a single branch: if $i == 9$ and $b > 1$, the input is strictly an overflow (`ErrVarintOverflow`). The only valid bytes at index 9 are `0x00` and `0x01`.
* **Varint Bomb DoS Defense & Bounded Execution**: A malicious network client or corrupted disk record can transmit an endless stream of bytes with the continuation bit set (`0x80 0x80 0x80 ...`). If a decoder loops until it encounters $b < 0\text{x}80$ without an iteration limit, it can enter an infinite loop or scan gigabytes of memory, causing severe CPU exhaustion (Denial-of-Service). Lattice guarantees bounded execution: the decoding loop terminates at index 9, executing at most 10 iterations regardless of how large the input buffer is.
* **Truncation Error Semantics & Zero-on-Error Return Contract**: When a buffer terminates before a varint is complete (e.g. `[0x80]` or empty buffer), `GetVarint64` returns `(0, 0, errors.ErrVarintTruncated)`. Returning `0` bytes consumed on error is a critical safety guarantee: it prevents callers from accidentally advancing their buffer read cursor when a frame or record is incomplete.
* **Canonical vs Non-Canonical Varint Encodings**: A canonical varint uses the minimum required number of bytes (e.g. `0` encoded as `[0x00]`). An overlong or non-canonical varint encodes a small number using unnecessary continuation bytes whose payload bits are 0 (e.g. `0` encoded as `[0x80, 0x00]`). Lattice's encoder (`PutVarint64`) strictly emits canonical minimal encodings. Its decoder accepts valid non-canonical encodings up to 10 bytes for standard compatibility (matching Go's `encoding/binary.Uvarint`), while strictly rejecting overflows.
* **CRC32-IEEE Error Detection vs Cryptographic Authentication**: Cyclic Redundancy Checks compute a 32-bit polynomial residue using the IEEE 802.3 generator polynomial (`0xEDB88320`). CRC32 guarantees detection of all single-bit errors, all double-bit errors within standard block lengths, all odd numbers of bit errors, and any burst error up to 32 bits. However, CRC32 provides **zero cryptographic integrity or authentication**: an attacker modifying a disk record or network packet can trivially recalculate the valid CRC32 without a secret key. In Lattice, CRC32 is strictly used for *accidental error detection* (e.g., SSD bit rot, torn writes during kernel panics/power failure, framing misalignment).
* **Hardware-Accelerated CRC32 Throughput**: Rather than table-lookup loops in interpreted or unoptimized code, Go's `hash/crc32` standard library detects CPU architecture extensions at startup and executes hardware carryless multiplication instructions (ARM64 `PMULL`/`CRC32`, AMD64 `PCLMULQDQ`). This achieves ~11.7 to 13.8 GB/s throughput (~340 ns for a 4KB SSTable block) with strictly zero heap allocations (`0 B/op`, `0 allocs/op`).
* **Boolean Verify Contract vs Domain Error Decoupling**: The binary primitive `Verify(data []byte, expected uint32) bool` returns a simple boolean without allocating or returning `ErrChecksumMismatch`. Low-level binary primitives must remain lean and allocation-free, leaving domain-specific error wrapping, logging, and crash-recovery routing (e.g., distinguishing a torn write at the tail of a WAL from bit rot in the middle of a segment) to higher-level engine parsers.
* **Hard Key and Value Storage Bounds**: Storage engines enforce hard boundary limits on keys ($1 \le \text{KeyLen} \le 65,535$ bytes) and values ($0 \le \text{ValLen} \le 4,194,304$ bytes / 4 MiB) to prevent memory exhaustion, bound on-disk prefix compression, ensure 16-bit key length headers in binary WAL/SSTable records, and prevent long GC mark cycles. Nil/empty keys are rejected with `ErrEmptyKey`, while zero-length values are permitted as valid valueless markers (e.g., for sets or tombstone deletes).
* **O(1) Admission Validation Without Payload Inspection**: In database request ingestion, validation functions must inspect only the slice header's length (`len(s)`). Validating length requires 0 payload byte copies, 0 hash calculations, and 0 memory allocations (`0 B/op`, `0 allocs/op`). Benchmarks prove constant-time performance (~0.45 ns/op when return values are properly consumed to prevent dead-code elimination) whether validating a 16-byte key or a 4 MiB value.
* **Byte-Length Invariants vs Character/Rune Counts**: Storage layers are binary-safe. Key and value limits strictly measure raw byte length, not Unicode characters or runes. Multi-byte UTF-8 sequences (e.g. 4-byte runes) count as 4 bytes toward the 65,535-byte limit, preventing buffer overflow when binary headers reserve exactly 2 bytes for key length (`uint16`).
* **Strong Typing for Storage Primitives (`OpType`, `SeqNum`)**: In storage engine internals, representing operation types (`OpType byte`) and sequence numbers (`SeqNum uint64`) as distinct Go types rather than raw primitives prevents catastrophic parameter transposition bugs at compile time (e.g. passing a sequence number where a length or offset is expected).
* **64-Bit Monotonic Sequence Numbers & Exhaustion Mathematics**: A monotonically increasing 64-bit unsigned sequence number (`uint64`) provides a total order over all database writes. With $2^{64} = 18,446,744,073,709,551,616$ distinct sequence states, an engine operating at a sustained write throughput of 1,000,000 writes/second will not exhaust its sequence space for approximately $584,554\text{ Gregorian years}$ ($1.84 \times 10^{19} / (10^6 \times 86400 \times 365.2425) \approx 584,554$) or $584,542\text{ Julian years}$ ($365.25\text{ days}$). Overflow protection via `Next()` returning `ErrSeqNumOverflow` guarantees that the local scalar type never silently wraps to zero, though global sequence assignment across concurrent goroutines requires atomic coordination in the write path (Phase 02 WAL).
* **Tombstone Deletion Mechanics in LSM-Trees**: LSM storage engines are append-only; files on disk (SSTables) are immutable. Deletions cannot overwrite or erase previous writes in place without incurring expensive random I/O. Instead, deletions append a tombstone record (`OpTypeDelete = 0x02`) stamped with a higher sequence number. During point lookups and range scans, encountering a tombstone indicates that the key was deleted, shadowing all earlier versions. Tombstones can only be physically eradicated during background compaction when the record reaches the oldest level containing that key.
* **Zero-Value Semantics at Storage Admission**: In Go, default uninitialized variables hold zero bytes (`0x00`). In Lattice, `OpType(0x00)` is explicitly declared `OpTypeInvalid`. Storage ingestion pipelines validate all incoming opcodes (`op.Valid()`), ensuring uninitialized structs or corrupted network packets cannot masquerade as valid database operations.
* **Multi-Version InternalKey Storage Model**: An LSM-tree does not update records in place. Instead, it maintains multiple historical versions of each key across memory (MemTable) and disk (SSTables). Each entry is identified by an `InternalKey` composed of `UserKey []byte`, `SeqNum SeqNum`, and `OpType OpType`.
* **LSM Version Ordering Invariant (Newest First)**: In `CompareInternalKey`, when user keys are identical, records are sorted by `SeqNum` descending. Because higher sequence numbers correspond to newer database operations, descending sequence ordering guarantees that linear scans, binary searches, and SkipList point lookups encounter the latest version of a key first. Once the newest version is found (whether a `PUT` or a `DELETE` tombstone), older versions residing in deeper levels can be ignored.
* **OpType Tie-Breaking Semantics**: If two internal keys have identical user keys and identical sequence numbers, `OpType` acts as the tertiary tie-breaker sorted descending (`OpTypeDelete (0x02)` before `OpTypePut (0x01)`). This guarantees strict total ordering across all storage keys.
* **Mathematical Comparator Laws in Systems Software**: Custom comparators must strictly adhere to mathematical order axioms: reflexivity (`cmp(a, a) == 0`), antisymmetry (`sign(cmp(a, b)) == -sign(cmp(b, a))`), and transitivity (`a < b` and `b < c` $\implies a < c$). Violating transitivity corrupts binary search in SSTable blocks, breaks the heap property in K-way compaction priority queues, and causes SkipList forward pointers to form cyclic or un-traversable loops.
* **Defensive Copying vs Borrowed Slices in Storage Engines**: Slices in Go (`[]byte`) are reference headers sharing underlying backing arrays. If a constructor accepts a caller slice without copying, subsequent mutations by the caller silently corrupt in-memory indexes and on-disk records. `NewInternalKey` and `DecodeInternalKey` enforce memory safety by defensively copying `UserKey`. In performance-critical inner loops where inputs are known to be immutable (e.g. comparator checks), direct struct literal construction allows zero-allocation borrowed execution (`0 B/op`).
* **Memory Allocation & GC Pressure**: Creating millions of small heap allocations triggers frequent Go Garbage Collector mark-and-sweep cycles, stealing CPU cycles and introducing millisecond-level tail latency spikes. `sync.Pool` provides zero-allocation buffer reuse.

### Decisions Made & Trade-offs
* **Decision**: Single source of truth for key/value limits (`MinKeyLen = 1`, `MaxKeyLen = 65535`, `MinValueLen = 0`, `MaxValueLen = 4194304`), returning typed errors `*KeyTooLargeError` and `*ValueTooLargeError` with `KeySize`/`ValueSize` and `MaxSize`.
* **Alternative Considered**: Checking limits ad-hoc in WAL, MemTable, and TCP layers, or embedding raw payload strings in error messages.
* **Trade-off**: Requires dedicated validation functions and typed errors, but guarantees uniform boundary enforcement across all subsystems, enables callers to programmatically inspect sizes via `errors.As`, and prevents sensitive customer data from leaking into logs.
* **Decision**: Standard library `hash/crc32` with IEEE polynomial (`0xEDB88320`) and boolean verification API (`Verify(data, expected) bool`).
* **Alternative Considered**: Custom CRC implementation, CRC32C (Castagnoli), or returning `error` from `Verify`.
* **Trade-off**: Standard library IEEE variant avoids external dependencies, is universally interoperable with POSIX tools and network protocols, leverages Go's architecture-specific assembly optimizations, and avoids unnecessary error object allocations in hot parsing loops.
* **Decision**: Zero external dependencies for the entire storage engine and network layer; standard library only.
* **Alternative Considered**: Utilizing third-party serialization, networking, or logging libraries (e.g. gRPC, zap, zerolog, protobuf).
* **Trade-off**: Requires writing binary framing, SkipList, LRU cache, and wrapping `log/slog` from scratch, but eliminates supply-chain vulnerabilities, avoids transitive dependency conflicts, and ensures complete interview defensibility.
* **Decision**: Pure shift-and-mask Big-Endian implementation with early BCE check (`_ = buf[width-1]`) over `unsafe.Pointer` casting or runtime branch checks.
* **Alternative Considered**: Using `unsafe.Pointer` to reinterpret slice bytes, or inspecting host endianness at runtime.
* **Trade-off**: Requires manual bit-shifting code, but guarantees 100% memory safety, portability across heterogeneous architectures (ARM64, x86-64), and allows Go compiler pattern matching to emit single byte-swap machine instructions (`BSWAP` / `REV`).
* **Decision**: Bounded varint decoder with explicit 10th-byte validation (`b > 1`) and returning `(0, 0, err)` on any failure.
* **Alternative Considered**: Unbounded scanning for `b < 0x80`, or returning partial bytes consumed on error.
* **Trade-off**: Requires strict overflow and truncation branching, but bounds execution against Varint continuation-bit DoS attacks (capped at 10 iterations) and prevents callers from advancing read cursors on corrupted streams.
* **Decision**: Wrapping standard library `log/slog` rather than adopting third-party frameworks like Uber's `zap` or `zerolog`.
* **Alternative Considered**: Adding `go.uber.org/zap` for marginal allocation advantages in structured logging.
* **Trade-off**: `slog` introduced in Go 1.21 provides high-performance structured JSON and Text handlers directly in the Go standard library. Wrapping it provides full interface decoupling while preserving zero external runtime dependencies.
* **Decision**: Automated key-based and interface-based redaction in `ReplaceAttr` (`Redactable` interface).
* **Alternative Considered**: Relying on developers to manually redact sensitive keys at every call site.
* **Trade-off**: Incurs a minor map lookup per log attribute, but provides defense-in-depth against accidental leakage of passwords, bearer tokens, or API keys in persistent log streams.
* **Decision**: Zero-allocation no-op logger (`nopHandler`) for testing and suppressed logging.
* **Alternative Considered**: Discarding logs using `io.Discard` with a standard JSON handler.
* **Trade-off**: `io.Discard` still forces `slog` to format JSON strings and allocate attribute slices. Implementing `nopHandler.Enabled(...) bool { return false }` skips all formatting and argument evaluations completely.
* **Decision**: Strict containment of storage engine packages under `internal/` (`internal/wal`, `internal/memtable`, `internal/sstable`, etc.) with public client isolated in `pkg/client`.
* **Alternative Considered**: Flat root package structure or placing storage packages in `pkg/`.
* **Trade-off**: Prevents external applications from using internal engine components as independent micro-libraries, but completely prevents public API freeze and gives absolute freedom to refactor internal storage layouts and locking primitives without breaking external consumers.
* **Decision**: Enable high-signal linters (`govet`, `errcheck`, `staticcheck`, `ineffassign`, `unused`, `errorlint`, `nolintlint`) and `gofmt` while explicitly disabling noisy rules (`shadow`, `fieldalignment`).
* **Alternative Considered**: Enabling all 70+ available linters in `golangci-lint` or relying solely on `go vet`.
* **Trade-off**: Requires conscious configuration curation in `.golangci.yml`, but eliminates linter fatigue while providing bulletproof static verification against unchecked disk I/O errors and runtime type-assertion panics.
* **Decision**: Dual sentinel and contextual typed error architecture (`KeyTooLargeError`, `ValueTooLargeError`, `ChecksumMismatchError`, `TornWriteError` implementing `Is(target error) bool` matching sentinels).
* **Alternative Considered**: Sentinel errors only (string-based) or heavy hierarchical exception classes.
* **Trade-off**: Requires defining both sentinels and error structs with custom `Is()` methods, but provides zero-allocation sentinel checks for simple callers while enabling structured diagnostic inspection (`errors.As`) without breaking `%w` unwrap chains.
* **Decision**: Omitting raw key/value byte slices from size violation error structs (`KeyTooLargeError`, `ValueTooLargeError`), recording only sizes and thresholds.
* **Alternative Considered**: Formatting raw key/value strings into error messages for debugging convenience.
* **Trade-off**: Callers cannot inspect the raw payload directly from the error object, but completely prevents sensitive customer data, tokens, or PII from leaking into logs.
* **Decision**: Big-Endian encoding for all fixed-width integer fields; unsigned varints for internal data block offsets.
* **Trade-off**: Big-Endian requires bit-shifting on x86 machines, but guarantees deterministic portability across platforms and aligns with network standards.

### Security & Supply-Chain Considerations
* **Dependency Attacks**: In modern infrastructure software, malicious packages or hijacked maintainer accounts can inject backdoors. By maintaining zero external dependencies in the storage core, Lattice reduces its external attack surface to zero.
* **Defensive Anti-Tear Writes via BCE**: In low-level binary encoding, calling `PutUint32` with an undersized slice could write partial bytes before crashing. By anchoring an early bounds check (`_ = buf[3]`) prior to any slice mutation, the function guarantees an all-or-nothing write invariant: an invalid buffer panics without mutating a single byte of caller memory.
* **Varint Bomb Vulnerability**: A malicious stream containing endless continuation bytes ($0\text{x}80$) could cause an infinite loop or integer overflow. Lattice limits varint decoding to a maximum of 10 bytes; anything beyond returns `ErrVarintOverflow`.
* **Subsystem Isolation as Defense-in-Depth**: By keeping all storage engine implementation details inside `internal/`, we prevent external code or rogue consumer modules from accessing internal unexported memory pools, raw block caches, or un-synchronized file descriptors.
* **Audit-Proof Suppressions via `nolintlint`**: Unchecked use of `//nolint` can allow developers to bypass critical security and durability checks. By enforcing `require-specific: true` and `require-explanation: true`, any lint suppression must name the specific analyzer and justify the override in code review.
* **Log Redaction at Error Generation & Logger Level**: Error types and logging layers represent primary vectors for accidental credential and secret leakage. By designing error types that capture only metadata and providing automated logger-level key redaction (`[REDACTED]`) with strict sensitive-key precedence over custom `Redactable` implementations, Lattice enforces a multi-tier defense against credentials or sensitive values entering persistent log streams.

### Performance & Hardware Dynamics
* **Hardware Byte Swap Inlining**: Modern Go compilers recognize the standard big-endian shift-and-mask idiom (`buf[0] = byte(v >> 24); ...` and `uint32(buf[0])<<24 | ...`) and replace the operations with single hardware byte-reversal instructions (`BSWAP` on x86-64, `REV` on ARM64). On Apple Silicon (M4), `GetUint*` decodes at ~0.24 ns/op (single instruction execution), and `PutUint*` executes in ~0.50–0.90 ns/op when memory writes are observed, with 0 heap allocations.
* Package boundary placement has zero runtime CPU cost in Go; inlining and compiler optimizations occur across package boundaries during compilation.
* Disabling naive `fieldalignment` struct packing preserves intentional hardware cache-line padding (e.g. 64-byte spacing between write sequencer atomics and background flush pointers), preventing catastrophic multicore bus invalidation.
* Pre-allocated sentinel errors (`ErrKeyNotFound`, `ErrEmptyKey`) incur zero heap allocations on error return paths, preventing GC churn during frequent cache misses or negative key queries.
* Zero-allocation `nopHandler` (`Enabled(...) bool { return false }`) completely eliminates argument evaluation, string formatting, and heap allocation overhead in performance-critical benchmark suites where logging is disabled.

### Subsystem Interview Questions
* **Basic**: What is a Go module, and how does `go.mod` differ from the legacy `$GOPATH` workspace model?
* **Intermediate**: How does Go's Minimal Version Selection (MVS) differ from semver resolution algorithms used in npm or Cargo?
* **Deep**: What security guarantees does `go.sum` provide, and why does a repository with zero external dependencies not need a `go.sum` file until external packages are introduced?
* **Deep**: Why is zero external dependencies considered a major architectural advantage for a distributed database engine?
* **"Did You Actually Build This?"**: What happens if an external dependency author alters an already-tagged release in Git, and how does Go's toolchain detect it?
* **"Did You Actually Build This?"**: What is the purpose of `.editorconfig` in a Go systems project, and why must Go source files use tabs rather than spaces for indentation?
* **"Did You Actually Build This?"**: Why does Lattice place `wal`, `memtable`, and `sstable` under `internal/` rather than the repository root or `pkg/`?
* **"Did You Actually Build This?"**: What happens when two Go packages in a storage engine have mutually dependent structs (e.g., `Engine` needs `MemTable`, and `MemTable` needs `Engine` to trigger a flush)? How do you structure packages to prevent `import cycle not allowed`?
* **"Did You Actually Build This?"**: Why did you choose Big-Endian for fixed-width integers when modern x86-64 and ARM64 processors are predominantly Little-Endian?
* **"Did You Actually Build This?"**: In `PutUint32`, why did you include `_ = buf[3]` as the first line instead of writing directly to `buf[0]`? What does this do for both security and compiler performance?
* **"Did You Actually Build This?"**: How do you prove that your binary encoding functions perform zero heap allocations in Go?
* **"Did You Actually Build This?"**: Why is a round-trip test (`Get(Put(x)) == x`) insufficient on its own when testing binary serialization primitives?
* **"Did You Actually Build This?"**: Why is 127 a boundary in 7-bit varint encoding, and why does 128 require another byte?
* **"Did You Actually Build This?"**: Why does `math.MaxUint64` need 10 bytes, and what is the special constraint on the tenth byte?
* **"Did You Actually Build This?"**: How does `GetVarint64` detect integer overflow and avoid silent wraparound?
* **"Did You Actually Build This?"**: How does the decoder avoid infinite loops when fed hostile input like a "Varint Bomb"?
* **"Did You Actually Build This?"**: How is canonical encoding enforced by `PutVarint64`, and what is the decoder's policy on non-canonical encodings?
* **"Did You Actually Build This?"**: What exact return contract does `GetVarint64` adhere to when input is truncated, and why does it return zero bytes consumed?
* **"Did You Actually Build This?"**: Why must the test suite use an independent reference oracle instead of relying solely on round-trip `Get(Put(x)) == x` tests?
* **"Did You Actually Build This?"**: How do you prevent partial ("torn") buffer mutations in `PutVarint64` when the destination slice is undersized?
* **"Did You Actually Build This?"**: Why did Lattice choose CRC32-IEEE over cryptographic hashes like SHA-256 or MD5 for WAL records and SSTable blocks?
* **"Did You Actually Build This?"**: What is the difference between CRC32-IEEE and CRC32C (Castagnoli), and why does Lattice use the IEEE variant?
* **"Did You Actually Build This?"**: Why does `Verify(data, expected)` return a boolean rather than returning `ErrChecksumMismatch`?
* **"Did You Actually Build This?"**: Can CRC32 protect against a malicious adversary modifying database files or network packets?
* **"Did You Actually Build This?"**: How does Go's standard library achieve ~12-14 GB/s throughput for CRC32-IEEE without custom assembly written in Lattice?
* **"Did You Actually Build This?"**: Why is the maximum key size in Lattice exactly 65,535 bytes rather than 64,000 or 65,536 bytes?
* **"Did You Actually Build This?"**: Why are zero-length values permitted in Lattice while zero-length keys are strictly rejected?
* **"Did You Actually Build This?"**: How does `ValidateKey` ensure that multibyte UTF-8 strings do not bypass the 64 KB storage boundary?
* **"Did You Actually Build This?"**: Why does `ValidateKey` run in ~0.22 ns regardless of whether the key is 16 bytes or 65,535 bytes?
* **"Did You Actually Build This?"**: What information do `KeyTooLargeError` and `ValueTooLargeError` expose, and why are raw payload bytes omitted?
* **"Did You Actually Build This?"**: Why does Lattice define distinct types `type OpType byte` and `type SeqNum uint64` instead of passing raw `byte` and `uint64` values?
* **"Did You Actually Build This?"**: What is the mathematical exhaustion lifespan of a 64-bit sequence number at 1 million writes per second?
* **"Did You Actually Build This?"**: Why is `OpType(0x00)` defined as invalid rather than making `0x00 = PUT`?
* **"Did You Actually Build This?"**: How do sequence numbers and operation types resolve concurrent updates and deletions during K-Way compaction?
* **"Did You Actually Build This?"**: Why does `SeqNum.Next()` return an error at `MaxSeqNum` rather than letting Go wrap around to 0?
* **"Did You Actually Build This?"**: Why does `CompareInternalKey` sort sequence numbers descending rather than ascending?
* **"Did You Actually Build This?"**: What happens if an LSM-tree comparator violates transitivity? How does this manifest in production?
* **"Did You Actually Build This?"**: Why does `NewInternalKey` make a defensive copy of `UserKey` while `CompareInternalKey` does not allocate?
* **"Did You Actually Build This?"**: Why is the trailer size of an `InternalKey` exactly 9 bytes in Lattice, whereas LevelDB and RocksDB use 8 bytes?
* **"Did You Actually Build This?"**: Why is `bytes.Compare` used for user key ordering instead of converting keys to strings or comparing runes?

* **"Did You Actually Build This?"**: In your initial scaffolding, what subtle bug can occur when configuring `.gitignore` for compiled binary names like `lattice` and runtime directories like `wal/`?
* **"Did You Actually Build This?"**: Why must `cmd/` packages contain a dummy `func main() {}` in `main.go` even during a purely structural scaffolding phase?
* **"Did You Actually Build This?"**: Why does Lattice configure `errcheck` with `check-type-assertions: true` in `.golangci.yml`?
* **"Did You Actually Build This?"**: Why did you explicitly disable `fieldalignment` in `govet` for a database storage engine?
* **"Did You Actually Build This?"**: What syntax difference exists between `golangci-lint` v1 and v2 regarding formatting tools like `gofmt`?
* **"Did You Actually Build This?"**: Why did you configure `nolintlint` with `require-specific: true` and `require-explanation: true`?
* **"Did You Actually Build This?"**: Why does `KeyTooLargeError` implement `Is(target error) bool` returning `target == ErrKeyTooLarge`?
* **"Did You Actually Build This?"**: What is the critical control-flow difference between `ErrChecksumMismatch` and `ErrTornWrite` during WAL startup recovery?
* **"Did You Actually Build This?"**: Why does `internal/errors/errors.go` import the standard library as `import stdErrors "errors"`?
* **"Did You Actually Build This?"**: Why don't Lattice's size limit errors (`KeyTooLargeError`, `ValueTooLargeError`) include the offending key or value bytes in their `.Error()` string?
* **"Did You Actually Build This?"**: Why did you wrap Go's standard library `log/slog` rather than pulling in external logging frameworks like `zap` or `zerolog`?
* **"Did You Actually Build This?"**: How does your logger implementation handle sensitive data redaction, and why is `ReplaceAttr` better than manual sanitization at every log call site?
* **"Did You Actually Build This?"**: How does `NewNop()` achieve zero-allocation logging suppression in hot benchmark loops?
* **"Did You Actually Build This?"**: How does `Err(err error)` integrate domain errors without destroying their semantic types or masking root causes?
* **"Did You Actually Build This?"**: Why is synchronous disk logging dangerous in an LSM storage engine, and how should logging be separated between hot write paths and administrative operations?

---

# 2. Write-Ahead Log (WAL) & Durability Subsystem

### Concepts I Must Personally Understand
* **Write-Ahead Logging Invariant**: Changes must be securely committed to persistent storage before they are applied to in-memory data structures. If memory is modified first and a crash occurs during logging, data is lost or corrupted.
* **The OS Page Cache Boundary**: Calling `file.Write()` only copies bytes from user-space into kernel RAM (the page cache). Bytes are not durable until the kernel executes an I/O barrier (`fsync()` or `fdatasync()`).
* **Group Commit Mechanics**: Serializing individual `fdatasync()` calls caps throughput to storage IOPS ($1\text{k}-5\text{k}$ ops/sec). Coalescing writes across concurrent threads into a single batched `fdatasync()` scales throughput to $80\text{k}+$ ops/sec.

### Decisions Made & Trade-offs
* **Decision**: Group Commit pipeline with a cooperative leader-follower batch queue.
* **Alternative Considered**: Strict synchronous `fdatasync()` per write, or asynchronous background syncing.
* **Trade-off**: Adds a small micro-batch latency delay ($\le 2\text{ms}$), but increases write throughput by over $40\times$. Asynchronous syncing was rejected because it loses unwritten acknowledged writes on power loss.

### Failure Modes & Disaster Scenarios
* **Torn Tail Write**: A power loss mid-write writes only part of a record. Detected via CRC32 mismatch at EOF and truncated cleanly.
* **Mid-Log Bit Rot**: Corruption in the middle of a historical log triggers `panic`, as it indicates disk failure rather than a clean power drop.
* **Zero-Allocation 21-Byte Binary Framing**: Every WAL record begins with a fixed 21-byte physical header: `[CRC32 (4B) | RecordType (1B) | SeqNum (8B) | Timestamp (8B)]`. Key design invariants:
  - **Early Bounds Check (`_ = buf[20]`)**: In `EncodeHeader`, the 21st byte is read at entry. If the caller provides an undersized slice (`len < 21`), Go's runtime panics immediately *before* writing any bytes, preventing partial or torn writes to memory buffers.
  - **Uninitialized Memory Defense**: `RecordType(0x00)` is explicitly `RecordTypeInvalid`. In pre-allocated WAL files or uninitialized disk blocks (zeroed pages), an unwritten block presents a type byte of `0x00`. Decoders fail-fast with `*errors.InvalidRecordTypeError` rather than misinterpreting zeroed blocks as a valid `PUT` operation.
  - **Big-Endian Portability**: All multi-byte integers are serialized with strict network byte order (Big-Endian), ensuring binary WAL log segments can be transported and replayed across mixed-endian CPU architectures (ARM64, x86_64, RISC-V) without bit-shifting discrepancies.
* **Full Physical Record Framing & CRC Coverage**:
  - Full record layout: `[CRC32 (4B) | RecordType (1B) | SeqNum (8B) | Timestamp (8B) | KeyLength (2B) | KeyBytes (Var) | ValueLength (4B) | ValueBytes (Var)]`.
  - **Authoritative CRC Scope**: CRC32-IEEE is calculated across ALL bytes following the 4-byte CRC field: `RecordType || SeqNum || Timestamp || KeyLength || KeyBytes || ValueLength || ValueBytes`. The 4 CRC bytes themselves are excluded from calculation.
  - **Streaming Zero-Allocation CRC Replay**: In `DecodeRecord`, CRC is verified on-the-fly across read chunks using `crc32.Update(crc, crc32.IEEETable, chunk)` without allocating temporary contiguous buffers.
* **Stream-Safety & Hostile Reader Anti-DoS Invariants**:
  - **Early Allocation Bounding**: The decoder treats `io.Reader` streams as potentially hostile. Before executing `make([]byte, length)`, `keyLen` is checked against `binary.MaxKeyLen = 65,535` and `valLen` against `binary.MaxValueLen = 4,194,304` (4 MiB). Malicious length fields (e.g. `math.MaxUint32` 4 GiB) are rejected immediately with `*errors.ValueTooLargeError` without allocating heap memory or panicking from OOM.
  - **Partial I/O and Fragmentation Defense**: Streams must be read via `io.ReadFull`. Decoders safely process streams delivering data 1 byte at a time, arbitrary chunks, short reads returning fewer bytes than requested with nil error, and readers returning remaining data plus `io.EOF` simultaneously.
  - **Slice Ownership & Immutability**: Decoded `Key` and `Value` slices are newly allocated and strictly owned by the returned `Record`, guaranteeing zero aliasing with reader buffers or future decode calls. Encoding never mutates caller input slices.

* **CRC Coverage vs. Structural Validation**:
  - A WAL decoder must balance integrity verification against resource exhaustion (DoS) protection.
  - Not all bit corruptions reach the CRC verification phase. If a mutation alters `RecordType` to an invalid enum (e.g. `0x00` or `0x05`), or corrupts `ValueLength` to exceed `binary.MaxValueLen` (4 MiB), the decoder immediately fails fast with a structural error (`*errors.InvalidRecordTypeError` or `*errors.ValueTooLargeError`).
  - This early rejection is an intentional security perimeter: waiting for CRC verification before validating lengths would require allocating gigabytes of heap memory or reading gigabytes of stream data, opening catastrophic memory exhaustion vectors.
  - Thus, **structural validation protects the machine**, while **checksum verification protects data integrity**.
* **Exhaustive Single-Bit Corruption & Bit Restoration**:
  - Disk bit-rot typically begins with isolated single-bit flips in magnetic or solid-state media cells.
  - To prove detection, an exhaustive test flips each of the 8 bits across every single post-CRC byte ($4..\text{len}-1$). Every mutation must fail (either via checksum mismatch or structural error).
  - Furthermore, flipping the bit back must cleanly restore 100% valid decoding, proving that corruption detection is deterministic and non-destructive.
* **CRC Field Corruption Mechanics**:
  - The 4-byte CRC field itself is explicitly excluded from the checksum calculation input.
  - Therefore, mutating any bit in bytes 0..3 modifies the expected CRC without altering the actual computed CRC of the payload, guaranteeing an immediate, unambiguous `*errors.ChecksumMismatchError` with structured `Expected` and `Actual` fields.
* **Why Checksum Verification Is NOT Cryptographic Authentication**:
  - CRC32-IEEE uses the standard IEEE 802.3 polynomial (`0xEDB88320`) designed to detect accidental transmission noise, bit-rot, and torn blocks.
  - It has a 32-bit state space ($2^{32} \approx 4.29 \times 10^9$). On completely random multi-bit noise, the theoretical probability of an undetected collision is $2^{-32} \approx 2.33 \times 10^{-10}$.
  - Crucially, CRC32 is **non-cryptographic**. Any adversary with write access to the WAL file can modify key or value payloads and trivially recompute the matching CRC32 polynomial in nanoseconds. Tamper-proofing against hostile modification requires cryptographic MACs (HMAC-SHA256) or asymmetric digital signatures.

* **WAL Directory Ownership & Layout (`<db_path>/wal/`)**:
  - WAL segments are strictly isolated in a dedicated subdirectory: `<db_path>/wal/`.
  - Path construction uses platform-aware `filepath.Join(dbPath, wal.DirName)`, ensuring canonical path normalization, trailing slash trimming, and portable path separator handling.
* **Restrictive POSIX Permissions (`0700`) & Umask Security**:
  - Write-Ahead Logs store customer plaintext keys, values, and transaction metadata. If initialized with default umasks (e.g. `0755` or `0777`), other unprivileged local users or processes on multi-tenant servers could read or tamper with database logs.
  - Lattice enforces `0700` (`rwx------`): owner full access, zero access for group and others.
  - Because `0700` specifies `0` for group and other bits, process umask can never accidentally add permissions to group or others (`0700 &^ umask` preserves zeroed group/other bits).
  - Furthermore, if an existing WAL directory possesses loose permissions (e.g. `info.Mode().Perm() & 0077 != 0`), `InitDir` automatically hardens it to `0700` using descriptor-based `fchmod` (`f.Chmod`) combined with `os.SameFile` inode pinning to prevent symlink-substitution TOCTOU attacks.
* **Atomic Creation & TOCTOU Race Mitigation**:
  - In concurrent database systems, naive initialization patterns perform a check-then-act sequence:
    ```go
    // ANTI-PATTERN: Vulnerable to TOCTOU race
    if _, err := os.Stat(walPath); os.IsNotExist(err) {
        os.Mkdir(walPath, 0700)
    }
    ```
    This sequence creates a Time-of-Check to Time-of-Use (TOCTOU) vulnerability where a concurrent goroutine or hostile process creates a file, symlink, or directory in the microsecond between `Stat()` and `Mkdir()`.
  - Lattice executes direct atomic `os.Mkdir(walPath, DirMode)`. If it succeeds, the directory was created atomically by the kernel, eliminating create/check races. If it returns `os.ErrExist`, the path already exists, and Lattice transitions to authoritative inspection and descriptor-based hardening.
* **Symlink Rejection, Inode Pinning & Conflicting Object Defense**:
  - Inspection of existing paths begins with `os.Lstat(walPath)` rather than `os.Stat(walPath)`.
  - `os.Lstat` does not follow symbolic links. If an attacker or misconfigured deployment places a symlink at `<db_path>/wal` pointing to `/etc` or a separate sensitive volume, `os.Stat` would report a directory, causing the database to write logs into the symlink target.
  - `os.Lstat` detects `info.Mode() & os.ModeSymlink != 0` and immediately aborts with `*errors.NotADirectoryError` (matching `errors.ErrNotADirectory`).
  - To prevent a TOCTOU race where an existing directory is replaced with a symlink between type verification and permission hardening, Lattice opens the directory descriptor (`os.Open`), verifies the descriptor refers to a directory and matches the initial `Lstat` inode via `os.SameFile(info, finfo)`, executes `f.Chmod(0700)` (`fchmod` operating strictly on the open file descriptor without pathname resolution), and verifies with a post-hardening `os.Lstat` that the pathname was not swapped during hardening.
  - Likewise, if `<db_path>/wal` is an existing regular file, FIFO, socket, or device, `InitDir` rejects it without modifying, truncating, or deleting the conflicting object.
* **Idempotency & Pre-existing File Safety**:
  - Database reboots and repeated initialization calls must be non-destructive.
  - Calling `InitDir` on an already initialized WAL directory converges cleanly. Pre-existing WAL segments (`wal_*.log`) and database state files (`MANIFEST`, `CURRENT`) are never truncated, modified, or deleted. The directory inode itself is preserved without recreation.
* **"Strict Sync" Durability Contract (`WALWriter.AppendSync`)**:
  - A database write is not committed merely because `file.Write()` accepted bytes into the operating system page cache. A kernel panic or sudden power loss immediately vaporizes uncommitted dirty pages.
  - Lattice enforces the **Strict Sync** contract: `AppendSync(rec)` returns `nil` IF AND ONLY IF:
    1. The complete serialized record bytes have been transferred to the operating system (`written == len(buf)`).
    2. The hardware durability barrier (`fdatasync`) has completed without error.
  - If a write is incomplete, or if `fdatasync` fails, `AppendSync` returns an error. It **never** returns `nil` on partial writes or synchronization failures.
* **Three Tiers of Write Durability**:
  1. *OS-Visible Write* (`file.Write`): Bytes reside in volatile kernel page cache (RAM). Read calls from other processes can observe the data, but power loss destroys it.
  2. *Filesystem Synchronization* (`fdatasync`): Operating system flushes dirty page cache buffers to the storage controller command queue, satisfying the POSIX durability contract.
  3. *Physical Media Durability*: Storage controller flushes non-volatile write cache lines to physical flash blocks/cells. True media durability depends on controller capacitor backups, drive firmware, and hardware barrier execution.
* **`fsync()` vs `fdatasync()` Performance Optimization**:
  - `fsync(fd)` flushes modified in-core data blocks AND all associated inode metadata (access time, modification time, file size), typically forcing two separate physical write operations to disk.
  - `fdatasync(fd)` flushes only modified data blocks and updates metadata only if the physical file size has changed.
  - In `Lattice`, Linux builds use `syscall.Fdatasync(int(f.Fd()))`. On non-Linux platforms (Darwin, Windows) where `fdatasync(2)` is unavailable in the kernel, it falls back to `f.Sync()`.
* **Partial Writes, Short Writes & Torn Tails**:
  - Disk full conditions or interrupted system calls can produce short writes (`0 < n < len(buf)`). `WALWriter` loops until all bytes are written. If a write returns 0 bytes without an error, it terminates with `io.ErrShortWrite` to prevent infinite busy loops.
  - If a write partially succeeds and then encounters an unrecoverable disk error, the filesystem contains a partial record (torn tail).
  - Systems Invariant: `AppendSync` does **not** attempt automatic rollback or file truncation during a failed append. Doing so during disk distress risks further corruption. Instead, cleanup of torn tail writes at EOF is strictly deferred to startup recovery (`P02-S03-M01`).
* **Non-Destructive File Opening & Append Invariants**:
  - WAL segment files are opened with `os.O_WRONLY | os.O_CREATE | os.O_APPEND` with `0600` (`FileMode`) permissions.
  - `O_TRUNC` is strictly prohibited. When reopening an existing WAL segment upon process restart, `O_APPEND` positions every write to EOF, preserving all pre-existing records intact.
* **Concurrency Model: Per-Writer Mutex Serialization**:
  - Multiple goroutines calling `AppendSync` on the same `WALWriter` are serialized by an internal `sync.Mutex`.
  - This ensures that record frames are strictly atomic on disk: goroutine A's record and goroutine B's record are never interleaved or torn.
  - No global process mutex is used; concurrency protection is localized to the active segment writer. Group Commit (`P02-S04`) will later build on this to amortize `fdatasync` across concurrent callers.

### Subsystem Interview Questions
* **Basic**: Why does a database need a Write-Ahead Log?
* **Intermediate**: What is the difference between `fsync()` and `fdatasync()`, and why does pre-allocating WAL files matter?
* **Intermediate**: Why is the WAL record header fixed at 21 bytes, and how does `RecordType(0x00) = Invalid` protect against torn page cache flushes?
* **Deep**: Why isn't `file.Write()` success enough for a database WAL? What are the three tiers of write durability?
* **Deep**: What exact contract does `AppendSync` provide, and what happens if `fdatasync()` fails after `file.Write()` succeeds?
* **Deep**: If a write fails halfway through writing a WAL record, why doesn't `AppendSync` truncate the file immediately, and why does recovery own torn tail cleanup?
* **Deep**: Why must concurrent WAL writes never be interleaved, and how does Lattice prevent interleaving without a global mutex?
* **Deep**: Why is `AppendSync` deliberately simpler than later Group Commit?
* **Deep**: Walk me through the exact concurrency flow of your Group Commit implementation. What happens if the leader goroutine panics while holding the batch?
* **Deep**: How does Go's bounds check elimination (BCE) pattern `_ = buf[20]` provide anti-tear guarantees during binary serialization?
* **Deep**: How do you prevent denial-of-service memory exhaustion when streaming WAL records from an untrusted or corrupted io.Reader?
* **Deep**: Walk me through the exact CRC coverage in Lattice's WAL records. Why is the CRC field excluded from its own checksum, and how does streaming CRC verification work without allocating contiguous buffers?
* **Deep**: Why does a WAL decoder intercept corruptions with structural errors like `ValueTooLargeError` before performing CRC32 checksum verification?
* **Deep**: Why is CRC32 insufficient for cryptographic tamper detection in distributed logs, and what are its mathematical error-detection guarantees versus an HMAC?
* **Deep**: How does Lattice avoid Time-of-Check to Time-of-Use (TOCTOU) race conditions when initializing the WAL directory?
* **Deep**: Why does Lattice use `os.Lstat` instead of `os.Stat` when inspecting an existing WAL path, and what security threat does this mitigate?
* **Deep**: In the existing-directory permission hardening path, why is pathname-based `os.Chmod` vulnerable to TOCTOU, and how does Lattice mitigate it using descriptor-based `fchmod` and `os.SameFile`?
* **Follow-up**: How do you prevent group commit queues from consuming unbounded RAM if the disk becomes completely saturated?
* **"Did You Actually Build This?"**: How do you distinguish between an uncompleted torn write at the tail of the WAL versus a corrupted record in the middle of the file during startup recovery?
* **"Did You Actually Build This?"**: How do you handle readers that return fewer bytes than requested without an error (short reads) or readers that return data and io.EOF in the same call?
* **"Did You Actually Build This?"**: Walk me through how you proved your WAL checksum integrity across every field. Did you test single-bit flips, and what happens if you mutate the CRC field itself?
* **"Did You Actually Build This?"**: What happens if two goroutines call WAL directory initialization at the exact same millisecond when the directory does not yet exist? Does it require a mutex?

---


# 3. In-Memory MemTable & Concurrent SkipList

### Concepts I Must Personally Understand
* **Probabilistic SkipList**: A hierarchy of linked lists where higher levels act as "express lanes". Nodes have probabilistic heights governed by a geometric coin flip ($p=0.25$). Expected search, insert, and delete complexity is $O(\log N)$.
* **Lock-Free Read Traversal**: Because SkipList nodes are never rebalanced or rotated (unlike AVL or Red-Black trees), forward pointers can be read concurrently using `atomic.LoadPointer` without acquiring locks.
* **Memory Accounting**: A MemTable must track its exact byte footprint (keys + values + node headers + pointer slices) so it knows when to trigger a flush to disk.

### Decisions Made & Trade-offs
* **Decision**: Probabilistic SkipList with exclusive write locking and lock-free atomic reads.
* **Alternative Considered**: Concurrent Hash Map, Red-Black Tree, B+ Tree in RAM.
* **Trade-off**: Hash maps do not support range scans. Red-Black trees require complex rotations that necessitate coarse-grained locking. SkipLists have higher pointer memory overhead (~$1.33$ pointers per node average), which is an acceptable cost for lock-free read concurrency.

### Concurrency Invariants
* Writers acquire a mutex before inserting nodes and splicing pointers.
* Splicing executes from bottom (Level 0) to top (Level $H-1$), ensuring concurrent readers traversing at higher levels never observe dangling or uninitialized pointers.

### Subsystem Interview Questions
* **Basic**: What is the time complexity of SkipList search, insert, and delete?
* **Intermediate**: Why are SkipLists preferred over Red-Black trees in modern storage engines like RocksDB and LevelDB?
* **Deep**: How do you ensure that a concurrent reader does not read a partially initialized node while a writer is inserting it into the SkipList?
* **Follow-up**: How do you calculate the exact memory usage of a MemTable in Go, given that slices and structs have internal overhead?
* **"Did You Actually Build This?"**: What happens if two concurrent writers attempt to insert keys with the same user key but different sequence numbers? How does your SkipList order them?

---

# 4. Persistent SSTable File Formats & Block Layout

### Concepts I Must Personally Understand
* **SSTable (Sorted String Table)**: An immutable on-disk file storing ordered key-value records organized into fixed-size data blocks (typically 4KB).
* **Prefix Compression**: Consecutive sorted keys often share identical prefixes. Storing shared length, unshared length, and delta bytes reduces disk footprint significantly.
* **Restart Points**: To allow binary search within a compressed block, prefix compression is reset every 16 records (`SharedLen = 0`). The restart array at the end of the block stores 32-bit offsets to these uncompressed records.
* **Two-Level Block Index**: A sparse index storing only the largest key of each 4KB data block. To find a key, the reader binary searches the in-memory index to locate the single candidate 4KB block, then binary searches within that block.

### Decisions Made & Trade-offs
* **Decision**: Fixed 48-byte footer containing handles to the index and metaindex blocks, terminated by an 8-byte magic number (`0x4C41545453535401`).
* **Alternative Considered**: Header at the start of the file.
* **Trade-off**: Putting metadata handles at the end (footer) allows writing the file sequentially in a single pass without knowing final offsets in advance.

### Security Considerations
* **Corrupted Block Offset Attack**: A maliciously altered index handle pointing outside file bounds could cause out-of-bounds reads. Every offset and size read from the index is validated against the physical file size.

### Subsystem Interview Questions
* **Basic**: What is an SSTable, and why are SSTable files immutable?
* **Intermediate**: How does prefix compression work inside an SSTable data block, and why are restart points necessary?
* **Deep**: Describe the exact layout of your SSTable footer and how the reader uses it to bootstrap a point lookup.
* **Follow-up**: What is the difference between a dense index and a sparse index, and why do LSM-trees use sparse indexes?
* **"Did You Actually Build This?"**: If a key does not exist in an SSTable, what is the exact binary search logic used on the sparse index to determine that it isn't present without reading every block?

---

# 5. Probabilistic Bloom Filter Subsystem

### Concepts I Must Personally Understand
* **Bloom Filter Theory**: A bit array of $m$ bits with $k$ independent hash functions. Tests set membership with zero false negatives (if it returns False, the key definitely does not exist) and bounded false positives.
* **Kirsch-Mitzenmacher Optimization**: Instead of computing $k$ independent hash functions, compute two 64-bit hashes $h_1(x)$ and $h_2(x)$ using Murmur3, and derive the remaining hashes via $g_i(x) = h_1(x) + i \cdot h_2(x) \pmod{m}$. This reduces CPU hashing overhead by over $60\%$.
* **Sizing Formula**: With 10 bits per key and $k=7$ hash functions, the empirical false positive rate is approximately $0.82\%$ (under 1%).

### Decisions Made & Trade-offs
* **Decision**: 10 bits per key, Murmur3 double-hashing, integrated into SSTable filter blocks.
* **Alternative Considered**: Cuckoo filter or per-block Bloom filters.
* **Trade-off**: Bloom filters do not support deletion (which is fine because SSTables are immutable). Cuckoo filters support deletion but are more complex to serialize and have worse memory locality.

### Performance & Hardware Dynamics
* A Bloom filter lookup requires only $k=7$ memory bit reads, which execute in CPU L1/L2 cache and avoid a costly NVMe flash read ($50-100\mu\text{s}$).

### Subsystem Interview Questions
* **Basic**: What problem does a Bloom filter solve in an LSM storage engine?
* **Intermediate**: Why can a Bloom filter produce false positives but never false negatives?
* **Deep**: Explain the Kirsch-Mitzenmacher double-hashing technique and why it is mathematically sound.
* **Follow-up**: If you have 100,000 keys in an SSTable, how many bytes does a 10-bit/key Bloom filter consume in memory?
* **"Did You Actually Build This?"**: How do you measure the empirical false positive rate of your Bloom filter implementation to verify it matches theoretical expectations?

---

# 6. Manifest Log & VersionSet Concurrency

### Concepts I Must Personally Understand
* **The Version Concept**: An immutable snapshot representing the exact set of active SSTables across all levels ($L_0..L_N$) at a single point in time.
* **Atomic Version Transitions (`VersionEdit`)**: Flushes and compactions create and delete files. These transitions are represented as `VersionEdit` delta records appended to the `MANIFEST` file.
* **Version-Pinned Reference Counting**: Active readers increment `version.refCount`. A compaction installs a new `Version` via an atomic pointer swap, but obsolete files cannot be unlinked from disk until the old version's reference count drops to zero.

### Decisions Made & Trade-offs
* **Decision**: Append-only `MANIFEST` log with a `CURRENT` pointer file (RocksDB/LevelDB model).
* **Alternative Considered**: Overwriting a monolithic `metadata.json` state file on every flush/compaction.
* **Trade-off**: Appending small edits is crash-safe and high performance; rewriting a monolithic state file risks corruption on power failure and incurs high write amplification as the file set grows.

### Subsystem Interview Questions
* **Basic**: What is the purpose of the `MANIFEST` file?
* **Intermediate**: How does the `CURRENT` file prevent corruption during startup?
* **Deep**: How does Lattice allow background compactions to delete SSTables while concurrent readers are actively reading from them?
* **Follow-up**: What happens if the database crashes after writing a new SSTable but before appending the `VersionEdit` to the `MANIFEST`?
* **"Did You Actually Build This?"**: Walk me through the exact garbage collection lifecycle of an obsolete SSTable file from the moment compaction finishes until the physical file is unlinked.

---

# 7. Crash Recovery, Torn Writes & Startup Replay

### Concepts I Must Personally Understand
* **Recovery Ordering**:
  1. Read `CURRENT` to find the active `MANIFEST`.
  2. Replay `MANIFEST` to reconstruct the active `VersionSet` (SSTable levels).
  3. Identify all uncommitted WAL logs newer than the manifest checkpoint.
  4. Replay WAL records sequentially into the MemTable.
  5. Validate that all referenced SSTable files physically exist on disk.
* **Torn Write Truncation Policy**: If a CRC32 mismatch occurs at the very end of the active WAL log, it represents an interrupted partial write. Truncate to the last valid byte and proceed. Mid-log corruption is fatal.

### Failure Modes & Disaster Scenarios
* **Missing SSTable File**: If an SSTable recorded in the manifest is missing from disk, Lattice halts with `ErrMissingSSTable` to prevent serving corrupted partial datasets.

### Subsystem Interview Questions
* **Basic**: What steps does Lattice take when starting up after an ungraceful crash?
* **Intermediate**: How do you distinguish between an uncommitted partial write at EOF and data corruption in the middle of a log?
* **Deep**: How does Lattice ensure that replaying the WAL after a crash does not result in duplicate keys or sequence number regressions?
* **Follow-up**: What happens if the machine crashes while the recovery process itself is running?
* **"Did You Actually Build This?"**: If a flush completes and writes an SSTable to disk, but the process is killed before the WAL is deleted, how do you prevent the recovered database from inserting duplicate records from the old WAL?

---

# 8. Leveled Compaction & K-Way Merge Sort

### Concepts I Must Personally Understand
* **Leveled Compaction Invariant**: For any level $L_i$ ($i \ge 1$), no two SSTables share overlapping key ranges. At most one SSTable lookup per level is required.
* **Compaction Scoring**: Compaction is triggered when $L_0$ file count exceeds 4, or when the total byte size of $L_i$ exceeds $10\text{MB} \times 10^{i-1}$.
* **K-Way Merge Sort**: Compaction streams inputs through a min-heap priority queue, merging records in ascending key order and descending sequence number order.
* **Tombstone Eradication Rule**: A tombstone can only be dropped if no deeper level ($L_{target+1}..L_N$) contains a revision of that key. Dropping it prematurely causes deleted keys to reappear ("ghost key resurrect").

### Decisions Made & Trade-offs
* **Decision**: Leveled Compaction with a $10\times$ level size multiplier.
* **Alternative Considered**: Size-Tiered Compaction.
* **Trade-off**: Higher write amplification during merges, but strictly bounded read amplification ($O(L)$ lookups) and bounded space amplification (~$1.11-1.33\times$).

### Subsystem Interview Questions
* **Basic**: Why is compaction necessary in an LSM-tree database?
* **Intermediate**: What is the difference between Level 0 and Level 1 in Leveled Compaction?
* **Deep**: Explain the "Ghost Key Resurrect" bug. How does your compaction algorithm mathematically prove it is safe to purge a tombstone?
* **Follow-up**: How does the K-Way merge sort resolve duplicate keys with different sequence numbers?
* **"Did You Actually Build This?"**: What happens if write traffic is so heavy that compaction cannot keep up? How does your engine prevent unbounded $L_0$ file accumulation?

---

# 9. Sharded LRU Read Block Cache

### Concepts I Must Personally Understand
* **Why MemTable is Not a Read Cache**: The MemTable caches only recent, un-flushed writes. Once flushed to an SSTable, hot data resides on disk. A dedicated block cache stores decompressed 4KB data blocks in RAM.
* **Mutex Contention & Cache Sharding**: A single global LRU cache protected by a single mutex creates extreme lock contention across multi-core CPUs. Sharding the cache into 16 independent LRU instances eliminates contention.
* **False Sharing**: Placing two cache shard structs on the same 64-byte hardware cache line causes CPU cache line invalidation on concurrent access. Pad shard structs to 64-byte boundaries.

### Subsystem Interview Questions
* **Basic**: What is an LRU cache, and why is it needed in an LSM-tree if the MemTable is already in RAM?
* **Intermediate**: How does sharding an LRU cache reduce lock contention?
* **Deep**: What is false sharing in multi-threaded systems, and how do you prevent it in Go?
* **Follow-up**: What are the trade-offs between caching decompressed blocks in user space versus relying on the operating system page cache?
* **"Did You Actually Build This?"**: What eviction policy do you use when an inserted 4KB block exceeds the shard's configured memory budget?

---

# 10. Single-Node Engine Integration & Backpressure

### Concepts I Must Personally Understand
* **Progressive Write Pacing**: If $L0$ file count exceeds 8, delay incoming writes by $1\text{ms}$. If it exceeds 12, throttle writes to match compaction throughput. This prevents disk space exhaustion and maintains stable tail latencies.
* **Graceful Shutdown**: Close the network listener $\to$ wait for active requests to finish $\to$ flush the active MemTable $\to$ wait for background compactions to halt $\to$ sync the manifest $\to$ close all file descriptors.

### Subsystem Interview Questions
* **Basic**: What happens to incoming writes when the MemTable reaches its 64MB capacity?
* **Intermediate**: How does Lattice prevent write stalls from crashing the server?
* **Deep**: Walk through the lifecycle of an engine shutdown. How do you guarantee zero in-flight data loss?
* **Follow-up**: How do readers and writers synchronize during the atomic swap of the active MemTable to the immutable MemTable?
* **"Did You Actually Build This?"**: If a flush takes 500ms to complete, can the database still accept new writes during that window? Why or why not?

---

# 11. TCP Binary Wire Protocol & Networking

### Concepts I Must Personally Understand
* **Frame Anatomy**: 18-byte fixed header (`Magic: 0x4C415454`, `OpCode`, `Flags`, `SeqID`, `PayloadLength`) + Variable Payload + 4-byte `CRC32`.
* **Frame-Bomb Protection**: Reject frames where `PayloadLength > 5MB` before allocating memory buffers.
* **TCP Stream Framing**: TCP does not have native message boundaries; a single `read()` call can return a partial frame or multiple coalesced frames. The decoder must maintain a buffer state machine.

### Subsystem Interview Questions
* **Basic**: Why use a custom binary protocol over TCP instead of JSON over HTTP?
* **Intermediate**: How do you detect and handle partial frame reads over a non-blocking TCP socket?
* **Deep**: How do you protect a network server against memory exhaustion attacks caused by malformed frame length headers?
* **Follow-up**: What is the purpose of the 8-byte Sequence ID in your binary header?
* **"Did You Actually Build This?"**: How do you pool byte buffers in Go using `sync.Pool` to achieve zero heap allocations on the network read path?

---

# 12. Distributed Foundations & Node Topology

### Concepts I Must Personally Understand
* **Node Identity**: Each node has a persistent unique ID (`uint64`) and an address list of cluster peers.
* **RPC Transport**: Persistent TCP connections between cluster nodes with keep-alive probes and heartbeat multiplexing.
* **Cluster Sizing**: Consensus clusters use an odd number of nodes ($N=3$ or $N=5$) to maximize fault tolerance ($F = \lfloor (N-1)/2 \rfloor$).

### Subsystem Interview Questions
* **Basic**: Why do distributed consensus clusters typically have 3 or 5 nodes instead of 4 or 6?
* **Intermediate**: How do cluster peers discover each other and maintain persistent connections?
* **Deep**: What happens if two nodes in a 3-node cluster attempt to start an election simultaneously?
* **Follow-up**: How does the node transport distinguish between client requests and peer-to-peer Raft messages?
* **"Did You Actually Build This?"**: How do you prevent connection leaks when a peer repeatedly disconnects and reconnects?

---

# 13. Raft Consensus Engine (V1.1)

### Concepts I Must Personally Understand
* **The Three Sub-Problems of Raft**:
  1. **Leader Election**: Randomized timers ($150-300\text{ms}$) prevent split votes; majority votes ($Q = \lfloor N/2 \rfloor + 1$) elect a single leader.
  2. **Log Replication**: Leader appends proposals to its log and replicates via `AppendEntries`. Entries commit once stored on a majority.
  3. **Safety Invariants**: Election Safety, Leader Append-Only, Log Matching, Leader Completeness, State Machine Safety.
* **Split-Brain Mitigation**: A partitioned minority ($N=1$ in a 3-node cluster) cannot form a quorum ($1 < 2$) and cannot commit writes. The majority partition ($N=2$) continues safely.

### Subsystem Interview Questions
* **Basic**: Explain how Raft elects a leader.
* **Intermediate**: Why must Raft election timers be randomized?
* **Deep**: Explain the Log Matching Invariant and how the leader enforces it using `prevLogIndex` and `prevLogTerm`.
* **Follow-up**: What happens if a candidate receives a vote from a node with a lower term?
* **"Did You Actually Build This?"**: Walk through what happens when an old partitioned leader reconnects to the cluster after the other two nodes have elected a new leader and committed 50 new writes.

---

# 14. Distributed State Machine Replication

### Concepts I Must Personally Understand
* **Replicated State Machine Model**: Identical state machines fed identical sequences of committed commands in identical order reach identical states.
* **Separation of Concerns**: Raft manages log replication and consensus; the LSM storage engine acts as the state machine that applies committed log entries.
* **Follower Redirection**: If a client sends a write request to a follower, the follower responds with a redirect error containing the leader's network address.

### Subsystem Interview Questions
* **Basic**: What is a Replicated State Machine?
* **Intermediate**: At what exact moment does the leader apply a log entry to its local LSM storage engine?
* **Deep**: How do you prevent duplicate command execution if a client retries a proposal that was committed but whose response was lost in transit?
* **Follow-up**: What interface must the storage engine implement to plug into the Raft consensus module?
* **"Did You Actually Build This?"**: What happens if the leader commits an entry, applies it to its local engine, and crashes before notifying followers to apply it?

---

# 15. Linearizable Reads & the ReadIndex Protocol

### Concepts I Must Personally Understand
* **The Stale Read Threat**: A partitioned leader that has not yet discovered it was deposed could serve reads from its local engine that do not reflect writes committed by a new leader.
* **The ReadIndex Protocol**:
  1. Leader records current `commitIndex`.
  2. Leader sends a heartbeat broadcast to followers to confirm a majority still acknowledges its leadership.
  3. Leader waits until its local state machine applies entries up to that `commitIndex`.
  4. Leader serves the read from its local engine.
* **Linearizability Guarantee**: Reads always reflect the latest committed write in the global cluster timeline without writing read requests to the replicated log.

### Subsystem Interview Questions
* **Basic**: What is a linearizable read?
* **Intermediate**: Why can't a Raft leader simply serve reads directly from its local storage engine?
* **Deep**: Walk through the exact steps of the `ReadIndex` protocol. Why does it not require appending a log entry to disk?
* **Follow-up**: How does `ReadIndex` compare to Leader Leases, and what are the clock-drift risks of Leader Leases?
* **"Did You Actually Build This?"**: How do you test whether your database allows stale reads during a simulated network partition?

---

# 16. Fault Injection & Chaos Engineering

### Concepts I Must Personally Understand
* **Simulated Network Partitions**: Using software drop rules to disconnect nodes and verify consensus safety.
* **Process Termination Testing**: Injecting `kill -9` (`SIGKILL`) signals during high write throughput to prove zero acknowledged data loss.
* **Disk Fault Simulation**: Mocking the filesystem interface (`VFS`) to inject I/O errors during `fdatasync()` and verify engine recovery behavior.

### Subsystem Interview Questions
* **Basic**: What is chaos testing in distributed databases?
* **Intermediate**: How do you verify that a database survives sudden power failure without physical hardware testing?
* **Deep**: Describe the design of a test that proves zero split-brain writes can occur under a network partition.
* **Follow-up**: What invariants do you assert when a node restarts after an unexpected crash?
* **"Did You Actually Build This?"**: What was the most subtle bug uncovered by your chaos or race-detection test suite?

---

# 17. System-Wide Security & Threat Modeling

### Concepts I Must Personally Understand
* **Path Traversal Defense**: All database filenames (`wal_*.log`, `*.sst`, `MANIFEST-*`) are validated against a strict regex whitelist (`^[a-zA-Z0-9_-]+$`) to prevent directory traversal attacks (`../../etc/passwd`).
* **Resource Exhaustion Defense**: Max connection limits ($4,096$), max frame payload limits ($5\text{MB}$), connection read/write timeouts ($5\text{s}$).
* **Memory Bleed Sanitization**: Byte buffers recycled via `sync.Pool` are zeroed or truncated before reuse to prevent cross-connection data leakage.

### Subsystem Interview Questions
* **Basic**: What security risks exist in a raw TCP database protocol?
* **Intermediate**: How do you prevent path traversal attacks when storing SSTable files on disk?
* **Deep**: How does Lattice protect itself against Slowloris-style connection exhaustion attacks?
* **Follow-up**: What are the security implications of recycling byte buffers across requests in Go?
* **"Did You Actually Build This?"**: If a client sends a malformed frame header claiming a payload of 2GB, what exact error does Lattice return, and does the connection stay open or terminate?

---

# 18. Master Checklist: 30 "Did You Actually Build This?" Exposure Questions

These questions are specifically engineered to distinguish candidates who personally implemented, tested, and debugged a storage engine from those who merely memorized high-level architecture concepts.

1. *Why did you implement a SkipList rather than using Go's built-in `map` for the MemTable?*
2. *What exact formula did you use to calculate the height of a new SkipList node, and what is the probability $p$?*
3. *What happens if an `fdatasync()` call on the WAL fails midway through a Group Commit batch? What do the waiting client goroutines receive?*
4. *How do you know an SSTable data block has ended when parsing a sequential byte stream from disk?*
5. *Where is the restart point array stored inside a 4KB SSTable data block, and how does the reader locate it?*
6. *Why is the SSTable footer fixed at 48 bytes? What are the exact sizes of each field in the footer?*
7. *How does the binary search on the sparse index work if the target key falls between two block index keys?*
8. *What happens if an SSTable has a corrupted Bloom filter block on disk? Does the entire database crash?*
9. *How do you calculate the optimal number of hash functions $k$ for a Bloom filter given $m$ bits and $n$ keys?*
10. *Why do you use Murmur3 double-hashing instead of running 7 independent MD5 or SHA-256 hashes?*
11. *What is a `VersionEdit` record, and how is it serialized to the `MANIFEST` file?*
12. *Why do you need both a `MANIFEST` file and a `CURRENT` file? Why can't you just use `MANIFEST` directly?*
13. *How do you ensure that two concurrent background compactions do not try to merge the exact same SSTable file?*
14. *Under what exact conditions is a tombstone physically purged from disk during Leveled Compaction?*
15. *What happens if you drop a tombstone at Level 1 when Level 2 still contains an older version of that key?*
16. *How does your K-Way merge iterator use a min-heap to deduplicate identical keys with different sequence numbers?*
17. *How do you prevent a slow reader from keeping obsolete SSTables pinned in memory forever?*
18. *Why did you shard the LRU block cache into 16 shards instead of using a single mutex?*
19. *What is false sharing in a sharded cache struct, and how did you prevent it in Go?*
20. *What exact write pacing delays are injected when Level 0 file count reaches 8 and 12 files?*
21. *How does the TCP frame decoder handle a network read that returns only 7 bytes of an 18-byte header?*
22. *How do you prevent a client from sending a 2GB payload length header and causing an out-of-memory crash?*
23. *What happens if a Raft candidate receives a `RequestVote` RPC from another candidate with a higher term?*
24. *How does a Raft follower detect whether the leader's log is at least as up-to-date as its own?*
25. *Why can't a Raft leader commit a log entry from a previous term by counting replicas?*
26. *How does the `ReadIndex` protocol prevent stale reads without writing a new log entry to disk?*
27. *What happens if a node crashes while flushing an SSTable to disk? How does recovery clean up the partial file?*
28. *How do you verify that your database passes Go's race detector (`go test -race`) under concurrent client load?*
29. *What profiling tool did you use to identify CPU bottlenecks in your SkipList or WAL write path?*
30. *If you had to scale this database to support 100 terabytes of data across 50 machines, what architectural component would you need to add?*

---

# 19. Implementation Learning Log

This section is a living record of actual engineering obstacles, debugging sessions, unexpected behaviors, and corrected assumptions encountered during development. It ensures mistakes become long-term technical interview assets.

### Log Entry Template
```markdown
### Entry [YYYY-MM-DD] — [Micro-Phase ID]: [Descriptive Title]
- **Date**: YYYY-MM-DD
- **Micro-Phase**: Pxx-Sxx-Mxx
- **Problem**: <Exact bug, failure, or bottleneck observed>
- **Initial Assumption**: <What I initially thought would work>
- **What Was Actually True**: <Underlying OS, runtime, or hardware reality>
- **How It Was Discovered**: <Specific test, race detector, or profiling tool>
- **Fix Applied**: <Code or algorithmic adjustment made>
- **Core Lesson**: <Generalizable engineering principle learned>
- **Interview Relevance**: <How to weave this story into a behavioral or technical interview>
```

*(Entries will be appended chronologically as implementation progresses)*

### Entry 2026-09-06 — P00-S01-M01: Establishing Hermetic Go Module & Runtime Boundaries
- **Date**: 2026-09-06
- **Micro-Phase**: P00-S01-M01
- **Problem**: Need to establish a clean, hermetic Go build foundation and prevent development artifacts (database files, binaries, crash dumps) from leaking into git history.
- **Initial Assumption**: Can start writing Go code directly without formal module boundary setup.
- **What Was Actually True**: Without an explicit `go.mod`, modern Go toolchains cannot enforce deterministic dependency resolution (MVS). Furthermore, without strict `.gitignore` rules for storage files (`*.sst`, `wal/`, `*.tmp`), database runtime and crash artifacts risk accidental version control pollution.
- **How It Was Discovered**: Inspection of Go module requirements and file I/O lifecycle in LSM-tree databases.
- **Fix Applied**: Initialized `go.mod` (Go 1.22+) with zero external dependencies; configured `.gitignore` with strict filters for binaries, test coverage, pprof dumps, and database runtime files; configured `.editorconfig` for standard Go formatting.
- **Core Lesson**: Systems databases must implement defense-in-depth at the filesystem boundary from day zero, preventing sensitive runtime data or binary files from entering version control.
- **Interview Relevance**: Demonstrates deep software engineering discipline: hermetic reproducible builds, understanding of Go's Minimal Version Selection (MVS), and zero-dependency architecture.

### Entry 2026-09-06 — P00-S01-M02: Canonical Go Project Scaffolding & Compiler Boundaries
- **Date**: 2026-09-06
- **Micro-Phase**: P00-S01-M02
- **Problem**: Establish canonical Go directory layout (`cmd/`, `internal/`, `pkg/`) across 17 distinct packages without introducing premature database implementations, while guaranteeing toolchain buildability and clean Git tracking.
- **Initial Assumption**: Creating package directories with `doc.go` alone is sufficient for `go build ./...`, and adding binary names (`lattice`, `lattice-cli`, `lattice-bench`) or runtime folders (`wal/`, `data/`) directly to `.gitignore` cleanly isolates runtime outputs.
- **What Was Actually True**:
  1. The Go toolchain requires executable packages (`package main` in `cmd/*`) to declare `func main() {}`; omitting it causes `go build ./...` to fail with `function main is undeclared in the main package`.
  2. Git treats unanchored entries in `.gitignore` (e.g. `lattice` or `wal/`) as wildcard matches across the entire repository tree, inadvertently silencing `cmd/lattice/` and `internal/wal/` from version control.
- **How It Was Discovered**: `go build ./...` produced compilation errors on empty `cmd/` packages, and `git status` showed `cmd/lattice` and `internal/wal` missing until diagnosed via `git check-ignore -v`.
- **Fix Applied**:
  1. Added anchored leading slashes (`/lattice`, `/wal/`, `/data/`) in `.gitignore` to restrict exclusion to root binaries and database data directories.
  2. Added minimal empty `func main() {}` in `cmd/*/main.go` alongside `doc.go` to satisfy Go compiler requirements without introducing premature logic.
  3. Created canonical `doc.go` across all 14 `internal/` packages and `pkg/client`.
- **Core Lesson**: Toolchains and build systems have specific compiler invariants (e.g., `package main` requires an entrypoint). In systems programming, directory boundaries must be verified end-to-end with compiler tools (`go build`, `go vet`, `go test`) and Git diagnostics (`git check-ignore`) rather than assumed.
- **Interview Relevance**: Demonstrates real-world mastery of Go compiler rules (`internal/` access control, `package main` requirements), gitignore pattern syntax pitfalls, and architectural discipline in isolating internal storage engines from public APIs.

### Entry 2026-09-06 — P00-S01-M03: Static Analysis Architecture & Linter Signal Optimization
- **Date**: 2026-09-06
- **Micro-Phase**: P00-S01-M03
- **Problem**: Establish a reproducible, zero-external-dependency static analysis foundation enforcing strict error handling, error wrapping, and formatting without suffering from linter fatigue or false alarms.
- **Initial Assumption**: Can use standard v1 `.golangci.yml` syntax with `linters:` and include `gofmt` directly in the linter list.
- **What Was Actually True**:
  1. Modern `golangci-lint` (v2.x) strictly requires `version: "2"` at configuration root; omitting it causes `can't load config: unsupported version of the configuration` (exit code 3).
  2. In v2 schema, formatters like `gofmt` were separated from `linters` into a dedicated `formatters` configuration section (`formatters.enable: [gofmt]`).
  3. Generic struct packing linters (`govet.fieldalignment`) produce false alarms in storage engines where structs are intentionally padded to prevent multicore CPU cache-line false sharing (64-byte spacing) or to align with on-disk binary layouts.
- **How It Was Discovered**: `golangci-lint config verify` validated schema migration; negative testing of unhandled errors (`errcheck`), blanket `//nolint` (`nolintlint`), and bad formatting (`gofmt`) confirmed that each analyzer accurately caught bugs and exited with code 1.
- **Fix Applied**: Configured `.golangci.yml` (v2 schema) with `govet` (excluding `shadow` and `fieldalignment`), `errcheck` (`check-type-assertions: true`), `staticcheck`, `ineffassign`, `unused`, `errorlint`, `nolintlint`, and `gofmt` formatter; verified that `max-issues-per-linter: 0` ensures zero truncated findings.
- **Core Lesson**: Static analysis in systems engineering must be optimized for signal over quantity. Linters must strictly protect durability invariants (forcing disk I/O error checking) while actively suppressing superficial heuristics that undermine low-level hardware concurrency design.
- **Interview Relevance**: Demonstrates production-grade engineering maturity: deep knowledge of static analysis ASTs vs compiler passes, appreciation of cache-line dynamics over naive struct packing, supply-chain hygiene (hermetic dev tooling), and defense-in-depth code quality enforcement.

### Entry 2026-09-06 — P00-S02-M01: Establishing Domain Error Vocabulary & Non-Leaking Diagnostics
- **Date**: 2026-09-06
- **Micro-Phase**: P00-S02-M01
- **Problem**: Define a cohesive, leaf-package domain error architecture in `internal/errors` that distinguishes operational conditions from media corruptions and recovery signals, without leaking sensitive customer payload data into log streams.
- **Initial Assumption**: Can define simple string sentinels or embed raw keys and values directly into error formatting strings for debugging convenience.
- **What Was Actually True**:
  1. Formatting user-supplied keys and values into error strings (e.g. `fmt.Sprintf("key %s exceeds limit", key)`) creates a critical security vulnerability: passwords, authorization bearer tokens, and PII get written directly to plain-text application logs during validation failures.
  2. Callers need both constant sentinel identity (for zero-allocation `errors.Is(err, ErrKeyTooLarge)`) and structured diagnostics (e.g., actual size vs limit). This requires custom error structs (`KeyTooLargeError`, `ValueTooLargeError`, `ChecksumMismatchError`, `TornWriteError`) implementing `Is(target error) bool` returning equality against the sentinel.
  3. Inside `package errors`, importing the standard library `errors` package creates an identifier collision unless aliased (e.g., `import stdErrors "errors"`).
- **How It Was Discovered**: Security threat modeling of logging pipelines and race-detector unit testing of `%w` error unwrap chains in `internal/errors/errors_test.go`.
- **Fix Applied**: Implemented leaf package `internal/errors` with 7 sentinels and 4 contextual typed errors implementing `Is(error) bool`; omitted raw payload byte slices from error types, logging only numeric lengths, limits, and offsets; validated 100% of error unwrap and `errors.As` extraction paths under `go test -race`.
- **Core Lesson**: Error types are part of the system's security boundary and observability contract. By designing errors that separate high-level classification (`errors.Is`) from diagnostic detail (`errors.As`), and strictly censoring raw data at the point of origin, systems engines achieve durability, auditability, and data confidentiality simultaneously.
- **Interview Relevance**: Demonstrates advanced Go error design patterns (`Is(error) bool` matching, `errors.As` type extraction, `%w` unwrap propagation), threat-model-conscious error formatting, and modular leaf-package decoupling.

### Entry 2026-09-06 — P00-S02-M02: Concurrency-Safe Structured Logging & Automated Redaction
- **Date**: 2026-09-06
- **Micro-Phase**: P00-S02-M02
- **Problem**: Establish a lightweight, high-performance internal structured logging foundation in `internal/logger` wrapping Go's standard library `log/slog` that provides thread safety across hundreds of goroutines, scopes logs by subsystem (`WithComponent`), and prevents credential leakage into log files.
- **Initial Assumption**: Can use standard `slog.Default()` or output unstructured text directly via standard `log` package, or pull in an external dependency like `go.uber.org/zap`.
- **What Was Actually True**:
  1. Standard `log.Print` lacks structured attributes, forcing log aggregators to use brittle regexes to extract subsystem names, latencies, and node IDs.
  2. Pulling in third-party libraries (`zap`, `zerolog`) unnecessarily expands the runtime dependency graph and introduces supply-chain attack vectors. Standard library `log/slog` (Go 1.21+) provides high-performance JSON and text handlers out of the box with zero external dependencies.
  3. In a storage engine handling user credentials, API keys, and session tokens, relying on developers to manually redact keys at every call site is error-prone. Hooking into `slog.HandlerOptions.ReplaceAttr` provides automated, case-insensitive key masking (`[REDACTED]`) and supports domain types implementing `Redactable`.
  4. In benchmark suites, logging to `io.Discard` with a standard handler still incurs serialization and allocation overhead. A custom `nopHandler` with `Enabled(...) bool { return false }` short-circuits `slog` record allocations entirely.
- **How It Was Discovered**: Concurrency testing with 100 parallel goroutines writing 5,000 log events verified zero race detector findings (`go test -race`). Unit tests verified JSON field indexing, component scoping, and redaction of mixed-case sensitive keys.
- **Fix Applied**: Built `internal/logger` wrapping `log/slog` with `Logger` interface, JSON/Text formats, automated `ReplaceAttr` redaction for baseline sensitive keys, `Redactable` interface, subsystem scoping via `WithComponent`, `Err(err)` structured attribute helper, and zero-cost `NewNop()`.
- **Core Lesson**: Logging is a critical security boundary and concurrency bottleneck in database engines. Automating sensitive field redaction at the handler level guarantees compliance by design, while wrapping `slog` maintains standard library purity with zero runtime dependencies.
### Entry 2026-09-06 — Phase 00 Final Security Audit: Typed Nil Receiver Traps, Interface Reflection, and Compound Key Redaction
- **Date**: 2026-09-06
- **Phase**: Phase 00 Final Security & Correctness Audit
- **Problem**: Hostile testing of Phase 00 revealed two critical panic vectors (typed nil receivers in domain errors and `Redactable` interface assertion) and a sensitive data leakage vector (compound keys bypassing exact-match redaction).
- **Initial Assumption**: 
  1. Go interface nil checks `r != nil` prevent nil pointer dereferences on methods.
  2. Domain error `Error()` methods will only be called on non-nil pointers.
  3. Exact keyword matching (`"password"`, `"api_key"`) in `ReplaceAttr` is sufficient to redact secrets.
- **What Was Actually True**:
  1. In Go, an interface storing a typed nil pointer `(*MyType)(nil)` is NOT equal to `nil` because its concrete type word is non-nil. When `a.Value.Any().(Redactable)` succeeds on a typed nil pointer, `r != nil` evaluates to `true`, and calling `r.Redact()` immediately triggers a segmentation fault (`invalid memory address or nil pointer dereference`), crashing the database process.
  2. A function returning `error` frequently returns a typed pointer initialized to nil. Calling `err.Error()` on `(*KeyTooLargeError)(nil)` panicked with nil pointer dereference.
  3. Attackers or developers logging compound keys like `"db_password"`, `"client_secret"`, `"auth_token"`, `"session_token"`, `"api-key"`, and `"private-key"` bypassed exact map lookups and leaked raw secrets into plain-text logs.
- **How It Was Discovered**: Adversarial probing and hostile fuzzing during the comprehensive Phase 00 security audit.
- **Fix Applied**: 
  1. Added nil-receiver guards to all typed error `Error()` methods, returning the sentinel error string safely.
  2. Built `safeRedact()` using `reflect.ValueOf(r)` to detect typed nil pointers across pointers, interfaces, maps, slices, and channels, coupled with `recover()` to prevent panics in custom `Redact()` implementations from terminating the server.
  3. Implemented `isSensitiveKey()` with delimiter normalization (canonicalizing `-` to `_`) and compound pattern matching for high-risk stems (`password`, `secret`, `credential`, `private_key`, `api_key`, `*_token`, `token`, `auth`, `authorization`), while avoiding false positives on metadata like `token_count`.
  4. Added permanent regression test suites in `internal/errors/errors_test.go` and `internal/logger/logger_test.go`.
- **Core Lesson**: In Go systems programming, interface nil checks are treacherous due to the `(type, value)` interface representation. Defensive libraries must use reflection or explicit receiver guards, and logging security must never rely on naive exact string matches for credentials.
- **Interview Relevance**: Demonstrates elite mastery of Go internals (interface memory layout, nil interface vs nil pointer trap, runtime panic recovery), adversarial security testing, and robust defense-in-depth API design.

### Entry 2026-09-07 — P01-S01-M01: Fixed-Width Big-Endian Codecs, Bounds Check Elimination (BCE), and Anti-Tear Writes
- **Date**: 2026-09-07
- **Micro-Phase**: P01-S01-M01
- **Problem**: Implement zero-allocation Big-Endian fixed integer codecs for uint16, uint32, and uint64, ensuring determinism, platform independence, and safety against partial ("torn") buffer writes when given undersized slices.
- **Initial Assumption**: Writing sequential byte index assignments (`buf[0] = ...; buf[1] = ...`) is sufficient and standard.
- **What Was Actually True**: In Go, naive sequential writes cause the runtime to perform multiple independent bounds checks. More dangerously, if an undersized buffer is provided (e.g. 3 bytes for `PutUint32`), indices 0, 1, and 2 are mutated *before* the panic occurs on index 3, corrupting caller memory with a torn write.
- **How It Was Discovered**: Negative boundary testing and inspection of Go SSA compiler bounds check elimination (BCE) rules.
- **Fix Applied**: Placed an explicit early bounds check `_ = buf[width-1]` at the entry of each `PutUint*` and `GetUint*` function. This causes the function to panic immediately *before* writing any bytes, eliminating torn writes while allowing the Go compiler to prove that subsequent indices `0..width-1` are in-bounds and eliminate all subsequent branch checks.
- **Core Lesson**: Low-level binary primitives must be tear-resistant by design. Anchoring bounds at the maximum index guarantees an atomic all-or-nothing write semantic without adding error handling overhead to hot scalar codecs.
- **Interview Relevance**: Demonstrates deep understanding of Go compiler optimizations (BCE, SSA pass), hardware byte swapping, zero-allocation benchmarking, and defensive memory design in storage engines.

### Entry 2026-09-07 — P01-S01-M02: 7-Bit Varints, 10th-Byte Overflow Invariants, and DoS-Resistant Bounded Decoding
- **Date**: 2026-09-07
- **Micro-Phase**: P01-S01-M02
- **Problem**: Implement a high-performance, zero-allocation 7-bit unsigned varint codec (`PutVarint64`, `GetVarint64`, `VarintLen`) resilient against malformed streams, integer overflow, truncated buffers, and unbounded scan CPU exhaustion attacks (Varint Bomb DoS).
- **Initial Assumption**: Can loop `for b >= 0x80` shifting 7 bits per byte until a terminating byte is found, and allow standard integer overflow wrapping if a stream is oversized.
- **What Was Actually True**:
  1. An attacker or corrupted disk segment transmitting continuous `0x80` bytes can force an unbounded loop to scan entire memory buffers, burning CPU cycles in an infinite scan loop (Varint Bomb DoS).
  2. 64-bit unsigned integers require at most 10 bytes ($9 \times 7 = 63$ bits, leaving exactly 1 bit in byte 10). If byte 10 contains payload bits 1..6 (`b & 0x7E != 0`) or continuation bit 7 (`b & 0x80 != 0`), it represents values $\ge 2^{64}$ or an illegal 11th byte, both requiring strict `ErrVarintOverflow`.
  3. Truncated inputs must return `(0, 0, ErrVarintTruncated)` rather than partial byte counts, ensuring callers do not advance read pointers on incomplete frames.
  4. In `PutVarint64`, writing bytes sequentially into an undersized slice corrupts caller memory before panicking. Pre-computing `VarintLen(v)` and touching `_ = buf[needed-1]` upfront guarantees an atomic all-or-nothing write.
- **How It Was Discovered**: Boundary analysis of $2^{64}-1$ byte vectors, adversarial testing with $1,000,000$ consecutive `0x80` bytes, and native Go fuzzing (`FuzzGetVarint64`, `FuzzRoundTripVarint64`).
- **Fix Applied**: Implemented bounded loop (`i < MaxVarintLen64`), 10th-byte overflow check (`i == 9 && b > 1`), atomic anti-tear BCE check in `PutVarint64`, zero-on-error return contract `(0, 0, err)`, and extensive tests against independent reference implementations and `encoding/binary.Uvarint`.
- **Core Lesson**: Binary parsers exposed to untrusted disk or network inputs must guarantee strictly bounded execution. Any loop processing variable-length data must be capped at the theoretical maximum length of the underlying data type.
- **Interview Relevance**: Demonstrates deep understanding of LEB128/varint mechanics, bitwise arithmetic limits, DoS vector mitigation in storage engines, and zero-allocation systems engineering.

### Entry 2026-09-07 — P01-S01-M03: CRC32-IEEE Verification, Hardware Acceleration, and Non-Cryptographic Error Boundaries
- **Date**: 2026-09-07
- **Micro-Phase**: P01-S01-M03
- **Problem**: Implement a high-performance, zero-allocation CRC32-IEEE checksum calculator and verifier for WAL records, SSTable blocks, and TCP wire frames, establishing an authoritative error detection boundary.
- **Initial Assumption**: Can use any CRC32 variant (such as Castagnoli) or implement a custom lookup table in code, and return `ErrChecksumMismatch` directly from `Verify()`.
- **What Was Actually True**:
  1. The storage engine architecture explicitly specifies standard IEEE 802.3 polynomial (`0xEDB88320`) across WAL records and network frames; using CRC32C or custom variants breaks on-disk and wire compatibility.
  2. Rolling a custom software table or bitwise algorithm in production is unoptimized. Go's standard library `hash/crc32` leverages architecture-specific assembly instructions (ARM64 PMULL, AMD64 PCLMULQDQ) that deliver hardware carryless multiplication at 11.7–13.8 GB/s with 0 allocations.
  3. `Verify(data []byte, expected uint32) bool` must remain a pure boolean contract. Lower-level binary codecs should never allocate or bind to domain-specific recovery errors like `ErrChecksumMismatch`. It is the responsibility of higher-level parsers (WAL replay, SSTable block decoders) to translate verification failures into specific domain actions (e.g. truncating torn tail writes vs panicking on mid-log corruption).
  4. CRC32 provides mathematical guarantees for accidental media corruption (detecting all odd bit errors, double bit errors, and burst errors $\le 32$ bits), but provides zero cryptographic authentication. Adversarial testing must verify that tests never confuse accidental bit-rot resilience with cryptographic tamper-resistance.
- **How It Was Discovered**: Differential testing against an independent bit-by-bit software simulation oracle and native Go fuzzing with ~3,000,000 randomized iterations.
- **Fix Applied**: Built `internal/binary/crc.go` wrapping `hash/crc32.ChecksumIEEE`, providing clean `Checksum` and `Verify` functions with zero heap allocations; created independent bit-by-bit test oracle in `crc_test.go` to eliminate circular dependency on the standard library.
- **Core Lesson**: Use standard library primitives for hardware-accelerated algorithms, keep primitive verification APIs boolean and allocation-free, and maintain clear boundaries between physical corruption detection and cryptographic security.
- **Interview Relevance**: Demonstrates mastery of hardware carryless multiplication dynamics, polynomial error detection mathematics vs cryptographic security guarantees, zero-allocation API contracts, and independent oracle verification.

### Entry 2026-09-07 — P01-S02-M01: Boundary Invariants, O(1) Admission Validation, and Payload-Free Error Context
- **Date**: 2026-09-07
- **Micro-Phase**: P01-S02-M01
- **Problem**: Implement strict boundary validation for keys ($1 \le \text{KeyLen} \le 65,535$) and values ($0 \le \text{ValLen} \le 4,194,304$), ensuring $O(1)$ constant-time execution without payload copies, zero memory allocations on valid paths, and error context that preserves data confidentiality.
- **Initial Assumption**: Can validate keys and values by inspecting byte contents or character counts, and can embed the invalid key or value directly into the returned error message for caller debugging.
- **What Was Actually True**:
  1. Inspecting contents or runes (`utf8.RuneCount`) is a fatal architectural mistake: database storage constraints are strictly byte-based. A 4-byte UTF-8 rune (e.g. emoji) occupies 4 bytes of disk/index memory. Counting characters allows 65,535 multi-byte runes (262,140 bytes) to overflow the 16-bit binary record length header (`uint16`), causing catastrophic buffer corruption in WAL and SSTable blocks.
  2. Formatting raw keys or values into error messages creates a severe security vulnerability: sensitive secrets (passwords, bearer tokens, API keys) leak directly into application and cloud log streams during validation failures.
  3. Validating slice length requires only reading the slice header length (`len(s)`). It requires 0 payload byte copies, 0 hash computations, and 0 memory allocations (`0 B/op`, `0 allocs/op`), executing in ~0.22 ns/op regardless of payload size (from 16 bytes to 4 MiB).
  4. Nil and zero-length values are completely legal valueless markers in LSM storage (used for tombstone deletions, sets, or boolean flags), whereas nil or zero-length keys must be strictly rejected with `ErrEmptyKey`.
- **How It Was Discovered**: Boundary testing of UTF-8 multi-byte strings, security threat modeling of error logging pipelines, and empirical benchmarking on Apple M4.
- **Fix Applied**: Implemented `ValidateKey` and `ValidateValue` in `internal/binary/validate.go` with single source-of-truth constants; returned typed errors `*KeyTooLargeError` and `*ValueTooLargeError` exposing actual size and max size without payload bytes; validated $max-1, max, max+1$ boundaries, input immutability, and 5.84M fuzz iterations with 0 crashes.
- **Core Lesson**: Database admission validators must enforce raw byte-length limits in $O(1)$ time, treat binary slices as opaque, and strictly redact customer payloads at the error-generation boundary.
- **Interview Relevance**: Demonstrates deep understanding of storage engine memory boundaries, binary framing limits (16-bit uint16 header bounds), $O(1)$ slice header performance, and secure diagnostic error design.

### Entry 2026-09-07 — P01-S02-M02: Strongly Typed Enums, Monotonic 64-Bit Sequences, and Wraparound Immunity
- **Date**: 2026-09-07
- **Micro-Phase**: P01-S02-M02
- **Problem**: Establish deterministic abstractions for operation types (`OpType`) and sequence numbers (`SeqNum`), ensuring zero-value rejection, compile-time type safety, and arithmetic overflow protection.
- **Initial Assumption**: Could use raw Go primitives (`byte` and `uint64`) across the codebase without custom type wrappers, and rely on standard `s++` incrementing for sequence numbers.
- **What Was Actually True**:
  1. Relying on raw primitives invites silent parameter transposition bugs across complex function signatures (e.g., inadvertently passing a sequence number where an offset or length is expected). Declaring `type OpType byte` and `type SeqNum uint64` enforces static compiler validation.
  2. Zero-value (`0x00`) must never represent a valid operation like `PUT`. When uninitialized memory or zeroed disk pages are processed, treating `0x00` as valid would fabricate bogus insertions. Explicitly declaring `0x00 = OpTypeInvalid` ensures fail-closed admission.
  3. In Go, unsigned integer arithmetic silently wraps around on overflow (`math.MaxUint64 + 1 == 0`). If a sequence number allocator were allowed to wrap to 0, newly committed writes would receive sequence numbers smaller than existing historical records, causing newer user data to be shadowed and permanently deleted by older records during compaction. Guarding sequence progression via `Next()` returning `ErrSeqNumOverflow` guarantees wraparound immunity.
- **How It Was Discovered**: Review of LSM compaction deduplication rules and static analysis of Go integer overflow behavior.
- **Fix Applied**: Created `internal/binary/types.go` declaring `OpType` (with `Valid()`, `Validate()`, `String()`, `ParseOpType`) and `SeqNum` (with `Next()`, `String()`); added `ErrInvalidOpType`, `ErrSeqNumOverflow`, and corresponding typed error structs in `internal/errors`; tested with exhaustive 256-byte loop, boundary progression, zero-allocation benchmarks, and >5.50M fuzz iterations.
- **Core Lesson**: Storage primitives must be strictly typed, zero-values must be explicitly invalid for operational enums, and monotonic sequences must provide explicit boundary overflow defense.
- **Interview Relevance**: Demonstrates deep appreciation for compile-time safety in systems software, LSM tombstone lifecycle dynamics, 64-bit sequence exhaustion arithmetic, and defensive wraparound prevention.

### Entry 2026-09-07 — P01-S02-M03: InternalKey Data Model, Newest-First Version Ordering, and Comparator Transitivity
- **Date**: 2026-09-07
- **Micro-Phase**: P01-S02-M03
- **Problem**: Implement the multi-version storage engine key model (`InternalKey`) combining user keys, 64-bit sequence numbers, and operation types, along with a canonical comparator and bidirectional binary codec.
- **Initial Assumption**: Could sort sequence numbers ascending (like user keys), allow callers to pass raw uncopied slices, and use LevelDB's 8-byte packed trailer `(SeqNum << 8) | OpType`.
- **What Was Actually True**:
  1. Sequence numbers must strictly sort DESCENDING for identical user keys. In LSM storage engines (SkipList MemTable, SSTables, compaction merge heaps), sorting sequence numbers descending guarantees that point lookups and iterators encounter the newest revision first, immediately shadowing older versions without searching through historical iterations.
  2. LevelDB and RocksDB pack sequence numbers into 7 bytes (56 bits), which restricts sequence numbers to $2^{56}-1$. Lattice explicitly specifies full 64-bit sequence numbers (`math.MaxUint64`), requiring an 8-byte Big-Endian sequence field and a 1-byte OpType field for a fixed 9-byte trailer (`InternalKeyTrailerLen = 9`).
  3. Slices in Go share underlying backing arrays. If `NewInternalKey` or `DecodeInternalKey` borrows the caller's slice, mutating the caller's buffer silently corrupts internal indexes. Enforcing owned defensive copies in constructors while permitting zero-allocation borrowed usage for direct struct literals achieves both memory safety and high-performance comparator evaluation.
  4. The comparator must satisfy strict mathematical order axioms (reflexivity, antisymmetry, and transitivity). Violating transitivity corrupts binary searches in SSTable blocks and breaks min-heap priority queues during K-way compaction.
- **How It Was Discovered**: Mathematical proof of comparator order properties, LevelDB trailer bit-width analysis against 64-bit sequence bounds, and empirical benchmark testing on Apple M4.
- **Fix Applied**: Built `internal/binary/internalkey.go` implementing `InternalKey`, `NewInternalKey` (with defensive copy), `Clone()`, `String()`, `Equal()`, `CompareInternalKey`, `AppendInternalKey`, `EncodeInternalKey`, and `DecodeInternalKey`; added `ErrInternalKeyTruncated` in `internal/errors`; verified mathematical laws over 5,000 randomized triples, sort integration, and >5.82M fuzz iterations.
- **Core Lesson**: In multi-version storage engines, key ordering must prioritize the newest sequence number first, trailer serialization must match architectural integer widths, and defensive copying must protect long-lived indexes from caller slice mutations.
- **Interview Relevance**: Demonstrates deep understanding of LSM version resolution, newest-first iterator dynamics, mathematical comparator transitivity, 64-bit vs 56-bit sequence number engineering trade-offs, and zero-allocation binary comparison.

### Entry 2026-09-07 — Phase 00 Remediation: Strict Sensitive-Key Precedence Over Redactable Value Handlers
- **Date**: 2026-09-07
- **Phase**: Phase 00 Final Security Remediation
- **Problem**: In `internal/logger/logger.go`, `makeReplaceAttr` evaluated whether an attribute value implemented the `Redactable` interface *before* checking whether the attribute key was sensitive. This created an API/security-contract ambiguity: a buggy or hostile `Redact()` method could return an unscrubbed secret when logged under a sensitive key (`password`, `accessToken`), bypassing key-based redaction.
- **Initial Assumption**: Since `Redact()` is intended to return a safe representation, evaluating it first was assumed to be safe.
- **What Was Actually True**: In defense-in-depth security architectures, known sensitive boundaries must be fail-closed: **Sensitive Attribute Key Always Wins**. If an attribute key is classified as sensitive, the logger must immediately replace the value with `[REDACTED]` without executing custom value methods. Executing user-provided `Redact()` logic on sensitive keys creates a bypass vector and wastes CPU cycles.
- **How It Was Discovered**: Pre-Phase 02 regression audit and hostile reproducer test (`TestSensitiveKeyPrecedenceOverHostileRedactable`).
- **Fix Applied**: 
  1. Reordered `makeReplaceAttr`: empty check $\to$ sensitive key check ($\to$ immediate `[REDACTED]`) $\to$ `Redactable` check on non-sensitive keys ($\to$ `safeRedact`) $\to$ unchanged attribute.
  2. Added comprehensive regression suite proving: sensitive keys never invoke `Redact()`; non-sensitive keys invoke `Redact()` and preserve safe fields; typed-nil pointers under sensitive keys output `[REDACTED]` without panic; panicking `Redactable` implementations under sensitive keys never trigger panics; benign keys (`author`, `token_count`) avoid false-positive masking; concurrent logging under 50 goroutines records zero sensitive invocations.
  3. Benchmarked sensitive paths: short-circuiting `Redact()` evaluation for sensitive keys reduced latency from 1,294 ns/op to 787 ns/op (39.2% improvement) and heap allocations from 569 B/op (17 allocs) to 136 B/op (9 allocs).
- **Core Lesson**: Security policies must prioritize unambiguous, structural invariants (e.g. key sensitivity classification) over user-provided callback transformations. Fail-closed boundaries prevent downstream implementation flaws from escalating into secret leakage.
- **Interview Relevance**: Demonstrates systems-grade defense-in-depth thinking, fail-closed security boundary design, empirical benchmark auditing, and concurrency-safe regression verification.

### Entry 2026-09-08 — Phase 02: 21-Byte Physical WAL Header Framing & Zero-Tear Bounds Check Elimination
- **Date**: 2026-09-08
- **Phase**: Phase 02 Sub-Phase 02.1 Micro-Phase 01 (`P02-S01-M01`)
- **Problem**: In a crash-resilient write-ahead log, writing variable-length records directly to disk requires an infallible physical framing header. If a memory buffer passed to the header encoder is smaller than the required header width (21 bytes), naive sequential writes (`buf[0] = ...; buf[1] = ...; buf[15] = ...`) will mutate the beginning of the buffer before hitting a panic on subsequent indices, leaving a partially mutated, torn buffer in memory. Furthermore, on-disk corruption or reading unwritten preallocated blocks must not be confused with valid operations.
- **Initial Assumption**: Deserializing headers with dynamic allocation or slice checking at each field write is sufficient.
- **What Was Actually True**: 
  1. **Torn Writes in RAM**: Partial buffer mutation before a panic corrupts memory state. By inserting an early bounds check elimination dummy read (`_ = buf[HeaderSize-1]` i.e. `_ = buf[20]`), Go's compiler emits a single bounds check at entry. If the slice has fewer than 21 bytes, it panics immediately without modifying any byte, guaranteeing atomic fail-fast behavior.
  2. **Uninitialized Memory Poisoning**: Storage engines often pre-allocate WAL files using fallocate or write zero-filled pages. If `RecordType(0x00)` is mapped to an operation (or treated as default `PUT`), an unwritten or zeroed page can be erroneously replayed during recovery. Mapping `0x00` strictly to `RecordTypeInvalid` guarantees that zeroed memory immediately halts recovery with an explicit diagnostic (`*errors.InvalidRecordTypeError`).
  3. **Zero Allocation Contract**: Encoding, decoding, and appending the 21-byte header must never trigger GC allocations. Deserializing via fixed Big-Endian integer getters and returning by value guarantees $0\text{ B/op}$ and $0\text{ allocs/op}$.
- **How It Was Verified**:
  1. Anti-tear tests (`TestEncodeHeaderEarlyBoundsCheckAntiTear`) verifying buffers of lengths $0..20$ panic with zero byte mutations.
  2. Fuzzing (`FuzzRecordHeaderCodec`) executing >3.2M iterations without panics or discrepancies.
  3. Benchmarks on Apple M4: `EncodeHeader` at $0.99\text{ ns/op}$, `DecodeHeader` at $1.48\text{ ns/op}$, `AppendHeader` at $1.36\text{ ns/op}$, all with $0\text{ allocs/op}$.
- **Core Lesson**: In binary serialization, correctness begins at the framing boundary. Defensive zero-tear invariants in userland memory mirrors crash durability on disk: an operation either commits completely or fails without side-effects.
- **Interview Relevance**: Demonstrates deep understanding of Go compiler bounds check elimination (BCE), binary memory safety invariants, zero-allocation serialization, and disk preallocation failure modes.

### Entry 2026-09-08 — Phase 02: Full WAL Record Serialization, Hostile Reader Anti-DoS & Streaming CRC Verification
- **Date**: 2026-09-08
- **Phase**: Phase 02 Sub-Phase 02.1 Micro-Phase 02 (`P02-S01-M02`)
- **Problem**: Deserializing variable-length binary records from an `io.Reader` introduces serious security and correctness hazards. If a corrupted or malicious WAL stream presents a 32-bit length field like `0xFFFFFFFF` (4 GiB), a naive decoder calling `make([]byte, valLen)` immediately triggers an out-of-memory crash or severe allocation latency, causing denial-of-service. Additionally, network and disk readers can fragment writes (returning single bytes or short reads), and reusing scratch buffers across decode iterations risks memory aliasing where subsequent reads overwrite data currently referenced by active MemTables.
- **Initial Assumption**: Reading records by allocating buffers based on decoded length headers and verifying CRC on contiguous buffers after all bytes have arrived.
- **What Was Actually True**:
  1. **Anti-DoS Early Allocation Bounding**: Decoders must validate key length against `binary.MaxKeyLen = 65,535` and value length against `binary.MaxValueLen = 4,194,304` (4 MiB) *prior* to executing any memory allocation. Malicious lengths fail fast with structured errors (`*errors.ValueTooLargeError`), completely neutralizing allocation-based resource exhaustion attacks.
  2. **Streaming Zero-Allocation CRC32-IEEE**: Rather than allocating a contiguous slice for the entire record to calculate CRC, `DecodeRecord` streams bytes into discrete field buffers and updates CRC incrementally using `crc32.Update(crc, crc32.IEEETable, chunk)`. This calculates the exact hardware-accelerated CRC across all post-CRC bytes (`RecordType || SeqNum || Timestamp || KeyLength || Key || ValueLength || Value`) with zero heap allocations for the checksum calculation.
  3. **Stream-Safe Read Loop**: Standard Go `io.Reader` contracts permit returning fewer bytes than requested without error, or returning data alongside `io.EOF`. Using `io.ReadFull` across all header and payload stages guarantees that partial chunks, single-byte readers, and fragmented streams are assembled deterministically. A clean `io.EOF` is emitted if and only if the stream ends cleanly at byte offset 0 of a record header.
  4. **Strict Memory Ownership**: Decoded `Key` and `Value` slices are newly allocated and strictly owned by the returned `Record`. They never alias internal decode scratchpads, guaranteeing memory safety when passed to concurrent MemTable writers. Conversely, `EncodeRecord` uses `copy()` and never mutates caller-owned memory.
- **How It Was Verified**:
  1. Exhaustive test suite covering requirements A through AB: minimal records, max key (65KB), max value (4MB), empty values, all 4 record types, min/max SeqNum/Timestamp, exact CRC manual byte vectors, single-byte bit flips across all 7 fields, truncations, invalid types, and malicious 4 GiB length fields.
  2. Stream safety tests with 1-byte readers, arbitrary chunk readers (1, 2, 3, 5, 7, 11, 17, 31 bytes), short readers with nil error, and simultaneous data + EOF readers.
  3. Native Go fuzzing (`FuzzRecordCodec`) executing >2.92M iterations with 0 crashes, 0 panics, and 100% round-trip structural stability.
  4. Benchmarks on Apple M4: `AppendRecord` with reused buffer at $20.10\text{ ns/op}$ ($0\text{ allocs/op}$), `EncodeRecord` at $43.04\text{ ns/op}$ ($1\text{ alloc/op}$), and `DecodeRecord` at $91.54\text{ ns/op}$.
- **Core Lesson**: In storage engine codecs, untrusted streams must be bounded at every step. Check limits before allocation, use streaming checksums to avoid temporary buffers, and enforce clear memory ownership to prevent aliasing bugs.
- **Interview Relevance**: Demonstrates deep systems reasoning on streaming I/O contracts, denial-of-service memory defense, hardware-accelerated CRC updating, memory ownership semantics, and native Go fuzz testing.

### Entry 2026-09-08 — Phase 02: WAL Corruption, Exhaustive Single-Bit Verification & Structural vs Checksum Boundaries
- **Date**: 2026-09-08
- **Phase**: Phase 02 Sub-Phase 02.1 Micro-Phase 03 (`P02-S01-M03`)
- **Problem**: Validating WAL durability requires proving that corruptions across any field of a physical record are deterministically intercepted. However, a naive test might expect every single byte mutation to fail with `ErrChecksumMismatch`. In reality, corrupting a record type byte to `0x00` (invalid) or corrupting `ValueLength` to `4 GiB` triggers structural format errors *before* the checksum calculation is ever reached. Failing to distinguish between early structural rejection (anti-DoS perimeter) and checksum rejection (data corruption detection) obscures the security model. Furthermore, asserting that "CRC32 guarantees corruption protection" conflates error detection with cryptographic tamper resistance.
- **Initial Assumption**: Every bit flip across the record should fail with `ErrChecksumMismatch`, and CRC32 provides comprehensive integrity guarantees.
- **What Was Actually True**:
  1. **Structural Rejection vs Checksum Rejection**: If a corruption produces an invalid record type (`*errors.InvalidRecordTypeError`), an oversized length (`*errors.ValueTooLargeError`), an empty key on PUT, or a non-empty value on DELETE (`*errors.InvalidRecordPayloadError`), the decoder must reject it structurally before allocating memory or reading payload bytes. This is an intentional anti-DoS design: structural validation protects the machine from resource exhaustion, while CRC32 protects validly framed data against silent corruption.
  2. **CRC Field Exclusion Invariant**: The 4-byte CRC field at offsets 0..3 is strictly excluded from the checksum computation input. Mutating any bit in bytes 0..3 alters the stored CRC while leaving the payload's computed CRC unchanged, guaranteeing an unambiguous `*errors.ChecksumMismatchError` with expected vs actual diagnostics.
  3. **Exhaustive Deterministic Single-Bit Testing**: Testing every bit position ($8 \times \text{post-CRC bytes}$) proves that 100% of single-bit flips are intercepted. Restoring each mutated bit confirms that decoding cleanly recovers the original record without side effects.
  4. **Non-Cryptographic CRC Caveat**: CRC32-IEEE operates in a 32-bit state space ($2^{32} \approx 4.29 \times 10^9$) with an undetected collision probability of $\approx 2.33 \times 10^{-10}$ on random noise. However, an attacker with write access can deliberately forge matching CRC32 values for tampered data in microseconds. True tamper resistance requires cryptographic primitives (HMAC or digital signatures).
- **How It Was Verified**:
  1. `TestCRCFieldCorruption`: Tested 32 bit flips across offsets 0..3; all 32 returned `ErrChecksumMismatch` with exact expected/actual values.
  2. `TestCRCScopeCoverage`: Independently computed CRC across `RecordType || SeqNum || Timestamp || KeyLen || Key || ValLen || Val` using `binary.Checksum`; confirmed match with stored CRC; proved mutating any covered field changes the checksum; proved mutating offsets 0..3 does not change the payload checksum.
  3. `TestExhaustiveSingleBitCorruption`: Tested 472 single-bit flips across 59 payload bytes; intercepted 419 checksum mismatches and 53 structural errors with 0 silent accepts; 100% cleanly restored.
  4. `TestStructuralVsChecksumRejectionMatrix`: Verified explicit table mapping of structural vs checksum error paths.
  5. `TestCorruptionSemantics_ReversibilityAndCollisions`: Proved mutation detection, clean decoding, restoration recovery, absence of accidental collisions across 50 random mutations, and strict determinism across 100 consecutive runs.
  6. `TestDeterministicRandomizedCorruption`: 1,000 seeded randomized records across all 4 types (PUT, DELETE, BATCH_START, BATCH_COMMIT); 100% intercepted.
- **Core Lesson**: Error detection mechanisms must be layered: structural validation defends process resources, while checksums defend data integrity. Systems engineers must know exactly what each layer guarantees and never mistake cyclic redundancy codes for cryptographic authentication.
- **Interview Relevance**: Demonstrates mastery of error-detection theory, defense-in-depth security boundaries, deterministic exhaustive verification, and the precise mathematical boundaries of CRC32.

### Entry 2026-09-08 — Phase 02: Secure WAL Directory Initialization, 0700 Permissions, Inode Pinning & TOCTOU-Free Symlink Defense
- **Date**: 2026-09-08
- **Phase**: Phase 02 Sub-Phase 02.2 Micro-Phase 01 (`P02-S02-M01`)
- **Problem**: In multi-tenant systems, WAL files contain plaintext data that must not be readable or traversable by other local users. Initializing the WAL directory with naive check-then-act sequences (`os.Stat` followed by `os.Mkdir`) creates dangerous Time-of-Check to Time-of-Use (TOCTOU) race conditions under concurrent execution. Furthermore, if `<db_path>/wal` is pre-created by an attacker as a symbolic link pointing to a sensitive system directory, standard directory creation calls might write into unintended locations. In the existing-directory permission-hardening path, naive `os.Chmod(walPath, 0700)` relies on a second pathname lookup that is vulnerable to symlink substitution between `Lstat` and `Chmod`. Finally, directory initialization must be completely idempotent and non-destructive to existing WAL files.
- **Initial Assumption**: Using `os.MkdirAll` or `os.Stat` then `os.Mkdir` is sufficient for directory setup, and pathname-based `os.Chmod` is safe after `os.Lstat`.
- **What Was Actually True**:
  1. **Atomic Creation Over TOCTOU**: Executing `os.Mkdir(walPath, DirMode)` directly lets the OS kernel handle atomicity. If it succeeds, the directory was created without race, eliminating create/check races. If it returns `os.ErrExist`, the path already exists and Lattice authoritatively inspects the entry.
  2. **Symlink Rejection via `os.Lstat`**: Standard `os.Stat` follows symbolic links, obscuring whether the target directory was hijacked via symlink. Using `os.Lstat` inspects the link itself. Detecting `info.Mode() & os.ModeSymlink != 0` allows immediately aborting with `*errors.NotADirectoryError`, preventing symlink traversal or redirection attacks.
  3. **Descriptor-Based Permission Hardening & Inode Pinning**: Target permission mode is `0700` (`rwx------`). Because group and other bits are explicitly 0, process umask cannot accidentally grant permissions to group or other users (`0700 &^ umask` preserves zeroed group/other bits). If an existing directory was created with loose permissions (e.g. `0755` or `0777`), `InitDir` avoids pathname-based `os.Chmod`. Instead, it opens the directory descriptor (`os.Open`), verifies `finfo.IsDir()` and `os.SameFile(info, finfo)` to pin the inode, executes `f.Chmod(DirMode)` (`fchmod` directly on the open descriptor, never following symlinks), and validates via post-`Lstat` `os.SameFile` comparison.
  4. **Non-Destructive Idempotency & Inode Preservation**: `InitDir` never deletes, truncates, recreates, or overwrites existing files or directories. Pre-existing WAL segments (`wal_*.log`) and database files (`MANIFEST`, `CURRENT`) remain completely untouched across repeated or concurrent initializations, and the directory inode is preserved.
  5. **Mutex-Free Concurrency**: 50 goroutines racing to initialize the same WAL directory converge safely without requiring application-level mutexes.
- **How It Was Verified**:
  1. `TestInitDir_FreshDatabasePath`: Verified creation and 0700 mode on fresh database root.
  2. `TestInitDir_Idempotency`: Verified repeated calls return the same path with zero side effects.
  3. `TestInitDir_PermissionHardening`: Verified that 0777 pre-existing directories are tightened to 0700.
  4. `TestInitDir_ExistingDirectoryInodePreservation`: Proved pre-existing loose and 0700 directory inodes are strictly preserved via `os.SameFile` without deletion or recreation.
  5. `TestInitDir_ExistingRegularFileAtWALPath`: Verified that existing regular files fail with `ErrNotADirectory` and are preserved without modification.
  6. `TestInitDir_ExistingSymlinks`: Tested directory symlinks, file symlinks, and broken symlinks; all 3 rejected with `ErrNotADirectory`.
  7. `TestInitDir_DbPathAsSymlink`: Proved that if `dbPath` itself is a symlink to an external volume, `InitDir` creates the real WAL directory inside the symlink target cleanly.
  8. `TestInitDir_ConcurrentInitialization`: Tested 50 concurrent goroutines released via barrier; 100% converged on the valid 0700 directory with 0 errors.
  9. `TestInitDir_FilesystemFailureModes`: Verified error propagation on empty path (`os.ErrInvalid`), missing parent (`fs.ErrNotExist`), file parent, and read-only parent (`fs.ErrPermission`).
  10. `TestInitDir_PreservesExistingFiles`: Proved pre-existing WAL logs and database files are 100% byte-for-byte preserved.
- **Core Lesson**: Filesystem security begins at directory creation. Eliminate create/check races by relying on atomic kernel syscalls, inspect existing entries with `Lstat` to defeat symlink hijacking, pin inodes and use descriptor-based `fchmod` to eliminate symlink-substitution TOCTOU during permission hardening, enforce 0700 permissions to protect data confidentiality, and guarantee that re-initialization is strictly non-destructive.
- **Interview Relevance**: Demonstrates mastery of POSIX file permissions, umask dynamics, descriptor-based filesystem operations (`fchmod`), inode pinning (`os.SameFile`), TOCTOU race mitigation, symlink security threats, and non-destructive idempotent systems design.

### Entry 2026-09-08 — Phase 02: Strict Sync WAL Appender, fdatasync Durability Barrier & Non-Interleaving Concurrency
- **Date**: 2026-09-08
- **Phase**: Phase 02 Sub-Phase 02.2 Micro-Phase 02 (`P02-S02-M02`)
- **Problem**: When user writes are accepted into an LSM storage engine, persisting them to disk requires physical non-volatile durability before returning success. Naively assuming `file.Write()` provides durability is a fatal flaw: `file.Write()` only copies bytes into the operating system dirty page cache (RAM). A sudden power outage or kernel panic immediately vaporizes all dirty pages, violating ACID durability. Furthermore, under high goroutine concurrency, multiple writers appending to the same file descriptor risk interleaving record fragments or corrupting record frames if not strictly synchronized.
- **Initial Assumption**: `file.Write()` followed by `file.Sync()` on every append is sufficient, or that Go provides portable `fdatasync`.
- **What Was Actually True**:
  1. **Strict Sync Invariant**: `AppendSync(rec)` must return `nil` if and only if both the complete byte transfer and the hardware synchronization barrier (`fdatasync`) succeed. If `fdatasync` fails, `AppendSync` returns an error and never reports success.
  2. **`fdatasync()` vs `fsync()` Semantics**: `fdatasync(2)` flushes only modified data blocks, omitting unchanged inode metadata updates (access/modification timestamps) and saving physical disk head seeks / NVMe write operations. On Linux, this is invoked directly via `syscall.Fdatasync(int(f.Fd()))`. On Darwin (macOS) and Windows where `fdatasync(2)` is not implemented by the kernel, the engine falls back to `f.Sync()`.
  3. **Torn Tails Belong to Recovery, Not AppendSync**: If an append encounters a short write or write failure mid-record, `AppendSync` does not attempt to roll back or truncate the file. In-flight truncation during disk distress risks cascading corruption. Instead, cleanup of partial/torn writes at EOF is strictly delegated to startup recovery (`P02-S03-M01`).
  4. **Per-Writer Mutex Serialization**: An internal `sync.Mutex` on `WALWriter` serializes concurrent `AppendSync` invocations. Goroutine records are appended as contiguous, non-interleaved atomic frames.
  5. **Non-Destructive Reopening**: Segment files are opened with `os.O_WRONLY | os.O_CREATE | os.O_APPEND` with `0600` permissions. Process restarts never truncate or overwrite existing WAL segments.
- **How It Was Verified**:
  1. `TestWriter_FreshWALAppend`: Verified single PUT append, exact file length, and bit-for-bit replay via `DecodeRecord`.
  2. `TestWriter_DeleteRecord` & `TestWriter_BatchMarkers`: Verified DELETE tombstones and batch markers.
  3. `TestWriter_MultipleRecordsSequential`: Verified concatenated binary layout `encoded(A) || encoded(B) || encoded(C)`.
  4. `TestWriter_ReopenExistingFilePreservesRecords`: Proved reopening existing segment preserves earlier records intact without truncation.
  5. `TestWriter_InvalidRecordRejected`: Proved invalid records are rejected before touching file buffers.
  6. `TestWriter_ShortWriteHandled`: Proved short writes loop until completion, and persistent zero-byte writes return `io.ErrShortWrite`.
  7. `TestWriter_WriteFailureReturned`: Injected write failure via seam; verified error returned.
  8. `TestWriter_SyncFailureReturned`: Injected `fdatasync` failure after write; verified `AppendSync` returns error and never reports success.
  9. `TestWriter_ClosedWriter`: Verified operations on closed writer return `errors.ErrWriterClosed`.
  10. `TestWriter_ConcurrentAppenders`: 50 concurrent goroutines appended records; verified all 50 decoded cleanly without data races, corruption, or interleaving.
  11. `TestWriter_FilePermissions`: Verified `0600` permissions on Darwin/POSIX.
  12. `TestWriter_DescriptorLifecycle`: 100 sequential open/append/close cycles without descriptor leaks.
  13. `TestWriter_InputImmutability`: Proved caller's key and value memory are never modified.
  14. `TestWriter_OneThousandRecordsReplay`: 1,000 records sequentially appended and verified.
- **Core Lesson**: True database durability begins at the kernel barrier. Distinguish between OS page cache acceptance, filesystem data synchronization, and physical storage media durability. Never claim success on a write whose durability barrier failed, and never truncate files mid-flight during append failures.
- **Interview Relevance**: Demonstrates mastery of database durability tiers, POSIX system calls (`write`, `fsync`, `fdatasync`), Linux vs Darwin kernel barrier behaviors, concurrency serialization without global mutexes, and the precise division of responsibility between the append path and recovery subsystem.

---

# 20. Questions I Personally Failed & Corrected Understandings

When studying or answering mock interview questions during the development of Lattice, any question that exposes an incomplete or incorrect understanding is logged here alongside the rigorous, verified correction.

### Failure Entry Template
```markdown
### Question: <Exact Interview Question>
- **Subsystem**: <e.g., WAL / SkipList / Raft>
- **My Initial (Flawed) Answer**: <What I originally thought or stated>
- **Why It Was Incomplete or Wrong**: <Technical flaw in the reasoning>
- **The Correct, Authoritative Explanation**: <Deep, precise systems answer>
- **Key Invariant or Concept to Remember**: <Core takeaway to avoid future slips>
```

*(Entries will be appended whenever knowledge gaps are discovered)*

---

*End of Technical Interview Knowledge Base — Lattice v1.0.0-KNOWLEDGE-BASE*


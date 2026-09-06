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
* **Automated Log Privacy & Sensitive-Data Redaction**: Storage engines are the custodian of customer data. If user keys, authentication tokens, API credentials, or database values are logged during debug sessions, cluster logs become high-severity data leakage vectors. Lattice implements automated attribute replacement via `slog.HandlerOptions.ReplaceAttr`: any attribute whose key matches sensitive keywords (`password`, `secret`, `token`, `auth`, `api_key`, `credentials`, etc.) is automatically replaced with `"[REDACTED]"`. Furthermore, custom domain types can implement the `Redactable` interface (`Redact() any`) to safely scrub sensitive fields before serialization.
* **Logging Failure Handling & Preventing Recursive Loops**: A fundamental invariant in systems software is that logging must never cause a recursive failure loop. If a disk becomes full or an output pipe breaks, attempting to log the logger's write failure can trigger an infinite logging recursion that crashes the process. The logger must fail gracefully (e.g. dropping records or reporting through independent telemetry) without escalating into unhandled panics.
* **Why Logging Must Not Become a Hidden Global Bottleneck**: In LSM storage engines processing 100,000+ ops/sec, synchronous file logging in the critical write path (e.g. inside WAL append or SkipList insertion) causes catastrophic tail latency spikes because disk writes and JSON formatting steal CPU and block worker threads. Storage engines must log only lifecycle events at `INFO` in the foreground, leaving high-frequency operation tracing to sampling metrics or asynchronous telemetry buffers.
* **Endianness**: Big-Endian (Network Byte Order) stores the most significant byte at the lowest memory address; Little-Endian stores the least significant byte first. Network protocols and on-disk files must specify a canonical byte order to remain portable across CPU architectures.
* **Variable-Length Integers (Varints)**: 7-bit varints encode small integers in fewer bytes by using the high bit ($0\text{x}80$) as a continuation flag. An integer $\le 127$ uses 1 byte, while $2^{64}-1$ uses 10 bytes.
* **Memory Allocation & GC Pressure**: Creating millions of small heap allocations triggers frequent Go Garbage Collector mark-and-sweep cycles, stealing CPU cycles and introducing millisecond-level tail latency spikes. `sync.Pool` provides zero-allocation buffer reuse.

### Decisions Made & Trade-offs
* **Decision**: Zero external dependencies for the entire storage engine and network layer; standard library only.
* **Alternative Considered**: Utilizing third-party serialization, networking, or logging libraries (e.g. gRPC, zap, zerolog, protobuf).
* **Trade-off**: Requires writing binary framing, SkipList, LRU cache, and wrapping `log/slog` from scratch, but eliminates supply-chain vulnerabilities, avoids transitive dependency conflicts, and ensures complete interview defensibility.
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
* **Varint Bomb Vulnerability**: A malicious stream containing endless continuation bytes ($0\text{x}80$) could cause an infinite loop or integer overflow. Lattice limits varint decoding to a maximum of 10 bytes; anything beyond returns `ErrVarintOverflow`.
* **Subsystem Isolation as Defense-in-Depth**: By keeping all storage engine implementation details inside `internal/`, we prevent external code or rogue consumer modules from accessing internal unexported memory pools, raw block caches, or un-synchronized file descriptors.
* **Audit-Proof Suppressions via `nolintlint`**: Unchecked use of `//nolint` can allow developers to bypass critical security and durability checks. By enforcing `require-specific: true` and `require-explanation: true`, any lint suppression must name the specific analyzer and justify the override in code review.
* **Log Redaction at Error Generation & Logger Level**: Error types and logging layers represent primary vectors for accidental credential and secret leakage. By designing error types that capture only metadata and providing automated logger-level key redaction (`[REDACTED]`), Lattice enforces a two-tier defense against credentials or sensitive values entering persistent log streams.

### Performance & Hardware Dynamics
* Standard library `binary.BigEndian.PutUint64` is inlined by the Go compiler into single register swap instructions (`BSWAP` on x86), incurring zero runtime function call overhead.
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

### Subsystem Interview Questions
* **Basic**: Why does a database need a Write-Ahead Log?
* **Intermediate**: What is the difference between `fsync()` and `fdatasync()`, and why does pre-allocating WAL files matter?
* **Deep**: Walk me through the exact concurrency flow of your Group Commit implementation. What happens if the leader goroutine panics while holding the batch?
* **Follow-up**: How do you prevent group commit queues from consuming unbounded RAM if the disk becomes completely saturated?
* **"Did You Actually Build This?"**: How do you distinguish between an uncompleted torn write at the tail of the WAL versus a corrupted record in the middle of the file during startup recovery?

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


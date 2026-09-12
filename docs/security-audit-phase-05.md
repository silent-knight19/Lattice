# SEC-P05 Comprehensive Security Audit Report

## Security Gate Before Phase 06: Dependency, Supply-Chain, Application, Storage, Concurrency, Persistence & Infrastructure Audit

* **Repository**: `https://github.com/silent-knight19/Lattice`
* **Branch**: `main`
* **Audit Baseline HEAD**: `25eef7005f4ee3f5a9c38a98834c7d654f1e39b9`
* **Baseline Commit Subject**: `feat(filter): [P05-S02-M02] verify empirical false-positive rate`
* **Audit Date**: September 12, 2026
* **Compiler & Platform**: `go version go1.27.1 darwin/arm64` (target toolchain `go 1.22.0`)
* **Audit Mode**: Strict Audit-Only (zero production code, dependency, or configuration modifications)
* **Overall Verdict**: **YELLOW — PROCEED ONLY AFTER REQUIRED REMEDIATIONS (PASS WITH REQUIRED REMEDIATIONS)**

---

## 1. Executive Summary

A comprehensive, adversarial security audit of the Lattice storage engine repository was performed covering all code implemented through completed Phase 05. The audit evaluated dependency supply-chain security, static security rules, parser and binary codecs, filesystem operations, crash recovery and durability semantics, concurrency invariants under the Go race detector, memory bounds, and adversarial threat models.

Lattice maintains an exceptionally strong security posture established in Phases 00 through 04: zero external runtime dependencies, hermetic pure-Go builds, atomic hardlink publication (`os.Link`) without rename overwrites, strict owner-only file permissions (`0600`/`0700`), defensive inode pinning (`os.SameFile`), and fail-closed crash recovery.

However, detailed manual code analysis of newly implemented Phase 05 components (`internal/filter` and `internal/sstable` filter block integration) identified two code-level security issues:
1. **SEC-P05-01 (High)**: `TableReader.ReadFilterBlock` performs an unconstrained memory allocation (`make([]byte, int(filterHandle.Size))`) directly from disk-derived metadata without enforcing the architectural upper bound (`MaxBitsetBytes + FilterBlockTrailerSize = 256 MiB + 13 B`) or platform integer limits before allocation, introducing a denial-of-service memory exhaustion and 32-bit slice bounds panic vector.
2. **SEC-P05-02 (Medium)**: `TableWriter.Add` silently discards the error returned by `w.filterBuilder.AddKey(key.UserKey)` (`_ = ...`). If the filter builder has been sealed, has encountered an allocation error, or has a nil receiver, keys added to the SSTable data blocks are omitted from the Bloom filter block. This produces a Bloom filter false negative (`MayContain` returning `false` for present keys), causing point lookups in the LSM engine to skip the table and return silent key omission / read data loss.

### Findings Summary

| Severity | Count | Category | Status |
| :--- | :---: | :--- | :--- |
| **Critical** | 0 | — | Clean |
| **High** | 1 | Code Vulnerability (Resource Exhaustion / DoS) | Requires Remediation (P1) |
| **Medium** | 1 | Code Vulnerability (Silent Corruption / Data Omission) | Requires Remediation (P1) |
| **Low** | 1 | Hardening / Defense-in-Depth | Remediation Backlog (P2) |
| **Informational** | 3 | Supply-Chain & Operational Hardening | Remediation Backlog (P3) |
| **Total Findings** | **6** | **2 Code Vulnerabilities, 1 Defense-in-Depth, 3 Informational** | **Actionable** |

### Overall Verdict

> **YELLOW — PROCEED ONLY AFTER REQUIRED REMEDIATIONS**
> 
> The codebase is fundamentally sound and exhibits zero data races, zero external CVEs, and robust persistence guarantees. However, Phase 06 implementation (Manifest, VersionSet, Compaction) must not proceed until SEC-P05-01 (unbounded filter allocation) and SEC-P05-02 (silent filter key omission) are remediated to prevent propagating filter block vulnerabilities into the multi-level SSTable subsystem.

---

## 2. Audit Scope & Methodology

### Repository Baseline Verification
The authoritative repository state was captured prior to auditing:
```text
Commit Hash: 25eef7005f4ee3f5a9c38a98834c7d654f1e39b9
Branch: main
Working Tree: Clean (0 uncommitted changes)
Go Toolchain: go1.27.1 darwin/arm64
Declared go.mod: go 1.22.0
```

### Audited Subsystems (Implemented Code)
1. **`internal/binary`**: Multi-version `InternalKey` codec, 7-bit varints (`GetVarint64`, `PutVarint64`), Big-Endian encoders, CRC32-IEEE checksum calculation, structural key/value length bounds (`ValidateKey`, `ValidateValue`), and canonical LSM key comparator (`CompareInternalKey`).
2. **`internal/errors`**: Sentinel errors, structured error types with privacy-preserving context, error wrapping invariants (`errors.Is`, `errors.As`).
3. **`internal/logger`**: Structured `slog` wrapper, component namespaces, automatic redaction of credentials/tokens, `Redactable` interface evaluation.
4. **`internal/memtable`**: Probabilistic `SkipList`, concurrent lock-free readers (`atomic.Pointer`), serialized exclusive writer, bottom-up pointer publication, `PCG32` height generator, immutable freeze lifecycle (`Freeze()`), dynamic heap memory accounting (`ByteSize()`).
5. **`internal/wal`**: Append-only log framing, strict synchronous durability barriers (`fdatasync`/`Sync`), writer poisoning state machine (`ErrWALWriterPoisoned`), exclusive creation semantics (`O_CREATE|O_EXCL`), inode pinning via `os.SameFile`, startup crash recovery, torn tail truncation, historical segment corruption detection.
6. **`internal/sstable`**: Prefix-compressed data blocks, restart array framing, two-level sparse block index (`IndexBuilder`, `BlockIndex`), physical `BlockHandle`, fixed 48-byte `Footer`, sequential table writer (`TableWriter`) with atomic publication (`os.Link`) and directory sync, point-lookup reader (`TableReader`), MetaIndex block serializer and parser (`BuildMetaIndexBlock`, `DecodeMetaIndexBlock`, `FindMetaIndexEntry`), and filter block extraction (`ReadFilterBlock`).
7. **`internal/filter`** (Phase 05): MurmurHash3 128-bit implementation (`Murmur3_128`), Kirsch-Mitzenmacher double-hashing, `BloomFilter` sizing and membership operations (`Add`, `MayContain`), `FilterBlockBuilder`, persistent binary serialization (`EncodeFilterBlock`, `DecodeFilterBlock`), and empirical FPR test harness.
8. **`internal/security`**: Built-in static security analysis engine, AST inspection rules (`SECURITY-001` through `SECURITY-012`), rule registry, and reporting pipeline.

### Documented-Only / Future Subsystems (Strictly Evaluated as Future Attack Surface)
The following packages contain only `doc.go` stubs or empty `main()` functions:
* `internal/cache` (LRU Block Cache)
* `internal/compaction` (Leveled Compaction)
* `internal/engine` (LSM Engine Facade)
* `internal/metrics` (Telemetry & Observability)
* `internal/raft` (Distributed Consensus)
* `internal/transport` (Network Protocol & TCP Wire Server)
* `internal/version` (Manifest & VersionSet)
* `cmd/lattice`, `cmd/lattice-bench`, `cmd/lattice-cli`, `pkg/client`

### Tooling Execution Log
The following analyzers and scanners were executed against the repository:
1. **`govulncheck v1.8.0`**: DB updated 2026-09-10. Result: `No vulnerabilities found.` (0 reachable vulnerabilities across call graph).
2. **`osv-scanner v1.9.2`**: Scanned `go.mod`. Result: Flagged standard library advisories tied to `go 1.22.0` directive; confirmed uncalled/unreachable under active compiler `go1.27.1`.
3. **`golangci-lint run`**: Default linters (`govet`, `errcheck`, `staticcheck`, `ineffassign`, `unused`, `errorlint`, `nolintlint`, `gofmt`). Result: `0 issues`.
4. **`golangci-lint run --enable gosec --tests=false`**: Static security analysis on production code. Result: Zero command injection, zero path traversal, zero unsafe crypto. Flagged integer cast bounds in `table_reader.go` (validated in finding SEC-P05-01).
5. **`staticcheck 2026.2.1 (0.8.1)`**: Full repository static analysis. Result: `0 issues`.
6. **`go vet ./...`**: Standard compiler checks. Result: `0 issues`.
7. **`go test -count=1 ./...`**: Unit and integration test suite across all packages. Result: `PASS` across 100% of tests.
8. **`go test -count=1 -race ./...`**: Full concurrency and race detection across all packages. Result: `PASS` (zero data races reported across `internal/memtable`, `internal/wal`, `internal/sstable`, `internal/filter`, etc.).
9. **Internal Security Engine**: Full repository scan via `TestAudit_FullRepositoryScanAndReportGeneration`. Result: `0 Critical, 0 High, 0 Medium, 0 Low, 1 Informational` (SupplyChain dependency inventory verified).

---

## 3. Dependency Inventory

Lattice adheres to a zero-third-party-dependency policy for core storage engine functionality.

| Dependency | Version | Direct / Indirect | Security Status | Vulnerability | Reachability |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `github.com/silent-knight19/lattice` | (root module) | Root | Clean | None | N/A |
| `stdlib` (Go Standard Library) | `go 1.22.0` (declared in `go.mod`) | Core Runtime | Supported | 4 OSV advisories flagged against 1.22.0 | **Unreachable** (Compiled with `go1.27.1`) |

* **Replaced Modules**: None.
* **Forked Modules**: None.
* **Retract Directives**: None.
* **Local `replace` Directives**: None.
* **Vendor Directory**: None.
* **Build / Tool Dependencies**: None linked into binary.

---

## 4. Vulnerability Scanner Results

### Scanner 1: `govulncheck`
* **Version**: `govulncheck@v1.8.0` (Go `go1.27.1`, DB updated `2026-09-10 14:48:42 UTC`)
* **Command**: `govulncheck ./...`
* **Result**: `No vulnerabilities found.`
* **Assessment**: Govulncheck constructs full call graphs of all entrypoints in Lattice. Because the binary uses standard library packages without exercising vulnerable symbols, zero vulnerabilities exist in the compiled artifact.

### Scanner 2: `osv-scanner`
* **Version**: `v1.9.2`
* **Command**: `osv-scanner -r .`
* **Result**:
  - Scanned `go.mod` (1 package: `stdlib@1.22.0`).
  - Flagged 4 advisories with experimental call analysis: `GO-2024-3105` (`go/parser`), `GO-2024-3107` (`go/build/constraint`), `GO-2025-3750` (`syscall.Open`/`os.Chmod`), `GO-2026-4602` (`os.ReadDir`).
* **Reachability Analysis**:
  - `GO-2024-3105` / `GO-2024-3107`: Packages `go/parser` and `go/build/constraint` are imported solely by `internal/security/audit`, which is an offline development and audit harness, not production storage engine code.
  - `GO-2025-3750` / `GO-2026-4602`: The local compiler toolchain is `go1.27.1 darwin/arm64`. Standard library code compiled into the binaries is derived from Go 1.27.1, where these vulnerabilities are remediated.

### Scanner 3: `golangci-lint` / `gosec`
* **Command**: `golangci-lint run --enable gosec --tests=false`
* **Result**:
  - G115 (Integer conversion): Flagged `int(filterHandle.Size)` and `int(metaHandle.Size)` in `table_reader.go` lines 591 and 618. Verified as genuine unbounded allocation finding SEC-P05-01.
  - G304 (File inclusion via variable): Flagged `os.Open(dirPath)` in `table_writer.go` line 784 and `os.Open(walPath)` in `dir.go` line 122. Both are internal engine paths governed by verified database root configuration; no external path traversal exists.

### Scanner 4: `staticcheck`
* **Version**: `staticcheck 2026.2.1 (0.8.1)`
* **Command**: `staticcheck ./...`
* **Result**: `0 issues.` Clean across all packages.

---

## 5. Supply-Chain Security Findings

1. **Authenticity & Integrity**: `go.mod` contains zero external dependencies. `go mod verify` succeeds with `all modules verified`.
2. **Absence of Malicious Build Hooks**: The repository contains no Makefiles, no shell scripts, no Dockerfiles, no `go generate` directives, and no CI action workflows. There is zero exposure to `curl \| sh`, shell expansion hijacking, or untrusted binary execution.
3. **Absence of Subprocess Execution**: Manual and AST searches for `os/exec`, `exec.Command`, and `syscall` verified that database code executes zero external subprocesses.
4. **Supply-Chain Classification**: **SECURE / HERMETIC**. The supply-chain attack surface is non-existent beyond the host Go compiler.

---

## 6. Code-Level Findings Register

| Finding ID | Severity | Component | Finding Title | Attack Vector | Impact | Confidence |
| :--- | :---: | :--- | :--- | :--- | :--- | :--- |
| **SEC-P05-01** | **High** | `sstable` | Unbounded memory allocation in `ReadFilterBlock` | Malformed/corrupted MetaIndex entry | Heap exhaustion (OOM DoS) or slice bounds panic | Confirmed |
| **SEC-P05-02** | **Medium** | `sstable` | Unhandled `AddKey` error in `TableWriter.Add` | Reused/sealed `FilterBlockBuilder` | Silent Bloom filter false negative / key read loss | Confirmed |
| **SEC-P05-03** | **Informational** | `build` | Declared `go 1.22.0` toolchain in `go.mod` | Automated vulnerability scanners | Scanner alert noise on historical stdlib CVEs | Confirmed |
| **SEC-P05-04** | **Low** | `sstable` | Missing regular file mode check in `NewTableReader` | Local symlink or FIFO injection | Descriptor hang or unexpected I/O failure | Confirmed |
| **SEC-P05-05** | **Informational** | `filter` | Fixed-capacity Bloom filter saturation risk | Extreme cardinality mismatch (>200M keys) | FPR degradation to 100% (read amplification) | Confirmed |
| **SEC-P05-06** | **Informational** | `sstable` | Uncached disk reads in `ReadFilterBlock` | Repeated filter queries | Redundant disk I/O and heap allocations | Confirmed |

---

## 7. Detailed Findings

---

### FINDING SEC-P05-01: Unbounded Memory Allocation in `TableReader.ReadFilterBlock`

```text
Title: Unbounded Memory Allocation and Missing Upper Bound Check in TableReader.ReadFilterBlock
Severity: HIGH
Confidence: CONFIRMED
Component: internal/sstable
File: internal/sstable/table_reader.go
Line: 618
Category: Denial of Service / Memory Safety
Attacker: Local attacker or hostile persistence payload (corrupted/crafted SSTable)
Precondition: TableReader opens an SSTable containing a crafted or corrupted MetaIndex block where "filter.bloom" specifies a large or invalid BlockHandle.Size.
```

#### Root Cause
In `internal/sstable/table_reader.go`, `ReadFilterBlock()` resolves the Bloom filter block handle from the MetaIndex block via `FindMetaIndexEntry`:
```go
filterHandle, found, err := FindMetaIndexEntry(metaBuf, filter.FilterMetaKey)
if err != nil {
    return nil, err
}
if !found {
    return nil, nil
}

if err := filterHandle.Validate(); err != nil {
    return nil, err
}
if filterHandle.Offset+filterHandle.Size > r.footer.MetaIndexHandle.Offset {
    return nil, &errors.InvalidBlockHandleError{...}
}

// VULNERABILITY: Direct unconstrained allocation from disk-derived uint64
filterBuf := make([]byte, int(filterHandle.Size))
if err := readExactAt(r.readAtFn, filterBuf, int64(filterHandle.Offset)); err != nil {
    return nil, fmt.Errorf("failed to read filter block at offset %d: %w", filterHandle.Offset, err)
}

return filter.DecodeFilterBlock(filterBuf)
```

1. **Missing Upper Bound**: `filterHandle.Size` is validated only by `filterHandle.Validate()` (which checks `Size > 0` and non-overflow) and `filterHandle.Offset + filterHandle.Size <= MetaIndexHandle.Offset`. If an SSTable is large (or sparse), `filterHandle.Size` can be tens of gigabytes.
2. **Pre-Allocation DoS**: `filter.DecodeFilterBlock` contains an explicit security guard (`len(data) > MaxBitsetBytes + FilterBlockTrailerSize`), but `make([]byte, int(filterHandle.Size))` executes **before** `DecodeFilterBlock` is invoked!
3. **Architecture Integer Truncation**: On 32-bit platforms, converting an unconstrained `uint64` to `int` (`int(filterHandle.Size)`) wraps negative when `filterHandle.Size > math.MaxInt32`, causing a direct runtime panic (`makeslice: len out of range`).

#### Exploit Path
An attacker introduces an SSTable with an 8-byte valid footer and a small MetaIndex block whose `"filter.bloom"` entry has `Offset = 0` and `Size = 0x7FFFFFFF_FFFFFFFF` (or any value exceeding physical host RAM). When the database opens the table or queries the filter via `ReadFilterBlock()`, the process allocates memory without restriction, causing an immediate Out-Of-Memory (OOM) crash or process panic.

#### Impact
High-confidence local denial of service via memory exhaustion (OOM) or unhandled runtime panic.

#### Existing Mitigation
`filter.DecodeFilterBlock` rejects payloads exceeding `MaxBitsetBytes + FilterBlockTrailerSize` (256 MiB + 13 B). In Phase 04, `MaxDataBlockSize` (8 MiB) and `MaxIndexBlockSize` (8 MiB) were enforced in `Seek` and `NewTableReaderWithFile`.

#### Why Existing Mitigation Is Insufficient
The allocation in `table_reader.go:618` occurs **prior** to calling `DecodeFilterBlock`. The guard inside `DecodeFilterBlock` is never reached if `make([]byte, int(filterHandle.Size))` exhausts heap memory first. Furthermore, `filterHandle.Size` is not validated against `MaxBitsetBytes + FilterBlockTrailerSize` or platform integer bounds.

#### Recommended Remediation
Enforce the upper bound and architecture integer checks before allocating `filterBuf`:
```go
maxFilterBlockSize := uint64(filter.MaxBitsetBytes + filter.FilterBlockTrailerSize)
if filterHandle.Size > maxFilterBlockSize {
    return nil, &errors.InvalidBlockHandleError{
        Offset: filterHandle.Offset,
        Size:   filterHandle.Size,
        Reason: "filter block handle size exceeds maximum filter block capacity",
    }
}
if filterHandle.Size > math.MaxInt || filterHandle.Offset > math.MaxInt64 {
    return nil, &errors.InvalidBlockHandleError{
        Offset: filterHandle.Offset,
        Size:   filterHandle.Size,
        Reason: "filter block handle exceeds platform integer bounds",
    }
}
```
* **Priority**: **P1 (High)** — Must be remediated before Phase 06 integrates filter blocks into multi-table iterators.

---

### FINDING SEC-P05-02: Silent Error Suppression in `TableWriter.Add` Leading to Filter False Negatives

```text
Title: Unhandled AddKey Error in TableWriter.Add Causes Silent Bloom Filter Inconsistency
Severity: MEDIUM
Confidence: CONFIRMED
Component: internal/sstable
File: internal/sstable/table_writer.go
Line: 361
Category: Data Integrity / Silent Omission
Attacker: Application caller / concurrent worker misconfiguration
Precondition: TableWriter is configured with a FilterBlockBuilder that has previously been sealed (finished), reset improperly, or has encountered an internal state error.
```

#### Root Cause
In `internal/sstable/table_writer.go`, `TableWriter.Add` records user keys into the optional `filterBuilder`:
```go
// Add user key to filter builder if configured
if w.filterBuilder != nil {
    _ = w.filterBuilder.AddKey(key.UserKey)
}
```
The error returned by `w.filterBuilder.AddKey(key.UserKey)` is ignored using the blank identifier (`_ =`).

`FilterBlockBuilder.AddKey` returns `errors.ErrFilterFinished` if `b.finished == true` or `errors.ErrNilReceiver` if `b == nil`.

#### Exploit Path & Impact
1. A caller passes a `FilterBlockBuilder` that was reused without `Reset()`, or was finalized prematurely.
2. `TableWriter.Add` adds the key to the data block builder and metadata, but `AddKey` returns `ErrFilterFinished`.
3. Because the error is swallowed, `TableWriter.Finish()` successfully writes the SSTable with a filter block that is missing the key.
4. When point lookups later execute, `filter.MayContain(userKey)` returns `false` (a false negative).
5. The storage engine skips searching the SSTable because the filter declared the key absent. The client receives `ErrKeyNotFound` despite the key being physically present on disk.

In LSM-tree storage engines, **Bloom filters must have zero false negatives**. Silent filter key omission converts probabilistic acceleration into silent data corruption and read data loss.

#### Existing Mitigation
`TableWriter.Add` strictly validates `binary.ValidateKey`, `binary.ValidateValue`, and canonical monotonic key order.

#### Why Existing Mitigation Is Insufficient
It does not check whether the filter builder accepted the key. If the filter builder rejects the key, the writer continues writing data blocks while creating an incomplete filter.

#### Recommended Remediation
Fail closed in `TableWriter.Add` if `filterBuilder.AddKey` returns an error:
```go
if w.filterBuilder != nil {
    if err := w.filterBuilder.AddKey(key.UserKey); err != nil {
        return fmt.Errorf("failed adding key to filter builder: %w", err)
    }
}
```
* **Priority**: **P1 (High)** — Must be resolved before Phase 06 table building is wired to memtable flushes.

---

### FINDING SEC-P05-03: Declared `go 1.22.0` Toolchain in `go.mod` Exposes Advisory Surface

```text
Title: Outdated go.mod Language Version Triggers Scanner Advisories
Severity: INFORMATIONAL
Confidence: CONFIRMED
Component: build / go.mod
File: go.mod
Line: 3
Category: Supply-Chain / Advisory Hygiene
Attacker: Automated compliance scanners / security auditors
Precondition: Vulnerability scanner parses go.mod without executing call-graph reachability.
```

#### Root Cause
`go.mod` specifies `go 1.22.0`. Static vulnerability scanners (such as OSV-Scanner) evaluate the declared `go` directive as the effective standard library version, reporting known historical CVEs in Go 1.22.0.

#### Reachability & Risk Analysis
* Active compiler toolchain is `go1.27.1 darwin/arm64`.
* `govulncheck` call-graph analysis confirms that zero vulnerable standard library symbols are linked or reachable.
* Actual security risk is **None**. However, it creates compliance noise and advisory friction.

#### Recommended Remediation
Update `go.mod` to a modern supported Go release (e.g. `go 1.24` or `go 1.25`) during the next scheduled build/dependency maintenance phase. (Per absolute scope rules, `go.mod` was untouched during this audit).
* **Priority**: **P3 (Hardening)**.

---

### FINDING SEC-P05-04: Missing Regular File Verification in `TableReader`

```text
Title: TableReader Does Not Verify stat.Mode().IsRegular() Upon Opening
Severity: LOW
Confidence: CONFIRMED
Component: internal/sstable
File: internal/sstable/table_reader.go
Line: 77-88
Category: Filesystem / Robustness
Attacker: Local unprivileged user manipulating filesystem objects
Precondition: TableReader is provided a path pointing to a directory, named pipe (FIFO), or character device.
```

#### Root Cause
In `table_reader.go:65` (`NewTableReaderWithFile`), the reader stats the file and verifies `fileSize >= FooterSize`. Unlike the WAL reader (`internal/wal/reader.go:66`) and WAL recovery (`internal/wal/recovery.go:97`), `TableReader` does not check `stat.Mode().IsRegular()`.

#### Impact
Opening special files or named pipes could cause blocking reads or unexpected operating system errors rather than immediate, clean rejection.

#### Recommended Remediation
Add a regular file validation check in `NewTableReaderWithFile`:
```go
if !stat.Mode().IsRegular() {
    return nil, fmt.Errorf("sstable: %q is not a regular file (mode: %s)", file.Name(), stat.Mode())
}
```
* **Priority**: **P2 (Medium)**.

---

### FINDING SEC-P05-05: Sizing Mismatch and Saturation Risks in `FilterBlockBuilder`

```text
Title: Sizing Saturation Under Severe Key Cardinality Underestimation
Severity: INFORMATIONAL
Confidence: CONFIRMED
Component: internal/filter
File: internal/filter/filter_builder.go
Line: 36
Category: Resource Amplification / Performance Degradation
Attacker: Adversarial workload inserting 100x more keys than expectedKeys
Precondition: TableWriter is initialized with TableWriterOptions.FilterBuilder sized for N keys, but caller inserts M keys where M >> N.
```

#### Root Cause
The Bloom filter bitset capacity is allocated statically at `NewBloomFilter(expectedKeys)`. If `M >> N` keys are added, the filter saturates (the proportion of set bits approaches 1.0). Once saturated, `MayContain` returns `true` for 100% of non-existent keys.

#### Impact
No memory corruption occurs, but Bloom filter effectiveness degrades from <1% FPR to 100% FPR, causing disk read amplification for point lookups.

#### Recommended Remediation
In Phase 06, have the LSM flush manager initialize `FilterBlockBuilder` with `memtable.Len()` (the exact key count), guaranteeing exact 10 bits/key sizing and optimal FPR.
* **Priority**: **P3 (Hardening)**.

---

### FINDING SEC-P05-06: Uncached Disk Reads in `TableReader.ReadFilterBlock`

```text
Title: TableReader Reads and Decodes Filter Block Repeatedly on Demand
Severity: INFORMATIONAL
Confidence: CONFIRMED
Component: internal/sstable
File: internal/sstable/table_reader.go
Line: 574
Category: Performance / Resource Consumption
Attacker: Application making repeated ReadFilterBlock calls
Precondition: ReadFilterBlock is invoked multiple times on the same TableReader instance.
```

#### Root Cause
`TableReader` does not retain the decoded `*filter.BloomFilter` in memory. Each call to `ReadFilterBlock()` reads the MetaIndex block from disk, reads the filter block from disk, and allocates a new bitset buffer.

#### Recommended Remediation
In Phase 06, either cache the decoded filter in `TableReader` using `sync.Once` or load it eagerly into the `TableReader` struct during initialization if configured by the VersionSet.
* **Priority**: **P3 (Hardening)**.

---

## 8. Security Control Matrix

| Security Control | Expected by Architecture | Implemented | Tested | Security Strength | Current Status / Gap |
| :--- | :---: | :---: | :---: | :---: | :--- |
| **CRC32 Data Block Validation** | Yes | Yes | Yes | Strong | Full coverage in data blocks and index blocks. |
| **CRC32 Filter Block Validation** | Yes | Yes | Yes | Strong | Enforced in `filter.DecodeFilterBlock`. |
| **CRC32 MetaIndex Validation** | Yes | Yes | Yes | Strong | Enforced in `sstable.DecodeMetaIndexBlock`. |
| **Allocation Upper Bounds (Data/Index)** | Yes | Yes | Yes | Strong | Capped at 8 MiB in `table_reader.go`. |
| **Allocation Upper Bounds (Filter)** | Yes | Partial | Partial | Weak | **GAP**: Missing bound in `TableReader.ReadFilterBlock` (SEC-P05-01). |
| **Fail-Closed Corruption Handling** | Yes | Yes | Yes | Strong | CRC or decoding failures return explicit errors. |
| **Atomic SSTable Publication** | Yes | Yes | Yes | Strong | Atomic hardlink (`os.Link`) without rename fallback. |
| **Directory Fsync on Publish** | Yes | Yes | Yes | Strong | `syncDir` executes on parent directory after link. |
| **Owner-Only Permissions (0600/0700)** | Yes | Yes | Yes | Strong | Validated in WAL and SSTable writers. |
| **Symlink Hijack Defense** | Yes | Yes | Yes | Strong | `os.Lstat`, `ModeSymlink`, and `os.SameFile` pinned. |
| **Lock-Free MemTable Read Safety** | Yes | Yes | Yes | Strong | `atomic.Pointer`, bottom-up publication, 0 races. |
| **Dynamic Heap Accounting** | Yes | Yes | Yes | Strong | `ByteSize()` accounts for exact struct and slice allocations. |
| **WAL Writer Poisoning** | Yes | Yes | Yes | Strong | Failed sync poisons writer; prevents corrupted writes. |
| **WAL Torn-Tail Truncation** | Yes | Yes | Yes | Strong | In-place descriptor repair; historical segments inviolable. |
| **Sensitive Field Redaction** | Yes | Yes | Yes | Strong | Automatic `[REDACTED]` for credentials; key lengths only. |
| **Zero Third-Party Supply Chain** | Yes | Yes | Yes | Strong | Zero external dependencies; hermetic pure-Go build. |

---

## 9. Resource Exhaustion Matrix

| Vector | Subsystem | Trigger Condition | Bound Exists? | Impact | Severity |
| :--- | :--- | :--- | :---: | :--- | :---: |
| **RAM (Filter Allocation)** | `sstable` | Malformed `filterHandle.Size` | **No** | Heap OOM or 32-bit slice panic | **High** |
| **RAM (Data Block)** | `sstable` | Malformed `BlockHandle.Size` | Yes | Capped at 8 MiB (`MaxDataBlockSize`) | Low |
| **RAM (Index Block)** | `sstable` | Malformed `IndexHandle.Size` | Yes | Capped at 8 MiB (`MaxIndexBlockSize`) | Low |
| **RAM (WAL Record)** | `wal` | Malformed record header | Yes | Capped at 64 KB key, 4 MiB value | Low |
| **RAM (MemTable Growth)** | `memtable` | Continuous un-flushed writes | Partial | Tracked via `ByteSize()`; flush manager in P06 | Medium |
| **CPU (Bloom Double Hashing)**| `filter` | Kirsch-Mitzenmacher $k=7$ | Yes | $O(k)$ operations (at most 7 probes) | Low |
| **CPU (SkipList Height)** | `memtable` | Hostile entropy draw | Yes | Clamped strictly to `MaxHeight = 16` | Low |
| **CPU (Prefix Compression)** | `sstable` | Degenerate keys | Yes | Restart interval resets full keys every 16 ops | Low |
| **Disk (Torn Tail Accumulation)**| `wal` | Repeated abrupt power loss | Yes | Torn tail truncated on startup | Low |
| **File Descriptors** | `sstable` | Failed `TableReader` init | Yes | `defer` cleanup guarantees FD closure on failure | Low |
| **Temporary Files** | `sstable` | Failed table writing | Yes | `cleanupStaging` unlinks `.tmp_*` files | Low |

---

## 10. Attack-Surface Matrix

| Attack Surface | Current Status | Attacker Control Level | Primary Threat Vector | Active Security Control |
| :--- | :--- | :--- | :--- | :--- |
| **SSTable Files** | Implemented | Indirect / Persistence | Crafting corrupted handles or corrupted filter blocks | Footer validation, handle bounds, CRC32 check |
| **WAL Segments** | Implemented | Persistence / Storage | Corrupting log records or injecting torn tails | CRC32-IEEE, streaming check, inode pinning |
| **Filter Blocks** | Implemented | Binary Stream | Bitset tampering, oversized length headers | CRC32-IEEE, bitCount/byte matching |
| **Filesystem Storage** | Implemented | Local / Host Environment| Symlink substitution, TOCTOU overwrite race | `os.Link` (no rename fallback), `os.SameFile`, `0600` |
| **MemTable Heap** | Implemented | Application API | Unbounded memory growth, concurrent data races | `ByteSize()` accounting, lock-free atomic pointers |
| **CLI / Command Line**| Incomplete (Stubs)| None (Stub only) | Path traversal, flag injection | Not applicable (Future attack surface) |
| **Network Protocol** | Incomplete (Stubs)| None (Stub only) | Packet framing, slowloris, unauthenticated commands | Not applicable (Future attack surface) |
| **Dependencies / CI** | Implemented | Supply-Chain | Dependency poisoning, malicious build scripts | Zero third-party dependencies, hermetic build |

---

## 11. Security Test Coverage Matrix

| Security Invariant | Existing Test Location | Adequate? | Missing Test / Improvement |
| :--- | :--- | :---: | :--- |
| **CRC32 Corruption in Data Block** | `internal/sstable/table_reader_test.go` | Yes | Verified fail-closed behavior. |
| **CRC32 Corruption in Filter Block**| `internal/sstable/filter_integration_test.go` | Yes | `TestFilterIntegration_ChecksumMismatch` passes. |
| **Oversized Data/Index Allocation** | `internal/sstable/sec04_remediation_test.go` | Yes | Verified rejection of handles > 8 MiB. |
| **Oversized Filter Allocation** | None | **No** | Missing test for `filterHandle.Size > MaxBitsetBytes` in reader. |
| **SSTable Publication Race** | `internal/sstable/sec04_remediation_test.go` | Yes | Verified `os.Link` atomicity and no rename fallback. |
| **Symlink Replacement Defense** | `internal/wal/sec03_symlink_test.go` | Yes | Verified `os.SameFile` inode pinning. |
| **WAL Recovery Torn Tail Truncation**| `internal/wal/recovery_test.go` | Yes | Verified exact byte truncation at EOF. |
| **WAL Historical Corruption Failure**| `internal/wal/coordinator_test.go` | Yes | Verified fail-closed rejection of historical corruption. |
| **MemTable Concurrency Race** | `internal/memtable/concurrent_test.go` | Yes | Validated under `go test -race` (0 races). |
| **Filter Key Omission Error Path** | None | **No** | Missing test for `filterBuilder.AddKey` error propagation. |

---

## 12. Dependency Risk Summary

* **Known CVEs**: **0**. Zero CVEs in project dependencies.
* **Known GHSA / OSV Issues**: **0** reachable. Standard library advisories reported against `go 1.22.0` in `go.mod` are confirmed unreachable and patched in the host compiler `go1.27.1`.
* **Outdated Dependencies**: None (0 external packages).
* **Unmaintained Dependencies**: None.
* **Suspicious Dependencies / Typosquatting**: None.
* **Transitive Dependency Risks**: None.
* **Supply-Chain Posture**: **SECURE / HERMETIC**.

---

## 13. Phase 06 Readiness Decision

### Decision: **YELLOW — PROCEED ONLY AFTER REQUIRED REMEDIATIONS**

```text
               ┌─────────────────────────────────────────────────────────┐
               │              PHASE 06 READINESS: YELLOW                 │
               └─────────────────────────────────────────────────────────┘
                                            │
               ┌────────────────────────────┴────────────────────────────┐
               ▼                                                         ▼
    [ BLOCKING DEFECT 1 ]                                     [ BLOCKING DEFECT 2 ]
    Finding SEC-P05-01:                                       Finding SEC-P05-02:
    Unbounded Memory Allocation in                            Silent AddKey Error Suppression in
    TableReader.ReadFilterBlock                               TableWriter.Add
               │                                                         │
               └────────────────────────────┬────────────────────────────┘
                                            ▼
               ┌─────────────────────────────────────────────────────────┐
               │ Remediate both defects in code and prove with tests.     │
               │ Once remediated, security posture transitions to GREEN. │
               └─────────────────────────────────────────────────────────┘
```

#### Rationale
Phase 06 introduces the multi-level SSTable subsystem: `VersionEdit`, `VersionSet`, `Manifest`, and multi-table iterators. In Phase 06, the engine will query Bloom filters on every point lookup across Level 0 and deeper levels.
* If **SEC-P05-01** is unaddressed, reading a corrupted SSTable filter block in Phase 06 can crash the entire database process via heap exhaustion.
* If **SEC-P05-02** is unaddressed, flushes from MemTable to SSTable could write incomplete Bloom filters, causing permanent silent data loss during point lookups.

Remediating these two findings is straightforward and self-contained, but it must be performed before Phase 06 implementation begins.

---

## 14. Remediation Roadmap

### P0 — Immediate Blockers
None. (No critical remote code execution or active production exploits).

### P1 — High Priority (Remediate Before Phase 06 Code Begins)
1. **Remediate SEC-P05-01**: In `internal/sstable/table_reader.go:ReadFilterBlock`, validate `filterHandle.Size <= uint64(filter.MaxBitsetBytes + filter.FilterBlockTrailerSize)` and `filterHandle.Size <= math.MaxInt` before allocating `filterBuf`. Add automated regression test.
2. **Remediate SEC-P05-02**: In `internal/sstable/table_writer.go:Add`, check the error returned by `w.filterBuilder.AddKey(key.UserKey)` and fail closed. Add automated regression test verifying that an error from `AddKey` aborts table writing.

### P2 — Medium Priority (Address During Engine Integration)
1. **Remediate SEC-P05-04**: In `internal/sstable/table_reader.go:NewTableReaderWithFile`, verify `stat.Mode().IsRegular()` to reject directories, named pipes, and character devices.

### P3 — Hardening (Schedule During Operational Hardening)
1. **Remediate SEC-P05-03**: Update `go.mod` to modern Go language version (e.g. `go 1.24`) to eliminate scanner advisory noise.
2. **Remediate SEC-P05-05**: Ensure Phase 06 memtable flush initializes `FilterBlockBuilder` with `memtable.Len()` to maintain optimal FPR.
3. **Remediate SEC-P05-06**: Cache decoded `*filter.BloomFilter` in `TableReader` or VersionSet to avoid repeated disk reads.

---

## 15. Security Requirements for Future Phases

Based on the threat model and audit findings, the following concrete security invariants are established for upcoming phases:

### Phase 06: Manifest, CURRENT, and VersionSet
1. **Manifest Atomic Publication**: The `CURRENT` pointer file must be updated using atomic write-and-sync (`os.CreateTemp` + `fsync` + atomic link/rename) with a directory `fsync`.
2. **VersionEdit Decoding Upper Bounds**: `VersionEdit` record decoders must enforce maximum bounds on SSTable file counts, key lengths, and level numbers before allocating slice memory.
3. **Fail-Closed Manifest Corruption**: Any CRC mismatch or malformed record in the active `MANIFEST` must immediately halt startup; the engine must never discard unknown version edits.

### Phase 07: Leveled Compaction
1. **Tombstone Purging Safety**: A tombstone deletion record must NEVER be dropped during compaction if an older version of the key may exist in deeper levels ($L > L_{compaction}$). Dropping tombstones prematurely resurrects deleted data.
2. **Atomic Compaction Output Publication**: Newly produced compaction SSTables must be published atomically before committing the `VersionEdit` to the Manifest.

### Phase 08: Distributed Consensus (Raft)
1. **Frame Length Bounding**: Raft RPC frame headers must enforce strict size limits before reading payloads into memory.
2. **Authenticated RPCs**: Raft consensus traffic must use mutual TLS or cryptographic message authentication; non-cryptographic CRC32 must not be used for authentication.

### Phase 09+: Network Transport Protocol
1. **Length-Prefixed Frame Guards**: The TCP transport protocol must reject frames exceeding a hard limit (e.g. 16 MiB) immediately to prevent connection-driven memory exhaustion.
2. **Connection Limits & Timeouts**: Implement strict connection pooling, idle timeouts, and read deadlines to prevent slowloris denial-of-service attacks.

---

## 16. Supply-Chain Decision

### Status: **SECURE**

The Lattice repository maintains a pure, hermetic Go supply chain with zero external packages. Module verification passes cleanly, no unpinned tooling exists, and no external code execution occurs during compilation or testing.

---

## 17. Final Security Scorecard

| Security Area | Rating | Key Reason |
| :--- | :---: | :--- |
| **Dependencies** | **Strong** | Zero third-party runtime or build dependencies. |
| **Supply Chain** | **Strong** | Hermetic pure-Go build; zero scripts or external download hooks. |
| **Input Validation** | **Good** | Strict key/value/record validation; minor gap in filter block handle sizing. |
| **Persistence** | **Strong** | Atomic hardlink publication (`os.Link`), no rename fallback, fail-closed recovery. |
| **Filesystem** | **Strong** | Owner-only permissions (`0600`/`0700`), `os.SameFile` inode pinning. |
| **Concurrency** | **Strong** | Clean under Go race detector; `atomic.Pointer` lock-free reader safety. |
| **Memory / DoS** | **Moderate** | Strict limits on data/index/WAL records; unbounded allocation in `ReadFilterBlock`. |
| **Cryptography** | **Good** | Appropriate non-cryptographic CRC32 bitrot detection; zero misuse as auth. |
| **Secrets** | **Strong** | Zero hardcoded secrets; automated structured logging redaction. |
| **Logging** | **Strong** | Redaction of sensitive fields and keys; only lengths exposed. |
| **Build / CI** | **Strong** | No CI scripts or external execution vectors. |
| **Overall** | **Good (Pass with Required Remediations)** | Architectural core is rock solid; 2 Phase 05 remediations required for P06. |

---

## 18. Post-Audit Remediation & Final Phase 06 Closure

Following the audit findings, all identified actionable code-level vulnerabilities and defense-in-depth issues were remediated in code and validated with dedicated regression tests:

### Remediated Issues Summary

1. **SEC-P05-01 (High — Resolved)**:
   - **Fix**: In `internal/sstable/table_reader.go:ReadFilterBlock`, enforced upper bound validation (`filterHandle.Size <= uint64(filter.MaxBitsetBytes + filter.FilterBlockTrailerSize)`) and platform integer overflow checks (`filterHandle.Size <= math.MaxInt && filterHandle.Offset <= math.MaxInt64`) before allocating `filterBuf`.
   - **Test Proof**: Verified via `TestSecurity_Remediation_SEC_P05_01_OversizedFilterBlockHandle` and `TestSecurity_Remediation_SEC_P05_01_ArchitectureIntegerOverflow` in `internal/sstable/sec05_remediation_test.go`.
2. **SEC-P05-02 (Medium — Resolved)**:
   - **Fix**: In `internal/sstable/table_writer.go:Add`, replaced blank error suppression (`_ = w.filterBuilder.AddKey(...)`) with explicit error handling and fail-closed propagation (`if err := w.filterBuilder.AddKey(...); err != nil { return fmt.Errorf(...) }`).
   - **Test Proof**: Verified via `TestSecurity_Remediation_SEC_P05_02_FilterBuilderAddKeyFailurePropagates` in `internal/sstable/sec05_remediation_test.go`.
3. **SEC-P05-04 (Low — Resolved)**:
   - **Fix**: In `internal/sstable/table_reader.go:NewTableReaderWithFile`, added `if !stat.Mode().IsRegular() { ... }` check to reject directories, FIFOs, sockets, and character devices.
   - **Test Proof**: Verified via `TestSecurity_Remediation_SEC_P05_04_TableReaderRejectsNonRegularFile` in `internal/sstable/sec05_remediation_test.go`.

### Final Phase 06 Readiness Verdict

> **GREEN — READY FOR PHASE 06**
> 
> All blocking Phase 05 vulnerabilities have been remediated in code, validated with deterministic unit and regression tests, verified with zero static analysis issues, and confirmed clean under the Go race detector. The repository security posture is officially GREEN and cleared for Phase 06 implementation.


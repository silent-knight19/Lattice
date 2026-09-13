# SEC-AUDIT-P00-P05 — Full Codebase Security Audit (Phase 00 → Phase 05 Security Gate)

* **Audit Identity**: `SEC-AUDIT-P00-P05`
* **Audit Date (UTC)**: 2026-09-12
* **Mode**: Audit-only. No production code, dependency, configuration, or test was modified.
  Only artifact created: this report (`docs/security-audit-phase-00-05.md`).
* **Time-box**: bounded audit per §2A (P0–P3 prioritized; P4–P5 compressed).
* **Overall Verdict**: **YELLOW — remediate SEC-001 (P1) before Phase 06; otherwise strong posture**
* **Phase 06 Gate**: **YELLOW — PAUSED until SEC-001 remediated and regression-tested**

---

## 1. Executive Summary

The entire Lattice implementation from Phase 00 through Phase 05 was audited as an
adversarial baseline. The codebase exhibits a **strong security posture**:

* Zero third-party runtime dependencies; hermetic pure-Go build (`go mod verify`: clean).
* `govulncheck`: **No vulnerabilities found**. `go vet`: clean. Default `golangci-lint`: **0 issues**.
* `go test ./...`: **PASS** on all packages. `go test -race ./...`: **PASS, zero data races**
  (including the ~255 s memtable race suite).
* Prior Phase 05 remediations verified **intact at HEAD**: `ReadFilterBlock` allocation bound,
  `TableWriter.Add` filter-error propagation, `NewTableReaderWithFile` `IsRegular` check.
* No `os/exec`, no `net/` listeners, no `os.Rename` overwrite path, no `WriteFile`/`RemoveAll`/
  `Symlink` in production code; CRC32 fail-closed handling throughout WAL/SSTable/filter.

One new **confirmed vulnerability** was found and arithmetically proven with a PoC:

* **SEC-001 (HIGH, CONFIRMED)**: `DecodeMetaIndexBlock` (`internal/sstable/meta_index.go:174-181`)
  performs `uint64(varintLen) + keyLen + BlockHandleSize` with an **unbounded attacker-controlled
  `keyLen` (up to 2⁶⁴−1)**, compares the **wrapped** value for equality, then slices with
  `int(keyLen)`. A 32-byte crafted MetaIndex block with valid CRC32
  (`keyLen = 2⁶⁴−6`, `varintLen = 10`, entry length 20) passes the length check via wrap
  (`10 + (2⁶⁴−6) + 16 ≡ 20 mod 2⁶⁴`) and then **panics** (`slice bounds out of range [10:4]`,
  reproduced). Reachable via `TableReader.ReadFilterBlock → FindMetaIndexEntry` and any
  `DecodeMetaIndexBlock` caller holding a crafted/corrupt SSTable. Sibling decoder
  `DecodeBlockIndex` (`index_builder.go:456`) already bounds `keyLen` — the MetaIndex path is
  an omission, **not** a regression of a previously established guarantee.

Supporting weaknesses (no RCE, no arbitrary file access, no privilege escalation found):

* **SEC-002 (MEDIUM)**: SSTable writer stages into a `MkdirAll`-created parent with no parent
  symlink pinning; `NewTableWriterWithFile` bypasses all staging checks.
* **SEC-003 (MEDIUM)**: MemTable/SkipList has no entry-count/byte-cap; unbounded heap growth is
  accounting-only (`ByteSize`) until the Phase 06/10 flush manager exists.
* **SEC-004/005/006 (LOW)**: reader symlink-following asymmetry; `MaskSecret` prefix/suffix leak;
  logger redaction keyword/recursion gaps.
* **SEC-007/008/009 (INFORMATIONAL)**: `go 1.22.0` directive scanner noise; WAL rotation
  close-failure stale-active; saturating accounting + contract-panic hardening notes.

**Finding totals**: Critical 0 · High 1 · Medium 2 · Low 3 · Informational 3 (9 total).
Dependency: 0 reachable. Supply-chain: hermetic/secure.
Cross-phase regression verdict: **NO** — earlier guarantees intact; SEC-001 is an original
omission in Phase 04/05 code, present since introduction, not a later bypass.

---

## 2. Audit Scope

### In scope (audited at HEAD)

```text
Phase 00  foundations, errors, logger, lint config
Phase 01  binary primitives (endian, varint, CRC, validate, InternalKey, types)
Phase 02  WAL (record framing, writer, reader, rotation, recovery, coordinator, queue/runner)
Phase 03  MemTable + SkipList (node, random/PCG32, size accounting, iterator, freeze)
Phase 04  SSTable (BlockBuilder, IndexBuilder, footer, BlockHandle, TableWriter, TableReader,
          MetaIndex, publication, permissions)
Phase 05  Bloom filter (sizing, Murmur3, double hashing, Add/MayContain, FilterBlockBuilder,
          Encode/Decode, MetaIndex filter integration, FPR harness)
Cross-cutting  internal/security engine+rules, errors taxonomy, filesystem ops, concurrency,
          logging/secrets/crypto, build config, tests
```

### Out of scope (not implemented; future requirements only, §20)

Manifest/CURRENT/VersionSet, recovery orchestration above WAL/SSTable primitives, compaction,
block cache, engine, TCP protocol, CLI behavior beyond stubs, Raft/replication/linearizable reads.

### Coverage statement (per bounded-audit rule)

* **Audited**: dependency graph, supply chain, binary parsers, WAL, MemTable/SkipList, SSTable
  writer/reader/publication, MetaIndex, Bloom/filter blocks, checksums, errors, panics, integers,
  filesystem ops, concurrency (`-race` + manual), FD/goroutine lifetime, logging/secrets/crypto,
  test-security controls, cross-phase regression, docs-vs-code claims.
* **Partially audited**: CPU-amplification quantification (reasoned O(k)/O(16)-bounded, not
  micro-benchmarked per vector); FPR adversarial distribution (deterministic-population basis
  accepted, saturation modeled, not re-measured).
* **Not audited**: network/Raft/consensus (do not exist); formal verification; exhaustive
  malformed-byte enumeration.
* **Unverified**: none at P0–P3 level. All P0–P3 claims below are evidence-backed.

---

## 3. Exact Repository Baseline

```text
Branch:       main (up to date with origin/main)
HEAD:         51d53175019ddd8d363f7caa6bd51d54d9d66319
HEAD subject: security: [SEC-P05-REMED] remediate filter block bounds and error handling vulnerabilities
Working tree: clean at audit start (git status), clean at audit end except this report
Remote:       https://github.com/silent-knight19/Lattice.git (fetch+push)
Go toolchain: go1.27.1 darwin/arm64 (go.mod declares `go 1.22.0`)
Platform:     darwin/arm64
Go files:     170; packages: 23 (go list ./...)
```

Phase checkpoint history (`git log --oneline --all`, 64 commits, chronological):

```text
P00  5626212 module+layout → 40a5eba/ffeb0ab/964708c/7ac8b3b + fixes ac47b91/94643f5/c6737df
P01  262d773 endian → 57a13e2 varint → e5d9d73 CRC → c0ac804 validate → 7501807/08a72a3 types+InternalKey → ea08c0f audit fix
P02  1e39abd header → 1aa1c70/cbba6fd/2784326/e7b884f record+dir → 0da8033/bce7527/4bf8b02/8fe671e/782e8a3/72f76c9/66673b8 writer/reader/torn-tail/rotation/recovery → 75bd160/33c944c/98c5fc9 group commit → 4330dae/d20e717/e0b1cb7/7fa4490/57472d2 SEC-01..04
P03  e536656 node+height → 65cd2f6/07f1722/57e8a67/830f10c/fd84252 insert/reads/accounting/iterator/freeze → 96bbda0 SEC-P03 audit
P04  9370dc4 block → 78132ee trailer → ec872df/0633c3b index+footer → 41ed0bd/38ec23d writer+reader → ba954b6/0902c68/070d2fc/1272fb4/6bead86/dea69f3 hardening (Link-only, race close)
P05  eefd914 sizing → ba06a28 murmur3+probes → 0e3a48c serialization → 25eef70 1M FPR → c9c3d05 audit → 51d5317 REMED (current HEAD)
```

No evidence of a later phase bypassing an earlier security guarantee (see §12).

---

## 4. Phase 00–05 Reconstruction

| Phase | Subsystems implemented | Security-relevant surface |
| ----- | ---------------------- | ------------------------- |
| P00 | module layout, `errors`, `logger`, golangci config | sentinel/wrapped errors; redacted structured logging; lint gates (govet/errcheck/staticcheck/errorlint) |
| P01 | endian, varint, CRC32-IEEE, `ValidateKey/Value`, `InternalKey`+comparator, SeqNum/OpType | every higher layer inherits these bounds; truncation/overflow/canonical-ordering contracts |
| P02 | WAL record codec, sync writer + poisoning, reader, rotation/sequencing, torn-tail recovery, multi-segment coordinator, group-commit queue/runner | CRC fail-closed recovery; `O_EXCL`/`O_APPEND`/0600; symlink+`SameFile` pinning; seq monotonicity; batch ownership |
| P03 | SkipList node/height (`MaxHeight 16`, PCG32+`crypto/rand` seed), serialized writer + lock-free readers, `ByteSize` accounting, weak live iterator, `Freeze` | OOM accounting (no cap); aliasing discipline; freeze race; iterator lifetime; height clamp |
| P04 | BlockBuilder+restart+CRC, IndexBuilder+BlockHandle, 48 B footer+magic, TableWriter (staging+`os.Link` publish+dir sync+0600/0700), TableReader+Seek, MetaIndex codec | path traversal/symlink/TOCTOU/perms; offset+size overflow; footer/index/block/meta corruption handling |
| P05 | Bloom sizing (`MaxKeyCount`, `MaxBitsetBytes` 256 MiB), Murmur3-128, double hashing, `FilterBlockBuilder`, `Encode/DecodeFilterBlock`, `ReadFilterBlock`, 1 M FPR harness | filter alloc bound (remediated); `AddKey` error path (remediated); MetaIndex keyLen wrap (**SEC-001**, missed) |

Stub-only (doc.go / empty main, correctly treated as future surface):
`internal/{cache,compaction,engine,metrics,raft,transport,version}`, `cmd/lattice*`, `pkg/client`.

**DOCUMENTATION/IMPLEMENTATION MISMATCH** (informational, §13/SEC-009 context):
`README.md` roadmap checkboxes still show P02–P05 unchecked although code, tests, and audits
through P05 (+REMED) are present at HEAD. No security credit is affected; flagged so future
readers do not under-scope the trusted base.

---

## 5. Dependency Inventory

| Module | Version | Direct? | Role |
| ------ | ------- | ------- | ---- |
| `github.com/silent-knight19/lattice` | root | — | all production code |
| `go` toolchain directive | `1.22.0` | — | language version floor |
| stdlib (compiled) | `go1.27.1` | core | runtime + `slog`, `sync/atomic`, `hash/crc32`, `crypto/rand`, `os`, `io` |
| third-party runtime deps | none | — | `go list -m all` returns root only |
| indirect/test/build deps | none | — | no `go.sum` (zero external modules) |
| replace/fork/retract/vendor | none | — | verified |

`go mod graph`: only `lattice → go@1.22.0 → toolchain@go1.22.0`. `go mod verify`: `all modules verified`.

---

## 6. Scanner Results

| Tool | Version | Command | Scope | Result | Limitations |
| ---- | ------- | ------- | ----- | ------ | ----------- |
| govulncheck | v1.1.4 (DB vuln.go.dev) | `/tmp/lattice-audit-bin/govulncheck ./...` | full call graph | **No vulnerabilities found** | needs network DB; stdlib-only graph |
| go vet | go1.27.1 | `go vet ./...` | all pkgs | clean, 0 issues | compiler checks only |
| golangci-lint | 2.13.2 | `golangci-lint run ./...` | default linters | **0 issues** | no gosec in default set |
| golangci+gosec | 2.13.2 | `run --enable gosec --tests=false ./...` | prod code | 66 G115 + 3 G304 + 11 `unused` | G115/G304 mostly noise (see below); `unused` are test seams |
| go test | go1.27.1 | `go test -count=1 ./...` | all pkgs | **PASS** | — |
| go test -race | go1.27.1 | `go test -count=1 -race ./...` | all pkgs | **PASS, 0 races** | race detector is necessary, not sufficient (manual review done, §15) |
| osv-scanner / staticcheck / trivy / grype / syft / nancy | — | — | — | **not installed; NOT executed** (not claimed) | recorded as unavailable, not as clean |

**gosec triage** (manual, not scanner-trusting):

* G115 `byte(v>>N)` in `binary/endian.go`, `uint32(xorshifted)` in `memtable/random.go`,
  `uint32(len(...))` in builders: shift-and-mask serialization idioms and pre-bounded lengths;
  no attacker-controlled overflow. **False positives / hardening notes at most.**
* G115 `int(filterHandle.Size)`-class casts in `table_reader.go`: now guarded by
  `MaxIndexBlockSize`/`MaxDataBlockSize`/`MaxBitsetBytes+13` + `math.MaxInt` checks. OK.
* G304 `os.ReadFile` in `security/audit+model` (offline audit harness reading its scan roots),
  `os.Open(path/dirPath)` in `table_reader.go:45` / `table_writer.go` dir-sync: engine-configured
  paths, not CLI/network input (see SEC-002/SEC-004 for the residual local-symlink nuances).
* `unused` (node accessors, `searchNode`, `validateStructure`, test sync-fault seams): dead code,
  not a vulnerability; retained seams are test-only.

---

## 7. Supply-Chain Review

* **Authenticity**: zero external modules → nothing to typosquat, confuse, or poison; `go.sum`
  absent by construction; `go mod verify` clean.
* **Build hooks**: no Makefile, no shell scripts, no Dockerfiles, no CI workflows (`.github/`
  absent), no `go:generate` directives found. No `curl|sh`, no downloads, no external executables.
* **Execution**: no `os/exec`/`exec.Command` in production code (only `internal/security/rules`
  detector + `testdata` fixture); `syscall` only `Fdatasync` in `internal/wal/sync_linux.go`;
  no `net/` listeners/dialers in production; no `WriteFile`/`RemoveAll`/`Rename`/`Symlink` in
  production (test-only).
* **Verdict**: **SECURE / HERMETIC**. Attack surface is the host Go compiler (`go1.27.1`) itself.
  Residual: toolchain integrity is out of repo control (standard `GOTOOLCHAIN`/host trust
  assumption, noted, not a finding).

---

## 8. Threat Model (current, implemented code only)

```text
[Persistence attacker: crafted WAL/SSTable/MetaIndex/filter bytes + valid CRC32]
        │  (local file write, disk corruption w/ recomputed CRC, malicious fixture)
        ▼
[TRUST BOUNDARY: storage decode] binary/varint → WAL record → block/index/footer/
        MetaIndex/filter decoders → fail-closed errors (SEC-001 breaks this: panic)
        ▼
[Process core] MemTable/SkipList (API-caller keys/values, concurrent readers)
        ▼
[TRUST BOUNDARY: filesystem publish] staging → os.Link → dir sync (TOCTOU/symlink/perms)
        ▼
[Local host users] 0600/0700 files, symlink races, descriptor/special-file tricks
```

Explicitly **not** threat-modeled as current (no code): network frames, Raft peers, CLI
injection, multi-tenant auth. Future requirements in §20.

Attacker capabilities assumed: (a) supply arbitrary bytes to any decoder with correctly
recomputed CRC32 (CRC is malleable, non-cryptographic); (b) local unprivileged filesystem
maneuvers (symlink pre-creation, FIFO/special files, concurrent destination creation);
(c) API-caller volume (many/large keys/values). Capability (a) is the standard used for
SEC-001; random (non-adversarial) corruption is already fail-closed via CRC mismatch.

---

## 9. Attack-Surface Matrix

| Attack surface | Phase | Attacker control | Impact | Existing defense | Remaining risk |
| -------------- | ----: | ---------------- | ------ | ---------------- | -------------- |
| WAL record decode | P02 | persistence bytes+CRC | OOM/panic/corrupt replay | key≤64 KiB,val≤4 MiB caps pre-alloc; trunc→UnexpectedEOF; CRC fail-closed | none found (OK) |
| WAL files/rotation/recovery | P02 | local fs + seg bytes | overwrite/replay/gap/seq skew | O_EXCL/O_APPEND/0600, Lstat+SameFile, read-only history, gap/dup/seq enforcement | stale-active on close-fail (SEC-008 info) |
| MemTable/SkipList | P03 | API keys/values/volume | unbounded RAM; ordering corruption | ValidateKey/Value, defensive copies, frozen gate, bottom-up atomic publish | **no byte/entry cap (SEC-003)** |
| SSTable writer/staging | P04 | local fs (parent/symlink) | hijack/overwrite/perms | Lstat dst-exists, CreateTemp O_EXCL, SameFile, Link-only+EEXIST, 0600 check | **parent unpinned (SEC-002)** |
| SSTable reader (footer/index/block) | P04 | persistence bytes+CRC | OOM/panic/misdirected read | magic/padding, ValidateAgainstFileSize, 8 MiB caps, arch checks, CRC, monotonic offsets, key ordering | symlink-follow asymmetry (SEC-004) |
| MetaIndex decode | P04/05 | persistence bytes+CRC | **process panic (DoS)** | CRC, offsets monotonic/bounded, handle Validate | **keyLen wrap panic (SEC-001 HIGH)** |
| Bloom filter blocks | P05 | persistence bytes+CRC | OOM/panic/false-negative reads | 256 MiB+13 pre-alloc cap, CRC, k==7, bitCount↔bytes match, owned copy | residual large-but-legal alloc (hardening) |
| Filter builder (write path) | P05 | caller misuse | silent key omission | **remediated**: AddKey error propagates (verified) | none (regression test present) |
| Binary primitives | P00/01 | bytes via all above | inherited parser flaws | trunc/overflow guards, no alloc on decode | contract-panics on misuse (hardening) |
| Filesystem publication | P04 | local symlink/TOCTOU | arbitrary overwrite | os.Link no-rename + EEXIST + cleanup | parent-dir gap (SEC-002) |
| Dependencies/CI/build | — | supply chain | poisoning/backdoor | zero deps, no scripts/CI/actions | toolchain-host trust only |
| Logging/diagnostics | P00 | logged keys/values | secret disclosure | sensitive-key precedence, len-only errors, safe String() | keyword/recursion gaps (SEC-006), MaskSecret leak (SEC-005) |
| CLI/network/Raft | future | none (stubs) | — | — | future surface (§20) |

---

## 10. Phase-by-Phase Security Review

### P00 — Foundations, errors, logger. Verdict: **Good**

* `internal/errors`: typed sentinels, correct `Is/As` chains (`WALWriterPoisonedError.Unwrap`),
  `safeUint32` clamping, length-only messages (no raw key/value bytes). OK.
* `internal/logger`: `slog` wrapper, sensitive-key precedence over hostile `Redactable`,
  typed-nil + panic recovery in `safeRedact`. Gaps → SEC-006 (missing stems:
  `pass/pwd/passphrase`, `cookie/set-cookie`, `session*`, `ssn`, `card/cvv/cvc`,
  `mnemonic/seed`, `salt/nonce`, `*_key` generics; no recursion into groups/nested
  values/`msg`/error strings). Heuristic limitation is documented in known-limitations #10;
  residual is real but bounded (LOW).
* Lint gates present and passing. `security` AST engine (SEC-01/02) scans clean on the repo
  (1 informational inventory finding, consistent with hermetic graph).

### P01 — Binary primitives. Verdict: **Good (hardening notes only)**

* `varint.go:64-101`: empty→Truncated, 10-iteration bound, 10th-byte `b>1`→Overflow. No
  alloc/index. Non-canonical acceptance is a documented limitation (#7), not a flaw.
* `validate.go`: pure length checks; `safeUint32` prevents truncation confusion.
* `endian.go`: `Put*/Get*` panic on short buffers **by contract** (`_ = buf[N]`); all in-scope
  callers pre-size exactly (WAL 21 B header, 48 B footer, 16 B handles). Future-callers risk →
  hardening note (fallible `TryGet*` suggested, P3).
* `crc.go`: pure IEEE, zero misuse as MAC in-scope; must stay non-auth (P06+ note).
* `internalkey.go`: `DecodeInternalKey:178-195` checks `len≥10` before slicing, validates key
  before `make` (≤65,535). `NewInternalKey`/`DecodeInternalKey` copy; struct-literal borrowing
  is a documented convention risk (limitation #8/#16) enforced at entry points. OK.
* Integer boundaries reasoned safe; gosec G115 byte-cast noise confirmed benign.

### P02 — WAL. Verdict: **Strong**

* Record codec (`record.go:380-504`): `keyLen uint16` + `PUT/DELETE≠0/BATCH==0` rules before
  `make`; `valLen` checked against 4 MiB **before** allocation (malicious `0xFFFFFFFF` →
  `ValueTooLarge`, no 4 GiB alloc); header `ReadFull` distinguishes `EOF@0` vs torn header;
  CRC streamed and verified; `recSize` bounded ≈4.2 MiB. No unbounded `make`, no swallow.
* Writer (`writer.go:80-215,362-404`): symlink/dir/non-regular reject, `O_APPEND`/`O_EXCL`,
  0600, `SameFile` pin, poison-on-sync-failure, short-write handling. OK.
* Reader (`reader.go:53-181`): symlink/dir/non-regular reject, `SameFile` pin, `Next`
  delegates bounds to `DecodeRecord`, offset arithmetic ≤ ~4 MiB. OK.
* Rotation/sequencing (`rotation.go`): strict `ParseSegmentID` (affix/digits/no-leading-zero/
  `ParseUint64`/nonzero), `MaxUint64` overflow guard, fail-closed `active=nil` on create-fail.
  Nit: `activeLen+recWireSize` wrap only at ~9 EB (unreachable; P3 note). Close-failure path →
  SEC-008 (info).
* Recovery/coordinator: latest-segment-only truncation, read-only history, gap/duplicate/
  sequence-monotonicity enforcement; quiescence assumption documented (limitation #11). OK.
* Queue/runner: `make` sites caller-bounded (`capacity`, `min(count,1024)`); modulo after
  zero-guards; single prod `go func` (`runner.go:145`) with `stopCh/doneCh` + drain; no
  `WaitGroup/Once` misuse. OK.

### P03 — MemTable/SkipList. Verdict: **Moderate (SEC-003)**

* Lock-free readers (`atomic.Pointer`, bottom-up L0→H publish, clamped height) + serialized
  writer mutex; freeze fast-path + under-lock recheck; duplicate-swap via atomic container +
  defensive copies (`node.go`, `iterator.go` `Key()/Value()` copies; `rawValue` unexported).
  Zero races under `-race` (254 s suite). OK.
* **SEC-003**: `Insert` enforces only per-record bounds; **no table-level entry/byte ceiling**.
  Sustained writes grow heap without bound until an external flusher (Phase 06/10) intervenes.
  In the current single-process library this is API-caller-controlled (MEDIUM, not remotely
  triggerable today); it becomes P0-adjacent once networking exists.
* PCG32: mutex-guarded, `crypto/rand` seed, key-independent, 15-iteration bound; `UnixNano`
  fallback + fixed stream note as non-issue for non-crypto heights. `size.go` saturating
  add/sub prevents wrap but masks violations (P3 observability note).

### P04 — SSTable. Verdict: **Moderate (SEC-001 in MetaIndex; SEC-002/004 in fs)**

* Writer happy-path hardening verified: `Lstat` dst-exists reject, same-dir `CreateTemp`
  `O_EXCL`, fd `Stat` regular + `Perm&0077` + `Lstat/SameFile` anti-swap, `os.Link` publish
  with `EEXIST→ErrSSTableExists`, explicit **no `Rename` fallback** (comment + test), dir
  sync, per-error-path staging cleanup. Index `keyLen` bounded pre-arithmetic (good).
* Reader happy-path hardening verified: footer magic/padding, `ValidateAgainstFileSize`
  (8 MiB caps both handles), data/index/filter handle arch + overlap checks, block CRC +
  restart monotonicity + prefix-reconstruction + key-order + value-cap enforcement, owned
  value copies, `readExactAt` EOF discipline, FD-close-on-failure, `RWMutex` + `ReadAt`
  concurrency, `IsRegular` gate (P05 REMED intact).
* **SEC-001** (MetaIndex, §12) breaks the "corruption fails closed" invariant with a panic.
* **SEC-002/SEC-004** filesystem asymmetries (§12).

### P05 — Bloom/filter. Verdict: **Good post-REMED (SEC-001 adjacent)**

* `OptimalBitsetSize` overflow guard; `NewBloomFilter` nil on `<0`/`>MaxKeyCount`;
  `Add/MayContain` nil/zero guards + defense-in-depth `byteIdx<len`; modular double-hashing
  without pre-modulo overflow; `Probes` diagnostic-only. Murmur3 matches reference
  (constants/rotations/LE-tail/fmix verified by reading).
* `Encode/DecodeFilterBlock`: 13 B floor, 256 MiB+13 ceiling **before allocation**, CRC-then-
  `k==7`-then-`bitCount`-then-`bytes==ceil(bitCount/8)` order, owned copy. OK.
* `ReadFilterBlock`: upper bound + arch checks **before** `make` (REMED verified at
  `table_reader.go:612-626`); meta alloc bounded by 8 MiB footer cap. OK.
* `TableWriter.Add:360-364`: `AddKey` error now propagates fail-closed (REMED verified).
* Saturation/FPR: fixed 10 bits/key + 7 probes; `M>>N` degrades to 100 % FPR (read amplification,
  no corruption) — operational sizing concern for the Phase 06 flush manager, not a vuln.
  Deterministic-population FPR basis accepted; adversarial hash-concentration would raise FPR
  (availability tilt) but cannot cause false negatives through the reviewed code.
* `ReadFilterBlock` re-reads + reallocates per call (no cache): perf note (P3), not security.

---

## 11. Filesystem Security

Enumerated prod ops: `Mkdir(All)` `table_writer.go:182` (+WAL `dir.go:80` strict `Mkdir`);
`CreateTemp` `:189`; `Lstat` `:161,:222`; `Stat` `:205`; `SameFile` `:228`; `Link` `:616`;
`Remove` (staging cleanup) `:199,:208,:218,:230, cleanupStaging`; `Open` `table_reader.go:45`,
dir-sync `Open` `:786`; `Stat` `:77`; `ReadAt`; WAL `OpenFile(O_EXCL|O_APPEND|O_RDONLY)`,
`ReadDir`, `Join+Clean`. No prod `WriteFile/RemoveAll/Rename/Symlink`.

* **Path traversal**: `dstPath` parent via `filepath.Dir` (no `Clean` call, no component
  whitelist); file numbers are caller `uint64`-formatted elsewhere per threat model, but the
  writer accepts arbitrary `dstPath` strings from the embedding application. Absolute paths
  and `..` are passed through to `MkdirAll`+`CreateTemp`+`Link`. In a library context the
  caller is trusted with its own paths; the gap matters if Phase 06+/CLI ever derive paths
  from less-trusted input → SEC-002 (MEDIUM, defense-in-depth with concrete local impact).
* **Symlink**: staging fd + `Lstat/SameFile` check is solid **for the file**; the **parent
  directory** is never `Lstat`ed/pinned, so a swapped parent symlink redirects staging +
  publication into an attacker directory (WAL `dir.go` pins; writer does not). → SEC-002.
* **TOCTOU**: dst `Lstat`-exists check → later `Link(EEXIST)` is atomic-fail-closed (correct
  check+enforce pattern, no Rename). `Stat`→`ReadAt` in reader has no pin, but content is
  fully revalidated; only residual is special-file blocking → SEC-004 (LOW).
* **Arbitrary write/delete**: confined to caller-supplied `dstPath` + same-dir staging +
  staging cleanup; no attacker-controlled absolute-target primitive beyond the caller's own
  path argument. No finding beyond SEC-002.
* **Permissions**: default 0600/0700; custom modes pass `ValidateFileMode` (rejects group/other
  + exec bits); staging fd `Perm&0077` enforced post-create (umask-safe for the file, though
  pre-existing parents are never tightened — SEC-002 note).

---

## 12. Detailed Findings

### SEC-001 — Panic via MetaIndex keyLen integer wrap (crafted SSTable → process crash)

```text
Title: Unbounded MetaIndex keyLen wraps length check and panics on slice
Severity: HIGH
Confidence: CONFIRMED (arithmetic + PoC reproduced, path traced)
Category: VULNERABILITY (attacker-triggerable panic / DoS)
Phase Introduced: Phase 04 (MetaIndex codec; still present at HEAD)
Current Component: internal/sstable MetaIndex decode (filter-read path)
Affected File: internal/sstable/meta_index.go
Affected Line(s): 168-181 (varint decode 168; wrapped add 174; equality 175; slicing 180-183)

Attacker Capability: write (or replace) an SSTable/MetaIndex byte stream and compute CRC32-IEEE
  (public, malleable — no secret). No code execution, no privileges needed beyond file delivery.
Attack Preconditions: (1) victim opens the crafted table and calls ReadFilterBlock (or any
  DecodeMetaIndexBlock caller); (2) MetaIndex block carries valid CRC32 over attacker bytes.
Trust Boundary: persistent-storage decode (every SSTable byte untrusted).
Root Cause: keyLen (uint64, up to 2^64-1 from GetVarint64) is added without bounds:
  expectedLen := uint64(varintLen) + keyLen + BlockHandleSize  // wraps mod 2^64
  then compared for equality against len(entrySlice), then used as int(keyLen) for slicing.
  No keyLen ceiling (unlike DecodeBlockIndex index_builder.go:456 which rejects
  keyLen==0 || keyLen>MaxEncodedInternalKeyLen before arithmetic).
Attack Path:
  ReadFilterBlock (table_reader.go:600) → FindMetaIndexEntry → DecodeMetaIndexBlock →
  entry offsets validate (offsets[0]==0, monotonic, in-region) → keyLen=2^64-6 (10-byte varint)
  → expectedLen = 10+(2^64-6)+16 ≡ 20 → equals len(entrySlice)=20 → keyEnd = 10+int(2^64-6) = 4
  → entrySlice[10:4] → runtime panic: slice bounds out of range [10:4].
Security Impact: database process crash (denial of service) on filter lookup of a crafted table.
  Random corruption still fails closed at CRC; only an adversary who recomputes CRC triggers it —
  exactly the in-scope adversarial-storage model. No memory disclosure; no corruption (crash only).
Exploitability: realistic for a local/persistence attacker (malicious SSTable dropped into data
  dir, compromised volume/snapshot, hostile test fixture, future untrusted replication peer).
  Not remotely triggerable today (no network listener exists).
Existing Mitigations: CRC32 gate; offset monotonicity/bounds; BlockHandle.Validate;
  ReadFilterBlock filter-handle bound (SEC-P05-01 REMED).
Why Mitigation Is Insufficient: CRC is non-auth (attacker recomputes); offset checks do not
  constrain keyLen; the wrapped equality is the very check meant to stop it; sibling
  index decoder has the bound but MetaIndex was missed.
Evidence: PoC (/tmp/lattice-audit-poc, outside repo) replicating lines 168-181 exactly:
  varintLen=10, keyLen=18446744073709551610, entryLen=20 → expectedLen(wrapped)=20 == 20 →
  keyStart=10 keyEnd=4 → PANIC REPRODUCED: slice bounds out of range [10:4]. Code read confirms
  identical types/ops in repo; 32-byte block layout passes all preceding gates (count=1,
  offsets=[0], CRC valid by construction).
Recommended Remediation: before line 174, reject keyLen==0 || keyLen > <small bound>
  (e.g. 1<<20, MetaIndex keys are short ASCII like "filter.bloom") and compute with
  overflow-checked addition (keyLen > uint64(len(entrySlice)) → corrupt); mirror
  index_builder.go:456-458. Add regression test with the PoC vector (valid CRC, keyLen=2^64-6).
Suggested Phase/Owner: P1, before Phase 06 (VersionSet will fan filter reads across tables).

Remediation Status: REMEDIATED (SEC-001 / F-001)
Remediation Date: 2026-09-13
Remediation Scope: internal/sstable/meta_index.go (DecodeMetaIndexBlock)
Remediation Strategy:
  1. Reject keyLen == 0 || keyLen > binary.MaxEncodedInternalKeyLen immediately upon varint decode.
  2. Enforce minimum entry length: len(entrySlice) >= minEntryLen (varintLen + BlockHandleSize).
  3. Validate key length using overflow-safe subtraction bounds: keyLen == uint64(len(entrySlice) - minEntryLen).
  4. Perform bounded int(keyLen) conversion strictly after bounds validation.
  5. Fail closed with *errors.IndexBlockCorruptedError without panic.
Verification & Traceability Evidence:
  - Targeted Regression: TestSecurity_Remediation_SEC_001_IntegerOverflowPanicPoC (PoC returns ErrIndexBlockCorrupted, no panic)
  - Boundary Matrix: TestSecurity_Remediation_SEC_001_BoundaryMatrix (keyLen=0, MaxUint64, MaxUint64-1, MaxUint64-6, MaxUint64-7, edge sizes)
  - Mutation Testing: TestSecurity_Remediation_SEC_001_MutationTesting (byte-level corruptions fail closed)
  - Higher-Level Reader: TestSecurity_Remediation_SEC_001_TableReader_ReadFilterBlockPath (TableReader fails closed)
  - Fuzzing: FuzzMetaIndexBlock_Decode (7,349,042 raw-byte executions, 0 crashes)
  - Full Suite: go test ./... (PASS), go test -race ./... (PASS, 0 races)
  - Static Analysis: go vet ./... (clean), golangci-lint run ./... (0 issues), go mod verify (clean)
```

### SEC-002 — SSTable staging parent unpinned; WithFile bypasses staging checks

```text
Title: TableWriter parent directory not pinned; WithFile constructor skips file checks
Severity: MEDIUM
Confidence: STRONG
Category: SECURITY WEAKNESS (local filesystem)
Phase Introduced: Phase 04
Current Component: internal/sstable TableWriter construction
Affected File: internal/sstable/table_writer.go
Affected Line(s): 181-184 (MkdirAll parent, no Lstat/pin); 189 (CreateTemp follows parent);
  259-301 (NewTableWriterWithFile: no Stat/Perm/symlink checks)

Attacker Capability: local unprivileged user able to replace/plant a symlink at the parent
  path component between/before writer setup; or caller passing an attacker-influenced dstPath.
Attack Preconditions: parent component attacker-writable or pre-planted symlink; writer
  invoked with dstPath under it.
Trust Boundary: host filesystem (multi-user host, shared /tmp-style data roots).
Root Cause: staging hardens the FILE (O_EXCL, SameFile, Link-only) but trusts the DIRECTORY:
  MkdirAll(parentDir, 0700) follows symlinks, never Lstat-verifies parent identity, never
  tightens pre-existing parent modes (umask/existing perms inherited).
Attack Path: attacker swaps parent → writer's CreateTemp + Link + dir-sync all occur inside
  attacker directory → quota/visibility tampering, misleading durability, potential
  cross-user disclosure of table bytes staged under attacker-observable tree.
Security Impact: bounded local integrity/visibility issue; no overwrite of arbitrary victims
  (Link target is still caller's dstPath), no RCE.
Exploitability: moderate; requires local fs positioning. Unreachable remotely today.
Existing Mitigations: dst Lstat-exists gate; O_EXCL staging; SameFile; Link+EEXIST; 0600 file check.
Why Mitigation Is Insufficient: all mitigations anchor to the file, none to the directory.
Evidence: code read of 181-232 vs WAL dir.go:80-158 (which pins); no parent Lstat/SameFile
  anywhere in sstable writer path (grep).
Recommended Remediation: Lstat each parent component (or open parent with O_DIRECTORY|O_NOFOLLOW
  and pin via SameFile/fstat), reject symlink parents, chmod-tighten created parents to 0700
  regardless of umask; document WithFile as test-only or replicate checks.
Suggested Phase/Owner: P1 (before CLI/engine derive paths from less-trusted input).
```

### SEC-003 — MemTable has no table-level memory ceiling

```text
Title: Unbounded MemTable heap growth (accounting without enforcement)
Severity: MEDIUM
Confidence: STRONG
Category: SECURITY WEAKNESS (resource exhaustion)
Phase Introduced: Phase 03
Current Component: internal/memtable SkipList.Insert
Affected File: internal/memtable/skiplist.go
Affected Line(s): 181-208 (per-record validation only); size.go accounting; no maxEntries/maxBytes

Attacker Capability: API caller (today) / network client (future) issuing many/large writes.
Attack Preconditions: sustained writes without a flush; no active-table rotation exists yet.
Trust Boundary: application-write API → process heap.
Root Cause: ByteSize() is exact accounting, not a limit; no insert path rejects on table size.
Attack Path: loop of 4 MiB values (each individually legal) → heap grows until OOM-killer.
Security Impact: local/API-driven OOM DoS; no corruption (writes fail only when allocator does).
Exploitability: trivial for anyone holding the write API; not remotely reachable today.
Existing Mitigations: per-key (64 KiB) / per-value (4 MiB) caps; exact ByteSize for future triggers.
Why Mitigation Is Insufficient: caps bound records, not the table.
Evidence: Insert validates key/OpType/value then splices; no size gate; known-limitations #17
  acknowledges single-table unboundedness implicitly via flush deferral.
Recommended Remediation: Phase 06/10: enforce active-table byte + entry ceilings at Insert
  (return ErrMemTableFull → freeze/rotate), wire flush manager to ByteSize().
Suggested Phase/Owner: P1 (must exist before network write path in Phase 11).
```

### SEC-004 — TableReader follows symlinks (asymmetry with WAL reader)

```text
Title: NewTableReader opens path without Lstat/SameFile symlink pinning
Severity: LOW
Confidence: CONFIRMED
Category: SECURITY WEAKNESS
Phase Introduced: Phase 04
Current Component: internal/sstable TableReader open
Affected File: internal/sstable/table_reader.go
Affected Line(s): 41-50 (os.Open follows symlinks); contrast internal/wal/reader.go:65-102

Attacker Capability: local user swapping a table path symlink between open and read.
Attack Preconditions: attacker write access to the path component hosting the table symlink.
Trust Boundary: host filesystem.
Root Cause: no Lstat symlink/dir/non-regular pre-check, no SameFile descriptor pin.
Attack Path: symlink → special file would already be rejected by IsRegular post-Stat (REMED);
  remaining: symlink swap to a DIFFERENT valid SSTable (wrong-table read) or FIFO timing games
  (largely neutralized by IsRegular + full content revalidation).
Security Impact: wrong-table content served under expected path; no overwrite, no escalation.
Exploitability: low; content validation (magic/CRC/handles) still fully applies.
Existing Mitigations: IsRegular gate; footer/handle/CRC validation; RLock + positional ReadAt.
Why Mitigation Is Insufficient: identity (which file) is unpinned even though content is validated.
Evidence: code comparison reader-vs-WAL; no Lstat/SameFile in sstable reader path.
Recommended Remediation: mirror WAL: Lstat → reject symlink → Open → fstat → SameFile → IsRegular.
Suggested Phase/Owner: P2.
```

### SEC-005 — MaskSecret leaks key prefix/suffix for secrets longer than 8 chars

```text
Title: Partial-secret disclosure in audit finding redaction
Severity: LOW
Confidence: CONFIRMED
Category: SECURITY WEAKNESS (information disclosure, offline surface)
Phase Introduced: Phase 00 (SEC-01/02 foundations)
Current Component: internal/security/model finding redaction
Affected File: internal/security/model/finding.go
Affected Line(s): 59-75 (prefix s[:3] + suffix s[n-2:] emitted for n>8)

Attacker Capability: reader of audit reports/stdout/logs containing masked secrets.
Attack Preconditions: a real secret flows through MaskSecret (offline audit harness context).
Trust Boundary: diagnostic output → report consumers.
Root Cause: `prefix := s[:3]; suffix := s[n-2:]` aids low-entropy token brute-force.
Attack Path: collect masked tokens → reduce search space (5 known chars + length + sha256 short prefix).
Security Impact: low (3-byte sha prefix + 5 chars rarely decisive; but strictly unnecessary).
Exploitability: low; no evidence prod storage secrets traverse this path (audit harness only).
Existing Mitigations: full opaque branch for n≤8; sha prefix truncated to 3 bytes.
Why Mitigation Is Insufficient: the n>8 branch still emits raw secret characters.
Evidence: direct code read; no prod caller handling live credentials found (testdata uses synthetic).
Recommended Remediation: use the opaque `***[REDACTED len= sha256=]***` form for ALL lengths.
Suggested Phase/Owner: P2.
```

### SEC-006 — Logger redaction: keyword + recursion gaps

```text
Title: Sensitive-key set misses common stems; nested/group values never redacted
Severity: LOW
Confidence: STRONG
Category: SECURITY WEAKNESS (heuristic disclosure boundary)
Phase Introduced: Phase 00
Current Component: internal/logger
Affected File: internal/logger/logger.go
Affected Line(s): 96-110 (key set); 322-354 (top-level Attr.Key only; groups/msg/errors uninspected)

Attacker Capability: none (accidental disclosure, not attacker-driven).
Attack Preconditions: developer logs a secret under an uncovered key (pass/pwd/passphrase,
  cookie/set-cookie, session*, ssn, card/cvv/cvc, mnemonic/seed, salt/nonce, *_key generics)
  or nested in a Group/map/slice/struct, msg string, or error value.
Trust Boundary: application logging → files/aggregators.
Root Cause: O(1) top-level keyword match by design (throughput), no recursion, no value sniffing.
Attack Path: Info("login", "passwd", pw) [covered] vs Info("login", "pass", pw) [NOT covered].
Security Impact: bounded accidental disclosure; documented limitation #10 already discloses the
  heuristic nature; sensitive-key precedence + Redactable + RedactedKeys mitigate the known paths.
Exploitability: requires developer misuse; no prod log call sites emitting secrets found.
Existing Mitigations: precedence over hostile Redactable; typed-nil/panic-safe eval; len-only
  domain String() methods; Config.RedactedKeys extension point.
Why Mitigation Is Insufficient: allowlist is narrow and shallow.
Evidence: key-set read; handler ReplaceAttr operates per top-level Attr; tests cover only flat attrs.
Recommended Remediation: extend stems (pass/pwd/session/cookie/ssn/card/mnemonic/salt/nonce/*key),
  document Group/msg/error non-coverage, add opt-in recursive scrubber for diagnostic builds (P3).
Suggested Phase/Owner: P2/P3.
```

### SEC-007 — Declared `go 1.22.0` floor generates scanner advisory noise

```text
Title: go.mod language floor predates patched stdlib advisories
Severity: INFORMATIONAL
Confidence: CONFIRMED
Category: HARDENING OPPORTUNITY (supply-chain hygiene)
Phase Introduced: Phase 00
Current Component: build
Affected File: go.mod:3

Attacker Capability: none (compliance-scanner confusion only).
Attack Preconditions: scanner reads go directive as effective stdlib version.
Trust Boundary: CI/compliance pipeline.
Root Cause: `go 1.22.0` directive vs `go1.27.1` actual compiler.
Attack Path: N/A. govulncheck call-graph analysis: No vulnerabilities found.
Security Impact: none demonstrated; noise risks masking future real advisories.
Existing Mitigations: zero external deps; hermetic build; govulncheck clean.
Recommended Remediation: bump directive to a supported floor (e.g. go 1.24/1.25) at next
  scheduled build maintenance (NOT done in this audit per no-modify rule).
Suggested Phase/Owner: P3.
```

### SEC-008 — WAL rotation close-failure leaves stale active writer

```text
Title: rotateLocked returns on oldWriter.Close() error without clearing rw.active
Severity: INFORMATIONAL
Confidence: STRONG
Category: HARDENING OPPORTUNITY (fail-closed completeness)
Phase Introduced: Phase 02
Current Component: internal/wal rotation
Affected File: internal/wal/rotation.go
Affected Line(s): 473-477 (return before active transition; active still points at close-failed writer)

Attacker Capability: none directly (I/O-fault conditions only).
Attack Preconditions: Close() of sealed segment fails (disk error, fault injection).
Trust Boundary: local durability path.
Root Cause: only the create-failure branch (485) nils active; close-failure branch does not.
Attack Path: subsequent appends hit a closed/failed writer → ErrWriterClosed (fail-closed in
  effect, but activeID is stale and diagnostics mislead).
Security Impact: negligible availability confusion; no corruption (writer is sealed/closed).
Existing Mitigations: error propagated; writer Close idempotent; poison state machine.
Recommended Remediation: nil active + mark poisoned on close-failure too; test both branches.
Suggested Phase/Owner: P3.
```

### SEC-009 — Hardening bundle (contract panics, saturation, DebugString, docs drift)

```text
Title: Defense-in-depth notes: intentional panics, saturating counters, raw debug formatter
Severity: INFORMATIONAL
Confidence: CONFIRMED (code-read)
Category: HARDENING OPPORTUNITY
Phase Introduced: Phases 00-03
Current Components: binary/wal/memtable
Affected Files/Lines:
  internal/binary/endian.go:14,28,43,59,74,94 + internal/binary/varint.go:39 +
    internal/wal/record.go:134 (fail-fast Put* panics on short buffers by contract);
  internal/memtable/size.go:78-108 (safeAdd/safeSub saturate at MaxUint64/0);
  internal/binary/internalkey.go:77-91 (DebugString exposes raw UserKey by design);
  README.md:84-87 (P02-P05 checkboxes unchecked despite implemented+audited code)

Attacker Capability: none today (all panic sites fed by exact-size in-scope callers).
Attack Preconditions: a FUTURE caller passing untrusted lengths to Put* without pre-check.
Trust Boundary: internal API misuse boundary.
Root Cause: performance-oriented contracts (zero-alloc, no error return) + silent saturation
  + intentional forensic formatter + stale roadmap checkboxes.
Security Impact: none currently; latent panic-DoS if contracts are violated later.
Existing Mitigations: exact-size callers verified; saturation prevents wrap; DebugString has no
  prod log call sites; docs drift affects scoping, not code.
Recommended Remediation (P3): fallible TryGet* variants for untrusted paths; saturating
  counters should also emit/log a violation signal; lint-ban DebugString in log/error paths;
  tick README roadmap to match audited reality.
Suggested Phase/Owner: P3.
```

---

## 13. Resource-Exhaustion Matrix

| Resource | Component | Trigger | Bound | Exploitability | Severity |
| -------- | --------- | ------- | ----- | -------------- | -------- |
| RAM | MetaIndex decode | crafted keyLen (SEC-001) | none pre-panic (panics first) | local/persistence | HIGH (DoS) |
| RAM | filter block read | large-but-legal handle ≤256 MiB+13 | 256 MiB+13 + arch checks | local/persistence | LOW (bounded, single alloc) |
| RAM | data/index block read | malformed handle | 8 MiB caps + arch checks | local/persistence | LOW |
| RAM | WAL record decode | huge len prefix | 64 KiB key / 4 MiB value pre-alloc | persistence bytes | LOW |
| RAM | MemTable | many/large writes (SEC-003) | **none** (accounting only) | API caller | MEDIUM |
| RAM | varint decode | 10-byte varints | 10-iteration bound, no alloc | any bytes | none |
| CPU | Bloom probes | any key | O(k=7) + O(1) mods | any caller | LOW |
| CPU | SkipList height | entropy draw | MaxHeight 16, 15-iter bound | N/A (key-independent PCG) | LOW |
| CPU | block restart scan | degenerate keys | restart interval 16 | persistence bytes | LOW |
| CPU | CRC32 | large blocks | linear in bounded block sizes | persistence bytes | LOW |
| CPU | recovery replay | many segments | streaming, fail-closed | persistence | LOW |
| Disk | staging files | crashed writers | per-path cleanup; crash-window orphans (no sweeper) | local | LOW |
| Disk | WAL growth | unrotated appends | rotation + sequencing | API caller | LOW |
| FD | readers/writers | repeated open→error | close-on-all-error-paths verified | any caller | LOW |
| Goroutines | group-commit runner | `go run()` lifecycle | stopCh/doneCh + drain; Stop required | operator misuse | LOW |
| Channels | commit queue | flooding | 1024-task / 64 KiB batch caps | API caller | LOW |

---

## 14. Security Control Matrix

| Security invariant | Implemented control | Test | Adequate? | Gap |
| ------------------ | ------------------- | ---- | --------- | --- |
| WAL CRC + truncation rejection | streaming CRC, ReadFull discipline | corruption/recovery/fuzz suites | yes | — |
| WAL symlink/perm/TOCTOU | O_EXCL/O_APPEND/0600, Lstat+SameFile | sec03_* tests | yes | — |
| WAL torn-tail | latest-only truncation | recovery tests | yes | — |
| WAL seq monotonicity | coordinator enforcement | coordinator tests | yes | — |
| Key/value bounds | ValidateKey/Value at all ingest points | validate + writer tests | yes | — |
| Data/index alloc caps | 8 MiB + arch checks | sec04_remediation | yes | — |
| Filter alloc cap | 256 MiB+13 + arch checks | sec05_remediation | yes | — |
| MetaIndex malformed rejection | CRC + offsets + handles | meta tests | **NO** | **SEC-001 keyLen wrap** |
| Filter false-negative freedom | AddKey err propagation | sec05_remediation | yes | — |
| Publication atomicity | os.Link, no Rename fallback | publication-race tests | yes | parent pin (SEC-002) |
| Staging symlink/perms | fd Stat + SameFile + 0600 | staging tests | partial | WithFile bypass (SEC-002) |
| Reader special-file rejection | IsRegular | sec05_remediation | partial | symlink identity (SEC-004) |
| MemTable race freedom | atomic publish + mutex writer | -race suites (0 races) | yes | — |
| MemTable growth bound | ByteSize accounting | size tests | **NO** | **SEC-003 no ceiling** |
| Freeze safety | double-checked frozen gate | freeze tests | yes | — |
| Iterator isolation | defensive copies, weak-consistency contract | iterator/fuzz tests | yes | — |
| Log redaction | precedence + len-only formats | redaction/adversarial tests | partial | SEC-005/006 |
| Error taxonomy | typed sentinels, Is/As chains | errors tests | yes | — |
| Supply chain | zero deps, no hooks | mod verify + scans | yes | — |

---

## 15. Security Test Coverage

| Security property | Existing test | Dynamic validation (`-race` + suites) | Manual review | Result |
| ----------------- | ------------- | -------------------------------------- | ------------- | ------ |
| Truncation rejection (all decoders) | varint/footer/record/block/meta/filter tests | PASS | traced | holds, except SEC-001 wrap |
| CRC fail-closed | corruption suites (WAL+SSTable+filter) | PASS | traced | holds (random); adversarial needs CRC recompute (modeled) |
| Alloc bounds (WAL/data/index/filter) | sec04/sec05 remediation | PASS | traced | holds |
| Alloc bounds (MetaIndex entries) | meta tests | PASS | traced | **fails (SEC-001)** |
| Integer overflow (seq/segment/offset) | overflow guard tests | PASS | traced | holds (EB-scale nits only) |
| Publication safety | race + no-rename-fallback tests | PASS | traced | holds for file; parent gap noted |
| Symlink defense (WAL) | sec03_symlink | PASS | traced | holds |
| Symlink defense (SSTable reader) | sec05 non-regular test | PASS | traced | partial (SEC-004) |
| Concurrency | concurrent + fuzz + invariants suites | **0 races** | lock-order/goroutine/FD review | holds |
| Filter FPR + zero false negatives | 1 M empirical harness (0.8186 %, CI-bounded) | PASS | reasoned | holds for measured population |
| Permissions | 0600/0700 + insecure-mode rejection | PASS | traced | holds for files; parents noted |
| Redaction | precedence/panic/concurrent tests | PASS | traced | partial (SEC-005/006) |
| Fuzz coverage | varint/record/block/footer/filter/skiplist fuzz | PASS (no crashes reported) | — | **add SEC-001 vector** |

---

## 16. Dependency Verdict

```text
Known vulnerable dependencies:       0 (govulncheck: No vulnerabilities found)
Reachable vulnerable dependencies:   0
Unreachable vulnerable dependencies: 0 (no third-party modules at all; stdlib compiled
                                     from go1.27.1 where historical 1.22 advisories
                                     are remediated)
Supply-chain weaknesses:             none in repo (hermetic; no scripts/CI/actions/
                                     generators/downloads/exec). Residual host-toolchain
                                     trust assumption only.
Dependency upgrade requirements:     none security-driven. Optional hygiene: raise
                                     go directive floor (SEC-007, P3).
```

---

## 17. Overall Security Scorecard

| Area | Rating | Key reason |
| ---- | ------ | ---------- |
| Dependency security | Strong | zero external deps; govulncheck clean |
| Supply chain | Strong | no hooks/scripts/CI/exec/network in prod |
| Binary parsing | Good | trunc/overflow guards; one MetaIndex wrap (SEC-001) |
| WAL | Strong | caps, CRC fail-closed, symlink/perm/poisoning, quiescent recovery |
| MemTable | Moderate | race-free + aliasing discipline, but no table ceiling (SEC-003) |
| SkipList | Good | clamped height, atomic publish, freeze gate |
| SSTable writer | Good | Link-only publish, staging checks; parent pin gap (SEC-002) |
| SSTable reader | Good | caps + CRC + ordering; MetaIndex panic + symlink asymmetry |
| Filesystem | Good | 0600/0700, EEXIST-link, cleanup; directory pinning incomplete |
| Bloom filter | Good | bounded alloc, CRC, k/bitCount binding; REMEDs verified |
| Concurrency | Strong | -race clean incl. 255 s memtable suite; ordered locks; owned FDs |
| Memory/DoS | Moderate | per-record + per-block caps hold; table-level + MetaIndex gaps |
| Integrity | Good | CRC used correctly as corruption (not auth) detection everywhere |
| Secrets | Strong | no hardcoded secrets; synthetic fixtures only |
| Logging | Good | precedence redaction + len-only formats; keyword/recursion gaps |
| Build/CI | Strong | nothing to poison (no CI/scripts/Dockerfiles) |
| Test security | Good | corruption/race/fuzz/regression suites; SEC-001 vector missing |
| **Overall** | **Good (YELLOW-gated)** | one HIGH panic-DoS to fix pre-P06; fundamentals rock solid |

---

## 18. Phase 06 Gate

### YELLOW — DO NOT START PHASE 06 UNTIL SEC-001 IS REMEDIATED

```text
P0 blockers: none (no RCE / arbitrary file access / auth bypass / privilege escalation)
P1 gate:     SEC-001 (HIGH, CONFIRMED panic-DoS on crafted MetaIndex)
             + SEC-002/SEC-003 should be scheduled before network exposure (Phase 11)
```

Rationale: Phase 06 (Manifest/VersionSet/multi-table iterators) will fan filter/metadata reads
across many tables, multiplying SEC-001 exposure from "one crafted table crashes a lookup" to
"one crafted table crashes the engine". The fix is small (bound keyLen pre-arithmetic, mirror
`index_builder.go:456`) plus a regression test with the §12 vector. After remediation +
`go test -race` + rescan, posture transitions to GREEN for Phase 06 purposes. This mirrors the
Phase 05 precedent (HIGH → YELLOW → remediate → GREEN).

---

## 19. Remediation Roadmap

### P0 — Blocking: none.

### P1 — High priority (before Phase 06 code begins)

1. **SEC-001**: bound `keyLen` in `DecodeMetaIndexBlock` (reject `keyLen==0 ||
   keyLen>1<<20`-class ceiling AND `keyLen > len(entrySlice)` before the add/slice), use
   overflow-checked length math; add PoC regression test (valid-CRC `keyLen=2⁶⁴−6` vector
   must return `IndexBlockCorrupted`, not panic); run `go test -race ./...`. Why: only
   confirmed crash primitive in the tree; P06 multiplies its blast radius.
2. **SEC-003**: design active-table byte+entry ceilings + `ErrMemTableFull`→freeze/rotate
   contract (enforcement lands with the Phase 06/10 flush manager; the decision must precede
   P06 interfaces). Why: unbounded heap is the other broad-DoS primitive.
3. **SEC-002**: pin staging parent (Lstat/SameFile, 0700 tighten, reject symlink parents);
   document or constrain `NewTableWriterWithFile`. Why: closes the remaining local
   publication-hijack window before paths derive from less-trusted input.

### P2 — Medium (schedule into hardening, before Phase 11 networking)

* SEC-004 (reader Lstat+SameFile mirroring WAL), SEC-005 (opaque MaskSecret for all lengths),
  SEC-006 (redaction stem + recursion/opt-in scrubber).

### P3 — Future hardening

* SEC-007 (go directive bump), SEC-008 (rotation close-failure active-nil), SEC-009 bundle
  (TryGet* variants, saturating-counter signals, DebugString lint-ban, README checkboxes).

No remediation was implemented in this audit (audit-only rule honored).

---

## 20. Future Security Requirements (Phase 06+, not current vulnerabilities)

* **Manifest/CURRENT**: atomic publish (temp+fsync+link, dir sync), VersionEdit decode ceilings
  (file counts, key lens, level numbers) **before allocation** — apply the SEC-001 lesson:
  bound every varint-driven length pre-arithmetic, in every new codec.
* **Recovery**: fail-closed on Manifest CRC/malformed edits; never discard unknown edits silently.
* **Compaction**: atomic output publication before Manifest commit; tombstone-drop safety
  (never drop while older versions may live deeper); input/output deletion safety (remove only
  files the committed Version no longer references).
* **Cache**: isolation (no cross-table slice aliasing; owned copies on insert/return),
  size accounting with enforcement (not accounting-only — cf. SEC-003).
* **Engine**: input validation at the API boundary (key/value ceilings re-checked, not assumed
  from callers); table-count/level ceilings for multi-table iterators.
* **TCP/transport**: length-prefixed frame hard ceiling pre-allocation (e.g. 5–16 MiB per
  threat model), read deadlines, connection caps, slowloris timeouts, buffer zeroing before
  pool reuse.
* **Raft/replication**: peer authentication (mTLS or MAC — never CRC32/Murmur3), term/replay
  validation, message size ceilings, snapshot chunk bounding, treat replicated SSTables as
  adversarial input (SEC-001 class) until revalidated.

---

## 21. Audit Limitations

* Bounded audit: P0–P3 fully covered; P4 compressed; P5 future-only (≤10 %).
* No exploit was executed against the live repo (PoC replicated the exact arithmetic offline
  in `/tmp`; repo tree was never crashed or fuzzed destructively).
* `osv-scanner`/`staticcheck`/`trivy`/`grype`/`syft`/`nancy` unavailable — recorded, not claimed.
* Race-freedom is detector + manual reasoning, not proof, for all interleavings.
* FPR adversarial analysis is reasoning-based (saturation → 100 % FPR availability tilt, no
  false negatives through reviewed code), not a re-measurement campaign.
* Areas marked in §2 stand as stated; nothing is implied fully audited beyond evidence.

---

## 22. Exact Commands Executed

```text
git status
git branch --show-current
git rev-parse HEAD
git log --oneline --decorate --all --graph -40
git log --oneline --all | wc -l   (64 commits)
git remote -v
git diff --stat  (clean)
go version                                       → go1.27.1 darwin/arm64
go env GOVERSION GOOS GOARCH                     → go1.27.1 darwin arm64
go list -m all                                   → root module only
go mod graph                                     → lattice → go@1.22.0 → toolchain
go mod verify                                    → all modules verified
go list ./...                                    → 23 packages
go vet ./...                                     → clean
go test -count=1 ./...                           → PASS (all test packages)
go test -count=1 -race ./...                     → PASS, 0 races (memtable 254.8 s)
golangci-lint run ./...                          → 0 issues
golangci-lint run --enable gosec --tests=false ./... → 66 G115 + 3 G304 + 11 unused (triaged §6)
GOBIN=/tmp/lattice-audit-bin go install golang.org/x/vuln/cmd/govulncheck@v1.1.4  (tool install
  to /tmp only; project graph untouched)
/tmp/lattice-audit-bin/govulncheck ./...         → No vulnerabilities found
which govulncheck/osv-scanner/gosec/staticcheck/trivy/grype/syft/nancy (availability check)
find internal pkg cmd -type f -name '*.go' | wc -l  (170 files)
repo-wide greps: os/exec|exec.Command|unsafe|net/|crypto|math/rand|password|secret|token|
  api_key|private_key|Bearer|credential|WriteFile|RemoveAll|Rename|Symlink|panic(|Must
offline PoC in /tmp/lattice-audit-poc (replicated meta_index.go:168-181 arithmetic):
  → PANIC REPRODUCED: slice bounds out of range [10:4]
reads (no writes): go.mod, README, docs/*, threat-model, known-limitations,
  security-audit-phase-05, table_reader/writer, footer, block_handle, meta_index,
  index_builder, block_builder, bloom, filter_builder, murmur3, varint/validate/endian/
  crc/internalkey/types, skiplist/node/random/iterator, wal record/reader/writer/
  rotation/queue, errors, logger, finding.go, .golangci.yml
```

Post-report (pending at write time): `git add docs/security-audit-phase-00-05.md`,
`git commit -m "security: [SEC-AUDIT-P00-P05] full codebase security audit"`,
`git log -1 --oneline --decorate`, `git status`.

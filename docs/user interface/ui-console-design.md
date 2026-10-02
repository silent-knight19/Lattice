# Lattice Console — UI Design Specification

> Status: **Design spec (pre-implementation)**
> Audience: contributors building the Lattice web console
> Related: [`architecture-spec.md`](../architecture-spec.md), [`sstable-format-spec.md`](../sstable-format-spec.md), [`wal-record-format.md`](../wal-record-format.md), [`pprof-runbook.md`](../pprof-runbook.md), [`interview-knowledge.md`](../interview-knowledge.md)

---

## 1. Executive summary

Lattice is an LSM key-value engine with a Raft consensus layer, TLS/mTLS, Prometheus
telemetry, and forensic CLI utilities. It has **no user interface**. That is the single
largest gap between "a good systems project" and "a system an interviewer can see working."

The proposed **Lattice Console** is a read-mostly web application served by the daemon on
loopback, exposing six capabilities that today require five different CLI invocations and a
Prometheus stack:

1. **Cluster topology** — who is leader, who is lagging, is quorum healthy.
2. **LSM Tree explorer** — a live, sized, color-coded view of every SSTable at every level.
3. **Forensics** — SSTable block/CRC/Bloom inspection and WAL record replay in the browser.
4. **Raft timeline** — terms, elections, roles, and replication lag over time.
5. **Keyspace browser** — prefix scans, value inspection, tombstones, origin level.
6. **Lab** — run workloads, trigger compactions, SIGKILL the node, watch recovery.

### 1.1 Positioning against competitors

| System | Cluster/raft view | LSM/storage internals | Key browser | Live actions | Verdict |
|---|---|---|---|---|---|
| **CockroachDB DB Console** | Excellent (nodes, ranges, insights) | Weak (file storage listing only) | No (SQL instead) | Limited | Best *nav shell* reference |
| **YugabyteDB UI** | Good (tablets, Raft, health) | Weak (metrics only) | No | Limited | Best *cluster-health* reference |
| **Redis Insight** | N/A | N/A | Excellent (types, TTL, bulk edit) | Excellent | Best *browser UX* reference |
| **TiDB/TiKV Grafana** | Metric panels only | Metric panels only | No | No | Best *metric taxonomy* reference |
| **etcd / `etcd-dashboard`** | Member list | None | Basic KV | Limited | Baseline only |
| **RocksDB `ldb` / Pebble tools** | None | Deep but CLI/awkward | CLI scan | Manual | **Capability we want, UX we lack** |
| **Consensus Visualizer** | Excellent term timeline | None | None | None | Best *raft timeline* reference |
| **Lattice Console (proposed)** | Strong | **Deep — the differentiator** | Good | Strong | — |

**The gap we are exploiting:** every production database UI shows you *that* the storage
engine is slow, but none show you *why at the block level*. Lattice already has
`cmd/lattice/inspect.go` producing a full `ForensicReport` (footer, index block, meta-index,
Bloom filter parameters, per-block CRCs, restart offsets, key ranges). Surfacing that in a
browser is a genuine differentiator, not a dashboard clone.

---

## 2. Goals and non-goals

### Goals

- **G1.** Replace `lattice-cli` REPL + `lattice inspect` + `lattice dump-wal` + Prometheus +
  `pprof` for 90% of day-to-day inspection tasks.
- **G2.** Make Raft behavior *visible*: term changes, leader election, replication lag, quorum.
- **G3.** Make LSM behavior *visible*: level sizes, compaction triggers, write amplification.
- **G4.** Provide a safe "lab" mode that can crash the node and prove recovery, gated behind
  explicit admin intent.
- **G5.** Zero new runtime dependencies in the daemon binary — stdlib only, consistent with the
  rest of the project (`go.mod` has no third-party requirements).
- **G6.** Be usable as a live interview demo: cold start to interesting picture in < 60 seconds.

### Non-goals

- **NG1.** Not an authorization system. We reuse the existing `internal/transport` role model
  (`reader`/`writer`/`admin`) rather than inventing identities.
- **NG2.** Not a metrics retention store. We render from the live in-process registry; long-term
  storage stays in Prometheus. (Exception: a bounded in-memory ring buffer for the Raft
  timeline, which Prometheus cannot reconstruct.)
- **NG3.** Not a replacement for `pprof`. We embed links and profile downloads rather than
  reimplementing the profile viewer.
- **NG4.** Not a general SQL shell — Lattice is a KV store.
- **NG5.** Not production internet-facing software. Loopback bind by default; admin actions
  require explicit confirmation. See §7.

---

## 3. Information architecture

```
Lattice Console
│
├── Overview                 (/)              health, leader, flags, sparklines
├── Cluster                  (/cluster)       topology graph, peer table, quorum
│   └── Node drill-down      (/cluster/:id)   per-node detail + proxy selector
├── Storage                  (/storage)       LSM tree, memtable, WAL, manifest
│   ├── LSM Tree             (/storage/lsm)
│   ├── SSTable Inspector    (/storage/sstables)
│   └── WAL Replay           (/storage/wal)
├── Raft                     (/raft)          status, log, timeline, leader
├── Keyspace                 (/keys)          prefix browse, values, tombstones
├── Metrics                  (/metrics)       latency, throughput, cache, disk
├── Lab                      (/lab)           workloads, compaction, crash & recover
├── Console                  (/console)       in-browser REPL
└── System                   (/system)        config, TLS, security posture, pprof
```

Navigation follows CockroachDB's proven left-rail grouping (**health → components → activity →
advanced debug**), with Redis Insight's *Browser* promoted to a first-class page because a KV
store's data browser is its most-used screen.

### 3.1 Page specs

#### 3.1.1 Overview `/`

Single screen answering "is this database okay?"

- **Status banner** — one line: `HEALTHY · leader n1 (term 4) · 3/3 members · recovered 12s ago`
  or `DEGRADED · no quorum · last election 3s ago`. Uses the same
  `LivenessHandler`/`ReadinessHandler` semantics as `internal/metrics/server.go`.
- **Tile row (6 tiles)** — each a sparkline + current value, click-through:

  | Tile | Source |
  |---|---|
  | Uptime / restarts | node state + `RecoverWAL` timestamps |
  | Ops/s (put/get/delete) | `lattice_request_duration_seconds{op=...}` rate |
  | Write p99 latency | `lattice_engine_write_latency_seconds` histogram quantile |
  | LSM total size | sum of `Version.Files` sizes |
  | Memtable fill % | `lattice_memtable_active_bytes` / configured limit |
  | Disk free | `lattice_disk_free_bytes` |

- **Cluster strip** — one row per node: id, address, role badge, term, commit index,
  leader-follower lag, live/ready dot.
- **Recent events feed** — last 20 Raft transitions, compactions, flushes, recovery events
  (from the event bus, §5.4).
- **Active incidents banner** — non-empty `LastCleanupErrors()`, failed compactions, orphaned
  files, WAL-poisoned state.

#### 3.1.2 Cluster `/cluster`

- **Topology graph** — nodes laid out in a circle; leader highlighted; edges annotated with
  `matchIndex` and lag in entries; edge animates on AppendEntries traffic. SVG, hand-rolled,
  no graph library (keeps frontend deps minimal).
- **Peer table** — sortable: `node_id, address, role, term, nextIndex, matchIndex, lag,
  last_heartbeat, reachable`. Row click → node drill-down.
- **Quorum panel** — `quorum_size`, `granted_votes`, whether writes are currently accepted,
  and a plain-English explanation of current availability (`3/3 → 2/3 writes OK`).
- **Node drill-down** — mirrors CockroachDB's `remote_node_id` proxy selector: pick any node,
  all subsequent API calls target it.
- **Transport health** — active connections, in-flight requests, pipeline rejections,
  pipeline limit hits (`lattice_pipeline_*`), TLS/mTLS posture.

#### 3.1.3 LSM Tree `/storage/lsm`

**The signature page.** A live treemap/column chart of the LSM structure.

- **Level columns** L0 (leftmost, unsorted) → deepest level, each column a stack of file
  rectangles with `width ∝ file size`, `height ∝ entry count`.
- **File rectangle contents** — `file number`, entry count, key range
  `user:alice … user:zoe`, sequence-number range, ref count. Hover → tooltip; click → inspector.
- **Overlap highlighting** — files whose key ranges overlap within a level glow red. This
  directly explains why a compaction was chosen.
- **Memtable overlay** — active + immutable memtables drawn above L0 with fill percentage and
  flush threshold marker, so you can *predict* the next flush.
- **WAL segment strip** — below the tree: segments as bars sized by length, active one
  highlighted, archived ones fading; click → WAL replay.
- **Compaction panel** — enabled/running, run count, failure count, last description
  (`L0 → L1, 4 files, 12.3 MB`) from `Engine.CompactionStats()`; plus per-level duration
  histogram (`lattice_compaction_duration_seconds{level=...}`).
- **Write amplification estimate** — bytes flushed vs bytes ingested vs compacted, derived.
- **Manual compaction trigger** — admin-gated button calling `SignalCompaction()` /
  `RunCompactionOnce()`.

#### 3.1.4 SSTable Inspector `/storage/sstables`

Direct, faithful rendering of `ForensicReport` from `cmd/lattice/inspect.go`.

- **File picker** — all `.sst` files in the data dir with size, level, mtime.
- **Header strip** — path, file size, overall `Valid`, `MagicValid`, `PaddingValid`.
- **Footer panel** — `FooterMagic`, index `BlockHandle{Offset,Size}`, meta `BlockHandle`,
  with a **hex viewer** of the raw footer bytes.
- **Block map** — horizontal offset ruler with blocks drawn proportionally: data blocks (grey),
  index block (blue), meta-index (purple), Bloom filter (green), footer (amber). Click to inspect.
- **Per-block table** — index, offset, size, stored vs computed CRC, pass/fail chip, record
  count, restart count, first/last key (rendered with `FormatBytes` escaping to avoid XSS).
- **Bloom filter panel** — present?, bit count, hash count, key count, byte size, and a
  **bloom membership simulator**: type a key, see whether it *would* hit, cross-checked
  against an actual lookup. A great interview demonstration.
- **Index block panel** — entry count, CRC verdict, and the separator-key list.
- **Corruption banner** — if any CRC fails or magic is invalid, a red banner explains which
  block failed and what it implies (unrecoverable vs. truncated tail).
- **Raw hex view** — offset/length range request with a `xxd`-style renderer.

#### 3.1.5 WAL Replay `/storage/wal`

- **Segment list** — number, byte length, record count, first/last LSN, checksum status.
- **Record timeline** — one row per record: LSN, type (`PUT/DELETE/BATCH/COMMIT/CHECKPOINT`),
  key, value size, CRC status, timestamp if present. Filters by type, key prefix, "corrupt only".
- **Replay simulation** — "Replay to LSN N" shows exactly which records would be applied and
  which were already covered by a checkpoint (`RecoverWALFromCheckpoint`).
- **Corruption highlighting** — truncated or CRC-mismatched records flagged inline, matching
  `DumpWALReport` output.
- **Raw record view** — hex of a selected record's payload.

#### 3.1.6 Raft `/raft`

- **Status panel** — role badge (Follower/Candidate/Leader), current term, leader id,
  commit index, log size, quorum size, granted votes. Mirrors the exported `raft.Node`
  accessors: `Role()`, `Term()`, `LeaderID()`, `CommitIndex()`, `QuorumSize()`,
  `GrantedVotesCount()`, `NextIndex()`, `MatchIndex()`.
- **Term timeline (ribbon chart)** — one horizontal band per term; leader terms filled;
  elections marked; gaps and restarts visible. Fed by `Node.TransitionHook` into a ring
  buffer. This is the *Consensus Visualizer* pattern applied to a live system.
- **Log viewer** — paginated entries: index, term, type, CRC, size. Filter by term/index range.
- **Replication lag chart** — per-peer `commitIndex - matchIndex` over time.
- **HardState panel** — current term, votedFor, commitIndex, persisted vs in-memory, and
  whether the Raft log WAL is healthy.
- **Read-Index panel** — `lattice_raft_read_index_latency_seconds` histogram and current
  in-flight ReadIndex requests (demonstrates linearizable reads without a leader round-trip).
- **Danger zone** — admin-gated `BecomeCandidate()` / `StepDownSameTerm()` to force an
  election for demonstration, with typed confirmation.

#### 3.1.7 Keyspace `/keys`

Modeled on Redis Insight's Browser, adapted to LSM semantics.

- **Prefix search** — exact `GET` plus prefix range listing.
  *Requires a new engine merge iterator (§6.1, Phase E).*
- **Result table** — key, value size, type, sequence number, TTL/expiry, tombstone flag, and
  **which level/memtable it was found in** (a genuinely LSM-specific column: the same key may
  exist as a tombstone in L1 and a value in L5).
- **Value viewer** — text / hex / base64 toggle, with `FormatBytes`-equivalent escaping.
- **TTL & tombstone visibility** — show shadows and tombstones (toggleable; off by default).
- **Editor** — put and delete single keys, with the same role gating as the transport.
- **Bulk actions** — delete by prefix, with a confirmation summary (count + bytes) before
  executing. Mirrors the Redis Insight pattern of "show what will be deleted first."
- **Stats panel** — key count, value bytes, tombstone count, distribution across levels
  (stacked bar) — the LSM equivalent of Redis's `Database analysis`.

#### 3.1.8 Metrics `/metrics`

Dashboard grid, following TiKV/YugabyteDB metric taxonomy but rendered as first-class cards
with live updates rather than a raw Prometheus query builder.

| Group | Panels | Metrics |
|---|---|---|
| **Request** | Throughput by op, p50/p95/p99 latency by op, error rate, in-flight, rejections | `lattice_request_duration_seconds{op}`, `lattice_requests_in_flight`, `lattice_pipeline_rejections_total`, `lattice_pipeline_limit_hits_total` |
| **Engine** | Write/read latency, memtable bytes, block cache hit rate | `lattice_engine_write_latency_seconds`, `lattice_engine_read_latency_seconds`, `lattice_memtable_active_bytes`, `lattice_block_cache_{hits,misses}_total` |
| **Compaction** | Flush duration, compaction duration by level, run/failure counts | `lattice_flush_duration_seconds`, `lattice_compaction_duration_seconds{level}`, `CompactionStats` |
| **Raft** | Proposal latency, ReadIndex latency, term/leader, commit index | `lattice_raft_proposal_latency_seconds`, `lattice_raft_read_index_latency_seconds`, raft node state |
| **WAL** | Bytes written, active segment size | `lattice_wal_bytes_written_total`, `lattice_wal_active_segment_bytes` |
| **Resources** | Connections, disk free/total, file descriptors | `lattice_connections_active`, `lattice_disk_{free,total}_bytes` |

Every panel has: a sparkline, current/p50/p95/p99, a raw-value tooltip, and a "view as
Prometheus" link opening the raw `/metrics` scrape. Panels are shareable via URL query params
(`/metrics?panels=raft,compaction&range=15m`).

#### 3.1.9 Lab `/lab`

The demo stage. Everything behind an `admin` gate and a red "this can crash the node" banner.

- **Workload generator** — configurable keyspace size, value size, read/write mix, concurrency;
  streams live throughput and latency charts. Can drive the in-process engine or a cluster via
  the real transport (`PUT/GET/BATCH`), with an explicit "use real transport" toggle so
  viewers see Raft in the path.
- **Compaction lab** — force flush, force compaction, disable compaction and watch L0 grow.
- **Crash & recover** — `SIGKILL` self; countdown; then automatically restart, run recovery,
  and show the recovery report (WAL segments replayed, records applied, records truncated by
  checkpoint, final sequence number) plus a before/after key count with a consistency
  assertion. **The single highest-value demo in the console.**
- **Partition lab** — with a real 3-node cluster, drop peer traffic and watch the cluster lose
  quorum, then heal.
- **Benchmark runner** — drive `internal/benchmark` and plot results against previous runs.

#### 3.1.10 Console `/console`

- Browser REPL mirroring `cmd/lattice-cli` semantics: `GET, PUT, DELETE, EXISTS, BATCH, STATS,
  HELP, EXIT`.
- **Help panel auto-generated** from the parser's command table.
- **Multi-node target switcher** — send a command to leader or any follower to *demonstrate*
  the not-leader redirect path visually.
- **Command history** persisted to `localStorage`, exportable as a shell script.

#### 3.1.11 System `/system`

- **Effective configuration** — every flag with its resolved value and provenance
  (flag / config file / default): data dir, ports, peer set, TLS paths.
- **Security posture** — TLS/mTLS enabled, peer cert OU, client authz policy, whether
  `--insecure-transport` is in effect (loud warning), loopback-only binding.
- **Profiling links** — deep links into `pprof` endpoints with the target node preselected.
- **Diagnostics bundle** — gather config, metrics scrape, health, recent events, and Raft state
  into a downloadable `.tar.gz`. Mirrors CockroachDB's diagnostic bundle concept.
- **Danger zone** — orphan-file cleanup (`CleanOrphanedFilesWithReport`), results shown.

---

## 4. Visual design

- **Theme** — dark by default (operators' preference), light theme supported; respects
  `prefers-color-scheme`.
- **Level palette** — a perceptually uniform sequential ramp for L0 → L6 (e.g. `#4C6EF5` →
  `#7048E8`) so level depth reads at a glance, plus red `#FA5252` reserved *exclusively* for
  corruption/failure so it never appears decoratively.
- **Role colors** — Leader `#2F9E44`, Follower `#4C6EF5`, Candidate `#F59F00`.
- **Accessibility** — every status encoded as color + icon + text (never color alone); AA
  contrast; full keyboard navigation; focus rings retained.
- **Safe rendering** — all keys, values, and file paths rendered as escaped text (reuse
  `FormatBytes` semantics, which already defends against terminal escape injection — the web
  equivalent must defeat HTML/script injection).

---

## 5. Backend design

### 5.1 Deployment shape

Two options were considered.

**Option A — in-process admin server in the daemon.** New `internal/admin` package
instantiated by `cmd/lattice` when `--admin-address` is set. Direct, type-safe access to
`*engine.Engine` and `*raft.Node`. Loopback-only enforced by default, mirroring the existing
`--pprof-address` treatment in `cmd/lattice/pprof.go`.

**Option B — separate `lattice-console` binary** speaking a new admin wire protocol.

**Decision: Option A for the API, with a thin multi-node aggregator later.**

- In-process access means no new wire protocol, no new opcode, no new authentication scheme —
  all of which would be significant new attack surface on a security-hardened engine.
- The data being displayed (LSM versions, compaction state, Raft internals) is *per-node and
  inherently local*; it was never going to be sensibly remote.
- The engine is already a single process holding all of this state; the UI is a view, not a
  control plane.
- Cluster aggregation is a *frontend* concern in v1 (the browser fans out to each node's admin
  port, all loopback). A `lattice-console` aggregator binary becomes worthwhile only if remote
  exposure is ever needed, and is explicitly optional (Phase I).

New flag: `--admin-address` (loopback only unless `--insecure-transport` is set — same rule as
`--pprof-address`).

### 5.2 Why REST + SSE rather than gRPC/WebSocket

- The engine is stdlib-only; `net/http` needs zero dependencies. WebSocket would require either
  a dependency or hand-rolled RFC 6455 framing — not worth it.
- Server-Sent Events over `net/http` is ~40 lines and gives us push for metrics, raft
  transitions, and compaction events.
- REST endpoints are trivially testable with `httptest`, matching the project's existing
  `httptest`-based tests (`cmd/lattice/pprof_test.go`, `internal/metrics/server_test.go`).

### 5.3 API surface

Base path `/api/v1`. All responses `application/json` unless noted. Mutating endpoints require
the `X-Lattice-Admin: confirm` header *and* an admin principal.

#### Node & health

| Method | Path | Returns |
|---|---|---|
| GET | `/health` | liveness, readiness, recovering/closed/poisoned flags |
| GET | `/node` | node id, addresses, version, start time, uptime, data dir, flags |
| GET | `/config` | resolved configuration + provenance of each value |

#### Engine & storage

| Method | Path | Returns |
|---|---|---|
| GET | `/engine/stats` | `NextSeqNum`, `NextFileNum`, `IsRecovered`, `IsRecovering`, `IsWALPoisoned`, memtable count/bytes, obsolete file count, last cleaner report, backpressure state |
| GET | `/lsm/tree` | per-level file lists: file number, size, smallest/largest key, seqnum range, refs |
| GET | `/lsm/compaction` | `CompactionStats` + last error + per-level duration series |
| POST | `/lsm/compact` | trigger compaction (admin) |
| POST | `/lsm/flush` | trigger memtable flush (admin) |
| GET | `/sstables` | on-disk SSTable inventory with level, size, mtime |
| GET | `/sstables/inspect?file=` | `ForensicReport` JSON (reuses `InspectSSTable`) |
| GET | `/sstables/raw?file=&offset=&length=` | base64 + hex bytes (offset/length capped) |
| GET | `/wal/segments` | segment inventory |
| GET | `/wal/dump?segment=&verbose=` | `DumpWALReport` JSON (refactored from `DumpWAL`) |
| GET | `/manifest` | MANIFEST version-edit history |

#### Raft

| Method | Path | Returns |
|---|---|---|
| GET | `/raft/status` | role, term, leader id, commit index, log bounds, quorum, granted votes, hard state |
| GET | `/raft/peers` | `NextIndex`/`MatchIndex` maps + derived lag |
| GET | `/raft/log?from=&to=` | paginated log entries |
| GET | `/raft/timeline` | term/role transition events from the ring buffer |
| POST | `/raft/campaign` | force election (admin) |
| POST | `/raft/stepdown` | force step-down (admin) |

#### Keys

| Method | Path | Returns |
|---|---|---|
| GET | `/keys?prefix=&limit=&include_shadows=` | prefix listing with origin level |
| GET | `/keys/get?key=` | value + metadata |
| POST | `/keys/put` | `{key, value}` |
| POST | `/keys/delete` | `{key}` |

#### Metrics & events

| Method | Path | Returns |
|---|---|---|
| GET | `/metrics` | **JSON** snapshot of the registry (new `Registry.SnapshotJSON`) |
| GET | `/metrics/prometheus` | raw exposition (proxy of `/metrics`) |
| GET | `/events` | **SSE** stream: metrics samples, raft transitions, compaction events, recovery events |
| GET | `/pprof/` | proxy/link list to the node's pprof server |

#### Lab (all admin-gated)

| Method | Path | Returns |
|---|---|---|
| POST | `/lab/workload/start` | begins a workload, returns job id |
| GET | `/lab/workload/{id}` | live progress + latency series |
| POST | `/lab/crash` | schedules `SIGKILL` of self after N seconds |
| POST | `/lab/recover-report` | last recovery report |
| POST | `/lab/cleanup-orphans` | `CleanOrphanedFilesWithReport` |
| GET | `/diagnostics` | zipped bundle (config + metrics + health + raft + recent events) |

### 5.4 Event bus

A small `internal/events` package: bounded ring buffer (capacity ~1000) plus subscriber
channels. Producers:

- `raft.Node` `TransitionHook` (already exists) → term/role transitions.
- Compaction worker start/finish/fail.
- Memtable flush completion.
- WAL segment rotation.
- Engine recovery start/finish.
- `CleanOrphanReport` results.

Consumers: the `/events` SSE handler, the Overview event feed, the Raft timeline.

> **Note:** ring-buffer memory is bounded and pre-allocated; the daemon's shutdown path must
> unregister any gauges added for the admin server, following the existing `UnregisterGaugeFunc`
> pattern in `cmd/lattice/daemon.go`.

---

## 6. Known implementation gaps (must be closed for feature parity)

These are *backend* work the UI depends on. Each is a legitimate, self-contained piece of
systems engineering that strengthens the project independently of the UI.

### 6.1 Engine merge iterator (blocks Phase E — Keyspace browser)

Lattice currently exposes **point lookups only** (`Engine.Get`, `Engine.Exists`,
`Engine.pointLookup`). There is no exported iterator, range scan, or prefix scan. A keyspace
browser requires one:

- Merge iterator over: active memtable → immutable memtables (newest first) → L0 files
  (by descending sequence number) → L1..Ln (by ascending file order).
- Version pinning so the iteration is consistent under concurrent compaction.
- Optional `include_shadows` to reveal tombstones and superseded versions.
- Result should report **where each key was resolved from** (level/memtable), which is both a
  UI requirement and a genuinely useful engine diagnostic.

Effort: moderate. This is the single largest backend dependency of the UI plan.

### 6.2 Metrics JSON snapshot

`internal/metrics` exposes Prometheus text output and `Histogram.Snapshot()`, but there is no
structured JSON accessor with histogram quantiles. Add `Registry.SnapshotJSON()` returning
counters, gauges, and per-metric quantile summaries. Prefer this over parsing Prometheus text
in the browser.

### 6.3 `DumpWAL` refactor

`DumpWAL(path, verbose, stdout, stderr) int` writes directly to writers and returns an exit
code. Extract the report-building logic so the admin API can return `DumpWALReport` as JSON
while the CLI keeps its exact current behavior and exit codes.

### 6.4 Raft introspection surface

`raft.Node` already exposes most of what the UI needs (`Role`, `Term`, `LeaderID`,
`CommitIndex`, `QuorumSize`, `GrantedVotesCount`, `NextIndex`, `MatchIndex`). Likely additions:
a `HardState()` accessor, log-entry pagination, and installing the `TransitionHook` into the
event bus by default.

### 6.5 Admin-path authorization

Mutating admin endpoints must be gated. Reuse `internal/transport`'s existing
`AuthorizeRole(opcode)` model. Because the admin server is separate from the client transport,
decide explicitly whether it (a) requires a client certificate via mTLS like the data path, or
(b) relies on loopback binding plus a confirmation header. **Recommendation: loopback + header
for v1, mTLS optional, and no remote exposure** — documented as a deliberate limitation in
`docs/known-limitations.md`.

---

## 7. Security requirements

The console must not become the weakest link in a security-hardened engine.

| Requirement | Rationale |
|---|---|
| Loopback-only bind by default | Mirrors `--pprof-address` precedent |
| Remote bind requires `--insecure-transport` | Consistent with the data path's existing rule |
| All mutating endpoints require admin role + `X-Lattice-Admin: confirm` header | Prevents accidental destructive action from a stray browser tab |
| Every key, value, and path rendered as escaped text | `FormatBytes` already defends terminal escape injection; the web layer must defend HTML/script injection |
| File-serving endpoints constrained to the data directory | `security.CleanAndValidatePath` + `sstable.ValidatePathNoSymlinks` are already used by `InspectSSTable` — reuse them, never accept raw paths |
| Hex/raw byte endpoints length-capped | Prevents a single request from exfiltrating or DoSing on a huge read |
| No credentials in the frontend bundle | Auth stays server-side |
| `Cache-Control: no-store` on all API responses | Console data is operational and sensitive |
| pprof exposure unchanged | Do not widen pprof reachability; link to it, don't inline it |

---

## 8. Frontend design

### 8.1 Stack

| Concern | Choice | Rationale |
|---|---|---|
| Framework | **React 18 + TypeScript** | Ecosystem, component model, hiring/interview legibility |
| Build | **Vite** | Fast dev server, trivial static output, one `npm run build` |
| Data fetching | **TanStack Query** | Polling, caching, SSE-friendly, avoids hand-rolled state |
| Charts | **uPlot** (timeseries) + hand-rolled **SVG** (LSM tree, topology, timeline) | uPlot is tiny and fast for dense time series; bespoke SVG keeps the build small for structural views |
| Tables | **TanStack Table** | Virtualized key/log listings |
| Styling | **Tailwind CSS** + `clsx` | Fast, consistent, no design-system dependency |
| Components | Hand-rolled primitives (button, table, badge, drawer) | Avoids a heavy component library; full control over the level palette |
| REPL editor | Plain `<textarea>` initially; CodeMirror 6 if time permits | Avoids a heavy editor dependency for v1 |

> **Constraint honored:** all JavaScript dependencies are *build-time only*. The shipped artifact
> is a static bundle embedded in the Go binary via `go:embed`. The daemon binary gains **zero
> Go dependencies**.

### 8.2 Backend-for-frontend tradeoff

Chosen: **static SPA + JSON REST + SSE** (no Next.js/SSR).

- The Go binary embeds and serves the SPA. One binary to run, one process to supervise.
- SSE instead of WebSockets.
- No Node.js process in the deployment story — important for a systems project.

### 8.3 Repository layout

```
web/
├── package.json, vite.config.ts, tsconfig.json, tailwind.config.ts
├── index.html
└── src/
    ├── main.tsx, App.tsx, router.tsx
    ├── api/
    │   ├── client.ts            # typed fetch wrapper, error envelope, admin header
    │   ├── types.ts             # mirrors Go JSON structs
    │   └── events.ts            # SSE subscription hook
    ├── components/
    │   ├── primitives/          # Button, Table, Badge, Drawer, Tooltip, Card
    │   ├── charts/              # Sparkline, TimeSeries, Heatmap
    │   ├── lsm/                 # LSMTree, FileRect, LevelColumn, MemtableOverlay
    │   ├── cluster/             # TopologyGraph, PeerTable, QuorumPanel
    │   ├── raft/                # TermTimeline, LogTable, LagChart
    │   └── forensics/           # BlockMap, HexViewer, CRCVerdict, BloomSimulator
    ├── pages/                   # one folder per §3.1 page
    └── styles/
```

Go side:

```
internal/admin/          # HTTP server, routing, authz
internal/admin/handlers_*.go
internal/events/         # ring buffer + subscribers
cmd/lattice/admin.go     # --admin-address wiring, lifecycle, shutdown
web/dist/                # built SPA, embedded via go:embed
```

### 8.4 Testing strategy

| Layer | Tool | Notes |
|---|---|---|
| Go handlers | `httptest` + table-driven tests | Mirrors `cmd/lattice/pprof_test.go` / `internal/metrics/server_test.go` conventions |
| Go admin server lifecycle | `internal/admin` tests | Start, request, graceful shutdown, gauge unregistration |
| Path safety | tests asserting traversal and symlink rejection | Reuse `security.CleanAndValidatePath`; **must** be tested |
| Frontend unit | Vitest | Pure helpers (level bucketing, byte formatting, latency coloring) |
| Frontend components | Vitest + Testing Library | Block map, topology, timeline render from fixture JSON |
| End-to-end | Playwright | Boot daemon + serve SPA, exercise Overview → LSM → Inspector → Raft |
| Live cluster | Playwright against a 3-node cluster | Leader failover via the Lab page |
| Visual regression | Playwright screenshots | Optional, if time permits |

**Definition of done for every page:** handler tests exist, the page renders against a real
daemon in an e2e test, and it degrades gracefully when the daemon is mid-recovery.

---

## 9. Risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Merge iterator is a large backend task | Keyspace page slips | Ship it as its own reviewed phase; the rest of the console is unaffected |
| Live event stream overloads the daemon | Latency regression on the data path | Bounded ring buffer, coalesced metric samples (1 Hz), SSE behind a small concurrency limit |
| Admin server widens attack surface | Security regression | Loopback default, admin gate on mutations, path validation, dedicated security test file, update `known-limitations.md` |
| Frontend build not reproducible | Reviewer friction | Commit `web/dist`; document `npm ci && npm run build`; add a CI check |
| Too many pages → shallow | Weak portfolio signal | Build in the priority order of §10 and stop when time runs out; each page is independently demoable |
| Grafana duplication | Wasted effort | Console complements Prometheus: Prometheus for retention/alerting, console for structure and forensics |

---

## 10. Build order (summary)

Detailed task-by-task plan, effort estimates, and per-phase acceptance criteria live in
[`ui-console-plan.md`](./ui-console-plan.md).

| Phase | Theme | Demo milestone |
|---|---|---|
| **A** | Admin server skeleton, `/health`, `/node`, `/metrics` JSON | `curl localhost:7070/api/v1/health` returns JSON |
| **B** | Frontend scaffold, shell, Overview, Metrics | Console renders live health |
| **C** | LSM Tree + SSTable Inspector | The signature page — visual tree, click into block CRCs and Bloom params |
| **D** | Raft status, peers, term timeline | Failover is visible as a term ribbon |
| **E** | Merge iterator + Keyspace browser | Browse keys, see which level resolved each one |
| **F** | WAL replay + manifest | Scrub a WAL segment in the browser |
| **G** | Lab: workloads, compaction, crash & recover | SIGKILL → automatic restart → recovery report |
| **H** | Console REPL, System/security, diagnostics bundle | Full operator console |
| **I** | Polish, docs, screenshots, README | Interview-ready |

---

## 11. Interview framing

The console is not decoration; it is the artifact that makes the following claims checkable in
one click each:

| Claim | Page that proves it |
|---|---|
| "The LSM tree is leveled, and compactions are picked by key-range overlap" | LSM Tree — overlap highlighting |
| "SSTables are self-describing with CRC-verified blocks and Bloom filters" | SSTable Inspector — per-block CRC table, Bloom simulator |
| "Crash recovery replays the WAL exactly once, respecting checkpoints" | Lab — crash & recover report |
| "Raft survives leader failure and maintains single-leader consistency" | Raft — term timeline + peer lag during failover |
| "Cache hit rate reflects real workload behavior" | Metrics — block cache panel |
| "I understand security threat models" | System — security posture; admin gating |

See [`ui-console-plan.md`](./ui-console-plan.md) for the execution plan and
[`interview-knowledge.md`](../interview-knowledge.md) for the narrative this UI supports.

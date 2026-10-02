# Lattice Console — Implementation Plan

> Companion to [`ui-console-design.md`](./ui-console-design.md).
> This document is the **task-level execution plan**: what to build, in what order, with
> acceptance criteria and rough effort estimates.

---

## How to use this plan

- Phases are **independently demoable**. Stop after any phase and you still have something
  worth showing.
- Effort estimates assume familiarity with the codebase. `S` ≈ half a day, `M` ≈ 1–2 days,
  `L` ≈ 3–5 days.
- Every phase ends with `go build ./...`, `go vet ./...`, and the relevant `go test` invocation
  passing. The race gate (`go test -race -short ./...`) is required from Phase B onward.
- The project remains **stdlib-only in Go**. Any new Go dependency requires an explicit,
  documented exception.

---

## Phase A — Admin server skeleton (M)

**Goal:** a running HTTP admin surface on the daemon, provable with `curl`.

### Tasks

1. `internal/admin/server.go` — `Server` struct holding `*engine.Engine`, optional `*raft.Node`,
   node identity, and config; `Start`/`Shutdown` mirroring `internal/metrics.Server`.
2. `internal/admin/router.go` — `http.ServeMux` with the `/api/v1` prefix; JSON writer helper
   with consistent `Content-Type: application/json` and `Cache-Control: no-store`.
3. `cmd/lattice/config.go` — add `--admin-address` flag (default `""`, loopback-enforced).
4. `cmd/lattice/admin.go` — wire server startup into the daemon lifecycle **alongside** the
   pprof and metrics servers; shutdown in the same path; unregister any gauges added.
5. Handlers: `GET /api/v1/health`, `GET /api/v1/node`, `GET /api/v1/config`,
   `GET /api/v1/metrics`.
6. `internal/metrics/registry.go` — add `SnapshotJSON()` (design doc §6.2).
7. Loopback enforcement plus `--insecure-transport` interaction, mirroring `pprof.go`.

### Acceptance criteria

- [ ] `lattice --admin-address 127.0.0.1:7070 …` starts and serves.
- [ ] `curl localhost:7070/api/v1/health` returns valid JSON with live/ready state.
- [ ] `curl localhost:7070/api/v1/metrics` returns JSON with counters, gauges, and quantiles.
- [ ] Binding to a non-loopback address without `--insecure-transport` fails with a clear error.
- [ ] Table-driven handler tests for each endpoint (success and error paths).
- [ ] A test asserts admin gauges are unregistered on shutdown (mirrors the existing
      metrics-server lifecycle test).
- [ ] `go vet ./...` and `go build ./...` clean.

---

## Phase B — Frontend scaffold, shell, Overview, Metrics (L)

**Goal:** the first pixels. A real console rendering live daemon state.

### Tasks

1. `web/` — Vite + React + TypeScript + Tailwind scaffold; router with the design-doc §3.1
   routes (placeholder pages for the not-yet-built ones).
2. `web/src/api/client.ts` — typed fetch wrapper, uniform error envelope, admin header support.
3. `web/src/api/types.ts` — TypeScript mirrors of the Go JSON structs.
4. `web/src/components/primitives/*` — Button, Card, Badge, Table, Drawer, Tooltip, StatusDot.
5. `web/src/components/charts/*` — Sparkline and TimeSeries (uPlot), Heatmap.
6. **Overview page** — status banner, six tiles, cluster strip, events feed (polling initially;
   SSE arrives in Phase D).
7. **Metrics page** — the full design-doc §3.1.8 panel grid, grouped by
   Request/Engine/Compaction/Raft/WAL/Resources, with shareable URL state.
8. `web/dist` embedding via `go:embed`; serve static assets from the admin server with correct
   content types and an SPA fallback route.
9. Vitest setup; unit tests for formatting helpers.

### Acceptance criteria

- [ ] `npm ci && npm run build` produces `web/dist`.
- [ ] Daemon serves the console at `/` on the admin address.
- [ ] Overview shows live values that change while `lattice-cli` writes.
- [ ] Metrics page renders every registered `lattice_*` metric in at least one panel.
- [ ] `go test -race -short ./...` passes.
- [ ] A Playwright smoke test loads `/` and asserts the health banner is present.

---

## Phase C — LSM Tree + SSTable Inspector (L)

**Goal:** the signature page, and the clearest differentiator versus competitors.

### Tasks

1. `GET /api/v1/lsm/tree` — walk `VersionSet.Current()` / `Version.Files` and emit per-level
   file records: number, size, entry count, smallest/largest key, seqnum range, refs.
2. `GET /api/v1/lsm/compaction`, `POST /api/v1/lsm/compact`, `POST /api/v1/lsm/flush`.
3. `GET /api/v1/sstables`, `GET /api/v1/sstables/inspect`, `GET /api/v1/sstables/raw`.
   - Reuse `InspectSSTable` from `cmd/lattice/inspect.go`. **Preferred:** move
     `ForensicReport` and `InspectSSTable` into a shared package (e.g. `internal/inspect`) so
     both `cmd/lattice` and `internal/admin` use them, with the CLI as a thin wrapper. This
     avoids duplicating forensic logic.
   - **Path safety is mandatory:** resolve `file=` to a path under the data dir using
     `security.CleanAndValidatePath` + `sstable.ValidatePathNoSymlinks`. Never accept an
     arbitrary path from the query string.
4. Frontend: `LSMTree` (level columns, sized file rectangles, memtable overlay, WAL strip),
   `BlockMap`, `HexViewer`, `CRCVerdict`, `BloomSimulator`.
5. SSTable Inspector page wiring.
6. Tests: handler tests including traversal and symlink rejection; component tests rendering
   `BlockMap` and `CRCVerdict` from fixture JSON; an e2e test that writes enough data to produce
   L0 files, then asserts the tree renders them.

### Acceptance criteria

- [ ] The tree renders every level with proportionally sized files.
- [ ] Overlapping files within a level are visibly flagged.
- [ ] Clicking a file opens a full `ForensicReport` rendering: footer, index, meta-index, Bloom
      parameters, and a per-block CRC table.
- [ ] The Bloom simulator agrees with an actual `Get` for both present and absent keys.
- [ ] `?file=../../etc/passwd` and a symlink escape are both rejected with a clear error, and a
      test asserts it.
- [ ] Hex viewer is length-capped.

---

## Phase D — Raft status, peers, term timeline (M)

**Goal:** make consensus behavior visible.

### Tasks

1. `internal/events` — bounded ring buffer plus subscribers; install `raft.Node`
   `TransitionHook`.
2. `GET /api/v1/raft/status`, `/raft/peers`, `/raft/log`, `/raft/timeline`.
3. `raft.Node` additions as needed: `HardState()`, log pagination (design doc §6.4).
4. `POST /api/v1/raft/campaign`, `/raft/stepdown` (admin-gated, typed confirmation in UI).
5. `GET /api/v1/events` — SSE endpoint, 1 Hz coalesced metric samples plus discrete events.
6. Frontend: `TermTimeline` (ribbon chart), `PeerTable`, `LagChart`, `LogTable`, status panel.
7. Overview's event feed switches from polling to SSE.
8. Tests: events ring buffer unit tests (wraparound, slow-consumer drop policy); SSE handler
   test; three-node integration test asserting a campaign produces a new term.

### Acceptance criteria

- [ ] Killing the leader in a 3-node cluster visibly produces a new term on the ribbon chart.
- [ ] Peer lag is accurate after failover.
- [ ] A follower returns a redirect/not-leader response that the UI surfaces with a "retry" action.
- [ ] The SSE stream does not leak goroutines on client disconnect (covered by `-race`).

---

## Phase E — Merge iterator + Keyspace browser (L)

**Goal:** browse data, and see which LSM level resolved each key.

### Tasks

1. **Engine merge iterator** (design doc §6.1) — the largest single backend task.
   - `internal/engine/iterator.go`: merged iteration across memtables and levels,
     version-pinned.
   - Include an origin descriptor (level / memtable index) per returned key.
   - `include_shadows` mode exposing tombstones and superseded versions.
   - Unit tests: correctness against a naive map oracle, tombstone handling, L0 ordering by
     descending sequence number, consistency under concurrent compaction (run under `-race`).
2. `GET /api/v1/keys`, `GET /api/v1/keys/get`, `POST /api/v1/keys/put`,
   `POST /api/v1/keys/delete`.
3. Frontend Keyspace page: prefix search, result table with origin level, value viewer
   (text/hex/base64), tombstone toggle, bulk delete-by-prefix with confirmation summary, stats
   panel with level distribution.

### Acceptance criteria

- [ ] Iterator tests pass, including the concurrent-compaction consistency test under `-race`.
- [ ] Prefix listing returns keys in correct order with correct origin levels.
- [ ] A deleted key shows as a tombstone when shadows are enabled and is absent when disabled.
- [ ] Bulk delete shows an accurate "N keys, X bytes" confirmation before executing.

---

## Phase F — WAL replay + MANIFEST (S)

**Goal:** forensic scrubbing in the browser.

### Tasks

1. Refactor `DumpWAL` (design doc §6.3) so the report is reusable as JSON while the CLI behavior
   and exit codes are unchanged. Keep existing CLI tests green.
2. `GET /api/v1/wal/segments`, `GET /api/v1/wal/dump`, `GET /api/v1/manifest`.
3. Frontend WAL page: segment list, record timeline with filters, replay-to-LSN simulation,
   corruption highlighting, raw record hex.
4. Tests: report JSON shape; CLI parity test.

### Acceptance criteria

- [ ] Existing `dump_wal` CLI tests still pass unchanged.
- [ ] The UI shows a deliberately corrupted segment with the exact failing record highlighted.
- [ ] Replay simulation correctly distinguishes records covered by a checkpoint.

---

## Phase G — Lab: workloads, compaction, crash & recover (M)

**Goal:** the highest-value interview demo.

### Tasks

1. `internal/admin/lab.go` — workload jobs (bounded goroutine pools, cancellable,
   rate-limited), compaction/flush triggers, orphan cleanup, diagnostics bundle.
2. `POST /api/v1/lab/crash` — scheduled `syscall.Kill(os.Getpid(), syscall.SIGKILL)` after a
   countdown, guarded by admin role plus confirmation. **Never** trigger this from a page load
   or a background job; only from an explicit POST.
3. Recovery reporting: capture the outcome of `RecoverWAL` (segments replayed, records applied,
   checkpoint-truncated records, final sequence number) and serve it at
   `POST /api/v1/lab/recover-report`.
4. Frontend Lab page: workload controls with live charts, compaction lab, crash-and-recover with
   a clear countdown and a post-restart report card.
5. Tests: crash path covered by a subprocess-based test (never crash the test binary itself —
   reuse the pattern in `cmd/lattice/chaos_sigkill_test.go`); workload job cancellation tests.

### Acceptance criteria

- [ ] `go test -race -short ./...` passes with no leaked goroutines after job cancellation.
- [ ] The crash → restart → recovery flow produces a report showing keys intact and sequence
      numbers consistent.
- [ ] The recovery report matches what `lattice dump-wal` shows for the same data directory.
- [ ] The Lab page is impossible to trigger accidentally (requires admin plus typed confirmation).

---

## Phase H — Console REPL, System/security, diagnostics (M)

**Goal:** the complete operator console.

### Tasks

1. `POST /api/v1/console/exec` — execute one parsed CLI command. **Reuse** `cmd/lattice-cli`'s
   parser semantics rather than reimplementing them; extract shared parsing into an importable
   package if required, keeping `lattice-cli` behavior identical.
2. Multi-node target switcher.
3. System page: effective configuration with provenance, security posture, pprof deep links.
4. Diagnostics bundle download.
5. Tests: parser-parity tests, config provenance tests.

### Acceptance criteria

- [ ] Browser REPL supports `GET/PUT/DELETE/EXISTS/BATCH/STATS/HELP` with identical semantics to
      the CLI REPL.
- [ ] Sending a write to a follower surfaces the redirect in the UI.
- [ ] The security posture page correctly warns when `--insecure-transport` is active.

---

## Phase I — Polish, docs, and interview readiness (S)

### Tasks

1. Empty states, loading skeletons, error boundaries, responsive layout, keyboard navigation.
2. Accessibility pass: contrast, focus rings, non-color status encoding.
3. `README.md` section with screenshots and a 60-second demo script.
4. `docs/DEMO.md` — the full demo ladder, now UI-assisted.
5. Update `docs/known-limitations.md` with the console's security posture.
6. Record a short screencast: Overview → LSM tree → SSTable CRC table → Raft failover → crash &
   recover.

### Acceptance criteria

- [ ] A cold `git clone && go build && ./lattice --admin-address …` shows a working console.
- [ ] Every page has been manually verified against a live daemon and a live 3-node cluster.
- [ ] `go vet ./...`, `go build ./...`, `go test -race -short ./...` all pass.

---

## Effort summary

| Phase | Theme | Effort | Cumulative demo value |
|---|---|---|---|
| A | Admin server skeleton | M | `curl` proves the API exists |
| B | Frontend shell, Overview, Metrics | L | First real screens |
| C | LSM Tree + SSTable Inspector | L | **Signature differentiator** |
| D | Raft visibility | M | Consensus becomes observable |
| E | Merge iterator + Keyspace | L | Data browsing + a real engine feature |
| F | WAL + MANIFEST | S | Forensic completeness |
| G | Lab / crash & recover | M | **Best single demo** |
| H | REPL, System, diagnostics | M | Full operator console |
| I | Polish and docs | S | Interview-ready |

**Total: ~6–8 weeks part-time.** Phases A+B+C alone (roughly 3 weeks) already produce a console
worth leading an interview with.

---

## Suggested starting slice (first weekend)

If you want to see something on day two, do **Phase A only**:

1. `internal/admin` server with `/api/v1/health`, `/api/v1/node`, `/api/v1/metrics`.
2. `--admin-address` flag with loopback enforcement.
3. `SnapshotJSON()` in `internal/metrics`.
4. Handler tests.

That is roughly 200 lines of Go, proves the architecture, and creates the foundation every later
phase builds on. Then move to Phase B and get the Overview page on screen.

---

## Decisions to make before starting Phase A

1. **Port and flag name.** Proposed `--admin-address`, default off. Confirm it does not collide
   with `--metrics-address` or `--pprof-address` conventions.
2. **`ForensicReport` relocation.** Recommended: move it to a shared package so the CLI and the
   admin API share one implementation. Confirm this is acceptable given that the CLI's fuzz tests
   (`inspect_fuzz_test.go`, `dump_wal_fuzz_test.go`) must keep compiling.
3. **Frontend build policy.** Recommended: commit `web/dist` so `go build` works without Node.js
   installed. Confirm.
4. **Whether the admin server should require mTLS in v1.** Recommendation: no — loopback plus a
   confirmation header, documented as a limitation. Confirm.
5. **Phase E timing.** The merge iterator is engine work that stands on its own merits. It can be
   merged and reviewed independently of the UI if time is short.

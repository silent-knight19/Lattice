# SEC-7 — Implementation & Verification Record

> Task: SEC-7 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/limits.go` | `BodyLimit`, `HandlerTimeout`, `MethodGuard`, `MaxBytesError`, `bodyLimitFor`, per-route caps |
| `internal/admin/httptimeout.go` | `ServerTimeouts`, `ServerTimeoutConfig`, `NewHTTPServer` |
| `internal/admin/limits_test.go` | Cap derivation, oversize rejection, deadline behaviour, method enforcement |

Package total: **292 cases**, `-race` clean, **zero third-party dependencies**.

---

## Caps derive from the engine, not from round numbers

| Route | Cap | Source |
|---|---|---|
| `/keys/put`, `/keys/delete` | **4,260,863 B** (~4.16 MiB) | `binary.MaxKeyLen (65535) + binary.MaxValueLen (4 MiB) + 1 KiB JSON overhead` |
| `/console/exec` | 64 KiB | a console command is a short text line |
| `/lab/workload/start` | 8 KiB | a small parameter object |
| any other route | 5 MiB | matches the binary transport's `MaxPayloadLength` |

The key-write cap is the important one: it is derived from the **storage layer's own limits**
(`internal/binary/validate.go`), so the API rejects nothing the engine would accept anyway.
A hand-picked "1 MiB is plenty" would have been a latent bug that only surfaced when someone
stored a legitimate large value.

---

## Verification — live server

| Test | Result |
|---|---|
| 4 KB body → `/lab/workload/start` | `{"read":4096,"status":"READ_OK"}` |
| 64 KB body → `/lab/workload/start` | `{"read":8192,"status":"REJECTED_413"}` — cut at exactly 8192 |
| 128 KB body → `/console/exec` | `{"read":65536,"status":"REJECTED_413"}` — cut at exactly 65536 |
| 3 MB legitimate key/value → `/keys/put` | `{"read":3000025,"status":"READ_OK"}` |
| `GET` on a POST-only route | **404** (router keys on method+path); handler never ran |
| `GET/PUT/DELETE/TRACE/OPTIONS` via `MethodGuard` | **405** + `Allow: POST`; handler never ran |

The critical property is that the read **stops at the cap** rather than buffering the whole
body and then complaining. `http.MaxBytesReader` bounds the read at the transport level, so a
handler that ignores the limit still cannot exceed it.

---

## Server timeouts (subtask 7.2)

| Field | Value | Why |
|---|---|---|
| `ReadHeaderTimeout` | 5s | **Slowloris.** Without it a client can hold a connection open indefinitely by dribbling headers. Most important field for a loopback service a hostile page can reach. |
| `ReadTimeout` | 15s | Bounds header+body arrival; a legitimate 4 MiB write needs time |
| `WriteTimeout` | **0 (disabled)** | **SSE-critical.** A non-zero value severs every `/api/v1/events` connection after that duration |
| `IdleTimeout` | 30s | Reclaims keep-alive connections |
| `MaxHeaderBytes` | 1 MiB | Matches existing servers |

`WriteTimeout` being zero is not an oversight — it is the reason `HandlerTimeout` exists as a
separate, per-request mechanism that exempts streaming paths.

---

## Deadlines are cooperative, and documented as such

`HandlerTimeout` attaches a `context.WithTimeout` to the request context. Every engine and
Raft call in this codebase takes a `context.Context`, so they honour it — verified by
`TestHandlerTimeout_CancelsSlowHandler`, which asserts a blocked handler is actually
cancelled.

A handler that ignores `ctx` would still run to completion. That is a **cooperative limit,
not a hard kill**: it is defence in depth against an accidental hang, not a substitute for
bounded work. Stated here so the guarantee is not over-read.

---

## Method enforcement (subtask 7.4)

Two layers, deliberately:

- The **router** keys routes on method+path, so `GET /keys/put` is simply an unregistered
  route → **404**. Correct, and it leaks nothing about which methods exist.
- **`MethodGuard`** emits the standards-correct **405 with an `Allow` header** for endpoints
  that declare their methods, and is the explicit "never let a GET reach a mutating handler"
  rule.

`TRACE` is rejected by `MethodGuard` alongside the others.

---

## A blocking failure that was not mine

Mid-task, `internal/transport` stopped compiling — `undefined: isLoopbackAddress` from a
concurrent session's in-progress edit. I verified this was **not** caused by my changes by
stashing `internal/admin` entirely and rebuilding: the failure was identical.

Since `internal/admin/authz.go` aliases the transport role types, this blocked verification.
Rather than idle or (worse) "fix" someone else's file, I:

1. Built an isolated harness in `/tmp` with a minimal local stand-in for the transport
   symbols, copied the **real** SEC-7 sources unmodified, and ran the full suite there
   (**241 cases, `-race` clean**).
2. Waited for `internal/transport` to build again, then re-verified in the **real** package
   (**292 cases, `-race` clean**).

Editing another session's half-finished file would have caused a lost-update conflict. The
isolated harness gave real coverage of my own code without touching theirs.

---

## Two bugs in my own tests

1. **`io.ReadAll` returns `[]byte`, not `string`** — a multiple-assignment type error caught
   by `go vet`.
2. **A cap-boundary test was wrong.** I asserted a 1-byte cap on a 1-byte body produces a
   `MaxBytesError`. It does not: a body of *exactly* the cap is legal. Fixed to use a 2-byte
   body, and I added a positive boundary case asserting that a body of exactly `cap` bytes
   **succeeds** — proving the limit is inclusive and does not reject legitimate payloads
   sitting precisely on the line.

That second one is worth flagging: had the assertion passed for the wrong reason, it would
have masked an off-by-one that rejected valid maximum-size keys.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 7.1 `MaxBytesReader` per route, engine-derived | ✅ verified at 8192 / 65536 / 4.16 MiB |
| 7.2 Server timeouts, `WriteTimeout` 0 for SSE | ✅ `TestServerTimeouts_*` |
| 7.3 Per-handler deadlines, streaming exempt | ✅ cancellation + exemption tests |
| 7.4 Explicit method rejection | ✅ 404 via router, 405 via `MethodGuard` |
| oversize body → rejected | ✅ |
| no unbounded `io.ReadAll` on bodies | ✅ no occurrences in the package |

**SEC-7 acceptance criteria: met.**

---

## Remaining

- **SEC-8** must add the SSE connection budget; SEC-7 deliberately left streaming unbounded
  so it could be governed in one place.
- **SEC-6** is partially satisfied already: `BindLoopback` performs the pre-bind and
  post-bind loopback enforcement SEC-6.1–6.3 describe. What remains is the flag wiring and
  the loud startup warning (ADM-7).

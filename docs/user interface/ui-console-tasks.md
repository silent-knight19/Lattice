# Lattice Console — Master Task & Subtask Breakdown

> Derived from [`ui-console-design.md`](./ui-console-design.md) and
> [`ui-console-plan.md`](./ui-console-plan.md).
> **This is the executable work breakdown. No code written yet.**

---

## 0. How to read this document

**Format.** Every unit is `ID → Subtask`. Each subtask states its file, its *security goal*
(always present), the concrete action, the test that proves it, and a done-condition.

**Ordering rule.** Part 0 (Security Spine) is a **hard gate**: no feature task may begin until
the spine tasks it depends on are green. Security is not a phase at the end — it is built
first and re-verified continuously.

**Definition of Done (DoD), applies to every subtask without exception:**

1. `go build ./...` clean.
2. `go vet ./...` clean.
3. New code has tests; security-sensitive code has *negative* tests (attack cases).
4. `go test -race -short ./...` green from Task TB-1 onward.
5. No new third-party Go dependency (`go.mod` stays dependency-free).
6. No secret, key material, or absolute host path in any API response or log line.

**Effort key.** `S` ≈ half-day · `M` ≈ 1–2 days · `L` ≈ 3–5 days.

---

## 1. Findings from the pre-implementation security review

These were discovered by reading the existing code *before* planning. They change the design
and are the reason Part 0 exists.

| # | Finding | Evidence | Consequence |
|---|---|---|---|
| F1 | **No CSRF / Origin defence exists anywhere in the project.** A web page the operator visits can `fetch('http://127.0.0.1:7070/...')` and the browser will happily send it — loopback binding does **not** stop this. | `grep -rn 'Origin\|Referer\|CSRF' internal/ cmd/` → no matches | Console is browser-facing and has mutating endpoints. **Drive-by localhost attack is the top threat.** New spine tasks SEC-1..SEC-4 are mandatory, not optional. |
| F2 | **No CSP, no `X-Frame-Options`, no `Referrer-Policy` anywhere.** Only `X-Content-Type-Options: nosniff` on 3 metrics handlers. | `grep -rn 'Content-Security-Policy'` → no matches | Clickjacking + XSS blast radius. SEC-5 is mandatory. |
| F3 | **Request bodies are unbounded** on `pprof.go` and `metrics/server.go` (only `MaxHeaderBytes` set). | `internal/metrics/server.go:168-175`, `cmd/lattice/pprof.go:83-88` | Admin server must use `http.MaxBytesReader` on every body. SEC-7. |
| F4 | **SSE holds a goroutine + TCP connection per client.** Existing `connLimiterListener` pattern exists but is tuned for short scrapes (256). | `internal/metrics/server.go:63-98` | Long-lived SSE must have its **own much lower cap** or it starves the API. SEC-8. |
| F5 | **`security.ResolvePath` exists and is strong** (containment + symlink canonicalisation + `evalDeepestExistingAncestor`), plus `ValidateDatabaseFileName` allowlist `^[a-zA-Z0-9_.-]+$`. | `internal/security/path.go:22,77,217` | File endpoints must compose **both** plus a file-type allowlist. TC-2. |
| F6 | **No exported engine iterator.** `Engine` has only point lookups. | `grep 'func (e *Engine) \(Iter\|NewIter\|Scan\|Range\)'` → none | Phase E requires real engine work (TE-1). |
| F7 | **`raft.Node` already exposes nearly everything needed** — `Role`, `Term`, `LeaderID`, `CommitIndex`, `QuorumSize`, `GrantedVotesCount`, `NextIndex`, `MatchIndex`, `Storage()`, `ClusterStats()`, `Topology()`, `LastApplied`, `ActiveReadRoundsCount`. `Storage` has `HardState()`, `Entries(from,to)`, `LastIndex()`. | `internal/raft/node.go`, `internal/raft/storage.go` | Design-doc §6.4 is **much smaller than estimated** — mostly assembly, not new plumbing. |
| F8 | **`Version.Files(level)` already returns a deep defensive copy** and `Version` is refcount-pinned via `TryRef()`. | `internal/version/version.go:85,180` | LSM tree endpoint can read safely if it pins the Version. Must not skip `TryRef`. TC-1. |
| F9 | **Post-bind loopback verification is the established pattern** (defends against `0.0.0.0`/`::` surprises). | `cmd/lattice/pprof.go:50-56` | Admin server must replicate it verbatim. TA-2. |
| F10 | `go.mod` has **zero** third-party requires; Go 1.23 declared, toolchain 1.27.1. | `cat go.mod` | Preserved as a hard constraint. |
| **F11** | **`CleanAndValidatePath` does not block traversal** — `../../etc/passwd` is accepted. Only `ResolvePath`/`ValidateContainment` enforce containment. | SEC-0.3 empirical probe | File endpoints need a **triple gate**; reusing CLI path handling over HTTP would be a critical arbitrary-file-read. |
| **F12** | **JS-visible test results are untrustworthy.** Probes reported "Failed to fetch" while the server had already executed the mutating request. | SEC-0.1 server-side log | All SEC-12 security assertions must be made **server-side**, never from the browser. |

### 1.1 Threat model (console-specific)

| Threat | Severity | Primary control |
|---|---|---|
| **Drive-by localhost** (any website triggers admin actions on operator's browser) | **Critical** | SEC-1 Origin allowlist, SEC-2 Host validation, SEC-3 CSRF token, SEC-4 no-CORS invariant |
| **DNS rebinding** (attacker domain resolves to 127.0.0.1, Origin looks same-origin) | **Critical** | SEC-2 Host header pinned to loopback + expected port |
| **XSS via stored key/value bytes** | **Critical** | SEC-5 CSP, no `dangerouslySetInnerHTML` (TB-2), escaping mirrors `FormatBytes` |
| **Clickjacking** (operator tricked into clicking Lab "crash") | High | SEC-5 `X-Frame-Options: DENY` + `frame-ancestors 'none'` |
| **Path traversal / symlink escape** via `?file=` | High | TC-2 triple-gate: filename allowlist + `ResolvePath` + type allowlist |
| **SSE connection exhaustion** | High | SEC-8 separate low cap + per-IP cap |
| **Resource exhaustion** (huge prefix scan, unbounded body, expensive inspect) | High | SEC-7 body caps, TC-6 scan caps + context timeouts on every handler |
| **Local privilege escalation** (any local user hits loopback) | Medium | Accepted risk — documented; unix-socket binding listed as future work (TI-4) |
| **Information disclosure** (absolute paths, TLS key contents, other tenants' data in errors) | Medium | SEC-9 error sanitiser; config endpoint redacts; no key contents ever |
| **Log injection** via attacker-controlled keys | Medium | SEC-10 structured logging, escape control chars |
| **CSRF token comparison timing leak** | Low | SEC-3 `crypto/subtle.ConstantTimeCompare` |
| **Read-only cross-origin data exfiltration** | Low | CORS invariant (SEC-1): never emit `Access-Control-Allow-Origin` |

> **Measured correction (SEC-0.2).** No-CORS headers stop an attacker *reading* a response;
> they do **not** stop them *sending* the request. A "simple" `text/plain` or form POST needs
> no preflight and is delivered unconditionally. Therefore **SEC-1 (Origin allowlist) is the
> control that actually prevents delivery**, and the no-CORS invariant is the *exfiltration*
> control — it must never be described as the CSRF defence. Preflight is not a defence either:
> the spike observed the browser send `OPTIONS` **and then still send the POST**.

**Explicitly accepted, documented risk:** loopback binding is *not* an authentication boundary
against other local users. Recorded in `docs/known-limitations.md` at TD-6.

---

# PART 0 — SECURITY SPINE (hard gate; blocks all feature work)

> Every task here lands **before** the corresponding feature task. Total ≈ `L`.
> These are real security controls, not paperwork — each has an attack test.

> ## ✅ SEC-0 COMPLETE — see [`sec0-spike-findings.md`](./sec0-spike-findings.md)
>
> The spike ran against real headless Chrome and **confirmed F1/F2/F3** and surfaced **two new
> findings** that changed the design:
>
> - **F11** — `CleanAndValidatePath` does **not** block traversal. Reusing the CLI's
>   `InspectSSTable`/`DumpWAL` behind HTTP without `ResolvePath` would have been a
>   **critical arbitrary-file-read**. The triple-gate in LSM-2.3 is now evidence-based.
> - **F12** — JS-visible results are untrustworthy: probes reported "Failed to fetch" while the
>   server had **already executed the mutation**. Every SEC-12 assertion must be server-side.
>
> **One planned assumption was disproven:** no-CORS headers do **not** block request *delivery*
> — they only block reading the *response*. So the Origin allowlist (SEC-1) is the primary
> control, and "no CORS" is the exfiltration control, **not** the CSRF defence.

## SEC-0 — Security spike: confirm the threat surface (S)

- **0.1** Stand up a throwaway PoC page on a local port that `fetch`es the admin API from a
  foreign origin. **Goal:** empirically confirm F1 (browser will send the request) before we
  rely on a theoretical defence. Record the result in the PR description.
  - *Done when:* the PoC demonstrates or refutes drive-by reachability, with a screenshot.
- **0.2** Confirm CORS default behaviour: with no `Access-Control-*` headers, verify the browser
  blocks *reading* the response cross-origin (blocks exfiltration) but *not sending* it.
  - *Done when:* both halves documented; drives whether SEC-1 needs to block or merely not-allow.
- **0.3** Re-audit `cmd/lattice/inspect.go` + `dump_wal.go` for any path handling we will
  reuse, noting exactly which validation each performs.
  - *Done when:* a table mapping "endpoint → validation primitives required" exists.

## SEC-1 — Origin allowlist middleware (S) — *Critical*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/origin.go`, `internal/admin/middleware.go`,
> `origin_test.go`, `middleware_test.go`. 100 test cases, `-race` clean.
> **Verified against the real SEC-0 attacker in headless Chrome: zero handler executions**
> (server-side ground truth per F12), while self-origin / localhost-alias / no-Origin
> requests all pass. See [`sec1-verification.md`](./sec1-verification.md).
> All subtasks 1.1–1.3 met; see that document for the attack matrix.

- **File:** `internal/admin/middleware.go`
- **1.1** Implement `OriginGuard(next http.Handler, allowedOrigins []string)`.
  - Allow requests with **no** `Origin` header (curl, native clients, same-origin GET/HEAD).
  - If `Origin` present: parse it, require exact scheme+host+port match against the
    server's own origin list. Reject otherwise with `403`.
  - Never perform substring/suffix matching — `evil-127.0.0.1.attacker.com` must fail.
- **1.2** Apply to **all** `/api/v1/*` routes, not just mutating ones.
- **1.3** **Hard invariant, enforced by a test:** the admin server must never emit any
  `Access-Control-Allow-*` header. Add a test that walks every route and asserts absence.
- **Tests (attack cases):** `Origin: http://evil.com` → 403 · `Origin: https://localhost:7070`
  on an http listener → 403 (scheme mismatch) · `Origin: null` → 403 · absent Origin → 200 ·
  `Origin: http://127.0.0.1:7070` when bound to 127.0.0.1:7070 → 200.

## SEC-2 — Host header validation / DNS-rebinding defence (S) — *Critical*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/hostguard.go`, `internal/admin/bind.go`,
> `hostguard_test.go`, `bind_test.go`. Package total: 162 cases, `-race` clean.
> **Verified on two layers:** a rebinding request (`Host: attacker.example` with a
> self-consistent Origin) is refused **400** before reaching any handler; the real
> SEC-0 drive-by attacker in headless Chrome produced **zero executions**. A legitimate
> console request still returns **200**. See [`sec2-verification.md`](./sec2-verification.md).
> All subtasks 2.1–2.3 met.

- **File:** `internal/admin/middleware.go`
- **2.1** Implement `HostGuard(next, allowedHosts []string)`.
  - Derive the allowed host set from the **actual bound listener address**, not from config
    (so `--admin-address 127.0.0.1:7070` allows only `127.0.0.1:7070`, `[::1]:7070`).
  - Compare `r.Host` case-insensitively after normalising the port; reject if absent or unlisted.
  - This defeats DNS rebinding: the attacker's page has `Host: evil.com` even though the TCP
    connection reached 127.0.0.1.
- **2.2** Apply to all routes **and** to the SSE endpoint.
- **2.3** Post-bind verification copied from `cmd/lattice/pprof.go:50-56`: after `net.Listen`,
  assert the resolved `*net.TCPAddr.IP.IsLoopback()` when a loopback bind was requested.
  - Rationale: `Addrs()`/config can lie; the kernel result is truth. Defence in depth.
- **Tests:** `Host: evil.com` → 400 · `Host: 127.0.0.1:9999` (wrong port) → 400 ·
  correct host → 200 · wildcard-bind attempt without `--insecure-transport` → startup error.

## SEC-3 — CSRF token for mutating requests (M) — *Critical*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/csrf.go`, `internal/admin/respond.go` (shared rejection
> helpers, per SEC-9), `csrf_test.go`. Package total: **189 cases**, `-race` clean.
> **Verified in real Chrome against the full SEC-1+2+3 stack:** a legitimate same-origin write
> returns **200**; a token-without-confirm and a confirm-without-token both return **403**;
> **zero** forgeries reached the handler across every run.
> **Two real bugs were caught by the tests** (a `GET /session` panic and a token-validation
> defect that would have bricked the console). See [`sec3-verification.md`](./sec3-verification.md).
> Subtasks 3.1–3.6 met.

- **Files:** `internal/admin/csrf.go`, `cmd/lattice/admin.go`
- **3.1** Generate a 256-bit cryptographically random token at **daemon startup**
  (`crypto/rand`). Never derive from time/PID (predictable).
- **3.2** Serve it **only** via `GET /api/v1/session`, which is itself protected by SEC-1 + SEC-2.
  Bind it to server start time so a restart invalidates outstanding tokens.
- **3.3** Require `X-Lattice-CSRF: <token>` on every non-GET/HEAD request, in addition to the
  `X-Lattice-Admin: confirm` header from the design doc. **Both** headers required.
- **3.4** Compare with `crypto/subtle.ConstantTimeCompare` (no early return).
- **3.5** Return `403` with a generic body. **Never** echo the expected or received token.
- **3.6** Token must never appear in logs, and the `/session` response must set
  `Cache-Control: no-store`.
- **Tests:** missing header → 403 · wrong token → 403 · token from a previous daemon run → 403 ·
  correct token → 200 · response body of a failed attempt contains neither token.

## SEC-4 — Admin authorisation wiring (M) — *Critical*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/authz.go`, `internal/admin/router.go`,
> `internal/admin/routes.go`, `authz_test.go`. Package total: **240 cases**, `-race` clean.
> **Verified against the real route plan with a `reader` principal and a valid CSRF token:**
> every read route returned 200, and all destructive routes returned 403 with **exactly one**
> handler execution server-side. See [`sec4-verification.md`](./sec4-verification.md).
> **A design weakness was found and fixed during implementation:** the declared `Permission`
> was initially decoration, unenforced by the router. It now enforces its own declared
> permission and fails closed with no resolver. Subtasks 4.1–4.6 met.

- **Files:** `internal/admin/authz.go`
- **4.1** Reuse `transport.Role`, `transport.ParseRole`, `transport.RolePermissions`,
  `transport.PermissionAdmin` (verified present in `internal/transport/authz.go`).
  **Do not invent a parallel role system** (design-doc NG1).
- **4.2** Define an explicit permission mapping for every admin route:
  - read-only routes → `PermissionRead`
  - key mutation, compaction/flush triggers → `PermissionWrite`
  - raft campaign/stepdown, lab crash, orphan cleanup, diagnostics → `PermissionAdmin`
- **4.3** **Default-deny routing:** the router must have a catch-all that returns 404 for
  unregistered `/api/v1` paths, and a middleware that rejects any route not explicitly
  registered *with* a required permission. Fail closed on missing metadata.
- **4.4** Fail-closed on unknown roles: an unrecognised role string grants **nothing**.
- **4.5** Support `--admin-authz-policy` / `--admin-authz-policy-file` reusing the existing
  fingerprint→role file format, so mTLS operators keep working unchanged.
- **4.6** When TLS is configured, prefer deriving the role from the verified client
  certificate (`transport.CertificateFingerprintSHA256` + `AuthorizeRole`) over any header.
- **Tests:** `reader` hitting a `PermissionWrite` route → 403 · unknown role → 403 ·
  unregistered `/api/v1/x` → 404 (not 200, not 500) · every route has explicit permission
  metadata (table-driven completeness test).

## SEC-5 — Security response headers + CSP (S) — *High*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/headers.go`, `headers_test.go`. Package total: **281 cases**,
> `-race` clean.
> **Verified A/B in real Chrome:** identical injected-script markup **executes** without the
> headers and is **blocked** with them. `img onerror` and `eval()` were also blocked.
> See [`sec5-verification.md`](./sec5-verification.md). Subtasks 5.1–5.3 met.
> **Recorded consequence:** CSP forbids `unsafe-inline`, so the SPA build must use bundled CSS
> (or a nonce) for any dynamic styling — carried into FE-3.

- **File:** `internal/admin/middleware.go`
- **5.1** `SecurityHeaders(next)` applied to **every** response including errors and static assets:
  - `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`
    - **No `unsafe-inline`, no `unsafe-eval`.** Inline styles must move to bundled CSS or a
      per-response nonce — decide in TB-3 and record the choice.
  - `X-Content-Type-Options: nosniff`
  - `X-Frame-Options: DENY`
  - `Referrer-Policy: no-referrer`
  - `Cross-Origin-Opener-Policy: same-origin`
  - `Cross-Origin-Resource-Policy: same-origin`
  - `Permissions-Policy: geolocation=(), camera=(), microphone=()`
  - `Cache-Control: no-store` on all `/api/v1` responses
- **5.2** Static asset serving must set correct explicit `Content-Type` per extension and must
  **not** rely on sniffing. `.js` → `text/javascript`, `.css` → `text/css`, `.svg` →
  `image/svg+xml`.
- **5.3** Add a golden test asserting the exact header set on: `/`, a 200 API response, a 403,
  a 404, and a 500.
- **Tests:** headers present on all five response classes; no `unsafe-inline` anywhere in CSP;
  `no-store` present on every API response.

## SEC-6 — Loopback-only binding enforcement (S) — *High*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `cmd/lattice/admin.go`, `internal/admin/server.go`, validation in
> `cmd/lattice/config.go`. 30 admin-wiring tests + 3 `web` tests, `-race` clean.
> **This closes Part 0.** See [`sec6-verification.md`](./sec6-verification.md).
> Subtasks 6.1–6.5 met. Delivered together with **ADM-1** (`admin.NewServer` lifecycle),
> which SEC-6 depends on, and the first slice of **ADM-7** (daemon wiring).

- **File:** `cmd/lattice/admin.go`, reuses `isLoopback` from `config.go:120`
- **6.1** Default `--admin-address` to empty (server disabled). Opt-in only.
- **6.2** Reject a non-loopback `--admin-address` unless `--insecure-transport` is set —
  identical rule to `--pprof-address`.
- **6.3** Log a **loud one-line warning at startup** whenever the admin server is enabled,
  stating that it exposes engine internals.
- **6.4** When `--insecure-transport` permits remote binding, require an **additional**
  explicit `--admin-allow-remote` flag. Two independent opt-ins cannot be triggered by accident.
- **Tests:** remote bind without the second flag → startup error · with both → starts and warns ·
  empty address → no listener · port conflict → clean fail-fast.

> **Note — deviation from the original wording.** 6.2/6.4 were specified in terms of
> `--pprof-address`'s rule, but pprof's rule is *stricter*: pprof refuses every non-loopback
> bind and has **no** `--insecure-transport` escape. Admin is deliberately the opposite — it
> permits a remote bind, but only behind **two** independent opt-ins. Matching pprof exactly
> would have made 6.4 unimplementable; matching metrics exactly would have made 6.4
> redundant. The shipped behaviour is the two-opt-in rule, verified by
> `TestAdminFlagNonLoopbackRequiresBothOptIns`.
>
> A **wildcard** bind (`0.0.0.0`/`::`) is refused outright even with both opt-ins, because it
> publishes the console on every interface and yields no derivable Host allowlist
> (`TestNewAdminServerRefusesWildcardEvenWithOptIns`).

## SEC-7 — Request size caps + handler timeouts (S) — *High*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/limits.go`, `internal/admin/httptimeout.go`,
> `limits_test.go`. Package total: **292 cases**, `-race` clean.
> **Verified live:** an 8 KiB-capped route accepted 4 KB and rejected 64 KB at exactly 8192
> bytes with a `MaxBytesError`; a 3 MB legitimate key/value was accepted; `MethodGuard`
> returned 405 + `Allow: POST` for GET/PUT/DELETE/TRACE/OPTIONS without reaching the handler.
> See [`sec7-verification.md`](./sec7-verification.md). Subtasks 7.1–7.4 met.

- **File:** `internal/admin/middleware.go`, `internal/admin/server.go`
- **7.1** Wrap every request body with `http.MaxBytesReader`. Caps:
  - key/value mutations → reuse engine `MaxKeySize`/`MaxValueSize` semantics + 1 KiB envelope
  - `console/exec` → 64 KiB
  - workload start → 8 KiB
- **7.2** Server timeouts, following `internal/metrics/server.go:168-175` and tightening:
  `ReadHeaderTimeout 5s`, `ReadTimeout 15s`, **`WriteTimeout 0` (SSS streams — see SEC-8)**,
  `IdleTimeout 30s`, `MaxHeaderBytes 1 MiB`.
- **7.3** Every non-streaming handler wraps its work in `context.WithTimeout` (default 10s) and
  returns `503`/`504` rather than hanging.
- **7.4** Reject unknown methods per route explicitly; never let a GET reach a mutating handler.
- **Tests:** oversize body → 413 · slow-body (Slowloris) → cut off at `ReadHeaderTimeout` ·
  each handler honours its deadline · no unbounded `io.ReadAll` remains (grep-check).

## SEC-8 — SSE resource governance (M) — *High*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/events.go` (bounded bus — also satisfies the core of
> FND-4), `internal/admin/sse.go` (SSE handler + budgets), `events_test.go`, `sse_test.go`.
> Package total: **310 cases**, `-race` clean.
> **Verified live:** a real SSE client received `retry: 3000`, the connect comment, and
> live `raft.transition` events as proper `event:`/`data:` frames. Under load: **accepted=3,
> refused=17** at a cap of 3; per-IP cap held at 2; **goroutines 7→3, subscribers 0** after
> disconnect. See [`sec8-verification.md`](./sec8-verification.md). Subtasks 8.1–8.6 met.

- **File:** `internal/admin/events.go`
- **8.1** **Separate connection budget.** SSE clients are long-lived; reuse the
  `connLimiterListener` pattern (`internal/metrics/server.go:63-98`) but with a low cap
  (default 8, configurable), enforced *before* any goroutine or buffer is allocated.
- **8.2** Per-IP cap (default 4) to blunt a single-host reconnect storm.
- **8.3** Hard cap on total registered subscribers; when exceeded, reject the newest with 503
  **before** allocating a channel. Never block a producer on a slow consumer — drop and count
  a `lattice_admin_events_dropped_total` counter.
- **8.4** Mandatory `r.Context().Done()` handling; release every resource on disconnect.
- **8.5** Heartbeat comment every 15s to defeat idle-proxy reaping; `retry:` field on connect.
- **8.6** Never place secrets or unbounded strings into the event payload.
- **Tests:** 100 concurrent connects → at most the cap succeed, no goroutine leak ·
  `go test -race` clean · subscriber disconnect releases its slot · slow consumer causes drops,
  not producer blocking.

## SEC-9 — Error sanitisation (S) — *High*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/errors.go`, `internal/admin/requestid.go`, and
> `respond.go` (unified onto one envelope writer), `errors_test.go`.
> Package total: **339 cases**, `-race` clean.
> **Verified live:** a maximally hostile `InvalidPathError` (absolute SSH key path + root +
> symlink reason) produced `{"code":"invalid_path","message":"the supplied path was rejected"}`
> with **zero** leakage of any of the 6 secret markers, while the **server log captured the
> full detail keyed by `X-Request-Id`**. See [`sec9-verification.md`](./sec9-verification.md).
> Subtasks 9.1–9.4 met.

- **File:** `internal/admin/respond.go`
- **9.1** Every error response uses a stable, opaque envelope:
  `{"error":{"code":"invalid_request","message":"<static text>"}}`.
- **9.2** Map internal errors to codes via `errors.Is` against the sentinels in
  `internal/errors` (e.g. `*errors.InvalidPathError` → `invalid_path`). Never forward
  `err.Error()` verbatim for internal errors.
- **9.3** **Never** include absolute filesystem paths, key contents, TLS key material, or
  goroutine dumps in a response body. Log details server-side with the request ID instead.
- **9.4** Return a `X-Request-Id` on every response for correlation.
- **Tests:** force an internal error, assert the body contains no path and no raw Go error;
  assert a request ID is present and echoed in logs.

## SEC-10 — Structured logging with injection defence (S) — *Medium*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/log.go` (`AdminLogger`, `SanitizeLogField`, `RedactValue`,
> `AuditMiddleware`, `ErrorSink`), `log_test.go`. `events.go`'s duplicate sanitiser was removed
> in favour of the canonical one. Package total: **364 cases**, `-race` clean.
> **Verified live:** a POST whose key contained `\nINFO FORGED LOG LINE {"level":"INFO"}` produced
> **4 records, all valid single-line JSON, 0 forged lines**; the live CSRF token logged under a
> *neutral* field name was **redacted**; the audit line retained
> `request_id / method / route / decision / role / principal_fp`.
> **Probing found three real gaps in the existing logger** (documented below), and a fourth bug
> I introduced and then fixed. See [`sec10-verification.md`](./sec10-verification.md).
> Subtasks 10.1–10.4 met.

- **File:** `internal/admin/log.go`
- **10.1** Log to stderr via the existing `internal/logger`.
- **10.2** Attacker-controlled bytes (keys, values, filenames from requests) are logged only
  through a sanitiser that strips/escapes control characters and truncates (e.g. 256 bytes) —
  blocking log forging and terminal escape injection.
- **10.3** Never log CSRF tokens, TLS key contents, or full certificate bodies. Log only
  fingerprints.
- **10.4** Emit one structured audit line per mutating request: request ID, route, principal
  fingerprint, role, decision, timestamp.
- **Tests:** a key containing `\nFAKE LOG LINE` produces exactly one log line ·
  token values never appear in captured output.

## SEC-11 — Static asset serving safety (S) — *High*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/static.go`, `internal/admin/static_test.go`, and a new
> `web/` package (`web/embed.go` + committed placeholder `web/dist/`).
> Package total: **391 cases**, `-race` clean.
> **Verified live with literal unnormalised bytes (via `nc`, bypassing curl's client-side
> path normalisation):** `/../etc/passwd`, `/../../../../etc/passwd`,
> `/assets/../../../etc/passwd` and `/api/v1/../` all returned **404**; percent-encoded forms
> returned the SPA shell with **no** `/etc/passwd` content. All `/api*` paths returned **JSON
> 404, never HTML**. `index.html` served `no-store` with a content-derived ETag.
> **SEC-11.5 confirmed:** `go build ./...` succeeds with only the placeholder present and no
> Node.js installed. See [`sec11-verification.md`](./sec11-verification.md).
> Subtasks 11.1–11.5 met.

- **File:** `internal/admin/static.go`
- **11.1** Serve only from the embedded `web/dist` filesystem via `embed.FS`. **No filesystem
  path ever reaches the asset router from user input.**
- **11.2** SPA fallback: unknown non-`/api` paths return `index.html`; unknown `/api` paths
  return JSON `404` (never the SPA).
- **11.3** Reject any request path containing `..`, a null byte, or a backslash after
  normalisation.
- **11.4** Serve assets with `ETag` + `Cache-Control: public, max-age=...` for hashed files;
  `index.html` always `no-store` so a redeploy is picked up.
- **11.5** Add `go:embed` of a **minimal placeholder `dist`** so the Go build never breaks for
  someone without Node installed (build-tag or committed stub).
- **Tests:** `/../../etc/passwd` → 404/400 · `/api/v1/../` → JSON 404 · null byte → 400 ·
  `index.html` is `no-store` · build succeeds with `web/dist` absent.

## SEC-12 — Security test suite (M) — *deliverable artefact*  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `internal/admin/security_test.go` + `security_test_helpers_test.go`.
> Package total: **430 cases**, `-race` clean, **passes under `-short`** (the CI gate) in ~1.1s.
> Every attack class from SEC-0…SEC-11 is now exercised against one fully-wired server, with
> **server-side execution counters** as ground truth per F12. Includes the SEC-12.2 route
> completeness gate and the SEC-12.3 CORS invariant.
> See [`sec12-verification.md`](./sec12-verification.md). Subtasks 12.1–12.4 met.

- **File:** `internal/admin/security_test.go`
- **12.1** One table-driven test file that runs the **entire** attack suite against a live
  server: drive-by origin, rebinding host, missing/wrong CSRF, wrong role, traversal, symlink
  escape, oversize body, SSE flood, header presence, error leakage.
- **12.2** Include a `TestNoRouteExistsWithoutPermissionMetadata` completeness test so a new
  route cannot be added without declaring its permission.
- **12.3** Include a `TestNoCORSHeadersEverEmitted` invariant test.
- **12.4** Wire into `go test -race -short ./...`.
- **Done when:** this file is the regression net for every later feature task, and each feature
  task adds its own cases here.

## SEC-13 — Security documentation (S)  ✅ **COMPLETE**

> **Status: DONE.** `SECURITY.md` gained **Trust Boundary 5** and a new §6 operator checklist;
> `docs/threat-model.md` gained **Threats 16–20**; `docs/known-limitations.md` gained
> **limitations 102–107**, including an explicit statement that the admin server is **not yet
> wired into the daemon** (ADM-7), so no claim in these documents implies it is reachable today.
> Every test name and Go symbol cited was verified to exist. Subtasks 13.1–13.3 met.

- **File:** `internal/admin/log.go`

- **13.1** Add a console section to `SECURITY.md` covering the threat model and controls.
- **13.2** Record the accepted risks explicitly in `docs/known-limitations.md`:
  loopback ≠ authentication against local users; CSRF token is per-daemon-boot;
  no multi-user isolation in the console.
- **13.3** Document the operator checklist: bind loopback, prefer mTLS + admin role, treat the
  console as privileged, do not expose it.

---

# PART 1 — FOUNDATIONAL BACKEND PREREQUISITES

> Independent of the UI. Each is valuable engine/tooling work on its own merits and can be
> reviewed and merged separately — which is a deliberate hedge against Phase E slipping.

## FND-1 — `internal/metrics`: JSON snapshot (S)

**Why first:** every dashboard depends on it, and it is a pure addition with no risk.

- **1.1** `SnapshotJSON()` on `*Registry` returning counters, gauges, and per-metric histogram
  quantiles (count, sum, bucket counts, p50/p95/p99 where computable from the histogram).
- **1.2** Reuse the existing `Histogram.Snapshot()`; do not re-derive bucket boundaries.
- **1.3** **Security:** metric *labels* are internal identifiers only. If any label value could
  ever carry user data, it must be escaped. Add a test asserting no label value contains
  `<` or control characters.
- **1.4** Bound the response: a large `HistogramVec` (e.g. per-level, per-op) must not produce
  an unbounded document. Cap emitted series and say so in the payload.
- **Tests:** JSON round-trips; quantile maths matches the existing Prometheus exposition test
  fixtures; empty registry yields valid empty JSON.

## FND-2 — Shared forensic inspection package (M)

**Goal:** stop the admin API from reimplementing `inspect.go`, and make it reusable.

- **2.1** Create `internal/inspect` and move `ForensicReport`, `DataBlockInfo`,
  `BloomFilterInfo`, `InspectSSTable`, `FormatBytes` there.
- **2.2** Keep `cmd/lattice/inspect.go` as a **thin wrapper** so CLI output and exit codes
  (`ExitInspectSuccess`=0, usage=1, file=2, corrupt=3) are byte-identical.
- **2.3** Update `inspect_test.go` and `inspect_fuzz_test.go` to the new import path. Fuzz
  corpus and seeds must be preserved — they are the regression net for path handling.
- **2.4** Add JSON struct tags to the report types for clean serialisation (avoid a parallel
  DTO that can drift).
- **2.5** **Security:** keep `security.CleanAndValidatePath` + `sstable.ValidatePathNoSymlinks`
  inside the moved function — defence must not be lost in the refactor. Add a test that runs
  the *moved* function against the traversal corpus from `security/path_test.go`.
- **2.6** `FormatBytes` stays the single escaping primitive; add a companion
  `FormatBytesJSON`-safe path if needed rather than a second, laxer escaper.

## FND-3 — `DumpWAL` refactor to a report (M)

- **3.1** Extract report-building from `DumpWAL(path, verbose, stdout, stderr) int` into
  `BuildDumpReport(path string, verbose bool) (*DumpWALReport, error)`; `DumpWAL` becomes a
  thin printer over it.
- **3.2** CLI output and exit codes must be **provably unchanged** — snapshot existing test
  expectations before touching anything.
- **3.3** **Security:** report may contain raw key bytes. Add a size cap on total records
  returned and a per-record value-length cap so a maliciously large WAL cannot produce a
  multi-GB response.
- **3.4** Preserve `dump_wal_fuzz_test.go` seeds across the refactor.

## FND-4 — `internal/events`: bounded ring buffer (M)

- **4.1** `Bus` with a fixed-capacity ring (default 1000, **pre-allocated at construction**)
  plus subscriber channels.
- **4.2** `Publish` is **non-blocking**: if a subscriber is full or slow, drop the event and
  increment a counter. **A slow SSE client must never block a compaction or Raft goroutine.**
- **4.3** Cap subscriber count; reject beyond it *before* allocating.
- **4.4** Typed events: raft transition, compaction start/finish/fail, flush, WAL rotation,
  recovery start/finish, orphan-clean result.
- **4.5** **Security:** events may embed key bytes or paths → run every payload field through
  the SEC-10 sanitiser and truncation at publish time, not at render time.
- **4.6** Provide `Recent(n)` for the Overview feed.
- **Tests:** ring wraparound; oldest-dropped-first; producer never blocks with a wedged
  subscriber; subscriber cap enforced; `-race` clean.

## FND-5 — Raft introspection: close the small gaps (S)

> Largely assembly — F7 shows most accessors already exist.

- **5.1** Add `raft.Node.HardState()` delegating to `Storage.HardState()` (F7).
- **5.2** Add a bounded `raft.Node.LogPage(from, to LogIndex, max int)` over
  `Storage.Entries(from, to)`, with `max` enforced server-side so a client cannot request the
  entire log.
- **5.3** Install the existing `TransitionHook` (`internal/raft/node.go:24`) into the FND-4 bus
  by default.
- **5.4** **Security:** the log contains **committed command payloads = user key/value data**.
  The admin endpoint must apply the same role gating (`PermissionAdmin`) as reading keys, and
  the log page must be size-capped. Note this explicitly in the API docs — a log viewer is a
  data-exfiltration surface, not just a debug view.

## FND-6 — Engine merge iterator (L) — *the big one*

**Why standalone:** this is real engine work, useful without any UI. Merge and review
independently if time is short.

- **6.1** `internal/engine/iterator.go` — merged iteration in precedence order:
  active memtable → immutable memtables (newest first) → L0 (descending seqnum) → L1..Ln
  (ascending file order).
- **6.2** **Version pinning.** Hold a pinned `*version.Version` for the iterator's lifetime via
  `TryRef()` (F8) and release with `Unref()` on `Close()`. The iterator must be safe against
  concurrent compaction reclaiming the version — test this under `-race` with compaction
  running concurrently.
- **6.3** Collapse to the newest version per user key; honour tombstones (a tombstone at a
  newer seqnum hides older values).
- **6.4** `WithShadows(bool)` exposes superseded entries and tombstones for the forensics view.
- **6.5** Emit an **origin descriptor** per key: `{kind: memtable|immutable|l0..ln, level, fileNum}`.
- **6.6** **Security-relevant properties (treat as correctness requirements):**
  - Bounded work: a scan must respect a caller-supplied limit *and* a byte budget; it must be
    cancellable via context. A full-keyspace scan is a DoS vector against the live engine.
  - Keys are attacker-controlled bytes; the iterator must not assume printable/UTF-8.
  - No panic on adversarial ordering — return errors, never panic on malformed internal state.
- **6.7** Tests: correctness vs. a naive `map` oracle; tombstone handling; L0 descending-seqnum
  ordering; shadow mode; version-pinning under concurrent compaction; `-race` clean;
  cancellation mid-scan; limit respected.

---

# PART 2 — ADMIN SERVER (Phase A)

**Depends on:** all of Part 0, FND-1. **Blocks:** every feature phase.

## ADM-1 — Server skeleton and lifecycle (M)

- **1.1** `internal/admin/server.go` — `Server{engine, raft, nodeID, cfg, csrfToken, bus, ...}`.
  Model the lifecycle on `internal/metrics/server.go` (bind synchronously in the constructor so
  port conflicts fail fast, `Start()` spawns the accept goroutine, `Shutdown(ctx)` drains).
- **1.2** `NewServer` binds synchronously. Port-in-use must fail at construction, not later.
- **1.3** `Start` / `Shutdown` idempotent, guarded by `atomic.Bool` + mutex exactly like
  `PprofServer` (`cmd/lattice/pprof.go:97-118`).
- **1.4** Shutdown must stop accepting, drain in-flight requests within the deadline, and close
  **all** SSE streams promptly (they hold no DB locks, so they close fast).
- **1.5** **Security:** `Shutdown` closes the CSRF token — no new mutating request may succeed
  after shutdown begins.
- **Tests:** double-Start rejected; Shutdown-before-Start safe; double-Shutdown safe;
  post-shutdown requests fail; SSE clients disconnected on shutdown; `-race` clean.

## ADM-2 — Binding + middleware chain (M)

- **2.1** Reuse `isLoopback` (`cmd/lattice/config.go:120`) and apply SEC-6 rules.
- **2.2** Wrap the listener with a `connLimiterListener` (pattern from
  `internal/metrics/server.go:63-98`) — but with a **higher** cap than the metrics server since
  a browser opens several connections; default 64, configurable.
- **2.3** Assemble the middleware chain in a fixed, reviewable order:
  ```
  Recoverer → SecurityHeaders → HostGuard → OriginGuard
            → conn/body caps → authz → CSRF (mutations only) → route
  ```
  Document **why** each sits where it does. `Recoverer` outermost so it also catches panics in
  inner middleware.
- **2.4** **Security:** `Recoverer` must return the sanitised 500 envelope (SEC-9) and log the
  panic server-side. **Never** return a panic message or stack to the client.
- **2.5** `WriteTimeout` left at 0 for SSE; use per-request deadlines elsewhere (SEC-7).
- **Tests:** each middleware independently; chain order verified by a test that asserts a
  request rejected by `HostGuard` never reaches `OriginGuard`'s success path.

## ADM-3 — Router with default-deny (M)

- **3.1** `http.ServeMux` with `/api/v1/` subtree. Register `/api/v1/` and
  `/api/v1` explicitly so the catch-all 404 is JSON, never the SPA.
- **3.2** **Every** route registered with explicit `{permission, mutating bool, timeout}`.
  A registry-level lookup fails closed when metadata is absent (SEC-4).
- **3.3** Method enforcement per route (SEC-7.4) — a registered GET path must 405 on POST
  rather than falling through.
- **3.4** **Tests:** route/permission completeness; unknown path → JSON 404; wrong method → 405
  with `Allow` header; no route reachable without permission metadata.

## ADM-4 — Core read handlers (M)

- **4.1** `GET /health` — reuse `LivenessCheck`/`ReadinessCheck` semantics from
  `internal/metrics`; include recovering/closed/WAL-poisoned flags. `PermissionRead`.
- **4.2** `GET /node` — id, addresses, version, uptime, data-dir **basename only**.
  `PermissionRead`.
  - **Security:** expose the data directory's base name, never the absolute path (F3/SEC-9).
- **4.3** `GET /config` — resolved config **with provenance**, and **redacted**:
  `PermissionRead`, but responses must mask TLS private-key paths (show only the directory or
  `<redacted>`), never key contents, and never certificate PEM.
  - Record the redaction list explicitly so it is reviewable.
- **4.4** `GET /metrics` — JSON snapshot from FND-1. `PermissionRead`.
- **4.5** `GET /metrics/prometheus` — raw text passthrough; keep `nosniff`.
  **Security:** serve as an attachment-style response so a crafted metric label can never be
  interpreted as HTML by the browser (defence-in-depth behind CSP).
- **Tests:** each handler success + authz-denied + sanitised-error cases; config redaction
  asserted by scanning the marshalled body for the key path.

## ADM-5 — Event stream / SSE (M)

- **5.1** `GET /events` — SSE from the FND-4 bus plus a 1 Hz coalesced metric sample.
  `PermissionRead`; **admin role additionally required** (see 5.3).
- **5.2** Apply all of SEC-8.
- **5.3** **Security decision to record:** the event stream carries Raft transitions and metric
  samples. Require `PermissionAdmin` rather than `PermissionRead`, because the term timeline is
  operationally sensitive and this endpoint is the highest-value target for a drive-by attacker
  (an information leak with no side effects, hence easy to attempt). Document the rationale.
- **5.4** Per-connection goroutine exits on `r.Context().Done()` — no leaks.
- **Tests:** content-type `text/event-stream`; flushes arrive; disconnect frees the slot;
  flood rejected at the cap; payload sanitiser applied to a hostile key.

## ADM-6 — Static asset serving + embed (M)

- **6.1** Implement SEC-11 over `embed.FS`.
- **6.2** `go:embed` with a committed minimal stub `dist` so the Go build never breaks when
  Node is absent.
- **6.3** SPA fallback vs. JSON-404 split for `/api`.
- **6.4** **Security:** `index.html` served with the full SEC-5 header set including CSP; the
  CSP must actually permit the built bundle (test with the real build output, not just the stub).
- **Tests:** traversal, null byte, backslash; content types; cache headers; build without Node.

## ADM-7 — Daemon wiring (M)

- **7.1** `--admin-address` flag in `cmd/lattice/config.go` following the existing flag pattern
  (`fs.StringVar`, default `""`), added to the `Config` struct with a JSON tag consistent with
  its siblings.
- **7.2** `cmd/lattice/admin.go` — start alongside pprof/metrics in `daemon.go`'s ordered
  startup (Step 2b pattern at `daemon.go:381`) and shutdown in the Step-8 ordered drain
  (`daemon.go:561-604`).
- **7.3** **Unregister every gauge the admin server registers**, mirroring
  `UnregisterGaugeFunc` calls at `daemon.go:600-604`. Add a test that a second daemon start in
  the same process does not panic on duplicate registration.
- **7.4** Update the daemon doc comment (the ordered-steps list at `daemon.go:70-79`) so the
  documented startup order stays truthful.
- **7.5** Startup banner per SEC-6.3.
- **Tests:** full daemon lifecycle with the admin server enabled; clean shutdown; no duplicate
  registration panic; `known-limitations.md` updated.

**Phase A exit gate:** `curl localhost:7070/api/v1/health` returns JSON; SEC-12 suite green;
`go build`/`go vet`/`go test -race -short ./...` clean.

---

# PART 3 — FRONTEND FOUNDATION (Phase B)

> JS deps are **build-time only**. Shipped artefact is a static bundle in `go:embed`.
> The Go binary gains zero dependencies.

## FE-1 — Scaffold and build pipeline (M)  ✅ **COMPLETE + VERIFIED**

> **Status: DONE.** `web/` — Vite 8 + React 19 + TypeScript 5.9 + Tailwind 4. `npm ci && npm run build`
> produces `web/dist`, embedded by `web/embed.go` and served by the running daemon.
> See [`fe1-verification.md`](./fe1-verification.md). Subtasks 1.1–1.4 met.
> **Also fixed a blocking fresh-clone bug** (1.2-adjacent) — see the note below.

- **1.1** `web/` — Vite + React 18 + TypeScript + Tailwind. Pin exact versions in
  `package-lock.json`; commit it.
- **1.2** Build output to `web/dist`, embedded by ADM-6. Add an npm script that also runs
  typecheck + tests.
- **1.3** **Supply-chain hygiene (security):** enable `npm ci` (never `npm install`) in any
  script or CI step; commit the lockfile; document `npm ci && npm run build`; add a
  `npm audit` step. Record the accepted audit advisories rather than silently ignoring them.
- **1.4** Vite dev server must **proxy** `/api` to the daemon and must not be exposed publicly.
- **Tests:** `npm run build` succeeds; `tsc --noEmit` clean; bundle contains no unexpected
  `eval`/`new Function` (which would violate the CSP from SEC-5).

> **Deviations from the original text, each evidence-based** (full reasoning in
> `fe1-verification.md`):
>
> - **React 18 → 19.3.0.** The 19.x line has been GA since 2024-12-05, so it is the mature
>   version; "18" was simply current when this plan was written.
> - **TypeScript → 5.9.3, not 7.x.** TypeScript 7.0.2 ships **no compiler API**, and
>   typescript-eslint (which FE-2.3 needs for the `react/no-danger` XSS rule) declares peer
>   `typescript >=4.8.4 <6.1.0`. On TS 7 `npm ci` fails `ERESOLVE` and a forced install
>   crashes ESLint. TS 7 would therefore break a **security control in this very plan**.
> - **Tailwind 4.3.3 with CSS-first config.** The planned `tailwind.config.ts` is a v3
>   artefact; v4 moves configuration into `@theme` blocks in CSS. The file tree in §8.3
>   should be read accordingly.
> - **ESLint 10, not 9.** `eslint@9` prints an explicit "no longer supported" deprecation on
>   install; typescript-eslint 8.71 supports `^10`.
>
> **Blocking bug found and fixed:** `.gitignore`'s `dist/` rule excluded `web/dist`, so a
> fresh clone had **no** embedded assets and *every* `go build ./...` failed with
> `pattern all:dist: no matching files found` — including for contributors with no Node.js.
> Negation rules (`!web/dist/`, `!web/dist/**`) now keep the built bundle committed.

## FE-2 — API client and XSS-safe primitives (M) — *Critical*

- **2.1** `api/client.ts` — typed fetch wrapper; uniform error envelope; attaches the CSRF
  header on mutations **from memory only**.
- **2.2** **Security:** the CSRF token must never be written to `localStorage`, `sessionStorage`,
  a cookie, or the URL. Hold it in a module-level variable obtained from `GET /session`.
  - Rationale: any persistent store is readable by XSS and leaks across tabs/tabs-origins.
  - Add a test/grep-check that `localStorage` and `document.cookie` are never written.
- **2.3** **Security — the central XSS rule:** keys, values, filenames, and error strings are
  attacker-controlled bytes. They must be rendered **only** as React children (auto-escaped).
  - Add an ESLint rule **forbidding `dangerouslySetInnerHTML`** and a CI grep-check.
  - Add a unit test that renders a key of `<img src=x onerror=alert(1)>` and asserts it appears
    as literal text.
- **2.4** Provide a single `<ByteText>` component that renders arbitrary bytes safely:
  printable UTF-8 as text, everything else as `\xHH` escapes — mirroring `FormatBytes`
  (FND-2.6). Use it for **every** byte-origin value in the app.
- **2.5** Never build HTML strings for tooltips/labels; use JSX props.
- **2.6** Timeouts + abort on every fetch; surface a consistent error state.
- **Tests:** hostile-string rendering test; no-`localStorage` check; client adds CSRF only on
  mutations.

## FE-3 — App shell, routing, error boundaries (M)

- **3.1** Left-rail nav matching the §3.1 information architecture; a route per page with
  placeholders for unbuilt ones.
- **3.2** **Security:** an error boundary that renders failures safely — never
  `error.stack` into the DOM; show a request ID for correlation instead (ties to SEC-9).
- **3.3** Session bootstrap: fetch `GET /session` on load; on 403 show a clear "admin
  confirmation required" state rather than silently failing.
- **3.4** Global 401/403 handling that distinguishes "wrong role" from "wrong origin" so the
  operator can diagnose a blocked legitimate request.
- **Tests:** error boundary renders a request ID and no stack; nav renders all routes.

## FE-4 — Component primitives and charts (M)

- **4.1** Button, Card, Badge, Table, Drawer, Tooltip, StatusDot, ConfirmDialog.
- **4.2** **Security — `ConfirmDialog` is a security control, not just UX.** All destructive
  actions (crash, cleanup, campaign, stepdown, bulk delete) route through it, and it must
  require typed confirmation for the highest-severity ones (design-doc §3.1.9).
  - Add a unit test asserting destructive buttons cannot be activated without confirmation.
- **4.3** Sparkline / TimeSeries / Heatmap on uPlot.
- **4.4** Charts render **numbers from the API only** — never HTML strings in labels/tooltips.
- **4.5** Status is encoded as colour **+ icon + text** (never colour alone) per the
  accessibility requirement in the design doc.

## FE-5 — Overview page (M)

- **5.1** Health banner from `/health`; six tiles; cluster strip; incidents banner.
- **5.2** **Security:** the incidents banner surfaces internal error text — render it through
  `<ByteText>`/escaping, never raw. Include a hostile-string fixture in the page test.
- **5.3** Recent events feed — polled initially, SSE from ADM-5.
- **5.4** Graceful degradation while the daemon is recovering (design-doc DoD).
- **Tests:** renders from fixture JSON; hostile error string renders as text; recovering state
  shows correctly.

## FE-6 — Metrics page (M)

- **6.1** Full panel grid from design-doc §3.1.8, grouped and shareable via URL query params.
- **6.2** URL param parsing must be **allowlisted** — unknown params ignored, values
  length-capped, and never reflected into the DOM unescaped.
  - Test: `?panels=<script>` renders as text and does not execute.
- **6.3** "View as Prometheus" opens the raw endpoint (with the CSP from SEC-5 still applied).
- **Tests:** every registered `lattice_*` metric appears in ≥1 panel; hostile URL params safe.

**Phase B exit gate:** console renders live health; Playwright smoke test passes; no
`localStorage` writes; no `dangerouslySetInnerHTML` in the bundle.

---

# PART 4 — FEATURE PAGES (Phases C–H)

Each page = backend endpoints + frontend page + security cases added to SEC-12.

## LSM-1 — LSM tree endpoint (M)

- **1.1** `GET /lsm/tree` — pin the current `*version.Version` via `TryRef()` (**F8: mandatory**;
  skip this and compaction can nil the level slices mid-read), read `Files(level)` (already a
  deep copy), build per-level records, `Unref()` in a `defer`.
- **1.2** Emit: level, fileNum, size, smallest/largest key, seqnum range.
- **1.3** **Security:** `SmallestKey`/`LargestKey` are **encoded `binary.InternalKey` blobs**,
  not user text — they are `[]byte` and must be emitted **base64/hex, never as raw strings**.
  This is a subtle and easy mistake; call it out in the API schema and test it.
- **1.4** Cap the number of files returned per level (e.g. 500) with an explicit truncation
  flag, so a pathological tree cannot produce a giant response.
- **1.5** `PermissionRead`.
- **Tests:** pinning under concurrent compaction (`-race`); keys base64-encoded; truncation flag
  correct; empty version handled.

## LSM-2 — SSTable inventory + inspect endpoint (M) — *Critical path*

- **2.1** `GET /sstables` — inventory with level/size/mtime. `PermissionRead`.
- **2.2** `GET /sstables/inspect` — calls the FND-2 shared inspector. `PermissionRead`.
  **This endpoint reads arbitrary files — treat it as the highest-risk read endpoint.**
- **2.3** **Triple-gated path resolution (SEC-5 / F5). In this exact order:**
  1. `security.ValidateDatabaseFileName(file)` — rejects any `/`, `\`, null byte, `..`,
     and anything outside `^[a-zA-Z0-9_.-]+$`. Takes a **bare filename only**.
  2. `security.ResolvePath(dataDir, file)` — containment + symlink canonicalisation via
     `evalDeepestExistingAncestor`.
  3. **Type allowlist by extension:** `.sst`, and (for WAL endpoints) `^wal_\d{12}\.log$`,
     `MANIFEST-*`, `CURRENT`. Reject everything else.
  4. Then open — and re-verify containment immediately before use where a TOCTOU window is
     plausible (document the residual risk; note `O_NOFOLLOW` as future hardening).
- **2.4** `GET /sstables/raw` — offset/length reads.
  **Security:** `offset ≥ 0`, `length` clamped to a hard cap (e.g. 64 KiB); reject
  `length > filesize - offset`; use `io.LimitReader`; never `os.ReadFile` a whole SSTable.
- **2.5** `POST /lsm/compact`, `POST /lsm/flush` — `PermissionWrite` **and** CSRF **and**
  confirmation header. Mutations of live engine state from a browser must satisfy the full stack.
- **Tests (all mandatory):** `../../etc/passwd` → 403/400 · absolute path → rejected ·
  `..%2f` encoded traversal → rejected · symlink out of dataDir → rejected · wrong extension →
  rejected · oversize `length` → 400/413 · concurrent-compaction read under `-race`.

## LSM-3 — SSTable inspector UI (M)

- **3.1** `BlockMap` (offset ruler, colour-coded block classes), `HexViewer`, `CRCVerdict`,
  `BloomSimulator`, footer/index/meta panels.
- **3.2** **Security:** every key byte-string renders via `<ByteText>` (FE-2.4). The hex viewer
  renders escaped text, never injected markup. Filenames from the API are escaped too.
- **3.3** Bloom simulator: local computation must **agree with a real `Get`** for present and
  absent keys (a stated acceptance criterion in the design doc — do not fake it).
- **3.4** Corruption banner shown when any CRC fails; never suppress a failure state.
- **Tests:** component tests from fixture JSON including a hostile key fixture; simulator
  agreement test.

## LSM-4 — LSM tree UI (M)

- **4.1** Level columns, sized file rectangles, memtable overlay, WAL strip, compaction panel,
  write-amp estimate, manual trigger button (behind `ConfirmDialog`).
- **4.2** Overlap highlighting computed **client-side from the returned key ranges** (base64
  decoded) — with a unit test using a known overlapping fixture.
- **4.3** **Security:** the manual-compaction button must be hidden/disabled without
  `PermissionWrite`, and must still require typed confirmation (defence in depth: UI gating is
  convenience, server gating is the control).
- **Tests:** tree renders levels proportionally; overlap flags correct; trigger requires
  confirmation; read-only role sees no trigger.

## RAFT-1 — Raft read endpoints (M)

- **1.1** `GET /raft/status`, `/raft/peers`, `/raft/log`, `/raft/timeline` from FND-5 +
  existing accessors (F7). `PermissionRead`; **`/raft/log` is `PermissionAdmin`** (it carries
  committed payloads — FND-5.4).
- **1.2** Log pagination: server-enforced `max` per page; client cannot request unbounded.
- **1.3** **Security:** peer addresses are internal topology. Acceptable on loopback, but log
  that decision, and ensure they are not emitted on any non-loopback bind without admin.
- **1.4** `/raft/log` responses must use the same escaping path for key bytes.
- **Tests:** status matches a live node; log pagination capped; `/raft/log` denies a reader.

## RAFT-2 — Raft mutating endpoints (M) — *dangerous*

- **2.1** `POST /raft/campaign`, `/raft/stepdown`. **Full stack:** `PermissionAdmin` + CSRF +
  `X-Lattice-Admin: confirm` + server-side rate limit (e.g. one per 5s).
- **2.2** **Rationale to document:** these endpoints can trigger repeated elections, which is a
  **denial-of-availability vector** against the cluster. The rate limit is a security control,
  not a nicety.
- **2.3** Typed confirmation in the UI; audit-logged per SEC-10.4.
- **Tests:** reader/writer denied; missing CSRF denied; rate limit enforced; audit line written.

## RAFT-3 — Raft UI (M)

- **3.1** Status panel, term-timeline ribbon, log viewer, lag chart, HardState, ReadIndex panel.
- **3.2** Danger zone behind `ConfirmDialog` with typed input.
- **3.3** **Security:** danger-zone controls rendered only with `PermissionAdmin`; every action
  shows the CSRF-protected POST, never an optimistic UI state that hides failure.
- **Tests:** timeline renders term changes from fixtures; danger controls hidden for reader.

## KEY-1 — Keyspace endpoints (M)

- **1.1** `GET /keys` (prefix), `/keys/get`, `POST /keys/put`, `POST /keys/delete` on the FND-6
  iterator.
- **1.2** `PermissionRead` / `PermissionWrite`; mutations need CSRF + confirmation header.
- **1.3** **Security:**
  - `limit` server-enforced (default 100, max 1000); `prefix` length-capped.
  - Response byte budget — a scan must not stream unbounded data.
  - Keys/values are attacker-controlled: same escaping contract as everywhere else.
  - The endpoint is a **bulk-read primitive**: rate-limit it and document it, since a
    prefix scan of `""` exfiltrates the whole keyspace to anyone who can reach the port.
- **1.4** Bulk delete-by-prefix requires `PermissionWrite`, CSRF, **and** a two-step
  confirm-with-count (the Redis Insight pattern in the design doc).
- **Tests:** limit enforced; empty-prefix bounded by the cap; reader cannot write; bulk delete
  requires confirmation.

## KEY-2 — Keyspace UI (M)

- **2.1** Prefix search, result table with **origin level**, value viewer, tombstone toggle,
  bulk delete with confirmation summary, stats panel.
- **2.2** **Security:** value viewer renders through `<ByteText>` with text/hex/base64 toggle;
  no `data:` URL rendering of values (would reintroduce an XSS/SSRF-adjacent vector).
- **2.3** Bulk-delete summary must state the exact count and bytes from the server, not a
  client-side guess.
- **Tests:** origin level correct; tombstone toggle correct; hostile value renders as text.

## WAL-1 — WAL + MANIFEST endpoints (S)

- **1.1** `GET /wal/segments`, `/wal/dump`, `/manifest` on FND-3. `PermissionRead`
  (`/wal/dump` `PermissionAdmin` — WAL records contain raw key/value data, same class as the
  Raft log).
- **1.2** Same triple-gated path resolution as LSM-2, with the WAL/MANIFEST extension allowlist.
- **1.3** Record-count and per-record size caps from FND-3.3.
- **Tests:** traversal/symlink/wrong-type rejected; caps enforced; reader denied on dump.

## WAL-2 — WAL UI (S)

- **2.1** Segment list, record timeline with filters, replay-to-LSN simulation, corruption
  highlighting, raw record hex.
- **2.2** **Security:** corruption/error text escaped; keys via `<ByteText>`.
- **Tests:** corrupt segment highlights the exact failing record; replay distinguishes
  checkpoint-covered records.

## LAB-1 — Workload engine (L)

- **1.1** `internal/admin/lab.go` — bounded goroutine pool, cancellable via context, per-job
  caps: max concurrency (e.g. 64), max duration, max keys.
- **1.2** **Security (DoS):** a workload job must never be able to exhaust the engine or the
  host. Enforce caps server-side regardless of what the client requests; clamp hostile values
  rather than rejecting them silently.
- **1.3** Global concurrency cap across all jobs; at most N jobs at once.
- **1.4** All work under a job context cancelled on shutdown or disconnect.
- **1.5** `PermissionAdmin` + CSRF. Audit-logged.
- **Tests:** caps clamped; cancellation stops promptly; shutdown cancels all; `-race` clean with
  zero goroutine leaks; a client requesting 1e9 ops is clamped.

## LAB-2 — Crash & recover (M) — *most dangerous feature*

- **2.1** `POST /lab/crash` — schedules `SIGKILL` of self after a countdown.
- **2.2** **Security — layered gates, all mandatory:** `PermissionAdmin` + CSRF +
  `X-Lattice-Admin: confirm` + **a typed confirmation token** (operator must type the node ID)
  + **an arm/disarm model** (endpoint arms; a separate endpoint fires) so a stray retry cannot
  kill the node.
- **2.3** **Never** trigger from a page load, a retry, a background job, or an SSE reconnect.
  Only from an explicit POST to the fire endpoint, with a single-use nonce.
- **2.4** Countdown must be cancellable until T-1s.
- **2.5** Recovery report from `RecoverWAL`: segments replayed, records applied, checkpoint-truncated
  records, final sequence number.
- **2.6** **Security:** the recovery report is a data-integrity artefact — sanitise it, and do
  not let it echo raw record payloads.
- **Tests:** all gates enforced individually and in combination; arm-without-fire does nothing;
  countdown cancellable; nonce single-use; subprocess crash test reusing the
  `cmd/lattice/chaos_sigkill_test.go` pattern (never crash the test binary itself).

## LAB-3 — Diagnostics bundle (M)

- **3.1** `GET /diagnostics` — zip of config (redacted), metrics, health, raft state, recent events.
- **3.2** **Security — bundle is an exfiltration primitive.** Redact absolutely: no TLS key
  paths, no certificate PEM, no key/value contents, no CSRF token, no absolute paths outside
  the data dir. Include a manifest of what was redacted.
- **3.3** Cap total bundle size; stream rather than buffer entirely in memory.
- **3.4** `PermissionAdmin`; audit-logged (it leaves the trust boundary).
- **Tests:** bundle contains none of the forbidden strings; size cap enforced; reader denied.

## CON-1 — Console REPL (M)

- **1.1** `POST /console/exec` reusing `cmd/lattice-cli` parser semantics (extract to a shared
  package if needed; keep CLI behaviour identical).
- **1.2** **Security — this endpoint is arbitrary command execution.** It must have
  `PermissionAdmin` + CSRF + confirmation header, plus its own rate limit and per-request
  deadline.
- **1.3** **Never** shell out. Parse and dispatch in-process only — no `exec.Command`, ever.
  Add a CI grep-check banning `os/exec` in `internal/admin`.
- **1.4** Map CLI operations through the same `RequiredPermission(op)` logic so a `reader` cannot
  `PUT` via the REPL even if admitted to the endpoint.
- **1.5** Cap input size (SEC-7.1) and output size; truncate rather than OOM.
- **Tests:** reader denied; `os/exec` absent; size caps; CLI parser parity; every op honours
  its permission.

## CON-2 — Console REPL UI (S)

- **2.1** REPL mirroring CLI semantics, auto-generated help, multi-node target switcher,
  history in `localStorage`.
- **2.2** **Security:** history in `localStorage` is a deliberate trade-off — it must store
  **commands only, never results**, so no key/value data is persisted in the browser.
  Document this explicitly.
- **2.3** Redirect/not-leader surfaced with a retry affordance.
- **Tests:** history contains no response data; follower write surfaces the redirect.

## SYS-1 — System page (S)

- **1.1** Effective config with provenance; security posture; pprof deep links; orphan cleanup.
- **1.2** **Security:** reuse the ADM-4.3 redaction list; the posture page must loudly flag
  `--insecure-transport` and any non-loopback admin bind.
- **1.3** **pprof is not proxied through the admin server** — link to the pprof listener only.
  Proxying would widen pprof reachability, which the design doc explicitly forbids.
- **1.4** Orphan cleanup: `PermissionAdmin` + CSRF + typed confirmation.
- **Tests:** posture warns correctly; no pprof proxy route exists; reader denied cleanup.

---

# PART 5 — CROSS-CUTTING HARDENING & RELEASE GATE

## XD-1 — Continuous security verification (M, spans all phases)

- **1.1** A single `make security-check`-style script (or documented command sequence) that
  runs, in order: `go build`, `go vet`, `go test -race -short ./...`, the SEC-12 suite, the
  frontend typecheck/tests, and the static greps below.
- **1.2** **Static greps that must stay green** (each catches a regression SEC tasks prevent):
  | Check | Prevents |
  |---|---|
  | no `dangerouslySetInnerHTML` in `web/src` | XSS (FE-2.3) |
  | no `os/exec` in `internal/admin` | command injection (CON-1.3) |
  | no `Access-Control-Allow` in `internal/admin` | CORS exfiltration (SEC-1.3) |
  | no `unsafe-inline` / `unsafe-eval` in CSP strings | CSP weakening (SEC-5.1) |
  | no `io.ReadAll` on request bodies in `internal/admin` | unbounded body (SEC-7.1) |
  | no `os.ReadFile` of a whole SSTable/WAL in handlers | memory DoS (LSM-2.4) |
  | no `localStorage` write of token or API response data | token/data leakage (FE-2.2, CON-2.2) |
  | every `net.Listen` in `internal/admin` loopback-verified | bind bypass (SEC-2.3) |
- **1.3** Run XD-1 in CI on every push.
- **1.4** Each feature task from Part 4 must add its own attack cases to SEC-12 before it is
  considered done.

## XD-2 — Performance & DoS budget (S)

- **2.1** Verify the admin server does not regress the **data path**: with the console open and
  SSE streaming, `lattice-bench` throughput must be within an agreed tolerance of baseline.
  Record the numbers.
- **2.2** Confirm SSE + dashboard polling adds no lock contention on engine paths (the events
  bus must never be called while holding an engine lock — assert this in review).
- **2.3** Measure `InspectSSTable` on the largest realistic SSTable; if slow, ensure it is
  admin-gated and time-bounded, not exposed to readers.
- **Done when:** a benchmark comparison table exists in the PR.

## XD-3 — Fuzzing the new attack surface (M)

- **3.1** Extend `inspect`/`dump_wal` fuzz seeds to the new endpoints.
- **3.2** Fuzz the path-resolution helper directly: random filenames, traversals, encoded
  traversals, unicode confusables, overlong names, null bytes. Assert **no panic and no escape**.
- **3.3** Fuzz `ByteText` with random bytes; assert it never emits raw markup.
- **4.** Run all fuzz targets briefly in CI (`-fuzztime=10s`).

## XD-4 — Documentation (S)

- **4.1** `SECURITY.md` console section (SEC-13.1).
- **4.2** `docs/known-limitations.md`: accepted risks + the console's security posture
  (SEC-13.2).
- **4.3** `docs/threat-model.md`: add the console threat model from §1.1 above.
- **4.4** `README.md`: console section + a 60-second demo script.
- **4.5** `docs/DEMO.md`: the full demo ladder, UI-assisted.

---

# PART 6 — DEPENDENCIES, SEQUENCING & RISKS

## 6.1 Dependency graph

```
Part 0  SEC-0 … SEC-13            (no deps — START HERE)
          │
Part 1  FND-1 ──┬─ FND-2 ──┬─ FND-3
          │      └─ FND-4 ──┴─ FND-5
          └────────────────────── FND-6  (iterator; independent of UI)
          │
Part 2  ADM-1 → ADM-2 → ADM-3 → ADM-4 → ADM-5 → ADM-6 → ADM-7
          │
Part 3  FE-1 → FE-2 → FE-3 → FE-4 → FE-5 → FE-6
          │
Part 4  LSM-1 → LSM-2 → LSM-3/LSM-4
          RAFT-1 → RAFT-2 → RAFT-3
          KEY-1 (needs FND-6) → KEY-2
          WAL-1 → WAL-2
          LAB-1 → LAB-2 → LAB-3
          CON-1 → CON-2 ; SYS-1
          │
Part 5  XD-1 (continuous) · XD-2, XD-3, XD-4 (release)
```

## 6.2 Critical path

**SEC-0 → SEC-1 → SEC-2 → SEC-3 → ADM-1 → ADM-2 → ADM-3 → LSM-2 → LSM-3**

The LSM/SSTable inspector is the signature differentiator and also the highest-risk read
endpoint, so it is the first real feature built — deliberately *after* the spine.

## 6.3 Parallelisable tracks

Once Part 0 lands, three tracks can proceed independently:
- **Backend** (Part 2 → Part 4 endpoints)
- **Frontend** (Part 3)
- **Engine** (FND-6 iterator) — mergeable on its own regardless of UI progress

## 6.4 Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Drive-by localhost attack against the console | High if SEC-1..4 skipped | **Critical** | Hard gate: no feature work before the spine |
| CSP blocks the real bundle (inline styles from a chart lib) | Medium | Medium | Decide nonce-vs-CSS in FE-3; test against the **real** build, not the stub |
| Version pinning forgotten in `LSM-1` → nil-slice panic under compaction | Medium | High | F8 noted; mandatory `TryRef`/`Unref`; `-race` concurrency test |
| `binary.InternalKey` blobs leaked as raw strings in the LSM tree API | Medium | Medium | LSM-1.3 explicit base64 requirement + test |
| SSE floods exhaust the host | Medium | High | SEC-8 separate budget; XM-2.1 benchmark gate |
| FND-6 iterator stalls a scan on a huge keyspace | Medium | High | Byte budget + context cancellation (FND-6.6) |
| `console/exec` becomes an RCE foothold | Low | **Critical** | CON-1.3 in-process only + `os/exec` CI grep |
| Too much scope → nothing finished | High | Medium | Every phase independently demoable; stop cleanly at any boundary |
| JS supply-chain compromise | Low | High | Lockfile, `npm ci`, `npm audit` (FE-1.3) |

## 6.5 Stop/go checkpoints

| After | Question | If not satisfied |
|---|---|---|
| Part 0 | Does SEC-12 pass, including a real drive-by PoC? | **Stop.** Do not build features on an unproven spine. |
| Part 2 | Is `curl` giving JSON with no leaks in error bodies? | Fix SEC-9 before any UI work |
| Part 4 (LSM) | Do traversal + symlink tests pass? | **Stop.** This endpoint reads arbitrary files. |
| Part 4 (LAB) | Are all crash gates enforced individually *and* combined? | **Stop.** Highest-risk feature. |
| Part 5 | Do all XD-1.2 greps stay green? | **Stop.** A red grep means a regressed control. |

## 6.6 Effort roll-up

| Part | Content | Effort |
|---|---|---|
| 0 | Security spine (13 tasks) | **L** |
| 1 | Foundations (6 tasks, incl. the iterator) | **L** |
| 2 | Admin server (7 tasks) | **L** |
| 3 | Frontend foundation (6 tasks) | **L** |
| 4 | Feature pages (13 tasks) | **XL** |
| 5 | Hardening + docs (4 tasks) | **M** |

**Realistic total: ~10–14 weeks part-time** (the original 6–8 week estimate did not include a
security spine, which is now the largest single addition).

**Reduced scope that still ships something defensible:**
Part 0 + Part 2 + Part 3 + LSM-1..4 → a console with the signature page and a hardened
surface. Roughly 5–7 weeks.

## 6.7 Open decisions (need your call before Part 2)

1. **Admin auth for v1** — loopback + CSRF token (recommended, no new identity system), or
   require mTLS + client cert role from day one (stronger, more setup friction)?
2. **`/events` role** — I recommend `PermissionAdmin` over `PermissionRead` (ADM-5.3). Confirm.
3. **`web/dist` committed?** — yes, so `go build` works without Node; costs a binary-blob diff.
4. **`ForensicReport` relocation** (FND-2) — acceptable given the CLI fuzz tests must be updated?
5. **Where the CSP nonce decision lands** if a chart library needs inline styles (§6.4).

---

# APPENDIX — Traceability

| Design-doc § | Task IDs |
|---|---|
| §5.3 API surface (Node/health) | ADM-4 |
| §5.3 API surface (Engine/storage) | LSM-1, LSM-2 |
| §5.3 API surface (Raft) | FND-5, RAFT-1, RAFT-2 |
| §5.3 API surface (Keys) | FND-6, KEY-1 |
| §5.3 API surface (Metrics/events) | FND-1, ADM-5 |
| §5.3 API surface (Lab) | LAB-1, LAB-2, LAB-3 |
| §5.4 Event bus | FND-4, ADM-5 |
| §6.1 Merge iterator | FND-6 |
| §6.2 Metrics JSON | FND-1 |
| §6.3 DumpWAL refactor | FND-3 |
| §6.4 Raft introspection | FND-5 |
| §6.5 Admin authorization | SEC-4, ADM-3 |
| §7 Security requirements | **Part 0 in full**, plus XD-1 |
| §8.4 Testing strategy | XD-1, XD-3, per-task tests |

**Security coverage note:** every row of the §7 security table maps to at least one task in
Part 0. The table in §1.1 additionally enumerates 12 threats that the design doc did **not**
name — chiefly the drive-by localhost and DNS-rebinding classes (F1/F2), which are the reason
Part 0 exists and why the estimate grew.

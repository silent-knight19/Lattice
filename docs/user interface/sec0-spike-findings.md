# SEC-0 — Security Spike Findings

> Task: SEC-0 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE.**
> Purpose: empirically establish the console threat surface *before* building defences on theory.
> Artefacts: `/tmp/lattice-sec0/` (target, attacker page, harnesses). No production code changed.

---

## Verdict

**The threat is real and was measured, not assumed.** Two of the three planned findings are
confirmed with evidence. One planned defence (CORS as a reader-side control) was **disproven**
and must not be relied upon.

| # | Planned finding | Result | Evidence |
|---|---|---|---|
| F1 | Drive-by localhost — foreign page can reach loopback admin API | **CONFIRMED (worse than assumed)** | Server log shows all 4 probes delivered & executed |
| F2 | No CSP / clickjacking headers | **CONFIRMED** | grep: zero `Content-Security-Policy` matches |
| F3 | Unbounded request bodies | **CONFIRMED** | `pprof.go:83-88`, `metrics/server.go:168-175` set only `MaxHeaderBytes` |
| **F11** | *(new)* `CleanAndValidatePath` does **not** block traversal | **CONFIRMED** | Empirical: `../../etc/passwd` → ACCEPTED |
| **F12** | *(new)* JS-visible failure ≠ request not delivered | **CONFIRMED** | JS said "Failed to fetch"; server executed anyway |

---

## SEC-0.1 — Drive-by localhost attack (CRITICAL, CONFIRMED)

### Method
A loopback target on `127.0.0.1:7070` stood in for the future admin server (no
`Access-Control-*` headers, one read endpoint, one mutating POST). A page on a **different
origin** (`127.0.0.1:9099`) fired four probes. Driven with real headless Chrome. The target
logged every inbound request server-side — because the JavaScript-visible result is *not*
trustworthy (see F12).

### Result — the JS view
```
A: request BLOCKED before send: Failed to fetch
B: request BLOCKED before send: Failed to fetch
C: request BLOCKED (preflight rejected) -> Failed to fetch
D: multipart BLOCKED -> Failed to fetch
```

### Result — the SERVER view (ground truth)
```
13:36:20.035 HIT          GET  /api/v1/ping   Origin="http://127.0.0.1:9099"
13:36:20.040 HIT-MUTATING POST /api/v1/crash  Origin="http://127.0.0.1:9099" CSRF="" Admin=""
13:36:20.041 HIT-MUTATING OPTIONS /api/v1/crash Origin="http://127.0.0.1:9099"
13:36:20.042 HIT-MUTATING POST /api/v1/crash  Origin="http://127.0.0.1:9099"
```

**Every probe reached the server and the mutating handler ran — twice.** JavaScript reported
"Failed to fetch" because the browser withheld the *response* (no CORS headers), but the
*request and its side effects had already happened.*

### Control (same-origin, to prove the harness was sound)
Same page served from the target's own origin → all probes returned `200` and were readable.
The harness works; the cross-origin difference is the variable.

### Conclusion
An unrelated website can **trigger any mutating admin action on an operator's machine** with
zero user interaction. For `POST /api/v1/lab/crash` that means *any website can SIGKILL the
database daemon.* Loopback binding provides **no protection** here.

### Consequences for the design
| Planned control | Verdict |
|---|---|
| SEC-1 Origin allowlist | **Mandatory.** Rejects the foreign `Origin` server-side. |
| SEC-2 Host validation | **Mandatory.** Also blocks DNS rebinding (`Host: evil.com`). |
| SEC-3 CSRF token | **Mandatory.** Defence in depth — Origin checks alone are single-layer. |
| SEC-4 no-CORS invariant | **Mandatory, but for a different reason than assumed** — see SEC-0.2. |

---

## SEC-0.2 — CORS does NOT stop sending; it only stops reading (plan DISPROVEN)

The plan assumed "no CORS headers ⇒ requests are blocked." **That is wrong.** The experiment
shows CORS governs *response visibility*, not *request delivery*:

- **Sending:** a "simple" request (no custom headers, `Content-Type: text/plain`, or a form
  POST) triggers **no preflight** and is delivered unconditionally.
- **Reading:** the browser withholds the response because the server sends no
  `Access-Control-Allow-Origin`.

So the correct rule is:

> **No-CORS stops an attacker *reading* data. It does not stop them *doing* things.**

**Implication — the highest-value console target is the read path, not the write path.**
Exfiltration of engine internals is blocked by CORS; but state-changing actions are one
`text/plain` POST away. Therefore:

1. **SEC-1 (Origin allowlist) is the primary control**, not a hardening nicety — it is the
   only thing that actually stops delivery.
2. **SEC-4's CORS invariant is retained** as the exfiltration control, but it must never be
   described as the CSRF defence.
3. **Preflight is not a defence** — probe C shows the browser sent `OPTIONS` *and then still
   sent the POST*. A permissive preflight handler that answers `OPTIONS` with
   `Access-Control-Allow-*` would make things strictly worse. The admin server must **not**
   implement CORS at all.
4. **Custom headers alone do not stop a determined attacker** who is willing to trigger a
   preflight — which is precisely why SEC-3's token is independent of SEC-1.

---

## SEC-0.3 — Path-handling audit for reuse

### What the existing CLI tools actually do

| Function | `CleanAndValidatePath` | `ValidatePathNoSymlinks` | `Lstat`/regular-file/`SameFile` | **Containment** |
|---|---|---|---|---|
| `InspectSSTable` (`inspect.go:149`) | yes | yes | yes | **NO** |
| `DumpWAL` (`dump_wal.go:58`) | yes | via `wal.OpenReader` | via reader | **NO** |

Both are **correct for their current CLI context**: a human operator supplies a path and
already has filesystem access. Neither is safe to expose over HTTP, where the path is
attacker-supplied.

### F11 — measured: `CleanAndValidatePath` does not block traversal

```
../../etc/passwd        -> ACCEPTED as "../../etc/passwd"
/etc/passwd             -> ACCEPTED as "/etc/passwd"
sub/../../etc/passwd    -> ACCEPTED as "../etc/passwd"
```
```
ResolvePath(root, "../../etc/passwd") -> REJECTED
ResolvePath(root, "/etc/passwd")      -> REJECTED
ResolvePath(root, "ok.sst")           -> ACCEPTED
```

`CleanAndValidatePath` only rejects empty paths and null bytes. **Containment comes entirely
from `ResolvePath` / `ValidateContainment`.** Reusing the CLI functions behind an HTTP
endpoint would have been a **critical arbitrary-file-read vulnerability**.

### Mandatory gate for every file-serving endpoint (LSM-2.3, WAL-1.2)

1. `security.ValidateDatabaseFileName(file)` — bare filename only; rejects `/`, `\`, null,
   `..`, and anything outside `^[a-zA-Z0-9_.-]+$`.
2. `security.ResolvePath(dataDir, file)` — the containment + symlink-canonicalisation gate
   that `CleanAndValidatePath` does **not** provide.
3. Extension allowlist — `.sst`; `^wal_\d{12}\.log$`; `MANIFEST-*`; `CURRENT`.
4. Re-verify immediately before open where TOCTOU is plausible.

Steps 1–3 are all required: 1 stops traversal lexically, 2 defeats symlink escape, 3 stops
reading files that are legitimate but not intended to be exposed.

---

## SEC-0 additional findings

**F12 — never trust JS-visible results in a security test.** The probes all reported
"Failed to fetch" while the server had already executed the mutation. A test that asserts on
the client-side outcome would have concluded "the attack failed" and shipped a vulnerable
console. Every assertion in SEC-12 must therefore be made **server-side**.

**F3 — unbounded bodies confirmed.** `cmd/lattice/pprof.go:83-88` and
`internal/metrics/server.go:168-175` set `MaxHeaderBytes` only. SEC-7 applies
`http.MaxBytesReader` per route.

**F2 — no CSP anywhere.** Confirmed by grep. SEC-5 is mandatory; the admin server must send
`frame-ancestors 'none'` to prevent clickjacking the Lab page's crash button.

---

## Revised priorities

| Priority | Task | Why |
|---|---|---|
| **P0** | SEC-1 Origin allowlist | *Only* control that stops request delivery (SEC-0.2) |
| **P0** | SEC-2 Host validation | Stops DNS rebinding, where Origin looks legitimate |
| **P0** | SEC-3 CSRF token | Independent layer; custom headers alone are insufficient |
| **P0** | SEC-4 no-CORS invariant | Exfiltration control — **relabelled**, it is not the CSRF defence |
| **P0** | LSM-2.3 / WAL-1.2 triple-gate | F11: containment is *not* optional |
| P1 | SEC-5 headers/CSP | XSS + clickjacking blast radius |
| P1 | SEC-7 body caps | F3 |
| P1 | SEC-8 SSE budget | SSE floods the host |

## Acceptance for SEC-0 — met

- [x] **0.1** Drive-by reachability demonstrated with real browser + server-side proof.
- [x] **0.2** Both halves of CORS behaviour documented; drives the SEC-1 design.
- [x] **0.3** Endpoint → validation table produced; F11 recorded.

## Gate decision

Part 0 may proceed. **The spine is justified by measurement.** Two design assumptions in the
original plan were wrong and are now corrected before any code was written — which is
precisely what this spike existed to catch.

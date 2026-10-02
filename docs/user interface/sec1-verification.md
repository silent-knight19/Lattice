# SEC-1 — Implementation & Verification Record

> Task: SEC-1 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**
> Depends on: [`sec0-spike-findings.md`](./sec0-spike-findings.md).

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/origin.go` | `parseOrigin`, `canonicalOrigin`, `splitHostPortStrict`, `originsEqual`, `OriginAllowlist`, `SelfOrigins` |
| `internal/admin/middleware.go` | `Middleware`, `Chain`, `OriginGuard`, `writeForbidden`, no-CORS invariant helpers |
| `internal/admin/origin_test.go` | 66 cases — parsing, normalization, allowlist decisions, `SelfOrigins` |
| `internal/admin/middleware_test.go` | 34 cases — attack matrix, all-verbs, leakage, CORS invariant, chain order |

**Dependencies added: zero.** `go.mod` still has no `require` directive. Stdlib only.

---

## Why the parsing is strict

Origin comparison is the whole security boundary, so the parser is built to fail closed on
anything unusual rather than normalize toward acceptance. It rejects, among others:

| Input | Why rejected |
|---|---|
| `null` | Opaque origin (sandboxed iframe, some redirects, `file://`) |
| `http://user:pass@evil.com` | Userinfo smuggling — no browser Origin contains it |
| `http://127.0.0.1:7070.evil.com` | Prefix confusion |
| `http://evil-127.0.0.1:7070` | Suffix confusion |
| `http://localhost.:7070` | Trailing dot is a distinct, registerable DNS name |
| `http://example.com:abc`, `:99999` | Non-numeric / out-of-range port |
| `http://evil.com/path`, `?q=`, `#f` | A real Origin has no path/query/fragment |
| `http://a.com http://b.com` | Multiple origins — never partially matched |
| `javascript:`, `data:`, `file:`, `myapp:` | Non-http(s) schemes |
| bare `::1` unbracketed | Ambiguous authority |

Comparison is **component equality** on `(scheme, host, port)` — never `Contains`,
`HasSuffix`, or `HasPrefix`. That is what makes the four confusion rows above fail closed.

**Default ports are materialized.** A browser sends `http://localhost` (no port) for port 80,
so `splitHostPortStrict` fills in the scheme default before comparison; otherwise a correct
same-origin request would be rejected.

---

## Verification against the real attack

The **unchanged SEC-0 attacker page** was run in headless Chrome against a loopback server
using the production `OriginGuard`.

### Ground truth (server-side — per F12, the only trustworthy signal)

```
PASS — guarded.log is EMPTY. Zero executions.
```

Compare with SEC-0, where the same page produced:

```
HIT-MUTATING POST /api/v1/crash  Origin="http://127.0.0.1:9099"   (×2)
```

### The JavaScript view was identical before and after

```
A: request BLOCKED before send: Failed to fetch
B: request BLOCKED before send: Failed to fetch
...
```

**Identical output, opposite reality.** Before SEC-1 the mutations executed twice; after
SEC-1, zero times. This is finding F12 in action and is exactly why every assertion in
`middleware_test.go` uses a server-side `atomic.Int64` execution counter rather than
inspecting the client-visible response.

### Legitimate traffic still works

| Request | Result |
|---|---|
| `Origin: http://127.0.0.1:7070` (the real console) | **200** + payload |
| `Origin: http://localhost:7070` (alias) | **200** |
| no `Origin` (curl, `lattice-cli`, probes) | **200** |
| `Origin: http://127.0.0.1:9099` (attacker) | **403** |
| `Origin: null` | **403** |

`SelfOrigins("127.0.0.1:7070", "http")` derives
`["http://127.0.0.1:7070", "http://localhost:7070"]`, so an operator binding loopback gets
correct behavior with no manual configuration.

---

## A bug my own test caught

`TestSelfOrigins` initially failed:

```
SelfOrigins("0.0.0.0:7070") = [http://0.0.0.0:7070], want []
```

That would have produced an origin string that is **unreachable as an origin** and actively
misleading if an operator pasted it into configuration — they'd believe remote access was
allowlisted when no browser origin actually is. `SelfOrigins` now returns `nil` for a
wildcard bind (`net.IP.IsUnspecified()`), forcing an explicit allowlist. Fixed in
`origin.go:272-280`.

Worth noting as process: this was caught by the test suite, not by review.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 1.1 `OriginGuard` with exact-match, no substring matching | ✅ `originsEqual` + 4 confusion-row tests |
| 1.2 Applied to **all** routes, not just mutating | ✅ `TestOriginGuard_AllMethodsAreGuarded` (7 verbs) |
| 1.3 No `Access-Control-*` ever emitted | ✅ `TestNoCORSHeadersOnAnyResponse` (4 response classes) |
| `evil.com` → 403 | ✅ |
| scheme mismatch → 403 | ✅ |
| `Origin: null` → 403 | ✅ |
| absent Origin → 200 | ✅ |
| exact self origin → 200 | ✅ |

**SEC-1 acceptance criteria: met.**

---

## Deliberate design decision: absent Origin is ALLOWED

`Allows("")` returns `true`. This is the one permissive case, and it is safe because:

- Browsers omit `Origin` on same-origin GET/HEAD navigation and subresource loads.
- Non-browser clients (`curl`, `lattice-cli`, Prometheus, health probes) never send it.
- **An attacker cannot suppress `Origin`** on a request a browser makes cross-origin — that
  is exactly what SEC-0 proved. So rejecting absent-Origin adds no security while breaking
  every legitimate non-browser caller.

The `"null"` opaque origin is **not** allowed.

---

## Known limitations of SEC-1 (carried to SEC-2)

OriginGuard alone is **not** sufficient:

1. **DNS rebinding.** An attacker domain resolving to `127.0.0.1` presents an `Origin` the
   attacker controls *and* whose DNS name they own — it can look entirely legitimate while
   the TCP connection reaches loopback. → **SEC-2 (Host header validation)**.
2. **Custom headers / preflight.** Requiring `X-Lattice-Admin` forces a preflight, but a
   permissive preflight handler would *undo* this protection. SEC-0 observed the browser send
   `OPTIONS` and then send the POST anyway. → the admin server must never implement CORS
   (enforced by SEC-1.3), and defence in depth comes from **SEC-3 (CSRF token)**.

Neither gap weakens SEC-1; they are the reasons SEC-2 and SEC-3 exist.

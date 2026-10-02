# SEC-5 — Implementation & Verification Record

> Task: SEC-5 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**
> Depends on: [`sec4-verification.md`](./sec4-verification.md).

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/headers.go` | `SecurityHeaders`, `NoStoreAPI`, `ContentSecurityPolicy`, `PermissionsPolicy`, `contentTypeByExtension`, `isAPIPath` |
| `internal/admin/headers_test.go` | Golden header set across 5 response classes, CSP directive assertions, content-type table |

Package total: **281 cases**, `-race` clean, **zero third-party dependencies**.

---

## Why this matters more here than in a typical web app

Lattice renders **attacker-controlled bytes** as its primary content. A key or value is
whatever the last writer put there. So the console is, structurally, a renderer of untrusted
input — exactly the shape of application CSP exists to contain.

CSP is the backstop that makes an XSS bug **non-exploitable** even if a rendering flaw slips
through: `'self'` script-src means injected markup cannot execute regardless.

The second motivation is the Lab page. It has a "crash this node" button. `X-Frame-Options:
DENY` plus `frame-ancestors 'none'` prevent an attacker framing the console and tricking an
operator into clicking a destructive control (clickjacking).

---

## The CSP

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none';
form-action 'none'; frame-ancestors 'none'
```

Each omission is a decision, not an oversight:

| Directive | Why |
|---|---|
| no `unsafe-inline` / `unsafe-eval` | blocks inline `<script>` and `eval()` outright |
| `script-src 'self'` | injected markup cannot execute even inline |
| `object-src 'none'` | blocks plugin content |
| `base-uri 'none'` | blocks `<base>` hijacking, which would re-point every relative URL |
| `form-action 'none'` | blocks form exfiltration — **`script-src` would NOT stop this** |
| `frame-ancestors 'none'` | modern clickjacking defense (X-Frame-Options kept for older agents) |
| `connect-src 'self'` | still permits the SSE stream to `/api/v1/events` |

`form-action 'none'` is the subtle one: without it, an injected `<form>` could POST key data
to an attacker server, and no script-src restriction would interfere.

**Recorded consequence for the SPA:** with `unsafe-inline` forbidden, any dynamic styling in
the frontend must come from bundled CSS or a per-response nonce. Carried into FE-3.

---

## Verification — A/B in real Chrome

Two routes served **identical injected markup**; one with `SecurityHeaders`, one without:

| Route | Result |
|---|---|
| `/nocsp` (no headers) | `<div id="out">INLINE-SCRIPT-EXECUTED</div>` |
| `/` (with SEC-5) | `<div id="out">pending</div>` |

The control matters: without it, "the script did not run" could just mean the page failed to
load. It proves the blocking is caused by CSP.

Two further attacks on the protected route, both blocked:

- `<img src=x onerror="...">` → did not fire (title unchanged)
- `eval("...")` → blocked (title unchanged; no `EVAL-EXECUTED`)

Headers as actually served:

```
Content-Security-Policy: default-src 'self'; script-src 'self'; ... frame-ancestors 'none'
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
Referrer-Policy: no-referrer
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
Permissions-Policy: accelerometer=(), ..., clipboard-write=(), interest-cohort=()
```

---

## Golden test (subtask 5.3)

`TestSecurityHeaders_Golden` asserts the **exact** header set on all five required response
classes — index page, 200 API, 403, 404, and 500 — and additionally asserts that none of them
carries a CORS header.

This is why `SecurityHeaders` sets headers **before** calling `next`: a handler that forgets
cannot produce a headerless response. `TestSecurityHeaders_SetsBeforeHandler` proves this with
a handler that touches no headers at all.

---

## Caching is resource-scoped

`Cache-Control: no-store` is applied by a **separate** middleware, `NoStoreAPI`, only to
`/api` paths. It is not part of `SecurityHeaders` because caching is a property of the
resource, not a blanket security property:

| Path | `Cache-Control` |
|---|---|
| `/api/v1/health` | `no-store` |
| `/api` | `no-store` |
| `/`, `/index.html`, `/assets/app-abc123.js` | unset (cacheable) |
| `/apifoo` | unset — prefix match is `/api/`, not `/api` |

`NoStoreAPI` also **respects a handler's existing value**, so a handler may deliberately
choose a stricter policy later without being silently clobbered.

---

## Content types without sniffing (subtask 5.2)

`contentTypeByExtension` maps every extension the SPA build emits to an explicit MIME type.
Combined with `nosniff`, a mislabelled or malicious file cannot be reinterpreted as script.

Two invariants are pinned by tests:

- **Unknown extensions default to `application/octet-stream`**, never to something
  executable. `app.js.bak`, `evil.xyz`, `x.exe`, `x.php` all fall back safely.
- **`text/javascript`** is used rather than the legacy `application/javascript` spelling.

The actual `embed.FS` routing and SPA fallback belong to SEC-11.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 5.1 Headers on **every** response incl. errors and static | ✅ golden test across 5 classes |
| 5.2 Explicit content types, no reliance on sniffing | ✅ table + two invariant tests |
| 5.3 Golden test on `/`, 200, 403, 404, 500 | ✅ `TestSecurityHeaders_Golden` |
| headers present on all five classes | ✅ |
| no `unsafe-inline` anywhere in CSP | ✅ `TestCSP_HasNoUnsafeDirectives` |
| `no-store` on every API response | ✅ `TestNoStoreAPI_*` |

**SEC-5 acceptance criteria: met.**

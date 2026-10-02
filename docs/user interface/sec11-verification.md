# SEC-11 — Implementation & Verification Record

> Task: SEC-11 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/static.go` | `StaticHandler`, `safeAssetPath`, `serveFile`, `etagFor`, `hashFile` |
| `internal/admin/static_test.go` | Traversal, encoding, cache, ETag, method and fallback tests |
| `web/embed.go` (new package) | `//go:embed all:dist`, `Sub()`, `HasAssets()` |
| `web/dist/index.html` (new) | Committed placeholder so `go build` never needs Node |

Package total: **391 cases**, `-race` clean, **zero third-party dependencies**.

---

## The design decision that removes the whole vulnerability class

> **No filesystem path derived from user input is ever opened.**

Assets are served from an `embed.FS`, and the only paths that can be opened are ones already
satisfying `fs.ValidPath` — a standard-library contract rejecting `.`, `..`, leading/trailing
slashes and backslashes.

This makes traversal **structurally impossible** rather than filtered. There is no code path
that concatenates a request path onto a root directory, which is the mistake behind most
path-traversal vulnerabilities. `safeAssetPath` adds redundant explicit checks so the intent is
legible and so a future refactor cannot silently widen it.

### Why `web/` is a separate package

`go:embed` cannot reference parent paths. Putting the directive in `internal/admin` would need
`../../web/dist`, which the toolchain rejects. So `web/embed.go` embeds its own `dist/`
subdirectory and exposes it via `fs.Sub`, and `internal/admin` imports that package. The
dependency runs in the correct direction: the server serves the frontend, it does not own it.

---

## Verification — literal bytes, not a normalising client

`curl` normalises `../` **client-side**, so testing with it proves almost nothing. Attacks were
therefore sent with `nc` as raw HTTP, bypassing any client-side normalisation.

| Raw request path | Status | Result |
|---|---|---|
| `/../etc/passwd` | **404** | rejected |
| `/../../../../etc/passwd` | **404** | rejected |
| `/assets/../../../etc/passwd` | **404** | rejected |
| `/api/v1/../` | **404** | rejected |
| `/%2e%2e/%2e%2e/etc/passwd` | 200 | SPA shell, **no leak** |
| `/..%2f..%2fetc%2fpasswd` | 200 | SPA shell, **no leak** |
| `/index.html%00.txt` | 200 | SPA shell, **no leak** |

Every response was additionally grepped for `root:` — the signature of a real `/etc/passwd`.
**Zero occurrences.**

The encoded forms returning 200 is correct: they do not resolve to a real file, so the SPA
fallback renders the shell. The important property is that the *content* is never external.

---

## SEC-11.2 — the API never returns HTML

| Path | Status | `text/html` header | JSON body |
|---|---|---|---|
| `/api/v1/nope` | 404 | no | yes |
| `/api/v1/../` | 404 | no | yes |
| `/api/unknown` | 404 | no | yes |
| `/api/v2/health` | 404 | no | yes |

A mistyped API call returns a JSON error a client library can parse, rather than an HTML
document that looks like a successful response.

---

## SEC-11.4 — caching and revalidation

| Resource | `Cache-Control` |
|---|---|
| `/`, `/index.html`, any non-`assets/` path | `no-store` |
| `/assets/app-9f2c1a.js` (content-hashed) | `public, max-age=31536000, immutable` |

`index.html` is never cached, so a redeploy is visible immediately.

**ETags are content-derived (SHA-256), not mtime-derived.** This matters specifically because
`embed.FS` has a fixed zero mtime: an mtime-based ETag would never change across rebuilds and
every redeploy would be invisible. `TestStatic_ETagChangesWithContent` pins this.

Conditional requests return **304** with an empty body; a stale validator returns the file.

---

## SEC-11.5 — the build never needs Node

`web/dist/index.html` is a **committed placeholder** that tells the operator to run
`npm ci && npm run build`. Verified: `go build ./...` succeeds with only the placeholder
present and no Node.js installed. `HasAssets()` reports whether a real build is embedded, so
ADM-7 can emit a startup warning rather than silently serving a stub.

---

## A bug in my own test

`TestStatic_RejectsNullByte` asserted that `/index\x00.html` should be **accepted**, on the
reasoning that the suffix was ordinary. It contains a null byte, so rejection is correct — the
assertion was wrong.

Rewritten to prove the rejection is caused by the null byte rather than by the extension, by
also asserting that `/index.html` and `/assets/app.js` **are** accepted.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 11.1 Serve only from `embed.FS`; no user path ever opened | ✅ `fs.ValidPath` is the authority |
| 11.2 SPA fallback; `/api` returns JSON 404 | ✅ verified live |
| 11.3 Reject `..`, null byte, backslash | ✅ verified with literal bytes |
| 11.4 `ETag` + cache headers; `index.html` `no-store` | ✅ verified live |
| 11.5 `go:embed` placeholder; build works without Node | ✅ verified |
| `/../../etc/passwd` → 404 | ✅ raw via `nc` |
| `/api/v1/../` → JSON 404 | ✅ |
| null byte → rejected | ✅ |
| `index.html` is `no-store` | ✅ |

**SEC-11 acceptance criteria: met.**

---

## Remaining

- **SEC-6**: flag wiring and the startup warning (ADM-7).
- **SEC-12**: the consolidated attack suite — the natural home for the raw-byte traversal
  cases, since they must not be written with a normalising HTTP client.
- **FE-1/FE-3** now have a working target: the console is servable, and CSP forbids
  `unsafe-inline`, so dynamic styling must come from bundled CSS or a nonce.

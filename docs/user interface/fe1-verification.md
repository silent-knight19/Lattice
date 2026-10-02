# FE-1 Verification — Scaffold and Build Pipeline

**Status:** COMPLETE. Subtasks 1.1–1.4 met.
**Code:** `web/` — `package.json`, `vite.config.ts`, `tsconfig.json`, `index.html`,
`eslint.config.js`, `src/main.tsx`, `src/App.tsx`, `src/styles/index.css`, `web/dist/`
**Verified live:** a built bundle is served by a running `lattice` daemon over HTTP.

---

## 1. Version selection was decided by evidence, not preference

The plan pinned **React 18**, written when that was current. Rather than assume, each
dependency's actual stability was checked against release dates, dist-tags, peer ranges, and
production reports.

| Package | Pinned | Evidence |
|---|---|---|
| `react` / `react-dom` | **19.3.0** | 19.x GA **2024-12-05** — ~22 months mature |
| `vite` | **8.3.2** | 8.0.0 released 2026-03-12 (204d); patch verified by building |
| `@vitejs/plugin-react` | **6.1.1** | peer `vite ^8.0.0` |
| `typescript` | **5.9.3** | see §2 — TS 7 is *incompatible with this plan* |
| `tailwindcss` | **4.3.3** | v4 stable since Jan 2025; *"for a brand-new project in 2026: v4, obviously"* |
| `@tailwindcss/vite` | **4.3.3** | peer supports `vite ^8` |
| `eslint` | **10.11.0** | `eslint@9` emits an explicit "no longer supported" deprecation |

### Dist-tags as a stability signal

```
react       latest 19.3.0    previous GA 19.0.0 = 2024-12-05
vite        latest 8.3.2     previous 7.3.6, 8.0.0 = 2026-03-12
tailwindcss latest 4.3.3     v3-lts 3.4.19 (LTS tag actively maintained)
typescript  latest 7.0.2     rc 7.0.1-rc still present; beta 6.0.0-beta still present
```

The lingering `rc`/`beta` tags on TypeScript were the first hint that `latest` was premature.

---

## 2. Why TypeScript 7 was rejected — it breaks a security control in this plan

This was the decisive finding, and it is not a "newer is riskier" judgement:

- **typescript-eslint 8.71.0 declares peer `typescript >=4.8.4 <6.1.0`** — TypeScript 7 is
  outside the supported range.
- On TS 7, `npm ci` fails with `ERESOLVE`, and a forced install makes ESLint **crash**:
  `TypeError: Cannot read properties of undefined (reading 'Cjs')` in `typescript-estree`
  (issue #12518, closed as duplicate).
- Root cause: **TypeScript 7 ships no compiler API.** Microsoft states it will "close
  whatever gaps remain with **7.1**".

**Why that matters here:** subtask **2.3 requires typescript-eslint** to forbid
`dangerouslySetInnerHTML`. Adopting TS 7 would have disabled an XSS control the plan
mandates. TypeScript 5.9.3 is the last 5.x and is comfortably inside the support window.

---

## 3. Blocking bug found: `web/dist` was gitignored

`web/dist` is embedded by `//go:embed all:dist`. The repository's `.gitignore` contained a
bare `dist/` rule, which excluded it. Consequences on a **fresh clone**:

```
$ go build ./...
web/embed.go:24:12: pattern all:dist: no matching files found
```

So the earlier claim that "`go build ./...` works without Node.js" was true locally but **false
for anyone cloning the repository**, including contributors with no Node installed. Proven by
`git archive HEAD web/`, which yielded only `embed.go` and `embed_test.go`, and by building
with `web/dist` temporarily moved aside.

Fixed with negations placed below the `dist/` rule (last match wins):

```
!web/dist/
!web/dist/**
```

Now `git status` reports `?? web/dist/` rather than `!! web/dist/`, and the built bundle is
commit-eligible while `node_modules` remains ignored.

---

## 4. Security controls, proven to fire

Lint rules were **probed with deliberately unsafe code**, because a rule that never fires
proves nothing. The probe was removed afterwards.

| Construct | Result |
|---|---|
| `dangerouslySetInnerHTML` | ✅ `react/no-danger` |
| `dangerouslySetInnerHTML` + children | ✅ `react/no-danger-with-children` |
| `document.cookie` | ✅ `no-restricted-properties` |
| bare `localStorage` | ✅ `no-restricted-globals` |
| `window.localStorage` | ⚠️ **initially missed** → fixed |
| `globalThis.localStorage` | ⚠️ **initially missed** → fixed |
| `window.sessionStorage` | ⚠️ **initially missed** → fixed |
| `document['cookie']`, `window.localStorage['setItem']` | ✅ `no-restricted-syntax` |

### 4.1 A hole in my own rule, found by probing it

`no-restricted-globals` matches only the **bare identifier**. `window.localStorage` and
`globalThis.localStorage` passed cleanly — an equally reachable path to the same persistent
store, so the SEC-2.2 guarantee was only half-enforced.

Replaced with `no-restricted-syntax` selectors covering the bare identifier, member access on
any object, and computed string keys. All seven forms are now rejected.

---

## 5. CSP compliance (SEC-5)

```
eval(        : 0
new Function : 0
document.write : 0
innerHTML    : 5  (all React DOM internals; none in src/)
```

The five `innerHTML` hits were inspected individually: prop-name string comparisons in React's
attribute handling, the `__html` branch (which our source never reaches, being forbidden by
lint), and React's `<script>`-injection guard element. `grep` over `src/` for
`innerHTML|dangerouslySetInnerHTML` returns nothing.

---

## 6. Live verification against the daemon

```
$ lattice --address 127.0.0.1:17080 --admin-address 127.0.0.1:17081 --data-dir …
lattice: WARNING: admin console ENABLED on http://127.0.0.1:17081 (loopback only)
```

The "console assets are the committed placeholder" hint is **correctly gone**, because
`HasAssets` now detects the real bundle.

```
GET /                        → 200 text/html         (real Vite index.html)
GET /assets/index-*.js       → 200 text/javascript  220,043 bytes
GET /assets/index-*.css      → 200 text/css          8,680 bytes (tailwindcss v4.3.3)
```

The CSS begins `/*! tailwindcss v4.3.3 | MIT License */`, confirming a genuine Tailwind build.

---

## 7. Build hygiene

- `npm audit` → **0 vulnerabilities** across 266 packages.
- `package-lock.json` committed; `npm ci` is the documented path (**1.3**).
- `npm run build` = `tsc --noEmit && vite build`, so a type error cannot ship (**1.2**).
- **Determinism verified:** three consecutive builds produced byte-identical output
  (`index-Bn9ozG0X.css` / `index-CWRy5yid.js` every time).
- Dev server binds **127.0.0.1** with `strictPort` and proxies `/api` to the daemon
  (`changeOrigin: false`, preserving the Host header so the daemon's HostGuard behaves as it
  does in production) (**1.4**).

---

## 8. Tests updated rather than deleted

Building a real bundle invalidated two assertions that encoded the *placeholder* era:

- `web.TestHasAssetsEmbeddedDistIsPlaceholder` → inverted to
  `TestHasAssetsEmbeddedDistIsRealBuild`. The placeholder-detection behaviour it was guarding
  is still covered separately.
- `cmd/lattice.TestNewAdminServerWarnsOnEnable` → now asserts the hint **tracks** the server's
  actual build state in both directions, rather than hardcoding one era.

A latent bug surfaced alongside: `NewStaticHandlerFS` never set `hasAssets`, so every
FS-constructed handler reported `HasRealBuild() == false` regardless of contents, making the
real-build branch untestable. Fixed, with `TestStaticHandlerFS_ReportsHasAssetsForBothStates`
covering four cases.

---

## 9. Known issue, pre-existing and out of scope

`go test -race -short ./...` intermittently reports a data race in committed
`internal/transport/peer_connection.go`, between `handleInboundConn` (read) and
`peerSupervisor.run` (write), surfaced by the committed raft test
`TestPartition_3Node_BidirectionalTrafficBlocked`.

- `internal/transport/` is **unmodified** in the working tree, and this work touches neither
  package.
- It did not reproduce across `-count=2` on `internal/raft` (152s, 0 races) nor 3 targeted
  reruns; an earlier full-repo gate passed clean.
- The uncommitted `internal/raft/{apply.go,node.go,export_test.go,apply_retry_test.go}` edits
  are unrelated pre-existing work and were **not** touched.

Reported rather than fixed: it belongs to a subsystem outside this task's scope.

---

## 10. Not in FE-1

Pages, routing, the API client, `ByteText`, and SSE land in FE-2 onward. The console renders a
shell that says the build pipeline is wired. It has never been viewed by a human in a browser —
that verification is still outstanding.

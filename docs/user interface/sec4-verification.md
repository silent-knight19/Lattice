# SEC-4 — Implementation & Verification Record

> Task: SEC-4 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**
> Depends on: [`sec3-verification.md`](./sec3-verification.md).

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/authz.go` | `Permits`, `Authz`, `PrincipalResolver`, `PrincipalFromTLS`, `RoleFromPolicy`, `PermissionFromHeaderRole`, type aliases to `internal/transport` |
| `internal/admin/router.go` | `RouteSpec`, `Router` (default-deny), `PrincipalResolver`, `NewRouter`, `Register` |
| `internal/admin/routes.go` | `routePlan` (36 routes), `Handlers`, `BuildRoutes`, `PlanFor` |
| `internal/admin/authz_test.go` | Permission matrix, plan completeness, blast-radius pinning, router default-deny, fail-closed |

Package total: **240 cases**, `-race` clean, **zero third-party dependencies**.

---

## No parallel identity system (subtask 4.1)

`Permission`, `Role`, `AuthzPolicy` and `Principal` are **Go type aliases** to their
`internal/transport` counterparts, not new types:

```go
type Permission = transport.Permission
type Role      = transport.Role
type AuthzPolicy = transport.AuthzPolicy
type Principal = transport.Principal
```

An operator who already runs `--client-authz-policy` uses the same role strings and the same
fingerprint→role file. `ParseRole` and `LoadAuthzPolicyFile` are re-exported wrappers, so
there is nothing new to learn or misconfigure (design-doc NG1).

---

## Intent vs authority

| Layer | Proves |
|---|---|
| Origin / Host / CSRF (SEC-1/2/3) | **INTENT** — this request really came from our console |
| RBAC (SEC-4) | **AUTHORITY** — this caller may do this thing |

The verification below makes the orthogonality concrete: a reader presenting a **valid CSRF
token and a valid confirmation header** is still refused every destructive route.

---

## The route plan

36 routes, each declared with a minimum permission and a justification. Two are deliberately
**admin-only rather than reader**, because a "read" of them is a bulk data export:

- **`/raft/log`** — the Raft log holds *committed commands*, which are user key/value payloads.
- **`/wal/dump`** — WAL records carry raw key/value bytes.

Also admin-only: `/lab/crash`, `/lab/cleanup-orphans`, `/raft/campaign`, `/raft/stepdown`,
`/console/exec`, `/diagnostics`, `/events`.

Data mutation (`/keys/put`, `/keys/delete`, `/lsm/compact`, `/lsm/flush`) is `PermissionWrite`.

---

## Verification

Real route plan, all handlers registered, **every request presenting as `reader`**, with a
**genuine CSRF token** so that only authorization could be the thing blocking:

| Route | Result |
|---|---|
| `GET /lsm/tree`, `/node`, `/config`, `/metrics`, `/engine/stats`, `/sstables`, `/raft/status`, `/raft/peers`, `/raft/timeline`, `/manifest`, `/wal/segments`, `/keys` | **200** |
| `POST /keys/put`, `/keys/delete`, `/lsm/compact`, `/lsm/flush`, `/lab/workload/start` | **403** |
| `POST /lab/crash`, `/console/exec`, `/raft/campaign`, `/lab/cleanup-orphans` | **403** |
| `GET /diagnostics`, `/raft/log`, `/wal/dump`, `/events` | **403** |
| `/api/v1/nope`, `/api/v2/health`, `/api/unknown` | **404** |

### Ground truth (server-side, per F12)

```
EXECUTED GET /api/v1/lsm/tree total=1
```

**Exactly one handler execution** — the read route. Five destructive POSTs carrying a valid
token and confirmation were all refused before reaching any handler.

---

## Two real bugs caught

### 1. `/api/v2/*` fell through to the SPA

The router matched on `strings.HasPrefix(path, "/api/v1")`, so `/api/v2/health` was not
recognized as API traffic and fell through to the SPA fallback — returning **200 with
HTML**. Two problems: it confirms an API exists to a prober, and a mistyped API call returns
a confusing HTML 200 instead of a JSON 404.

Fixed by matching on `"/api"` — the whole API surface is now isolated from the fallback.

### 2. The declared `Permission` was decoration

The first implementation declared `Permission` on each `RouteSpec` but left enforcement to a
separate `Authz` middleware that the caller had to remember to wrap around each route.
**Forgetting one wrapper would have exposed an admin-only route to everyone.** The declared
policy was not structurally enforced.

Fixed by binding enforcement into the router: it resolves the principal and checks
`spec.Permission` itself, unconditionally. `Router.SetPrincipalResolver` must be called; with
no resolver **every route is denied**. The declared permission can no longer be silently
ignored.

This was found while attempting to write the end-to-end verification — the probe could not
be wired sensibly, which is what exposed the gap.

---

## Three bugs in my own tests

Worth recording, since two would have masked real behaviour:

1. **`PermissionAdmin` is not a superset of `PermissionWrite`.** They are bits `1<<2` and
   `1<<1`; admin-only destructive routes are intentionally not write-gated. My completeness
   test wrongly required Write and flagged all seven.
2. **An assertion contradicted its own setup.** The test built a principal with
   `Authenticated: true` and then asserted that it must *not* be permitted.
3. **The header prefix is `"role "` with a space**, not `"role:"`. The debug run showed
   `"Role: admin"` lowercases to `"role: admin"`, which correctly does not match.

In each case the **code was right and the test was wrong**; I fixed the test. All three were
diagnosed by inspecting actual values rather than adjusting expectations until green.

---

## Fail-closed properties

| Situation | Behaviour |
|---|---|
| No `PrincipalResolver` set | **every route denied** |
| Resolver returns nil | denied |
| Principal not authenticated | denied |
| Unknown role (e.g. `"superuser"`, `"ADMIN"`) | denied |
| Route declares no permission | `NewRouter` **refuses to start** |
| Duplicate registration | refused |
| Unknown `/api` path | JSON 404, never the SPA |

`TestRoutePlan_EveryRouteDeclaresPermission` is a completeness test over all 36 routes, so
adding a route without a declared permission fails CI.

---

## Residual risk, stated plainly

The role must come from a verified client certificate via
`RoleFromPolicy(policy, fingerprint)`. `PermissionFromHeaderRole` exists only for
proxy-fronted deployments and is documented as a fallback that must never be the sole basis
for `PermissionAdmin`: a header is client-supplied and forgeable. It also refuses to set
`Authenticated`, so a header-derived principal is never permitted — asserted in tests.

**Loopback binding remains non-authenticating against other local users.** Carried to SEC-13.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 4.1 Reuse transport roles/permissions; no parallel system | ✅ type aliases + wrappers |
| 4.2 Explicit permission for every route | ✅ 36 routes in `routePlan` |
| 4.3 Default-deny router, fail closed on missing metadata | ✅ enforced by the router itself |
| 4.4 Unknown role grants nothing | ✅ `Role("superuser")` denied |
| 4.5 Reuse fingerprint→role policy file | ✅ `ParseAuthzPolicyJSON` / `LoadAuthzPolicyFile` |
| 4.6 Prefer certificate-derived role | ✅ `PrincipalFromTLS`; header fallback never authorizes |
| `reader` on a write route → 403 | ✅ verified live |
| unknown role → 403 | ✅ |
| unregistered `/api/v1/x` → 404 | ✅ verified live |
| every route has permission metadata | ✅ completeness test |

**SEC-4 acceptance criteria: met.**

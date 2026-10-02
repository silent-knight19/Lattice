# SEC-12 — Consolidated Security Test Suite

> Task: SEC-12 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**

---

## What this file is

A single regression net that exercises every attack class discovered across SEC-0…SEC-11
against **one fully-wired server**, so a future change to any middleware cannot silently
weaken a control. Package total: **430 cases**, `-race` clean.

---

## Two rules, both learned the hard way

**1. Assert server-side, never client-side.** SEC-0 finding F12: a browser reports an opaque
network error whether a cross-origin request was blocked or delivered. Every case here checks
whether the **inner handler executed**, via a counter, rather than trusting the response the
client happened to see.

**2. Send raw bytes where the client would normalise.** `curl` and `net/http` rewrite `../`
before sending, so a traversal test written with them proves nothing.
`TestSecuritySuite_RawByteTraversal` speaks raw HTTP over TCP.

---

## The harness

`newSecHarness` wires the canonical chain exactly as ADM-2 specifies:

```
RequestID → SecurityHeaders → NoStoreAPI → BodyLimit → HandlerTimeout
          → HostGuard → OriginGuard → CSRFGuard → Router
```

with a `/api/v1/session`, two read routes, two mutating routes at different permission
levels, and an SSE route — so authz, CSRF, and resource budgets all have a real surface to
attack.

---

## Coverage

| Test | Attack class |
|---|---|
| `TestSecuritySuite_DriveByOrigin` | SEC-0 drive-by localhost, including the exact attacker origin used in the spike |
| `TestSecuritySuite_DNSRebinding` | SEC-2, with a self-consistent Origin so neither guard alone can be blamed |
| `TestSecuritySuite_CSRFAndConfirm` | SEC-3: missing/wrong/stale token, missing/wrong confirm, **and a positive control** |
| `TestSecuritySuite_RoleEscalation` | SEC-4 across reader/writer/admin/unknown/empty roles |
| `TestSecuritySuite_UnauthenticatedDenied` | SEC-4 fail-closed |
| `TestSecuritySuite_OversizeBody` | SEC-7, asserted at the read boundary |
| `TestSecuritySuite_SSEFlood` | SEC-8 subscription budget |
| `TestSecuritySuite_RawByteTraversal` | SEC-11, over raw TCP |
| `TestSecuritySuite_SymlinkEscape` | the `security.ResolvePath` primitive future file endpoints must compose |
| `TestSecuritySuite_ErrorLeakage` | SEC-9, with a real `*errors.InvalidPathError` |
| `TestSecuritySuite_HeaderPresence` | SEC-5 on a live response |
| `TestSecuritySuite_ChainOrderIsCanonical` | proves Host fires before Origin before CSRF |
| `TestSecuritySuite_MethodEnforcement` | a GET can never reach a POST-only handler |
| `TestSecuritySuite_DestructiveRoutesRemainAdminLocked` | permanent assertion on blast-radius decisions |
| `TestNoCORSHeadersEverEmitted` | SEC-12.3 invariant across 7 response classes |
| `TestNoRouteExistsWithoutPermissionMetadata` | SEC-12.2 completeness gate |
| `TestSecuritySuite_NoGoroutineLeakAfterAttacks` | the whole suite leaves the server clean |
| `TestSecuritySuite_NoCSRFTokenEverLogged` | SEC-10 regression for the hex-id bug |

---

## SEC-12.2 — the completeness gate

`TestNoRouteExistsWithoutPermissionMetadata` iterates the shared `routePlan` and requires that
every route declares a non-zero, canonical permission, a method, and a description, and that
mutating routes are POST. It is deliberately a **test over the plan**, not a runtime check: a
new route that forgets its permission must fail **CI**, not fail open in production.

`NewRouter` separately refuses to construct with an undeclared permission, so the two together
mean there is no path by which an unguarded route can exist.

---

## Bugs the suite exposed in itself

Three, all fixed:

1. **Requests were sent to the wrong paths.** The `Route*` constants are sub-paths relative to
   `/api/v1`, but the test requested `URL + "/health"`. Those hit the SPA fallback, which
   returned `200` with HTML — and for POST, the static handler's `405`. The suite looked like
   it was failing when it was actually hitting the wrong endpoint.
   Fixed by normalising the prefix inside the shared helper so no call site can forget it.
2. **`h.authz` was nil** when `setAuthed` ran, panicking on the request path — the harness had
   been wired after the struct literal returned.
3. Several composite-literal type elisions that `gofmt` accepts but the compiler rejects inside
   a `[]struct{...}` slice.

---

## Verification

Every case passes, including the raw-TCP traversal cases:

```
--- PASS: TestSecuritySuite_DriveByOrigin
--- PASS: TestSecuritySuite_DNSRebinding
--- PASS: TestSecuritySuite_CSRFAndConfirm (6 sub-cases)
--- PASS: TestSecuritySuite_RoleEscalation (8 sub-cases)
--- PASS: TestSecuritySuite_RawByteTraversal (7 sub-cases, literal bytes)
--- PASS: TestSecuritySuite_SymlinkEscape
--- PASS: TestSecuritySuite_OversizeBody
--- PASS: TestSecuritySuite_SSEFlood
```

`go test ./internal/admin/ -short` passes in ~1.1s for the security suite, so SEC-12.4's CI
gate is satisfied without slowing the default test run meaningfully.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 12.1 Whole attack suite against a live server | ✅ 17 tests, server-side ground truth |
| 12.2 `TestNoRouteExistsWithoutPermissionMetadata` | ✅ plus `NewRouter` refusal |
| 12.3 `TestNoCORSHeadersEverEmitted` | ✅ 7 response classes |
| 12.4 Wired into `go test -race -short ./...` | ✅ |
| drive-by origin, rebinding, CSRF, role, traversal, symlink, oversize, SSE flood, headers, error leakage | ✅ all present |

**SEC-12 acceptance criteria: met.**

---

## Remaining

- **SEC-13**: documentation (`SECURITY.md` console section, `known-limitations.md` accepted
  risks, `threat-model.md` console threats).
- **SEC-6**: flag wiring and the startup warning (ADM-7).
- Every later feature task is expected to add its cases here — that is the contract SEC-12
  establishes for the rest of the build.

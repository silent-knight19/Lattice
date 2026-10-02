# SEC-6 Verification — Loopback-Only Binding Enforcement

**Status:** COMPLETE. Subtasks 6.1–6.5 met. **This closes Part 0.**
**Code:** `cmd/lattice/admin.go`, `internal/admin/server.go`, validation in `cmd/lattice/config.go`
**Tests:** 30 in `cmd/lattice/admin_test.go`, 3 in `web/embed_test.go` — all `-race` clean

---

## 1. What shipped

| Subtask | Requirement | Implementation | Test |
|---|---|---|---|
| 6.1 | Disabled by default | `AdminAddress` defaults to `""`; `NewAdminServer` returns `(nil, nil)` | `TestAdminFlagDisabledByDefault`, `TestNewAdminServerDisabledByDefault` |
| 6.2 | Non-loopback refused without opt-in | `Config.Validate` loopback check | `TestAdminFlagNonLoopbackRequiresBothOptIns/no_opt-ins` |
| 6.3 | Loud startup warning | Two-line `WARNING` block + placeholder hint | `TestNewAdminServerWarnsOnEnable` |
| 6.4 | Second opt-in required | `--admin-allow-remote` **and** `--insecure-transport` | `TestAdminFlagNonLoopbackRequiresBothOptIns/*` |
| 6.5 | Tests | 30 lifecycle + flag tests | this document |

Enforcement is layered:

1. **`Config.Validate`** — flag parsing, so a bad invocation fails before any setup.
2. **`NewAdminServer`** — defence in depth, re-checking both opt-ins, because it is reachable
   from tests and future call sites that bypass flag parsing.
3. **`admin.BindLoopback`** — pre-bind loopback check, then **post-bind verification against the
   address the kernel actually returned**, so a surprising resolution cannot escape.
4. **Wildcard refusal** — `0.0.0.0`/`::` refused at all three layers.

---

## 2. The decision that mattered: two opt-ins, not pprof's rule

The task text said "identical rule to `--pprof-address`". That was wrong, and following it
literally would have been a security *regression*:

- pprof refuses **every** non-loopback bind and has **no** `--insecure-transport` escape at all
  (`cmd/lattice/config.go:509-511`). There is no flag to bolt 6.4 onto, so 6.4 becomes
  unimplementable.
- `--metrics-address` *does* allow `--insecure-transport`, but then 6.4's second opt-in is
  redundant — one flag already suffices.

**Shipped:** admin is stricter than metrics and looser than pprof. A remote bind requires
`--insecure-transport` **and** `--admin-allow-remote`. `--insecure-transport` is frequently
enabled for the data path, so relying on it alone would silently publish engine internals;
the second flag makes the dangerous act explicit.

Each refusal names the **missing** opt-in, and the tests assert that specific message so they
cannot pass for an unrelated reason:

```
no opt-ins             → "…must be a loopback interface…; a non-loopback admin bind additionally
                          requires --insecure-transport and --admin-allow-remote"
insecure-transport only → "…requires the explicit --admin-allow-remote opt-in
                          (--insecure-transport alone is deliberately not sufficient)"
admin-allow-remote only → rejected for missing --insecure-transport
```

---

## 3. Findings

### 3.1 `HasAssets` reported a real build when it was a placeholder

`web.HasAssets` returned `len(entries) > 0` over `dist/assets`. The committed
`assets/placeholder.txt` made that **true**, so the "run `npm run build`" startup warning would
have **never fired** — the exact opposite of its purpose.

Fixed to match on the file *types* a real Vite bundle emits (`.js`/`.mjs`/`.css`) rather than
the directory's existence. This is a warning-only control, so a false positive would cost a
confusing message but never a permission — but a false *negative* cost the operator the only
signal that their UI was a stub.

`TestHasAssetsEmbeddedDistIsPlaceholder` asserts the committed `dist` still reports placeholder,
so the warning stays visible until FE-1 ships a real build.

### 3.2 Fail-fast requires binding during initialization

The console binds in **Step 2c**, alongside pprof, *before* the engine recovers the data
directory. A port conflict therefore fails before the engine commits to opening storage —
matching how `NewPprofServer` is already ordered. Binding lazily at `Start()` would have
discovered the conflict only after recovery.

`NewServer` binds **synchronously** in its constructor, so `TestAdminServerPortConflictFailsFast`
observes the failure at construction and asserts no server is returned.

### 3.3 Shutdown ordering

The console drains **before** `eng.Close()`, so in-flight requests observe a live engine. The
same ordering appears in both the `cleanup()` path and the orderly-shutdown path. Gauges are
unregistered in both.

---

## 4. Tests

**Lifecycle (`NewAdminServer`):** disabled-by-default · warning content · non-loopback refused
without opt-ins · remote allowed with both · wildcard refused · post-bind loopback verification ·
port conflict · repeated start rejected · idempotent shutdown · port released · stops serving
after shutdown · metric registration, double-registration, nil-safety.

**Flag layer (`ParseFlags`):** default off · loopback accepted · non-loopback matrix · both
opt-ins accepted · `--admin-allow-remote` without address errors · wildcard rejected · malformed
address/port rejected · port collisions with data/metrics/pprof · mutually exclusive policy
sources.

**`web`:** placeholder rejected · real bundle accepted · committed `dist` is a placeholder.

### Properties asserted rather than implementation

- Loopback is verified on **`srv.Addr()`** — the address the kernel returned — not on the flag
  that was passed (`TestAdminServerBindsLoopbackOnly`).
- Shutdown is verified by **the port refusing connections afterwards**, not by inspecting
  internal state (`TestAdminServerStopsServingAfterShutdown`).
- Metric registration is verified through **rendered Prometheus output**, the same path a real
  scrape takes, so a gauge that registers but fails to render is caught.
- `TestAdminServerAllowsRemoteWithBothOptIns` binds a real non-loopback interface address and
  skips if the host has none, rather than asserting against a wildcard that is refused anyway.

---

## 5. ADM-1 dependency

SEC-6 requires a server to wire. **`admin.NewServer` (ADM-1) was implemented as part of this
work**: synchronous bind, middleware chain construction, accept loop, graceful drain,
idempotent shutdown, and the panic-to-500 recoverer. `cmd/lattice/admin.go` depends on it.

ADM-1's default principal resolver **fails closed**: with no policy or no verified client
certificate it returns `nil` and every request is denied. The console is reachable and
correctly refuses everything until mTLS is configured. The loopback-principal /
session-bootstrap question from the plan therefore remains **open by design** — closing it
without mTLS would mean granting admin to any local process, which SEC-0's drive-by finding is a
direct argument against.

---

## 6. Deliberately not done

The following remain open and are recorded in `docs/known-limitations.md`:

- **Endpoint handlers** — `NewServer` registers only `/session` and `/events`; the rest arrive
  with their phases. An absent handler means the route is absent (default-deny).
- **The `/system` posture page** SEC-13 documentation was corrected not to claim. Not written.
- **Unix-socket binding** — would narrow Limitation 102.

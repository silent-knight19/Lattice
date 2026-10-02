# SEC-2 — Implementation & Verification Record

> Task: SEC-2 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**
> Depends on: [`sec1-verification.md`](./sec1-verification.md).

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/hostguard.go` | `HostAllowlist`, `canonicalHost`, `parseHostHeader`, `SelfHosts`, `HostGuard`, `writeBadHost` |
| `internal/admin/bind.go` | `BindOptions`, `BindResult`, `BindLoopback` (pre-bind + post-bind verification) |
| `internal/admin/hostguard_test.go` | Host parsing, allowlist decisions, DNS-rebinding test, leakage |
| `internal/admin/bind_test.go` | Pre-bind enforcement, wildcard refusal, post-bind derivation, default-port elision |

Package total: **162 cases**, `-race` clean, **zero third-party dependencies**.

`parseHostHeader` **reuses** `splitHostPortStrict` from `origin.go`, so the Host and Origin
parsers cannot drift apart on edge cases such as default-port elision.

---

## Why Host validation is a separate control

DNS rebinding defeats `OriginGuard` entirely. The attacker owns `attacker.example` and
points its DNS record at `127.0.0.1`. The browser connects to the loopback admin server
believing it is talking to `attacker.example`, and sends:

```
Host: attacker.example
Origin: http://attacker.example
```

Both headers are **self-consistent** and both are attacker-controlled. There is nothing
an Origin check can flag. Loopback binding does not help either, because the connection
genuinely does go to loopback.

`TestHostGuard_DNSRebinding` encodes exactly this. It first asserts the precondition — that
`OriginGuard` **alone would let the request through** — and then shows `HostGuard` in front
stops it. Without that precondition the test could pass for the wrong reason.

---

## Verification

### DNS rebinding (rebinding is what makes this hard)

```
curl -H 'Host: attacker.example' -H 'Origin: http://attacker.example' .../crash
  → 400 Bad Request        handler executed: 0
```

### Origin-only drive-by (SEC-1 layer still intact behind SEC-2)

```
curl -H 'Host: 127.0.0.1:7070' -H 'Origin: http://evil.com' .../crash
  → 403 Forbidden          handler executed: 0
```

### Legitimate console traffic

```
curl -H 'Host: 127.0.0.1:7070' -H 'Origin: http://127.0.0.1:7070' .../crash
  → 200 OK                handler executed: 1   ← the only execution
```

### Real browser, unchanged SEC-0 attacker page

```
PASS — execution log EMPTY. Zero executions.
```

Ground truth was taken server-side per **F12**: the browser again reported
"Failed to fetch" for every probe, which is indistinguishable from a blocked request. Only
the server-side counter proves the difference.

---

## Middleware ordering

The canonical chain places `HostGuard` **before** `OriginGuard`:

```
Recoverer → SecurityHeaders → HostGuard → OriginGuard → authz → CSRF → route
```

HostGuard is cheaper (one header, no URL parse) and rejects rebinding before any origin
work happens. Defensively, checking Host first also means a rebound request never reaches
origin parsing at all.

---

## Rejection semantics: 400, not 403

An unrecognized `Host` means the client addressed a server that is not us. That is a
**malformed request**, not an authorization failure, and the distinction matters when
reading logs during an incident. This is the one deliberate divergence from `OriginGuard`,
which returns 403 because a forbidden origin *is* an authorization decision.

Both responses are constant-bodied and leak nothing (asserted by
`TestHostGuard_RejectionLeaksNothing`): no allowlist contents, no echo of the supplied Host,
no CORS headers.

---

## Fail-closed by construction

| Situation | Behaviour |
|---|---|
| `nil` HostAllowlist | denies everything, including a valid Host |
| Empty allowlist | denies everything |
| Malformed configured entry | dropped, never silently accepted |
| Empty `Host` header | **denied** (unlike `Origin`, there is no legitimate empty-Host case) |
| Wildcard bind (`0.0.0.0`) | **always refused**, even with the remote opt-in |

**Empty Host is denied** — this is a deliberate difference from SEC-1, where an absent
`Origin` is allowed. Every HTTP/1.1 request carries a Host, and there is no legitimate
console client that omits it. Failing closed is the entire point of this guard.

### Wildcard binds are refused unconditionally

`BindLoopback` refuses `0.0.0.0` / `::` **even when `AllowRemote` is set**. Three reasons:

1. It exposes the console on every interface, which is never the operator's intent when they
   enable a *remote* bind (they mean one specific interface).
2. It yields **no derivable Host allowlist** — `SelfHosts` returns `nil` for a wildcard,
   so there is no legitimate `Host` value to accept, which would either break the server or
   tempt an operator into disabling the guard.
3. It is the configuration most likely to be reached accidentally.

---

## Post-bind verification (subtask 2.3)

`BindLoopback` mirrors `cmd/lattice/pprof.go:50-56`, which the codebase already established
(`SEC-P13-M04-002`). Three checks, in order:

1. **Pre-bind** — non-loopback refused unless explicitly opted in.
2. **Bind synchronously** — a port conflict fails here, not later at `Start()`.
3. **Post-bind** — verify what the *kernel* actually bound. Configuration can lie;
   `listener.Addr()` cannot.

Allowlists are derived from the **bound** address, never the requested string. With
`Addr: "127.0.0.1:0"` the kernel assigns an ephemeral port, and the derived allowlist
correctly uses that real port rather than `0` — asserted by
`TestBindLoopback_PostBindVerification`.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 2.1 `HostGuard` with allowlist derived from the **bound** address | ✅ `BindLoopback` + `SelfHosts` |
| 2.2 Applied to all routes **and** the SSE endpoint | ✅ applied in the canonical chain, ahead of routing |
| 2.3 Post-bind loopback verification | ✅ `BindLoopback` mirrors `pprof.go` |
| `Host: evil.com` → 400 | ✅ |
| `Host: 127.0.0.1:9999` → 400 | ✅ |
| correct host → 200 | ✅ |
| wildcard bind without opt-in → startup error | ✅ (and refused *with* opt-in too) |

**SEC-2 acceptance criteria: met.**

---

## What remains open

- **SEC-3 (CSRF token)** is still required. Host + Origin defeat cross-site *delivery*, but
  a token is the independent layer that also protects against a future origin/host bypass.
- **Local privilege**: loopback binding is still not an authentication boundary against
  other local users. Carried to SEC-13 as a documented accepted risk.

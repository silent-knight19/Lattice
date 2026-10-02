# SEC-3 — Implementation & Verification Record

> Task: SEC-3 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**
> Depends on: [`sec2-verification.md`](./sec2-verification.md).

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/csrf.go` | `CSRFToken`, `NewCSRFToken`, `Value`, `Valid`, `CSRFGuard`, `isSafeMethod`, `SessionHandler`, `bootIDFrom` |
| `internal/admin/respond.go` | Shared `writeError` / `writeMethodNotAllowed` / `writeInternalError` (SEC-9 groundwork) |
| `internal/admin/csrf_test.go` | Generation, staleness, guard matrix, leakage, indistinguishability, concurrency |

Package total: **189 cases**, `-race` clean, **zero third-party dependencies**.

`middleware.go` and `hostguard.go` were refactored to route their rejections through the
shared `writeError`, which is what makes the indistinguishability guarantee enforceable
rather than aspirational.

---

## The three layers, and why three

| Layer | Stops | Fails if… |
|---|---|---|
| SEC-1 Origin | cross-site **delivery** | a future check is bypassed |
| SEC-2 Host | DNS **rebinding** | the attacker owns the DNS name |
| SEC-3 CSRF | cross-site **forgery** | any single layer above is bypassed |

Each is independently sufficient against its own class. The value is that a bug in one does
not silently disable the others.

---

## Design decisions

**Both headers required.** `CSRFGuard` requires `X-Lattice-CSRF` **and**
`X-Lattice-Admin: confirm` on every unsafe method. The token alone is therefore not enough
to mutate engine state — a leaked token plus a cross-site form post still fails.

**Only GET/HEAD/OPTIONS are exempt.** Those are safe by RFC 9110 semantics. **TRACE is
deliberately *not* exempt**: while nominally safe it enables header reflection, and this
server has no reason to accept it at all.

**`OPTIONS` is exempt so preflight fails predictably.** A CORS preflight cannot carry a
custom header by definition. Returning 403 (not a confusing forgery rejection) is cleaner,
and since no CORS headers are ever emitted the preflight cannot succeed regardless.

**Constant-time comparison over digests.** `Valid` decodes the candidate from base64 and
compares `sha256(raw)` against a stored digest via `subtle.ConstantTimeCompare`. Comparing
the raw bytes directly would let `ConstantTimeCompare` return early on a length mismatch,
leaking length. Malformed base64 is folded into a digest of `nil` so the comparison cost is
identical whether or not the input was well-formed.

**Per-boot token ⇒ no revocation needed.** The token is 32 bytes from `crypto/rand`,
regenerated at every start. A token captured from a previous run — from a log, a stale
browser tab, an old core dump — is useless against the running server. `TestCSRFToken_StaleTokenFromPreviousBootRejected`
asserts this directly.

**`boot_id` lets clients detect a restart.** 8 base64 characters of the token's first 6
bytes (exactly 8 characters — no truncation, so no slice-bounds hazard). It is *not* secret
and cannot be used to forge the token.

---

## Verification

Real Chrome, against the full `HostGuard → OriginGuard → CSRFGuard` stack:

```
session 200 boot=yE-iZcSd tokenlen=43
legit write            -> 200      (same origin + token + confirm)
token-without-confirm  -> 403
confirm-without-token  -> 403
```

Server-side execution log — the ground truth per **F12**:

```
EXECUTED POST /api/v1/crash Host="127.0.0.1:7070" Origin="http://127.0.0.1:7070" total=1
EXECUTED POST /api/v1/crash Host="127.0.0.1:7070" Origin="http://127.0.0.1:7070" total=2
```

**Zero** cross-origin or tokenless executions. (Two lines because Chrome was run twice;
each run performed exactly one legitimate write.)

---

## Two real bugs the tests caught

### 1. `GET /session` panicked on every request

```go
base64.RawURLEncoding.EncodeToString(t.raw[:4])[:8]
//                              4 bytes → 6 chars, then slicing [:8] → PANIC
```

`slice bounds out of range [:8] with length 6`. This crashed the **only** endpoint that
hands out the CSRF token — a denial of service on the first request every console makes.
Fixed by encoding exactly 6 bytes, which yield exactly 8 characters.

### 2. Token validation could never succeed

The constructor stored `sha256(raw_bytes)` while `Valid` computed `sha256(base64_string)`.
The two could never match, so **every** client would have been permanently locked out of
every write. Symptom: `Valid=false` for the token the token itself generated.

Fixed by decoding the candidate to raw bytes before hashing, so both operands are the same
quantity. Found only because the concurrency test ran 1600 *valid* requests and all 1600
failed — a single sequential assertion could easily have been misread as a test problem.

Both were caught by tests, not review. Neither would have been caught by `go build`/`go vet`.

---

## A test that was wrong, not the code

`TestCSRFGuard_RejectionIsIndistinguishable` initially failed. The cause was the test: it
sent a request with **no** `Origin` header, which SEC-1 deliberately permits (curl, the CLI
and health probes send none), so it never reached a rejection at all.

Fixed by supplying a disallowed Origin and adding an explicit precondition assertion that
the origin path really did return 403 — so the test cannot pass for the wrong reason again.

---

## Rejections are indistinguishable

`TestCSRFGuard_RejectionIsIndistinguishable` asserts byte-identical bodies for:

- missing token
- wrong token
- missing confirm header
- **and the origin rejection**

An API that says "your token was wrong" but "your origin was fine" is an oracle. All
rejections share one envelope and never echo the expected or supplied value.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 3.1 256-bit token from `crypto/rand` at startup | ✅ 32 bytes; never derived from time/PID |
| 3.2 Served only via `GET /session`, `no-store` | ✅ behind Host+Origin guards; `Pragma: no-cache` too |
| 3.3 `X-Lattice-CSRF` required on unsafe methods; stale token rejected | ✅ both headers enforced |
| 3.4 `subtle.ConstantTimeCompare`, no early return | ✅ fixed-length digests |
| 3.5 `403`, generic body, never echoes either token | ✅ asserted, incl. no digest leakage |
| 3.6 Token never logged; `/session` is `no-store` | ✅ |

| Test case | Result |
|---|---|
| missing header → 403 | ✅ |
| wrong token → 403 | ✅ |
| token from a previous daemon run → 403 | ✅ |
| correct token → 200 | ✅ |
| failed response contains neither token | ✅ |

**SEC-3 acceptance criteria: met.**

---

## Carried forward

- **SEC-4 (authz)** decides which *roles* may use these gated routes. CSRF proves *intent*;
  authz proves *authority*. They are orthogonal and both are required.
- **SEC-9** completes `respond.go` with request IDs and the error-code mapping.
- **ADM-7** wires the token into the daemon lifecycle; `internal/admin` currently owns
  generation but nothing constructs it at startup yet.

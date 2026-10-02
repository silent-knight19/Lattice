# SEC-10 — Implementation & Verification Record

> Task: SEC-10 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/log.go` | `AdminLogger`, `SanitizeLogField`, `RedactValue`, `isHighEntropyToken`, `AuditMiddleware`, `responseRecorder` |
| `internal/admin/log_test.go` | Injection, truncation, redaction, audit-trail and nil-safety tests |

Package total: **364 cases**, `-race` clean, **zero third-party dependencies**.

`events.go`'s sanitiser was **removed** and now calls `SanitizeLogField`, so there is exactly
one implementation of the escaping rule.

---

## What probing the existing logger actually found

Before writing anything, I measured `internal/logger`. Three of four assumptions turned out to
be wrong, and one thing turned out to be **already safe**:

| Probe | Result |
|---|---|
| newline in a value | **SAFE** — emitted as the two characters `\n` (slog quotes every field) |
| ANSI escape in a value | **SAFE** — emitted as `<ESC>` |
| 200 KB value | **LEAKED** — logged in full, no truncation |
| `csrf_token` under key `csrf_token` | redacted |
| **same secret under key `request_id`** | **LEAKED** |
| **`csrf=SECRET` inside a message** | **LEAKED** |
| **`tls_key`, `cert`, PEM body** | **LEAKED** — not in the sensitive-key list at all |

Two consequences worth stating plainly:

1. **Forgery defence is defence in depth, not the primary control.** slog's quoting already
   neutralises newlines and escapes. `SanitizeLogField` guarantees the property even if a field
   later reaches a non-quoting sink — but the honest claim is "two layers", not "we fixed it".
2. **The base logger's redaction is purely key-name based and has real gaps.** The admin logger
   therefore redacts by **value semantics** as well.

---

## The rule

Three independent redaction triggers, because key-name matching alone demonstrably leaks:

1. the field **name** is sensitive (extended list: `csrf_token`, `tls_key`, `private_key`,
   `cert`, `cert_pem`, `authorization`, `cookie`, …)
2. the **value** embeds PEM material (high precision, no false positives)
3. the value has the **shape** of a bearer token: ≥32 chars, mixed character classes, no
   spaces, and **not pure hex**

Plus unconditional truncation to 256 bytes on a rune boundary.

---

## Live verification

A POST whose key contained a newline and a fake JSON log line produced **4 records**:

```
line 1: VALID JSON
line 2: VALID JSON
line 3: VALID JSON
line 4: VALID JSON
invalid lines: 0
```

The forged text survived only as escaped data *inside* records — never as a record.

The audit line as actually written:

```
request_id = 2a4c0dbdccdfe70838ab4153e22f337a | method POST | route /api/v1/keys/put
           | decision allowed | role writer | fp deadbeefcafe
```

And the live CSRF token, deliberately logged under the neutral field name `request_id`, was
**`[REDACTED]`** — the exact leak the base logger had.

---

## A bug I introduced, then caught

The first working version redacted the audit line's own `request_id`:

```json
{"msg":"admin audit","request_id":"[REDACTED]","method":"POST",...}
```

A 32-char hex string passed my entropy test (letters + digits, no symbols), so it was
classified as a token. That silently **broke the log correlation SEC-9.4 exists to provide** —
the operator could no longer tie a browser error to a log line.

Fixed by exempting hex identifiers, including common digest prefixes (`sha256:…`). A regression
test now asserts that a 32-hex request id, an uppercase fingerprint, and a `sha256:` digest are
all preserved, while a same-length base64url token is still redacted.

This is the kind of bug that only surfaces when you actually look at real output: the security
assertions all passed, and the feature was quietly broken.

---

## Two nil-dereference panics fixed

Found by tests, both on the request path:

- `AuditMiddleware(nil, nil)` panicked because `resolve(r)` was called unguarded.
- `(*AdminLogger)(nil).Info()` panicked dereferencing `a.inner`.

Both are now structural: `inner0()` returns a `nopLogger` for a nil receiver, and the resolver
is guarded. Nil-safety is a property of the type here, not a per-method afterthought.

---

## Audit design decisions

**One line per mutating request**, written whatever the outcome, including a panic further down
the chain. It records the **certificate fingerprint**, never certificate material — a SHA-256 of
public bytes is the only safe way to identify a caller in a log.

**Read-only requests are not audited by default.** They are high volume and lower risk;
logging every dashboard poll would bury the signal.

**`responseRecorder` forwards `Flush`.** Without it the audit wrapper would silently break the
SSE endpoint, since `http.Flusher` would not be visible through the wrapper. `Unwrap()` is also
provided for the same reason.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 10.1 Log via `internal/logger` | ✅ JSON through the existing logger |
| 10.2 Control chars stripped + truncated | ✅ verified live, 0 forged lines |
| 10.3 Never log tokens / TLS keys / certs | ✅ redacted even under neutral field names |
| 10.4 One audit line per mutating request | ✅ verified live with all required fields |
| key with `\nFAKE LOG LINE` → exactly one line | ✅ 4 records, 0 forged |
| token values never in captured output | ✅ |

**SEC-10 acceptance criteria: met.**

---

## Remaining

- **SEC-11**: static asset serving over `embed.FS`.
- **SEC-12**: consolidated attack suite — the natural home for a regression test that a
  32-hex request id is never redacted, since that bug was security-adjacent.

---

## Addendum (SEC-6 work): a flaky FALSE-POSITIVE leak test in `internal/logger`

Found while running the **full repository** gate for the first time — a gate that had never
been run end-to-end before, and it immediately paid for itself.

### Symptom

```
--- FAIL: TestSEC002_ExpandedSensitiveKeywords/card_cvc
    raw secret value leaked in output for key "card_cvc":
    {"time":"...19.459888 +0530 IST",...,"card_cvc":"[REDACTED]"}
```

The value **is** `[REDACTED]`. The test reported a leak that never happened.

### Root cause

`internal/logger/nested_redaction_test.go` asserted non-leakage with:

```go
if strings.Contains(buf.String(), tc.rawVal) {   // tc.rawVal == "888"
```

`buf.String()` is the **entire log line, including the `time` field**. The timestamp carries
sub-second digits, so the secret `"888"` appeared inside the legitimate timestamp
`"...19.459888"`. The assertion therefore failed whenever the clock happened to produce those
three digits — a **clock-dependent flake**, not a redaction failure.

This is a security test in the redaction suite emitting a **false leak report**. The practical
harm is credibility: a flaky "SECRET LEAKED" in CI trains reviewers to dismiss real findings,
which is the worst possible failure mode for a control whose entire job is to be believed.

### Fix

The leak scan now excludes the `time` field and re-marshals the remainder, so the secret is
still checked everywhere it could actually be logged — the message and every other field.

The fix was verified in both directions:

- **False positive gone:** `-count=20` passes.
- **Still detects a genuine leak:** a temporary test logged `"888"` under a key the redactor
  does not know; the scan still caught it. The assertion was not weakened into something that
  can never fail. (That temporary test was removed after proving the point.)

### Why it belonged here

It is a defect in the redaction suite that SEC-10 verified, discovered only because SEC-6
required running the full repository gate rather than the admin package alone.

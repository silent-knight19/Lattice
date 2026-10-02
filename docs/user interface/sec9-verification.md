# SEC-9 — Implementation & Verification Record

> Task: SEC-9 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE + VERIFIED.**

---

## What was built

| File | Contents |
|---|---|
| `internal/admin/errors.go` | `CodeForError`, `statusForCode`, `Fail`, `FailRequest`, `WriteCoded`, `ErrorSink`, static message table |
| `internal/admin/requestid.go` | `RequestID` middleware, `NewRequestID`, context helpers |
| `internal/admin/respond.go` | `writeError` now delegates to `WriteCoded` — one envelope, one message table |
| `internal/admin/errors_test.go` | Mapping, wrapping, and the leak assertions |

Package total: **339 cases**, `-race` clean, **zero third-party dependencies**.

---

## The concrete risk, not a hypothetical one

`(*latticeerrors.InvalidPathError).Error()` in this codebase renders:

```
invalid path "/Users/operator/.ssh/id_ed25519" outside root "/var/lib/lattice/data":
symlink /tmp/evil -> /Users/operator/.ssh
```

Forwarding that verbatim would hand a caller the filesystem layout, the operator's home
directory name, and the data directory — and every handler's error string becomes a probing
oracle distinguishing "no such file" from "permission denied" from "corrupt".

---

## Verified: server sees everything, client sees nothing

Same request, both views:

**Client response:**
```
HTTP/1.1 400 Bad Request
X-Request-Id: 3159e0e1d5c82f39a3d31a6fcc6ae18a
{"error":{"code":"invalid_path","message":"the supplied path was rejected"}}
```

**Server log (same request id):**
```
[audit] id=9fbf637b8170b5b9ab0c64aa8a5bb121 code=invalid_path
       detail=invalid path "/Users/operator/.ssh/id_ed25519" outside root
              "/var/lib/lattice/data": symlink /tmp/evil -> /Users/operator/.ssh
```

A byte-level audit of the response confirmed **no** occurrence of `id_ed25519`, `.ssh`,
`operator`, `/var/lib`, `symlink`, or `InvalidPath`. A checksum failure carrying
`customer:PII:ssn = 123-45-6789` likewise returned only
`{"code":"corrupted","message":"the data failed an integrity check"}`.

---

## The rule

> The client receives a stable, opaque **CODE** plus **STATIC** text. The **DETAIL** goes to the
> server log, correlated by request id.

Two structural guarantees rather than conventions:

1. `messageFor` looks up a **fixed table**. There is no interpolation anywhere, so no
   request-derived value can reach the wire. An unknown code falls back to the generic
   internal message rather than to empty or to `err.Error()`.
2. `writeError` (used by the guards) now **delegates to `WriteCoded`**, so guard rejections and
   endpoint failures emit one identical envelope from one message table. The previous
   implementation allowed a caller to pass an arbitrary message string.

---

## Request IDs (subtask 9.4)

128 bits from `crypto/rand`, **not** a counter and **not** derived from the clock:

- a predictable id would let a caller guess another request's id and forge correlated log lines;
- an id that encodes request properties leaks those properties into the log.

**An inbound `X-Request-Id` is ignored, never trusted.** Echoing a client-supplied value would
let an attacker inject chosen content — including newlines — into log lines. Verified by
`TestRequestID_IgnoresInboundHeader`.

---

## Two anti-oracle decisions

**Not-found codes are merged.** `ErrKeyNotFound`, "no such file" and "permission denied" all
collapse into one `not_found` family. Distinguishing them is exactly what turns error handling
into a probe.

**Integrity failures are uniform.** WAL-poisoned, checksum mismatch, and Raft-corruption all
map to `corrupted`, revealing nothing about which node or segment is affected.

---

## Refactor: one envelope, not two

`errors.go` initially declared its own `errorResponse` while `respond.go` had `errorBody` — a
duplication that would let the two drift, with one of them eventually gaining an unsafe path.
The duplicate was removed and both now flow through `WriteCoded`. All 310 pre-existing tests
passed unchanged through the refactor.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 9.1 Stable opaque envelope | ✅ `{"error":{"code","message"}}`, byte-pinned |
| 9.2 Map via `errors.Is` against sentinels | ✅ 15 mappings + wrapping test |
| 9.3 No paths / keys / key material in bodies | ✅ 6-marker byte audit, live |
| 9.4 `X-Request-Id` on every response | ✅ 256-request uniqueness test |
| internal error leaks no path or raw Go error | ✅ |
| request id present and echoed in logs | ✅ proven live |

**SEC-9 acceptance criteria: met.**

---

## Remaining

- **SEC-10** supplies the real `ErrorSink` (audit logger) and the control-character sanitiser
  for log fields. The `ErrorSink` seam exists precisely so this package stays independent of
  logging policy.
- **SEC-11** adds static asset serving over `embed.FS`.
- **SEC-12** consolidates the whole attack suite.

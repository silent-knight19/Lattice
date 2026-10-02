# SEC-13 — Security Documentation Record

> Task: SEC-13 of [`ui-console-tasks.md`](./ui-console-tasks.md). **Status: COMPLETE.**
> Documentation only; no code was changed by this task.

---

## What was written

| File | Change |
|---|---|
| `SECURITY.md` | **Trust Boundary 5** added (§4 now lists five boundaries) and a new **§6 Lattice Console — Operator Checklist** |
| `docs/threat-model.md` | **Threats 16–20** covering drive-by CSRF, clickjacking, arbitrary file read, resource exhaustion, and error/log disclosure |
| `docs/known-limitations.md` | **Limitations 102–107** — the console accepted risks, plus one explicit gap |

Each follows the file's existing conventions: threats use the same
*Threat / Attack Surface / Impact / Likelihood / Mitigation / Automated Test / Residual Risk*
shape, and limitations use the same *Limitation / Why Accepted / Dimensional Impact* shape.

---

## The documentation records what was measured, not what was assumed

Every control is cited with the test that proves it, and every claim was verified to name a
real symbol:

| Claim | Evidence cited |
|---|---|
| Drive-by attack is real | `sec0-spike-findings.md` — handler executed **twice** while JS reported "Failed to fetch" |
| `CleanAndValidatePath` does not block traversal | measured; `TestSecuritySuite_SymlinkEscape` |
| No-CORS stops reads, not delivery | `sec0-spike-findings.md` §SEC-0.2 |
| Base-logger redaction is key-name only | measured leaks; `RedactValue` |
| 430 cases, `-race` clean | `go test ./internal/admin/ -count=1 -race` |

---

## An honesty correction made during the task

The first draft documented two things that **do not exist yet**:

- a `--admin-allow-remote` flag,
- a `/system` page reporting posture.

Writing documentation that implies shipped capability is the most common failure mode of a
security write-up, so both were corrected, and a new entry was added:

> **Limitation 107 — Console Admin Server Is Not Yet Wired Into the Daemon.**
> `internal/admin` is complete and tested, but no flag starts it; `--admin-address`,
> `--admin-allow-remote` and `--admin-authz-policy` do not exist. ADM-7 performs the wiring.

`BindLoopback`'s remote-bind opt-in is real code, but it is **not** exposed to an operator yet,
and the docs now say so.

---

## The accepted risks, stated plainly

Four entries exist specifically so nobody has to infer them:

**102 — Loopback is not authentication.** Any local user or process can reach the port. A unix
socket with filesystem permissions would narrow this and is recorded as future work.

**103 — The CSRF token is per-daemon-boot.** A restart invalidates it, so a tab open across a
restart fails every write until it re-fetches `/api/v1/session`. `boot_id` exists so clients can
detect this. The upside is that there is no persistence and therefore no revocation to get wrong.

**104 — No multi-user session isolation.** No login, no sessions, one shared view; authorization
is re-evaluated per request. Concurrent operators share one audit trail, separated only by
`principal_fp` and `request_id`.

**105 — Forensics endpoints depend on a three-part path gate.** A standing hazard for future
code, not a current defect.

**106 — Error bodies are intentionally uninformative.** Diagnosing a client failure requires the
response's `X-Request-Id` and access to the server log. That is the trade, not an oversight.

---

## Operator checklist (§6 of SECURITY.md)

Deployment: bind loopback; do not expose; prefer mTLS so roles come from verified certificates;
grant `admin` sparingly. Runtime: expect a loud startup warning; treat degraded-mode signals as
incidents. Reporting: console issues should state whether a browser is required, since
browser-class findings (drive-by, XSS, clickjacking) are the priority.

---

## Subtask acceptance

| Subtask | Status |
|---|---|
| 13.1 Console section in `SECURITY.md` | ✅ Trust Boundary 5 + §6 checklist |
| 13.2 Accepted risks in `known-limitations.md` | ✅ 102–107, incl. the not-yet-wired gap |
| 13.3 Operator checklist | ✅ §6.1–6.4 |

**SEC-13 acceptance criteria: met.**

---

## Part 0 status

SEC-0 … SEC-13 are **all complete**. SEC-6 was the last, and it closed with the
`--admin-address` flags, the two-opt-in remote-bind rule, the startup warning, and daemon
wiring (`cmd/lattice/admin.go`). See [`sec6-verification.md`](./sec6-verification.md).

**Limitation 107 is now RESOLVED**; 108 and 109 were added to record what the console still
does not do, so the gap is described accurately rather than implied absent.

# Security Policy

## 1. Overview

Lattice is designed from the ground up with a **security-first, zero-third-party-runtime-dependency** posture. Storage engine internals must remain resilient against local filesystem attacks (symlink redirection, TOCTOU races, descriptor hijacking) and memory-corruption risks.

This document describes the security policy, vulnerability disclosure process, and trust boundaries governing Lattice.

---

## 2. Supported Versions

Security updates and patches are provided for the following releases:

| Version | Supported | Notes |
|---|---|---|
| `v0.1.x` (current main) | :white_check_mark: | Production engine & distributed consensus (Phases 00–21 complete) |
| `< v0.1.0` | :x: | Experimental / pre-release phases |

---

## 3. Reporting a Vulnerability

We take the security of Lattice seriously. If you discover a vulnerability or security weakness in Lattice, please report it privately:

### 3.1 Reporting Channels
- **GitHub Private Vulnerability Reporting**: Use the "Report a vulnerability" button under the **Security** tab of the repository (`silent-knight19/Lattice`).
- **Direct Security Contact**: Email security findings to `security-reports@lattice.local` (or create a private advisory on GitHub).

> [!IMPORTANT]
> **Please do NOT report security vulnerabilities via public GitHub issues, discussions, or pull requests.**

### 3.2 Report Details
Please include in your report:
1. **Description**: Summary of the vulnerability or security weakness.
2. **Affected Component**: File(s), functions, or subsystems affected (e.g. `internal/sstable`, `internal/logger`, `cmd/lattice`).
3. **Reproducer / Proof-of-Concept**: Minimal code, commands, or test case to reproduce the issue.
4. **Impact Assessment**: What an attacker could achieve (e.g., local arbitrary overwrite, secret disclosure, DoS).
5. **Environment**: Operating system, architecture, Go version.

### 3.3 Response Timeline
- **Acknowledgement**: Within 24 hours of receiving the report.
- **Initial Triage & Assessment**: Within 72 hours.
- **Remediation & Patch**: Target fix within 14 days for Critical/High severity issues.
- **Coordinated Disclosure**: 90-day standard window from report to public disclosure, or upon patch release.

---

## 4. Trust Boundaries & Scope

As defined in [`docs/threat-model.md`](docs/threat-model.md), Lattice enforces five distinct trust boundaries:

1. **Trust Boundary 1: Client / Transport Layer (`internal/transport`)**
   - Defensive TCP binary framing with magic validation (`0x4C415454`), CRC32-IEEE checksum verification, and hard payload length bounds (`MaxPayloadLength = 5 MiB`) enforced before memory allocation.
   - Connection concurrency ceilings (`MaxConnections = 4096`), Slowloris timeouts (`HeaderTimeout`, `PayloadTimeout`, `IdleTimeout`), and listener backoff on transient errors.
2. **Trust Boundary 2: Storage & Host Filesystem (`internal/sstable`, `internal/wal`, `internal/version`, `internal/engine`)**
   - Strict path confinement (`ValidateContainment`), path canonicalization, and symlink escape defenses.
   - Strict POSIX file permissions (`0600`/`0700`), parent directory file descriptor pinning (`renameAt`, `openat`), and atomic publication via hard link (`os.Link`) without destructive `os.Rename` fallbacks.
   - CRC32 verification and atomic file-replacement across WAL segments, SSTable blocks, and MANIFEST / CURRENT state transitions.
3. **Trust Boundary 3: Distributed Cluster & Peer Transport (`internal/raft`, `internal/cluster`, `internal/transport`)**
   - Node identity binding (`fromPeerID` matching authenticated peer address and request sender), self-vote rejection, and unknown node isolation.
   - Monotonic terms and epochs, nonce tracking with sliding-window replay rejection (`DefaultReplayWindowSize = 4096`), and opcode-level peer isolation (`0x81`..`0x84`).
   - Fail-closed cluster mode blocking direct engine write bypasses, and linearizable read verification (`ReadIndex` + `WaitForApplied` + quorum heartbeat confirmation).
4. **Trust Boundary 4: Internal Logging, Observability & Diagnostics (`internal/logger`, `internal/metrics`, `cmd/lattice`)**
   - Automated redaction of sensitive credentials, passwords, cryptographic keys, tokens, session IDs, and seeds across flat attributes, nested structures, maps, slices, structs, and formatted messages.
   - Bounded metric label cardinality (`O(1)` pre-allocated dimension matrices) preventing metric memory exhaustion.
   - Diagnostic pprof endpoints strictly bound to loopback addresses (`127.0.0.1` / `::1`) with pre- and post-bind address assertion.
5. **Trust Boundary 5: Lattice Console Admin API (`internal/admin`, `web`)**
   - Loopback-only by default, and **disabled entirely unless `--admin-address` is set**. Pre-**and** post-bind loopback verification, mirroring the pprof precedent; wildcard binds (`0.0.0.0` / `::`) are refused unconditionally. A non-loopback bind requires **two independent opt-ins** — `--insecure-transport` *and* `--admin-allow-remote` — so enabling insecure transport for the data path cannot silently publish engine internals.
   - Defended against the **drive-by localhost** attack — an unrelated web page causing the operator's browser to issue admin requests. Four independent layers: `OriginGuard` (exact scheme/host/port), `HostGuard` (DNS-rebinding defence), `CSRFGuard` (256-bit per-boot token, constant-time digest comparison), and a permanent no-`Access-Control-*` invariant.
   - Default-deny routing: a route cannot exist without declaring a permission, enforced by the router itself and by a CI completeness test. Reuses the existing `reader` / `writer` / `admin` RBAC vocabulary from `internal/transport` — no parallel identity system.
   - Response hardening: strict CSP without `unsafe-inline`/`unsafe-eval`, `frame-ancestors 'none'`, `nosniff`, `no-store`; opaque error envelopes with per-request `X-Request-Id`; bounded request bodies, per-handler deadlines, and SSE-specific connection budgets.
   - Static assets are served from an `embed.FS` and validated against `fs.ValidPath`, so **no user-supplied path is ever opened**. No filesystem path from request input is concatenated anywhere.
   - Design and verification records: [`docs/user interface/ui-console-design.md`](docs/user%20interface/ui-console-design.md) and [`docs/user interface/sec0-spike-findings.md`](docs/user%20interface/sec0-spike-findings.md).

---

## 5. Security Architecture & Audit History

Lattice maintains comprehensive security audit artifacts and verification tests:
- Threat Model: [`docs/threat-model.md`](docs/threat-model.md)
- Whole-Codebase Security Audit Report: [`docs/security/security-audit-report.md`](docs/security/security-audit-report.md)
- Phase 00 Security Seal: [`docs/security/security-audit-phase-00.md`](docs/security/security-audit-phase-00.md)
- Phase 00–05 Combined Audit: [`docs/security-audit-phase-00-05.md`](docs/security-audit-phase-00-05.md)
- Phase 06 Security Seal: [`docs/security/security-audit-phase-06.md`](docs/security/security-audit-phase-06.md)
- Phase 07 Security Seal: [`docs/security/security-audit-phase-07.md`](docs/security/security-audit-phase-07.md)
- Phase 08–09 Security Seal: [`docs/security/security-audit-phase-08-09.md`](docs/security/security-audit-phase-08-09.md)
- Phase 10–12 Security Seal: [`docs/security/security-audit-phase-10-12.md`](docs/security/security-audit-phase-10-12.md)
- SSTable Publication Hardening: [`internal/sstable/sec06_remediation_test.go`](internal/sstable/sec06_remediation_test.go)
- Automated AST Security Linter: [`internal/security/`](internal/security/)
- Console Security Test Suite (430 cases, `-race` clean): [`internal/admin/security_test.go`](internal/admin/security_test.go)

---

## 6. Lattice Console — Operator Checklist

The console exposes engine internals and, in the Lab, the ability to terminate the process.
It is a **privileged** surface and should be treated as such.

### 6.1 Deployment

1. **Bind loopback only.** Leave `--admin-address` unset unless you need the console; the console does not exist without it. When set, prefer `127.0.0.1:7070`. Never bind it to a routable interface: a non-loopback bind needs both `--insecure-transport` and `--admin-allow-remote`, and even then a wildcard (`0.0.0.0` / `::`) is refused.
2. **Do not expose it.** There is no supported reverse-proxy or internet-facing deployment. If you must reach it remotely, use an SSH tunnel to loopback rather than a network bind.
3. **Prefer mTLS.** When the daemon is configured with client certificates, the console derives the caller's role from the verified certificate fingerprint. Configure it with `--admin-authz-policy` / `--admin-authz-policy-file`, which accept exactly the same `fp=role` syntax as the data path's `--client-authz-policy` — there is no separate console identity system to learn. **Without a policy the console authorizes nothing** and returns `403` for every API request; that is fail-closed by design, not a misconfiguration (limitation 109).
4. **Grant `admin` sparingly.** `admin` unlocks Raft leadership campaigns, orphan-file cleanup, diagnostics bundles, and the crash Lab. Prefer `reader` for dashboards and `writer` for routine key operations.

### 6.2 Runtime

5. **Expect a loud startup warning.** The daemon logs one line when the admin server is enabled, precisely because it exposes engine internals.
6. **Watch for degraded mode.** The Overview page surfaces WAL poisoning, failed compactions, orphaned files, and lost quorum. Treat those as incidents.
7. **Read the startup output.** The daemon logs one line when the admin server is enabled. The `/system` page (Phase H) will additionally flag `--insecure-transport` and report whether a real frontend build is embedded; until then, check the startup flags directly.

### 6.3 Known accepted risks

See [`docs/known-limitations.md`](docs/known-limitations.md) for the full register. The
console-relevant entries are:

- **Loopback is not authentication.** Any local user or process can reach the port. This is an accepted boundary of the design, not an oversight.
- **The CSRF token is per-daemon-boot.** A restart invalidates it; a browser tab holding a stale token will fail every write until it re-fetches `/api/v1/session`.
- **There is no multi-user session isolation in the console.** Every caller sees the same view; authorization is enforced per request, not per session.
- **A local attacker can read the daemon's process memory**, including the CSRF token, and therefore acts with whatever role that token plus their own identity permits. Loopback is not a sandbox.

### 6.4 Reporting a console vulnerability

Console issues follow the same process as §3. Please state whether the finding requires a
browser (drive-by, XSS, clickjacking) or only direct HTTP access; the reproduction differs and
the browser-class issues are the priority.

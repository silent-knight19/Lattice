# Security Policy

## 1. Overview

Lattice is designed from the ground up with a **security-first, zero-third-party-runtime-dependency** posture. Storage engine internals must remain resilient against local filesystem attacks (symlink redirection, TOCTOU races, descriptor hijacking) and memory-corruption risks.

This document describes the security policy, vulnerability disclosure process, and trust boundaries governing Lattice.

---

## 2. Supported Versions

Security updates and patches are provided for the following releases:

| Version | Supported | Notes |
|---|---|---|
| `v0.1.x` (current main) | :white_check_mark: | Active development (Phases 00–12 completed) |
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

As defined in [`docs/threat-model.md`](docs/threat-model.md), Lattice enforces three distinct trust boundaries:

1. **Trust Boundary 1: Client / Transport Layer (`internal/transport`)**
   - Defensive TCP binary framing with hard byte limits to prevent frame-bomb DoS and unbounded allocations.
2. **Trust Boundary 2: Storage & Host Filesystem (`internal/sstable`, `internal/wal`, `internal/version`)**
   - Path traversal prevention, strict file permissions (`0600`/`0700`), parent directory descriptor pinning, symlink validation (`validatePathNoSymlinks`), atomic publication via hard link (`os.Link`) without destructive `os.Rename` fallbacks.
3. **Trust Boundary 3: Internal Logging & Diagnostics (`internal/logger`)**
   - Automated redaction of sensitive credentials, passwords, cryptographic keys, tokens, session IDs, and seeds across flat attributes, nested structures, maps, slices, structs, and formatted messages.

---

## 5. Security Architecture & Audit History

Lattice maintains comprehensive security audit artifacts and verification tests:
- Threat Model: [`docs/threat-model.md`](docs/threat-model.md)
- Phase 00 Security Seal: [`docs/security/security-audit-phase-00.md`](docs/security/security-audit-phase-00.md)
- Phase 00–05 Combined Audit: [`docs/security-audit-phase-00-05.md`](docs/security-audit-phase-00-05.md)
- SSTable Publication Hardening: [`internal/sstable/sec06_remediation_test.go`](internal/sstable/sec06_remediation_test.go)
- Automated AST Security Linter: [`internal/security/`](internal/security/)

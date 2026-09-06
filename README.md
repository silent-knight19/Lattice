# Lattice

> **A High-Performance Distributed Key-Value Storage Engine Being Built from the Ground Up**

[![Status: Engineering Foundations](https://img.shields.io/badge/Status-Engineering%20Foundations-blue.svg)](#)
[![Design Spec](https://img.shields.io/badge/Docs-Architecture%20Spec-blue.svg)](docs/architecture-spec.md)
[![Implementation Plan](https://img.shields.io/badge/Docs-Implementation%20Plan-green.svg)](docs/implementation-plan.md)
[![Interview Knowledge](https://img.shields.io/badge/Docs-Interview%20Defense-purple.svg)](docs/interview-knowledge.md)
[![Known Limitations](https://img.shields.io/badge/Docs-Known%20Limitations-orange.svg)](docs/known-limitations.md)
[![Threat Model](https://img.shields.io/badge/Docs-Threat%20Model-red.svg)](docs/threat-model.md)
[![Language](https://img.shields.io/badge/Language-Go%201.22+-00ADD8.svg)](https://go.dev)
[![Architecture](https://img.shields.io/badge/Storage-LSM--Tree%20%2B%20Raft-orange.svg)](#)

---

## Overview

**Lattice** is a ground-up distributed key-value storage engine being developed in Go. It does not wrap SQLite, RocksDB, LevelDB, Redis, or etcd. Every subsystem—from append-only write-ahead logging (WAL) and custom binary wire protocols to probabilistic Bloom filtering, k-way leveled merge compaction, and Raft distributed consensus—is designed and implemented from core systems primitives.

The comprehensive system architecture and 184-step micro-phase execution blueprint are complete. Implementation is proceeding incrementally following strict engineering discipline: test-driven micro-steps, race detection (`-race`), continuous security threat reviews, empirical benchmark validation, and interview-oriented documentation tracking.

---

## Architecture Design Targets

The system design specifies the following architectural targets (currently undergoing incremental implementation):

* **Storage Engine**: Log-Structured Merge-Tree (LSM-Tree) with write-optimized sequential disk I/O.
* **Durability & WAL Target**: Pre-allocated append-only binary logs with CRC32-IEEE checksumming and cooperative **Group Commit** batching (`fdatasync()` barriers).
* **In-Memory MemTable**: Probabilistic concurrent **SkipList** with lock-free reads and monotonic 64-bit sequence numbers.
* **Persistent SSTable Format**: Immutable binary files with 4KB Data Blocks, prefix compression, 16-record restart points, Two-Level Block Indexes, and 48-byte fixed Footers.
* **Probabilistic Filtering**: Murmur3-based **Bloom filters** (10 bits/key, 7 hash functions) targeting $\ge 99\%$ cold read disk avoidance.
* **Compaction Strategy**: Leveled compaction ($L_0$ overlapping, $L_1..L_N$ non-overlapping partitioned runs with a $10\times$ multiplier) driven by k-way merge iterators.
* **Concurrency Model**: Version-pinned snapshot reads (`VersionSet` with atomic reference counting) ensuring reads never block on background flushes or compactions.
* **Wire Protocol**: Custom length-prefixed binary framing protocol over raw TCP sockets with frame-bomb protections.
* **Distributed Consensus (V1.1 Target)**: Single-group **Raft** consensus supporting leader election, randomized heartbeat timers, quorum replication ($Q = \lfloor N/2 \rfloor + 1$), and linearizable reads via the `ReadIndex` protocol.

---

## System Topology Blueprint

```
+-----------------------------------------------------------------------------------+
|                                  LATTICE SYSTEM                                   |
|                                                                                   |
|  +--------------------+        +--------------------+        +-----------------+  |
|  |    TCP Clients     |  --->  |    Wire Protocol   |  --->  | Engine Dispatch |  |
|  |  (CLI / Bench / SDK|        |  (Binary Framing)  |        | (Group Commit)  |  |
|  +--------------------+        +--------------------+        +--------+--------+  |
|                                                                       |           |
|         +-------------------------------------------------------------+           |
|         |                                                                         |
|         v                                                             v           |
|  +--------------+   Buffered Appends   +--------------+         +--------------+  |
|  |  Write-Ahead | ===================> | Page Cache / |         | Active       |  |
|  |  Log (WAL)   |   `fdatasync()`      | Disk Drive   |         | MemTable     |  |
|  +--------------+                      +--------------+         | (SkipList)   |  |
|                                                                 +-------+------+  |
|                                                                         | Swap    |
|                                                                         v         |
|  +--------------------+   Flushes via Merge   +--------------------------------+  |
|  |   Leveled SSTables | <==================== | Immutable MemTable Pipeline    |  |
|  | (L0..LN, Bloom, Idx|                       +--------------------------------+  |
|  +---------+----------+                                                           |
|            |                                                                      |
|            +============== k-way Merging Compactor (Background)                   |
+-----------------------------------------------------------------------------------+
```

---

## Roadmap & Verified Implementation Status

*In adherence to our Project Truth Rule, completion checkmarks (`[x]`) are awarded only after code is written, tested under `-race`, verified against invariants, and reviewed.*

* [x] **Architecture Specification** — Completed in [`docs/architecture-spec.md`](docs/architecture-spec.md)
* [x] **Micro-Phase Implementation Planning (184 Steps)** — Completed in [`docs/implementation-plan.md`](docs/implementation-plan.md)
* [x] **Interview Knowledge Base & Defense Guide** — Completed in [`docs/interview-knowledge.md`](docs/interview-knowledge.md)
* [x] **Architecture Decision Records (ADR 001–008)** — Completed in [`docs/decisions/`](docs/decisions/)
* [x] **Living Known Limitations Document** — Completed in [`docs/known-limitations.md`](docs/known-limitations.md)
* [x] **System Security Threat Model** — Completed in [`docs/threat-model.md`](docs/threat-model.md)
* [ ] **Phase 00: Repository & Engineering Foundations** (Ready for `P00-S01-M01`)
* [ ] **Phase 01: Core Storage Primitives & Binary Encodings**
* [ ] **Phase 02: Write-Ahead Log (WAL) & Durability Subsystem**
* [ ] **Phase 03: In-Memory MemTable & Concurrent SkipList**
* [ ] **Phase 04: Persistent SSTable Subsystem**
* [ ] **Phase 05: Probabilistic Bloom Filter Subsystem**
* [ ] **Phase 06: Manifest Log & VersionSet Management**
* [ ] **Phase 07: Crash Recovery & Integrity Verification**
* [ ] **Phase 08: Leveled Compaction Subsystem**
* [ ] **Phase 09: Sharded LRU Read Block Cache**
* [ ] **Phase 10: Single-Node Storage Engine Integration**
* [ ] **Phase 11: TCP Binary Wire Protocol & Networking**
* [ ] **Phase 12: CLI, Interactive REPL & Forensic Diagnostics**
* [ ] **Phase 13: Benchmarking Suite & Performance Profiling**
* [ ] **Phase 14: Distributed Cluster Foundations & Node Topology**
* [ ] **Phase 15: Raft Consensus Engine (V1.1)**
* [ ] **Phase 16: Distributed State Machine Replication**
* [ ] **Phase 17: Linearizable Reads (ReadIndex Protocol)**
* [ ] **Phase 18: Fault Injection & Chaos Testing Suite**
* [ ] **Phase 19: Comprehensive Security Hardening**
* [ ] **Phase 20: Production Hardening & Operational Observability**
* [ ] **Phase 21: Resume & Technical Interview Portfolio Validation**

---

## Core Engineering Documentation

* [Full Architecture & System Design Specification (48 Sections & Diagrams)](docs/architecture-spec.md)
* [Micro-Phase Implementation Plan (184 Verifiable Micro-Steps)](docs/implementation-plan.md)
* [Technical Interview Knowledge Base & Defense Guide](docs/interview-knowledge.md)
* [Architecture Decision Records (ADRs)](docs/decisions/)
* [Known Limitations & Architectural Boundaries](docs/known-limitations.md)
* [System Security Threat Model](docs/threat-model.md)

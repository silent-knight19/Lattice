# ADR-006: Custom Length-Prefixed Binary Wire Protocol over Raw TCP

* **Status**: Accepted
* **Date**: 2026-09-06
* **Deciders**: Architecture & Networking Core Team
* **Technical Invariants Affected**: Network Framing, Serialization Overhead, Socket Lifecycle

---

## 1. Context
Lattice requires a client-facing communication interface over the network to process high-throughput commands (`PUT`, `GET`, `DELETE`, `EXISTS`, `BATCH`).

## 2. Problem
Using high-level application protocols like HTTP/1.1 REST with JSON creates significant serialization overhead (string formatting, JSON reflection) and excessive header bloat. Standard RPC frameworks like gRPC abstract away the underlying socket mechanics, buffer management, and framing invariants that this project aims to demonstrate.

## 3. Decision
We implement a **Custom Length-Prefixed Binary Protocol over Raw TCP**:
* **Fixed 18-Byte Header**: `Magic (4B: 0x4C415454) + OpCode (1B) + Flags (1B) + SeqID (8B) + PayloadLen (4B)`.
* **Payload**: Raw binary key/value bytes.
* **Trailer**: 4-Byte `CRC32-IEEE` checksum over header and payload.

## 4. Alternatives Considered
1. **HTTP/1.1 REST with JSON**: Easy to query with `curl`, but high CPU parsing overhead and no pipelining.
2. **gRPC / Protocol Buffers**: Industrial standard, but hides socket programming, byte buffers, and network error handling behind code generators.
3. **RESP (Redis Serialization Protocol)**: Human-readable and compatible with `redis-cli`, but text/binary hybrid parsing is slower than pure fixed binary framing.

## 5. Reasoning
* **Maximum Byte Efficiency**: The 18-byte header is lean and aligns to 64-bit boundaries.
* **Educational & Interview Rigor**: Writing a custom TCP framer demonstrates socket programming, endianness, non-blocking I/O, and frame-bomb protection without third-party abstraction.
* **Zero-Allocation Buffer Reuse**: Inbound frames can be parsed directly into pooled byte buffers (`sync.Pool`), minimizing heap allocations.

## 6. Trade-offs
* **Tooling Ecosystem**: Requires custom client libraries (`pkg/client`) and a dedicated CLI client (`cmd/lattice-cli`) rather than standard HTTP tools like `curl`.

## 7. Consequences & Mitigations
* **Frame-Bomb Mitigation**: Header parser strictly validates that `PayloadLength \le 5MB`. Any frame claiming larger payloads is rejected immediately and the socket is severed.

## 8. Evidence Classification
* **Theoretical Property**: Fixed $O(1)$ header decoding time with zero reflection.
* **Design Target**: $\ge 150,000 \text{ frame decodes/sec}$ per CPU core.

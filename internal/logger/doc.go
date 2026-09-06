// Package logger provides a concurrency-safe, structured logging abstraction for Lattice,
// built on top of the Go standard library log/slog package.
//
// # Architectural Principles
//
// 1. Zero External Dependencies: Relies entirely on Go's standard library (log/slog, io, sync),
// eliminating supply-chain attack vectors and maintaining hermetic builds.
//
// 2. Structured & Machine-Readable: Supports both JSON format (production default, suitable for
// automated ingestion and log indexing) and Text format (human-readable development and CLI).
//
// 3. Subsystem Scoping: Provides WithComponent(name) to tag log events with their originating
// subsystem (e.g. "wal", "memtable", "sstable", "raft") for clean log filtering.
//
// 4. Privacy by Design & Redaction: Automatically masks sensitive fields (such as passwords,
// bearer tokens, API keys, and secret credentials) using configurable redaction hooks, preventing
// accidental credential leakage into persistent log streams.
//
// 5. Concurrency Safety: All logger instances and handlers are fully safe for concurrent use
// across arbitrary numbers of goroutines.
//
// 6. Testability: Provides NewNop() for zero-overhead silent testing, and accepts arbitrary
// io.Writer destinations for deterministic assertion testing in unit suites.
package logger

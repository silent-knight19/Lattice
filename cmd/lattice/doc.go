// Package main provides the primary daemon entrypoint for the Lattice distributed
// key-value database server, as well as forensic inspection tooling for SSTables
// and write-ahead log (WAL) segments.
//
// In addition to its core binary TCP storage protocol, the daemon optionally
// provides an isolated HTTP diagnostics server exposing standard Go runtime/pprof
// profiling endpoints (CPU, heap, goroutines, allocs, block, mutex, trace) via
// the --pprof-address flag. The pprof server enforces a strict loopback-only
// binding policy (127.0.0.1, ::1, localhost) to prevent unauthorized remote
// inspection of runtime internals.
//
// Beginning in Phase 14, the daemon supports optional distributed cluster topology
// configuration via --node-id, --peer-address, and --cluster-peers (or equivalent
// JSON/key-value configuration fields). When unconfigured, the daemon operates
// strictly in single-node V1 mode. While Phase 14 establishes node identity, static
// peer topology models, and peer RPC framing foundations, full distributed consensus
// (Raft leader election, heartbeat scheduling, and log replication) is deferred to
// Phase 15.
package main

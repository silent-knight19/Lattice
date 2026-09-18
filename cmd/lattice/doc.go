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
package main

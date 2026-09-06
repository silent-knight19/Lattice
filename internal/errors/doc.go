// Package errors defines structured domain errors, sentinels, and diagnostic
// error types for the Lattice distributed storage engine.
//
// # Architectural Principles
//
// 1. Leaf Package: internal/errors has zero dependencies on other internal packages,
// preventing circular import cycles across storage and distributed subsystems.
//
// 2. Sentinel Errors: Stable package-level error values (e.g., ErrKeyNotFound,
// ErrChecksumMismatch) represent expected or unrecoverable domain conditions.
// Callers must inspect sentinels using errors.Is() rather than direct equality (==)
// to support %w wrapping.
//
// 3. Typed Errors: Where diagnostic context (e.g., file offsets, expected vs actual
// checksums, byte length limits) is necessary, structured error types implement
// error and provide an Is(error) bool method matching the corresponding sentinel.
// This allows callers to either inspect general conditions via errors.Is() or extract
// detailed diagnostics via errors.As().
//
// 4. Defense-in-Depth & Privacy: Domain errors deliberately avoid formatting raw key
// or value payloads into error messages, preventing unintentional leakage of user data
// or credentials into application logs.
package errors

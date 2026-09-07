package errors_test

import (
	stdErrors "errors"
	"fmt"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

func TestSentinelIdentity(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
		msg  string
	}{
		{"ErrKeyNotFound", errors.ErrKeyNotFound, "key not found"},
		{"ErrEmptyKey", errors.ErrEmptyKey, "key cannot be empty"},
		{"ErrKeyTooLarge", errors.ErrKeyTooLarge, "key exceeds maximum allowed size"},
		{"ErrValueTooLarge", errors.ErrValueTooLarge, "value exceeds maximum allowed size"},
		{"ErrChecksumMismatch", errors.ErrChecksumMismatch, "checksum mismatch: data corrupted"},
		{"ErrTornWrite", errors.ErrTornWrite, "torn write detected at tail of log"},
		{"ErrCompactionRunning", errors.ErrCompactionRunning, "compaction already in progress"},
	}

	for _, tc := range sentinels {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err == nil {
				t.Fatalf("expected non-nil sentinel error for %s", tc.name)
			}
			if tc.err.Error() != tc.msg {
				t.Errorf("expected error message %q, got %q", tc.msg, tc.err.Error())
			}
			// Direct comparison
			if !stdErrors.Is(tc.err, tc.err) {
				t.Errorf("sentinel %s must match itself via errors.Is()", tc.name)
			}
		})
	}
}

func TestSentinelWrappingWithErrorsIs(t *testing.T) {
	tests := []struct {
		name     string
		sentinel error
	}{
		{"ErrKeyNotFound", errors.ErrKeyNotFound},
		{"ErrEmptyKey", errors.ErrEmptyKey},
		{"ErrKeyTooLarge", errors.ErrKeyTooLarge},
		{"ErrValueTooLarge", errors.ErrValueTooLarge},
		{"ErrChecksumMismatch", errors.ErrChecksumMismatch},
		{"ErrTornWrite", errors.ErrTornWrite},
		{"ErrCompactionRunning", errors.ErrCompactionRunning},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Single wrap
			wrappedOnce := fmt.Errorf("engine layer: %w", tc.sentinel)
			if !stdErrors.Is(wrappedOnce, tc.sentinel) {
				t.Errorf("errors.Is failed for singly-wrapped sentinel %s", tc.name)
			}

			// Multi-level wrap
			wrappedTwice := fmt.Errorf("server: %w", fmt.Errorf("storage: %w", tc.sentinel))
			if !stdErrors.Is(wrappedTwice, tc.sentinel) {
				t.Errorf("errors.Is failed for multi-wrapped sentinel %s", tc.name)
			}
		})
	}
}

func TestSentinelNegativeComparisons(t *testing.T) {
	allSentinels := []error{
		errors.ErrKeyNotFound,
		errors.ErrEmptyKey,
		errors.ErrKeyTooLarge,
		errors.ErrValueTooLarge,
		errors.ErrChecksumMismatch,
		errors.ErrTornWrite,
		errors.ErrCompactionRunning,
	}

	for i, a := range allSentinels {
		for j, b := range allSentinels {
			if i != j {
				if stdErrors.Is(a, b) {
					t.Errorf("distinct sentinels %v and %v must not match via errors.Is", a, b)
				}
				wrapped := fmt.Errorf("context: %w", a)
				if stdErrors.Is(wrapped, b) {
					t.Errorf("wrapped sentinel %v must not match unrelated sentinel %v", a, b)
				}
			}
		}

		// Negative check against generic error
		unrelated := stdErrors.New("unrelated error")
		if stdErrors.Is(a, unrelated) {
			t.Errorf("sentinel %v must not match arbitrary unrelated error", a)
		}
	}
}

func TestKeyTooLargeError(t *testing.T) {
	typedErr := &errors.KeyTooLargeError{
		KeySize: 70000,
		MaxSize: 65535,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrKeyTooLarge) {
		t.Errorf("KeyTooLargeError must match ErrKeyTooLarge via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrValueTooLarge) {
		t.Errorf("KeyTooLargeError must not match ErrValueTooLarge")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("memtable put: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrKeyTooLarge) {
		t.Errorf("wrapped KeyTooLargeError must match ErrKeyTooLarge via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.KeyTooLargeError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *KeyTooLargeError from wrapped chain")
	}
	if extracted.KeySize != 70000 || extracted.MaxSize != 65535 {
		t.Errorf("extracted fields mismatch: got KeySize=%d, MaxSize=%d", extracted.KeySize, extracted.MaxSize)
	}

	// Error string validation
	msg := typedErr.Error()
	if !strings.Contains(msg, "70000") || !strings.Contains(msg, "65535") {
		t.Errorf("unexpected error string formatting: %q", msg)
	}
}

func TestValueTooLargeError(t *testing.T) {
	typedErr := &errors.ValueTooLargeError{
		ValueSize: 5000000,
		MaxSize:   4194304,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrValueTooLarge) {
		t.Errorf("ValueTooLargeError must match ErrValueTooLarge via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrKeyTooLarge) {
		t.Errorf("ValueTooLargeError must not match ErrKeyTooLarge")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("wal append: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrValueTooLarge) {
		t.Errorf("wrapped ValueTooLargeError must match ErrValueTooLarge via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.ValueTooLargeError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *ValueTooLargeError from wrapped chain")
	}
	if extracted.ValueSize != 5000000 || extracted.MaxSize != 4194304 {
		t.Errorf("extracted fields mismatch: got ValueSize=%d, MaxSize=%d", extracted.ValueSize, extracted.MaxSize)
	}

	// Error string validation
	msg := typedErr.Error()
	if !strings.Contains(msg, "5000000") || !strings.Contains(msg, "4194304") {
		t.Errorf("unexpected error string formatting: %q", msg)
	}
}

func TestChecksumMismatchError(t *testing.T) {
	typedErr := &errors.ChecksumMismatchError{
		Offset:   4096,
		Expected: 0x12345678,
		Actual:   0xdeadbeef,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrChecksumMismatch) {
		t.Errorf("ChecksumMismatchError must match ErrChecksumMismatch via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrTornWrite) {
		t.Errorf("ChecksumMismatchError must not match ErrTornWrite")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("sstable read block: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrChecksumMismatch) {
		t.Errorf("wrapped ChecksumMismatchError must match ErrChecksumMismatch via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.ChecksumMismatchError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *ChecksumMismatchError from wrapped chain")
	}
	if extracted.Offset != 4096 || extracted.Expected != 0x12345678 || extracted.Actual != 0xdeadbeef {
		t.Errorf("extracted fields mismatch: offset=%d, expected=0x%x, actual=0x%x",
			extracted.Offset, extracted.Expected, extracted.Actual)
	}

	// Error string formatting
	msg := typedErr.Error()
	if !strings.Contains(msg, "4096") || !strings.Contains(msg, "0x12345678") || !strings.Contains(msg, "0xdeadbeef") {
		t.Errorf("unexpected error string formatting: %q", msg)
	}
}

func TestTornWriteError(t *testing.T) {
	// Case 1: With reason
	errWithReason := &errors.TornWriteError{
		Offset: 8192,
		Reason: "partial header encountered before EOF",
	}

	if !stdErrors.Is(errWithReason, errors.ErrTornWrite) {
		t.Errorf("TornWriteError must match ErrTornWrite via errors.Is")
	}
	if stdErrors.Is(errWithReason, errors.ErrChecksumMismatch) {
		t.Errorf("TornWriteError must not match ErrChecksumMismatch")
	}

	wrapped := fmt.Errorf("recovery replay: %w", errWithReason)
	if !stdErrors.Is(wrapped, errors.ErrTornWrite) {
		t.Errorf("wrapped TornWriteError must match ErrTornWrite via errors.Is")
	}

	var extracted *errors.TornWriteError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *TornWriteError")
	}
	if extracted.Offset != 8192 || extracted.Reason != "partial header encountered before EOF" {
		t.Errorf("extracted fields mismatch: %+v", extracted)
	}

	msgWithReason := errWithReason.Error()
	if !strings.Contains(msgWithReason, "8192") || !strings.Contains(msgWithReason, "partial header") {
		t.Errorf("unexpected message: %q", msgWithReason)
	}

	// Case 2: Without reason
	errWithoutReason := &errors.TornWriteError{
		Offset: 16384,
	}
	msgWithout := errWithoutReason.Error()
	if !strings.Contains(msgWithout, "16384") || strings.Contains(msgWithout, ":") {
		t.Errorf("unexpected message without reason: %q", msgWithout)
	}
}

func TestErrorsAsNegativeExtraction(t *testing.T) {
	sentinel := errors.ErrKeyNotFound
	wrapped := fmt.Errorf("lookup: %w", sentinel)

	var keyErr *errors.KeyTooLargeError
	if stdErrors.As(wrapped, &keyErr) {
		t.Errorf("errors.As should return false when target type does not exist in chain")
	}
	if keyErr != nil {
		t.Errorf("target pointer should remain nil when extraction fails")
	}
}

func TestNilReceiverTypedErrors(t *testing.T) {
	var k *errors.KeyTooLargeError
	var v *errors.ValueTooLargeError
	var c *errors.ChecksumMismatchError
	var tw *errors.TornWriteError

	// Ensure calling Error() on nil typed pointers does not panic and returns sentinel strings
	if k.Error() != errors.ErrKeyTooLarge.Error() {
		t.Errorf("expected %q, got %q", errors.ErrKeyTooLarge.Error(), k.Error())
	}
	if v.Error() != errors.ErrValueTooLarge.Error() {
		t.Errorf("expected %q, got %q", errors.ErrValueTooLarge.Error(), v.Error())
	}
	if c.Error() != errors.ErrChecksumMismatch.Error() {
		t.Errorf("expected %q, got %q", errors.ErrChecksumMismatch.Error(), c.Error())
	}
	if tw.Error() != errors.ErrTornWrite.Error() {
		t.Errorf("expected %q, got %q", errors.ErrTornWrite.Error(), tw.Error())
	}

	// Ensure calling Is() on nil typed pointers matches corresponding sentinels
	if !k.Is(errors.ErrKeyTooLarge) {
		t.Errorf("nil *KeyTooLargeError must match ErrKeyTooLarge via Is()")
	}
	if !v.Is(errors.ErrValueTooLarge) {
		t.Errorf("nil *ValueTooLargeError must match ErrValueTooLarge via Is()")
	}
	if !c.Is(errors.ErrChecksumMismatch) {
		t.Errorf("nil *ChecksumMismatchError must match ErrChecksumMismatch via Is()")
	}
	if !tw.Is(errors.ErrTornWrite) {
		t.Errorf("nil *TornWriteError must match ErrTornWrite via Is()")
	}
}

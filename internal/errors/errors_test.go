package errors_test

import (
	stdErrors "errors"
	"fmt"
	"io/fs"
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
		{"ErrVarintOverflow", errors.ErrVarintOverflow, "varint exceeds maximum 64-bit integer size"},
		{"ErrVarintTruncated", errors.ErrVarintTruncated, "varint buffer truncated or incomplete"},
		{"ErrInvalidOpType", errors.ErrInvalidOpType, "invalid operation type"},
		{"ErrSeqNumOverflow", errors.ErrSeqNumOverflow, "sequence number overflow"},
		{"ErrInternalKeyTruncated", errors.ErrInternalKeyTruncated, "internal key buffer truncated: missing trailer or user key"},
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
		{"ErrVarintOverflow", errors.ErrVarintOverflow},
		{"ErrVarintTruncated", errors.ErrVarintTruncated},
		{"ErrInvalidOpType", errors.ErrInvalidOpType},
		{"ErrSeqNumOverflow", errors.ErrSeqNumOverflow},
		{"ErrInternalKeyTruncated", errors.ErrInternalKeyTruncated},
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
		errors.ErrVarintOverflow,
		errors.ErrVarintTruncated,
		errors.ErrInvalidOpType,
		errors.ErrSeqNumOverflow,
		errors.ErrInternalKeyTruncated,
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
	var op *errors.InvalidOpTypeError
	var seq *errors.SeqNumOverflowError
	var recType *errors.InvalidRecordTypeError
	var recPayload *errors.InvalidRecordPayloadError

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
	if op.Error() != errors.ErrInvalidOpType.Error() {
		t.Errorf("expected %q, got %q", errors.ErrInvalidOpType.Error(), op.Error())
	}
	if seq.Error() != errors.ErrSeqNumOverflow.Error() {
		t.Errorf("expected %q, got %q", errors.ErrSeqNumOverflow.Error(), seq.Error())
	}
	if recType.Error() != errors.ErrInvalidRecordType.Error() {
		t.Errorf("expected %q, got %q", errors.ErrInvalidRecordType.Error(), recType.Error())
	}
	if recPayload.Error() != errors.ErrInvalidRecordPayload.Error() {
		t.Errorf("expected %q, got %q", errors.ErrInvalidRecordPayload.Error(), recPayload.Error())
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
	if !op.Is(errors.ErrInvalidOpType) {
		t.Errorf("nil *InvalidOpTypeError must match ErrInvalidOpType via Is()")
	}
	if !seq.Is(errors.ErrSeqNumOverflow) {
		t.Errorf("nil *SeqNumOverflowError must match ErrSeqNumOverflow via Is()")
	}
	if !recType.Is(errors.ErrInvalidRecordType) {
		t.Errorf("nil *InvalidRecordTypeError must match ErrInvalidRecordType via Is()")
	}
	if !recPayload.Is(errors.ErrInvalidRecordPayload) {
		t.Errorf("nil *InvalidRecordPayloadError must match ErrInvalidRecordPayload via Is()")
	}
}

func TestInterfaceWrappedTypedNilErrors(t *testing.T) {
	// Verify that typed nils stored in the error interface dispatch safely and match sentinels
	typedNils := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"KeyTooLargeError", (*errors.KeyTooLargeError)(nil), errors.ErrKeyTooLarge},
		{"ValueTooLargeError", (*errors.ValueTooLargeError)(nil), errors.ErrValueTooLarge},
		{"ChecksumMismatchError", (*errors.ChecksumMismatchError)(nil), errors.ErrChecksumMismatch},
		{"TornWriteError", (*errors.TornWriteError)(nil), errors.ErrTornWrite},
		{"InvalidOpTypeError", (*errors.InvalidOpTypeError)(nil), errors.ErrInvalidOpType},
		{"SeqNumOverflowError", (*errors.SeqNumOverflowError)(nil), errors.ErrSeqNumOverflow},
		{"InvalidRecordTypeError", (*errors.InvalidRecordTypeError)(nil), errors.ErrInvalidRecordType},
		{"InvalidRecordPayloadError", (*errors.InvalidRecordPayloadError)(nil), errors.ErrInvalidRecordPayload},
	}

	for _, tc := range typedNils {
		t.Run(tc.name, func(t *testing.T) {
			// Interface dispatch on Error() must not panic and must equal sentinel error string
			msg := tc.err.Error()
			if msg != tc.sentinel.Error() {
				t.Errorf("expected %q, got %q", tc.sentinel.Error(), msg)
			}

			// errors.Is via interface dispatch must match corresponding sentinel
			if !stdErrors.Is(tc.err, tc.sentinel) {
				t.Errorf("interface-wrapped typed nil %s must match sentinel %v via errors.Is()", tc.name, tc.sentinel)
			}

			// errors.Is must not match an unrelated sentinel
			if stdErrors.Is(tc.err, errors.ErrKeyNotFound) {
				t.Errorf("interface-wrapped typed nil %s must not match unrelated ErrKeyNotFound", tc.name)
			}
		})
	}
}

func TestInvalidOpTypeError(t *testing.T) {
	typedErr := &errors.InvalidOpTypeError{
		Op: 0xFF,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrInvalidOpType) {
		t.Errorf("InvalidOpTypeError must match ErrInvalidOpType via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrKeyNotFound) {
		t.Errorf("InvalidOpTypeError must not match ErrKeyNotFound")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("wal decode: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrInvalidOpType) {
		t.Errorf("wrapped InvalidOpTypeError must match ErrInvalidOpType via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.InvalidOpTypeError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *InvalidOpTypeError")
	}
	if extracted.Op != 0xFF {
		t.Errorf("extracted Op mismatch: got 0x%02x, expected 0xff", extracted.Op)
	}

	// Error string formatting
	msg := typedErr.Error()
	if !strings.Contains(msg, "0xff") {
		t.Errorf("unexpected error message: %q", msg)
	}
}

func TestSeqNumOverflowError(t *testing.T) {
	typedErr := &errors.SeqNumOverflowError{
		Current: 18446744073709551615,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrSeqNumOverflow) {
		t.Errorf("SeqNumOverflowError must match ErrSeqNumOverflow via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrVarintOverflow) {
		t.Errorf("SeqNumOverflowError must not match ErrVarintOverflow")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("sequence generator: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrSeqNumOverflow) {
		t.Errorf("wrapped SeqNumOverflowError must match ErrSeqNumOverflow via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.SeqNumOverflowError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *SeqNumOverflowError")
	}
	if extracted.Current != 18446744073709551615 {
		t.Errorf("extracted Current mismatch: got %d", extracted.Current)
	}

	// Error string formatting
	msg := typedErr.Error()
	if !strings.Contains(msg, "18446744073709551615") {
		t.Errorf("unexpected error message: %q", msg)
	}
}

func TestHeaderTruncatedSentinel(t *testing.T) {
	if errors.ErrHeaderTruncated == nil {
		t.Fatalf("ErrHeaderTruncated must not be nil")
	}

	wrapped := fmt.Errorf("wal decode header: %w", errors.ErrHeaderTruncated)
	if !stdErrors.Is(wrapped, errors.ErrHeaderTruncated) {
		t.Errorf("wrapped ErrHeaderTruncated must match via errors.Is")
	}

	if stdErrors.Is(errors.ErrHeaderTruncated, errors.ErrInternalKeyTruncated) {
		t.Errorf("ErrHeaderTruncated must not match ErrInternalKeyTruncated")
	}

	if !strings.Contains(errors.ErrHeaderTruncated.Error(), "header truncated") {
		t.Errorf("unexpected error message: %q", errors.ErrHeaderTruncated.Error())
	}
}

func TestInvalidRecordTypeError(t *testing.T) {
	typedErr := &errors.InvalidRecordTypeError{
		Type: 0x99,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrInvalidRecordType) {
		t.Errorf("InvalidRecordTypeError must match ErrInvalidRecordType via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrInvalidOpType) {
		t.Errorf("InvalidRecordTypeError must not match ErrInvalidOpType")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("wal decode header: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrInvalidRecordType) {
		t.Errorf("wrapped InvalidRecordTypeError must match ErrInvalidRecordType via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.InvalidRecordTypeError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *InvalidRecordTypeError")
	}
	if extracted.Type != 0x99 {
		t.Errorf("extracted Type mismatch: got 0x%02x, expected 0x99", extracted.Type)
	}

	// Error string formatting
	msg := typedErr.Error()
	if !strings.Contains(msg, "0x99") {
		t.Errorf("unexpected error message: %q", msg)
	}
}

func TestInvalidRecordPayloadSentinel(t *testing.T) {
	if errors.ErrInvalidRecordPayload == nil {
		t.Fatalf("ErrInvalidRecordPayload must not be nil")
	}

	wrapped := fmt.Errorf("wal decode payload: %w", errors.ErrInvalidRecordPayload)
	if !stdErrors.Is(wrapped, errors.ErrInvalidRecordPayload) {
		t.Errorf("wrapped ErrInvalidRecordPayload must match via errors.Is")
	}

	if stdErrors.Is(errors.ErrInvalidRecordPayload, errors.ErrInvalidRecordType) {
		t.Errorf("ErrInvalidRecordPayload must not match ErrInvalidRecordType")
	}

	if !strings.Contains(errors.ErrInvalidRecordPayload.Error(), "invalid wal record payload") {
		t.Errorf("unexpected error message: %q", errors.ErrInvalidRecordPayload.Error())
	}
}

func TestInvalidRecordPayloadError(t *testing.T) {
	typedErr := &errors.InvalidRecordPayloadError{
		Type:   0x02,
		Reason: "delete tombstone cannot have a value payload",
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrInvalidRecordPayload) {
		t.Errorf("InvalidRecordPayloadError must match ErrInvalidRecordPayload via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrInvalidRecordType) {
		t.Errorf("InvalidRecordPayloadError must not match ErrInvalidRecordType")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("wal validate record: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrInvalidRecordPayload) {
		t.Errorf("wrapped InvalidRecordPayloadError must match ErrInvalidRecordPayload via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.InvalidRecordPayloadError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *InvalidRecordPayloadError")
	}
	if extracted.Type != 0x02 {
		t.Errorf("extracted Type mismatch: got 0x%02x, expected 0x02", extracted.Type)
	}
	if extracted.Reason != "delete tombstone cannot have a value payload" {
		t.Errorf("extracted Reason mismatch: got %q", extracted.Reason)
	}

	// Error string formatting with reason
	msg := typedErr.Error()
	if !strings.Contains(msg, "0x02") || !strings.Contains(msg, "delete tombstone cannot have a value payload") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Error string formatting without reason
	noReason := &errors.InvalidRecordPayloadError{Type: 0x03}
	if !strings.Contains(noReason.Error(), "0x03") {
		t.Errorf("unexpected error message: %q", noReason.Error())
	}
}

func TestNotADirectorySentinel(t *testing.T) {
	if errors.ErrNotADirectory == nil {
		t.Fatalf("ErrNotADirectory must not be nil")
	}

	wrapped := fmt.Errorf("wal init dir: %w", errors.ErrNotADirectory)
	if !stdErrors.Is(wrapped, errors.ErrNotADirectory) {
		t.Errorf("wrapped ErrNotADirectory must match via errors.Is")
	}

	if stdErrors.Is(errors.ErrNotADirectory, errors.ErrKeyNotFound) {
		t.Errorf("ErrNotADirectory must not match ErrKeyNotFound")
	}

	if !strings.Contains(errors.ErrNotADirectory.Error(), "path is not a directory") {
		t.Errorf("unexpected error message: %q", errors.ErrNotADirectory.Error())
	}
}

func TestNotADirectoryError(t *testing.T) {
	typedErr := &errors.NotADirectoryError{
		Path: "/var/lib/lattice/wal",
		Mode: 0644,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrNotADirectory) {
		t.Errorf("NotADirectoryError must match ErrNotADirectory via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrKeyNotFound) {
		t.Errorf("NotADirectoryError must not match ErrKeyNotFound")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("wal dir setup: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrNotADirectory) {
		t.Errorf("wrapped NotADirectoryError must match ErrNotADirectory via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.NotADirectoryError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *NotADirectoryError")
	}
	if extracted.Path != "/var/lib/lattice/wal" {
		t.Errorf("extracted Path mismatch: got %q, expected /var/lib/lattice/wal", extracted.Path)
	}
	if extracted.Mode != 0644 {
		t.Errorf("extracted Mode mismatch: got %v, expected 0644", extracted.Mode)
	}

	// Error string formatting with path and mode
	msg := typedErr.Error()
	if !strings.Contains(msg, "/var/lib/lattice/wal") || !strings.Contains(msg, "not a directory") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilTyped *errors.NotADirectoryError
	if nilTyped.Error() != errors.ErrNotADirectory.Error() {
		t.Errorf("typed nil error string mismatch: got %q, want %q", nilTyped.Error(), errors.ErrNotADirectory.Error())
	}

	// Empty path fallback
	emptyPath := &errors.NotADirectoryError{Mode: fs.ModeSymlink}
	if emptyPath.Error() != errors.ErrNotADirectory.Error() {
		t.Errorf("empty path error string mismatch: got %q, want %q", emptyPath.Error(), errors.ErrNotADirectory.Error())
	}
}

func TestErrWriterClosed(t *testing.T) {
	if errors.ErrWriterClosed == nil {
		t.Fatalf("ErrWriterClosed must not be nil")
	}

	wrapped := fmt.Errorf("wal append: %w", errors.ErrWriterClosed)
	if !stdErrors.Is(wrapped, errors.ErrWriterClosed) {
		t.Errorf("wrapped ErrWriterClosed must match via errors.Is")
	}

	if stdErrors.Is(errors.ErrWriterClosed, errors.ErrKeyNotFound) {
		t.Errorf("ErrWriterClosed must not match ErrKeyNotFound")
	}

	if !strings.Contains(errors.ErrWriterClosed.Error(), "wal writer is closed") {
		t.Errorf("unexpected error message: %q", errors.ErrWriterClosed.Error())
	}
}

func TestErrReaderClosed(t *testing.T) {
	if errors.ErrReaderClosed == nil {
		t.Fatalf("ErrReaderClosed must not be nil")
	}

	wrapped := fmt.Errorf("wal read: %w", errors.ErrReaderClosed)
	if !stdErrors.Is(wrapped, errors.ErrReaderClosed) {
		t.Errorf("wrapped ErrReaderClosed must match via errors.Is")
	}

	if stdErrors.Is(errors.ErrReaderClosed, errors.ErrKeyNotFound) {
		t.Errorf("ErrReaderClosed must not match ErrKeyNotFound")
	}

	if !strings.Contains(errors.ErrReaderClosed.Error(), "wal reader is closed") {
		t.Errorf("unexpected error message: %q", errors.ErrReaderClosed.Error())
	}
}

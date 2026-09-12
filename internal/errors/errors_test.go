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
		{"ErrVarintNonCanonical", errors.ErrVarintNonCanonical, "non-canonical varint encoding"},
		{"ErrInvalidOpType", errors.ErrInvalidOpType, "invalid operation type"},
		{"ErrSeqNumOverflow", errors.ErrSeqNumOverflow, "sequence number overflow"},
		{"ErrInternalKeyTruncated", errors.ErrInternalKeyTruncated, "internal key buffer truncated: missing trailer or user key"},
		{"ErrSegmentGap", errors.ErrSegmentGap, "wal segment gap detected"},
		{"ErrDuplicateSegment", errors.ErrDuplicateSegment, "duplicate wal segment ID detected"},
		{"ErrSequenceOutOfOrder", errors.ErrSequenceOutOfOrder, "sequence number out of order"},
		{"ErrQueueClosed", errors.ErrQueueClosed, "wal write queue is closed"},
		{"ErrQueueFull", errors.ErrQueueFull, "wal write queue is full"},
		{"ErrQueueEmpty", errors.ErrQueueEmpty, "wal write queue is empty"},
		{"ErrTaskAlreadyCompleted", errors.ErrTaskAlreadyCompleted, "wal write task already completed"},
		{"ErrTaskAlreadyEnqueued", errors.ErrTaskAlreadyEnqueued, "wal write task already enqueued"},
		{"ErrNilTask", errors.ErrNilTask, "wal write task cannot be nil"},
		{"ErrInvalidQueueCapacity", errors.ErrInvalidQueueCapacity, "wal write queue capacity must be greater than zero"},
		{"ErrRunnerRunning", errors.ErrRunnerRunning, "group commit runner is already running"},
		{"ErrRunnerClosed", errors.ErrRunnerClosed, "group commit runner is closed"},
		{"ErrInvalidSkipListHeight", errors.ErrInvalidSkipListHeight, "invalid skiplist node height"},
		{"ErrInvalidSkipListLevel", errors.ErrInvalidSkipListLevel, "invalid skiplist level"},
		{"ErrMemTableFrozen", errors.ErrMemTableFrozen, "memtable is frozen"},
		{"ErrIteratorClosed", errors.ErrIteratorClosed, "iterator is closed"},
		{"ErrNilReceiver", errors.ErrNilReceiver, "nil receiver pointer"},
		{"ErrKeyOutOfOrder", errors.ErrKeyOutOfOrder, "key out of order: keys must be added in strictly increasing canonical order"},
		{"ErrBlockFinished", errors.ErrBlockFinished, "block builder is finished"},
		{"ErrInvalidRestartInterval", errors.ErrInvalidRestartInterval, "invalid restart interval: must be greater than zero"},
		{"ErrBlockOverflow", errors.ErrBlockOverflow, "block size exceeds maximum 32-bit addressable capacity"},
		{"ErrBlockHandleTruncated", errors.ErrBlockHandleTruncated, "block handle truncated: buffer smaller than 16 bytes"},
		{"ErrInvalidBlockHandle", errors.ErrInvalidBlockHandle, "invalid block handle: size must be greater than zero and offset+size must not overflow"},
		{"ErrIndexFinished", errors.ErrIndexFinished, "index builder is finished"},
		{"ErrIndexBlockTruncated", errors.ErrIndexBlockTruncated, "index block truncated: buffer smaller than trailer"},
		{"ErrIndexBlockCorrupted", errors.ErrIndexBlockCorrupted, "index block corrupted: invalid offsets, count, or entries"},
		{"ErrInvalidFooter", errors.ErrInvalidFooter, "invalid sstable footer"},
		{"ErrInvalidFooterMagic", errors.ErrInvalidFooterMagic, "invalid sstable footer magic number"},
		{"ErrFooterTruncated", errors.ErrFooterTruncated, "sstable footer truncated: buffer smaller than 48 bytes"},
		{"ErrInvalidFooterSize", errors.ErrInvalidFooterSize, "invalid sstable footer buffer size: must be exactly 48 bytes"},
		{"ErrInvalidFooterPadding", errors.ErrInvalidFooterPadding, "invalid sstable footer padding: reserved padding bytes must be zero"},
		{"ErrTableWriterClosed", errors.ErrTableWriterClosed, "sstable writer is closed"},
		{"ErrTableWriterFinalized", errors.ErrTableWriterFinalized, "sstable writer is already finalized"},
		{"ErrSSTableExists", errors.ErrSSTableExists, "sstable file already exists"},
		{"ErrTableReaderClosed", errors.ErrTableReaderClosed, "sstable reader is closed"},
		{"ErrDataBlockCorrupted", errors.ErrDataBlockCorrupted, "data block corrupted: invalid crc, restart metadata, or entry framing"},
		{"ErrFilterBlockTruncated", errors.ErrFilterBlockTruncated, "filter block truncated: buffer smaller than trailer"},
		{"ErrFilterBlockCorrupted", errors.ErrFilterBlockCorrupted, "filter block corrupted: invalid bit count, size, or metadata"},
		{"ErrUnsupportedHashCount", errors.ErrUnsupportedHashCount, "unsupported filter hash count: must be 7"},
		{"ErrFilterFinished", errors.ErrFilterFinished, "filter block builder is finished"},
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
		{"ErrVarintNonCanonical", errors.ErrVarintNonCanonical},
		{"ErrInvalidOpType", errors.ErrInvalidOpType},
		{"ErrSeqNumOverflow", errors.ErrSeqNumOverflow},
		{"ErrInternalKeyTruncated", errors.ErrInternalKeyTruncated},
		{"ErrSegmentGap", errors.ErrSegmentGap},
		{"ErrDuplicateSegment", errors.ErrDuplicateSegment},
		{"ErrSequenceOutOfOrder", errors.ErrSequenceOutOfOrder},
		{"ErrQueueClosed", errors.ErrQueueClosed},
		{"ErrQueueFull", errors.ErrQueueFull},
		{"ErrQueueEmpty", errors.ErrQueueEmpty},
		{"ErrTaskAlreadyCompleted", errors.ErrTaskAlreadyCompleted},
		{"ErrTaskAlreadyEnqueued", errors.ErrTaskAlreadyEnqueued},
		{"ErrNilTask", errors.ErrNilTask},
		{"ErrInvalidQueueCapacity", errors.ErrInvalidQueueCapacity},
		{"ErrRunnerRunning", errors.ErrRunnerRunning},
		{"ErrRunnerClosed", errors.ErrRunnerClosed},
		{"ErrInvalidSkipListHeight", errors.ErrInvalidSkipListHeight},
		{"ErrInvalidSkipListLevel", errors.ErrInvalidSkipListLevel},
		{"ErrMemTableFrozen", errors.ErrMemTableFrozen},
		{"ErrIteratorClosed", errors.ErrIteratorClosed},
		{"ErrNilReceiver", errors.ErrNilReceiver},
		{"ErrKeyOutOfOrder", errors.ErrKeyOutOfOrder},
		{"ErrBlockFinished", errors.ErrBlockFinished},
		{"ErrInvalidRestartInterval", errors.ErrInvalidRestartInterval},
		{"ErrBlockOverflow", errors.ErrBlockOverflow},
		{"ErrTableReaderClosed", errors.ErrTableReaderClosed},
		{"ErrDataBlockCorrupted", errors.ErrDataBlockCorrupted},
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
		errors.ErrVarintNonCanonical,
		errors.ErrInvalidOpType,
		errors.ErrSeqNumOverflow,
		errors.ErrInternalKeyTruncated,
		errors.ErrSegmentGap,
		errors.ErrDuplicateSegment,
		errors.ErrSequenceOutOfOrder,
		errors.ErrQueueClosed,
		errors.ErrQueueFull,
		errors.ErrQueueEmpty,
		errors.ErrTaskAlreadyCompleted,
		errors.ErrTaskAlreadyEnqueued,
		errors.ErrNilTask,
		errors.ErrInvalidQueueCapacity,
		errors.ErrRunnerRunning,
		errors.ErrRunnerClosed,
		errors.ErrInvalidSkipListHeight,
		errors.ErrInvalidSkipListLevel,
		errors.ErrMemTableFrozen,
		errors.ErrIteratorClosed,
		errors.ErrNilReceiver,
		errors.ErrKeyOutOfOrder,
		errors.ErrBlockFinished,
		errors.ErrInvalidRestartInterval,
		errors.ErrBlockOverflow,
		errors.ErrBlockHandleTruncated,
		errors.ErrInvalidBlockHandle,
		errors.ErrIndexFinished,
		errors.ErrIndexBlockTruncated,
		errors.ErrIndexBlockCorrupted,
		errors.ErrInvalidFooter,
		errors.ErrInvalidFooterMagic,
		errors.ErrFooterTruncated,
		errors.ErrInvalidFooterSize,
		errors.ErrInvalidFooterPadding,
		errors.ErrTableWriterClosed,
		errors.ErrTableWriterFinalized,
		errors.ErrSSTableExists,
		errors.ErrTableReaderClosed,
		errors.ErrDataBlockCorrupted,
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

func TestKeyOutOfOrderError(t *testing.T) {
	typedErr := &errors.KeyOutOfOrderError{
		PrevKeyLen: 10,
		CurrKeyLen: 8,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrKeyOutOfOrder) {
		t.Errorf("KeyOutOfOrderError must match ErrKeyOutOfOrder via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrKeyNotFound) {
		t.Errorf("KeyOutOfOrderError must not match ErrKeyNotFound")
	}

	// errors.As extraction
	var extracted *errors.KeyOutOfOrderError
	if !stdErrors.As(typedErr, &extracted) {
		t.Fatalf("errors.As failed to extract *KeyOutOfOrderError")
	}
	if extracted.PrevKeyLen != 10 || extracted.CurrKeyLen != 8 {
		t.Errorf("extracted fields mismatch: got PrevKeyLen=%d, CurrKeyLen=%d", extracted.PrevKeyLen, extracted.CurrKeyLen)
	}

	// Error string validation: must contain lengths and not leak keys
	msg := typedErr.Error()
	if !strings.Contains(msg, "10") || !strings.Contains(msg, "8") {
		t.Errorf("unexpected error string formatting: %q", msg)
	}

	// Nil receiver check
	var nilErr *errors.KeyOutOfOrderError
	if nilErr.Error() != errors.ErrKeyOutOfOrder.Error() {
		t.Errorf("expected sentinel message on nil receiver, got %q", nilErr.Error())
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
	var sg *errors.SegmentGapError
	var ds *errors.DuplicateSegmentError
	var soo *errors.SequenceOutOfOrderError

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
	if sg.Error() != errors.ErrSegmentGap.Error() {
		t.Errorf("expected %q, got %q", errors.ErrSegmentGap.Error(), sg.Error())
	}
	if ds.Error() != errors.ErrDuplicateSegment.Error() {
		t.Errorf("expected %q, got %q", errors.ErrDuplicateSegment.Error(), ds.Error())
	}
	if soo.Error() != errors.ErrSequenceOutOfOrder.Error() {
		t.Errorf("expected %q, got %q", errors.ErrSequenceOutOfOrder.Error(), soo.Error())
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
	if !sg.Is(errors.ErrSegmentGap) {
		t.Errorf("nil *SegmentGapError must match ErrSegmentGap via Is()")
	}
	if !ds.Is(errors.ErrDuplicateSegment) {
		t.Errorf("nil *DuplicateSegmentError must match ErrDuplicateSegment via Is()")
	}
	if !soo.Is(errors.ErrSequenceOutOfOrder) {
		t.Errorf("nil *SequenceOutOfOrderError must match ErrSequenceOutOfOrder via Is()")
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
		{"SegmentGapError", (*errors.SegmentGapError)(nil), errors.ErrSegmentGap},
		{"DuplicateSegmentError", (*errors.DuplicateSegmentError)(nil), errors.ErrDuplicateSegment},
		{"SequenceOutOfOrderError", (*errors.SequenceOutOfOrderError)(nil), errors.ErrSequenceOutOfOrder},
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

func TestSegmentIDOverflowError(t *testing.T) {
	typedErr := &errors.SegmentIDOverflowError{
		Current: 18446744073709551615,
	}

	// Must match sentinel via errors.Is
	if !stdErrors.Is(typedErr, errors.ErrSegmentIDOverflow) {
		t.Errorf("SegmentIDOverflowError must match ErrSegmentIDOverflow via errors.Is")
	}

	// Negative match against other sentinels
	if stdErrors.Is(typedErr, errors.ErrSeqNumOverflow) {
		t.Errorf("SegmentIDOverflowError must not match ErrSeqNumOverflow")
	}

	// Wrapped match via errors.Is
	wrapped := fmt.Errorf("wal segment rotation: %w", typedErr)
	if !stdErrors.Is(wrapped, errors.ErrSegmentIDOverflow) {
		t.Errorf("wrapped SegmentIDOverflowError must match ErrSegmentIDOverflow via errors.Is")
	}

	// Extraction via errors.As
	var extracted *errors.SegmentIDOverflowError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("errors.As failed to extract *SegmentIDOverflowError")
	}
	if extracted.Current != 18446744073709551615 {
		t.Errorf("extracted Current mismatch: got %d", extracted.Current)
	}

	// Error string formatting
	msg := typedErr.Error()
	if !strings.Contains(msg, "18446744073709551615") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Nil receiver error message
	var nilErr *errors.SegmentIDOverflowError
	if nilErr.Error() != errors.ErrSegmentIDOverflow.Error() {
		t.Errorf("nil receiver must return sentinel message")
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

func TestSegmentGapError(t *testing.T) {
	err := &errors.SegmentGapError{
		Expected: 3,
		Actual:   5,
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrSegmentGap) {
		t.Errorf("SegmentGapError must match ErrSegmentGap via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("SegmentGapError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("wal recovery: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrSegmentGap) {
		t.Errorf("wrapped SegmentGapError must match ErrSegmentGap via errors.Is")
	}

	var extracted *errors.SegmentGapError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *SegmentGapError from wrapped chain")
	}
	if extracted.Expected != 3 || extracted.Actual != 5 {
		t.Errorf("extracted mismatch: got Expected=%d, Actual=%d; want Expected=3, Actual=5", extracted.Expected, extracted.Actual)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "expected segment ID 3, got 5") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.SegmentGapError
	if nilErr.Error() != errors.ErrSegmentGap.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrSegmentGap.Error())
	}
}

func TestDuplicateSegmentError(t *testing.T) {
	err := &errors.DuplicateSegmentError{
		SegmentID: 4,
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrDuplicateSegment) {
		t.Errorf("DuplicateSegmentError must match ErrDuplicateSegment via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("DuplicateSegmentError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("wal recovery: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrDuplicateSegment) {
		t.Errorf("wrapped DuplicateSegmentError must match ErrDuplicateSegment via errors.Is")
	}

	var extracted *errors.DuplicateSegmentError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *DuplicateSegmentError from wrapped chain")
	}
	if extracted.SegmentID != 4 {
		t.Errorf("extracted mismatch: got SegmentID=%d, want 4", extracted.SegmentID)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "duplicate wal segment ID 4 detected") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.DuplicateSegmentError
	if nilErr.Error() != errors.ErrDuplicateSegment.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrDuplicateSegment.Error())
	}
}

func TestSequenceOutOfOrderError(t *testing.T) {
	err := &errors.SequenceOutOfOrderError{
		Previous: 100,
		Current:  99,
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrSequenceOutOfOrder) {
		t.Errorf("SequenceOutOfOrderError must match ErrSequenceOutOfOrder via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("SequenceOutOfOrderError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("wal recovery: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrSequenceOutOfOrder) {
		t.Errorf("wrapped SequenceOutOfOrderError must match ErrSequenceOutOfOrder via errors.Is")
	}

	var extracted *errors.SequenceOutOfOrderError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *SequenceOutOfOrderError from wrapped chain")
	}
	if extracted.Previous != 100 || extracted.Current != 99 {
		t.Errorf("extracted mismatch: got Previous=%d, Current=%d; want Previous=100, Current=99", extracted.Previous, extracted.Current)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "current 99 is not greater than previous 100") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.SequenceOutOfOrderError
	if nilErr.Error() != errors.ErrSequenceOutOfOrder.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrSequenceOutOfOrder.Error())
	}
}

func TestInvalidQueueCapacityError(t *testing.T) {
	err := &errors.InvalidQueueCapacityError{
		Capacity: -1,
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrInvalidQueueCapacity) {
		t.Errorf("InvalidQueueCapacityError must match ErrInvalidQueueCapacity via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("InvalidQueueCapacityError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("queue init: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrInvalidQueueCapacity) {
		t.Errorf("wrapped InvalidQueueCapacityError must match ErrInvalidQueueCapacity via errors.Is")
	}

	var extracted *errors.InvalidQueueCapacityError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidQueueCapacityError from wrapped chain")
	}
	if extracted.Capacity != -1 {
		t.Errorf("extracted mismatch: got Capacity=%d; want Capacity=-1", extracted.Capacity)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "invalid wal write queue capacity -1: must be greater than zero") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.InvalidQueueCapacityError
	if nilErr.Error() != errors.ErrInvalidQueueCapacity.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidQueueCapacity.Error())
	}
}

func TestInvalidSkipListHeightError(t *testing.T) {
	err := &errors.InvalidSkipListHeightError{
		Height:    0,
		MinHeight: 1,
		MaxHeight: 16,
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrInvalidSkipListHeight) {
		t.Errorf("InvalidSkipListHeightError must match ErrInvalidSkipListHeight via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("InvalidSkipListHeightError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("node allocation: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrInvalidSkipListHeight) {
		t.Errorf("wrapped InvalidSkipListHeightError must match ErrInvalidSkipListHeight via errors.Is")
	}

	var extracted *errors.InvalidSkipListHeightError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidSkipListHeightError from wrapped chain")
	}
	if extracted.Height != 0 || extracted.MinHeight != 1 || extracted.MaxHeight != 16 {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "invalid skiplist node height 0: must be between 1 and 16") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.InvalidSkipListHeightError
	if nilErr.Error() != errors.ErrInvalidSkipListHeight.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidSkipListHeight.Error())
	}
}

func TestInvalidSkipListLevelError(t *testing.T) {
	err := &errors.InvalidSkipListLevelError{
		Level:    16,
		MaxLevel: 15,
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrInvalidSkipListLevel) {
		t.Errorf("InvalidSkipListLevelError must match ErrInvalidSkipListLevel via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("InvalidSkipListLevelError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("forward traversal: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrInvalidSkipListLevel) {
		t.Errorf("wrapped InvalidSkipListLevelError must match ErrInvalidSkipListLevel via errors.Is")
	}

	var extracted *errors.InvalidSkipListLevelError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidSkipListLevelError from wrapped chain")
	}
	if extracted.Level != 16 || extracted.MaxLevel != 15 {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "invalid skiplist level 16: must be between 0 and 15") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.InvalidSkipListLevelError
	if nilErr.Error() != errors.ErrInvalidSkipListLevel.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidSkipListLevel.Error())
	}
}

func TestInvalidBlockHandleError(t *testing.T) {
	err := &errors.InvalidBlockHandleError{
		Offset: 1024,
		Size:   0,
		Reason: "block size cannot be zero",
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrInvalidBlockHandle) {
		t.Errorf("InvalidBlockHandleError must match ErrInvalidBlockHandle via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("InvalidBlockHandleError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("read block: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrInvalidBlockHandle) {
		t.Errorf("wrapped InvalidBlockHandleError must match ErrInvalidBlockHandle via errors.Is")
	}

	var extracted *errors.InvalidBlockHandleError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidBlockHandleError from wrapped chain")
	}
	if extracted.Offset != 1024 || extracted.Size != 0 {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "offset=1024") || !strings.Contains(msg, "size=0") || !strings.Contains(msg, "block size cannot be zero") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.InvalidBlockHandleError
	if nilErr.Error() != errors.ErrInvalidBlockHandle.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidBlockHandle.Error())
	}
	if !nilErr.Is(errors.ErrInvalidBlockHandle) {
		t.Errorf("nil *InvalidBlockHandleError must match ErrInvalidBlockHandle via Is")
	}
}

func TestIndexBlockCorruptedError(t *testing.T) {
	err := &errors.IndexBlockCorruptedError{
		Reason: "offsets array non-monotonic",
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrIndexBlockCorrupted) {
		t.Errorf("IndexBlockCorruptedError must match ErrIndexBlockCorrupted via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("IndexBlockCorruptedError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("parse index: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrIndexBlockCorrupted) {
		t.Errorf("wrapped IndexBlockCorruptedError must match ErrIndexBlockCorrupted via errors.Is")
	}

	var extracted *errors.IndexBlockCorruptedError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *IndexBlockCorruptedError from wrapped chain")
	}
	if extracted.Reason != "offsets array non-monotonic" {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "index block corrupted: offsets array non-monotonic") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.IndexBlockCorruptedError
	if nilErr.Error() != errors.ErrIndexBlockCorrupted.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrIndexBlockCorrupted.Error())
	}
	if !nilErr.Is(errors.ErrIndexBlockCorrupted) {
		t.Errorf("nil *IndexBlockCorruptedError must match ErrIndexBlockCorrupted via Is")
	}
}

func TestInvalidFooterMagicError(t *testing.T) {
	err := &errors.InvalidFooterMagicError{
		Expected: 0x4C41545453535401,
		Actual:   0x1234567890ABCDEF,
	}

	// errors.Is check (matches both ErrInvalidFooterMagic and ErrInvalidFooter)
	if !stdErrors.Is(err, errors.ErrInvalidFooterMagic) {
		t.Errorf("InvalidFooterMagicError must match ErrInvalidFooterMagic via errors.Is")
	}
	if !stdErrors.Is(err, errors.ErrInvalidFooter) {
		t.Errorf("InvalidFooterMagicError must match ErrInvalidFooter via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("InvalidFooterMagicError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("decode footer: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrInvalidFooterMagic) {
		t.Errorf("wrapped InvalidFooterMagicError must match ErrInvalidFooterMagic via errors.Is")
	}
	if !stdErrors.Is(wrapped, errors.ErrInvalidFooter) {
		t.Errorf("wrapped InvalidFooterMagicError must match ErrInvalidFooter via errors.Is")
	}

	var extracted *errors.InvalidFooterMagicError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidFooterMagicError from wrapped chain")
	}
	if extracted.Expected != 0x4C41545453535401 || extracted.Actual != 0x1234567890ABCDEF {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "expected 0x4c41545453535401") || !strings.Contains(msg, "got 0x1234567890abcdef") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.InvalidFooterMagicError
	if nilErr.Error() != errors.ErrInvalidFooterMagic.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidFooterMagic.Error())
	}
	if !nilErr.Is(errors.ErrInvalidFooterMagic) {
		t.Errorf("nil *InvalidFooterMagicError must match ErrInvalidFooterMagic via Is")
	}
	if !nilErr.Is(errors.ErrInvalidFooter) {
		t.Errorf("nil *InvalidFooterMagicError must match ErrInvalidFooter via Is")
	}
}

func TestInvalidFooterPaddingError(t *testing.T) {
	err := &errors.InvalidFooterPaddingError{
		Padding: [8]byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrInvalidFooterPadding) {
		t.Errorf("InvalidFooterPaddingError must match ErrInvalidFooterPadding via errors.Is")
	}
	if !stdErrors.Is(err, errors.ErrInvalidFooter) {
		t.Errorf("InvalidFooterPaddingError must match ErrInvalidFooter via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("InvalidFooterPaddingError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("validate footer: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrInvalidFooterPadding) {
		t.Errorf("wrapped InvalidFooterPaddingError must match ErrInvalidFooterPadding via errors.Is")
	}

	var extracted *errors.InvalidFooterPaddingError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidFooterPaddingError from wrapped chain")
	}
	if extracted.Padding != err.Padding {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check
	msg := err.Error()
	if !strings.Contains(msg, "invalid sstable footer padding: expected all zeros") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// Typed nil safety
	var nilErr *errors.InvalidFooterPaddingError
	if nilErr.Error() != errors.ErrInvalidFooterPadding.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidFooterPadding.Error())
	}
	if !nilErr.Is(errors.ErrInvalidFooterPadding) {
		t.Errorf("nil *InvalidFooterPaddingError must match ErrInvalidFooterPadding via Is")
	}
	if !nilErr.Is(errors.ErrInvalidFooter) {
		t.Errorf("nil *InvalidFooterPaddingError must match ErrInvalidFooter via Is")
	}
}

func TestInvalidFooterSizeError(t *testing.T) {
	errTruncated := &errors.InvalidFooterSizeError{
		Expected: 48,
		Actual:   30,
	}

	// errors.Is check for truncated: matches ErrFooterTruncated, ErrInvalidFooterSize, ErrInvalidFooter
	if !stdErrors.Is(errTruncated, errors.ErrFooterTruncated) {
		t.Errorf("errTruncated must match ErrFooterTruncated via errors.Is")
	}
	if !stdErrors.Is(errTruncated, errors.ErrInvalidFooterSize) {
		t.Errorf("errTruncated must match ErrInvalidFooterSize via errors.Is")
	}
	if !stdErrors.Is(errTruncated, errors.ErrInvalidFooter) {
		t.Errorf("errTruncated must match ErrInvalidFooter via errors.Is")
	}

	errOversized := &errors.InvalidFooterSizeError{
		Expected: 48,
		Actual:   60,
	}

	// errors.Is check for oversized: matches ErrInvalidFooterSize, ErrInvalidFooter (not ErrFooterTruncated)
	if stdErrors.Is(errOversized, errors.ErrFooterTruncated) {
		t.Errorf("errOversized must not match ErrFooterTruncated")
	}
	if !stdErrors.Is(errOversized, errors.ErrInvalidFooterSize) {
		t.Errorf("errOversized must match ErrInvalidFooterSize via errors.Is")
	}
	if !stdErrors.Is(errOversized, errors.ErrInvalidFooter) {
		t.Errorf("errOversized must match ErrInvalidFooter via errors.Is")
	}

	// Negative match
	if stdErrors.Is(errTruncated, errors.ErrKeyNotFound) {
		t.Errorf("must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("read footer: %w", errTruncated)
	var extracted *errors.InvalidFooterSizeError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *InvalidFooterSizeError from wrapped chain")
	}
	if extracted.Expected != 48 || extracted.Actual != 30 {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// Typed nil safety
	var nilErr *errors.InvalidFooterSizeError
	if nilErr.Error() != errors.ErrInvalidFooterSize.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrInvalidFooterSize.Error())
	}
	if !nilErr.Is(errors.ErrInvalidFooterSize) {
		t.Errorf("nil *InvalidFooterSizeError must match ErrInvalidFooterSize via Is")
	}
	if !nilErr.Is(errors.ErrInvalidFooter) {
		t.Errorf("nil *InvalidFooterSizeError must match ErrInvalidFooter via Is")
	}
}

func TestDataBlockCorruptedError(t *testing.T) {
	err := &errors.DataBlockCorruptedError{
		Offset: 4096,
		Reason: "checksum mismatch",
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
		t.Errorf("DataBlockCorruptedError must match ErrDataBlockCorrupted via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("DataBlockCorruptedError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("read block: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrDataBlockCorrupted) {
		t.Errorf("wrapped DataBlockCorruptedError must match ErrDataBlockCorrupted via errors.Is")
	}

	var extracted *errors.DataBlockCorruptedError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *DataBlockCorruptedError from wrapped chain")
	}
	if extracted.Offset != 4096 || extracted.Reason != "checksum mismatch" {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check with reason
	msg := err.Error()
	if !strings.Contains(msg, "data block corrupted at offset 4096: checksum mismatch") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// String check without reason
	errNoReason := &errors.DataBlockCorruptedError{Offset: 8192}
	if errNoReason.Error() != "data block corrupted at offset 8192" {
		t.Errorf("unexpected error message without reason: %q", errNoReason.Error())
	}

	// Typed nil safety
	var nilErr *errors.DataBlockCorruptedError
	if nilErr.Error() != errors.ErrDataBlockCorrupted.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrDataBlockCorrupted.Error())
	}
	if !nilErr.Is(errors.ErrDataBlockCorrupted) {
		t.Errorf("nil *DataBlockCorruptedError must match ErrDataBlockCorrupted via Is")
	}
}

func TestFilterBlockCorruptedError(t *testing.T) {
	err := &errors.FilterBlockCorruptedError{
		Reason: "bit count exceeds maximum key capacity",
	}

	// errors.Is check
	if !stdErrors.Is(err, errors.ErrFilterBlockCorrupted) {
		t.Errorf("FilterBlockCorruptedError must match ErrFilterBlockCorrupted via errors.Is")
	}

	// Negative match
	if stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Errorf("FilterBlockCorruptedError must not match ErrKeyNotFound")
	}

	// Wrapping check
	wrapped := fmt.Errorf("read filter: %w", err)
	if !stdErrors.Is(wrapped, errors.ErrFilterBlockCorrupted) {
		t.Errorf("wrapped FilterBlockCorruptedError must match ErrFilterBlockCorrupted via errors.Is")
	}

	var extracted *errors.FilterBlockCorruptedError
	if !stdErrors.As(wrapped, &extracted) {
		t.Fatalf("failed to extract *FilterBlockCorruptedError from wrapped chain")
	}
	if extracted.Reason != "bit count exceeds maximum key capacity" {
		t.Errorf("extracted mismatch: got %+v", extracted)
	}

	// String check with reason
	msg := err.Error()
	if !strings.Contains(msg, "filter block corrupted: bit count exceeds maximum key capacity") {
		t.Errorf("unexpected error message: %q", msg)
	}

	// String check without reason
	errNoReason := &errors.FilterBlockCorruptedError{}
	if errNoReason.Error() != errors.ErrFilterBlockCorrupted.Error() {
		t.Errorf("unexpected error message without reason: %q", errNoReason.Error())
	}

	// Typed nil safety
	var nilErr *errors.FilterBlockCorruptedError
	if nilErr.Error() != errors.ErrFilterBlockCorrupted.Error() {
		t.Errorf("typed nil error mismatch: got %q, want %q", nilErr.Error(), errors.ErrFilterBlockCorrupted.Error())
	}
	if !nilErr.Is(errors.ErrFilterBlockCorrupted) {
		t.Errorf("nil *FilterBlockCorruptedError must match ErrFilterBlockCorrupted via Is")
	}
}

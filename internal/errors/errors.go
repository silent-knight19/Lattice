package errors

import (
	stdErrors "errors"
	"fmt"
	"io/fs"
)

// Sentinel errors representing fundamental domain failure conditions in Lattice.
// All sentinels can be inspected across wrapping chains using errors.Is().
var (
	// ErrKeyNotFound indicates that the requested key does not exist in any storage layer
	// (active MemTable, immutable MemTables, or SSTables).
	ErrKeyNotFound = stdErrors.New("key not found")

	// ErrEmptyKey indicates that a zero-length key was provided where a valid key is required.
	ErrEmptyKey = stdErrors.New("key cannot be empty")

	// ErrKeyTooLarge indicates that a key exceeds the maximum allowed size (65,535 bytes / 64 KB - 1).
	ErrKeyTooLarge = stdErrors.New("key exceeds maximum allowed size")

	// ErrValueTooLarge indicates that a value exceeds the maximum allowed size (4 MB).
	ErrValueTooLarge = stdErrors.New("value exceeds maximum allowed size")

	// ErrChecksumMismatch indicates that a CRC32 checksum verification failed,
	// signalling data bit-rot, torn block, or transmission corruption.
	ErrChecksumMismatch = stdErrors.New("checksum mismatch: data corrupted")

	// ErrTornWrite indicates that a partial, uncommitted write was detected at the
	// physical end-of-file of an active WAL during crash recovery replay.
	ErrTornWrite = stdErrors.New("torn write detected at tail of log")

	// ErrCompactionRunning indicates that a requested compaction operation cannot
	// proceed because another compaction worker is actively processing the targeted levels.
	ErrCompactionRunning = stdErrors.New("compaction already in progress")

	// ErrVarintOverflow indicates that a varint byte sequence exceeds the maximum
	// 64-bit unsigned integer representation (exceeds 10 bytes, or the 10th byte
	// contains invalid payload bits).
	ErrVarintOverflow = stdErrors.New("varint exceeds maximum 64-bit integer size")

	// ErrVarintTruncated indicates that a buffer ended prematurely while decoding
	// a varint before encountering a terminating byte.
	ErrVarintTruncated = stdErrors.New("varint buffer truncated or incomplete")

	// ErrVarintNonCanonical indicates that a varint byte sequence uses a non-minimal
	// overlong encoding that is prohibited under strict canonical decoding contracts.
	ErrVarintNonCanonical = stdErrors.New("non-canonical varint encoding")

	// ErrInvalidOpType indicates that an operation type byte does not correspond
	// to any recognized database operation (only PUT and DELETE/TOMBSTONE are valid).
	ErrInvalidOpType = stdErrors.New("invalid operation type")

	// ErrSeqNumOverflow indicates that incrementing a sequence number would exceed
	// the maximum 64-bit unsigned integer representation (wraparound prohibited).
	ErrSeqNumOverflow = stdErrors.New("sequence number overflow")

	// ErrInternalKeyTruncated indicates that an encoded internal key byte sequence
	// is shorter than the minimum required size (user key + 9-byte sequence/op trailer).
	ErrInternalKeyTruncated = stdErrors.New("internal key buffer truncated: missing trailer or user key")

	// ErrHeaderTruncated indicates that a buffer provided to decode a WAL record header
	// is shorter than the required 21 bytes.
	ErrHeaderTruncated = stdErrors.New("wal record header truncated: buffer smaller than 21 bytes")

	// ErrInvalidRecordType indicates that a WAL record type byte does not correspond
	// to any recognized record type (PUT, DELETE, BATCH_START, BATCH_COMMIT).
	ErrInvalidRecordType = stdErrors.New("invalid wal record type")

	// ErrInvalidRecordPayload indicates that a WAL record payload does not conform to its record type
	// (e.g. non-empty value for DELETE/tombstone, or non-empty key/value for batch markers).
	ErrInvalidRecordPayload = stdErrors.New("invalid wal record payload")

	// ErrNotADirectory indicates that a filesystem path expected to be a directory
	// is a regular file, symlink, or other non-directory object.
	ErrNotADirectory = stdErrors.New("path is not a directory")

	// ErrWriterClosed indicates that an operation was attempted on a closed WAL writer.
	ErrWriterClosed = stdErrors.New("wal writer is closed")

	// ErrReaderClosed indicates that an operation was attempted on a closed WAL reader.
	ErrReaderClosed = stdErrors.New("wal reader is closed")

	// ErrSegmentIDOverflow indicates that incrementing a WAL segment ID would exceed
	// the maximum 64-bit unsigned integer representation (wraparound prohibited).
	ErrSegmentIDOverflow = stdErrors.New("segment ID overflow")

	// ErrSegmentGap indicates that a numerical gap was detected in the sequence
	// of WAL segment IDs during startup recovery.
	ErrSegmentGap = stdErrors.New("wal segment gap detected")

	// ErrDuplicateSegment indicates that multiple segment entries resolved to the
	// same numeric segment ID during startup recovery.
	ErrDuplicateSegment = stdErrors.New("duplicate wal segment ID detected")

	// ErrSequenceOutOfOrder indicates that a WAL record's sequence number regressed
	// or duplicated a previous sequence number, violating monotonic ordering.
	ErrSequenceOutOfOrder = stdErrors.New("sequence number out of order")

	// ErrQueueClosed indicates that an enqueue or dequeue operation was attempted
	// on a closed WAL write queue.
	ErrQueueClosed = stdErrors.New("wal write queue is closed")

	// ErrQueueFull indicates that a non-blocking enqueue was attempted on a full WAL write queue.
	ErrQueueFull = stdErrors.New("wal write queue is full")

	// ErrQueueEmpty indicates that a non-blocking dequeue was attempted on an empty WAL write queue.
	ErrQueueEmpty = stdErrors.New("wal write queue is empty")

	// ErrTaskAlreadyCompleted indicates that a write task was completed more than once.
	ErrTaskAlreadyCompleted = stdErrors.New("wal write task already completed")

	// ErrTaskAlreadyEnqueued indicates that a write task was enqueued more than once.
	ErrTaskAlreadyEnqueued = stdErrors.New("wal write task already enqueued")

	// ErrNilTask indicates that a nil write task was passed to an enqueue operation.
	ErrNilTask = stdErrors.New("wal write task cannot be nil")

	// ErrInvalidQueueCapacity indicates that a WAL write queue capacity was non-positive.
	ErrInvalidQueueCapacity = stdErrors.New("wal write queue capacity must be greater than zero")

	// ErrRunnerRunning indicates that Start was called on an already-running group commit runner.
	ErrRunnerRunning = stdErrors.New("group commit runner is already running")

	// ErrRunnerClosed indicates that an operation was attempted on a closed group commit runner.
	ErrRunnerClosed = stdErrors.New("group commit runner is closed")

	// ErrInvalidSkipListHeight indicates that a requested SkipList node height violates
	// the architectural bounds [MinHeight, MaxHeight].
	ErrInvalidSkipListHeight = stdErrors.New("invalid skiplist node height")

	// ErrInvalidSkipListLevel indicates that a requested SkipList level index violates
	// the bounds [0, height-1] for a node.
	ErrInvalidSkipListLevel = stdErrors.New("invalid skiplist level")

	// ErrMemTableFrozen indicates that an operation attempting mutation was rejected
	// because the MemTable/SkipList is permanently frozen (read-only).
	ErrMemTableFrozen = stdErrors.New("memtable is frozen")

	// ErrIteratorClosed indicates that an operation was attempted on a closed iterator.
	ErrIteratorClosed = stdErrors.New("iterator is closed")

	// ErrNilReceiver indicates that a method was invoked on a nil pointer receiver.
	ErrNilReceiver = stdErrors.New("nil receiver pointer")

	// ErrKeyOutOfOrder indicates that an entry was added out of strictly increasing canonical order.
	ErrKeyOutOfOrder = stdErrors.New("key out of order: keys must be added in strictly increasing canonical order")

	// ErrBlockFinished indicates that mutation was attempted on an already finished/sealed block builder.
	ErrBlockFinished = stdErrors.New("block builder is finished")

	// ErrInvalidRestartInterval indicates that an invalid restart interval was provided (must be >= 1).
	ErrInvalidRestartInterval = stdErrors.New("invalid restart interval: must be greater than zero")

	// ErrBlockOverflow indicates that a block's size exceeds 32-bit addressable capacity.
	ErrBlockOverflow = stdErrors.New("block size exceeds maximum 32-bit addressable capacity")

	// ErrBlockHandleTruncated indicates that a block handle buffer is shorter than 16 bytes.
	ErrBlockHandleTruncated = stdErrors.New("block handle truncated: buffer smaller than 16 bytes")

	// ErrInvalidBlockHandle indicates that a block handle has an invalid size (zero) or offset+size overflows.
	ErrInvalidBlockHandle = stdErrors.New("invalid block handle: size must be greater than zero and offset+size must not overflow")

	// ErrIndexFinished indicates that mutation was attempted on an already finished/sealed index builder.
	ErrIndexFinished = stdErrors.New("index builder is finished")

	// ErrIndexBlockTruncated indicates that an index block buffer is shorter than minimum trailer length.
	ErrIndexBlockTruncated = stdErrors.New("index block truncated: buffer smaller than trailer")

	// ErrIndexBlockCorrupted indicates that an index block's trailer, offsets, or entries are corrupted.
	ErrIndexBlockCorrupted = stdErrors.New("index block corrupted: invalid offsets, count, or entries")

	// ErrInvalidFooter indicates that an SSTable footer failed structural, size, or format magic checks.
	ErrInvalidFooter = stdErrors.New("invalid sstable footer")

	// ErrInvalidFooterMagic indicates that an SSTable footer magic number does not match 0x4C41545453535401.
	ErrInvalidFooterMagic = stdErrors.New("invalid sstable footer magic number")

	// ErrFooterTruncated indicates that a buffer provided to decode an SSTable footer is shorter than 48 bytes.
	ErrFooterTruncated = stdErrors.New("sstable footer truncated: buffer smaller than 48 bytes")

	// ErrInvalidFooterSize indicates that an SSTable footer buffer does not match exactly 48 bytes.
	ErrInvalidFooterSize = stdErrors.New("invalid sstable footer buffer size: must be exactly 48 bytes")

	// ErrInvalidFooterPadding indicates that the reserved 8-byte padding in an SSTable footer is non-zero.
	ErrInvalidFooterPadding = stdErrors.New("invalid sstable footer padding: reserved padding bytes must be zero")

	// ErrTableWriterClosed indicates that an operation was attempted on a closed SSTable writer.
	ErrTableWriterClosed = stdErrors.New("sstable writer is closed")

	// ErrTableWriterFinalized indicates that finalization was attempted on an already finalized SSTable writer.
	ErrTableWriterFinalized = stdErrors.New("sstable writer is already finalized")

	// ErrSSTableExists indicates that an SSTable file already exists at the target path and cannot be overwritten.
	ErrSSTableExists = stdErrors.New("sstable file already exists")

	// ErrTableReaderClosed indicates that an operation was attempted on a closed SSTable reader.
	ErrTableReaderClosed = stdErrors.New("sstable reader is closed")

	// ErrDataBlockCorrupted indicates that an SSTable data block's trailer, CRC, restart array, or entry framing is corrupted.
	ErrDataBlockCorrupted = stdErrors.New("data block corrupted: invalid crc, restart metadata, or entry framing")

	// ErrInsecureFileMode indicates that a file mode was requested that violates the security baseline
	// (e.g. granting group or other permissions, or execution bits).
	ErrInsecureFileMode = stdErrors.New("insecure file mode: permissions must not grant group/other access or execute bits")

	// ErrWriterPoisoned indicates that an operation was attempted on a WAL writer
	// that entered an unrecoverable poisoned state following a write or sync failure.
	ErrWriterPoisoned = stdErrors.New("wal writer is poisoned")

	// ErrFilterBlockTruncated indicates that a filter block buffer is shorter than the minimum trailer length (13 bytes).
	ErrFilterBlockTruncated = stdErrors.New("filter block truncated: buffer smaller than trailer")

	// ErrFilterBlockCorrupted indicates that a filter block's bit count, size, or metadata is corrupted.
	ErrFilterBlockCorrupted = stdErrors.New("filter block corrupted: invalid bit count, size, or metadata")

	// ErrUnsupportedHashCount indicates that a serialized filter block specifies an unsupported hash count (must be 7).
	ErrUnsupportedHashCount = stdErrors.New("unsupported filter hash count: must be 7")

	// ErrFilterFinished indicates that mutation was attempted on an already finished/sealed filter block builder.
	ErrFilterFinished = stdErrors.New("filter block builder is finished")

	// ErrInvalidLevel indicates that an LSM-tree level index violates the architectural bounds [0, NumLevels-1].
	ErrInvalidLevel = stdErrors.New("invalid level")

	// ErrCorruptedVersionEdit indicates that a serialized VersionEdit record failed structural or boundary validation.
	ErrCorruptedVersionEdit = stdErrors.New("version edit corrupted")

	// ErrTruncatedVersionEdit indicates that a buffer ended prematurely while decoding a VersionEdit record.
	ErrTruncatedVersionEdit = stdErrors.New("version edit truncated")

	// ErrUnsupportedVersionEdit indicates that a VersionEdit specifies an unrecognized format version byte.
	ErrUnsupportedVersionEdit = stdErrors.New("unsupported version edit format")

	// ErrDuplicateScalarField indicates that a scalar field was encountered more than once in a single VersionEdit record.
	ErrDuplicateScalarField = stdErrors.New("duplicate scalar field in version edit")
)

// KeyOutOfOrderError provides structured context when a key violates strictly increasing
// canonical ordering. Raw key bytes are omitted to prevent sensitive credential disclosure.
type KeyOutOfOrderError struct {
	PrevKeyLen int
	CurrKeyLen int
}

func (e *KeyOutOfOrderError) Error() string {
	if e == nil {
		return ErrKeyOutOfOrder.Error()
	}
	return fmt.Sprintf("key out of order: current key (len=%d) sorts before or equal to previous key (len=%d)", e.CurrKeyLen, e.PrevKeyLen)
}

// Is reports whether this error matches target sentinel ErrKeyOutOfOrder.
func (e *KeyOutOfOrderError) Is(target error) bool {
	return target == ErrKeyOutOfOrder
}

// KeyTooLargeError provides structured context when a key violates maximum size limits.
// It matches ErrKeyTooLarge when interrogated with errors.Is().
// Note: To prevent sensitive data leakage, raw key bytes are deliberately omitted.
type KeyTooLargeError struct {
	KeySize uint32
	MaxSize uint32
}

func (e *KeyTooLargeError) Error() string {
	if e == nil {
		return ErrKeyTooLarge.Error()
	}
	return fmt.Sprintf("key size %d bytes exceeds maximum allowed size of %d bytes", e.KeySize, e.MaxSize)
}

// Is reports whether this error matches target sentinel ErrKeyTooLarge.
func (e *KeyTooLargeError) Is(target error) bool {
	return target == ErrKeyTooLarge
}

// ValueTooLargeError provides structured context when a value violates maximum size limits.
// It matches ErrValueTooLarge when interrogated with errors.Is().
// Note: To prevent sensitive data leakage, raw value bytes are deliberately omitted.
type ValueTooLargeError struct {
	ValueSize uint32
	MaxSize   uint32
}

func (e *ValueTooLargeError) Error() string {
	if e == nil {
		return ErrValueTooLarge.Error()
	}
	return fmt.Sprintf("value size %d bytes exceeds maximum allowed size of %d bytes", e.ValueSize, e.MaxSize)
}

// Is reports whether this error matches target sentinel ErrValueTooLarge.
func (e *ValueTooLargeError) Is(target error) bool {
	return target == ErrValueTooLarge
}

// ChecksumMismatchError provides structured diagnostics when a CRC32 checksum verification fails.
// It matches ErrChecksumMismatch when interrogated with errors.Is().
type ChecksumMismatchError struct {
	Offset   int64
	Expected uint32
	Actual   uint32
}

func (e *ChecksumMismatchError) Error() string {
	if e == nil {
		return ErrChecksumMismatch.Error()
	}
	return fmt.Sprintf("checksum mismatch at offset %d: expected 0x%08x, got 0x%08x", e.Offset, e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinel ErrChecksumMismatch.
func (e *ChecksumMismatchError) Is(target error) bool {
	return target == ErrChecksumMismatch
}

// TornWriteError provides structured diagnostics when an incomplete write is detected
// at the tail of a log file during crash recovery.
// It matches ErrTornWrite when interrogated with errors.Is().
type TornWriteError struct {
	Offset int64
	Reason string
}

func (e *TornWriteError) Error() string {
	if e == nil {
		return ErrTornWrite.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("torn write detected at tail offset %d: %s", e.Offset, e.Reason)
	}
	return fmt.Sprintf("torn write detected at tail offset %d", e.Offset)
}

// Is reports whether this error matches target sentinel ErrTornWrite.
func (e *TornWriteError) Is(target error) bool {
	return target == ErrTornWrite
}

// InvalidOpTypeError provides structured context when an unrecognized operation type byte is encountered.
// It matches ErrInvalidOpType when interrogated with errors.Is().
type InvalidOpTypeError struct {
	Op byte
}

func (e *InvalidOpTypeError) Error() string {
	if e == nil {
		return ErrInvalidOpType.Error()
	}
	return fmt.Sprintf("invalid operation type: 0x%02x", e.Op)
}

// Is reports whether this error matches target sentinel ErrInvalidOpType.
func (e *InvalidOpTypeError) Is(target error) bool {
	return target == ErrInvalidOpType
}

// SeqNumOverflowError provides structured context when incrementing a sequence number
// exceeds the maximum 64-bit unsigned integer limit.
// It matches ErrSeqNumOverflow when interrogated with errors.Is().
type SeqNumOverflowError struct {
	Current uint64
}

func (e *SeqNumOverflowError) Error() string {
	if e == nil {
		return ErrSeqNumOverflow.Error()
	}
	return fmt.Sprintf("sequence number overflow: current %d is at maximum uint64 limit", e.Current)
}

// Is reports whether this error matches target sentinel ErrSeqNumOverflow.
func (e *SeqNumOverflowError) Is(target error) bool {
	return target == ErrSeqNumOverflow
}

// SegmentIDOverflowError provides structured context when incrementing a segment ID
// exceeds the maximum 64-bit unsigned integer limit.
// It matches ErrSegmentIDOverflow when interrogated with errors.Is().
type SegmentIDOverflowError struct {
	Current uint64
}

func (e *SegmentIDOverflowError) Error() string {
	if e == nil {
		return ErrSegmentIDOverflow.Error()
	}
	return fmt.Sprintf("segment ID overflow: current %d is at maximum uint64 limit", e.Current)
}

// Is reports whether this error matches target sentinel ErrSegmentIDOverflow.
func (e *SegmentIDOverflowError) Is(target error) bool {
	return target == ErrSegmentIDOverflow
}

// InvalidRecordTypeError provides structured context when an unrecognized WAL record type byte is encountered.
// It matches ErrInvalidRecordType when interrogated with errors.Is().
type InvalidRecordTypeError struct {
	Type byte
}

func (e *InvalidRecordTypeError) Error() string {
	if e == nil {
		return ErrInvalidRecordType.Error()
	}
	return fmt.Sprintf("invalid wal record type: 0x%02x", e.Type)
}

// Is reports whether this error matches target sentinel ErrInvalidRecordType.
func (e *InvalidRecordTypeError) Is(target error) bool {
	return target == ErrInvalidRecordType
}

// InvalidRecordPayloadError provides structured context when a WAL record payload
// violates constraints for its record type.
// It matches ErrInvalidRecordPayload when interrogated with errors.Is().
type InvalidRecordPayloadError struct {
	Type   byte
	Reason string
}

func (e *InvalidRecordPayloadError) Error() string {
	if e == nil {
		return ErrInvalidRecordPayload.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid wal record payload for type 0x%02x: %s", e.Type, e.Reason)
	}
	return fmt.Sprintf("invalid wal record payload for type 0x%02x", e.Type)
}

// Is reports whether this error matches target sentinel ErrInvalidRecordPayload.
func (e *InvalidRecordPayloadError) Is(target error) bool {
	return target == ErrInvalidRecordPayload
}

// NotADirectoryError provides structured context when an expected directory path
// is a regular file, symlink, or other non-directory object.
// It matches ErrNotADirectory when interrogated with errors.Is().
type NotADirectoryError struct {
	Path string
	Mode fs.FileMode
}

func (e *NotADirectoryError) Error() string {
	if e == nil {
		return ErrNotADirectory.Error()
	}
	if e.Path != "" {
		return fmt.Sprintf("path %q is not a directory (mode: %s)", e.Path, e.Mode)
	}
	return ErrNotADirectory.Error()
}

// Is reports whether this error matches target sentinel ErrNotADirectory.
func (e *NotADirectoryError) Is(target error) bool {
	return target == ErrNotADirectory
}

// SegmentGapError provides structured context when a gap is detected in the sequence of WAL segment IDs.
// It matches ErrSegmentGap when interrogated with errors.Is().
type SegmentGapError struct {
	Expected uint64
	Actual   uint64
}

func (e *SegmentGapError) Error() string {
	if e == nil {
		return ErrSegmentGap.Error()
	}
	return fmt.Sprintf("wal segment gap detected: expected segment ID %d, got %d", e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinel ErrSegmentGap.
func (e *SegmentGapError) Is(target error) bool {
	return target == ErrSegmentGap
}

// DuplicateSegmentError provides structured context when multiple entries resolve to the same segment ID.
// It matches ErrDuplicateSegment when interrogated with errors.Is().
type DuplicateSegmentError struct {
	SegmentID uint64
}

func (e *DuplicateSegmentError) Error() string {
	if e == nil {
		return ErrDuplicateSegment.Error()
	}
	return fmt.Sprintf("duplicate wal segment ID %d detected", e.SegmentID)
}

// Is reports whether this error matches target sentinel ErrDuplicateSegment.
func (e *DuplicateSegmentError) Is(target error) bool {
	return target == ErrDuplicateSegment
}

// SequenceOutOfOrderError provides structured context when a WAL record violates monotonic sequence numbering.
// It matches ErrSequenceOutOfOrder when interrogated with errors.Is().
type SequenceOutOfOrderError struct {
	Previous uint64
	Current  uint64
}

func (e *SequenceOutOfOrderError) Error() string {
	if e == nil {
		return ErrSequenceOutOfOrder.Error()
	}
	return fmt.Sprintf("sequence number out of order: current %d is not greater than previous %d", e.Current, e.Previous)
}

// Is reports whether this error matches target sentinel ErrSequenceOutOfOrder.
func (e *SequenceOutOfOrderError) Is(target error) bool {
	return target == ErrSequenceOutOfOrder
}

// InvalidQueueCapacityError provides structured context when a queue is initialized with a non-positive capacity.
// It matches ErrInvalidQueueCapacity when interrogated with errors.Is().
type InvalidQueueCapacityError struct {
	Capacity int
}

func (e *InvalidQueueCapacityError) Error() string {
	if e == nil {
		return ErrInvalidQueueCapacity.Error()
	}
	return fmt.Sprintf("invalid wal write queue capacity %d: must be greater than zero", e.Capacity)
}

// Is reports whether this error matches target sentinel ErrInvalidQueueCapacity.
func (e *InvalidQueueCapacityError) Is(target error) bool {
	return target == ErrInvalidQueueCapacity
}

// InvalidSkipListHeightError provides structured context when a SkipList node height violates bounds.
// It matches ErrInvalidSkipListHeight when interrogated with errors.Is().
type InvalidSkipListHeightError struct {
	Height    int
	MinHeight int
	MaxHeight int
}

func (e *InvalidSkipListHeightError) Error() string {
	if e == nil {
		return ErrInvalidSkipListHeight.Error()
	}
	return fmt.Sprintf("invalid skiplist node height %d: must be between %d and %d", e.Height, e.MinHeight, e.MaxHeight)
}

// Is reports whether this error matches target sentinel ErrInvalidSkipListHeight.
func (e *InvalidSkipListHeightError) Is(target error) bool {
	return target == ErrInvalidSkipListHeight
}

// InvalidSkipListLevelError provides structured context when accessing a SkipList forward level outside bounds.
// It matches ErrInvalidSkipListLevel when interrogated with errors.Is().
type InvalidSkipListLevelError struct {
	Level    int
	MaxLevel int
}

func (e *InvalidSkipListLevelError) Error() string {
	if e == nil {
		return ErrInvalidSkipListLevel.Error()
	}
	return fmt.Sprintf("invalid skiplist level %d: must be between 0 and %d", e.Level, e.MaxLevel)
}

// Is reports whether this error matches target sentinel ErrInvalidSkipListLevel.
func (e *InvalidSkipListLevelError) Is(target error) bool {
	return target == ErrInvalidSkipListLevel
}

// InvalidBlockHandleError provides structured context when a block handle violates constraints.
// It matches ErrInvalidBlockHandle when interrogated with errors.Is().
type InvalidBlockHandleError struct {
	Offset uint64
	Size   uint64
	Reason string
}

func (e *InvalidBlockHandleError) Error() string {
	if e == nil {
		return ErrInvalidBlockHandle.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid block handle [offset=%d, size=%d]: %s", e.Offset, e.Size, e.Reason)
	}
	return fmt.Sprintf("invalid block handle [offset=%d, size=%d]", e.Offset, e.Size)
}

// Is reports whether this error matches target sentinel ErrInvalidBlockHandle.
func (e *InvalidBlockHandleError) Is(target error) bool {
	return target == ErrInvalidBlockHandle
}

// IndexBlockCorruptedError provides structured context when an index block fails integrity or layout checks.
// It matches ErrIndexBlockCorrupted when interrogated with errors.Is().
type IndexBlockCorruptedError struct {
	Reason string
}

func (e *IndexBlockCorruptedError) Error() string {
	if e == nil {
		return ErrIndexBlockCorrupted.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("index block corrupted: %s", e.Reason)
	}
	return ErrIndexBlockCorrupted.Error()
}

// Is reports whether this error matches target sentinel ErrIndexBlockCorrupted.
func (e *IndexBlockCorruptedError) Is(target error) bool {
	return target == ErrIndexBlockCorrupted
}

// FilterBlockCorruptedError provides structured context when a filter block fails integrity or layout checks.
// It matches ErrFilterBlockCorrupted when interrogated with errors.Is().
type FilterBlockCorruptedError struct {
	Reason string
}

func (e *FilterBlockCorruptedError) Error() string {
	if e == nil {
		return ErrFilterBlockCorrupted.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("filter block corrupted: %s", e.Reason)
	}
	return ErrFilterBlockCorrupted.Error()
}

// Is reports whether this error matches target sentinel ErrFilterBlockCorrupted.
func (e *FilterBlockCorruptedError) Is(target error) bool {
	return target == ErrFilterBlockCorrupted
}

// InvalidFooterMagicError provides structured context when an SSTable footer magic number check fails.
// It matches ErrInvalidFooterMagic and ErrInvalidFooter when interrogated with errors.Is().
type InvalidFooterMagicError struct {
	Expected uint64
	Actual   uint64
}

func (e *InvalidFooterMagicError) Error() string {
	if e == nil {
		return ErrInvalidFooterMagic.Error()
	}
	return fmt.Sprintf("invalid sstable footer magic: expected 0x%016x, got 0x%016x", e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinels ErrInvalidFooterMagic or ErrInvalidFooter.
func (e *InvalidFooterMagicError) Is(target error) bool {
	return target == ErrInvalidFooterMagic || target == ErrInvalidFooter
}

// InvalidFooterPaddingError provides structured context when an SSTable footer padding check fails.
// It matches ErrInvalidFooterPadding and ErrInvalidFooter when interrogated with errors.Is().
type InvalidFooterPaddingError struct {
	Padding [8]byte
}

func (e *InvalidFooterPaddingError) Error() string {
	if e == nil {
		return ErrInvalidFooterPadding.Error()
	}
	return fmt.Sprintf("invalid sstable footer padding: expected all zeros, got 0x%x", e.Padding[:])
}

// Is reports whether this error matches target sentinels ErrInvalidFooterPadding or ErrInvalidFooter.
func (e *InvalidFooterPaddingError) Is(target error) bool {
	return target == ErrInvalidFooterPadding || target == ErrInvalidFooter
}

// InvalidFooterSizeError provides structured context when an SSTable footer buffer length is invalid.
// It matches ErrInvalidFooterSize, ErrFooterTruncated, and ErrInvalidFooter when interrogated with errors.Is().
type InvalidFooterSizeError struct {
	Expected int
	Actual   int
}

func (e *InvalidFooterSizeError) Error() string {
	if e == nil {
		return ErrInvalidFooterSize.Error()
	}
	if e.Actual < e.Expected {
		return fmt.Sprintf("sstable footer truncated: expected %d bytes, got %d bytes", e.Expected, e.Actual)
	}
	return fmt.Sprintf("invalid sstable footer buffer size: expected %d bytes, got %d bytes", e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinels ErrInvalidFooterSize, ErrFooterTruncated, or ErrInvalidFooter.
func (e *InvalidFooterSizeError) Is(target error) bool {
	if target == ErrInvalidFooter {
		return true
	}
	if e != nil && e.Actual < e.Expected && target == ErrFooterTruncated {
		return true
	}
	return target == ErrInvalidFooterSize
}

// DataBlockCorruptedError provides structured diagnostics when an SSTable data block fails integrity or layout checks.
// It matches ErrDataBlockCorrupted when interrogated with errors.Is().
type DataBlockCorruptedError struct {
	Offset uint64
	Reason string
}

func (e *DataBlockCorruptedError) Error() string {
	if e == nil {
		return ErrDataBlockCorrupted.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("data block corrupted at offset %d: %s", e.Offset, e.Reason)
	}
	return fmt.Sprintf("data block corrupted at offset %d", e.Offset)
}

// Is reports whether this error matches target sentinel ErrDataBlockCorrupted.
func (e *DataBlockCorruptedError) Is(target error) bool {
	return target == ErrDataBlockCorrupted
}

// InsecureFileModeError provides structured diagnostics when a file mode grants insecure permissions.
// It matches ErrInsecureFileMode when interrogated with errors.Is().
type InsecureFileModeError struct {
	Mode fs.FileMode
}

func (e *InsecureFileModeError) Error() string {
	if e == nil {
		return ErrInsecureFileMode.Error()
	}
	return fmt.Sprintf("insecure file mode %04o: permissions must not grant group/other access or execute bits", e.Mode.Perm())
}

// Is reports whether this error matches target sentinel ErrInsecureFileMode.
func (e *InsecureFileModeError) Is(target error) bool {
	return target == ErrInsecureFileMode
}

// WALWriterPoisonedError provides structured context when an operation is rejected on a poisoned WAL writer.
// It matches ErrWriterPoisoned when interrogated with errors.Is().
type WALWriterPoisonedError struct {
	Path   string
	Reason error
}

func (e *WALWriterPoisonedError) Error() string {
	if e == nil {
		return ErrWriterPoisoned.Error()
	}
	if e.Reason != nil {
		return fmt.Sprintf("wal writer at %q is poisoned: %v", e.Path, e.Reason)
	}
	return fmt.Sprintf("wal writer at %q is poisoned", e.Path)
}

// Is reports whether this error matches target sentinel ErrWriterPoisoned.
func (e *WALWriterPoisonedError) Is(target error) bool {
	return target == ErrWriterPoisoned
}

// Unwrap returns the underlying error that caused the writer to be poisoned.
func (e *WALWriterPoisonedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Reason
}

// InvalidLevelError provides structured context when an LSM-tree level index violates architectural bounds.
// It matches ErrInvalidLevel when interrogated with errors.Is().
type InvalidLevelError struct {
	Level    uint32
	MaxLevel uint32
}

func (e *InvalidLevelError) Error() string {
	if e == nil {
		return ErrInvalidLevel.Error()
	}
	return fmt.Sprintf("invalid level %d: must be in range [0, %d]", e.Level, e.MaxLevel)
}

// Is reports whether this error matches target sentinel ErrInvalidLevel.
func (e *InvalidLevelError) Is(target error) bool {
	return target == ErrInvalidLevel
}

// CorruptedVersionEditError provides structured diagnostics when a serialized VersionEdit is corrupted.
// It matches ErrCorruptedVersionEdit when interrogated with errors.Is().
type CorruptedVersionEditError struct {
	Offset int64
	Reason string
}

func (e *CorruptedVersionEditError) Error() string {
	if e == nil {
		return ErrCorruptedVersionEdit.Error()
	}
	if e.Offset >= 0 {
		return fmt.Sprintf("corrupted version edit at offset %d: %s", e.Offset, e.Reason)
	}
	return fmt.Sprintf("corrupted version edit: %s", e.Reason)
}

// Is reports whether this error matches target sentinel ErrCorruptedVersionEdit.
func (e *CorruptedVersionEditError) Is(target error) bool {
	return target == ErrCorruptedVersionEdit
}

// TruncatedVersionEditError provides structured context when a VersionEdit buffer ends prematurely.
// It matches ErrTruncatedVersionEdit when interrogated with errors.Is().
type TruncatedVersionEditError struct {
	Expected int
	Actual   int
}

func (e *TruncatedVersionEditError) Error() string {
	if e == nil {
		return ErrTruncatedVersionEdit.Error()
	}
	return fmt.Sprintf("version edit buffer truncated: expected at least %d bytes, got %d", e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinel ErrTruncatedVersionEdit.
func (e *TruncatedVersionEditError) Is(target error) bool {
	return target == ErrTruncatedVersionEdit
}

// UnsupportedVersionEditError provides structured context when a VersionEdit specifies an unknown format version.
// It matches ErrUnsupportedVersionEdit when interrogated with errors.Is().
type UnsupportedVersionEditError struct {
	Version byte
}

func (e *UnsupportedVersionEditError) Error() string {
	if e == nil {
		return ErrUnsupportedVersionEdit.Error()
	}
	return fmt.Sprintf("unsupported version edit format version: 0x%02x", e.Version)
}

// Is reports whether this error matches target sentinel ErrUnsupportedVersionEdit.
func (e *UnsupportedVersionEditError) Is(target error) bool {
	return target == ErrUnsupportedVersionEdit
}

// DuplicateScalarFieldError provides structured context when a scalar field is duplicated in a VersionEdit record.
// It matches ErrDuplicateScalarField when interrogated with errors.Is().
type DuplicateScalarFieldError struct {
	Tag       uint64
	FieldName string
}

func (e *DuplicateScalarFieldError) Error() string {
	if e == nil {
		return ErrDuplicateScalarField.Error()
	}
	if e.FieldName != "" {
		return fmt.Sprintf("duplicate scalar field %q (tag %d) in version edit", e.FieldName, e.Tag)
	}
	return fmt.Sprintf("duplicate scalar field with tag %d in version edit", e.Tag)
}

// Is reports whether this error matches target sentinel ErrDuplicateScalarField.
func (e *DuplicateScalarFieldError) Is(target error) bool {
	return target == ErrDuplicateScalarField
}

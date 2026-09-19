package errors

import (
	stdErrors "errors"
	"fmt"
	"io/fs"
	"path/filepath"
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

	// ErrInvalidCompactionPlan indicates that a generated or requested compaction plan
	// violates structural, level, key-range, or file-uniqueness invariants.
	ErrInvalidCompactionPlan = stdErrors.New("invalid compaction plan")

	// ErrByteCountOverflow indicates that accumulating file sizes or resource bounds
	// exceeded 64-bit unsigned integer capacity (math.MaxUint64).
	ErrByteCountOverflow = stdErrors.New("byte count overflow")

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

	// ErrInvalidCacheCapacity indicates that a block cache capacity was negative.
	ErrInvalidCacheCapacity = stdErrors.New("cache capacity cannot be negative")

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

	// ErrMemTableFull indicates that an insert was rejected because the MemTable
	// would exceed MaxMemTableSize. Caller should freeze/flush to disk.
	ErrMemTableFull = stdErrors.New("memtable is full")

	// ErrMemoryLimitExceeded indicates that a write was rejected or timed out because total
	// engine memory reached the hard ceiling (SEC-003 backpressure enforcement).
	ErrMemoryLimitExceeded = stdErrors.New("engine memory limit exceeded: write backpressure triggered")

	// ErrWriteThrottled indicates that an operation was rejected due to backpressure rate limits.
	ErrWriteThrottled = stdErrors.New("engine write throttled under backpressure")

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

	// ErrParentDirectorySymlink indicates that the SSTable parent directory (or an intermediate path component) is a symlink.
	ErrParentDirectorySymlink = stdErrors.New("parent directory is a symlink")

	// ErrParentDirectorySwapped indicates that the SSTable parent directory was replaced or redirected to a different object.
	ErrParentDirectorySwapped = stdErrors.New("parent directory swapped or replaced")

	// ErrSSTableSymlink indicates that an SSTable file path is a symbolic link.
	ErrSSTableSymlink = stdErrors.New("sstable file is a symlink")

	// ErrSSTableObjectChanged indicates that an SSTable file was swapped, replaced,
	// or modified to reference a different filesystem object during opening.
	ErrSSTableObjectChanged = stdErrors.New("sstable file object swapped or replaced")

	// ErrMissingSSTable indicates that an SSTable file referenced by the reconstructed
	// Version does not exist on disk during startup recovery.
	ErrMissingSSTable = stdErrors.New("referenced sstable file not found on disk")

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

	// ErrInvalidFileNum indicates that an SSTable file number was zero or invalid (must be >= 1).
	ErrInvalidFileNum = stdErrors.New("invalid file number: must be greater than zero")

	// ErrInvalidFileSize indicates that an SSTable file size was zero or invalid (must be >= 1).
	ErrInvalidFileSize = stdErrors.New("invalid file size: must be greater than zero")

	// ErrInvalidKeyRange indicates that an SSTable's SmallestKey sorts after its LargestKey.
	ErrInvalidKeyRange = stdErrors.New("invalid key range: smallest key sorts after largest key")

	// ErrInvalidSeqNumRange indicates that an SSTable's SmallestSeqNum is greater than its LargestSeqNum.
	ErrInvalidSeqNumRange = stdErrors.New("invalid sequence number range: smallest sequence number greater than largest sequence number")

	// ErrCorruptedVersionEdit indicates that a serialized VersionEdit record failed structural or boundary validation.
	ErrCorruptedVersionEdit = stdErrors.New("version edit corrupted")

	// ErrTruncatedVersionEdit indicates that a buffer ended prematurely while decoding a VersionEdit record.
	ErrTruncatedVersionEdit = stdErrors.New("version edit truncated")

	// ErrUnsupportedVersionEdit indicates that a VersionEdit specifies an unrecognized format version byte.
	ErrUnsupportedVersionEdit = stdErrors.New("unsupported version edit format")

	// ErrDuplicateScalarField indicates that a scalar field was encountered more than once in a single VersionEdit record.
	ErrDuplicateScalarField = stdErrors.New("duplicate scalar field in version edit")

	// ErrManifestWriterClosed indicates that an append or sync operation was attempted on a closed MANIFEST writer.
	ErrManifestWriterClosed = stdErrors.New("manifest writer is closed")

	// ErrManifestWriterPoisoned indicates that an operation was attempted on a MANIFEST writer
	// that entered an unrecoverable poisoned state following a write or sync failure.
	ErrManifestWriterPoisoned = stdErrors.New("manifest writer is poisoned")

	// ErrManifestCorrupted indicates that a MANIFEST record failed checksum verification or framing constraints.
	ErrManifestCorrupted = stdErrors.New("manifest record corrupted")

	// ErrManifestTruncated indicates that a MANIFEST record or header was truncated or incomplete.
	ErrManifestTruncated = stdErrors.New("manifest record truncated")

	// ErrManifestExists indicates that a MANIFEST file already exists at the target path and cannot be overwritten.
	ErrManifestExists = stdErrors.New("manifest file already exists")

	// ErrManifestNotFound indicates that the referenced active MANIFEST file does not exist on disk.
	ErrManifestNotFound = stdErrors.New("manifest file not found")

	// ErrManifestHeaderTruncated indicates that a MANIFEST record header buffer is shorter than 8 bytes.
	ErrManifestHeaderTruncated = stdErrors.New("manifest header truncated: buffer smaller than 8 bytes")

	// ErrManifestPayloadTruncated indicates that a MANIFEST record payload ended prematurely before its specified length.
	ErrManifestPayloadTruncated = stdErrors.New("manifest payload truncated: buffer smaller than payload length")

	// ErrInvalidManifestNum indicates that a manifest sequence number was zero or invalid (must be >= 1).
	ErrInvalidManifestNum = stdErrors.New("invalid manifest number: must be greater than zero")

	// ErrCurrentSymlink indicates that a CURRENT or CURRENT.tmp operation was rejected because the path is a symbolic link.
	ErrCurrentSymlink = stdErrors.New("cannot write CURRENT over symbolic link")

	// ErrCurrentDirectorySync indicates that hardware synchronization of the parent directory failed following a CURRENT rename.
	ErrCurrentDirectorySync = stdErrors.New("failed to synchronize parent directory for CURRENT")

	// ErrCurrentNotFound indicates that the CURRENT pointer file does not exist in the database directory.
	ErrCurrentNotFound = stdErrors.New("CURRENT file not found")

	// ErrCurrentCorrupted indicates that the CURRENT pointer file content is malformed, truncated, oversized, or non-canonical.
	ErrCurrentCorrupted = stdErrors.New("CURRENT file corrupted")

	// ErrNilVersion indicates that a nil Version pointer was passed to VersionSet.
	ErrNilVersion = stdErrors.New("version cannot be nil")

	// ErrNilVersionEdit indicates that a nil VersionEdit pointer was passed to VersionSet.
	ErrNilVersionEdit = stdErrors.New("version edit cannot be nil")

	// ErrDeadVersion indicates that an operation was attempted on a Version whose reference count has reached zero.
	ErrDeadVersion = stdErrors.New("cannot operate on dead version: reference count is zero")

	// ErrVersionAlreadyAppended indicates that a Version was attempted to be appended to a VersionSet when it already belongs to one.
	ErrVersionAlreadyAppended = stdErrors.New("version already belongs to a VersionSet")

	// ErrRecoveryInProgress indicates that startup crash recovery is actively executing
	// and concurrent mutations or second recovery invocations are prohibited.
	ErrRecoveryInProgress = stdErrors.New("recovery is already in progress")

	// ErrRecoveryAlreadyComplete indicates that startup crash recovery has already completed
	// and cannot be re-executed on an active engine instance.
	ErrRecoveryAlreadyComplete = stdErrors.New("recovery has already completed")

	// ErrRecoveryInvalidState indicates that recovery was attempted on an engine that already
	// contains live uncommitted in-memory mutations or prior active state.
	ErrRecoveryInvalidState = stdErrors.New("cannot recover engine with active uncommitted state")

	// ErrManifestReplayLimit indicates that MANIFEST replay exceeded global resource bounds
	// (replayed bytes, record count, or live file count).
	ErrManifestReplayLimit = stdErrors.New("manifest replay resource limit exceeded")

	// ErrSSTableSizeMismatch indicates that the physical size of an SSTable on disk does not match
	// the authoritative FileSize recorded in the MANIFEST metadata.
	ErrSSTableSizeMismatch = stdErrors.New("sstable physical size does not match manifest metadata")

	// ErrRecoveryBatchLimitExceeded indicates that a single WAL batch during recovery exceeded
	// the maximum allowed record count or memory byte budget.
	ErrRecoveryBatchLimitExceeded = stdErrors.New("recovery batch limit exceeded")

	// ErrInvalidMagic indicates that a network frame header magic number does not match 0x4C415454 ("LATT").
	ErrInvalidMagic = stdErrors.New("invalid protocol magic: expected 0x4C415454")

	// ErrFrameTooLarge indicates that a network frame's declared payload length exceeds the maximum ceiling (5 MB).
	ErrFrameTooLarge = stdErrors.New("frame payload exceeds maximum allowed size")

	// ErrInvalidOpCode indicates that a network frame specifies an unrecognized or unsupported operation code.
	ErrInvalidOpCode = stdErrors.New("invalid operation code")

	// ErrFrameTruncated indicates that a network frame ended prematurely before its declared header, payload, or trailer.
	ErrFrameTruncated = stdErrors.New("frame truncated or incomplete")

	// ErrInvalidPayload indicates that a network frame payload violates structural or length constraints for its operation.
	ErrInvalidPayload = stdErrors.New("invalid frame payload")

	// ErrInvalidStatus indicates that a network response specifies an unrecognized or invalid status code.
	ErrInvalidStatus = stdErrors.New("invalid response status code")

	// ErrServerClosed indicates that the network server has been closed or is shutting down.
	ErrServerClosed = stdErrors.New("server is closed")

	// ErrServerAlreadyStarted indicates that Start or Listen was called on a server that is already active.
	ErrServerAlreadyStarted = stdErrors.New("server already started")

	// ErrConnectionLimitExceeded indicates that an inbound connection was rejected because active connections reached capacity.
	ErrConnectionLimitExceeded = stdErrors.New("connection limit exceeded")

	// ErrInsecureTransport indicates that cleartext TCP is prohibited on a non-loopback address without InsecureTransport opt-in.
	ErrInsecureTransport = stdErrors.New("plaintext TCP forbidden on non-loopback address without InsecureTransport opt-in")

	// ErrInvalidNodeID indicates that a cluster node identifier is 0 or unparseable.
	ErrInvalidNodeID = stdErrors.New("invalid node ID: must be greater than zero")

	// ErrDuplicateNodeID indicates that multiple peers in a cluster topology share the same numeric node ID.
	ErrDuplicateNodeID = stdErrors.New("duplicate node ID in cluster topology")

	// ErrDuplicatePeerAddress indicates that multiple peers in a cluster topology share the same network address.
	ErrDuplicatePeerAddress = stdErrors.New("duplicate peer address in cluster topology")

	// ErrInvalidPeerAddress indicates that a peer network address is malformed or has an invalid port.
	ErrInvalidPeerAddress = stdErrors.New("invalid peer address: expected host:port")

	// ErrClusterTooLarge indicates that the number of configured peers exceeds the maximum permitted ceiling.
	ErrClusterTooLarge = stdErrors.New("cluster topology exceeds maximum allowed peers")

	// ErrSelfNotFound indicates that the local node ID was neither declared in cluster peers nor had a local address provided.
	ErrSelfNotFound = stdErrors.New("local node identity not found in cluster topology")

	// ErrSelfAddressMismatch indicates that the local peer address does not match the address declared for the local node in cluster peers.
	ErrSelfAddressMismatch = stdErrors.New("local peer address does not match address declared in cluster peers")

	// ErrWildcardAddress indicates that a wildcard IP (e.g. 0.0.0.0, ::) was provided where a specific peer target is required.
	ErrWildcardAddress = stdErrors.New("wildcard IP address forbidden as peer target")

	// ErrEmptyTopology indicates that a cluster topology contains zero nodes.
	ErrEmptyTopology = stdErrors.New("cluster topology must contain at least one node")

	// ErrInvalidPeerMessage indicates that a peer message opcode is unknown or unrecognized.
	ErrInvalidPeerMessage = stdErrors.New("invalid or unrecognized peer message type")

	// ErrInvalidPeerPayload indicates that a peer message payload is malformed, truncated, or contains trailing bytes.
	ErrInvalidPeerPayload = stdErrors.New("invalid peer message payload")

	// ErrInvalidPeerEntry indicates that an entry in an AppendEntries payload is malformed or invalid.
	ErrInvalidPeerEntry = stdErrors.New("invalid peer log entry")

	// ErrInvalidPeerBoolean indicates that a boolean field in a peer message has an invalid wire value (not 0x00 or 0x01).
	ErrInvalidPeerBoolean = stdErrors.New("invalid peer boolean wire value: must be 0 or 1")

	// ErrManagerClosed indicates that an operation was attempted on a closed peer connection manager.
	ErrManagerClosed = stdErrors.New("peer connection manager is closed")

	// ErrManagerNotStarted indicates that an operation was attempted before the peer connection manager was started.
	ErrManagerNotStarted = stdErrors.New("peer connection manager not started")

	// ErrManagerAlreadyStarted indicates that Start was called on an already started peer connection manager.
	ErrManagerAlreadyStarted = stdErrors.New("peer connection manager already started")

	// ErrPeerNotFound indicates that the target node ID does not exist in the cluster topology.
	ErrPeerNotFound = stdErrors.New("peer node not found in topology")

	// ErrPeerUnavailable indicates that the target peer connection is disconnected, reconnecting, or closing.
	ErrPeerUnavailable = stdErrors.New("peer is currently disconnected or unavailable")

	// ErrInvalidManagerConfig indicates that peer connection manager configuration parameters violate required bounds.
	ErrInvalidManagerConfig = stdErrors.New("invalid peer connection manager configuration")

	// ErrReplayedFrame indicates that an incoming peer frame was rejected as a duplicate or replayed message.
	ErrReplayedFrame = stdErrors.New("peer message rejected: duplicate or replayed frame")
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

// InvalidCacheCapacityError provides structured context when a block cache is initialized with a negative capacity.
// It matches ErrInvalidCacheCapacity when interrogated with errors.Is().
type InvalidCacheCapacityError struct {
	Capacity int
}

func (e *InvalidCacheCapacityError) Error() string {
	if e == nil {
		return ErrInvalidCacheCapacity.Error()
	}
	return fmt.Sprintf("invalid cache capacity %d: cannot be negative", e.Capacity)
}

// Is reports whether this error matches target sentinel ErrInvalidCacheCapacity.
func (e *InvalidCacheCapacityError) Is(target error) bool {
	return target == ErrInvalidCacheCapacity
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
	name := filepath.Base(e.Path)
	if name == "" || name == "." {
		name = "wal segment"
	}
	if e.Reason != nil {
		return fmt.Sprintf("wal writer for %q is poisoned: %v", name, e.Reason)
	}
	return fmt.Sprintf("wal writer for %q is poisoned", name)
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

// ManifestWriterPoisonedError provides structured context when an operation is rejected on a poisoned MANIFEST writer.
// It matches ErrManifestWriterPoisoned when interrogated with errors.Is().
type ManifestWriterPoisonedError struct {
	Path   string
	Reason error
}

func (e *ManifestWriterPoisonedError) Error() string {
	if e == nil {
		return ErrManifestWriterPoisoned.Error()
	}
	name := filepath.Base(e.Path)
	if name == "" || name == "." {
		name = "manifest"
	}
	if e.Reason != nil {
		return fmt.Sprintf("manifest writer for %q is poisoned: %v", name, e.Reason)
	}
	return fmt.Sprintf("manifest writer for %q is poisoned", name)
}

// Is reports whether this error matches target sentinel ErrManifestWriterPoisoned.
func (e *ManifestWriterPoisonedError) Is(target error) bool {
	return target == ErrManifestWriterPoisoned
}

// Unwrap returns the underlying error that caused the writer to be poisoned.
func (e *ManifestWriterPoisonedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Reason
}

// ManifestCorruptedError provides structured diagnostics when a MANIFEST record fails framing or checksum checks.
// It matches ErrManifestCorrupted when interrogated with errors.Is().
type ManifestCorruptedError struct {
	Offset int64
	Reason string
}

func (e *ManifestCorruptedError) Error() string {
	if e == nil {
		return ErrManifestCorrupted.Error()
	}
	if e.Offset >= 0 {
		return fmt.Sprintf("manifest record corrupted at offset %d: %s", e.Offset, e.Reason)
	}
	return fmt.Sprintf("manifest record corrupted: %s", e.Reason)
}

// Is reports whether this error matches target sentinel ErrManifestCorrupted.
func (e *ManifestCorruptedError) Is(target error) bool {
	return target == ErrManifestCorrupted
}

// MemTableFullError provides structured context when an insert exceeds MaxMemTableSize.
// It matches ErrMemTableFull when interrogated with errors.Is().
type MemTableFullError struct {
	Current uint64
	Needed  uint64
	Max     uint64
}

func (e *MemTableFullError) Error() string {
	if e == nil {
		return ErrMemTableFull.Error()
	}
	return fmt.Sprintf("memtable full: current %d + needed %d exceeds max %d", e.Current, e.Needed, e.Max)
}

// Is reports whether this error matches target sentinel ErrMemTableFull.
func (e *MemTableFullError) Is(target error) bool {
	return target == ErrMemTableFull
}

// ManifestReplayLimitError provides structured context when MANIFEST replay exceeds resource limits.
// It matches ErrManifestReplayLimit when interrogated with errors.Is().
type ManifestReplayLimitError struct {
	Resource string // "bytes", "records", or "live_files"
	Current  uint64
	Limit    uint64
	Offset   int64
	Record   int
}

func (e *ManifestReplayLimitError) Error() string {
	if e == nil {
		return ErrManifestReplayLimit.Error()
	}
	return fmt.Sprintf("manifest replay limit exceeded for %s: current %d exceeds limit %d at record %d (offset %d)",
		e.Resource, e.Current, e.Limit, e.Record, e.Offset)
}

// Is reports whether this error matches target sentinel ErrManifestReplayLimit.
func (e *ManifestReplayLimitError) Is(target error) bool {
	return target == ErrManifestReplayLimit
}

// SSTableSizeMismatchError provides structured context when an SSTable's physical size on disk
// differs from the authoritative FileSize stored in manifest metadata.
// It matches ErrSSTableSizeMismatch when interrogated with errors.Is().
type SSTableSizeMismatchError struct {
	Path     string
	FileNum  uint64
	Expected uint64
	Actual   int64
}

func (e *SSTableSizeMismatchError) Error() string {
	if e == nil {
		return ErrSSTableSizeMismatch.Error()
	}
	name := filepath.Base(e.Path)
	if name == "" || name == "." {
		name = "sstable"
	}
	return fmt.Sprintf("sstable physical size mismatch for %s (file %d): expected %d bytes, got %d bytes on disk",
		name, e.FileNum, e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinel ErrSSTableSizeMismatch.
func (e *SSTableSizeMismatchError) Is(target error) bool {
	return target == ErrSSTableSizeMismatch
}

// RecoveryBatchLimitError provides structured context when a WAL batch during recovery exceeds
// resource bounds (record count or cumulative byte size).
// It matches ErrRecoveryBatchLimitExceeded when interrogated with errors.Is().
type RecoveryBatchLimitError struct {
	LimitType string // "records" or "bytes"
	Limit     uint64
	Actual    uint64
}

func (e *RecoveryBatchLimitError) Error() string {
	if e == nil {
		return ErrRecoveryBatchLimitExceeded.Error()
	}
	return fmt.Sprintf("recovery batch limit exceeded for %s: actual %d exceeds limit %d",
		e.LimitType, e.Actual, e.Limit)
}

// Is reports whether this error matches target sentinel ErrRecoveryBatchLimitExceeded.
func (e *RecoveryBatchLimitError) Is(target error) bool {
	return target == ErrRecoveryBatchLimitExceeded
}

// InvalidMagicError provides structured context when a network frame magic check fails.
// It matches ErrInvalidMagic when interrogated with errors.Is().
type InvalidMagicError struct {
	Expected uint32
	Actual   uint32
}

func (e *InvalidMagicError) Error() string {
	if e == nil {
		return ErrInvalidMagic.Error()
	}
	return fmt.Sprintf("invalid protocol magic: expected 0x%08x, got 0x%08x", e.Expected, e.Actual)
}

// Is reports whether this error matches target sentinel ErrInvalidMagic.
func (e *InvalidMagicError) Is(target error) bool {
	return target == ErrInvalidMagic
}

// FrameTooLargeError provides structured context when a frame payload exceeds the ceiling limit.
// It matches ErrFrameTooLarge when interrogated with errors.Is().
type FrameTooLargeError struct {
	PayloadSize uint32
	MaxSize     uint32
}

func (e *FrameTooLargeError) Error() string {
	if e == nil {
		return ErrFrameTooLarge.Error()
	}
	return fmt.Sprintf("frame payload size %d bytes exceeds maximum allowed size of %d bytes", e.PayloadSize, e.MaxSize)
}

// Is reports whether this error matches target sentinel ErrFrameTooLarge.
func (e *FrameTooLargeError) Is(target error) bool {
	return target == ErrFrameTooLarge
}

// InvalidOpCodeError provides structured context when an unrecognized operation code is encountered.
// It matches ErrInvalidOpCode when interrogated with errors.Is().
type InvalidOpCodeError struct {
	OpCode byte
}

func (e *InvalidOpCodeError) Error() string {
	if e == nil {
		return ErrInvalidOpCode.Error()
	}
	return fmt.Sprintf("invalid operation code: 0x%02x", e.OpCode)
}

// Is reports whether this error matches target sentinel ErrInvalidOpCode.
func (e *InvalidOpCodeError) Is(target error) bool {
	return target == ErrInvalidOpCode
}

// InvalidPayloadError provides structured context when a frame payload violates operation constraints.
// It matches ErrInvalidPayload when interrogated with errors.Is().
type InvalidPayloadError struct {
	Reason string
}

func (e *InvalidPayloadError) Error() string {
	if e == nil {
		return ErrInvalidPayload.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid frame payload: %s", e.Reason)
	}
	return ErrInvalidPayload.Error()
}

// Is reports whether this error matches target sentinel ErrInvalidPayload.
func (e *InvalidPayloadError) Is(target error) bool {
	return target == ErrInvalidPayload
}

// InvalidStatusError provides structured context when an unrecognized response status code is encountered.
// It matches ErrInvalidStatus when interrogated with errors.Is().
type InvalidStatusError struct {
	Status byte
}

func (e *InvalidStatusError) Error() string {
	if e == nil {
		return ErrInvalidStatus.Error()
	}
	return fmt.Sprintf("invalid response status code: 0x%02x", e.Status)
}

// Is reports whether this error matches target sentinel ErrInvalidStatus.
func (e *InvalidStatusError) Is(target error) bool {
	return target == ErrInvalidStatus
}

// InvalidNodeIDError provides structured context when a cluster node ID is invalid or zero.
type InvalidNodeIDError struct {
	NodeID uint64
	Reason string
}

func (e *InvalidNodeIDError) Error() string {
	if e == nil {
		return ErrInvalidNodeID.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid node ID %d: %s", e.NodeID, e.Reason)
	}
	return fmt.Sprintf("invalid node ID %d: must be greater than zero", e.NodeID)
}

func (e *InvalidNodeIDError) Is(target error) bool {
	return target == ErrInvalidNodeID
}

// DuplicateNodeIDError provides structured context when multiple peers declare the same node ID.
type DuplicateNodeIDError struct {
	NodeID uint64
	Addr1  string
	Addr2  string
}

func (e *DuplicateNodeIDError) Error() string {
	if e == nil {
		return ErrDuplicateNodeID.Error()
	}
	if e.Addr1 != "" && e.Addr2 != "" {
		return fmt.Sprintf("duplicate node ID %d in cluster topology (%s and %s)", e.NodeID, e.Addr1, e.Addr2)
	}
	return fmt.Sprintf("duplicate node ID %d in cluster topology", e.NodeID)
}

func (e *DuplicateNodeIDError) Is(target error) bool {
	return target == ErrDuplicateNodeID
}

// DuplicatePeerAddressError provides structured context when multiple peers declare the same address.
type DuplicatePeerAddressError struct {
	Address string
	Node1   uint64
	Node2   uint64
}

func (e *DuplicatePeerAddressError) Error() string {
	if e == nil {
		return ErrDuplicatePeerAddress.Error()
	}
	if e.Node1 != 0 && e.Node2 != 0 {
		return fmt.Sprintf("duplicate peer address %s declared for node %d and node %d", e.Address, e.Node1, e.Node2)
	}
	return fmt.Sprintf("duplicate peer address %s in cluster topology", e.Address)
}

func (e *DuplicatePeerAddressError) Is(target error) bool {
	return target == ErrDuplicatePeerAddress
}

// InvalidPeerAddressError provides structured context when a peer endpoint is malformed.
type InvalidPeerAddressError struct {
	Address string
	Reason  string
}

func (e *InvalidPeerAddressError) Error() string {
	if e == nil {
		return ErrInvalidPeerAddress.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid peer address %q: %s", e.Address, e.Reason)
	}
	return fmt.Sprintf("invalid peer address %q", e.Address)
}

func (e *InvalidPeerAddressError) Is(target error) bool {
	return target == ErrInvalidPeerAddress
}

// ClusterTooLargeError provides structured context when cluster peer count exceeds ceiling.
type ClusterTooLargeError struct {
	Count int
	Max   int
}

func (e *ClusterTooLargeError) Error() string {
	if e == nil {
		return ErrClusterTooLarge.Error()
	}
	return fmt.Sprintf("cluster topology peer count %d exceeds maximum limit of %d", e.Count, e.Max)
}

func (e *ClusterTooLargeError) Is(target error) bool {
	return target == ErrClusterTooLarge
}

// InvalidPeerMessageError provides structured context when an unrecognized peer message opcode is encountered.
type InvalidPeerMessageError struct {
	OpCode byte
}

func (e *InvalidPeerMessageError) Error() string {
	if e == nil {
		return ErrInvalidPeerMessage.Error()
	}
	return fmt.Sprintf("invalid peer message type: 0x%02x", e.OpCode)
}

func (e *InvalidPeerMessageError) Is(target error) bool {
	return target == ErrInvalidPeerMessage
}

// InvalidPeerPayloadError provides structured context when a peer message payload is malformed.
type InvalidPeerPayloadError struct {
	Reason string
}

func (e *InvalidPeerPayloadError) Error() string {
	if e == nil {
		return ErrInvalidPeerPayload.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid peer message payload: %s", e.Reason)
	}
	return ErrInvalidPeerPayload.Error()
}

func (e *InvalidPeerPayloadError) Is(target error) bool {
	return target == ErrInvalidPeerPayload
}

// InvalidPeerBooleanError provides structured context when a boolean field in a peer message has an invalid wire value.
type InvalidPeerBooleanError struct {
	Field string
	Value byte
}

func (e *InvalidPeerBooleanError) Error() string {
	if e == nil {
		return ErrInvalidPeerBoolean.Error()
	}
	if e.Field != "" {
		return fmt.Sprintf("invalid peer boolean for field %s: wire value 0x%02x (must be 0 or 1)", e.Field, e.Value)
	}
	return fmt.Sprintf("invalid peer boolean: wire value 0x%02x (must be 0 or 1)", e.Value)
}

func (e *InvalidPeerBooleanError) Is(target error) bool {
	return target == ErrInvalidPeerBoolean
}

// InvalidPeerEntryError provides structured context when an entry in an AppendEntries payload is malformed.
type InvalidPeerEntryError struct {
	Index  int
	Reason string
}

func (e *InvalidPeerEntryError) Error() string {
	if e == nil {
		return ErrInvalidPeerEntry.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("invalid peer log entry at index %d: %s", e.Index, e.Reason)
	}
	return fmt.Sprintf("invalid peer log entry at index %d", e.Index)
}

func (e *InvalidPeerEntryError) Is(target error) bool {
	return target == ErrInvalidPeerEntry
}

// PeerUnavailableError provides structured context when an RPC cannot be sent because a peer is not in connected state.
type PeerUnavailableError struct {
	NodeID uint64
	State  string
}

func (e *PeerUnavailableError) Error() string {
	if e == nil {
		return ErrPeerUnavailable.Error()
	}
	if e.State != "" {
		return fmt.Sprintf("peer %d is unavailable: current state %s", e.NodeID, e.State)
	}
	return fmt.Sprintf("peer %d is unavailable", e.NodeID)
}

func (e *PeerUnavailableError) Is(target error) bool {
	return target == ErrPeerUnavailable
}

// UnknownPeerError provides structured context when an operation targets a peer not registered in topology.
type UnknownPeerError struct {
	NodeID uint64
}

func (e *UnknownPeerError) Error() string {
	if e == nil {
		return ErrPeerNotFound.Error()
	}
	return fmt.Sprintf("peer %d not found in cluster topology", e.NodeID)
}

func (e *UnknownPeerError) Is(target error) bool {
	return target == ErrPeerNotFound
}

// ReplayedFrameError provides structured context when an incoming peer message is rejected by the replay filter.
type ReplayedFrameError struct {
	NodeID uint64
	SeqID  uint64
	Nonce  uint64
	Reason string
}

func (e *ReplayedFrameError) Error() string {
	if e == nil {
		return ErrReplayedFrame.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("peer %d frame rejected (seq=%d, nonce=%d): %s", e.NodeID, e.SeqID, e.Nonce, e.Reason)
	}
	return fmt.Sprintf("peer %d frame rejected: duplicate or replayed frame (seq=%d, nonce=%d)", e.NodeID, e.SeqID, e.Nonce)
}

func (e *ReplayedFrameError) Is(target error) bool {
	return target == ErrReplayedFrame
}

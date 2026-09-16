package sstable

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
)

var (
	openFileFn     = func(name string) (*os.File, error) { return os.Open(name) }
	postOpenHookFn func() error
	postOpenHookMu sync.Mutex
)

// BlockCache defines the minimal block cache interface required by TableReader for SSTable data block caching.
// It is satisfied directly by *cache.ShardedCache and *cache.LRUShard.
type BlockCache interface {
	Get(key cache.BlockKey) ([]byte, bool)
	Put(key cache.BlockKey, val []byte)
}

// TableReaderOptions configures optional settings for TableReader, including
// block caching and explicit physical SSTable file identification.
type TableReaderOptions struct {
	// FileNum is the physical SSTable sequential file number (e.g. 1 for "000001.sst").
	// If FileNum is 0, NewTableReaderWithOptions attempts to parse FileNum from the filename.
	//
	// Cache identity: cached blocks are keyed by (FileNum, Offset). FileNum 0
	// disables block caching for that reader (reads go directly to disk) because
	// 0 is the unassigned sentinel and cannot distinguish distinct files.
	// When sharing one BlockCache across readers, every reader MUST use a
	// distinct non-zero FileNum; two different files using the same FileNum
	// would return each other's cached blocks.
	FileNum uint64

	// BlockCache is the optional BlockCache (e.g. *cache.ShardedCache) to use for data blocks.
	// If nil (or a typed-nil *cache.ShardedCache/*cache.LRUShard), caching is
	// disabled and all reads go directly to disk.
	BlockCache BlockCache
}

// isNilBlockCache reports whether a BlockCache interface holds no usable cache,
// covering both untyped nil and typed-nil pointers (e.g. (*cache.ShardedCache)(nil)).
func isNilBlockCache(c BlockCache) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// TableReader provides point-lookup access to an immutable, finalized SSTable file.
// Upon opening, the reader reads the fixed 48-byte footer, validates its format
// magic and padding, decodes the sparse block index, and retains the index in RAM.
//
// Lookups execute via a two-level binary search:
//  1. In-memory binary search over the sparse BlockIndex to locate the candidate data block.
//  2. Bounded disk read of the candidate block, CRC32 validation, binary search over
//     restart points, and prefix-compressed forward decoding to find the target user key.
//
// Concurrency:
// TableReader is safe for concurrent Seek and ReadBlock operations across multiple goroutines.
// Disk reads use positional ReadAt, ensuring no mutable file offset state is shared.
// ReadBlock and Seek hold the reader's shared read lock for the duration of the
// operation (including cache access and, on miss, disk I/O and validation), so a
// concurrent Close or SetFileNum/SetBlockCache waits for in-flight reads.
// BlockCache accesses then take only the targeted shard mutex; there is no global
// cache lock on the hot path and no lock-order inversion (reader -> shard only).
// Close acquires an exclusive write lock to safely release file resources.
type TableReader struct {
	mu         sync.RWMutex
	file       *os.File
	fileSize   int64
	footer     Footer
	index      *BlockIndex
	readAtFn   func(p []byte, off int64) (int, error)
	closed     bool
	fileNum    uint64
	blockCache BlockCache
}

// parseTableFilename attempts to extract the 6-digit numeric file number from an SSTable filename (e.g. "000042.sst").
// Returns (0, false) if the filename does not strictly conform to the "%06d.sst" pattern.
func parseTableFilename(name string) (uint64, bool) {
	if len(name) != 10 || !strings.HasSuffix(name, ".sst") {
		return 0, false
	}
	base := name[:6]
	for i := 0; i < 6; i++ {
		if base[i] < '0' || base[i] > '9' {
			return 0, false
		}
	}
	num, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, false
	}
	if fmt.Sprintf("%06d.sst", num) != name {
		return 0, false
	}
	return num, true
}

// NewTableReader opens an SSTable file at path and initializes a TableReader with default options.
func NewTableReader(path string) (*TableReader, error) {
	return NewTableReaderWithOptions(path, TableReaderOptions{})
}

// OpenTableReader is an alias for NewTableReader following idiomatic Go naming conventions.
func OpenTableReader(path string) (*TableReader, error) {
	return NewTableReader(path)
}

// NewTableReaderWithOptions opens an SSTable file at path with the provided options.
//
// Security & TOCTOU Hardening (SEC-007 / F-007):
//  1. Pre-open inspection: Inspects path with os.Lstat (without following symlinks),
//     verifying that the target is an existing regular file, not a directory, and not a symlink.
//  2. Intermediate path validation: validatePathNoSymlinks inspects all parent and ancestor path
//     components down to filepath.Dir(path), rejecting unpermitted symlink redirection.
//  3. Secure file open: Opens the descriptor via openFileFn (os.Open). A defer block guarantees
//     immediate descriptor cleanup if subsequent post-open validation fails.
//  4. Descriptor-based validation: Validates stat via file.Stat() (fstat on the opened descriptor),
//     ensuring the opened object is a regular file and not a directory.
//  5. Post-open pathname re-inspection: Re-inspects path via os.Lstat to ensure the pathname was
//     not swapped with a symlink during or immediately after the open operation.
//  6. Inode pinning & object identity invariance: Verifies that both os.SameFile(fstat, lstatBefore)
//     and os.SameFile(fstat, lstatAfter) hold. This eliminates TOCTOU substitution races by ensuring
//     the opened file descriptor refers to the exact same filesystem inode observed at the path before
//     and after opening.
//  7. Re-validation of intermediate components: Re-verifies ancestor components post-open.
//  8. Descriptor-centric lifecycle: Once validated, the opened *os.File descriptor is passed to
//     NewTableReaderWithFileAndOptions.
func NewTableReaderWithOptions(path string, opts TableReaderOptions) (*TableReader, error) {
	if path == "" {
		return nil, os.ErrInvalid
	}

	if opts.FileNum == 0 {
		if num, ok := parseTableFilename(filepath.Base(path)); ok {
			opts.FileNum = num
		}
	}

	// 1. Pre-open inspection of destination path without following symlinks
	lstatBefore, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if lstatBefore.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: sstable file %q is a symlink", errors.ErrSSTableSymlink, path)
	}
	if lstatBefore.IsDir() {
		return nil, fmt.Errorf("%w: sstable path %q is not a regular file (directory)", errors.ErrNotADirectory, path)
	}
	if !lstatBefore.Mode().IsRegular() {
		return nil, fmt.Errorf("sstable: %q is not a regular file (mode: %s)", path, lstatBefore.Mode())
	}

	// 2. Validate intermediate path components
	dir := filepath.Dir(path)
	if err := validatePathNoSymlinks(dir); err != nil {
		return nil, err
	}

	// 3. Open file descriptor
	file, err := openFileFn(path)
	if err != nil {
		return nil, err
	}

	// Guaranteed descriptor cleanup if subsequent post-open validation fails
	var success bool
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()

	// 4. Test seam hook for race / substitution injection
	postOpenHookMu.Lock()
	hook := postOpenHookFn
	postOpenHookMu.Unlock()
	if hook != nil {
		if err := hook(); err != nil {
			return nil, err
		}
	}

	// 5. Inspect the opened descriptor directly (fstat)
	fstat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat opened sstable descriptor: %w", err)
	}
	if fstat.IsDir() {
		return nil, fmt.Errorf("%w: sstable path %q is not a regular file (directory)", errors.ErrNotADirectory, path)
	}
	if !fstat.Mode().IsRegular() {
		return nil, fmt.Errorf("sstable: %q is not a regular file (mode: %s)", path, fstat.Mode())
	}

	// 6. Post-open pathname re-inspection without following symlinks
	lstatAfter, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: sstable path %q could not be statted post-open: %w", errors.ErrSSTableObjectChanged, path, err)
	}
	if lstatAfter.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: sstable file %q was replaced with a symlink", errors.ErrSSTableSymlink, path)
	}

	// 7. Inode pinning & object identity invariance
	if !os.SameFile(fstat, lstatBefore) || !os.SameFile(fstat, lstatAfter) {
		return nil, fmt.Errorf("%w: sstable file %q object identity mismatch", errors.ErrSSTableObjectChanged, path)
	}

	// 8. Re-validate intermediate components
	if err := validatePathNoSymlinks(dir); err != nil {
		return nil, err
	}

	// Hand over ownership of open file descriptor to NewTableReaderWithFileAndOptions
	success = true
	return NewTableReaderWithFileAndOptions(file, opts)
}

// NewTableReaderWithFile initializes a TableReader wrapping an existing open *os.File with default options.
func NewTableReaderWithFile(file *os.File) (*TableReader, error) {
	return NewTableReaderWithFileAndOptions(file, TableReaderOptions{})
}

// NewTableReaderWithFileAndOptions initializes a TableReader wrapping an existing open *os.File with provided options.
// The reader assumes ownership of the file descriptor; calling Close on the reader
// will close the provided file.
//
// Security Contract:
// If initialization fails on any early return path (stat failure, truncated file, corrupt footer,
// oversized index handle, corrupted index), the provided file descriptor is guaranteed to be closed,
// preventing descriptor leaks.
func NewTableReaderWithFileAndOptions(file *os.File, opts TableReaderOptions) (*TableReader, error) {
	if file == nil {
		return nil, errors.ErrNilReceiver
	}

	if opts.FileNum == 0 {
		if num, ok := parseTableFilename(filepath.Base(file.Name())); ok {
			opts.FileNum = num
		}
	}

	var success bool
	defer func() {
		if !success {
			_ = file.Close()
		}
	}()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat sstable file: %w", err)
	}
	if stat.IsDir() {
		return nil, fmt.Errorf("%w: sstable %q is not a regular file (directory)", errors.ErrNotADirectory, file.Name())
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("sstable: %q is not a regular file (mode: %s)", file.Name(), stat.Mode())
	}

	fileSize := stat.Size()
	if fileSize < int64(FooterSize) {
		return nil, &errors.InvalidFooterSizeError{
			Expected: FooterSize,
			Actual:   int(fileSize),
		}
	}

	// 1. Read fixed 48-byte footer from tail of file: [fileSize - 48 : fileSize]
	var footerBuf [FooterSize]byte
	footerOffset := fileSize - int64(FooterSize)
	if err := readExactAt(file.ReadAt, footerBuf[:], footerOffset); err != nil {
		return nil, fmt.Errorf("failed to read footer: %w", err)
	}

	// 2. Decode and validate footer structure, magic number, and padding
	footer, err := DecodeFooter(footerBuf[:])
	if err != nil {
		return nil, err
	}

	// 3. Validate block handles against physical file size
	if err := footer.ValidateAgainstFileSize(fileSize); err != nil {
		return nil, err
	}

	// 4. Read sparse index block from disk
	indexHandle := footer.IndexHandle
	if indexHandle.Size == 0 || indexHandle.Size > MaxIndexBlockSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: indexHandle.Offset,
			Size:   indexHandle.Size,
			Reason: "index block size exceeds MaxIndexBlockSize or is zero",
		}
	}
	if indexHandle.Offset > math.MaxUint64-indexHandle.Size || indexHandle.Offset+indexHandle.Size > uint64(fileSize)-FooterSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: indexHandle.Offset,
			Size:   indexHandle.Size,
			Reason: "index handle extends into footer, exceeds physical file boundary, or overflows address space",
		}
	}
	if indexHandle.Size > math.MaxInt || indexHandle.Offset > math.MaxInt64 {
		return nil, &errors.InvalidBlockHandleError{
			Offset: indexHandle.Offset,
			Size:   indexHandle.Size,
			Reason: "index handle offset or size exceeds architecture integer bounds",
		}
	}
	indexBuf := make([]byte, int(indexHandle.Size))
	if err := readExactAt(file.ReadAt, indexBuf, int64(indexHandle.Offset)); err != nil {
		return nil, fmt.Errorf("failed to read index block at offset %d: %w", indexHandle.Offset, err)
	}

	// 5. Decode index block and retain in RAM
	blockIndex, err := DecodeBlockIndex(indexBuf)
	if err != nil {
		return nil, err
	}

	success = true
	return &TableReader{
		file:       file,
		fileSize:   fileSize,
		footer:     footer,
		index:      blockIndex,
		readAtFn:   file.ReadAt,
		fileNum:    opts.FileNum,
		blockCache: opts.BlockCache,
	}, nil
}

// FileNum returns the physical SSTable file number associated with this reader.
func (r *TableReader) FileNum() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fileNum
}

// SetFileNum updates the physical SSTable file number associated with this reader.
//
// Changing the identity of a live reader orphans entries cached under the old
// (FileNum, Offset) keys (they remain until evicted but are unreachable via the
// new identity) and requires the new FileNum to be unique among all readers
// sharing the cache. Setting FileNum to 0 disables block caching for
// subsequent reads. Prefer immutable identity set at construction.
func (r *TableReader) SetFileNum(fileNum uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fileNum = fileNum
}

// BlockCache returns the configured BlockCache, or nil if block caching is disabled.
// A typed-nil cache (e.g. (*cache.ShardedCache)(nil)) is reported as nil.
func (r *TableReader) BlockCache() BlockCache {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if isNilBlockCache(r.blockCache) {
		return nil
	}
	return r.blockCache
}

// cacheEnabledLocked reports whether block caching is usable for the current
// reader state. Caching requires a non-nil cache AND a non-zero FileNum, so
// ambiguous identity (FileNum 0) never reads or populates shared cache entries.
// Caller must hold at least the read lock.
func (r *TableReader) cacheEnabledLocked() bool {
	return r.fileNum != 0 && !isNilBlockCache(r.blockCache)
}

// SetBlockCache updates the BlockCache for this reader. If nil, block caching is disabled.
func (r *TableReader) SetBlockCache(c BlockCache) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockCache = c
}

// FileSize returns the total physical byte size of the underlying SSTable file.
func (r *TableReader) FileSize() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.fileSize
}

// Footer returns a defensive copy of the parsed SSTable Footer.
func (r *TableReader) Footer() Footer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.footer
}

// Index returns the in-memory sparse BlockIndex.
func (r *TableReader) Index() *BlockIndex {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.index
}

// Close releases the underlying file descriptor and marks the reader as closed.
// Subsequent operations on this reader will return errors.ErrTableReaderClosed.
// Calling Close on an already-closed reader is a no-op and returns nil.
func (r *TableReader) Close() error {
	if r == nil {
		return errors.ErrNilReceiver
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}
	r.closed = true

	if r.file != nil {
		return r.file.Close()
	}
	return nil
}

// NewIterator creates an unpositioned sequential streaming TableIterator over this SSTable.
// The caller retains ownership of the TableReader; calling Close on the returned TableIterator
// does not close the underlying TableReader.
func (r *TableReader) NewIterator() (*TableIterator, error) {
	return NewTableIterator(r)
}

// validateBlockHandle verifies that candidate data block handle parameters conform to
// MaxDataBlockSize, 64-bit integer address limits, and physical file boundaries.
func (r *TableReader) validateBlockHandle(handle BlockHandle) error {
	if handle.Size == 0 || handle.Size > MaxDataBlockSize {
		return &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "data block size exceeds MaxDataBlockSize or is zero",
		}
	}
	if handle.Offset > math.MaxUint64-handle.Size || handle.Offset+handle.Size > uint64(r.fileSize)-FooterSize {
		return &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "candidate data block extends into footer, exceeds physical file boundary, or overflows address space",
		}
	}
	if handle.Size > math.MaxInt || handle.Offset > math.MaxInt64 {
		return &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "candidate data block offset or size exceeds architecture integer bounds",
		}
	}
	return nil
}

// ReadBlock reads and returns the validated bytes of a data block referenced by handle.
//
// Read Flow:
//  1. Validates handle bounds against MaxDataBlockSize, 64-bit integer limits, and physical file boundaries.
//     Malformed handles are rejected immediately before any cache lookup or disk I/O.
//  2. If block caching is enabled (non-nil cache AND non-zero FileNum), checks the
//     cache for (fileNum, handle.Offset).
//     On a cache hit with matching size, returns a defensive copy from cache
//     immediately, bypassing disk I/O and checksum calculation.
//  3. On a cache miss (or if cache is disabled):
//     - Reads handle.Size bytes from disk via positional ReadAt.
//     - Validates minimum block trailer length (>= 8 bytes).
//     - Validates CRC32-IEEE checksum over [entry data || restart offsets || restart count].
//     - Validates restart count, restart array boundaries, and full restart offset
//     - structure (first offset zero, strictly increasing, within entry bounds).
//     - If corrupted, returns an explicit error and NEVER inserts corrupted bytes into the cache.
//     - If valid and caching is enabled, inserts the validated block into the cache.
//  4. Returns the validated block buffer.
func (r *TableReader) ReadBlock(handle BlockHandle) ([]byte, error) {
	if r == nil {
		return nil, errors.ErrNilReceiver
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, errors.ErrTableReaderClosed
	}
	return r.readBlockLocked(handle)
}

// validateBlockRestartOffsets verifies full restart-offset structure: first
// offset zero, strictly increasing, and strictly within the entry-data region.
// blockBuf must already have passed trailer length, CRC, count, and array
// bounds checks.
func validateBlockRestartOffsets(blockBuf []byte, blockOffset uint64) error {
	restartCount := binary.GetUint32(blockBuf[len(blockBuf)-8 : len(blockBuf)-4])
	restartBytes := uint64(restartCount) * 4
	// Caller guarantees restartBytes+8 <= len(blockBuf); re-check defensively.
	if restartBytes+8 > uint64(len(blockBuf)) {
		return &errors.DataBlockCorruptedError{
			Offset: blockOffset,
			Reason: "data block restart array exceeds block size",
		}
	}
	entryDataEnd := len(blockBuf) - 8 - int(restartBytes)
	restartOffsetsStart := entryDataEnd
	var prev uint32
	for i := 0; i < int(restartCount); i++ {
		offPos := restartOffsetsStart + i*4
		off := binary.GetUint32(blockBuf[offPos : offPos+4])
		if i == 0 && off != 0 {
			return &errors.DataBlockCorruptedError{
				Offset: blockOffset,
				Reason: "first restart offset must be zero",
			}
		}
		if i > 0 && off <= prev {
			return &errors.DataBlockCorruptedError{
				Offset: blockOffset,
				Reason: "restart offsets are not strictly increasing",
			}
		}
		if uint64(off) >= uint64(entryDataEnd) {
			return &errors.DataBlockCorruptedError{
				Offset: blockOffset,
				Reason: "restart offset exceeds entry data boundary",
			}
		}
		prev = off
	}
	return nil
}

// readBlockLocked performs the handle validation, cache lookup, disk read, integrity validation,
// and cache insertion under shared read lock.
func (r *TableReader) readBlockLocked(handle BlockHandle) ([]byte, error) {
	// 1. Validate block handle bounds strictly BEFORE consulting cache
	if err := r.validateBlockHandle(handle); err != nil {
		return nil, err
	}

	cacheEnabled := r.cacheEnabledLocked()
	var key cache.BlockKey
	if cacheEnabled {
		// 2. Check BlockCache if enabled (non-zero FileNum, usable cache).
		key = cache.NewBlockKey(r.fileNum, handle.Offset)
		if val, ok := r.blockCache.Get(key); ok {
			// Ensure cached entry matches expected handle size
			if uint64(len(val)) == handle.Size {
				return val, nil
			}
		}
	}

	// 3. Read raw block bytes from disk via positional readAt
	blockBuf := make([]byte, int(handle.Size))
	if err := readExactAt(r.readAtFn, blockBuf, int64(handle.Offset)); err != nil {
		return nil, fmt.Errorf("failed to read data block at offset %d: %w", handle.Offset, err)
	}

	// 4. Verify block integrity before trusting or caching
	if len(blockBuf) < 8 {
		return nil, &errors.DataBlockCorruptedError{
			Offset: handle.Offset,
			Reason: "data block buffer smaller than minimum trailer length",
		}
	}

	// 4a. Verify CRC32-IEEE checksum
	expectedCRC := binary.GetUint32(blockBuf[len(blockBuf)-4:])
	actualCRC := binary.Checksum(blockBuf[:len(blockBuf)-4])
	if expectedCRC != actualCRC {
		var offInt64 int64
		if handle.Offset <= math.MaxInt64 {
			offInt64 = int64(handle.Offset) // #nosec G115
		}
		return nil, &errors.ChecksumMismatchError{
			Offset:   offInt64,
			Expected: expectedCRC,
			Actual:   actualCRC,
		}
	}

	// 4b. Verify restart count and restart array bounds
	restartCount := binary.GetUint32(blockBuf[len(blockBuf)-8 : len(blockBuf)-4])
	if restartCount == 0 {
		return nil, &errors.DataBlockCorruptedError{
			Offset: handle.Offset,
			Reason: "data block restart count is zero",
		}
	}
	if restartCount > MaxRestartCount {
		return nil, &errors.DataBlockCorruptedError{
			Offset: handle.Offset,
			Reason: fmt.Sprintf("data block restart count %d exceeds MaxRestartCount %d", restartCount, MaxRestartCount),
		}
	}
	restartBytes := uint64(restartCount) * 4
	if restartBytes+8 > uint64(len(blockBuf)) {
		return nil, &errors.DataBlockCorruptedError{
			Offset: handle.Offset,
			Reason: "data block restart array exceeds block size",
		}
	}

	// 4c. Verify full restart offset structure before trusting or caching, so
	// ReadBlock never caches a block that Seek would reject.
	if err := validateBlockRestartOffsets(blockBuf, handle.Offset); err != nil {
		return nil, err
	}

	// 5. Insert valid, verified block into cache
	if cacheEnabled {
		r.blockCache.Put(key, blockBuf)
	}

	return blockBuf, nil
}

// ReadDataBlock reads and returns the raw uncompressed bytes of a data block referenced by handle.
// Validates handle bounds against MaxDataBlockSize, 64-bit address bounds, physical file boundaries,
// and architecture integer limits.
// Delegates to ReadBlock to utilize the BlockCache when configured.
func (r *TableReader) ReadDataBlock(handle BlockHandle) ([]byte, error) {
	return r.ReadBlock(handle)
}

// Seek performs a point lookup for the given user key in the SSTable.
//
// Lookup Flow:
//  1. Validates user key constraints (1..65,535 bytes).
//  2. Acquires shared read lock (concurrent-safe).
//  3. Uses the in-memory sparse index binary search to locate the candidate data block.
//     If targetKey > all largest keys in the index, returns errors.ErrKeyNotFound with zero disk I/O.
//  4. Validates the data block handle against physical file bounds.
//  5. Reads candidate block from BlockCache or disk via readBlockLocked.
//  6. Binary-searches restart points to locate the nearest restart interval.
//  7. Scans prefix-compressed entries forward, reconstructing full InternalKeys.
//  8. If a match is found:
//     - If OpType == OpTypePut: returns an owned defensive copy of the value bytes and nil.
//     - If OpType == OpTypeDelete: returns nil and errors.ErrKeyNotFound (tombstone).
//  9. If the key does not exist or scanning passes the key, returns nil and errors.ErrKeyNotFound.
func (r *TableReader) Seek(userKey []byte) ([]byte, error) {
	if r == nil {
		return nil, errors.ErrNilReceiver
	}
	if err := binary.ValidateKey(userKey); err != nil {
		return nil, err
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.closed {
		return nil, errors.ErrTableReaderClosed
	}

	// 1. In-memory sparse index binary search
	handle, found := r.index.FindBlock(userKey)
	if !found {
		return nil, errors.ErrKeyNotFound
	}

	// 2. Validate bounds and read candidate data block from cache or disk
	blockBuf, err := r.readBlockLocked(handle)
	if err != nil {
		return nil, err
	}

	// 3. Decode and search prefix-compressed data block (already validated)
	return searchDataBlockVerified(blockBuf, userKey, handle.Offset)
}

// searchDataBlock validates block integrity, binary-searches restart points, decodes
// prefix-compressed records forward, and locates the target user key.
func searchDataBlock(blockBuf []byte, targetUserKey []byte, blockOffset uint64) ([]byte, error) {
	// Minimum data block trailer: RestartCount (4 bytes) + CRC32 (4 bytes) = 8 bytes
	if len(blockBuf) < 8 {
		return nil, &errors.DataBlockCorruptedError{
			Offset: blockOffset,
			Reason: "data block buffer smaller than minimum trailer length",
		}
	}

	// 1. Verify CRC32-IEEE checksum over [entry data || restart offsets || restart count]
	expectedCRC := binary.GetUint32(blockBuf[len(blockBuf)-4:])
	actualCRC := binary.Checksum(blockBuf[:len(blockBuf)-4])
	if expectedCRC != actualCRC {
		var offInt64 int64
		if blockOffset <= math.MaxInt64 {
			offInt64 = int64(blockOffset) // #nosec G115
		}
		return nil, &errors.ChecksumMismatchError{
			Offset:   offInt64,
			Expected: expectedCRC,
			Actual:   actualCRC,
		}
	}

	return searchDataBlockVerified(blockBuf, targetUserKey, blockOffset)
}

// searchDataBlockVerified searches an already CRC-verified data block.
func searchDataBlockVerified(blockBuf []byte, targetUserKey []byte, blockOffset uint64) ([]byte, error) {

	// 2. Parse and validate restart count
	restartCount := binary.GetUint32(blockBuf[len(blockBuf)-8 : len(blockBuf)-4])
	if restartCount == 0 {
		return nil, &errors.DataBlockCorruptedError{
			Offset: blockOffset,
			Reason: "data block restart count is zero",
		}
	}
	if restartCount > MaxRestartCount {
		return nil, &errors.DataBlockCorruptedError{
			Offset: blockOffset,
			Reason: fmt.Sprintf("data block restart count %d exceeds MaxRestartCount %d", restartCount, MaxRestartCount),
		}
	}

	// 3. Verify restart array bounds
	restartBytes := uint64(restartCount) * 4
	if restartBytes+8 > uint64(len(blockBuf)) {
		return nil, &errors.DataBlockCorruptedError{
			Offset: blockOffset,
			Reason: "data block restart array exceeds block size",
		}
	}
	entryDataEnd := len(blockBuf) - 8 - int(restartBytes)

	// 4. Parse and validate restart offsets
	restartOffsetsStart := entryDataEnd
	restartOffsets := make([]uint32, restartCount)
	for i := 0; i < int(restartCount); i++ {
		offPos := restartOffsetsStart + i*4
		off := binary.GetUint32(blockBuf[offPos : offPos+4])

		if i == 0 && off != 0 {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset,
				Reason: "first restart offset must be zero",
			}
		}
		if i > 0 && off <= restartOffsets[i-1] {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset,
				Reason: "restart offsets are not strictly increasing",
			}
		}
		if uint64(off) >= uint64(entryDataEnd) {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset,
				Reason: "restart offset exceeds entry data boundary",
			}
		}
		restartOffsets[i] = off
	}

	// 5. Helper to decode the user key at a restart point
	decodeRestartUserKey := func(restartIdx int) ([]byte, error) {
		off := int(restartOffsets[restartIdx])
		entrySlice := blockBuf[off:entryDataEnd]

		shared, n1, err := binary.GetVarint64Canonical(entrySlice)
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "varint shared key length corrupted",
			}
		}
		if shared != 0 {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "shared key length at restart point must be zero",
			}
		}

		unshared, n2, err := binary.GetVarint64Canonical(entrySlice[n1:])
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off) + uint64(n1),
				Reason: "varint unshared key length corrupted",
			}
		}

		valueLen, n3, err := binary.GetVarint64Canonical(entrySlice[n1+n2:])
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off) + uint64(n1+n2),
				Reason: "varint value length corrupted",
			}
		}

		if unshared < binary.MinKeyLen+binary.InternalKeyTrailerLen || unshared > binary.MaxEncodedInternalKeyLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "restart entry key length outside valid internal key boundaries",
			}
		}
		if valueLen > binary.MaxValueLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "restart entry value length exceeds MaxValueLen",
			}
		}

		hdrLen := n1 + n2 + n3
		totalPayload := uint64(hdrLen) + unshared + valueLen
		if totalPayload > uint64(len(entrySlice)) {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "restart entry payload exceeds entry data boundary",
			}
		}

		// Decode the InternalKey directly to strictly validate user key, seqnum, and optype
		keyBytes := entrySlice[hdrLen : hdrLen+int(unshared)]
		ik, err := binary.DecodeInternalKey(keyBytes)
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: fmt.Sprintf("invalid internal key at restart point: %v", err),
			}
		}

		return ik.UserKey, nil
	}

	// 6. Binary search over restart points to find the candidate restart interval
	left := 0
	right := int(restartCount) - 1
	for left < right {
		mid := (left + right + 1) / 2
		midUserKey, err := decodeRestartUserKey(mid)
		if err != nil {
			return nil, err
		}
		if bytes.Compare(midUserKey, targetUserKey) < 0 {
			left = mid
		} else {
			right = mid - 1
		}
	}

	// 7. Linear scan forward from restartOffsets[left]
	currOffset := int(restartOffsets[left])
	var reconstructedKey []byte
	var prevIK *binary.InternalKey

	for currOffset < entryDataEnd {
		entrySlice := blockBuf[currOffset:entryDataEnd]

		shared, n1, err := binary.GetVarint64Canonical(entrySlice)
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "varint shared key length corrupted",
			}
		}

		unshared, n2, err := binary.GetVarint64Canonical(entrySlice[n1:])
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset) + uint64(n1),
				Reason: "varint unshared key length corrupted",
			}
		}

		valueLen, n3, err := binary.GetVarint64Canonical(entrySlice[n1+n2:])
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset) + uint64(n1+n2),
				Reason: "varint value length corrupted",
			}
		}

		if unshared > binary.MaxEncodedInternalKeyLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "entry unshared key length exceeds MaxEncodedInternalKeyLen",
			}
		}
		if valueLen > binary.MaxValueLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "entry value length exceeds MaxValueLen",
			}
		}

		hdrLen := n1 + n2 + n3
		totalPayload := uint64(hdrLen) + unshared + valueLen
		if totalPayload > uint64(len(entrySlice)) {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "entry payload exceeds entry data boundary",
			}
		}

		if currOffset == int(restartOffsets[left]) && shared != 0 {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "shared key length at restart point must be zero",
			}
		}
		if shared > uint64(len(reconstructedKey)) {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "shared prefix length exceeds reconstructed key length",
			}
		}

		// Reconstruct key: retain shared prefix and append current delta
		deltaKey := entrySlice[hdrLen : hdrLen+int(unshared)]
		reconstructedKey = append(reconstructedKey[:shared], deltaKey...)

		if len(reconstructedKey) < binary.MinKeyLen+binary.InternalKeyTrailerLen || len(reconstructedKey) > binary.MaxEncodedInternalKeyLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "reconstructed key length outside valid internal key boundaries",
			}
		}

		// Decode and strictly validate reconstructed InternalKey
		currIK, err := binary.DecodeInternalKey(reconstructedKey)
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: fmt.Sprintf("invalid reconstructed internal key: %v", err),
			}
		}

		// Verify strict monotonic ordering within the data block
		if prevIK != nil {
			if binary.CompareInternalKey(*prevIK, currIK) >= 0 {
				return nil, &errors.DataBlockCorruptedError{
					Offset: blockOffset + uint64(currOffset),
					Reason: "entries violate strictly increasing canonical key ordering",
				}
			}
		}
		curr := currIK
		prevIK = &curr

		cmp := bytes.Compare(currIK.UserKey, targetUserKey)
		if cmp == 0 {
			// First encountered match is guaranteed to be the latest version (SeqNum DESC)
			if currIK.OpType == binary.OpTypeDelete {
				// Logically deleted tombstone
				return nil, errors.ErrKeyNotFound
			}
			valStart := hdrLen + int(unshared)
			valBytes := entrySlice[valStart : valStart+int(valueLen)]
			valCopy := make([]byte, len(valBytes))
			copy(valCopy, valBytes)
			return valCopy, nil
		}
		if cmp > 0 {
			// Ascending key order guarantees target key cannot appear later
			return nil, errors.ErrKeyNotFound
		}

		currOffset += hdrLen + int(unshared) + int(valueLen)
	}

	return nil, errors.ErrKeyNotFound
}

// readExactAt executes positional reads until buf is completely satisfied or an error is encountered.
func readExactAt(readAt func(p []byte, off int64) (int, error), buf []byte, offset int64) error {
	total := 0
	for total < len(buf) {
		n, err := readAt(buf[total:], offset+int64(total))
		total += n
		if err != nil {
			if total == len(buf) {
				return nil
			}
			if stdErrors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

// ReadFilterBlock reads, parses, and validates the BloomFilter referenced by the table's MetaIndex block.
//
// Return Contract:
//   - If the table contains no filter block (or MetaIndex is empty), returns (nil, nil).
//   - If the table contains a filter block, reads the block bytes from disk, validates its CRC32 checksum
//     and structural invariants via filter.DecodeFilterBlock, and returns the decoded *filter.BloomFilter.
//   - If the filter block is corrupted, truncated, or fails checksum verification, returns an explicit
//     error (fail-closed security).
//   - Zero side-effects: Does not modify the reader's point lookup paths.
func (r *TableReader) ReadFilterBlock() (*filter.BloomFilter, error) {
	if r == nil {
		return nil, errors.ErrNilReceiver
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, errors.ErrTableReaderClosed
	}

	metaHandle := r.footer.MetaIndexHandle
	if metaHandle.Size == 0 || metaHandle.Size == MetaIndexTrailerSize {
		// Empty MetaIndex block (entryCount = 0)
		return nil, nil
	}
	if metaHandle.Size > MaxIndexBlockSize || metaHandle.Size > MaxBlockSize || metaHandle.Size > math.MaxInt || metaHandle.Offset > math.MaxInt64 {
		return nil, &errors.InvalidBlockHandleError{
			Offset: metaHandle.Offset,
			Size:   metaHandle.Size,
			Reason: "metaindex handle size exceeds maximum block capacity or architecture integer bounds",
		}
	}

	// Read MetaIndex block from disk
	metaBuf := make([]byte, int(metaHandle.Size))
	if err := readExactAt(r.readAtFn, metaBuf, int64(metaHandle.Offset)); err != nil {
		return nil, fmt.Errorf("failed to read metaindex block at offset %d: %w", metaHandle.Offset, err)
	}

	// Resolve filter handle
	filterHandle, found, err := FindMetaIndexEntry(metaBuf, filter.FilterMetaKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	// Validate filter handle bounds against file size and metaindex boundary
	if err := filterHandle.Validate(); err != nil {
		return nil, err
	}
	maxFilterBlockSize := uint64(filter.MaxBitsetBytes + filter.FilterBlockTrailerSize)
	if filterHandle.Size > maxFilterBlockSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: filterHandle.Offset,
			Size:   filterHandle.Size,
			Reason: "filter block handle size exceeds maximum filter block capacity",
		}
	}
	if filterHandle.Size > math.MaxInt || filterHandle.Offset > math.MaxInt64 {
		return nil, &errors.InvalidBlockHandleError{
			Offset: filterHandle.Offset,
			Size:   filterHandle.Size,
			Reason: "filter block handle exceeds architecture integer bounds",
		}
	}
	if filterHandle.Offset > math.MaxUint64-filterHandle.Size || filterHandle.Offset+filterHandle.Size > r.footer.MetaIndexHandle.Offset {
		return nil, &errors.InvalidBlockHandleError{
			Offset: filterHandle.Offset,
			Size:   filterHandle.Size,
			Reason: "filter block handle overlaps or exceeds metaindex boundary",
		}
	}
	if filterHandle.Offset+filterHandle.Size > uint64(r.fileSize)-FooterSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: filterHandle.Offset,
			Size:   filterHandle.Size,
			Reason: "filter block handle exceeds physical file boundary",
		}
	}

	// Read Filter block from disk
	filterBuf := make([]byte, int(filterHandle.Size))
	if err := readExactAt(r.readAtFn, filterBuf, int64(filterHandle.Offset)); err != nil {
		return nil, fmt.Errorf("failed to read filter block at offset %d: %w", filterHandle.Offset, err)
	}

	// Decode and validate filter block
	return filter.DecodeFilterBlock(filterBuf)
}

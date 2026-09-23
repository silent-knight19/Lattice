package sstable

import (
	stdErrors "errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/security"
)

// writerState represents the lifecycle phase of an active TableWriter.
type writerState int

const (
	stateOpen writerState = iota
	stateFinalized
	stateClosed
	stateError
)

const (
	// DefaultFileMode is the secure default file permissions for SSTables (0600 - owner read/write only).
	DefaultFileMode os.FileMode = 0600
	// DefaultDirMode is the secure default directory permissions for SSTable storage (0700 - owner read/write/exec only).
	DefaultDirMode os.FileMode = 0700
)

// TableWriterOptions configures the construction and flushing behavior of an SSTable file.
type TableWriterOptions struct {
	// TargetBlockSize is the threshold size in bytes at which a data block is flushed to disk.
	// Default: 4096 bytes (TargetBlockSize).
	TargetBlockSize int

	// RestartInterval is the number of records between restart points in prefix compression.
	// Default: 16 (DefaultRestartInterval).
	RestartInterval int

	// FileMode is the permission mode for the created SSTable file.
	// Default: 0600 (DefaultFileMode).
	FileMode os.FileMode

	// FilterBuilder is an optional FilterBlockBuilder. If non-nil, user keys added to the SSTable
	// are recorded into the filter builder, and a serialized Filter Block is written and indexed
	// in the MetaIndex block under key "filter.bloom".
	FilterBuilder *filter.FilterBlockBuilder
}

// DefaultTableWriterOptions returns production defaults for TableWriterOptions.
func DefaultTableWriterOptions() TableWriterOptions {
	return TableWriterOptions{
		TargetBlockSize: TargetBlockSize,
		RestartInterval: DefaultRestartInterval,
		FileMode:        DefaultFileMode,
	}
}

// ValidateFileMode verifies that the requested file mode strictly adheres to the security baseline.
// In accordance with Threat 7 of docs/threat-model.md, SSTable files must be owner-only (0600 or 0400).
// Modes granting group/other permissions (mode & 0077 != 0), execution bits (mode & 0111 != 0),
// or lacking owner read (mode & 0400 == 0) are strictly rejected.
func ValidateFileMode(mode os.FileMode) error {
	perm := mode.Perm()
	if perm&0077 != 0 || perm&0111 != 0 || perm&0400 == 0 {
		return &errors.InsecureFileModeError{Mode: mode}
	}
	return nil
}

// SSTableMetadata encapsulates the immutable structural properties of a finalized SSTable file.
type SSTableMetadata struct {
	Path            string
	FileSize        uint64
	DataBlockCount  uint64
	EntryCount      uint64
	SmallestKey     []byte // encoded InternalKey
	LargestKey      []byte // encoded InternalKey
	SmallestSeqNum  uint64
	LargestSeqNum   uint64
	MetaIndexHandle BlockHandle
	IndexHandle     BlockHandle
	FilterHandle    BlockHandle
}

// Iterator defines the sequential record traversal interface accepted by TableWriter.Build.
// It is directly satisfied by memtable.Iterator and merge iterators.
type Iterator interface {
	Valid() bool
	Next() bool
	Key() binary.InternalKey
	Value() []byte
}

// TableWriter sequentially constructs an immutable SSTable file on persistent storage.
//
// Invariants & Operational Semantics:
//  1. Physical File Layout:
//     [Data Block 0] ... [Data Block N-1] [Meta Index Block] [Index Block] [48-Byte Footer]
//  2. Monotonic Ordering:
//     Records must be added in strictly increasing canonical order (UserKey ASC, SeqNum DESC, OpType DESC).
//  3. Block Boundary Flushes:
//     Data blocks are flushed when CurrentSizeEstimate >= TargetBlockSize.
//  4. Non-Overlapping Regions:
//     Every data block, meta-index block, index block, and footer occupies a disjoint physical byte range.
//  5. Atomic Staging & Durability:
//     When constructed with a path, writes stream to <path>.tmp. On Finish, the file is synced,
//     closed, and atomically renamed to <path>, followed by a directory sync.
//  6. Failure Cleanup:
//     If writing, syncing, or building fails, the staging file is unlinked, preventing orphaned corrupt files.
//  7. Concurrency:
//     TableWriter is single-threaded and not safe for concurrent use across multiple goroutines.
type TableWriter struct {
	mu   sync.Mutex
	opts TableWriterOptions

	dstPath string
	tmpPath string
	file    *os.File

	dataBlockBuilder *BlockBuilder
	indexBuilder     *IndexBuilder

	largestKeyInCurrentBlock []byte
	smallestKey              []byte
	largestKey               []byte
	smallestSeqNum           uint64
	largestSeqNum            uint64
	prevKey                  binary.InternalKey
	hasPrevKey               bool

	offset         uint64
	dataBlockCount uint64
	entryCount     uint64

	filterBuilder *filter.FilterBlockBuilder
	filterHandle  BlockHandle

	state writerState
	err   error

	parentDirFile *os.File
	parentDirStat os.FileInfo
	stagingStat   os.FileInfo

	// Test seams for deterministic fault injection
	writeFn       func(f *os.File, p []byte) (int, error)
	syncFn        func(f *os.File) error
	closeFn       func(f *os.File) error
	syncDirFn     func(dirPath string) error
	syncDirFileFn func(f *os.File) error
	linkFn        func(oldname, newname string) error
	preLinkHookFn func() error
}

// NewTableWriter initializes a TableWriter to write an SSTable to dstPath using a secure staging file.
// If an SSTable already exists at dstPath, it returns errors.ErrSSTableExists.
func NewTableWriter(dstPath string, opts TableWriterOptions) (*TableWriter, error) {
	cleanDst, err := security.CleanAndValidatePath(dstPath)
	if err != nil {
		return nil, fmt.Errorf("sstable: %w", err)
	}

	baseName := filepath.Base(cleanDst)
	if err := security.ValidateDatabaseFileName(baseName); err != nil {
		return nil, fmt.Errorf("sstable: %w", err)
	}

	// Reject overwriting an existing finalized SSTable or symlink
	if _, err := os.Lstat(cleanDst); err == nil {
		return nil, errors.ErrSSTableExists
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to check destination path %q: %w", cleanDst, err)
	}

	if opts.TargetBlockSize <= 0 {
		opts.TargetBlockSize = TargetBlockSize
	} else if opts.TargetBlockSize > MaxDataBlockSize {
		return nil, fmt.Errorf("sstable: target block size %d exceeds MaxDataBlockSize %d: %w", opts.TargetBlockSize, MaxDataBlockSize, os.ErrInvalid)
	}
	if opts.RestartInterval <= 0 {
		opts.RestartInterval = DefaultRestartInterval
	}
	if opts.FileMode == 0 {
		opts.FileMode = DefaultFileMode
	} else {
		if err := ValidateFileMode(opts.FileMode); err != nil {
			return nil, err
		}
	}

	parentDir := filepath.Dir(cleanDst)

	// Validate intermediate path components that already exist
	if err := validatePathNoSymlinks(parentDir); err != nil {
		return nil, err
	}

	// Check if parentDir already exists as a symlink or non-directory
	if fi, err := os.Lstat(parentDir); err == nil {
		if !isSystemSymlinkPrefix(parentDir) && fi.Mode()&os.ModeSymlink != 0 {
			return nil, errors.ErrParentDirectorySymlink
		}
		if !fi.IsDir() {
			return nil, &errors.NotADirectoryError{Path: parentDir, Mode: fi.Mode()}
		}
	}

	if err := os.MkdirAll(parentDir, DefaultDirMode); err != nil {
		return nil, err
	}

	// Inspect created/existing parent directory with Lstat (no symlink following)
	pLstat, err := os.Lstat(parentDir)
	if err != nil {
		return nil, err
	}
	if !isSystemSymlinkPrefix(parentDir) && pLstat.Mode()&os.ModeSymlink != 0 {
		return nil, errors.ErrParentDirectorySymlink
	}
	if !pLstat.IsDir() {
		return nil, &errors.NotADirectoryError{Path: parentDir, Mode: pLstat.Mode()}
	}

	// Secure directory descriptor acquisition:
	// Open the parent directory handle and verify via os.SameFile that the opened descriptor
	// references the exact inode observed by Lstat (not an attacker-substituted symlink or file).
	parentFile, err := os.Open(parentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open parent directory %q: %w", parentDir, err)
	}
	parentStat, err := parentFile.Stat()
	if err != nil {
		_ = parentFile.Close()
		return nil, fmt.Errorf("failed to stat open parent directory %q: %w", parentDir, err)
	}
	if !parentStat.IsDir() {
		_ = parentFile.Close()
		return nil, &errors.NotADirectoryError{Path: parentDir, Mode: parentStat.Mode()}
	}
	if !os.SameFile(parentStat, pLstat) {
		_ = parentFile.Close()
		return nil, errors.ErrParentDirectorySwapped
	}

	// Tighten existing directory permissions to 0700 if group/others have permission bits (SEC-P04-005)
	if runtime.GOOS != "windows" && parentStat.Mode().Perm()&0077 != 0 {
		if err := parentFile.Chmod(DefaultDirMode); err != nil {
			_ = parentFile.Close()
			return nil, fmt.Errorf("failed to tighten parent directory permissions on %q: %w", parentDir, err)
		}
		if updatedStat, err := parentFile.Stat(); err == nil {
			parentStat = updatedStat
		}
	}

	// Secure staging file creation:
	// Use os.CreateTemp with O_CREATE|O_EXCL semantics in the same parent directory.
	// This prevents predictable staging collisions, symlink hijacking, and TOCTOU overwrites.
	file, err := os.CreateTemp(parentDir, fmt.Sprintf(".tmp_%s_*", filepath.Base(cleanDst)))
	if err != nil {
		_ = parentFile.Close()
		return nil, err
	}
	tmpPath := file.Name()

	// Verify that the created temporary file resides within the pinned parent directory
	tmpDir := filepath.Dir(tmpPath)
	tmpDirStat, err := os.Lstat(tmpDir)
	if err != nil || !os.SameFile(parentStat, tmpDirStat) {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, errors.ErrParentDirectorySwapped
	}

	// Ensure permissions match opts.FileMode if caller specified custom mode
	if opts.FileMode != DefaultFileMode {
		if err := file.Chmod(opts.FileMode); err != nil {
			_ = file.Close()
			_ = os.Remove(tmpPath)
			_ = parentFile.Close()
			return nil, err
		}
	}

	// Verify file descriptor refers to a regular file and not a symlink
	fi, err := file.Stat()
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, fmt.Errorf("staging path is not a regular file")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, &errors.InsecureFileModeError{Mode: fi.Mode().Perm()}
	}

	lfi, err := os.Lstat(tmpPath)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, err
	}
	if lfi.Mode()&os.ModeSymlink != 0 || !os.SameFile(fi, lfi) {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, fmt.Errorf("staging path symlink detected")
	}

	dataBuilder, err := NewBlockBuilderWithInterval(opts.RestartInterval)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		_ = parentFile.Close()
		return nil, err
	}

	return &TableWriter{
		opts:             opts,
		dstPath:          cleanDst,
		tmpPath:          tmpPath,
		file:             file,
		parentDirFile:    parentFile,
		parentDirStat:    parentStat,
		stagingStat:      fi,
		dataBlockBuilder: dataBuilder,
		indexBuilder:     NewIndexBuilder(),
		filterBuilder:    opts.FilterBuilder,
		smallestSeqNum:   math.MaxUint64,
		state:            stateOpen,
		writeFn:          defaultWrite,
		syncFn:           defaultSync,
		closeFn:          defaultClose,
		syncDirFn:        nil,
		syncDirFileFn:    syncDirFile,
		linkFn:           os.Link,
	}, nil
}

// NewTableWriterWithFile initializes a TableWriter writing directly to an open *os.File without staging rename.
// Useful for direct file testing and custom file descriptors.
//
// FIND-NEW-01: caller-provided descriptors bypass staging symlink checks, so
// validate the descriptor here: must stat to a regular file (rejects dirs,
// pipes, sockets, devices opened by mistake).
func NewTableWriterWithFile(file *os.File, opts TableWriterOptions) (*TableWriter, error) {
	if file == nil {
		return nil, errors.ErrNilReceiver
	}
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, &errors.NotADirectoryError{Path: file.Name(), Mode: fi.Mode()}
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("sstable: %q is not a regular file (mode: %s): %w", file.Name(), fi.Mode(), os.ErrInvalid)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0077 != 0 {
		return nil, &errors.InsecureFileModeError{Mode: fi.Mode().Perm()}
	}

	cleanPath := filepath.Clean(file.Name())
	if cleanPath != "" && cleanPath != "." && cleanPath != "/" {
		if lfi, err := os.Lstat(cleanPath); err == nil {
			if lfi.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("sstable: %q is a symlink: %w", cleanPath, os.ErrInvalid)
			}
			if !os.SameFile(fi, lfi) {
				return nil, fmt.Errorf("sstable: %q descriptor/lstat mismatch: %w", cleanPath, os.ErrInvalid)
			}
			parentDir := filepath.Dir(cleanPath)
			if err := validatePathNoSymlinks(parentDir); err != nil {
				return nil, err
			}
		}
	}

	if opts.TargetBlockSize <= 0 {
		opts.TargetBlockSize = TargetBlockSize
	} else if opts.TargetBlockSize > MaxDataBlockSize {
		return nil, fmt.Errorf("sstable: target block size %d exceeds MaxDataBlockSize %d: %w", opts.TargetBlockSize, MaxDataBlockSize, os.ErrInvalid)
	}
	if opts.RestartInterval <= 0 {
		opts.RestartInterval = DefaultRestartInterval
	}
	if opts.FileMode == 0 {
		opts.FileMode = DefaultFileMode
	} else {
		if err := ValidateFileMode(opts.FileMode); err != nil {
			return nil, err
		}
	}

	dataBuilder, err := NewBlockBuilderWithInterval(opts.RestartInterval)
	if err != nil {
		return nil, err
	}

	return &TableWriter{
		opts:             opts,
		dstPath:          file.Name(),
		tmpPath:          "",
		file:             file,
		dataBlockBuilder: dataBuilder,
		indexBuilder:     NewIndexBuilder(),
		filterBuilder:    opts.FilterBuilder,
		smallestSeqNum:   math.MaxUint64,
		state:            stateOpen,
		writeFn:          defaultWrite,
		syncFn:           defaultSync,
		closeFn:          defaultClose,
		syncDirFn:        syncDir,
		linkFn:           os.Link,
	}, nil
}

// Add appends an internal key-value pair to the SSTable.
//
// Invariants:
//   - Keys must be added in strictly increasing canonical order (UserKey ASC, SeqNum DESC, OpType DESC).
//   - When the uncompressed data block estimate exceeds TargetBlockSize, the block is flushed to disk.
//   - Caller mutations to key or value after Add do not affect the writer or written SSTable.
func (w *TableWriter) Add(key binary.InternalKey, value []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == stateFinalized {
		return errors.ErrTableWriterFinalized
	}
	if w.state == stateClosed {
		return errors.ErrTableWriterClosed
	}
	if w.state == stateError {
		if w.err != nil {
			return w.err
		}
		return errors.ErrTableWriterClosed
	}

	// 1. Validate key, opType, and value BEFORE mutating writer state or flushing
	if err := binary.ValidateKey(key.UserKey); err != nil {
		return err
	}
	if err := key.OpType.Validate(); err != nil {
		return err
	}
	if err := binary.ValidateValue(value); err != nil {
		return err
	}

	// 2. Verify strictly monotonic key ordering
	if w.hasPrevKey {
		if binary.CompareInternalKey(w.prevKey, key) >= 0 {
			return &errors.KeyOutOfOrderError{
				PrevKeyLen: len(w.prevKey.UserKey),
				CurrKeyLen: len(key.UserKey),
			}
		}
	}

	// 3. Flush current block if adding another entry would exceed target size
	if w.dataBlockBuilder.CurrentSizeEstimate() >= w.opts.TargetBlockSize && !w.dataBlockBuilder.IsEmpty() {
		if err := w.flushDataBlock(); err != nil {
			return err
		}
	}

	// 3. Add to data block builder (flush and retry if block would overflow MaxDataBlockSize)
	if err := w.dataBlockBuilder.Add(key, value); err != nil {
		if stdErrors.Is(err, errors.ErrBlockOverflow) && !w.dataBlockBuilder.IsEmpty() {
			if flushErr := w.flushDataBlock(); flushErr != nil {
				return flushErr
			}
			if retryErr := w.dataBlockBuilder.Add(key, value); retryErr != nil {
				return retryErr
			}
		} else {
			return err
		}
	}

	// Add user key to filter builder if configured (VULN-003 / SEC-008 remediation)
	if w.filterBuilder != nil {
		if err := w.filterBuilder.AddKey(key.UserKey); err != nil {
			w.state = stateError
			w.err = fmt.Errorf("failed adding key to filter builder: %w", err)
			_ = w.cleanupStaging()
			return w.err
		}
	}

	// 4. Update largest key in current block (defensive copy)
	encodedKey := binary.AppendInternalKey(nil, key)
	w.largestKeyInCurrentBlock = encodedKey

	// 5. Update table metadata
	if w.entryCount == 0 {
		w.smallestKey = make([]byte, len(encodedKey))
		copy(w.smallestKey, encodedKey)
	}
	w.largestKey = make([]byte, len(encodedKey))
	copy(w.largestKey, encodedKey)

	seqNum := uint64(key.SeqNum)
	if seqNum < w.smallestSeqNum {
		w.smallestSeqNum = seqNum
	}
	if seqNum > w.largestSeqNum {
		w.largestSeqNum = seqNum
	}

	w.prevKey = key.Clone()
	w.hasPrevKey = true
	w.entryCount++
	return nil
}

// AddRaw decodes a raw encoded InternalKey byte slice and appends it with the given value.
func (w *TableWriter) AddRaw(encodedKey []byte, value []byte) error {
	ik, err := binary.DecodeInternalKey(encodedKey)
	if err != nil {
		return err
	}
	return w.Add(ik, value)
}

// Build iterates all key-value records from iter sequentially, appends them to the writer,
// and finalizes the SSTable file, returning the resulting SSTableMetadata.
func (w *TableWriter) Build(iter Iterator) (*SSTableMetadata, error) {
	if iter == nil {
		return nil, errors.ErrNilReceiver
	}

	if !iter.Valid() {
		if !iter.Next() {
			return w.Finish()
		}
	}

	for iter.Valid() {
		if err := w.Add(iter.Key(), iter.Value()); err != nil {
			_ = w.Close()
			return nil, err
		}
		if !iter.Next() {
			break
		}
	}

	// Propagate error from iterators that expose Err() error (SEC-P05-003)
	type errorableIterator interface {
		Err() error
	}
	if errIter, ok := iter.(errorableIterator); ok {
		if err := errIter.Err(); err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("iterator error during table build: %w", err)
		}
	}

	return w.Finish()
}

// flushDataBlock serializes and writes the current pending data block to disk, recording
// its handle and largest key in the IndexBuilder.
func (w *TableWriter) flushDataBlock() error {
	if w.dataBlockBuilder.IsEmpty() {
		return nil
	}

	blockBytes := w.dataBlockBuilder.Finish()
	blockSize := uint64(len(blockBytes))
	blockOffset := w.offset

	if err := w.writeAll(blockBytes); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return err
	}

	handle := BlockHandle{
		Offset: blockOffset,
		Size:   blockSize,
	}

	if err := w.indexBuilder.AddBlock(w.largestKeyInCurrentBlock, handle); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return err
	}

	w.dataBlockCount++
	w.dataBlockBuilder.Reset()
	w.largestKeyInCurrentBlock = nil
	return nil
}

// Finish flushes pending data blocks, writes the meta-index block, writes the index block,
// writes the fixed 48-byte footer, flushes buffers to stable storage via f.Sync, closes
// the file descriptor, and performs an atomic rename if a staging file was used.
func (w *TableWriter) Finish() (*SSTableMetadata, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == stateFinalized {
		return nil, errors.ErrTableWriterFinalized
	}
	if w.state == stateClosed {
		return nil, errors.ErrTableWriterClosed
	}
	if w.state == stateError {
		if w.err != nil {
			return nil, w.err
		}
		return nil, errors.ErrTableWriterClosed
	}

	// 1. Flush any remaining data block entries
	if err := w.flushDataBlock(); err != nil {
		return nil, err
	}

	// 2. Write Filter block if filter builder is configured and non-empty
	var filterHandle BlockHandle
	hasFilter := w.filterBuilder != nil && !w.filterBuilder.IsEmpty()
	if hasFilter {
		filterBytes := w.filterBuilder.Finish()
		filterOffset := w.offset
		if err := w.writeAll(filterBytes); err != nil {
			w.state = stateError
			w.err = err
			_ = w.cleanupStaging()
			return nil, err
		}
		filterHandle = BlockHandle{
			Offset: filterOffset,
			Size:   uint64(len(filterBytes)),
		}
		w.filterHandle = filterHandle
	}

	// 3. Write MetaIndex block (points to filter block if present, or 8-byte empty block)
	var metaBytes []byte
	if hasFilter {
		metaEntries := map[string]BlockHandle{
			filter.FilterMetaKey: filterHandle,
		}
		metaBytes = BuildMetaIndexBlock(metaEntries)
	} else {
		metaBytes = emptyMetaIndexBlock()
	}
	metaOffset := w.offset
	if err := w.writeAll(metaBytes); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}
	metaHandle := BlockHandle{
		Offset: metaOffset,
		Size:   uint64(len(metaBytes)),
	}

	// 3. Write Index block
	indexBytes := w.indexBuilder.Finish()
	if len(indexBytes) == 0 {
		// Empty table: serialize empty index block (8B) to ensure Size > 0
		indexBytes = emptyMetaIndexBlock()
	}
	indexOffset := w.offset
	if err := w.writeAll(indexBytes); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}
	indexHandle := BlockHandle{
		Offset: indexOffset,
		Size:   uint64(len(indexBytes)),
	}

	// 4. Construct and write 48-byte Footer
	footer := Footer{
		MetaIndexHandle: metaHandle,
		IndexHandle:     indexHandle,
	}
	if err := footer.Validate(); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}

	footerBytes := footer.Encode()
	if err := w.writeAll(footerBytes[:]); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}

	// Verify file boundary integrity
	totalFileSize := w.offset
	if err := footer.ValidateAgainstFileSize(int64(totalFileSize)); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}

	// 5. Durability Barrier (Sync)
	if err := w.syncFn(w.file); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}

	// 6. Close file
	if err := w.closeFn(w.file); err != nil {
		w.state = stateError
		w.err = err
		_ = w.cleanupStaging()
		return nil, err
	}
	w.file = nil

	// 7. Atomic Publication & Directory Sync (if staging file was used)
	if w.tmpPath != "" {
		if w.preLinkHookFn != nil {
			if err := w.preLinkHookFn(); err != nil {
				w.state = stateError
				w.err = err
				_ = w.cleanupStaging()
				return nil, err
			}
		}

		// Verify parent directory identity before publication
		curParent := filepath.Dir(w.dstPath)
		curParentStat, err := os.Lstat(curParent)
		if err != nil {
			w.state = stateError
			w.err = fmt.Errorf("%w: failed to inspect parent directory %q: %w", errors.ErrParentDirectorySwapped, curParent, err)
			_ = w.cleanupStaging()
			return nil, w.err
		}
		if !isSystemSymlinkPrefix(curParent) && curParentStat.Mode()&os.ModeSymlink != 0 {
			w.state = stateError
			w.err = fmt.Errorf("%w: parent directory %q is a symlink", errors.ErrParentDirectorySymlink, curParent)
			_ = w.cleanupStaging()
			return nil, w.err
		}
		if !curParentStat.IsDir() {
			w.state = stateError
			w.err = fmt.Errorf("%w: parent path %q is not a directory", errors.ErrNotADirectory, curParent)
			_ = w.cleanupStaging()
			return nil, w.err
		}
		if w.parentDirStat != nil && !os.SameFile(w.parentDirStat, curParentStat) {
			w.state = stateError
			w.err = fmt.Errorf("%w: parent directory %q was swapped or redirected", errors.ErrParentDirectorySwapped, curParent)
			_ = w.cleanupStaging()
			return nil, w.err
		}

		// Verify intermediate path components have not been replaced with symlinks
		if err := validatePathNoSymlinks(curParent); err != nil {
			w.state = stateError
			w.err = err
			_ = w.cleanupStaging()
			return nil, w.err
		}

		// Verify staging file directory identity
		curTmpParent := filepath.Dir(w.tmpPath)
		curTmpParentStat, err := os.Lstat(curTmpParent)
		if err != nil || (w.parentDirStat != nil && !os.SameFile(w.parentDirStat, curTmpParentStat)) {
			w.state = stateError
			w.err = fmt.Errorf("%w: staging directory %q was swapped or redirected", errors.ErrParentDirectorySwapped, curTmpParent)
			_ = w.cleanupStaging()
			return nil, w.err
		}

		// Verify destination path does not already exist before atomic link
		if _, err := os.Lstat(w.dstPath); err == nil {
			w.state = stateError
			w.err = errors.ErrSSTableExists
			_ = w.cleanupStaging()
			return nil, errors.ErrSSTableExists
		} else if !os.IsNotExist(err) {
			w.state = stateError
			w.err = err
			_ = w.cleanupStaging()
			return nil, fmt.Errorf("failed to check destination path %q: %w", w.dstPath, err)
		}

		// Verify staging file has not been replaced or swapped before atomic link (SEC-P04-001)
		curTmpStat, err := os.Lstat(w.tmpPath)
		if err != nil {
			w.state = stateError
			w.err = fmt.Errorf("failed to stat staging file %q: %w", w.tmpPath, err)
			_ = w.cleanupStaging()
			return nil, w.err
		}
		if curTmpStat.Mode()&os.ModeSymlink != 0 {
			w.state = stateError
			w.err = fmt.Errorf("staging file %q was replaced with a symlink", w.tmpPath)
			_ = w.cleanupStaging()
			return nil, w.err
		}
		if w.stagingStat != nil && !os.SameFile(w.stagingStat, curTmpStat) {
			w.state = stateError
			w.err = fmt.Errorf("staging file %q was replaced before publication: %w", w.tmpPath, os.ErrInvalid)
			_ = w.cleanupStaging()
			return nil, w.err
		}

		// Use atomic link(2) to publish the SSTable without TOCTOU overwrite races.
		// os.Link creates dstPath as a new hard link to tmpPath and fails atomically
		// with EEXIST if dstPath already exists, preventing any overwrite.
		//
		// Since tmpPath and dstPath are always in the same parent directory (same
		// filesystem mount), cross-device (EXDEV) failures cannot occur. Therefore
		// no fallback to os.Rename is needed or safe — Rename can silently replace
		// an existing destination, violating the no-overwrite invariant.
		err = w.linkFn(w.tmpPath, w.dstPath)
		if err == nil {
			_ = os.Remove(w.tmpPath)
		} else if os.IsExist(err) {
			w.state = stateError
			w.err = errors.ErrSSTableExists
			_ = w.cleanupStaging()
			return nil, errors.ErrSSTableExists
		} else {
			// Any other Link failure (permission denied, I/O error, etc.) is a real
			// error. Do NOT fall back to os.Rename — it is overwrite-capable and would
			// reintroduce the TOCTOU race this code exists to prevent.
			w.state = stateError
			w.err = fmt.Errorf("atomic publication via link failed for %q: %w", w.dstPath, err)
			_ = w.cleanupStaging()
			return nil, w.err
		}
		// Publication succeeded: file is now at dstPath, staging path no longer exists
		w.tmpPath = ""

		// Directory durability sync:
		// Synchronize the pinned parent directory descriptor directly.
		var syncErr error
		if w.syncDirFn != nil {
			syncErr = w.syncDirFn(filepath.Dir(w.dstPath))
		} else if w.parentDirFile != nil && w.syncDirFileFn != nil {
			syncErr = w.syncDirFileFn(w.parentDirFile)
		}
		if syncErr != nil {
			w.state = stateFinalized
			w.err = syncErr
			if w.parentDirFile != nil {
				_ = w.parentDirFile.Close()
				w.parentDirFile = nil
			}
			return nil, fmt.Errorf("file published to %q but directory sync failed: %w", w.dstPath, syncErr)
		}
		if w.parentDirFile != nil {
			_ = w.parentDirFile.Close()
			w.parentDirFile = nil
		}
	}

	w.state = stateFinalized

	// Construct metadata
	meta := &SSTableMetadata{
		Path:            w.dstPath,
		FileSize:        totalFileSize,
		DataBlockCount:  w.dataBlockCount,
		EntryCount:      w.entryCount,
		SmallestKey:     w.smallestKey,
		LargestKey:      w.largestKey,
		SmallestSeqNum:  w.smallestSeqNum,
		LargestSeqNum:   w.largestSeqNum,
		MetaIndexHandle: metaHandle,
		IndexHandle:     indexHandle,
		FilterHandle:    filterHandle,
	}
	if w.entryCount == 0 {
		meta.SmallestSeqNum = 0
		meta.LargestSeqNum = 0
	}

	return meta, nil
}

// Close releases writer resources and cleans up any unfinalized staging file.
// If the table was already finalized, Close is a safe no-op.
func (w *TableWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == stateFinalized {
		if w.parentDirFile != nil {
			_ = w.parentDirFile.Close()
			w.parentDirFile = nil
		}
		return nil
	}
	if w.state == stateClosed {
		return nil
	}

	w.state = stateClosed
	return w.cleanupStaging()
}

// cleanupStaging closes the open file descriptor, releases the pinned parent directory,
// and removes the .tmp file if present.
func (w *TableWriter) cleanupStaging() error {
	var firstErr error
	if w.file != nil {
		if err := w.closeFn(w.file); err != nil {
			firstErr = err
		}
		w.file = nil
	}
	if w.tmpPath != "" {
		// Only remove staging file if the parent directory has NOT been swapped
		shouldRemove := true
		if w.parentDirStat != nil {
			curParent := filepath.Dir(w.tmpPath)
			curParentStat, err := os.Lstat(curParent)
			if err != nil || (!isSystemSymlinkPrefix(curParent) && curParentStat.Mode()&os.ModeSymlink != 0) || !os.SameFile(w.parentDirStat, curParentStat) {
				shouldRemove = false
			}
		}
		if shouldRemove {
			if err := os.Remove(w.tmpPath); err != nil && !os.IsNotExist(err) {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		w.tmpPath = ""
	}
	if w.parentDirFile != nil {
		if err := w.parentDirFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		w.parentDirFile = nil
	}
	return firstErr
}

// writeAll writes the complete slice p to the underlying file in a loop handling short writes.
func (w *TableWriter) writeAll(p []byte) error {
	written := 0
	for written < len(p) {
		n, err := w.writeFn(w.file, p[written:])
		written += n
		if err != nil {
			return err
		}
		if n == 0 && written < len(p) {
			return fmt.Errorf("short write without error: written %d of %d", written, len(p))
		}
	}
	w.offset += uint64(written)
	return nil
}

// BytesWritten returns the total physical bytes emitted to the file so far.
func (w *TableWriter) BytesWritten() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offset
}

// EntryCount returns the total number of records added to the writer.
func (w *TableWriter) EntryCount() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.entryCount
}

// BlockCount returns the number of completed data blocks flushed to disk.
func (w *TableWriter) BlockCount() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dataBlockCount
}

// TempPath returns the physical staging path currently in use by this writer,
// or empty string if writing directly to a file descriptor.
func (w *TableWriter) TempPath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tmpPath
}

// EstimatedSize returns the estimated size of the SSTable if finalized now.
func (w *TableWriter) EstimatedSize() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	filterSize := uint64(0)
	metaSize := uint64(8)
	if w.filterBuilder != nil && !w.filterBuilder.IsEmpty() {
		filterSize = uint64(w.filterBuilder.CurrentSizeEstimate())
		metaSize = 41
	}
	return w.offset + uint64(w.dataBlockBuilder.CurrentSizeEstimate()) + uint64(w.indexBuilder.CurrentSizeEstimate()) + filterSize + metaSize + FooterSize
}

// emptyMetaIndexBlock creates an 8-byte valid serialized block with entryCount=0 and CRC32-IEEE.
// This guarantees MetaIndexHandle.Size = 8 > 0, satisfying Footer.Validate() and
// DecodeBlockIndex (which decodes entryCount=0 and returns an empty BlockIndex).
func emptyMetaIndexBlock() []byte {
	var buf [8]byte
	binary.PutUint32(buf[0:4], 0)
	crc := binary.Checksum(buf[0:4])
	binary.PutUint32(buf[4:8], crc)
	return buf[:]
}

func defaultWrite(f *os.File, p []byte) (int, error) {
	return f.Write(p)
}

func defaultSync(f *os.File) error {
	return f.Sync()
}

func defaultClose(f *os.File) error {
	return f.Close()
}

func syncDir(dirPath string) error {
	df, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	defer func() { _ = df.Close() }()

	if err := df.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func syncDirFile(f *os.File) error {
	if f == nil {
		return os.ErrInvalid
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	return f.Sync()
}

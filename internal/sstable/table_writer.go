package sstable

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// writerState represents the lifecycle phase of an active TableWriter.
type writerState int

const (
	stateOpen writerState = iota
	stateFinalized
	stateClosed
	stateError
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
	// Default: 0644.
	FileMode os.FileMode
}

// DefaultTableWriterOptions returns production defaults for TableWriterOptions.
func DefaultTableWriterOptions() TableWriterOptions {
	return TableWriterOptions{
		TargetBlockSize: TargetBlockSize,
		RestartInterval: DefaultRestartInterval,
		FileMode:        0644,
	}
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

	state writerState
	err   error

	// Test seams for deterministic fault injection
	writeFn func(f *os.File, p []byte) (int, error)
	syncFn  func(f *os.File) error
	closeFn func(f *os.File) error
}

// NewTableWriter initializes a TableWriter to write an SSTable to dstPath using a staging .tmp file.
// If an SSTable already exists at dstPath, it returns errors.ErrSSTableExists.
func NewTableWriter(dstPath string, opts TableWriterOptions) (*TableWriter, error) {
	if dstPath == "" {
		return nil, os.ErrInvalid
	}

	// Reject overwriting an existing finalized SSTable
	if _, err := os.Stat(dstPath); err == nil {
		return nil, errors.ErrSSTableExists
	}

	if opts.TargetBlockSize <= 0 {
		opts.TargetBlockSize = TargetBlockSize
	}
	if opts.RestartInterval <= 0 {
		opts.RestartInterval = DefaultRestartInterval
	}
	if opts.FileMode == 0 {
		opts.FileMode = 0644
	}

	parentDir := filepath.Dir(dstPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		return nil, err
	}

	tmpPath := dstPath + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, opts.FileMode)
	if err != nil {
		return nil, err
	}

	dataBuilder, err := NewBlockBuilderWithInterval(opts.RestartInterval)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}

	return &TableWriter{
		opts:             opts,
		dstPath:          dstPath,
		tmpPath:          tmpPath,
		file:             file,
		dataBlockBuilder: dataBuilder,
		indexBuilder:     NewIndexBuilder(),
		smallestSeqNum:   math.MaxUint64,
		state:            stateOpen,
		writeFn:          defaultWrite,
		syncFn:           defaultSync,
		closeFn:          defaultClose,
	}, nil
}

// NewTableWriterWithFile initializes a TableWriter writing directly to an open *os.File without staging rename.
// Useful for direct file testing and custom file descriptors.
func NewTableWriterWithFile(file *os.File, opts TableWriterOptions) (*TableWriter, error) {
	if file == nil {
		return nil, errors.ErrNilReceiver
	}

	if opts.TargetBlockSize <= 0 {
		opts.TargetBlockSize = TargetBlockSize
	}
	if opts.RestartInterval <= 0 {
		opts.RestartInterval = DefaultRestartInterval
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
		smallestSeqNum:   math.MaxUint64,
		state:            stateOpen,
		writeFn:          defaultWrite,
		syncFn:           defaultSync,
		closeFn:          defaultClose,
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

	// 1. Verify strictly monotonic key ordering
	if w.hasPrevKey {
		if binary.CompareInternalKey(w.prevKey, key) >= 0 {
			return &errors.KeyOutOfOrderError{
				PrevKeyLen: len(w.prevKey.UserKey),
				CurrKeyLen: len(key.UserKey),
			}
		}
	}

	// 2. Flush current block if adding another entry would exceed target size
	if w.dataBlockBuilder.CurrentSizeEstimate() >= w.opts.TargetBlockSize && !w.dataBlockBuilder.IsEmpty() {
		if err := w.flushDataBlock(); err != nil {
			return err
		}
	}

	// 3. Add to data block builder
	if err := w.dataBlockBuilder.Add(key, value); err != nil {
		return err
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

	// 2. Write MetaIndex block (in Phase 04, valid empty 8-byte block with 0 count + CRC32)
	metaBytes := emptyMetaIndexBlock()
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

	// 7. Atomic Rename & Directory Sync (if staging file was used)
	if w.tmpPath != "" {
		if err := os.Rename(w.tmpPath, w.dstPath); err != nil {
			w.state = stateError
			w.err = err
			_ = os.Remove(w.tmpPath)
			return nil, err
		}
		if err := syncDir(filepath.Dir(w.dstPath)); err != nil {
			w.state = stateError
			w.err = err
			return nil, err
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
		return nil
	}
	if w.state == stateClosed {
		return nil
	}

	w.state = stateClosed
	return w.cleanupStaging()
}

// cleanupStaging closes the open file descriptor and removes the .tmp file if present.
func (w *TableWriter) cleanupStaging() error {
	var firstErr error
	if w.file != nil {
		if err := w.closeFn(w.file); err != nil {
			firstErr = err
		}
		w.file = nil
	}
	if w.tmpPath != "" {
		if err := os.Remove(w.tmpPath); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
		}
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

// EstimatedSize returns the estimated size of the SSTable if finalized now.
func (w *TableWriter) EstimatedSize() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.offset + uint64(w.dataBlockBuilder.CurrentSizeEstimate()) + uint64(w.indexBuilder.CurrentSizeEstimate()) + 8 + FooterSize
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

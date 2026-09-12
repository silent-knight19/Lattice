package sstable

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
)

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
// TableReader is safe for concurrent Seek operations across multiple goroutines.
// Disk reads use positional ReadAt, ensuring no mutable file offset state is shared.
// Close acquires an exclusive write lock to safely release file resources.
type TableReader struct {
	mu       sync.RWMutex
	file     *os.File
	fileSize int64
	footer   Footer
	index    *BlockIndex
	readAtFn func(p []byte, off int64) (int, error)
	closed   bool
}

// NewTableReader opens an SSTable file at path and initializes a TableReader.
func NewTableReader(path string) (*TableReader, error) {
	if path == "" {
		return nil, os.ErrInvalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return NewTableReaderWithFile(file)
}

// OpenTableReader is an alias for NewTableReader following idiomatic Go naming conventions.
func OpenTableReader(path string) (*TableReader, error) {
	return NewTableReader(path)
}

// NewTableReaderWithFile initializes a TableReader wrapping an existing open *os.File.
// The reader assumes ownership of the file descriptor; calling Close on the reader
// will close the provided file.
//
// Security Contract:
// If initialization fails on any early return path (stat failure, truncated file, corrupt footer,
// oversized index handle, corrupted index), the provided file descriptor is guaranteed to be closed,
// preventing descriptor leaks.
func NewTableReaderWithFile(file *os.File) (*TableReader, error) {
	if file == nil {
		return nil, errors.ErrNilReceiver
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
		file:     file,
		fileSize: fileSize,
		footer:   footer,
		index:    blockIndex,
		readAtFn: file.ReadAt,
	}, nil

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

// Seek performs a point lookup for the given user key in the SSTable.
//
// Lookup Flow:
//  1. Validates user key constraints (1..65,535 bytes).
//  2. Acquires shared read lock (concurrent-safe).
//  3. Uses the in-memory sparse index binary search to locate the candidate data block.
//     If targetKey > all largest keys in the index, returns errors.ErrKeyNotFound with zero disk I/O.
//  4. Validates the data block handle against physical file bounds.
//  5. Reads exactly handle.Size bytes starting at handle.Offset via positional ReadAt.
//  6. Validates the data block CRC32-IEEE checksum and restart metadata.
//  7. Binary-searches restart points to locate the nearest restart interval.
//  8. Scans prefix-compressed entries forward, reconstructing full InternalKeys.
//  9. If a match is found:
//     - If OpType == OpTypePut: returns an owned defensive copy of the value bytes and nil.
//     - If OpType == OpTypeDelete: returns nil and errors.ErrKeyNotFound (tombstone).
//  10. If the key does not exist or scanning passes the key, returns nil and errors.ErrKeyNotFound.
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

	// 2. Validate data block handle bounds
	if handle.Size == 0 || handle.Size > MaxDataBlockSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "data block size exceeds MaxDataBlockSize or is zero",
		}
	}
	if handle.Offset > math.MaxUint64-handle.Size || handle.Offset+handle.Size > uint64(r.fileSize)-FooterSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "candidate data block extends into footer, exceeds physical file boundary, or overflows address space",
		}
	}
	if handle.Size > math.MaxInt || handle.Offset > math.MaxInt64 {
		return nil, &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "candidate data block offset or size exceeds architecture integer bounds",
		}
	}

	// 3. Read candidate data block from disk via ReadAt
	blockBuf := make([]byte, int(handle.Size))
	if err := readExactAt(r.readAtFn, blockBuf, int64(handle.Offset)); err != nil {
		return nil, fmt.Errorf("failed to read data block at offset %d: %w", handle.Offset, err)
	}

	// 4. Decode and search prefix-compressed data block
	return searchDataBlock(blockBuf, userKey, handle.Offset)
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
		return nil, &errors.ChecksumMismatchError{
			Offset:   int64(blockOffset),
			Expected: expectedCRC,
			Actual:   actualCRC,
		}
	}

	// 2. Parse and validate restart count
	restartCount := binary.GetUint32(blockBuf[len(blockBuf)-8 : len(blockBuf)-4])
	if restartCount == 0 {
		return nil, &errors.DataBlockCorruptedError{
			Offset: blockOffset,
			Reason: "data block restart count is zero",
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

		shared, n1, err := binary.GetVarint64(entrySlice)
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

		unshared, n2, err := binary.GetVarint64(entrySlice[n1:])
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off) + uint64(n1),
				Reason: "varint unshared key length corrupted",
			}
		}

		valueLen, n3, err := binary.GetVarint64(entrySlice[n1+n2:])
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

		shared, n1, err := binary.GetVarint64(entrySlice)
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "varint shared key length corrupted",
			}
		}

		unshared, n2, err := binary.GetVarint64(entrySlice[n1:])
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset) + uint64(n1),
				Reason: "varint unshared key length corrupted",
			}
		}

		valueLen, n3, err := binary.GetVarint64(entrySlice[n1+n2:])
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
	if filterHandle.Offset+filterHandle.Size > r.footer.MetaIndexHandle.Offset {
		return nil, &errors.InvalidBlockHandleError{
			Offset: filterHandle.Offset,
			Size:   filterHandle.Size,
			Reason: "filter block handle overlaps or exceeds metaindex boundary",
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

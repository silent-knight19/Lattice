package sstable

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TableReader provides point-lookup access to an immutable, finalized SSTable file.
// Upon opening, the reader reads the fixed 48-byte footer, validates its cryptographic
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
	closed   bool

	// readAtFn is the positional read function (default: file.ReadAt).
	// Can be overridden via test hooks for fault injection.
	readAtFn func(p []byte, off int64) (int, error)
}

// NewTableReader opens an existing SSTable file at path, reads and validates the footer,
// and decodes the sparse index block into RAM.
//
// If initialization fails at any stage, the opened file descriptor is guaranteed to be
// closed before returning to prevent resource leaks.
func NewTableReader(path string) (*TableReader, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open sstable file %q: %w", path, err)
	}

	reader, err := NewTableReaderWithFile(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return reader, nil
}

// OpenTableReader is an alias for NewTableReader following idiomatic Go naming conventions.
func OpenTableReader(path string) (*TableReader, error) {
	return NewTableReader(path)
}

// NewTableReaderWithFile initializes a TableReader wrapping an existing open *os.File.
// The reader assumes ownership of the file descriptor; calling Close on the reader
// will close the provided file.
func NewTableReaderWithFile(file *os.File) (*TableReader, error) {
	if file == nil {
		return nil, errors.ErrNilReceiver
	}

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat sstable file: %w", err)
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
	indexBuf := make([]byte, indexHandle.Size)
	if err := readExactAt(file.ReadAt, indexBuf, int64(indexHandle.Offset)); err != nil {
		return nil, fmt.Errorf("failed to read index block at offset %d: %w", indexHandle.Offset, err)
	}

	// 5. Decode index block and retain in RAM
	blockIndex, err := DecodeBlockIndex(indexBuf)
	if err != nil {
		return nil, err
	}

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
	if handle.Size == 0 || handle.Offset+handle.Size > uint64(r.fileSize)-FooterSize {
		return nil, &errors.InvalidBlockHandleError{
			Offset: handle.Offset,
			Size:   handle.Size,
			Reason: "candidate data block extends into footer or beyond physical file boundary",
		}
	}

	// 3. Read candidate data block from disk via ReadAt
	blockBuf := make([]byte, handle.Size)
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
		if int(off) >= entryDataEnd {
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

		hdrLen := n1 + n2 + n3
		if uint64(hdrLen)+unshared+valueLen > uint64(len(entrySlice)) {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "restart entry payload exceeds entry data boundary",
			}
		}
		if unshared < binary.InternalKeyTrailerLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(off),
				Reason: "restart entry key shorter than internal key trailer",
			}
		}

		// InternalKey is UserKey + 9-byte trailer (8-byte SeqNum + 1-byte OpType)
		userKey := entrySlice[hdrLen : hdrLen+int(unshared)-binary.InternalKeyTrailerLen]
		return userKey, nil
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

		hdrLen := n1 + n2 + n3
		if uint64(hdrLen)+unshared+valueLen > uint64(len(entrySlice)) {
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
		if int(shared) > len(reconstructedKey) {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "shared prefix length exceeds reconstructed key length",
			}
		}

		// Reconstruct key: retain shared prefix and append current delta
		deltaKey := entrySlice[hdrLen : hdrLen+int(unshared)]
		reconstructedKey = append(reconstructedKey[:shared], deltaKey...)

		if len(reconstructedKey) < binary.InternalKeyTrailerLen {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: "reconstructed key shorter than internal key trailer",
			}
		}

		userKeyLen := len(reconstructedKey) - binary.InternalKeyTrailerLen
		entryUserKey := reconstructedKey[:userKeyLen]
		opTypeByte := reconstructedKey[userKeyLen+8]

		opType, err := binary.ParseOpType(opTypeByte)
		if err != nil {
			return nil, &errors.DataBlockCorruptedError{
				Offset: blockOffset + uint64(currOffset),
				Reason: fmt.Sprintf("invalid operation type: 0x%02x", opTypeByte),
			}
		}

		cmp := bytes.Compare(entryUserKey, targetUserKey)
		if cmp == 0 {
			// First encountered match is guaranteed to be the latest version (SeqNum DESC)
			if opType == binary.OpTypeDelete {
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

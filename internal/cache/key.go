package cache

import "fmt"

// BlockKey uniquely identifies a physical SSTable data block across the entire database.
// It combines the immutable SSTable file number with the block's physical byte offset.
//
// Invariants:
//  1. FileNum is monotonically allocated by Engine/VersionSet and is never reused.
//  2. Offset is the strictly positive or zero starting byte offset of the block within that SSTable file.
//  3. Identical (FileNum, Offset) pairs across any goroutines or time windows map to the exact same block.
//  4. Different FileNums with identical Offsets never collide.
type BlockKey struct {
	FileNum uint64 // Monotonically allocated SSTable file number (FileMetadata.FileNum)
	Offset  uint64 // Starting byte offset of the data block within the SSTable (BlockHandle.Offset)
}

// NewBlockKey constructs a new BlockKey for the given file number and byte offset.
func NewBlockKey(fileNum, offset uint64) BlockKey {
	return BlockKey{
		FileNum: fileNum,
		Offset:  offset,
	}
}

// String returns a human-readable representation of the BlockKey for logging and diagnostics.
func (k BlockKey) String() string {
	return fmt.Sprintf("BlockKey(FileNum=%d, Offset=%d)", k.FileNum, k.Offset)
}

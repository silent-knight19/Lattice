package sstable

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// TestP08_SEC_017_MaxRestartCountEnforcement verifies that TableIterator and TableReader
// reject data blocks advertising a restart count exceeding MaxRestartCount (65,536).
func TestP08_SEC_017_MaxRestartCountEnforcement(t *testing.T) {
	dir := t.TempDir()
	sstPath := filepath.Join(dir, "corrupt_restart.sst")

	// 1. Build a raw data block advertising restart count = MaxRestartCount + 1
	var dataBlock bytes.Buffer
	dataBlock.WriteString("dummy-entry-data")

	// Restart points
	var restartCount uint32 = MaxRestartCount + 1
	// Write restart offsets
	for i := uint32(0); i < restartCount; i++ {
		var offBuf [4]byte
		binary.PutUint32(offBuf[:], 0)
		dataBlock.Write(offBuf[:])
	}
	// Write restart count
	var countBuf [4]byte
	binary.PutUint32(countBuf[:], restartCount)
	dataBlock.Write(countBuf[:])
	// Write CRC32
	checksum := binary.Checksum(dataBlock.Bytes())
	var crcBuf [4]byte
	binary.PutUint32(crcBuf[:], checksum)
	dataBlock.Write(crcBuf[:])

	dataBytes := dataBlock.Bytes()
	dataHandle := BlockHandle{Offset: 0, Size: uint64(len(dataBytes))}

	// 2. Build index block pointing to this data block
	ik, err := binary.NewInternalKey([]byte("user_key"), 100, binary.OpTypePut)
	if err != nil {
		t.Fatalf("failed to build internal key: %v", err)
	}

	var indexBlock bytes.Buffer
	encodedKey := binary.EncodeInternalKey(ik)
	var klenBuf [10]byte
	n := binary.PutVarint64(klenBuf[:], uint64(len(encodedKey)))
	indexBlock.Write(klenBuf[:n])
	indexBlock.Write(encodedKey)
	indexBlock.Write(dataHandle.AppendTo(nil))

	// Index block restart array (1 restart point at offset 0)
	var rBuf [4]byte
	binary.PutUint32(rBuf[:], 0)
	indexBlock.Write(rBuf[:])
	binary.PutUint32(rBuf[:], 1) // 1 restart point
	indexBlock.Write(rBuf[:])
	idxCRC := binary.Checksum(indexBlock.Bytes())
	binary.PutUint32(rBuf[:], idxCRC)
	indexBlock.Write(rBuf[:])

	indexBytes := indexBlock.Bytes()
	indexHandle := BlockHandle{Offset: uint64(len(dataBytes)), Size: uint64(len(indexBytes))}

	// 3. Write full SSTable file: data block + index block + dummy meta block + footer
	var fileBuf bytes.Buffer
	fileBuf.Write(dataBytes)
	fileBuf.Write(indexBytes)

	metaOffset := uint64(fileBuf.Len())
	fileBuf.WriteString("meta")
	metaHandle := BlockHandle{Offset: metaOffset, Size: 4}

	footer := Footer{
		IndexHandle:     indexHandle,
		MetaIndexHandle: metaHandle,
	}
	footerEnc := footer.Encode()
	fileBuf.Write(footerEnc[:])

	if err := os.WriteFile(sstPath, fileBuf.Bytes(), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	reader, err := NewTableReader(sstPath)
	if err != nil {
		t.Fatalf("failed to open table reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	// 4. Test TableReader.Seek: must fail with DataBlockCorruptedError mentioning MaxRestartCount
	_, seekErr := reader.Seek([]byte("user_key"))
	if seekErr == nil {
		t.Fatalf("expected Seek to fail on restart count exceeding MaxRestartCount, got nil")
	}
	var corruptedErr *errors.DataBlockCorruptedError
	if !stdErrorsIs(seekErr, &corruptedErr) && !strings.Contains(seekErr.Error(), "exceeds MaxRestartCount") {
		t.Fatalf("expected DataBlockCorruptedError mentioning MaxRestartCount, got: %v", seekErr)
	}

	// 5. Test TableIterator: loadBlock must fail closed
	it, err := reader.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator: %v", err)
	}
	defer func() { _ = it.Close() }()

	if it.Next() {
		t.Fatalf("expected it.Next() to return false on corrupted restart count")
	}
	if it.Err() == nil || !strings.Contains(it.Err().Error(), "exceeds MaxRestartCount") {
		t.Fatalf("expected it.Err() to report MaxRestartCount violation, got: %v", it.Err())
	}
}

func stdErrorsIs(err error, target interface{}) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*errors.DataBlockCorruptedError)
	return ok
}

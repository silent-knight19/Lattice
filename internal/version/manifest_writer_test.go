package version

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// -----------------------------------------------------------------------------
// TEST ORACLE: INDEPENDENT MANIFEST RECORD DECODER (§22)
// -----------------------------------------------------------------------------

// decodeManifestRecord is a test-only independent parser that verifies the physical wire
// framing of a single MANIFEST record directly from r:
//
//	[ CRC32 (4B) | PayloadLength (4B) ] [ VersionEdit Payload (N B) ]
//
// Invariants verified independently:
//  1. Clean EOF at offset 0 returns (nil, 0, io.EOF).
//  2. Header truncation (< 8 bytes) returns errors.ErrManifestHeaderTruncated.
//  3. PayloadLength bounds check (<= MaxVersionEditBytes).
//  4. Payload truncation (< PayloadLength bytes) returns errors.ErrManifestPayloadTruncated.
//  5. CRC32-IEEE checksum over PayloadLength (4B) + Payload (N B) matches CRC in header.
func decodeManifestRecord(r io.Reader) ([]byte, uint32, error) {
	var headerBuf [ManifestHeaderSize]byte
	n, err := io.ReadFull(r, headerBuf[:])
	if err != nil {
		if stdErrors.Is(err, io.EOF) && n == 0 {
			return nil, 0, io.EOF
		}
		return nil, 0, errors.ErrManifestHeaderTruncated
	}

	expectedCRC := binary.GetUint32(headerBuf[0:4])
	payloadLen := binary.GetUint32(headerBuf[4:8])

	if payloadLen > MaxVersionEditBytes {
		return nil, 0, fmt.Errorf("manifest: payload length %d exceeds max %d", payloadLen, MaxVersionEditBytes)
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, 0, errors.ErrManifestPayloadTruncated
	}

	// Calculate CRC32-IEEE over PayloadLength (4B) + Payload (N B)
	crc := crc32.Update(0, crc32.IEEETable, headerBuf[4:ManifestHeaderSize])
	crc = crc32.Update(crc, crc32.IEEETable, payload)

	if crc != expectedCRC {
		return nil, 0, &errors.ChecksumMismatchError{
			Expected: expectedCRC,
			Actual:   crc,
		}
	}

	return payload, expectedCRC, nil
}

// readAllManifestRecords reads and verifies all records in a MANIFEST file using
// the independent test oracle.
func readAllManifestRecords(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open manifest file %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var records [][]byte
	for {
		payload, _, err := decodeManifestRecord(f)
		if err != nil {
			if stdErrors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decodeManifestRecord failed at record index %d: %v", len(records), err)
		}
		records = append(records, payload)
	}
	return records
}

// -----------------------------------------------------------------------------
// EXACT-BYTE TEST FIXTURES (§23 & §44)
// -----------------------------------------------------------------------------

func TestManifestWriter_ExactByteFixture_EmptyEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}

	edit := NewVersionEdit()
	if err := w.LogEdit(*edit); err != nil {
		t.Fatalf("LogEdit failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Independently calculated fixture:
	// FormatVersion: 0x01 (1 byte)
	// PayloadLength: 1 -> 0x00000001
	// CRC32-IEEE over [0x00, 0x00, 0x00, 0x01, 0x01]: 0xa83ef6ca (2822698698)
	expectedBytes := []byte{
		0xa8, 0x3e, 0xf6, 0xca, // CRC32 (Big-Endian)
		0x00, 0x00, 0x00, 0x01, // PayloadLength = 1 (Big-Endian)
		0x01, // VersionEditFormatV1 (empty edit)
	}

	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read manifest file: %v", err)
	}

	if !bytes.Equal(fileBytes, expectedBytes) {
		t.Fatalf("Exact-byte fixture mismatch for empty edit:\ngot:  %x\nwant: %x", fileBytes, expectedBytes)
	}

	// Verify with independent oracle
	payload, crc, err := decodeManifestRecord(bytes.NewReader(fileBytes))
	if err != nil {
		t.Fatalf("independent decoder failed on fixture: %v", err)
	}
	if crc != 0xa83ef6ca {
		t.Fatalf("CRC mismatch: got 0x%08x, want 0xa83ef6ca", crc)
	}
	if !bytes.Equal(payload, []byte{0x01}) {
		t.Fatalf("payload mismatch: got %x, want 01", payload)
	}
}

func TestManifestWriter_ExactByteFixture_ScalarsOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}

	edit := NewVersionEdit()
	edit.SetNextFileNum(42)
	edit.SetLastSeqNum(100)

	if err := w.LogEdit(*edit); err != nil {
		t.Fatalf("LogEdit failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Independently calculated fixture:
	// Payload: 0x01 (Format), 0x01, 0x01, 0x2a (NextFileNum=42), 0x02, 0x01, 0x64 (LastSeqNum=100) -> 7 bytes
	// PayloadLength: 7 -> 0x00000007
	// CRC32-IEEE over [0x00, 0x00, 0x00, 0x07, 0x01, 0x01, 0x01, 0x2a, 0x02, 0x01, 0x64]: 0xaec64660 (2932229728)
	expectedBytes := []byte{
		0xae, 0xc6, 0x46, 0x60, // CRC32 (Big-Endian)
		0x00, 0x00, 0x00, 0x07, // PayloadLength = 7 (Big-Endian)
		0x01,             // FormatVersion 0x01
		0x01, 0x01, 0x2a, // TagNextFileNum(1), Len(1), Val(42)
		0x02, 0x01, 0x64, // TagLastSeqNum(2), Len(1), Val(100)
	}

	fileBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read manifest file: %v", err)
	}

	if !bytes.Equal(fileBytes, expectedBytes) {
		t.Fatalf("Exact-byte fixture mismatch for scalar edit:\ngot:  %x\nwant: %x", fileBytes, expectedBytes)
	}

	// Verify with independent oracle
	payload, crc, err := decodeManifestRecord(bytes.NewReader(fileBytes))
	if err != nil {
		t.Fatalf("independent decoder failed on fixture: %v", err)
	}
	if crc != 0xaec64660 {
		t.Fatalf("CRC mismatch: got 0x%08x, want 0xaec64660", crc)
	}
	if len(payload) != 7 {
		t.Fatalf("payload length mismatch: got %d, want 7", len(payload))
	}
}

// -----------------------------------------------------------------------------
// LIFECYCLE TESTS (§41)
// -----------------------------------------------------------------------------

func TestManifestWriter_Lifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}

	if w.Path() != path {
		t.Errorf("Path mismatch: got %s, want %s", w.Path(), path)
	}
	if w.Offset() != 0 {
		t.Errorf("expected initial Offset 0, got %d", w.Offset())
	}
	if w.RecordCount() != 0 {
		t.Errorf("expected initial RecordCount 0, got %d", w.RecordCount())
	}
	if w.IsClosed() {
		t.Errorf("expected IsClosed() == false")
	}
	if w.IsPoisoned() {
		t.Errorf("expected IsPoisoned() == false")
	}

	// Log first edit
	edit := NewVersionEdit()
	edit.SetNextFileNum(1)
	if err := w.LogEdit(*edit); err != nil {
		t.Fatalf("LogEdit failed: %v", err)
	}

	if w.RecordCount() != 1 {
		t.Errorf("expected RecordCount 1, got %d", w.RecordCount())
	}
	if w.Offset() <= 0 {
		t.Errorf("expected positive Offset after write, got %d", w.Offset())
	}

	// Explicit Sync
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// Close
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !w.IsClosed() {
		t.Errorf("expected IsClosed() == true after Close")
	}

	// Idempotent double close
	if err := w.Close(); err != nil {
		t.Errorf("repeated Close() must return nil, got: %v", err)
	}

	// Operations after close must return ErrManifestWriterClosed
	if err := w.LogEdit(*edit); !stdErrors.Is(err, errors.ErrManifestWriterClosed) {
		t.Errorf("LogEdit after Close must return ErrManifestWriterClosed, got: %v", err)
	}
	if err := w.LogEditPtr(edit); !stdErrors.Is(err, errors.ErrManifestWriterClosed) {
		t.Errorf("LogEditPtr after Close must return ErrManifestWriterClosed, got: %v", err)
	}
	if err := w.Sync(); !stdErrors.Is(err, errors.ErrManifestWriterClosed) {
		t.Errorf("Sync after Close must return ErrManifestWriterClosed, got: %v", err)
	}
}

func TestManifestWriter_CreateExists_Rejection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w1, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("first CreateManifestWriter failed: %v", err)
	}
	_ = w1.Close()

	// Second CreateManifestWriter must fail with ErrManifestExists and os.ErrExist
	w2, err := CreateManifestWriter(path)
	if err == nil {
		_ = w2.Close()
		t.Fatalf("expected CreateManifestWriter on existing path to fail")
	}
	if !stdErrors.Is(err, errors.ErrManifestExists) {
		t.Errorf("expected error wrapping ErrManifestExists, got: %v", err)
	}
	if !stdErrors.Is(err, os.ErrExist) {
		t.Errorf("expected error wrapping os.ErrExist, got: %v", err)
	}
}

func TestManifestWriter_OpenAppend_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	// Phase 1: Write first edit
	w1, err := OpenManifestWriter(path)
	if err != nil {
		t.Fatalf("OpenManifestWriter failed: %v", err)
	}
	edit1 := NewVersionEdit()
	edit1.SetNextFileNum(10)
	if err := w1.LogEdit(*edit1); err != nil {
		t.Fatalf("LogEdit 1 failed: %v", err)
	}
	offset1 := w1.Offset()
	if err := w1.Close(); err != nil {
		t.Fatalf("Close 1 failed: %v", err)
	}

	// Phase 2: Reopen existing manifest
	w2, err := OpenManifestWriter(path)
	if err != nil {
		t.Fatalf("reopen OpenManifestWriter failed: %v", err)
	}
	if w2.Offset() != offset1 {
		t.Errorf("reopened writer offset mismatch: got %d, want %d", w2.Offset(), offset1)
	}

	edit2 := NewVersionEdit()
	edit2.SetLastSeqNum(200)
	if err := w2.LogEdit(*edit2); err != nil {
		t.Fatalf("LogEdit 2 failed: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close 2 failed: %v", err)
	}

	// Verify both records exist and match
	records := readAllManifestRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	d1, err := DecodeVersionEdit(records[0])
	if err != nil {
		t.Fatalf("decode record 1 failed: %v", err)
	}
	if !d1.Equal(edit1) {
		t.Fatalf("record 1 logical mismatch")
	}

	d2, err := DecodeVersionEdit(records[1])
	if err != nil {
		t.Fatalf("decode record 2 failed: %v", err)
	}
	if !d2.Equal(edit2) {
		t.Fatalf("record 2 logical mismatch")
	}
}

func TestManifestWriter_NewManifestWriter_Validations(t *testing.T) {
	// Nil file check
	_, err := NewManifestWriter(nil)
	if err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected error wrapping os.ErrInvalid for nil file, got: %v", err)
	}

	// Empty path checks
	if _, err := OpenManifestWriter(""); err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected error for empty path in OpenManifestWriter")
	}
	if _, err := CreateManifestWriter(""); err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected error for empty path in CreateManifestWriter")
	}

	// Nil VersionEdit in LogEditPtr
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")
	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	if err := w.LogEditPtr(nil); err == nil || !stdErrors.Is(err, os.ErrInvalid) {
		t.Fatalf("expected error for nil edit in LogEditPtr, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// SEQUENTIAL APPEND: 50 EDITS (§21)
// -----------------------------------------------------------------------------

func TestManifestWriter_SequentialAppend_50Edits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}

	const numEdits = 50
	expectedEdits := make([]*VersionEdit, numEdits)

	for i := 0; i < numEdits; i++ {
		edit := NewVersionEdit()
		edit.SetNextFileNum(uint64(i + 1))
		edit.SetLastSeqNum(binary.SeqNum((i + 1) * 100))

		// Add some file additions and deletions varying across levels
		level := uint32(i % NumLevels)
		_ = edit.DeleteFile(level, uint64(i+1000))
		_ = edit.AddFile(level, FileMetadata{
			FileNum:        uint64(i + 2000),
			FileSize:       uint64((i + 1) * 4096),
			SmallestKey:    makeTestIK(fmt.Sprintf("key_%04d_a", i), uint64((i+1)*100), binary.OpTypePut),
			LargestKey:     makeTestIK(fmt.Sprintf("key_%04d_z", i), uint64((i+1)*100), binary.OpTypePut),
			SmallestSeqNum: uint64(i * 100),
			LargestSeqNum:  uint64((i + 1) * 100),
		})

		expectedEdits[i] = edit

		if err := w.LogEdit(*edit); err != nil {
			t.Fatalf("LogEdit failed at index %d: %v", i, err)
		}
	}

	if w.RecordCount() != numEdits {
		t.Errorf("RecordCount mismatch: got %d, want %d", w.RecordCount(), numEdits)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Verify all 50 records independently
	records := readAllManifestRecords(t, path)
	if len(records) != numEdits {
		t.Fatalf("record count mismatch: got %d, want %d", len(records), numEdits)
	}

	for i := 0; i < numEdits; i++ {
		decoded, err := DecodeVersionEdit(records[i])
		if err != nil {
			t.Fatalf("DecodeVersionEdit failed at index %d: %v", i, err)
		}
		if !decoded.Equal(expectedEdits[i]) {
			t.Fatalf("logical inequality at edit index %d", i)
		}
	}
}

// -----------------------------------------------------------------------------
// REOPEN AND APPEND TEST (§20)
// -----------------------------------------------------------------------------

func TestManifestWriter_ReopenAndAppend_50Edits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	const totalEdits = 50
	const chunkSize = 10
	expectedEdits := make([]*VersionEdit, totalEdits)

	for start := 0; start < totalEdits; start += chunkSize {
		w, err := OpenManifestWriter(path)
		if err != nil {
			t.Fatalf("OpenManifestWriter at start %d failed: %v", start, err)
		}

		for i := start; i < start+chunkSize; i++ {
			edit := NewVersionEdit()
			edit.SetNextFileNum(uint64(i + 1))
			edit.SetLastSeqNum(binary.SeqNum(i * 50))
			expectedEdits[i] = edit

			if err := w.LogEdit(*edit); err != nil {
				t.Fatalf("LogEdit at index %d failed: %v", i, err)
			}
		}

		if err := w.Close(); err != nil {
			t.Fatalf("Close at start %d failed: %v", start, err)
		}
	}

	// Verify all 50 records intact across all reopen sessions
	records := readAllManifestRecords(t, path)
	if len(records) != totalEdits {
		t.Fatalf("expected %d total records across reopens, got %d", totalEdits, len(records))
	}

	for i := 0; i < totalEdits; i++ {
		decoded, err := DecodeVersionEdit(records[i])
		if err != nil {
			t.Fatalf("DecodeVersionEdit failed at index %d: %v", i, err)
		}
		if !decoded.Equal(expectedEdits[i]) {
			t.Fatalf("edit mismatch at index %d", i)
		}
	}
}

// -----------------------------------------------------------------------------
// FAULT INJECTION & POISONING TESTS (§26 & §32)
// -----------------------------------------------------------------------------

func TestManifestWriter_FaultInjection_WriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Inject write failure
	injectedErr := stdErrors.New("simulated disk full")
	w.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, injectedErr
	})

	edit := NewVersionEdit()
	edit.SetNextFileNum(1)

	err = w.LogEdit(*edit)
	if err == nil {
		t.Fatalf("expected LogEdit to fail under injected write error")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping root cause %v, got: %v", injectedErr, err)
	}

	if !w.IsPoisoned() {
		t.Errorf("expected writer to be poisoned after write failure")
	}

	// Subsequent calls must immediately fail-closed with ErrManifestWriterPoisoned
	err2 := w.LogEdit(*edit)
	if !stdErrors.Is(err2, errors.ErrManifestWriterPoisoned) {
		t.Errorf("expected ErrManifestWriterPoisoned on subsequent LogEdit, got: %v", err2)
	}
	if !stdErrors.Is(err2, injectedErr) {
		t.Errorf("poisoned error must preserve root cause %v", injectedErr)
	}

	// Close on poisoned writer must return the poisoning error without panicking
	closeErr := w.Close()
	if !stdErrors.Is(closeErr, errors.ErrManifestWriterPoisoned) {
		t.Errorf("Close on poisoned writer must return ErrManifestWriterPoisoned, got: %v", closeErr)
	}
}

func TestManifestWriter_FaultInjection_ShortWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Inject 0-byte short write without error
	w.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, nil
	})

	edit := NewVersionEdit()
	edit.SetNextFileNum(1)

	err = w.LogEdit(*edit)
	if err == nil {
		t.Fatalf("expected LogEdit to fail under short write")
	}
	if !stdErrors.Is(err, io.ErrShortWrite) {
		t.Errorf("expected error wrapping io.ErrShortWrite, got: %v", err)
	}
	if !w.IsPoisoned() {
		t.Errorf("expected writer to be poisoned after short write")
	}
}

func TestManifestWriter_FaultInjection_SyncError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	// Write succeeds to OS buffer, but fdatasync hardware barrier fails
	injectedErr := stdErrors.New("simulated I/O sync failure")
	w.SetSyncFnForTesting(func(f *os.File) error {
		return injectedErr
	})

	edit := NewVersionEdit()
	edit.SetNextFileNum(1)

	err = w.LogEdit(*edit)
	if err == nil {
		t.Fatalf("expected LogEdit to fail under injected sync failure")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping injected sync failure, got: %v", err)
	}

	if !w.IsPoisoned() {
		t.Errorf("writer MUST be poisoned if fdatasync fails")
	}

	// Verify fail-closed behavior on subsequent calls
	err2 := w.LogEdit(*edit)
	if !stdErrors.Is(err2, errors.ErrManifestWriterPoisoned) {
		t.Errorf("expected ErrManifestWriterPoisoned, got: %v", err2)
	}
}

func TestManifestWriter_FaultInjection_CloseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}

	injectedErr := stdErrors.New("simulated close failure")
	w.SetCloseFnForTesting(func(f *os.File) error {
		_ = f.Close() // prevent actual leak
		return injectedErr
	})

	err = w.Close()
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected Close to surface injected close error, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// CORRUPTION MATRIX (§42)
// -----------------------------------------------------------------------------

func TestManifestWriter_CorruptionMatrix(t *testing.T) {
	// Build a valid record
	edit := NewVersionEdit()
	edit.SetNextFileNum(42)
	edit.SetLastSeqNum(100)
	payload := edit.Encode()

	recordLen := ManifestHeaderSize + len(payload)
	validRecord := make([]byte, recordLen)
	binary.PutUint32(validRecord[4:8], uint32(len(payload)))
	copy(validRecord[8:], payload)
	crc := crc32.ChecksumIEEE(validRecord[4:])
	binary.PutUint32(validRecord[0:4], crc)

	// Sub-test 1: Truncate Header at every byte (0..7)
	for i := 0; i < ManifestHeaderSize; i++ {
		t.Run(fmt.Sprintf("TruncateHeader_%d_bytes", i), func(t *testing.T) {
			r := bytes.NewReader(validRecord[:i])
			_, _, err := decodeManifestRecord(r)
			if i == 0 {
				if !stdErrors.Is(err, io.EOF) {
					t.Errorf("expected io.EOF on 0 bytes, got: %v", err)
				}
			} else {
				if !stdErrors.Is(err, errors.ErrManifestHeaderTruncated) {
					t.Errorf("expected ErrManifestHeaderTruncated at length %d, got: %v", i, err)
				}
			}
		})
	}

	// Sub-test 2: Truncate Payload
	for i := ManifestHeaderSize; i < len(validRecord)-1; i++ {
		t.Run(fmt.Sprintf("TruncatePayload_%d_bytes", i), func(t *testing.T) {
			r := bytes.NewReader(validRecord[:i])
			_, _, err := decodeManifestRecord(r)
			if !stdErrors.Is(err, errors.ErrManifestPayloadTruncated) {
				t.Errorf("expected ErrManifestPayloadTruncated at length %d, got: %v", i, err)
			}
		})
	}

	// Sub-test 3: Flip CRC bit (offsets 0..3)
	for byteIdx := 0; byteIdx < 4; byteIdx++ {
		t.Run(fmt.Sprintf("FlipCRCBit_Byte_%d", byteIdx), func(t *testing.T) {
			corrupted := slicesClone(validRecord)
			corrupted[byteIdx] ^= 0x01
			_, _, err := decodeManifestRecord(bytes.NewReader(corrupted))
			if err == nil || !stdErrors.Is(err, errors.ErrChecksumMismatch) {
				t.Errorf("expected ErrChecksumMismatch on corrupted CRC, got: %v", err)
			}
		})
	}

	// Sub-test 4: Flip Payload bit (offsets 8..end)
	for byteIdx := 8; byteIdx < len(validRecord); byteIdx++ {
		t.Run(fmt.Sprintf("FlipPayloadBit_Byte_%d", byteIdx), func(t *testing.T) {
			corrupted := slicesClone(validRecord)
			corrupted[byteIdx] ^= 0x01
			_, _, err := decodeManifestRecord(bytes.NewReader(corrupted))
			if err == nil || !stdErrors.Is(err, errors.ErrChecksumMismatch) {
				t.Errorf("expected ErrChecksumMismatch on corrupted payload, got: %v", err)
			}
		})
	}

	// Sub-test 5: Mutate Length field (offsets 4..7)
	t.Run("MutateLengthField", func(t *testing.T) {
		corrupted := slicesClone(validRecord)
		corrupted[7] += 1 // increment length by 1
		_, _, err := decodeManifestRecord(bytes.NewReader(corrupted))
		if err == nil {
			t.Errorf("expected error on mutated length field")
		}
	})
}

// slicesClone is a test helper for byte slice cloning.
func slicesClone(s []byte) []byte {
	c := make([]byte, len(s))
	copy(c, s)
	return c
}

// -----------------------------------------------------------------------------
// SYMLINK & DIRECTORY DEFENSE TESTS (§37 & §38)
// -----------------------------------------------------------------------------

func TestManifestWriter_SymlinkDefense(t *testing.T) {
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "real_manifest")
	if err := os.WriteFile(targetFile, []byte("fake"), 0600); err != nil {
		t.Fatalf("failed to create target file: %v", err)
	}

	symlinkPath := filepath.Join(dir, "symlink_manifest")
	if err := os.Symlink(targetFile, symlinkPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	// OpenManifestWriter over symlink must be rejected
	if _, err := OpenManifestWriter(symlinkPath); err == nil {
		t.Errorf("OpenManifestWriter must reject symlink")
	}

	// CreateManifestWriter over symlink must be rejected
	if _, err := CreateManifestWriter(symlinkPath); err == nil {
		t.Errorf("CreateManifestWriter must reject symlink")
	}
}

func TestManifestWriter_DirectoryDefense(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "subdir_manifest")
	if err := os.Mkdir(subDir, 0700); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	// OpenManifestWriter over directory must be rejected
	if _, err := OpenManifestWriter(subDir); err == nil {
		t.Errorf("OpenManifestWriter must reject directory")
	}

	// CreateManifestWriter over directory must be rejected
	if _, err := CreateManifestWriter(subDir); err == nil {
		t.Errorf("CreateManifestWriter must reject directory")
	}
}

// -----------------------------------------------------------------------------
// CONCURRENCY TESTS (§28 & §29)
// -----------------------------------------------------------------------------

func TestManifestWriter_Concurrency_SerializedAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	const numGoroutines = 10
	const editsPerGoroutine = 5
	const totalExpected = numGoroutines * editsPerGoroutine

	var wg sync.WaitGroup
	errCh := make(chan error, totalExpected)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for e := 0; e < editsPerGoroutine; e++ {
				edit := NewVersionEdit()
				edit.SetNextFileNum(uint64(goroutineID*1000 + e))
				edit.SetLastSeqNum(binary.SeqNum(goroutineID*1000 + e))

				if err := w.LogEdit(*edit); err != nil {
					errCh <- fmt.Errorf("goroutine %d edit %d failed: %w", goroutineID, e, err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent LogEdit error: %v", err)
	}

	if w.RecordCount() != totalExpected {
		t.Errorf("RecordCount mismatch: got %d, want %d", w.RecordCount(), totalExpected)
	}

	// Verify all records parsed cleanly without corruption or interleaving
	records := readAllManifestRecords(t, path)
	if len(records) != totalExpected {
		t.Fatalf("expected %d records in file, got %d", totalExpected, len(records))
	}
}

// -----------------------------------------------------------------------------
// MANIFEST FILENAME & PATH HELPER TESTS (§9)
// -----------------------------------------------------------------------------

func TestManifestFilenameAndPath(t *testing.T) {
	tests := []struct {
		num      uint64
		expected string
	}{
		{1, "MANIFEST-000001"},
		{42, "MANIFEST-000042"},
		{999999, "MANIFEST-999999"},
		{1000000, "MANIFEST-1000000"},
	}

	for _, tc := range tests {
		got := ManifestFilename(tc.num)
		if got != tc.expected {
			t.Errorf("ManifestFilename(%d) mismatch: got %s, want %s", tc.num, got, tc.expected)
		}

		path := ManifestPath("/var/data", tc.num)
		expectedPath := filepath.Join("/var/data", tc.expected)
		if path != expectedPath {
			t.Errorf("ManifestPath(%d) mismatch: got %s, want %s", tc.num, path, expectedPath)
		}
	}
}

// -----------------------------------------------------------------------------
// FUZZ TESTING (§43)
// -----------------------------------------------------------------------------

func FuzzManifestRecordDecode(f *testing.F) {
	// Seed 1: Empty record fixture
	f.Add([]byte{0xa8, 0x3e, 0xf6, 0xca, 0x00, 0x00, 0x00, 0x01, 0x01})

	// Seed 2: Scalar record fixture
	f.Add([]byte{0xae, 0xc6, 0x46, 0x60, 0x00, 0x00, 0x00, 0x07, 0x01, 0x01, 0x01, 0x2a, 0x02, 0x01, 0x64})

	// Seed 3: Truncated header
	f.Add([]byte{0xa8, 0x3e, 0xf6})

	// Seed 4: Corrupted CRC
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0x00, 0x00, 0x00, 0x01, 0x01})

	// Seed 5: Large length field
	f.Add([]byte{0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		payload, _, err := decodeManifestRecord(r)
		if err == nil {
			// If framing and CRC pass, payload should decode without panic
			_, _ = DecodeVersionEdit(payload)
		}
	})
}

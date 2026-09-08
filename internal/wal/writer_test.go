package wal_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestSegmentHelpers verifies segment naming and path formatting according to Section 18.2.
func TestSegmentHelpers(t *testing.T) {
	if got := wal.SegmentName(1); got != "wal_000000000001.log" {
		t.Errorf("SegmentName(1) mismatch: got %q, want %q", got, "wal_000000000001.log")
	}
	if got := wal.SegmentName(42); got != "wal_000000000042.log" {
		t.Errorf("SegmentName(42) mismatch: got %q, want %q", got, "wal_000000000042.log")
	}
	if got := wal.SegmentName(999999999999); got != "wal_999999999999.log" {
		t.Errorf("SegmentName(large) mismatch: got %q, want %q", got, "wal_999999999999.log")
	}

	dbPath := "/tmp/test_db"
	expectedPath := filepath.Join(dbPath, "wal", "wal_000000000007.log")
	if got := wal.SegmentPath(dbPath, 7); got != expectedPath {
		t.Errorf("SegmentPath mismatch: got %q, want %q", got, expectedPath)
	}
}

// TestGroup 1: Fresh WAL Append
func TestWriter_FreshWALAppend(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1),
		Timestamp: 1725800000,
		Key:       []byte("user_alpha"),
		Value:     []byte("payload_alpha_content"),
	}

	if err := w.AppendSync(rec); err != nil {
		t.Fatalf("AppendSync failed: %v", err)
	}

	// Verify file existence and exact size
	fi, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	expectedSize := int64(wal.MinRecordSize + len(rec.Key) + len(rec.Value))
	if fi.Size() != expectedSize {
		t.Errorf("file size mismatch: got %d, want %d", fi.Size(), expectedSize)
	}

	// Read and decode using authoritative DecodeRecord
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	decoded, err := wal.DecodeRecord(f)
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}
	if !rec.Equal(decoded) {
		t.Fatalf("decoded record mismatch: got %+v, want %+v", decoded, rec)
	}

	// Ensure stream cleanly hits EOF
	_, err = wal.DecodeRecord(f)
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF at end of file, got %v", err)
	}
}

// TestGroup 2: Delete Record
func TestWriter_DeleteRecord(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	delRec := wal.Record{
		Type:      wal.RecordTypeDelete,
		SeqNum:    binary.SeqNum(2),
		Timestamp: 1725800001,
		Key:       []byte("tombstone_key"),
		Value:     nil,
	}

	if err := w.AppendSync(delRec); err != nil {
		t.Fatalf("AppendSync failed: %v", err)
	}

	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	decoded, err := wal.DecodeRecord(f)
	if err != nil {
		t.Fatalf("DecodeRecord failed: %v", err)
	}

	if decoded.Type != wal.RecordTypeDelete {
		t.Errorf("expected DELETE type, got %s", decoded.Type)
	}
	if !bytes.Equal(decoded.Key, delRec.Key) {
		t.Errorf("key mismatch: got %q, want %q", decoded.Key, delRec.Key)
	}
	if len(decoded.Value) != 0 {
		t.Errorf("expected empty value for DELETE, got %d bytes", len(decoded.Value))
	}
}

// TestGroup 3: Batch Markers
func TestWriter_BatchMarkers(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	startRec := wal.Record{
		Type:      wal.RecordTypeBatchStart,
		SeqNum:    binary.SeqNum(10),
		Timestamp: 1725800010,
	}
	commitRec := wal.Record{
		Type:      wal.RecordTypeBatchCommit,
		SeqNum:    binary.SeqNum(11),
		Timestamp: 1725800011,
	}

	if err := w.AppendSync(startRec); err != nil {
		t.Fatalf("AppendSync start failed: %v", err)
	}
	if err := w.AppendSync(commitRec); err != nil {
		t.Fatalf("AppendSync commit failed: %v", err)
	}

	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	dStart, err := wal.DecodeRecord(f)
	if err != nil {
		t.Fatalf("decode start failed: %v", err)
	}
	if !startRec.Equal(dStart) {
		t.Errorf("start record mismatch: got %+v, want %+v", dStart, startRec)
	}

	dCommit, err := wal.DecodeRecord(f)
	if err != nil {
		t.Fatalf("decode commit failed: %v", err)
	}
	if !commitRec.Equal(dCommit) {
		t.Errorf("commit record mismatch: got %+v, want %+v", dCommit, commitRec)
	}
}

// TestGroup 4: Multiple Records
func TestWriter_MultipleRecordsSequential(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	records := []wal.Record{
		{Type: wal.RecordTypePut, SeqNum: 101, Timestamp: 1000, Key: []byte("k1"), Value: []byte("v1")},
		{Type: wal.RecordTypePut, SeqNum: 102, Timestamp: 1001, Key: []byte("k2"), Value: []byte("v2_longer")},
		{Type: wal.RecordTypeDelete, SeqNum: 103, Timestamp: 1002, Key: []byte("k3"), Value: nil},
		{Type: wal.RecordTypePut, SeqNum: 104, Timestamp: 1003, Key: []byte("k4"), Value: []byte("v4_data")},
	}

	var expectedBytes []byte
	for _, r := range records {
		if err := w.AppendSync(r); err != nil {
			t.Fatalf("AppendSync failed: %v", err)
		}
		enc, err := wal.EncodeRecord(r)
		if err != nil {
			t.Fatalf("EncodeRecord failed: %v", err)
		}
		expectedBytes = append(expectedBytes, enc...)
	}

	fileBytes, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}
	if !bytes.Equal(fileBytes, expectedBytes) {
		t.Fatalf("file bytes mismatch with concatenated encoded records")
	}

	// Sequential stream decode
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	for i, wantRec := range records {
		gotRec, err := wal.DecodeRecord(f)
		if err != nil {
			t.Fatalf("record %d decode failed: %v", i, err)
		}
		if !wantRec.Equal(gotRec) {
			t.Errorf("record %d mismatch: got %+v, want %+v", i, gotRec, wantRec)
		}
	}

	_, err = wal.DecodeRecord(f)
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF after all records, got %v", err)
	}
}

// TestGroup 5 & 6: Reopen Existing File & No Truncation
func TestWriter_ReopenExistingFilePreservesRecords(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	recA := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1),
		Timestamp: 1000,
		Key:       []byte("record_a_key"),
		Value:     []byte("record_a_val"),
	}
	recB := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(2),
		Timestamp: 2000,
		Key:       []byte("record_b_key"),
		Value:     []byte("record_b_val"),
	}

	// Writer 1 writes record A and closes
	w1, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter 1 failed: %v", err)
	}
	if err := w1.AppendSync(recA); err != nil {
		t.Fatalf("w1 AppendSync failed: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("w1 Close failed: %v", err)
	}

	bytesAfterA, err := os.ReadFile(w1.Path())
	if err != nil {
		t.Fatalf("ReadFile after A failed: %v", err)
	}

	// Writer 2 reopens the same segment file, appends record B, and closes
	w2, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter 2 failed: %v", err)
	}
	if err := w2.AppendSync(recB); err != nil {
		t.Fatalf("w2 AppendSync failed: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("w2 Close failed: %v", err)
	}

	bytesAfterB, err := os.ReadFile(w2.Path())
	if err != nil {
		t.Fatalf("ReadFile after B failed: %v", err)
	}

	// Verify Requirement 6 (No Truncation): original bytes of record A remain prefix of file
	if !bytes.HasPrefix(bytesAfterB, bytesAfterA) {
		t.Fatalf("bytes of record A were altered or truncated upon reopening")
	}

	// Read both records sequentially
	f, err := os.Open(w2.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	d1, err := wal.DecodeRecord(f)
	if err != nil {
		t.Fatalf("decode recA failed: %v", err)
	}
	if !recA.Equal(d1) {
		t.Errorf("recA mismatch: got %+v, want %+v", d1, recA)
	}

	d2, err := wal.DecodeRecord(f)
	if err != nil {
		t.Fatalf("decode recB failed: %v", err)
	}
	if !recB.Equal(d2) {
		t.Errorf("recB mismatch: got %+v, want %+v", d2, recB)
	}

	_, err = wal.DecodeRecord(f)
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF after recB, got %v", err)
	}
}

// TestGroup 7: Invalid Record
func TestWriter_InvalidRecordRejected(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 1)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	invalidCases := []struct {
		name string
		rec  wal.Record
	}{
		{
			name: "invalid record type",
			rec:  wal.Record{Type: wal.RecordTypeInvalid, Key: []byte("k"), Value: []byte("v")},
		},
		{
			name: "empty PUT key",
			rec:  wal.Record{Type: wal.RecordTypePut, Key: nil, Value: []byte("v")},
		},
		{
			name: "oversized PUT key",
			rec:  wal.Record{Type: wal.RecordTypePut, Key: make([]byte, binary.MaxKeyLen+1), Value: []byte("v")},
		},
		{
			name: "oversized PUT value",
			rec:  wal.Record{Type: wal.RecordTypePut, Key: []byte("k"), Value: make([]byte, binary.MaxValueLen+1)},
		},
		{
			name: "DELETE with value payload",
			rec:  wal.Record{Type: wal.RecordTypeDelete, Key: []byte("k"), Value: []byte("illegal_value")},
		},
		{
			name: "BATCH_START with key",
			rec:  wal.Record{Type: wal.RecordTypeBatchStart, Key: []byte("illegal_key")},
		},
		{
			name: "BATCH_COMMIT with value",
			rec:  wal.Record{Type: wal.RecordTypeBatchCommit, Value: []byte("illegal_value")},
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			err := w.AppendSync(tc.rec)
			if err == nil {
				t.Fatalf("expected error for invalid record %s, got nil", tc.name)
			}
		})
	}

	// Verify file remains 0 bytes (no partial or invalid data written)
	fi, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("expected empty file after invalid appends, got %d bytes", fi.Size())
	}
}

// TestGroup 8: Short Write
func TestWriter_ShortWriteHandled(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	t.Run("chunked short write loop succeeds", func(t *testing.T) {
		w, err := wal.OpenSegmentWriter(dbPath, 10)
		if err != nil {
			t.Fatalf("OpenSegmentWriter failed: %v", err)
		}
		defer func() { _ = w.Close() }()

		// Seam: write at most 5 bytes per write call
		w.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
			limit := len(p)
			if limit > 5 {
				limit = 5
			}
			return f.Write(p[:limit])
		})

		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    1,
			Timestamp: 100,
			Key:       []byte("short_write_key"),
			Value:     []byte("short_write_val_payload"),
		}

		if err := w.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync with chunked writes failed: %v", err)
		}

		// Verify record decoded back cleanly
		f, err := os.Open(w.Path())
		if err != nil {
			t.Fatalf("os.Open failed: %v", err)
		}
		defer func() { _ = f.Close() }()

		d, err := wal.DecodeRecord(f)
		if err != nil {
			t.Fatalf("DecodeRecord failed: %v", err)
		}
		if !rec.Equal(d) {
			t.Errorf("record mismatch: got %+v, want %+v", d, rec)
		}
	})

	t.Run("persistent zero byte write returns ErrShortWrite", func(t *testing.T) {
		w, err := wal.OpenSegmentWriter(dbPath, 11)
		if err != nil {
			t.Fatalf("OpenSegmentWriter failed: %v", err)
		}
		defer func() { _ = w.Close() }()

		// Seam: write 0 bytes without error
		w.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
			return 0, nil
		})

		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    1,
			Timestamp: 100,
			Key:       []byte("k"),
			Value:     []byte("v"),
		}

		err = w.AppendSync(rec)
		if err == nil {
			t.Fatalf("expected error on persistent 0-byte write, got nil")
		}
		if !stdErrors.Is(err, io.ErrShortWrite) {
			t.Errorf("expected io.ErrShortWrite, got %v", err)
		}
	})
}

// TestGroup 9: Write Failure
func TestWriter_WriteFailureReturned(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 20)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	injectedErr := stdErrors.New("simulated disk I/O write error")
	w.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, injectedErr
	})

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("key"),
		Value:     []byte("val"),
	}

	err = w.AppendSync(rec)
	if err == nil {
		t.Fatalf("expected write error, got nil")
	}
	if !stdErrors.Is(err, injectedErr) {
		t.Errorf("expected error wrapping injectedErr, got %v", err)
	}
}

// TestGroup 10: Sync Failure ("Strict Sync" Invariant)
func TestWriter_SyncFailureReturned(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 30)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	injectedSyncErr := stdErrors.New("simulated fdatasync failure on storage controller")
	w.SetSyncFnForTesting(func(f *os.File) error {
		return injectedSyncErr
	})

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("sync_fail_key"),
		Value:     []byte("sync_fail_val"),
	}

	// CRITICAL INVARIANT: AppendSync MUST NOT report success when sync fails!
	err = w.AppendSync(rec)
	if err == nil {
		t.Fatalf("STRICT SYNC VIOLATION: AppendSync returned nil despite fdatasync failure!")
	}
	if !stdErrors.Is(err, injectedSyncErr) {
		t.Errorf("expected error wrapping injectedSyncErr, got %v", err)
	}
}

// TestGroup 11: Write Success + Sync Success
func TestWriter_WriteSuccessAndSyncSuccess(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 40)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	syncCalled := false
	w.SetSyncFnForTesting(func(f *os.File) error {
		syncCalled = true
		return f.Sync()
	})

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("k"),
		Value:     []byte("v"),
	}

	if err := w.AppendSync(rec); err != nil {
		t.Fatalf("AppendSync failed: %v", err)
	}
	if !syncCalled {
		t.Fatalf("expected durability barrier (syncFn) to be invoked")
	}
}

// TestGroup 12: Closed Writer
func TestWriter_ClosedWriter(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 50)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Second close is idempotent
	if err := w.Close(); err != nil {
		t.Fatalf("subsequent Close must return nil, got %v", err)
	}

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       []byte("k"),
		Value:     []byte("v"),
	}

	err = w.AppendSync(rec)
	if err == nil {
		t.Fatalf("expected error when appending to closed writer, got nil")
	}
	if !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Errorf("expected errors.ErrWriterClosed, got %v", err)
	}
}

// TestGroup 13: Concurrent Appenders (No Interleaving)
func TestWriter_ConcurrentAppenders(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 60)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			rec := wal.Record{
				Type:      wal.RecordTypePut,
				SeqNum:    binary.SeqNum(idx + 1),
				Timestamp: uint64(1000 + idx),
				Key:       []byte(fmt.Sprintf("user_key_%04d", idx)),
				Value:     []byte(fmt.Sprintf("user_val_%04d_padding_data_block", idx)),
			}
			if err := w.AppendSync(rec); err != nil {
				errCh <- fmt.Errorf("goroutine %d AppendSync failed: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}

	// Verify all 50 records can be sequentially parsed without checksum errors or torn writes
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	recoveredKeys := make(map[string]bool)
	for i := 0; i < numGoroutines; i++ {
		rec, err := wal.DecodeRecord(f)
		if err != nil {
			t.Fatalf("record %d decode failed: %v (corruption or record interleaving detected)", i, err)
		}
		recoveredKeys[string(rec.Key)] = true
	}

	if len(recoveredKeys) != numGoroutines {
		t.Errorf("expected %d unique recovered keys, got %d", numGoroutines, len(recoveredKeys))
	}

	_, err = wal.DecodeRecord(f)
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF after %d records, got %v", numGoroutines, err)
	}
}

// TestGroup 14: File Permissions
func TestWriter_FilePermissions(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 70)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	fi, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}

	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != wal.FileMode {
			t.Errorf("expected file mode %04o, got %04o", wal.FileMode, fi.Mode().Perm())
		}
	}
}

// TestGroup 15: Descriptor Lifecycle (No Leaks)
func TestWriter_DescriptorLifecycle(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	// Repeatedly open, append, and close across 100 iterations
	for i := 0; i < 100; i++ {
		w, err := wal.OpenSegmentWriter(dbPath, uint64(i+100))
		if err != nil {
			t.Fatalf("iteration %d open failed: %v", i, err)
		}
		rec := wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(i + 1),
			Timestamp: uint64(i),
			Key:       []byte("k"),
			Value:     []byte("v"),
		}
		if err := w.AppendSync(rec); err != nil {
			t.Fatalf("iteration %d append failed: %v", i, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("iteration %d close failed: %v", i, err)
		}
	}
}

// TestGroup 16: Input Immutability
func TestWriter_InputImmutability(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 80)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	keyOriginal := []byte("immutable_key_12345")
	valOriginal := []byte("immutable_value_67890_content")

	keyInput := append([]byte(nil), keyOriginal...)
	valInput := append([]byte(nil), valOriginal...)

	rec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    1,
		Timestamp: 100,
		Key:       keyInput,
		Value:     valInput,
	}

	if err := w.AppendSync(rec); err != nil {
		t.Fatalf("AppendSync failed: %v", err)
	}

	// Assert caller memory is byte-for-byte untouched
	if !bytes.Equal(keyInput, keyOriginal) {
		t.Fatalf("key memory was mutated by AppendSync! got %q, want %q", keyInput, keyOriginal)
	}
	if !bytes.Equal(valInput, valOriginal) {
		t.Fatalf("value memory was mutated by AppendSync! got %q, want %q", valInput, valOriginal)
	}
}

// TestGroup 17: Cross-Check 1,000 Records
func TestWriter_OneThousandRecordsReplay(t *testing.T) {
	dbPath := t.TempDir()
	if _, err := wal.InitDir(dbPath); err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	w, err := wal.OpenSegmentWriter(dbPath, 90)
	if err != nil {
		t.Fatalf("OpenSegmentWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	const numRecords = 1000
	records := make([]wal.Record, numRecords)
	for i := 0; i < numRecords; i++ {
		records[i] = wal.Record{
			Type:      wal.RecordTypePut,
			SeqNum:    binary.SeqNum(i + 1),
			Timestamp: uint64(1725800000 + i),
			Key:       []byte(fmt.Sprintf("key_%06d", i)),
			Value:     []byte(fmt.Sprintf("value_%06d_data_payload_block", i)),
		}
		if err := w.AppendSync(records[i]); err != nil {
			t.Fatalf("AppendSync record %d failed: %v", i, err)
		}
	}

	// Sequential replay and verification with DecodeRecord
	f, err := os.Open(w.Path())
	if err != nil {
		t.Fatalf("os.Open failed: %v", err)
	}
	defer func() { _ = f.Close() }()

	for i := 0; i < numRecords; i++ {
		decoded, err := wal.DecodeRecord(f)
		if err != nil {
			t.Fatalf("record %d decode failed: %v", i, err)
		}
		if !records[i].Equal(decoded) {
			t.Fatalf("record %d mismatch: got %+v, want %+v", i, decoded, records[i])
		}
	}

	_, err = wal.DecodeRecord(f)
	if !stdErrors.Is(err, io.EOF) {
		t.Errorf("expected EOF at end of 1,000 records, got %v", err)
	}
}

// TestWriter_PathSafetyAndErrorHandling tests opening failure modes:
// empty path, opening directory, opening symlink.
func TestWriter_PathSafetyAndErrorHandling(t *testing.T) {
	t.Run("empty path rejected", func(t *testing.T) {
		_, err := wal.OpenWriter("")
		if err == nil {
			t.Fatalf("expected error on empty path, got nil")
		}
		if !stdErrors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})

	t.Run("directory path rejected", func(t *testing.T) {
		dir := t.TempDir()
		_, err := wal.OpenWriter(dir)
		if err == nil {
			t.Fatalf("expected error on opening directory as writer, got nil")
		}
		var notDirErr *errors.NotADirectoryError
		if !stdErrors.As(err, &notDirErr) {
			t.Errorf("expected *errors.NotADirectoryError, got %v", err)
		}
	})

	t.Run("symlink rejected", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.log")
		if err := os.WriteFile(target, []byte("hello"), 0600); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		symlink := filepath.Join(dir, "symlink.log")
		if err := os.Symlink(target, symlink); err != nil {
			t.Skipf("symlink creation not supported: %v", err)
		}

		_, err := wal.OpenWriter(symlink)
		if err == nil {
			t.Fatalf("expected error when opening symlink, got nil")
		}
	})

	t.Run("Path accessor", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "wal_000000000001.log")
		w, err := wal.OpenWriter(filePath)
		if err != nil {
			t.Fatalf("OpenWriter failed: %v", err)
		}
		defer func() { _ = w.Close() }()

		if w.Path() != filePath {
			t.Errorf("Path() mismatch: got %q, want %q", w.Path(), filePath)
		}
	})

	t.Run("non-existent parent directory fails", func(t *testing.T) {
		nonExistent := filepath.Join(t.TempDir(), "does_not_exist", "wal_000000000001.log")
		_, err := wal.OpenWriter(nonExistent)
		if err == nil {
			t.Fatalf("expected error when opening in non-existent parent, got nil")
		}
	})

	t.Run("Close with sync error returns error", func(t *testing.T) {
		dir := t.TempDir()
		filePath := filepath.Join(dir, "wal_000000000002.log")
		w, err := wal.OpenWriter(filePath)
		if err != nil {
			t.Fatalf("OpenWriter failed: %v", err)
		}

		injectedErr := stdErrors.New("simulated sync error during close")
		w.SetSyncFnForTesting(func(f *os.File) error {
			return injectedErr
		})

		err = w.Close()
		if err == nil {
			t.Fatalf("expected error when sync fails during Close, got nil")
		}
		if !stdErrors.Is(err, injectedErr) {
			t.Errorf("expected error wrapping injectedErr, got %v", err)
		}
	})
}

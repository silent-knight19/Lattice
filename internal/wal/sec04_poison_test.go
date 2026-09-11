package wal_test

import (
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/wal"
)

// TestSecurity_Remediation4_PartialWritePoisoning asserts that:
// 1. A partial write during Append or AppendSync transitions WALWriter into a poisoned state.
// 2. All subsequent operations (Append, AppendSync, Sync, Size) are rejected with ErrWriterPoisoned.
// 3. The original I/O error is preserved across subsequent rejections.
func TestSecurity_Remediation4_PartialWritePoisoning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poison_test.log")

	writer, err := wal.CreateWriter(path)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	// 1. Write initial valid record
	rec1 := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("key1"), Value: []byte("value1")}
	if err := writer.AppendSync(rec1); err != nil {
		t.Fatalf("initial AppendSync failed: %v", err)
	}

	// 2. Inject partial write fault: write 10 bytes then fail
	injectedErr := stdErrors.New("simulated partial write failure: disk I/O error")
	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		if len(p) > 10 {
			// Write first 10 bytes then fail
			n, _ := f.Write(p[:10])
			return n, injectedErr
		}
		return f.Write(p)
	})

	rec2 := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("key2"), Value: []byte("value2")}

	errAppend := writer.Append(rec2)
	if errAppend == nil {
		t.Fatal("expected error on partial write, got nil")
	}
	if !stdErrors.Is(errAppend, injectedErr) {
		t.Fatalf("expected error wrapping injectedErr, got %v", errAppend)
	}

	// 3. Assert writer is now permanently poisoned
	if !writer.IsPoisoned() {
		t.Fatal("expected writer.IsPoisoned() to be true after partial write")
	}

	// 4. Restore normal write function; subsequent operations must STILL fail
	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return f.Write(p)
	})

	rec3 := wal.Record{Type: wal.RecordTypePut, SeqNum: 3, Timestamp: 102, Key: []byte("key3"), Value: []byte("value3")}

	// Append must be rejected
	errAfter := writer.Append(rec3)
	if !stdErrors.Is(errAfter, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned from Append, got %v", errAfter)
	}
	if !stdErrors.Is(errAfter, injectedErr) {
		t.Fatalf("expected preserved root cause error in Append, got %v", errAfter)
	}

	// AppendSync must be rejected
	errSyncAppend := writer.AppendSync(rec3)
	if !stdErrors.Is(errSyncAppend, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned from AppendSync, got %v", errSyncAppend)
	}

	// Sync must be rejected
	errSync := writer.Sync()
	if !stdErrors.Is(errSync, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned from Sync, got %v", errSync)
	}

	// Size must be rejected
	_, errSize := writer.Size()
	if !stdErrors.Is(errSize, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned from Size, got %v", errSize)
	}

	// Close must return error wrapping ErrWriterPoisoned while closing descriptor
	errClose := writer.Close()
	if !stdErrors.Is(errClose, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned from Close, got %v", errClose)
	}
}

// TestSecurity_Remediation4_SyncFailurePoisoning asserts that a failure during
// fdatasync poisons the writer to prevent silent durability violations.
func TestSecurity_Remediation4_SyncFailurePoisoning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sync_poison.log")

	writer, err := wal.CreateWriter(path)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	injectedSyncErr := stdErrors.New("simulated fdatasync hardware flush failure")
	writer.SetSyncFnForTesting(func(f *os.File) error {
		return injectedSyncErr
	})

	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("key1"), Value: []byte("val1")}
	err = writer.AppendSync(rec)
	if !stdErrors.Is(err, injectedSyncErr) {
		t.Fatalf("expected injectedSyncErr from AppendSync, got %v", err)
	}

	if !writer.IsPoisoned() {
		t.Fatal("expected writer to be poisoned after sync failure")
	}

	// Subsequent append rejected
	rec2 := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("key2"), Value: []byte("val2")}
	errAppend := writer.Append(rec2)
	if !stdErrors.Is(errAppend, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned after sync failure, got %v", errAppend)
	}
}

// TestSecurity_Remediation4_ZeroByteWriteError asserts that an immediate I/O error
// with zero bytes written poisons the writer if the descriptor/filesystem failed.
func TestSecurity_Remediation4_ZeroByteWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zerobyte_poison.log")

	writer, err := wal.CreateWriter(path)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	injectedErr := stdErrors.New("simulated immediate disk I/O error")
	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, injectedErr
	})

	rec := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("key"), Value: []byte("val")}
	err = writer.Append(rec)
	if !stdErrors.Is(err, injectedErr) {
		t.Fatalf("expected injectedErr, got %v", err)
	}

	if !writer.IsPoisoned() {
		t.Fatal("expected writer to be poisoned after zero-byte write error")
	}

	err2 := writer.Append(rec)
	if !stdErrors.Is(err2, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned on subsequent append, got %v", err2)
	}
}

// TestSecurity_Remediation4_NoCorruptedMiddleRecord asserts the primary security invariant:
// A failed write CANNOT be followed by subsequent appends producing a corrupted middle record.
// Instead, after failure, recovery cleanly truncates the torn tail, and reopening allows safe appends.
func TestSecurity_Remediation4_NoCorruptedMiddleRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no_corrupt_middle.log")

	writer, err := wal.CreateWriter(path)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}

	// 1. Write record A cleanly
	recA := wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("record-A"), Value: []byte("value-A")}
	if err := writer.AppendSync(recA); err != nil {
		t.Fatalf("AppendSync record A failed: %v", err)
	}

	// 2. Inject partial write on record B (write 15 bytes of header then error)
	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		if len(p) > 15 {
			n, _ := f.Write(p[:15])
			return n, io.ErrUnexpectedEOF
		}
		return f.Write(p)
	})

	recB := wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("record-B"), Value: []byte("value-B")}
	_ = writer.Append(recB)

	// 3. Attempt to append record C via the same writer. Must be REJECTED!
	recC := wal.Record{Type: wal.RecordTypePut, SeqNum: 3, Timestamp: 102, Key: []byte("record-C"), Value: []byte("value-C")}
	errC := writer.Append(recC)
	if !stdErrors.Is(errC, errors.ErrWriterPoisoned) {
		t.Fatalf("SECURITY VIOLATION: record C was not rejected on poisoned writer: %v", errC)
	}

	_ = writer.Close()

	// 4. Run WAL Recovery: must detect and safely truncate torn tail (record B)
	recovRes, err := wal.RecoverSegment(path)
	if err != nil {
		t.Fatalf("RecoverSegment failed: %v", err)
	}
	if !recovRes.Truncated {
		t.Fatal("expected RecoverSegment to truncate partial tail")
	}
	if recovRes.ValidRecords != 1 {
		t.Fatalf("expected 1 valid record (record A), got %d", recovRes.ValidRecords)
	}

	// 5. Reopen fresh writer: must allow appending new record C safely at clean EOF
	writer2, err := wal.OpenWriter(path)
	if err != nil {
		t.Fatalf("OpenWriter after recovery failed: %v", err)
	}
	defer func() { _ = writer2.Close() }()

	if err := writer2.AppendSync(recC); err != nil {
		t.Fatalf("AppendSync record C on reopened writer failed: %v", err)
	}

	// 6. Verify log contents: exactly record A followed by record C, with zero corrupt bytes
	reader, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader failed: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var records []wal.Record
	for {
		rec, err := reader.Next()
		if stdErrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next failed: %v", err)
		}
		records = append(records, rec)
	}

	if len(records) != 2 {
		t.Fatalf("expected exactly 2 records, got %d", len(records))
	}
	if string(records[0].Key) != "record-A" || string(records[1].Key) != "record-C" {
		t.Fatalf("unexpected record sequence: got [%s, %s]", records[0].Key, records[1].Key)
	}
}

// TestSecurity_Remediation4_PoisonConcurrency asserts thread-safety under concurrent appends
// when a failure triggers poisoning.
func TestSecurity_Remediation4_PoisonConcurrency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poison_concurrent.log")

	writer, err := wal.CreateWriter(path)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}
	defer func() { _ = writer.Close() }()

	var once sync.Once
	injectedErr := stdErrors.New("concurrent injected failure")

	writer.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		var triggered bool
		once.Do(func() {
			triggered = true
		})
		if triggered {
			// Partial write on first encounter
			n, _ := f.Write(p[:5])
			return n, injectedErr
		}
		return f.Write(p)
	})

	const numGoroutines = 16
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			rec := wal.Record{
				Type:      wal.RecordTypePut,
				SeqNum:    binary.SeqNum(workerID + 1),
				Timestamp: uint64(workerID + 1),
				Key:       []byte(fmt.Sprintf("k-%d", workerID)),
				Value:     []byte("val"),
			}
			_ = writer.AppendSync(rec)
		}()
	}

	wg.Wait()

	if !writer.IsPoisoned() {
		t.Fatal("expected writer to be poisoned after concurrent failure")
	}

	// All future appends must fail
	recAfter := wal.Record{Type: wal.RecordTypePut, SeqNum: 999, Timestamp: 999, Key: []byte("k"), Value: []byte("v")}
	if err := writer.Append(recAfter); !stdErrors.Is(err, errors.ErrWriterPoisoned) {
		t.Fatalf("expected ErrWriterPoisoned after concurrency test, got %v", err)
	}
}

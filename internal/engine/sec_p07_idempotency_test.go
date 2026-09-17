package engine_test

import (
	"bytes"
	"crypto/sha256"
	stdErrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// hashDirectoryContents computes a composite SHA-256 hash of all regular files in dbPath.
func hashDirectoryContents(t *testing.T, dbPath string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dbPath)
	if err != nil {
		t.Fatalf("failed to read dir %s: %v", dbPath, err)
	}
	h := sha256.New()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dbPath, entry.Name()))
		if readErr != nil {
			t.Fatalf("failed to read file %s: %v", entry.Name(), readErr)
		}
		h.Write([]byte(entry.Name()))
		h.Write(data)
	}
	return h.Sum(nil)
}

// TestSEC_P07_002_RecoveryIdempotency_DoubleRecovery proves SEC-P07-002:
// Running recovery sequentially on independent engine instances against the exact same
// persistent storage state produces strictly identical reconstructed states (active MemTable,
// sequence watermarks, file number watermarks, and version hierarchy) with zero disk drift.
func TestSEC_P07_002_RecoveryIdempotency_DoubleRecovery(t *testing.T) {
	dir := t.TempDir()

	// 1. Create a dummy physical SSTable file matching FileSize: 1024
	sstPath := version.TablePath(dir, 1)
	if err := os.WriteFile(sstPath, make([]byte, 1024), 0600); err != nil {
		t.Fatalf("failed to write dummy sstable: %v", err)
	}

	// 2. Write MANIFEST-000001 with 000001.sst added at Level 0
	edit := version.NewVersionEdit()
	edit.SetNextFileNum(2)
	edit.SetLastSeqNum(10)
	sk, _ := binary.NewInternalKey([]byte("init_a"), 1, binary.OpTypePut)
	lk, _ := binary.NewInternalKey([]byte("init_z"), 10, binary.OpTypePut)
	if err := edit.AddFile(0, version.FileMetadata{
		FileNum:        1,
		FileSize:       1024,
		SmallestKey:    binary.EncodeInternalKey(sk),
		LargestKey:     binary.EncodeInternalKey(lk),
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	}); err != nil {
		t.Fatalf("edit.AddFile failed: %v", err)
	}

	manifestPath := version.ManifestPath(dir, 1)
	mw, err := version.CreateManifestWriter(manifestPath)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	if err := mw.LogEdit(*edit); err != nil {
		t.Fatalf("LogEdit failed: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close failed: %v", err)
	}

	// 3. Set CURRENT to point to MANIFEST-000001
	if err := version.SetCurrentManifest(dir, 1); err != nil {
		t.Fatalf("SetCurrentManifest failed: %v", err)
	}

	// 4. Write uncommitted WAL records in segment 1 (SeqNum 11..13)
	writeWALSegment(t, dir, 1,
		makePutRecord(11, "k1", "v1"),
		makePutRecord(12, "k2", "v2"),
		makeDeleteRecord(13, "k1"),
	)

	// Record composite hash of the persistent directory before any recovery
	hashBefore := hashDirectoryContents(t, dir)

	// --- Pass 1: Recover on Engine 1 ---
	eng1 := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	if err := eng1.RecoverWAL(); err != nil {
		t.Fatalf("Eng1 RecoverWAL failed: %v", err)
	}

	val1_k2, err1 := eng1.Get([]byte("k2"))
	if err1 != nil || string(val1_k2) != "v2" {
		t.Fatalf("Eng1 Get(k2) unexpected: val=%s, err=%v", string(val1_k2), err1)
	}
	_, err1_k1 := eng1.Get([]byte("k1"))
	if !stdErrors.Is(err1_k1, errors.ErrKeyNotFound) {
		t.Fatalf("Eng1 Get(k1) expected ErrKeyNotFound for tombstone, got: %v", err1_k1)
	}
	seq1 := eng1.NextSeqNum()
	fileNum1 := eng1.NextFileNum()
	_ = eng1.Close()

	// Verify disk hash after Pass 1: recovery MUST NOT mutate persistent files
	hashAfterPass1 := hashDirectoryContents(t, dir)
	if !bytes.Equal(hashBefore, hashAfterPass1) {
		t.Fatalf("SECURITY VIOLATION: storage state mutated during recovery Pass 1")
	}

	// --- Pass 2: Recover on Engine 2 (Exact same directory) ---
	eng2 := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	if err := eng2.RecoverWAL(); err != nil {
		t.Fatalf("Eng2 RecoverWAL failed: %v", err)
	}

	val2_k2, err2 := eng2.Get([]byte("k2"))
	if err2 != nil || string(val2_k2) != "v2" {
		t.Fatalf("Eng2 Get(k2) unexpected: val=%s, err=%v", string(val2_k2), err2)
	}
	_, err2_k1 := eng2.Get([]byte("k1"))
	if !stdErrors.Is(err2_k1, errors.ErrKeyNotFound) {
		t.Fatalf("Eng2 Get(k1) expected ErrKeyNotFound for tombstone, got: %v", err2_k1)
	}
	seq2 := eng2.NextSeqNum()
	fileNum2 := eng2.NextFileNum()
	_ = eng2.Close()

	// Verify disk hash after Pass 2
	hashAfterPass2 := hashDirectoryContents(t, dir)
	if !bytes.Equal(hashBefore, hashAfterPass2) {
		t.Fatalf("SECURITY VIOLATION: storage state mutated during recovery Pass 2")
	}

	// Assert strict equivalence between Pass 1 and Pass 2
	if seq1 != seq2 {
		t.Fatalf("Idempotency violation: seq watermark drifted: pass1=%d, pass2=%d", seq1, seq2)
	}
	if fileNum1 != fileNum2 {
		t.Fatalf("Idempotency violation: file watermark drifted: pass1=%d, pass2=%d", fileNum1, fileNum2)
	}
}

// TestSEC_P07_002_RecoveryIdempotency_InterruptedRecovery proves SEC-P07-002:
// An interrupted recovery (simulated via pre-publication abort/failure) cleanly leaves
// persistent disk state intact, and a subsequent recovery restarts from scratch and
// achieves the exact same correct recovered state.
func TestSEC_P07_002_RecoveryIdempotency_InterruptedRecovery(t *testing.T) {
	dir := t.TempDir()

	// Write uncommitted WAL records
	writeWALSegment(t, dir, 1,
		makePutRecord(1, "foo", "bar"),
		makePutRecord(2, "baz", "qux"),
	)

	hashBefore := hashDirectoryContents(t, dir)

	// Interrupted recovery: hook triggers Close before publication
	engInterrupted := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})

	resetHook := engine.SetRecoveryPrePublishHookForTesting(func(e *engine.Engine) {
		_ = e.Close() // Simulate interruption/shutdown right before publication
	})
	interruptedErr := engInterrupted.RecoverWAL()
	resetHook()

	if interruptedErr == nil {
		t.Fatalf("expected interrupted recovery to return an error, got nil")
	}

	// Assert disk was NOT modified by interrupted recovery
	hashAfterInterruption := hashDirectoryContents(t, dir)
	if !bytes.Equal(hashBefore, hashAfterInterruption) {
		t.Fatalf("SECURITY VIOLATION: interrupted recovery mutated disk state")
	}

	// Restart recovery on a fresh instance
	engRestarted := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:       dir,
		Backpressure: defaultEngineCfg(),
	})
	defer func() { _ = engRestarted.Close() }()

	if err := engRestarted.RecoverWAL(); err != nil {
		t.Fatalf("restart recovery failed after interruption: %v", err)
	}

	valFoo, err := engRestarted.Get([]byte("foo"))
	if err != nil || string(valFoo) != "bar" {
		t.Fatalf("Get(foo) failed after restart: %v, val: %s", err, string(valFoo))
	}
	valBaz, err := engRestarted.Get([]byte("baz"))
	if err != nil || string(valBaz) != "qux" {
		t.Fatalf("Get(baz) failed after restart: %v, val: %s", err, string(valBaz))
	}
	if engRestarted.NextSeqNum() != 2 {
		t.Fatalf("expected nextSeqNum 2, got: %d", engRestarted.NextSeqNum())
	}
}

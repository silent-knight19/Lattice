package engine_test

import (
	"context"
	stdErrors "errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
	"github.com/silent-knight19/lattice/internal/wal"
)

// referenceOracle is an independent in-memory map oracle that represents the
// ground-truth logical key/value/tombstone state.
type referenceOracle struct {
	data map[string]string // key -> val; missing if deleted
}

func newReferenceOracle() *referenceOracle {
	return &referenceOracle{data: make(map[string]string)}
}

func (o *referenceOracle) applyPut(key, val string) {
	o.data[key] = val
}

func (o *referenceOracle) applyDelete(key string) {
	delete(o.data, key)
}

// TestDifferentialRecoveryAgainstReferenceModel verifies that recovering from
// arbitrary sequences of PUT, DELETE, and BATCH operations with committed and
// uncommitted records reproduces the exact state of the independent oracle.
func TestDifferentialRecoveryAgainstReferenceModel(t *testing.T) {
	r := rand.New(rand.NewSource(1337))

	for iter := 0; iter < 5; iter++ {
		t.Run(fmt.Sprintf("Iteration_%d", iter), func(t *testing.T) {
			dir := t.TempDir()
			oracle := newReferenceOracle()

			walDir := filepath.Join(dir, "wal")
			if err := os.MkdirAll(walDir, 0700); err != nil {
				t.Fatalf("MkdirAll wal failed: %v", err)
			}

			// Open raw WAL file
			walFile := filepath.Join(walDir, "wal_000000000001.log")
			writer, err := wal.OpenWriter(walFile)
			if err != nil {
				t.Fatalf("OpenWriter failed: %v", err)
			}

			var currentSeq uint64
			keys := []string{"apple", "banana", "cherry", "durian", "elderberry", "fig", "grape"}

			// Generate random sequence of operations
			numOps := 30 + r.Intn(20)
		opsLoop:
			for i := 0; i < numOps; i++ {
				opType := r.Intn(3) // 0: Put, 1: Delete, 2: Batch
				k := keys[r.Intn(len(keys))]

				switch opType {
				case 0:
					currentSeq++
					val := fmt.Sprintf("val_%d_%d", i, r.Intn(1000))
					rec := wal.Record{
						Type:      wal.RecordTypePut,
						SeqNum:    binary.SeqNum(currentSeq),
						Timestamp: uint64(time.Now().UnixNano()),
						Key:       []byte(k),
						Value:     []byte(val),
					}
					if err := writer.AppendSync(rec); err != nil {
						t.Fatalf("AppendSync failed: %v", err)
					}
					oracle.applyPut(k, val)

				case 1:
					currentSeq++
					rec := wal.Record{
						Type:      wal.RecordTypeDelete,
						SeqNum:    binary.SeqNum(currentSeq),
						Timestamp: uint64(time.Now().UnixNano()),
						Key:       []byte(k),
					}
					if err := writer.AppendSync(rec); err != nil {
						t.Fatalf("AppendSync failed: %v", err)
					}
					oracle.applyDelete(k)

				case 2:
					// Atomic Batch
					batchSize := 2 + r.Intn(4)
					isCommitted := r.Float32() < 0.75 // 75% commit, 25% simulate crash before commit

					currentSeq++
					startRec := wal.Record{
						Type:      wal.RecordTypeBatchStart,
						SeqNum:    binary.SeqNum(currentSeq),
						Timestamp: uint64(time.Now().UnixNano()),
					}
					if err := writer.AppendSync(startRec); err != nil {
						t.Fatalf("AppendSync start failed: %v", err)
					}

					type pendingOp struct {
						isPut bool
						key   string
						val   string
					}
					var pending []pendingOp

					for b := 0; b < batchSize; b++ {
						currentSeq++
						bk := keys[r.Intn(len(keys))]
						bIsPut := r.Intn(2) == 0
						if bIsPut {
							bVal := fmt.Sprintf("batch_val_%d_%d", i, b)
							rec := wal.Record{
								Type:      wal.RecordTypePut,
								SeqNum:    binary.SeqNum(currentSeq),
								Timestamp: uint64(time.Now().UnixNano()),
								Key:       []byte(bk),
								Value:     []byte(bVal),
							}
							if err := writer.AppendSync(rec); err != nil {
								t.Fatalf("AppendSync batch put failed: %v", err)
							}
							pending = append(pending, pendingOp{isPut: true, key: bk, val: bVal})
						} else {
							rec := wal.Record{
								Type:      wal.RecordTypeDelete,
								SeqNum:    binary.SeqNum(currentSeq),
								Timestamp: uint64(time.Now().UnixNano()),
								Key:       []byte(bk),
							}
							if err := writer.AppendSync(rec); err != nil {
								t.Fatalf("AppendSync batch del failed: %v", err)
							}
							pending = append(pending, pendingOp{isPut: false, key: bk})
						}
					}

					if isCommitted {
						currentSeq++
						commitRec := wal.Record{
							Type:      wal.RecordTypeBatchCommit,
							SeqNum:    binary.SeqNum(currentSeq),
							Timestamp: uint64(time.Now().UnixNano()),
						}
						if err := writer.AppendSync(commitRec); err != nil {
							t.Fatalf("AppendSync commit failed: %v", err)
						}
						for _, p := range pending {
							if p.isPut {
								oracle.applyPut(p.key, p.val)
							} else {
								oracle.applyDelete(p.key)
							}
						}
					} else {
						// Simulated crash before BATCH_COMMIT: log ends abruptly at EOF
						break opsLoop
					}
				}
			}

			_ = writer.Close()

			// Recover with Engine
			eng := newTestEngine(dir)
			if err := eng.RecoverWAL(); err != nil {
				t.Fatalf("RecoverWAL failed: %v", err)
			}
			defer func() { _ = eng.Close() }()

			if !eng.IsRecovered() {
				t.Fatal("engine must be marked recovered")
			}

			// Validate every key in the keyspace against oracle
			for _, k := range keys {
				val, err := eng.Get([]byte(k))
				expectedVal, exists := oracle.data[k]

				if exists {
					if err != nil {
						t.Errorf("key %q: expected val %q, got error %v", k, expectedVal, err)
					} else if string(val) != expectedVal {
						t.Errorf("key %q: expected val %q, got %q", k, expectedVal, string(val))
					}
				} else {
					if err == nil {
						t.Errorf("key %q: expected not found/deleted, got val %q", k, string(val))
					} else if !stdErrors.Is(err, errors.ErrKeyNotFound) {
						t.Errorf("key %q: expected ErrKeyNotFound, got %v", k, err)
					}
				}
			}
		})
	}
}

// TestRecovery_NestedBatchStartFailsClosed verifies that encountering a nested
// BATCH_START before BATCH_COMMIT fails closed with ErrCorruptedBatch.
func TestRecovery_NestedBatchStartFailsClosed(t *testing.T) {
	dir := t.TempDir()
	walDir := filepath.Join(dir, "wal")
	_ = os.MkdirAll(walDir, 0700)

	walFile := filepath.Join(walDir, "wal_000000000001.log")
	w, err := wal.OpenWriter(walFile)
	if err != nil {
		t.Fatalf("OpenWriter failed: %v", err)
	}

	// First BATCH_START
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypeBatchStart, SeqNum: 1, Timestamp: 100})
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 101, Key: []byte("k"), Value: []byte("v")})
	// Nested BATCH_START (corrupted stream)
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypeBatchStart, SeqNum: 3, Timestamp: 102})
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypeBatchCommit, SeqNum: 4, Timestamp: 103})
	_ = w.Close()

	eng := newTestEngine(dir)
	err = eng.RecoverWAL()
	if err == nil {
		t.Fatal("expected failure on nested BATCH_START, got nil")
	}
	if !stdErrors.Is(err, errors.ErrCorruptedBatch) {
		t.Errorf("expected ErrCorruptedBatch, got %v", err)
	}
	if eng.IsRecovered() {
		t.Fatal("engine must not be marked recovered after corrupted batch")
	}
}

// TestRecovery_StagingFileNumberCollisionAvoidance verifies that uncommitted crash-window
// staging artifacts (.tmp_<fileNum>.sst_<random>) are detected so nextFileNum strictly advances.
func TestRecovery_StagingFileNumberCollisionAvoidance(t *testing.T) {
	dir := t.TempDir()

	// Write an orphan staging file with number 42
	stagingName := ".tmp_000042.sst_999999"
	stagingPath := filepath.Join(dir, stagingName)
	if err := os.WriteFile(stagingPath, []byte("staging content"), 0600); err != nil {
		t.Fatalf("WriteFile staging failed: %v", err)
	}

	eng := newTestEngine(dir)
	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("RecoverWAL failed: %v", err)
	}
	defer func() { _ = eng.Close() }()

	allocated := eng.AllocateFileNum()
	if allocated < 43 {
		t.Fatalf("AllocateFileNum returned %d, must be >= 43 to avoid colliding with staging file 42", allocated)
	}
}

// TestRecovery_CleanerFailureAbortsRecoveryTransition verifies that if orphan cleanup
// encounters a critical failure (e.g. directory sync failure), the recovery pipeline aborts
// and does not mark the engine recovered.
func TestRecovery_CleanerFailureAbortsRecoveryTransition(t *testing.T) {
	dir := t.TempDir()

	// Place an orphan staging file so cleaner tries to unlink and sync
	stagingName := ".tmp_000001.sst_123456"
	_ = os.WriteFile(filepath.Join(dir, stagingName), []byte("test"), 0600)

	// Inject cleaner sync directory failure
	syncErr := fmt.Errorf("injected disk sync failure")
	restore := engine.SetCleanerSyncDirFnForTesting(func(f *os.File) error {
		return syncErr
	})
	defer restore()

	eng := newTestEngine(dir)
	err := eng.RecoverWAL()
	if err == nil {
		t.Fatal("expected RecoverWAL to fail when directory sync fails, got nil")
	}

	if eng.IsRecovered() {
		t.Fatal("SECURITY VIOLATION: engine marked recovered despite critical cleaner sync failure")
	}
}

// TestRecovery_ConcurrencyAndLifecycleSafety tests concurrent mutation and close
// interactions during recovery.
func TestRecovery_ConcurrencyAndLifecycleSafety(t *testing.T) {
	t.Run("Mutations during recovery are rejected", func(t *testing.T) {
		dir := t.TempDir()
		eng := newTestEngine(dir)

		// Hook pre-publish to attempt concurrent mutation
		var mutationErr error
		restore := engine.SetRecoveryPrePublishHookForTesting(func(e *engine.Engine) {
			mutationErr = e.Put(context.Background(), []byte("key"), []byte("val"))
		})
		defer restore()

		if err := eng.RecoverWAL(); err != nil {
			t.Fatalf("RecoverWAL failed: %v", err)
		}
		defer func() { _ = eng.Close() }()

		if mutationErr == nil {
			t.Fatal("expected mutation during recovery to be rejected")
		}
		if !stdErrors.Is(mutationErr, errors.ErrRecoveryInProgress) {
			t.Errorf("expected ErrRecoveryInProgress, got %v", mutationErr)
		}
	})

	t.Run("Concurrent Close during recovery aborts cleanly", func(t *testing.T) {
		dir := t.TempDir()
		eng := newTestEngine(dir)

		restore := engine.SetRecoveryPrePublishHookForTesting(func(e *engine.Engine) {
			_ = e.Close()
		})
		defer restore()

		err := eng.RecoverWAL()
		if err == nil {
			t.Fatal("expected RecoverWAL to return ErrWriterClosed when closed concurrently")
		}
		if !stdErrors.Is(err, errors.ErrWriterClosed) {
			t.Errorf("expected ErrWriterClosed, got %v", err)
		}
	})

	t.Run("Multiple concurrent RecoverWAL calls serialize safely", func(t *testing.T) {
		dir := t.TempDir()
		eng := newTestEngine(dir)

		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				errs[idx] = eng.RecoverWAL()
			}(i)
		}
		wg.Wait()

		// Exactly one must succeed, others fail with ErrRecoveryInProgress or ErrRecoveryAlreadyComplete
		successCount := 0
		for _, err := range errs {
			if err == nil {
				successCount++
			}
		}
		if successCount != 1 {
			t.Fatalf("expected exactly 1 recovery to succeed, got %d (errors: %v)", successCount, errs)
		}
		_ = eng.Close()
	})
}

// TestCrossFindingInteractions validates complex combinations of failures across subsystems:
// - Manifest corruption + WAL
// - WAL corruption + orphan files
// - Missing SSTable + WAL
// - math.MaxUint64 file number overflow in staging/physical files
func TestCrossFindingInteractions(t *testing.T) {
	t.Run("Manifest corruption + WAL fails closed without replaying WAL into fresh state", func(t *testing.T) {
		dir := t.TempDir()

		// Write corrupt MANIFEST-000001
		_ = os.WriteFile(filepath.Join(dir, "MANIFEST-000001"), []byte("not a valid version edit framing"), 0644)
		_ = os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("MANIFEST-000001\n"), 0644)

		// Create a valid WAL with records
		walDir := filepath.Join(dir, "wal")
		_ = os.MkdirAll(walDir, 0700)
		w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000001.log"))
		if err != nil {
			t.Fatalf("OpenWriter failed: %v", err)
		}
		_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")})
		_ = w.Close()

		eng := newTestEngine(dir)
		err = eng.RecoverWAL()
		if err == nil {
			t.Fatal("expected recovery to fail closed on corrupt manifest, got nil")
		}

		if eng.IsRecovered() {
			t.Fatal("engine must not be marked recovered")
		}
		if eng.NextSeqNum() != 0 {
			t.Fatalf("sequence watermark was corrupted: %d", eng.NextSeqNum())
		}
	})

	t.Run("Historical WAL corruption + orphan files: fails closed and preserves unrecovered state", func(t *testing.T) {
		dir := t.TempDir()

		// Segment 1 (historical): corrupt CRC
		walDir := filepath.Join(dir, "wal")
		_ = os.MkdirAll(walDir, 0700)
		_ = os.WriteFile(filepath.Join(walDir, "wal_000000000001.log"), []byte("garbage corrupt historical segment"), 0600)
		// Segment 2 (latest)
		w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000002.log"))
		if err != nil {
			t.Fatalf("OpenWriter failed: %v", err)
		}
		_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 10, Timestamp: 100, Key: []byte("k"), Value: []byte("v")})
		_ = w.Close()

		// Orphan staging file
		stagingFile := filepath.Join(dir, ".tmp_000005.sst_123456")
		_ = os.WriteFile(stagingFile, []byte("staging content"), 0600)

		eng := newTestEngine(dir)
		err = eng.RecoverWAL()
		if err == nil {
			t.Fatal("expected recovery to fail closed on historical WAL corruption, got nil")
		}

		if eng.IsRecovered() {
			t.Fatal("engine must not be marked recovered")
		}
	})

	t.Run("Missing active SSTable + WAL fails closed", func(t *testing.T) {
		dir := t.TempDir()

		// Set up manifest with active SSTable 1 that does not exist physically
		_ = os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("MANIFEST-000001\n"), 0644)
		manPath := filepath.Join(dir, "MANIFEST-000001")
		mw, err := version.CreateManifestWriter(manPath)
		if err != nil {
			t.Fatalf("CreateManifestWriter failed: %v", err)
		}
		edit := version.NewVersionEdit()
		edit.SetNextFileNum(2)
		ik, _ := binary.NewInternalKey([]byte("a"), 1, binary.OpTypePut)
		encKey := binary.EncodeInternalKey(ik)
		_ = edit.AddFile(0, version.FileMetadata{
			FileNum:        1,
			FileSize:       1024,
			SmallestKey:    encKey,
			LargestKey:     encKey,
			SmallestSeqNum: 1,
			LargestSeqNum:  1,
		})
		_ = mw.LogEditPtr(edit)
		_ = mw.Close()

		// WAL with record
		walDir := filepath.Join(dir, "wal")
		_ = os.MkdirAll(walDir, 0700)
		w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000001.log"))
		if err != nil {
			t.Fatalf("OpenWriter failed: %v", err)
		}
		_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 200, Key: []byte("k2"), Value: []byte("v2")})
		_ = w.Close()

		eng := newTestEngine(dir)
		err = eng.RecoverWAL()
		if err == nil {
			t.Fatal("expected recovery to fail closed on missing SSTable, got nil")
		}

		if eng.IsRecovered() {
			t.Fatal("engine must not be marked recovered")
		}
	})

	t.Run("File number overflow: math.MaxUint64 staging file fails closed", func(t *testing.T) {
		dir := t.TempDir()

		// Staging file with math.MaxUint64 file number
		maxStagingName := ".tmp_18446744073709551615.sst_overflow"
		_ = os.WriteFile(filepath.Join(dir, maxStagingName), []byte("overflow"), 0600)

		eng := newTestEngine(dir)
		err := eng.RecoverWAL()
		if err == nil {
			t.Fatal("expected recovery to fail closed on math.MaxUint64 file number, got nil")
		}
		if !stdErrors.Is(err, errors.ErrFileNumOverflow) {
			t.Errorf("expected ErrFileNumOverflow, got %v", err)
		}
		if eng.IsRecovered() {
			t.Fatal("engine must not be marked recovered on file number overflow")
		}
	})
}

// -----------------------------------------------------------------------------
// POST-PUBLICATION FAILURE & ATOMIC ROLLBACK TESTS
// -----------------------------------------------------------------------------

func TestRecoveryAtomicity_PostPublishFailureRollbackAllEntryPoints(t *testing.T) {
	entryPoints := []string{"RecoverWAL", "RecoverWALFromCheckpoint", "RecoverWALWithManifestResult"}

	for _, ep := range entryPoints {
		t.Run("EntryPoint_"+ep, func(t *testing.T) {
			dir := t.TempDir()

			// 1. Create a valid MANIFEST with an active SSTable
			_ = os.WriteFile(filepath.Join(dir, "CURRENT"), []byte("MANIFEST-000001\n"), 0644)
			manPath := filepath.Join(dir, "MANIFEST-000001")
			mw, err := version.CreateManifestWriter(manPath)
			if err != nil {
				t.Fatalf("CreateManifestWriter failed: %v", err)
			}
			ik, _ := binary.NewInternalKey([]byte("dur_key"), 1, binary.OpTypePut)
			encKey := binary.EncodeInternalKey(ik)
			sstPath := filepath.Join(dir, "000001.sst")
			_ = os.WriteFile(sstPath, []byte("sstable_content_1024"), 0600)
			sstSize := uint64(len("sstable_content_1024"))

			edit := version.NewVersionEdit()
			edit.SetNextFileNum(2)
			edit.SetLastSeqNum(1)
			_ = edit.AddFile(0, version.FileMetadata{
				FileNum:        1,
				FileSize:       sstSize,
				SmallestKey:    encKey,
				LargestKey:     encKey,
				SmallestSeqNum: 1,
				LargestSeqNum:  1,
			})
			_ = mw.LogEditPtr(edit)
			_ = mw.Close()

			// 2. Create WAL with uncommitted records
			walDir := filepath.Join(dir, "wal")
			_ = os.MkdirAll(walDir, 0700)
			w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000001.log"))
			if err != nil {
				t.Fatalf("OpenWriter failed: %v", err)
			}
			_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 2, Timestamp: 100, Key: []byte("wal_k1"), Value: []byte("wal_v1")})
			_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 3, Timestamp: 200, Key: []byte("wal_k2"), Value: []byte("wal_v2")})
			_ = w.Close()

			// 3. Staging orphan file that should be cleaned on success
			stagingPath := filepath.Join(dir, ".tmp_000005.sst_temp123")
			_ = os.WriteFile(stagingPath, []byte("orphan"), 0600)

			vs := version.NewVersionSet()
			eng := engine.NewEngineWithOptions(engine.EngineOptions{
				DBPath:     dir,
				VersionSet: vs,
			})

			// 4. Inject post-publication failure
			restoreHook := engine.SetRecoveryPostPublishHookForTesting(func(e *engine.Engine) error {
				return stdErrors.New("simulated post-publication failure")
			})

			var recErr error
			switch ep {
			case "RecoverWAL":
				recErr = eng.RecoverWAL()
			case "RecoverWALFromCheckpoint":
				recErr = eng.RecoverWALFromCheckpoint(1)
			case "RecoverWALWithManifestResult":
				disc, discErr := version.DiscoverActiveManifest(dir)
				if discErr != nil {
					t.Fatalf("DiscoverCurrentManifest failed: %v", discErr)
				}
				res, repErr := version.ReplayManifest(disc)
				_ = disc.Close()
				if repErr != nil {
					t.Fatalf("ReplayManifest failed: %v", repErr)
				}
				recErr = eng.RecoverWALWithManifestResult(res)
			}

			if recErr == nil {
				t.Fatalf("[%s] expected recovery to fail on post-publish error, got nil", ep)
			}

			// 5. Verify 100% atomic rollback to pre-recovery state
			if eng.IsRecovered() {
				t.Fatalf("[%s] engine must not be marked recovered after rollback", ep)
			}
			if eng.ActiveMemTable().Len() != 0 {
				t.Fatalf("[%s] activeMem must be rolled back to len 0, got %d", ep, eng.ActiveMemTable().Len())
			}
			if len(eng.ImmMemTables()) != 0 {
				t.Fatalf("[%s] immMems must be rolled back to len 0, got %d", ep, len(eng.ImmMemTables()))
			}
			if eng.NextSeqNum() != 0 {
				t.Fatalf("[%s] nextSeqNum must be rolled back to 0, got %d", ep, eng.NextSeqNum())
			}
			if eng.NextFileNum() != 0 {
				t.Fatalf("[%s] nextFileNum must be rolled back to 0, got %d", ep, eng.NextFileNum())
			}
			if eng.VersionSet() != nil && eng.VersionSet().HasCurrent() {
				t.Fatalf("[%s] VersionSet must not retain current Version after rollback", ep)
			}
			if eng.Backpressure().CurrentBytes() != 0 {
				t.Fatalf("[%s] Backpressure must be rolled back to 0, got %d", ep, eng.Backpressure().CurrentBytes())
			}

			// Point lookup must fail (not returning uncommitted WAL keys)
			if _, getErr := eng.Get([]byte("wal_k1")); !stdErrors.Is(getErr, errors.ErrKeyNotFound) {
				t.Fatalf("[%s] expected ErrKeyNotFound, got %v", ep, getErr)
			}

			// 6. Clear fault injection and verify second recovery attempt SUCCEEDS
			restoreHook()

			var secondErr error
			switch ep {
			case "RecoverWAL":
				secondErr = eng.RecoverWAL()
			case "RecoverWALFromCheckpoint":
				secondErr = eng.RecoverWALFromCheckpoint(1)
			case "RecoverWALWithManifestResult":
				disc, discErr := version.DiscoverActiveManifest(dir)
				if discErr != nil {
					t.Fatalf("DiscoverCurrentManifest failed: %v", discErr)
				}
				res, repErr := version.ReplayManifest(disc)
				_ = disc.Close()
				if repErr != nil {
					t.Fatalf("ReplayManifest failed: %v", repErr)
				}
				secondErr = eng.RecoverWALWithManifestResult(res)
			}

			if secondErr != nil {
				t.Fatalf("[%s] second recovery attempt failed: %v", ep, secondErr)
			}

			if !eng.IsRecovered() {
				t.Fatalf("[%s] engine must be marked recovered after second recovery", ep)
			}
			if eng.NextSeqNum() != 3 {
				t.Fatalf("[%s] expected NextSeqNum == 3, got %d", ep, eng.NextSeqNum())
			}
			val, err := eng.Get([]byte("wal_k1"))
			if err != nil || string(val) != "wal_v1" {
				t.Fatalf("[%s] expected wal_k1 = wal_v1, got val=%q, err=%v", ep, string(val), err)
			}
			val2, err2 := eng.Get([]byte("wal_k2"))
			if err2 != nil || string(val2) != "wal_v2" {
				t.Fatalf("[%s] expected wal_k2 = wal_v2, got val=%q, err=%v", ep, string(val2), err2)
			}

			_ = eng.Close()
		})
	}
}

func TestRecoveryAtomicity_OrphanCleanupFailureRollback(t *testing.T) {
	dir := t.TempDir()

	// WAL with uncommitted record
	walDir := filepath.Join(dir, "wal")
	_ = os.MkdirAll(walDir, 0700)
	w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000001.log"))
	if err != nil {
		t.Fatalf("OpenWriter failed: %v", err)
	}
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")})
	_ = w.Close()

	// Staging orphan file that cleaner will attempt to clean and sync
	stagingPath := filepath.Join(dir, ".tmp_000005.sst_temp123")
	_ = os.WriteFile(stagingPath, []byte("orphan"), 0600)

	vs := version.NewVersionSet()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:     dir,
		VersionSet: vs,
	})

	// Inject cleaner sync directory failure (critical cleaner error)
	restoreSync := engine.SetCleanerSyncDirFnForTesting(func(*os.File) error {
		return stdErrors.New("simulated directory sync failure")
	})

	err = eng.RecoverWAL()
	if err == nil {
		t.Fatal("expected recovery to fail on critical cleaner directory sync error, got nil")
	}

	// Verify complete rollback to pre-recovery state
	if eng.IsRecovered() {
		t.Fatal("engine must not be marked recovered after cleaner failure rollback")
	}
	if eng.ActiveMemTable().Len() != 0 {
		t.Fatalf("activeMem must be rolled back to len 0, got %d", eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != 0 {
		t.Fatalf("nextSeqNum must be rolled back to 0, got %d", eng.NextSeqNum())
	}
	if eng.NextFileNum() != 0 {
		t.Fatalf("nextFileNum must be rolled back to 0, got %d", eng.NextFileNum())
	}
	if eng.VersionSet().HasCurrent() {
		t.Fatal("VersionSet must not retain current Version after rollback")
	}
	if eng.Backpressure().CurrentBytes() != 0 {
		t.Fatalf("Backpressure must be 0, got %d", eng.Backpressure().CurrentBytes())
	}

	// Restore sync function and verify second recovery attempt succeeds cleanly
	restoreSync()

	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("second RecoverWAL failed: %v", err)
	}
	if !eng.IsRecovered() {
		t.Fatal("engine must be marked recovered after second recovery")
	}
	val, err := eng.Get([]byte("k"))
	if err != nil || string(val) != "v" {
		t.Fatalf("expected k = v, got val=%q, err=%v", string(val), err)
	}

	_ = eng.Close()
}

func TestRecoveryAtomicity_PreconditionFailureZeroLiveMutation(t *testing.T) {
	dir := t.TempDir()

	// WAL with uncommitted record
	walDir := filepath.Join(dir, "wal")
	_ = os.MkdirAll(walDir, 0700)
	w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000001.log"))
	if err != nil {
		t.Fatalf("OpenWriter failed: %v", err)
	}
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")})
	_ = w.Close()

	// Staging file with math.MaxUint64 file number causing ErrFileNumOverflow
	maxStagingName := ".tmp_18446744073709551615.sst_overflow"
	stagingPath := filepath.Join(dir, maxStagingName)
	_ = os.WriteFile(stagingPath, []byte("overflow"), 0600)

	vs := version.NewVersionSet()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:     dir,
		VersionSet: vs,
	})

	err = eng.RecoverWAL()
	if !stdErrors.Is(err, errors.ErrFileNumOverflow) {
		t.Fatalf("expected ErrFileNumOverflow, got: %v", err)
	}

	// Verify zero live state modification
	if eng.IsRecovered() {
		t.Fatal("engine must not be marked recovered on overflow")
	}
	if eng.ActiveMemTable().Len() != 0 {
		t.Fatalf("activeMem must be len 0, got %d", eng.ActiveMemTable().Len())
	}
	if eng.NextSeqNum() != 0 {
		t.Fatalf("nextSeqNum must be 0, got %d", eng.NextSeqNum())
	}
	if eng.NextFileNum() != 0 {
		t.Fatalf("nextFileNum must be 0, got %d", eng.NextFileNum())
	}
	if eng.VersionSet().HasCurrent() {
		t.Fatal("VersionSet must not retain current Version on overflow")
	}

	// Remove overflow staging file and verify second recovery succeeds
	_ = os.Remove(stagingPath)
	if err := eng.RecoverWAL(); err != nil {
		t.Fatalf("second RecoverWAL failed: %v", err)
	}
	if !eng.IsRecovered() {
		t.Fatal("engine must be marked recovered after overflow file removed")
	}

	_ = eng.Close()
}

func TestRecoveryAtomicity_ConcurrentCloseRollback(t *testing.T) {
	dir := t.TempDir()

	walDir := filepath.Join(dir, "wal")
	_ = os.MkdirAll(walDir, 0700)
	w, err := wal.OpenWriter(filepath.Join(walDir, "wal_000000000001.log"))
	if err != nil {
		t.Fatalf("OpenWriter failed: %v", err)
	}
	_ = w.AppendSync(wal.Record{Type: wal.RecordTypePut, SeqNum: 1, Timestamp: 100, Key: []byte("k"), Value: []byte("v")})
	_ = w.Close()

	vs := version.NewVersionSet()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath:     dir,
		VersionSet: vs,
	})

	restoreHook := engine.SetRecoveryPostPublishHookForTesting(func(e *engine.Engine) error {
		return e.Close()
	})
	defer restoreHook()

	err = eng.RecoverWAL()
	if !stdErrors.Is(err, errors.ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed, got: %v", err)
	}

	if eng.IsRecovered() {
		t.Fatal("engine must not be marked recovered after concurrent Close")
	}
	if eng.ActiveMemTable().Len() != 0 {
		t.Fatalf("activeMem must be 0, got %d", eng.ActiveMemTable().Len())
	}
	if eng.VersionSet().HasCurrent() {
		t.Fatal("VersionSet must not retain current Version after Close")
	}
}

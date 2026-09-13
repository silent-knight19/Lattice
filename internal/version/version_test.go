package version

import (
	"bytes"
	stdErrors "errors"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

// Helper to create sample FileMetadata
func makeSampleFileMetadata(fileNum uint64, smallest, largest string) FileMetadata {
	return FileMetadata{
		FileNum:        fileNum,
		FileSize:       1024,
		SmallestKey:    []byte(smallest),
		LargestKey:     []byte(largest),
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	}
}

// -----------------------------------------------------------------------------
// CORE REFERENCE COUNTING & INVARIANTS (§19, §33)
// -----------------------------------------------------------------------------

func TestVersion_InitialState(t *testing.T) {
	var levels [NumLevels][]FileMetadata
	v := NewVersion(levels)

	if v.RefCount() != 1 {
		t.Fatalf("expected initial refCount = 1, got %d", v.RefCount())
	}
	if v.ID() != 0 {
		t.Fatalf("expected uninstalled ID = 0, got %d", v.ID())
	}
	for lvl := 0; lvl < NumLevels; lvl++ {
		if v.NumFiles(lvl) != 0 {
			t.Errorf("expected level %d to be empty, got %d files", lvl, v.NumFiles(lvl))
		}
		if v.Files(lvl) != nil {
			t.Errorf("expected level %d Files to return nil, got %v", lvl, v.Files(lvl))
		}
	}
}

func TestVersion_RefUnref(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})

	// Ref increments
	v.Ref()
	if v.RefCount() != 2 {
		t.Fatalf("expected refCount = 2, got %d", v.RefCount())
	}

	v.Ref()
	if v.RefCount() != 3 {
		t.Fatalf("expected refCount = 3, got %d", v.RefCount())
	}

	// Unref decrements
	v.Unref()
	if v.RefCount() != 2 {
		t.Fatalf("expected refCount = 2, got %d", v.RefCount())
	}

	v.Unref()
	if v.RefCount() != 1 {
		t.Fatalf("expected refCount = 1, got %d", v.RefCount())
	}

	// Final Unref reaches 0
	v.Unref()
	if v.RefCount() != 0 {
		t.Fatalf("expected refCount = 0, got %d", v.RefCount())
	}
}

func TestVersion_NoResurrection(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.Unref() // transitions to 0

	if v.RefCount() != 0 {
		t.Fatalf("expected refCount = 0, got %d", v.RefCount())
	}

	// TryRef must return false
	if v.TryRef() {
		t.Fatalf("expected TryRef() on dead Version to return false")
	}

	// Ref must panic (cannot resurrect)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected Ref() on dead Version to panic")
		}
	}()
	v.Ref()
}

func TestVersion_NoUnderflow(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})
	v.Unref() // count is 0

	// Second Unref must panic (double Unref)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected double Unref() to panic")
		}
	}()
	v.Unref()
}

func TestVersion_ExactlyOnceCleanup(t *testing.T) {
	v := NewVersion([NumLevels][]FileMetadata{})

	var cleanupCalls int32
	v.SetCleanupFnForTesting(func() {
		atomic.AddInt32(&cleanupCalls, 1)
	})

	// Add references
	v.Ref()
	v.Ref()
	if atomic.LoadInt32(&cleanupCalls) != 0 {
		t.Fatalf("cleanup called prematurely while refCount > 0")
	}

	v.Unref()
	if atomic.LoadInt32(&cleanupCalls) != 0 {
		t.Fatalf("cleanup called prematurely while refCount > 0")
	}

	v.Unref()
	if atomic.LoadInt32(&cleanupCalls) != 0 {
		t.Fatalf("cleanup called prematurely while refCount > 0")
	}

	// Final Unref (1 -> 0)
	v.Unref()
	if calls := atomic.LoadInt32(&cleanupCalls); calls != 1 {
		t.Fatalf("expected exactly 1 cleanup call, got %d", calls)
	}
}

// -----------------------------------------------------------------------------
// IMMUTABILITY & ALIASING DEFENSE (Invariant E)
// -----------------------------------------------------------------------------

func TestVersion_Immutability_SliceAliasing(t *testing.T) {
	var levels [NumLevels][]FileMetadata
	levels[0] = []FileMetadata{
		makeSampleFileMetadata(1, "a", "b"),
	}

	v := NewVersion(levels)

	// Mutate caller's original slice
	levels[0] = append(levels[0], makeSampleFileMetadata(2, "c", "d"))
	levels[0][0].FileNum = 999

	// Verify Version's internal state is unaffected
	if v.NumFiles(0) != 1 {
		t.Fatalf("expected 1 file in level 0, got %d", v.NumFiles(0))
	}
	files := v.Files(0)
	if files[0].FileNum != 1 {
		t.Fatalf("expected FileNum = 1, got %d (caller slice mutation leaked)", files[0].FileNum)
	}

	// Mutate slice returned by Files(0)
	files[0].FileNum = 888
	files2 := v.Files(0)
	if files2[0].FileNum != 1 {
		t.Fatalf("expected FileNum = 1, got %d (returned slice mutation leaked)", files2[0].FileNum)
	}
}

func TestVersion_Immutability_KeyAliasing(t *testing.T) {
	keyBytes := []byte("original_key")
	var levels [NumLevels][]FileMetadata
	levels[1] = []FileMetadata{
		{
			FileNum:     10,
			SmallestKey: keyBytes,
			LargestKey:  keyBytes,
		},
	}

	v := NewVersion(levels)

	// Mutate caller's key bytes
	keyBytes[0] = 'X'

	files := v.Files(1)
	if !bytes.Equal(files[0].SmallestKey, []byte("original_key")) {
		t.Fatalf("expected SmallestKey to be 'original_key', got %s", files[0].SmallestKey)
	}
	if !bytes.Equal(files[0].LargestKey, []byte("original_key")) {
		t.Fatalf("expected LargestKey to be 'original_key', got %s", files[0].LargestKey)
	}
}

// -----------------------------------------------------------------------------
// VERSIONSET ACTIVE CHAIN & PUBLICATION (§11, §12, §13)
// -----------------------------------------------------------------------------

func TestVersionSet_AppendVersion_Sequential(t *testing.T) {
	vs := NewVersionSet()

	if vs.Current() != nil {
		t.Fatalf("expected initial Current() to be nil")
	}
	if vs.ActiveCount() != 0 {
		t.Fatalf("expected initial ActiveCount = 0, got %d", vs.ActiveCount())
	}

	// Append V1
	v1 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}
	if v1.ID() != 1 {
		t.Fatalf("expected v1 ID = 1, got %d", v1.ID())
	}
	if vs.ActiveCount() != 1 {
		t.Fatalf("expected ActiveCount = 1, got %d", vs.ActiveCount())
	}

	// Current should return V1 pinned
	cur := vs.Current()
	if cur != v1 {
		t.Fatalf("expected Current to be v1")
	}
	if cur.RefCount() != 2 { // 1 owned by vs, 1 pinned by caller
		t.Fatalf("expected v1 refCount = 2, got %d", cur.RefCount())
	}
	cur.Unref()

	// Append V2
	v2 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v2); err != nil {
		t.Fatalf("AppendVersion(v2) failed: %v", err)
	}
	if v2.ID() != 2 {
		t.Fatalf("expected v2 ID = 2, got %d", v2.ID())
	}

	// Because no reader pinned V1, V1 should have been unreferenced by vs.AppendVersion
	// and unlinked from the active chain!
	if v1.RefCount() != 0 {
		t.Fatalf("expected unpinned v1 to reach refCount = 0 after V2 installation, got %d", v1.RefCount())
	}
	if vs.ActiveCount() != 1 {
		t.Fatalf("expected ActiveCount = 1 (only v2 active), got %d", vs.ActiveCount())
	}

	cur2 := vs.Current()
	if cur2 != v2 {
		t.Fatalf("expected Current to be v2")
	}
	cur2.Unref()
}

func TestVersionSet_AppendVersion_Validation(t *testing.T) {
	vs := NewVersionSet()

	// Nil version
	if err := vs.AppendVersion(nil); !stdErrors.Is(err, errors.ErrNilVersion) {
		t.Fatalf("expected ErrNilVersion for nil, got: %v", err)
	}

	// Dead version (refCount = 0)
	deadV := NewVersion([NumLevels][]FileMetadata{})
	deadV.Unref()
	if err := vs.AppendVersion(deadV); !stdErrors.Is(err, errors.ErrDeadVersion) {
		t.Fatalf("expected ErrDeadVersion for dead version, got: %v", err)
	}

	// Already appended version
	v1 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("initial append failed: %v", err)
	}
	if err := vs.AppendVersion(v1); !stdErrors.Is(err, errors.ErrVersionAlreadyAppended) {
		t.Fatalf("expected ErrVersionAlreadyAppended, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// PINNING & COMPACTION RETENTION (Invariant G, §21, §22)
// -----------------------------------------------------------------------------

func TestVersionSet_PinnedVersionSurvivesReplacement(t *testing.T) {
	vs := NewVersionSet()

	v1 := NewVersion([NumLevels][]FileMetadata{})
	var v1Cleaned bool
	v1.SetCleanupFnForTesting(func() {
		v1Cleaned = true
	})

	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}

	// Reader pins V1
	readerPin := vs.Current()
	if readerPin != v1 {
		t.Fatalf("expected readerPin == v1")
	}
	if v1.RefCount() != 2 {
		t.Fatalf("expected v1 refCount = 2, got %d", v1.RefCount())
	}

	// VersionSet installs V2, superseding V1
	v2 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v2); err != nil {
		t.Fatalf("AppendVersion(v2) failed: %v", err)
	}

	// Critical Check: V1 was superseded, but because readerPin holds it:
	// 1. v1.refCount must be 1 (held by reader).
	// 2. v1 must NOT be cleaned up.
	// 3. v1 must remain in vs.ActiveVersions()!
	if v1.RefCount() != 1 {
		t.Fatalf("expected v1 refCount = 1 while pinned by reader, got %d", v1.RefCount())
	}
	if v1Cleaned {
		t.Fatalf("v1 was cleaned up prematurely while pinned by reader!")
	}
	if vs.ActiveCount() != 2 {
		t.Fatalf("expected ActiveCount = 2 (v1 pinned + v2 current), got %d", vs.ActiveCount())
	}

	// Current() points to V2
	newCur := vs.Current()
	if newCur != v2 {
		t.Fatalf("expected Current to point to v2")
	}
	newCur.Unref()

	// Reader finishes reading and unpins V1
	readerPin.Unref()

	// Now V1 must be dead, cleaned up, and unlinked from the active chain
	if v1.RefCount() != 0 {
		t.Fatalf("expected v1 refCount = 0 after reader unpin, got %d", v1.RefCount())
	}
	if !v1Cleaned {
		t.Fatalf("expected v1 to be cleaned up after reader unpin")
	}
	if vs.ActiveCount() != 1 {
		t.Fatalf("expected ActiveCount = 1 after v1 unlinked, got %d", vs.ActiveCount())
	}
}

func TestVersionSet_CompactorSimulation(t *testing.T) {
	vs := NewVersionSet()

	// V1 contains obsolete SSTable file 101
	var levelsV1 [NumLevels][]FileMetadata
	levelsV1[0] = []FileMetadata{makeSampleFileMetadata(101, "key1", "key9")}
	v1 := NewVersion(levelsV1)

	var sstable101Reclaimed bool
	v1.SetCleanupFnForTesting(func() {
		// Simulates obsolete resource cleanup when V1 has no remaining readers
		sstable101Reclaimed = true
	})

	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}

	// Long-running reader pins V1 snapshot
	longRunningReader := vs.Current()
	if longRunningReader == nil {
		t.Fatalf("expected non-nil Current")
	}

	// Compaction finishes: compacts file 101 away, creates file 202 in V2
	var levelsV2 [NumLevels][]FileMetadata
	levelsV2[1] = []FileMetadata{makeSampleFileMetadata(202, "key1", "key9")}
	v2 := NewVersion(levelsV2)

	if err := vs.AppendVersion(v2); err != nil {
		t.Fatalf("AppendVersion(v2) failed: %v", err)
	}

	// Compactor checks if file 101 can be unlinked from disk:
	// Invariant: file 101 CANNOT be reclaimed while V1 is pinned!
	if sstable101Reclaimed {
		t.Fatalf("SSTable 101 resource was reclaimed while long-running reader holds V1!")
	}

	// Reader continues searching in V1
	f0 := longRunningReader.Files(0)
	if len(f0) != 1 || f0[0].FileNum != 101 {
		t.Fatalf("expected reader to see file 101 in V1")
	}

	// Reader finishes query and releases V1
	longRunningReader.Unref()

	// Now that no readers hold V1, SSTable 101 resource can be safely reclaimed
	if !sstable101Reclaimed {
		t.Fatalf("expected SSTable 101 resource to be reclaimed after reader finished")
	}
}

// -----------------------------------------------------------------------------
// CONCURRENCY & STRESS (§15, §27)
// -----------------------------------------------------------------------------

func TestVersionSet_ConcurrentReadersAndInstallers(t *testing.T) {
	vs := NewVersionSet()
	v0 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v0); err != nil {
		t.Fatalf("initial append failed: %v", err)
	}

	const numReaders = 8
	const numInstallers = 4
	const updatesPerInstaller = 50

	var installerWg sync.WaitGroup
	installerWg.Add(numInstallers)

	for i := 0; i < numInstallers; i++ {
		go func(workerID int) {
			defer installerWg.Done()
			for it := 0; it < updatesPerInstaller; it++ {
				var levels [NumLevels][]FileMetadata
				fileNum := uint64(workerID*updatesPerInstaller + it + 10)
				levels[0] = []FileMetadata{makeSampleFileMetadata(fileNum, "k1", "k2")}

				v := NewVersion(levels)
				if err := vs.AppendVersion(v); err != nil {
					t.Errorf("installer %d failed: %v", workerID, err)
					return
				}
			}
		}(i)
	}

	stopCh := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(numReaders)

	for r := 0; r < numReaders; r++ {
		go func(readerID int) {
			defer readerWg.Done()
			for {
				select {
				case <-stopCh:
					return
				default:
					cur := vs.Current()
					if cur != nil {
						if cur.RefCount() < 1 {
							t.Errorf("reader observed refCount < 1: %d", cur.RefCount())
						}
						_ = cur.NumFiles(0)
						cur.Unref()
					}
				}
			}
		}(r)
	}

	installerWg.Wait()
	close(stopCh)
	readerWg.Wait()

	// Final verification: exactly 1 active version remaining
	if vs.ActiveCount() != 1 {
		t.Fatalf("expected exactly 1 active version after all readers finish, got %d", vs.ActiveCount())
	}
	finalCur := vs.Current()
	if finalCur == nil {
		t.Fatalf("expected non-nil final Current")
	}
	if finalCur.ID() != uint64(numInstallers*updatesPerInstaller+1) {
		t.Errorf("expected final version ID = %d, got %d",
			numInstallers*updatesPerInstaller+1, finalCur.ID())
	}
	finalCur.Unref()
}

func TestVersion_DeterministicRandomLifecycle(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	const numVersions = 20
	for i := 0; i < numVersions; i++ {
		v := NewVersion([NumLevels][]FileMetadata{})
		var cleanups int32
		v.SetCleanupFnForTesting(func() {
			atomic.AddInt32(&cleanups, 1)
		})

		// Track modeled reference count
		modeledRefs := 1
		ops := rng.Intn(50) + 10

		for op := 0; op < ops; op++ {
			if modeledRefs > 1 && rng.Float32() < 0.5 {
				v.Unref()
				modeledRefs--
			} else {
				v.Ref()
				modeledRefs++
			}
		}

		// Now drain all remaining references
		for modeledRefs > 0 {
			v.Unref()
			modeledRefs--
		}

		if atomic.LoadInt32(&cleanups) != 1 {
			t.Fatalf("iteration %d: expected exactly 1 cleanup, got %d", i, cleanups)
		}
		if v.RefCount() != 0 {
			t.Fatalf("iteration %d: expected refCount = 0, got %d", i, v.RefCount())
		}
	}
}

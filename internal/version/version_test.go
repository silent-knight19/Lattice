package version

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
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

// -----------------------------------------------------------------------------
// SEC-003 / F-003: ActiveVersions() Lifetime & Pinning Tests
// -----------------------------------------------------------------------------

// TestVersionSet_ActiveVersions_OwnershipContract verifies that ActiveVersions()
// returns caller-pinned *Version pointers that remain valid, alive, and readable
// even after being superseded in the VersionSet, and that calling Unref() exactly once
// on every returned element permits eventual cleanup.
func TestVersionSet_ActiveVersions_OwnershipContract(t *testing.T) {
	vs := NewVersionSet()

	// 1. Empty VersionSet returns nil
	if av := vs.ActiveVersions(); av != nil {
		t.Fatalf("expected nil from empty VersionSet, got %v", av)
	}

	// 2. Install V1, pin it with reader P1
	var v1Cleaned bool
	v1 := NewVersion([NumLevels][]FileMetadata{
		0: {makeSampleFileMetadata(1, "a", "m")},
	})
	v1.SetCleanupFnForTesting(func() {
		v1Cleaned = true
	})
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}
	p1 := vs.Current()

	// 3. Install V2, pin it with reader P2
	var v2Cleaned bool
	v2 := NewVersion([NumLevels][]FileMetadata{
		0: {makeSampleFileMetadata(2, "n", "z")},
	})
	v2.SetCleanupFnForTesting(func() {
		v2Cleaned = true
	})
	if err := vs.AppendVersion(v2); err != nil {
		t.Fatalf("AppendVersion(v2) failed: %v", err)
	}
	p2 := vs.Current()

	// 4. Install V3 (current)
	var v3Cleaned bool
	v3 := NewVersion([NumLevels][]FileMetadata{
		1: {makeSampleFileMetadata(3, "a", "z")},
	})
	v3.SetCleanupFnForTesting(func() {
		v3Cleaned = true
	})
	if err := vs.AppendVersion(v3); err != nil {
		t.Fatalf("AppendVersion(v3) failed: %v", err)
	}

	// Active chain now contains V1 (pinned by p1), V2 (pinned by p2), V3 (current).
	// Invariant A: Call ActiveVersions() - every returned element must be pinned.
	// Initial refCounts before call:
	// V1: 1 (p1)
	// V2: 1 (p2)
	// V3: 1 (vs.current)
	active := vs.ActiveVersions()
	if len(active) != 3 {
		t.Fatalf("expected 3 active versions, got %d", len(active))
	}

	// After ActiveVersions(), refCounts must each have incremented by 1:
	if v1.RefCount() != 2 {
		t.Errorf("expected v1 refCount = 2, got %d", v1.RefCount())
	}
	if v2.RefCount() != 2 {
		t.Errorf("expected v2 refCount = 2, got %d", v2.RefCount())
	}
	if v3.RefCount() != 2 {
		t.Errorf("expected v3 refCount = 2, got %d", v3.RefCount())
	}

	// Invariant B: Caller can safely inspect all metadata on the returned snapshots
	if n := active[0].NumFiles(0); n != 1 {
		t.Errorf("expected active[0] NumFiles(0) == 1, got %d", n)
	}
	if n := active[1].NumFiles(0); n != 1 {
		t.Errorf("expected active[1] NumFiles(0) == 1, got %d", n)
	}
	if n := active[2].NumFiles(1); n != 1 {
		t.Errorf("expected active[2] NumFiles(1) == 1, got %d", n)
	}

	// Invariant C: Superseding current and releasing old reader pins does NOT invalidate active snapshots
	// Drop p1 and p2
	p1.Unref()
	p2.Unref()
	// Supersede V3 with V4
	v4 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v4); err != nil {
		t.Fatalf("AppendVersion(v4) failed: %v", err)
	}

	// At this point:
	// - p1 and p2 released their references to V1 and V2
	// - VersionSet released its reference to V3
	// BUT because `active` retains one reference to V1, V2, and V3:
	// None of them are finalized or cleaned up!
	if v1Cleaned || v2Cleaned || v3Cleaned {
		t.Fatalf("premature cleanup: v1=%v, v2=%v, v3=%v", v1Cleaned, v2Cleaned, v3Cleaned)
	}
	if v1.RefCount() != 1 || v2.RefCount() != 1 || v3.RefCount() != 1 {
		t.Fatalf("expected refCounts = 1, got v1=%d, v2=%d, v3=%d",
			v1.RefCount(), v2.RefCount(), v3.RefCount())
	}

	// Invariant D: Caller releases references exactly once
	for i, v := range active {
		v.Unref()
		if v.RefCount() != 0 {
			t.Errorf("expected active[%d] refCount = 0 after unref, got %d", i, v.RefCount())
		}
	}

	// Invariant E: All 3 versions are now cleaned up
	if !v1Cleaned || !v2Cleaned || !v3Cleaned {
		t.Fatalf("expected all versions cleaned up after active unrefs: v1=%v, v2=%v, v3=%v",
			v1Cleaned, v2Cleaned, v3Cleaned)
	}

	// Invariant F: ActiveCount in vs is now 1 (only V4)
	if vs.ActiveCount() != 1 {
		t.Errorf("expected ActiveCount = 1 (V4), got %d", vs.ActiveCount())
	}
}

// TestVersionSet_ActiveVersions_DeterministicLifetimeRace reproduces the exact F-003 race scenario:
// Caller holds a snapshot from ActiveVersions while another goroutine publishes new versions,
// dropping VersionSet references and ensuring the snapshot remains valid until caller unrefs.
func TestVersionSet_ActiveVersions_DeterministicLifetimeRace(t *testing.T) {
	vs := NewVersionSet()

	var v1Cleaned bool
	v1 := NewVersion([NumLevels][]FileMetadata{
		0: {makeSampleFileMetadata(100, "k1", "k2")},
	})
	v1.SetCleanupFnForTesting(func() {
		v1Cleaned = true
	})
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}

	// Caller obtains pinned snapshot of active versions
	snapshot := vs.ActiveVersions()
	if len(snapshot) != 1 || snapshot[0] != v1 {
		t.Fatalf("unexpected snapshot: %v", snapshot)
	}
	if v1.RefCount() != 2 {
		t.Fatalf("expected v1 refCount = 2 (1 in vs, 1 in snapshot), got %d", v1.RefCount())
	}

	// Concurrently or in interleaved sequence, publisher appends 10 new versions
	const newVersions = 10
	for i := 1; i <= newVersions; i++ {
		vn := NewVersion([NumLevels][]FileMetadata{
			0: {makeSampleFileMetadata(uint64(100+i), "k1", "k2")},
		})
		if err := vs.AppendVersion(vn); err != nil {
			t.Fatalf("AppendVersion failed: %v", err)
		}
	}

	// V1 was superseded in VersionSet. VersionSet dropped its reference.
	// In the vulnerable code, V1's refcount would be 0, v1Cleaned would be true,
	// and snapshot[0].levels would be nilled out!
	// With the fix:
	if v1Cleaned {
		t.Fatalf("F-003 VULNERABILITY DETECTED: v1 was finalized while caller still held snapshot pointer!")
	}
	if v1.RefCount() != 1 {
		t.Fatalf("expected v1 refCount = 1 held by snapshot, got %d", v1.RefCount())
	}

	// Verify metadata remains 100% readable without panics or data races
	files := snapshot[0].Files(0)
	if len(files) != 1 || files[0].FileNum != 100 {
		t.Fatalf("unexpected files in snapshot: %v", files)
	}

	// Now caller finishes with snapshot
	snapshot[0].Unref()

	// V1 must now be finalized
	if !v1Cleaned {
		t.Fatalf("expected v1 to be cleaned up after snapshot unref")
	}
	if v1.RefCount() != 0 {
		t.Fatalf("expected v1 refCount = 0, got %d", v1.RefCount())
	}
}

// TestVersionSet_ActiveVersions_FinalizationProtection verifies that Version.finalize()
// cannot run while an ActiveVersions pin exists, and that attempting to Ref a dead version panics
// while TryRef safely fails.
func TestVersionSet_ActiveVersions_FinalizationProtection(t *testing.T) {
	vs := NewVersionSet()

	var finalizations int32
	v := NewVersion([NumLevels][]FileMetadata{})
	v.SetCleanupFnForTesting(func() {
		atomic.AddInt32(&finalizations, 1)
	})
	if err := vs.AppendVersion(v); err != nil {
		t.Fatalf("AppendVersion failed: %v", err)
	}

	active := vs.ActiveVersions()
	if len(active) != 1 {
		t.Fatalf("expected 1 active version, got %d", len(active))
	}

	// Supersede v in VersionSet
	vNext := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(vNext); err != nil {
		t.Fatalf("AppendVersion(vNext) failed: %v", err)
	}

	// Finalization must NOT have occurred
	if count := atomic.LoadInt32(&finalizations); count != 0 {
		t.Fatalf("expected 0 finalizations while pinned by active, got %d", count)
	}

	// Unref caller's pin
	active[0].Unref()

	// Finalization must have occurred exactly once
	if count := atomic.LoadInt32(&finalizations); count != 1 {
		t.Fatalf("expected exactly 1 finalization, got %d", count)
	}

	// Attempting to Ref a dead version must panic (Invariant D - non-resurrection)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected Ref() on dead version to panic, but it did not")
		}
	}()
	v.Ref()
}

// TestVersionSet_ActiveVersions_ConcurrentStress tests high concurrency with
// simultaneous Appends, ActiveVersions snapshots, Current reads, and Unrefs.
func TestVersionSet_ActiveVersions_ConcurrentStress(t *testing.T) {
	vs := NewVersionSet()

	// Initial version
	v0 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v0); err != nil {
		t.Fatalf("AppendVersion failed: %v", err)
	}

	const numAppenders = 4
	const numActiveReaders = 4
	const numCurrentReaders = 4
	const opsPerWorker = 100

	var wg sync.WaitGroup
	errCh := make(chan error, (numAppenders+numActiveReaders+numCurrentReaders)*opsPerWorker)

	// Appender goroutines
	for a := 0; a < numAppenders; a++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				vn := NewVersion([NumLevels][]FileMetadata{
					0: {makeSampleFileMetadata(uint64(workerID*1000+i+1), "a", "z")},
				})
				if err := vs.AppendVersion(vn); err != nil {
					errCh <- fmt.Errorf("appender %d op %d failed: %w", workerID, i, err)
					return
				}
			}
		}(a)
	}

	// ActiveVersions reader goroutines
	for ar := 0; ar < numActiveReaders; ar++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				active := vs.ActiveVersions()
				if len(active) == 0 {
					errCh <- fmt.Errorf("active reader %d op %d observed empty active list", workerID, i)
					return
				}
				for _, v := range active {
					if v.ID() == 0 {
						errCh <- fmt.Errorf("active reader %d observed version with ID 0", workerID)
						return
					}
					// Verify metadata is readable
					_ = v.NumFiles(0)
					_ = v.Files(0)
					v.Unref()
				}
			}
		}(ar)
	}

	// Current reader goroutines
	for cr := 0; cr < numCurrentReaders; cr++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				cur := vs.Current()
				if cur == nil {
					errCh <- fmt.Errorf("current reader %d observed nil Current", workerID)
					return
				}
				if cur.ID() == 0 {
					errCh <- fmt.Errorf("current reader %d observed version with ID 0", workerID)
					return
				}
				_ = cur.NumFiles(0)
				cur.Unref()
			}
		}(cr)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent stress failure: %v", err)
	}

	// Final verification
	finalCur := vs.Current()
	if finalCur == nil {
		t.Fatalf("final Current() returned nil")
	}
	finalCur.Unref()

	// Active count must be 1 (only current remaining)
	if count := vs.ActiveCount(); count != 1 {
		t.Errorf("expected final ActiveCount = 1, got %d", count)
	}
}

// TestVersionSet_ActiveVersions_RandomizedLifecycle exercises repeated randomized
// cycles of installations, snapshot retentions, and out-of-order unrefs.
func TestVersionSet_ActiveVersions_RandomizedLifecycle(t *testing.T) {
	rng := rand.New(rand.NewSource(1337))
	vs := NewVersionSet()

	v0 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v0); err != nil {
		t.Fatalf("initial append failed: %v", err)
	}

	const iterations = 30
	var retainedSnapshots [][]*Version

	for iter := 0; iter < iterations; iter++ {
		// 1. Maybe install 1-3 new versions
		toInstall := rng.Intn(3) + 1
		for i := 0; i < toInstall; i++ {
			vn := NewVersion([NumLevels][]FileMetadata{})
			if err := vs.AppendVersion(vn); err != nil {
				t.Fatalf("iter %d append failed: %v", iter, err)
			}
		}

		// 2. Snapshot active versions
		snap := vs.ActiveVersions()
		if len(snap) == 0 {
			t.Fatalf("iter %d: expected non-empty snapshot", iter)
		}
		// Verify all elements in snapshot are alive and valid
		for _, v := range snap {
			if v.RefCount() <= 0 {
				t.Fatalf("iter %d: observed non-positive refCount in snapshot: %d", iter, v.RefCount())
			}
			_ = v.NumFiles(0)
		}

		retainedSnapshots = append(retainedSnapshots, snap)

		// 3. Maybe release a random retained snapshot
		if len(retainedSnapshots) > 0 && rng.Float32() < 0.6 {
			idx := rng.Intn(len(retainedSnapshots))
			releaseSnap := retainedSnapshots[idx]
			// Remove from slice
			retainedSnapshots = append(retainedSnapshots[:idx], retainedSnapshots[idx+1:]...)

			for _, v := range releaseSnap {
				v.Unref()
			}
		}
	}

	// Drain all remaining retained snapshots
	for _, snap := range retainedSnapshots {
		for _, v := range snap {
			v.Unref()
		}
	}

	// After all retained snapshots are drained, ActiveCount must be 1 (only current)
	if count := vs.ActiveCount(); count != 1 {
		t.Errorf("expected final ActiveCount = 1, got %d", count)
	}
}

// TestVersionSet_ActiveVersions_DeadNodeSkipped verifies that a node whose refCount
// has dropped to zero (awaiting unlink in finalize()) is skipped by ActiveVersions()
// and not returned or resurrected.
func TestVersionSet_ActiveVersions_DeadNodeSkipped(t *testing.T) {
	vs := NewVersionSet()

	v1 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}

	v2 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v2); err != nil {
		t.Fatalf("AppendVersion(v2) failed: %v", err)
	}

	// V1 was superseded and had no readers, so its refCount dropped to 0 and it was unlinked.
	// ActiveVersions must only return V2.
	active := vs.ActiveVersions()
	if len(active) != 1 {
		t.Fatalf("expected 1 active version (v2), got %d", len(active))
	}
	if active[0] != v2 {
		t.Errorf("expected active[0] == v2, got %v", active[0])
	}
	active[0].Unref()
}

// TestVersion_Files_DeepCopyImmutability verifies that Version.Files returns an independent
// deep copy of FileMetadata structs, ensuring mutating the returned keys does not mutate internal Version state.
func TestVersion_Files_DeepCopyImmutability(t *testing.T) {
	var levels [NumLevels][]FileMetadata
	origSmallest := []byte("original_smallest_key")
	origLargest := []byte("original_largest_key")
	levels[1] = []FileMetadata{
		{
			FileNum:     10,
			FileSize:    500,
			SmallestKey: append([]byte(nil), origSmallest...),
			LargestKey:  append([]byte(nil), origLargest...),
		},
	}

	v := NewVersion(levels)
	files1 := v.Files(1)
	if len(files1) != 1 {
		t.Fatalf("expected 1 file at level 1, got %d", len(files1))
	}

	// Mutate the returned slices
	files1[0].SmallestKey[0] ^= 0xFF
	files1[0].LargestKey[0] ^= 0xFF

	// Fetch again from Version; internal state must remain completely pristine
	files2 := v.Files(1)
	if !bytes.Equal(files2[0].SmallestKey, origSmallest) {
		t.Fatalf("Version.Files leaked mutable slice: SmallestKey corrupted: got %q, want %q",
			files2[0].SmallestKey, origSmallest)
	}
	if !bytes.Equal(files2[0].LargestKey, origLargest) {
		t.Fatalf("Version.Files leaked mutable slice: LargestKey corrupted: got %q, want %q",
			files2[0].LargestKey, origLargest)
	}
}

// TestVersionSet_LogAndApply_PoisonDiagnostic asserts that LogAndApply propagates the structured
// *ManifestWriterPoisonedError with root cause when the underlying writer is poisoned.
func TestVersionSet_LogAndApply_PoisonDiagnostic(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "MANIFEST-000001")
	w, err := CreateManifestWriter(manifestPath)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	defer func() { _ = w.Close() }()

	vs := NewVersionSet()
	vs.manifest = w
	v0 := NewVersion([NumLevels][]FileMetadata{})
	if err := vs.AppendVersion(v0); err != nil {
		t.Fatalf("initial AppendVersion failed: %v", err)
	}

	// Poison the writer via simulated I/O failure
	rootCause := stdErrors.New("simulated low-level disk I/O failure")
	w.SetWriteFnForTesting(func(f *os.File, p []byte) (int, error) {
		return 0, rootCause
	})
	edit := NewVersionEdit()
	edit.SetNextFileNum(100)
	_ = w.LogEditPtr(edit)

	if !w.IsPoisoned() {
		t.Fatalf("expected writer to be poisoned")
	}

	// LogAndApply should return the structured poisoned error containing rootCause
	applyErr := vs.LogAndApply(edit)
	if applyErr == nil {
		t.Fatalf("expected LogAndApply to fail on poisoned writer, got nil")
	}
	if !stdErrors.Is(applyErr, errors.ErrManifestWriterPoisoned) {
		t.Fatalf("expected ErrManifestWriterPoisoned sentinel, got: %v", applyErr)
	}
	var poisonedErr *errors.ManifestWriterPoisonedError
	if !stdErrors.As(applyErr, &poisonedErr) {
		t.Fatalf("expected *errors.ManifestWriterPoisonedError, got: %T (%v)", applyErr, applyErr)
	}
	if !stdErrors.Is(poisonedErr.Reason, rootCause) {
		t.Fatalf("expected root cause %v, got %v", rootCause, poisonedErr.Reason)
	}
}

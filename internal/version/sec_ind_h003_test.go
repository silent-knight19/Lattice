package version_test

import (
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/version"
)

// TestINDH003_CurrentPinSurvivesConcurrentFinalize replays the IND-H-003
// attack interleaving verbatim: goroutine A pins Current(), goroutine B
// supersedes it (dropping the set's reference, which would finalize an
// unpinned version), then A uses the pointer. Because Current() takes a Ref
// under the set lock, A's version must stay alive and readable, with cleanup
// deferred until A's Unref — exactly once.
func TestINDH003_CurrentPinSurvivesConcurrentFinalize(t *testing.T) {
	vs := version.NewVersionSet()

	v1 := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	finalized := 0
	v1.SetCleanupFnForTesting(func() { finalized++ })
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}

	// Goroutine A: pin the current version.
	pinned := vs.Current()
	if pinned == nil {
		t.Fatalf("Current() returned nil on non-empty set")
	}

	// Goroutine B: supersede it twice, as a flush+compaction burst would.
	for i := 0; i < 2; i++ {
		v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
		if err := vs.AppendVersion(v); err != nil {
			pinned.Unref()
			t.Fatalf("AppendVersion failed: %v", err)
		}
	}

	// A uses the pointer after B's finalizes: must be safe, not use-after-free.
	if got := pinned.RefCount(); got != 1 {
		t.Errorf("pinned refCount = %d, want 1 (A's sole surviving reference)", got)
	}
	if finalized != 0 {
		t.Errorf("cleanup ran %d times while A still pinned (want 0)", finalized)
	}
	if n := pinned.NumFiles(0); n != 0 {
		t.Errorf("pinned metadata unreadable: NumFiles(0) = %d, want 0", n)
	}
	_ = pinned.Files(0) // must not panic or race

	pinned.Unref()
	if got := pinned.RefCount(); got != 0 {
		t.Errorf("refCount after Unref = %d, want 0", got)
	}
	if finalized != 1 {
		t.Errorf("cleanup ran %d times after final Unref (want exactly 1)", finalized)
	}
}

// TestINDH003_CurrentOwnershipAccounting verifies the ownership contract each
// caller relies on: one Current() call transfers exactly one reference, nil
// when empty, and every pin is releasable without residue.
func TestINDH003_CurrentOwnershipAccounting(t *testing.T) {
	vs := version.NewVersionSet()
	if cur := vs.Current(); cur != nil {
		t.Fatalf("Current() on empty set = %v, want nil", cur)
	}

	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	if err := vs.AppendVersion(v); err != nil {
		t.Fatalf("AppendVersion failed: %v", err)
	}
	// Set owns the initial reference: refCount == 1.
	if got := v.RefCount(); got != 1 {
		t.Fatalf("refCount after append = %d, want 1", got)
	}

	a := vs.Current()
	b := vs.Current()
	if got := v.RefCount(); got != 3 {
		t.Errorf("refCount after 2 pins = %d, want 3", got)
	}
	a.Unref()
	if got := v.RefCount(); got != 2 {
		t.Errorf("refCount after 1 unpin = %d, want 2", got)
	}
	b.Unref()
	if got := v.RefCount(); got != 1 {
		t.Errorf("refCount after all unpins = %d, want 1 (set's own)", got)
	}
}

// TestINDH003_ConcurrentPinningRace runs the audit's concurrency attacker —
// readers pinning Current() while installers supersede — under the race
// detector. Any lost pin or premature finalize fails loudly via the CAS
// panics or the accounting checks.
func TestINDH003_ConcurrentPinningRace(t *testing.T) {
	vs := version.NewVersionSet()
	v0 := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	if err := vs.AppendVersion(v0); err != nil {
		t.Fatalf("initial append failed: %v", err)
	}

	const readers = 4
	const installers = 2
	const installsEach = 50

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(readers + installers)

	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					cur := vs.Current()
					if cur == nil {
						continue
					}
					if cur.RefCount() < 1 {
						t.Errorf("reader observed dead pin, refCount = %d", cur.RefCount())
					}
					_ = cur.NumFiles(0)
					cur.Unref()
				}
			}
		}()
	}
	for w := 0; w < installers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < installsEach; i++ {
				v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
				if err := vs.AppendVersion(v); err != nil {
					t.Errorf("AppendVersion failed: %v", err)
					return
				}
			}
		}()
	}

	// Wait for installers via a dedicated counter: poll until the current ID
	// advances past the initial version by installers*installsEach, then stop.
	for {
		cur := vs.Current()
		if cur == nil {
			continue
		}
		id := cur.ID()
		cur.Unref()
		if id >= uint64(installers*installsEach) {
			break
		}
	}
	close(stop)
	wg.Wait()
}

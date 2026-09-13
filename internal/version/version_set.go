package version

import (
	"sync"

	"github.com/silent-knight19/lattice/internal/errors"
)

// VersionSet coordinates the lifecycle, active version chain, and publication
// of immutable Version snapshots.
//
// Concurrency & Ownership Contract:
//  1. Active Chain: Maintained as a circular doubly-linked list with a dummy sentinel node.
//     All live versions (where refCount > 0) are members of this chain.
//  2. Atomic Publication: Installing a Version via AppendVersion() atomically replaces the
//     Current() pointer under vs.mu. Readers calling Current() always observe a fully initialized,
//     pinned Version snapshot.
//  3. Decoupled Lifetime: When a Version is superseded as Current, the VersionSet releases its
//     ownership reference. The superseded Version remains alive and readable in the active chain
//     for as long as any reader holds a pinned reference, and is automatically unlinked and
//     reclaimed when the last reader calls Unref().
type VersionSet struct {
	mu      sync.RWMutex
	current *Version
	dummy   Version // sentinel node for circular doubly-linked active version chain
	nextID  uint64
}

// NewVersionSet constructs an initialized VersionSet with an empty active version chain.
func NewVersionSet() *VersionSet {
	vs := &VersionSet{}
	vs.dummy.next = &vs.dummy
	vs.dummy.prev = &vs.dummy
	return vs
}

// AppendVersion installs an immutable Version into the VersionSet and publishes it as Current.
//
// Lifecycle & Reference Transition:
//   - Rejects nil with ErrNilVersion.
//   - Rejects dead versions (refCount <= 0) with ErrDeadVersion.
//   - Rejects versions already attached to a VersionSet with ErrVersionAlreadyAppended.
//   - The VersionSet assumes ownership of the Version's initial reference (refCount = 1).
//   - Inserts the Version at the tail of the circular doubly-linked active version chain.
//   - Atomically updates Current() to point to the newly published Version.
//   - Releases the VersionSet's ownership reference on the superseded old current Version (if any).
func (vs *VersionSet) AppendVersion(v *Version) error {
	if v == nil {
		return errors.ErrNilVersion
	}
	if v.refCount.Load() <= 0 {
		return errors.ErrDeadVersion
	}

	var oldCurrent *Version
	var alreadyAppended bool

	// Critical section: link new Version into active chain and update Current
	func() {
		vs.mu.Lock()
		defer vs.mu.Unlock()

		if v.vset != nil {
			alreadyAppended = true
			return
		}

		v.vset = vs
		vs.nextID++
		v.id = vs.nextID

		// Insert v before dummy (at the tail of the active chain)
		v.prev = vs.dummy.prev
		v.next = &vs.dummy
		vs.dummy.prev.next = v
		vs.dummy.prev = v

		oldCurrent = vs.current
		vs.current = v
	}()

	if alreadyAppended {
		return errors.ErrVersionAlreadyAppended
	}

	// Drop VersionSet's ownership reference on the superseded Version outside the lock
	// to prevent lock-inversion deadlocks with Unref() -> finalize() -> unlinkLocked().
	if oldCurrent != nil {
		oldCurrent.Unref()
	}

	return nil
}

// Current returns the actively published Version, pinned with an incremented reference count.
// The caller MUST call Unref() when finished with the Version snapshot.
// If no Version has been installed yet, Current returns nil.
func (vs *VersionSet) Current() *Version {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	if vs.current == nil {
		return nil
	}
	vs.current.Ref()
	return vs.current
}

// ActiveVersions returns a slice of all live Version pointers currently retained in the active chain.
func (vs *VersionSet) ActiveVersions() []*Version {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var result []*Version
	for cur := vs.dummy.next; cur != &vs.dummy; cur = cur.next {
		result = append(result, cur)
	}
	return result
}

// ActiveCount returns the count of live Version snapshots currently retained in the active chain.
func (vs *VersionSet) ActiveCount() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var count int
	for cur := vs.dummy.next; cur != &vs.dummy; cur = cur.next {
		count++
	}
	return count
}

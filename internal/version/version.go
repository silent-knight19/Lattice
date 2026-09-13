package version

import (
	"bytes"
	"sync/atomic"
)

// Version represents an immutable point-in-time snapshot of the database's SSTable metadata.
//
// Concurrency & Immutability Contract:
//  1. Immutability: Once constructed and published, a Version's level slices and metadata entries are strictly read-only.
//     All level slices and internal byte keys are defensively cloned upon construction.
//  2. Atomic Pinning: Concurrent readers pin a Version by incrementing refCount via Ref() or TryRef().
//     The Version's metadata cannot be reclaimed or invalidated while refCount > 0.
//  3. Non-Resurrection: Once the reference count transitions to zero, calling Ref() will fail fast (panic).
//     A Version whose final ownership has been released cannot be resurrected.
//  4. Exactly-Once Cleanup: The final 1->0 transition triggers resource reclamation and unlinks the Version
//     from its owning VersionSet active chain exactly once.
type Version struct {
	id        uint64
	levels    [NumLevels][]FileMetadata
	refCount  atomic.Int32
	vset      *VersionSet
	next      *Version
	prev      *Version
	cleanupFn func()
}

// NewVersion constructs an immutable Version containing the specified level file metadata.
// It defensively clones all slice headers and underlying key buffers to guarantee that
// caller-owned aliases cannot mutate the Version's metadata after construction.
//
// Initial Ownership Contract:
// The newly created Version is initialized with refCount = 1, representing initial ownership.
// When passed to VersionSet.AppendVersion(v), the VersionSet assumes ownership of this reference.
func NewVersion(levels [NumLevels][]FileMetadata) *Version {
	v := &Version{}
	v.refCount.Store(1)

	for lvl := 0; lvl < NumLevels; lvl++ {
		srcFiles := levels[lvl]
		if len(srcFiles) == 0 {
			continue
		}
		dstFiles := make([]FileMetadata, len(srcFiles))
		for i, meta := range srcFiles {
			dstFiles[i] = FileMetadata{
				FileNum:        meta.FileNum,
				FileSize:       meta.FileSize,
				SmallestKey:    bytes.Clone(meta.SmallestKey),
				LargestKey:     bytes.Clone(meta.LargestKey),
				SmallestSeqNum: meta.SmallestSeqNum,
				LargestSeqNum:  meta.LargestSeqNum,
			}
		}
		v.levels[lvl] = dstFiles
	}

	return v
}

// Ref retains an existing live Version by incrementing its reference count.
// It uses an atomic compare-and-swap loop to guarantee that a Version whose
// reference count has already reached zero cannot be resurrected.
// If the Version is dead (refCount <= 0), Ref panics.
func (v *Version) Ref() {
	if !v.TryRef() {
		panic("cannot Ref dead Version: reference count is zero or negative")
	}
}

// TryRef attempts to retain the Version by atomically incrementing its reference count,
// provided the Version is currently live (refCount > 0).
// Returns true if successfully retained, or false if the Version is dead.
func (v *Version) TryRef() bool {
	for {
		cur := v.refCount.Load()
		if cur <= 0 {
			return false
		}
		if v.refCount.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Unref releases one retained reference on the Version.
// If the reference count drops to zero, the Version is finalized: it unlinks itself
// from the owning VersionSet active chain and executes cleanup hooks exactly once.
// If Unref is called when refCount <= 0 (double Unref / underflow), Unref panics.
func (v *Version) Unref() {
	for {
		cur := v.refCount.Load()
		if cur <= 0 {
			panic("Version refCount underflow: double Unref")
		}
		if v.refCount.CompareAndSwap(cur, cur-1) {
			if cur == 1 {
				v.finalize()
			}
			return
		}
	}
}

// finalize handles reclamation and unlinking when refCount reaches 0.
func (v *Version) finalize() {
	if v.vset != nil {
		v.vset.mu.Lock()
		v.unlinkLocked()
		v.vset.mu.Unlock()
	}
	v.cleanup()
}

// unlinkLocked unlinks the Version from the VersionSet circular doubly-linked active chain.
// Must be called while holding v.vset.mu.
func (v *Version) unlinkLocked() {
	if v.prev != nil && v.next != nil {
		v.prev.next = v.next
		v.next.prev = v.prev
		v.prev = nil
		v.next = nil
	}
}

// cleanup releases internal resources and invokes optional test cleanup hooks.
func (v *Version) cleanup() {
	if v.cleanupFn != nil {
		v.cleanupFn()
	}
	for i := 0; i < NumLevels; i++ {
		v.levels[i] = nil
	}
}

// ID returns the unique sequential version ID assigned by the VersionSet upon installation.
func (v *Version) ID() uint64 {
	return v.id
}

// RefCount returns the current atomic reference count of the Version.
func (v *Version) RefCount() int32 {
	return v.refCount.Load()
}

// NumFiles returns the number of SSTable files at the designated level.
func (v *Version) NumFiles(level int) int {
	if level < 0 || level >= NumLevels {
		return 0
	}
	return len(v.levels[level])
}

// Files returns a defensive copy of the FileMetadata slice at the designated level.
func (v *Version) Files(level int) []FileMetadata {
	if level < 0 || level >= NumLevels {
		return nil
	}
	files := v.levels[level]
	if len(files) == 0 {
		return nil
	}
	cp := make([]FileMetadata, len(files))
	copy(cp, files)
	return cp
}

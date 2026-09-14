package version

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// VersionSet coordinates the lifecycle, active version chain, durability synchronization,
// and atomic publication of immutable Version snapshots.
//
// Concurrency & Ownership Contract:
//  1. Active Chain: Maintained as a circular doubly-linked list with a dummy sentinel node.
//     All live versions (where refCount > 0) are members of this chain.
//  2. Atomic Publication: Installing a Version via AppendVersion() or LogAndApply() atomically
//     replaces the Current() pointer under vs.mu. Readers calling Current() always observe
//     a fully initialized, pinned Version snapshot.
//  3. Decoupled Lifetime: When a Version is superseded as Current, the VersionSet releases its
//     ownership reference. The superseded Version remains alive and readable in the active chain
//     for as long as any reader holds a pinned reference, and is automatically unlinked and
//     reclaimed when the last reader calls Unref().
//  4. Serialized Manifest Commits: vs.applyMu serializes LogAndApply() invocations to prevent
//     stale-base mutations and guarantee identical manifest ordering and in-memory version ordering.
//     Readers calling Current() or ActiveVersions() acquire vs.mu.RLock() and are never blocked
//     across disk I/O operations.
//  5. Safe Obsolete File Reclamation: Files deleted by committed VersionEdits are reclaimed only
//     when no active Version in the active chain retains a reference to them (refCount = 0).
type VersionSet struct {
	mu            sync.RWMutex
	applyMu       sync.Mutex
	current       *Version
	dummy         Version // sentinel node for circular doubly-linked active version chain
	nextID        uint64
	manifest      *ManifestWriter
	dbPath        string
	nextFileNum   uint64
	lastSeqNum    binary.SeqNum
	obsoleteFiles map[uint64]struct{}
	cleaningFiles map[uint64]struct{}
	cleanupErrors []error

	// Pluggable filesystem hooks for deterministic fault injection and testing
	lstatFn   func(name string) (os.FileInfo, error)
	unlinkFn  func(name string) error
	syncDirFn func(f *os.File) error
}

// VersionSetOptions configures optional initialization properties of a VersionSet.
type VersionSetOptions struct {
	DBPath         string
	ManifestWriter *ManifestWriter
	NextFileNum    uint64
	LastSeqNum     binary.SeqNum
}

// NewVersionSet constructs an initialized VersionSet with an empty active version chain.
func NewVersionSet() *VersionSet {
	vs := &VersionSet{
		obsoleteFiles: make(map[uint64]struct{}),
		cleaningFiles: make(map[uint64]struct{}),
		lstatFn:       os.Lstat,
		unlinkFn:      os.Remove,
		syncDirFn:     func(f *os.File) error { return f.Sync() },
	}
	vs.dummy.next = &vs.dummy
	vs.dummy.prev = &vs.dummy
	return vs
}

// NewVersionSetWithOptions constructs an initialized VersionSet configured with VersionSetOptions.
func NewVersionSetWithOptions(opts VersionSetOptions) *VersionSet {
	vs := NewVersionSet()
	if opts.DBPath != "" {
		vs.dbPath = filepath.Clean(opts.DBPath)
	}
	vs.manifest = opts.ManifestWriter
	vs.nextFileNum = opts.NextFileNum
	vs.lastSeqNum = opts.LastSeqNum
	return vs
}

// SetDBPath sets the base database directory used for SSTable physical inspections and unlinks.
func (vs *VersionSet) SetDBPath(path string) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if path == "" {
		vs.dbPath = ""
		return
	}
	vs.dbPath = filepath.Clean(path)
}

// DBPath returns the base database directory path configured on the VersionSet.
func (vs *VersionSet) DBPath() string {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.dbPath
}

// SetManifestWriter sets the active ManifestWriter used for atomic manifest commits.
func (vs *VersionSet) SetManifestWriter(w *ManifestWriter) {
	vs.applyMu.Lock()
	defer vs.applyMu.Unlock()
	vs.manifest = w
}

// ManifestWriter returns the active ManifestWriter configured on the VersionSet.
func (vs *VersionSet) ManifestWriter() *ManifestWriter {
	vs.applyMu.Lock()
	defer vs.applyMu.Unlock()
	return vs.manifest
}

// NextFileNum returns the current next file number allocator watermark.
func (vs *VersionSet) NextFileNum() uint64 {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.nextFileNum
}

// SetNextFileNum updates the next file number allocator watermark if num is greater than the current watermark.
func (vs *VersionSet) SetNextFileNum(num uint64) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if num > vs.nextFileNum {
		vs.nextFileNum = num
	}
}

// LastSeqNum returns the current highest committed sequence watermark.
func (vs *VersionSet) LastSeqNum() binary.SeqNum {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.lastSeqNum
}

// SetLastSeqNum updates the highest committed sequence watermark if seq is greater than the current watermark.
func (vs *VersionSet) SetLastSeqNum(seq binary.SeqNum) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if seq > vs.lastSeqNum {
		vs.lastSeqNum = seq
	}
}

// ObsoleteFileCount returns the number of obsolete SSTable files currently tracked for reclamation.
func (vs *VersionSet) ObsoleteFileCount() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return len(vs.obsoleteFiles)
}

// LastCleanupErrors returns a copy of any errors encountered during obsolete file unlinking.
func (vs *VersionSet) LastCleanupErrors() []error {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	if len(vs.cleanupErrors) == 0 {
		return nil
	}
	cp := make([]error, len(vs.cleanupErrors))
	copy(cp, vs.cleanupErrors)
	return cp
}

// SetTestHooks configures custom filesystem hooks for deterministic testing.
func (vs *VersionSet) SetTestHooks(lstat func(string) (os.FileInfo, error), unlink func(string) error, syncDir func(*os.File) error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if lstat != nil {
		vs.lstatFn = lstat
	}
	if unlink != nil {
		vs.unlinkFn = unlink
	}
	if syncDir != nil {
		vs.syncDirFn = syncDir
	}
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

		// Advance watermarks based on installed files if uninitialized
		for lvl := 0; lvl < NumLevels; lvl++ {
			for _, f := range v.levels[lvl] {
				if f.FileNum >= vs.nextFileNum {
					vs.nextFileNum = f.FileNum + 1
				}
				if f.LargestSeqNum > uint64(vs.lastSeqNum) {
					vs.lastSeqNum = binary.SeqNum(f.LargestSeqNum)
				}
			}
		}

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

// HasCurrent reports whether an active Version snapshot is currently published in the VersionSet.
// Unlike Current(), HasCurrent does NOT increment reference counts or pin the version.
func (vs *VersionSet) HasCurrent() bool {
	vs.mu.RLock()
	defer vs.mu.RUnlock()
	return vs.current != nil
}

// ActiveVersions returns a slice of all live Version snapshots currently retained in the active chain.
//
// Ownership & Concurrency Contract:
// Every returned *Version is pinned with an incremented reference count owned by the caller.
// The caller owns one reference to each returned *Version and MUST call Unref() exactly once
// on every element in the returned slice when finished.
//
// Thread-Safety & Non-Resurrection:
// While holding vs.mu.RLock(), ActiveVersions walks the active chain and attempts atomic pinning
// via TryRef(). Only versions that are live (refCount > 0) are pinned and appended. Any node in
// the active chain whose reference count has already transitioned to zero (awaiting unlinking by
// finalize()) is safely omitted to prevent resurrection of dead versions.
// If the VersionSet is empty or contains no live versions, ActiveVersions returns nil.
func (vs *VersionSet) ActiveVersions() []*Version {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var result []*Version
	for cur := vs.dummy.next; cur != &vs.dummy; cur = cur.next {
		if cur.TryRef() {
			result = append(result, cur)
		}
	}
	return result
}

// ActiveCount returns the count of live Version snapshots currently retained in the active chain.
// Nodes whose reference count has transitioned to zero (awaiting unlinking) are not counted.
func (vs *VersionSet) ActiveCount() int {
	vs.mu.RLock()
	defer vs.mu.RUnlock()

	var count int
	for cur := vs.dummy.next; cur != &vs.dummy; cur = cur.next {
		if cur.refCount.Load() > 0 {
			count++
		}
	}
	return count
}

// validatePhysicalAddedFiles validates that all added SSTables exist on disk as regular files
// with exact matching physical byte sizes.
func (vs *VersionSet) validatePhysicalAddedFiles(adds []AddFileEntry) error {
	cleanDir := vs.dbPath
	if cleanDir == "" {
		return nil
	}
	for _, a := range adds {
		sstPath := TablePath(cleanDir, a.Meta.FileNum)
		info, err := vs.lstatFn(sstPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s", errors.ErrMissingSSTable, sstPath)
			}
			return fmt.Errorf("log and apply: failed to inspect sstable %s: %w", sstPath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: sstable %s is a symlink", errors.ErrSSTableSymlink, sstPath)
		}
		if info.IsDir() {
			return &errors.NotADirectoryError{Path: sstPath, Mode: info.Mode()}
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: sstable %s is not a regular file (mode: %s)", os.ErrInvalid, sstPath, info.Mode())
		}
		if info.Size() < 0 || uint64(info.Size()) != a.Meta.FileSize { // #nosec G115 - size checked non-negative
			return &errors.SSTableSizeMismatchError{
				Path:     sstPath,
				FileNum:  a.Meta.FileNum,
				Expected: a.Meta.FileSize,
				Actual:   info.Size(),
			}
		}
	}
	return nil
}

// collectObsoleteFilesLocked collects candidate obsolete files that are no longer referenced
// by any active version in the circular doubly-linked active version chain.
// Must be called with vs.mu held.
func (vs *VersionSet) collectObsoleteFilesLocked() ([]uint64, string) {
	if len(vs.obsoleteFiles) == 0 {
		return nil, vs.dbPath
	}

	// Identify all FileNums present across all active versions (where refCount > 0)
	activeFileNums := make(map[uint64]struct{})
	for cur := vs.dummy.next; cur != &vs.dummy; cur = cur.next {
		if cur.refCount.Load() > 0 {
			for lvl := 0; lvl < NumLevels; lvl++ {
				for _, f := range cur.levels[lvl] {
					activeFileNums[f.FileNum] = struct{}{}
				}
			}
		}
	}

	var toDelete []uint64
	for fileNum := range vs.obsoleteFiles {
		if _, active := activeFileNums[fileNum]; !active {
			if _, cleaning := vs.cleaningFiles[fileNum]; !cleaning {
				toDelete = append(toDelete, fileNum)
				vs.cleaningFiles[fileNum] = struct{}{}
				delete(vs.obsoleteFiles, fileNum)
			}
		}
	}

	return toDelete, vs.dbPath
}

// deletePhysicalFiles unlinks obsolete SSTable files safely from disk and executes
// directory synchronization. It is invoked outside vs.mu to avoid holding locks across disk I/O.
func (vs *VersionSet) deletePhysicalFiles(dbPath string, fileNums []uint64) ([]uint64, error) {
	if dbPath == "" || len(fileNums) == 0 {
		return nil, nil
	}

	cleanDBPath := filepath.Clean(dbPath)
	var cleaned []uint64
	var errorsList []error

	for _, fileNum := range fileNums {
		name := TableFilename(fileNum)
		sstPath := filepath.Clean(filepath.Join(cleanDBPath, name))

		// Security: directory confinement check
		if filepath.Dir(sstPath) != cleanDBPath || filepath.Base(sstPath) != name {
			err := fmt.Errorf("security violation: path traversal detected for file %d: %s", fileNum, sstPath)
			errorsList = append(errorsList, err)
			vs.recordCleanupResult(fileNum, false, err)
			continue
		}

		// Security: inspect before unlink (reject symlinks and directories)
		info, lstatErr := vs.lstatFn(sstPath)
		if lstatErr != nil {
			if os.IsNotExist(lstatErr) {
				// File is already absent; treat as safely cleaned
				cleaned = append(cleaned, fileNum)
				vs.recordCleanupResult(fileNum, true, nil)
				continue
			}
			errorsList = append(errorsList, lstatErr)
			vs.recordCleanupResult(fileNum, false, lstatErr)
			continue
		}

		if info.Mode()&os.ModeSymlink != 0 {
			err := fmt.Errorf("security violation: refusing to unlink symlink %s", sstPath)
			errorsList = append(errorsList, err)
			vs.recordCleanupResult(fileNum, false, err)
			continue
		}
		if info.IsDir() {
			err := fmt.Errorf("security violation: refusing to unlink directory %s", sstPath)
			errorsList = append(errorsList, err)
			vs.recordCleanupResult(fileNum, false, err)
			continue
		}
		if !info.Mode().IsRegular() {
			err := fmt.Errorf("security violation: refusing to unlink non-regular file %s (mode: %s)", sstPath, info.Mode())
			errorsList = append(errorsList, err)
			vs.recordCleanupResult(fileNum, false, err)
			continue
		}

		// Safely unlink regular file
		if removeErr := vs.unlinkFn(sstPath); removeErr != nil {
			if os.IsNotExist(removeErr) {
				cleaned = append(cleaned, fileNum)
				vs.recordCleanupResult(fileNum, true, nil)
			} else {
				errorsList = append(errorsList, removeErr)
				vs.recordCleanupResult(fileNum, false, removeErr)
			}
		} else {
			cleaned = append(cleaned, fileNum)
			vs.recordCleanupResult(fileNum, true, nil)
		}
	}

	// Synchronize parent directory if files were removed
	if len(cleaned) > 0 {
		if dirFile, openErr := os.Open(cleanDBPath); openErr == nil {
			if syncErr := vs.syncDirFn(dirFile); syncErr != nil {
				errorsList = append(errorsList, fmt.Errorf("directory sync failed after unlinking: %w", syncErr))
			}
			_ = dirFile.Close()
		}
	}

	if len(errorsList) > 0 {
		vs.mu.Lock()
		vs.cleanupErrors = append(vs.cleanupErrors, errorsList...)
		vs.mu.Unlock()
		return cleaned, fmt.Errorf("cleanup encountered %d error(s): %w", len(errorsList), errorsList[0])
	}

	return cleaned, nil
}

func (vs *VersionSet) recordCleanupResult(fileNum uint64, success bool, err error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	delete(vs.cleaningFiles, fileNum)
	if !success && err != nil {
		// Re-add to obsoleteFiles so it can be retried in future passes
		vs.obsoleteFiles[fileNum] = struct{}{}
		vs.cleanupErrors = append(vs.cleanupErrors, err)
	}
}

// CleanObsoleteFiles triggers an explicit scan and reclamation of unpinned obsolete SSTables.
func (vs *VersionSet) CleanObsoleteFiles() ([]uint64, error) {
	vs.mu.Lock()
	toDelete, dbPath := vs.collectObsoleteFilesLocked()
	vs.mu.Unlock()

	if len(toDelete) == 0 || dbPath == "" {
		return nil, nil
	}

	return vs.deletePhysicalFiles(dbPath, toDelete)
}

// LogAndApply commits a VersionEdit to the active MANIFEST log and atomically installs
// the resulting immutable Version snapshot as Current.
//
// Operational Semantics & Durability Boundary:
//  1. Serialization: State transitions are serialized by vs.applyMu. Readers calling Current()
//     or ActiveVersions() are non-blocking and never held across disk I/O.
//  2. Pre-Commit Validation: Validates edit structure, monotonic scalar watermarks, deletion presence
//     in base Version, absence of duplicate/conflicting additions, and physical SSTable existence/size.
//  3. Pre-Derivation: Constructs and freezes the new immutable Version snapshot before writing to MANIFEST.
//  4. Hardware Durability Barrier: Persists and synchronizes (fdatasync/Sync) the CRC32-framed record
//     to the active MANIFEST file. If write or sync fails, the writer is poisoned, the uninstalled
//     Version is safely discarded, and the error is returned without mutating VersionSet state.
//  5. Atomic Publication: Under vs.mu, installs the new Version as Current, links it to the active
//     version chain, advances scalar counters, and records deleted files as obsolete.
//  6. Ownership Transfer: Drops the VersionSet's ownership reference on the superseded Current.
//     If no readers pin the superseded Version, its refCount transitions to 0, triggering immediate
//     safe reclamation of unpinned obsolete SSTable files.
func (vs *VersionSet) LogAndApply(edit *VersionEdit) error {
	if vs == nil {
		return errors.ErrNilReceiver
	}
	if edit == nil {
		return errors.ErrNilVersionEdit
	}

	vs.applyMu.Lock()
	defer vs.applyMu.Unlock()

	// 1. Validate writer is available and healthy
	if vs.manifest == nil {
		return fmt.Errorf("%w: manifest writer is not configured", os.ErrInvalid)
	}
	if vs.manifest.IsClosed() {
		return errors.ErrManifestWriterClosed
	}
	if vs.manifest.IsPoisoned() {
		return errors.ErrManifestWriterPoisoned
	}

	// 2. Validate edit structure
	if err := edit.Validate(); err != nil {
		return err
	}

	// 3. Read current Version and watermarks under vs.mu.RLock
	var base *Version
	var curNextFile uint64
	var curLastSeq binary.SeqNum
	vs.mu.RLock()
	base = vs.current
	if base != nil {
		base.Ref() // Retain temporarily during derivation
	}
	curNextFile = vs.nextFileNum
	curLastSeq = vs.lastSeqNum
	vs.mu.RUnlock()

	if base != nil {
		defer base.Unref()
	}

	// 4. Derive and validate next immutable Version
	newVersion, newNextFile, newLastSeq, err := applyEditToVersion(base, edit, curNextFile, curLastSeq)
	if err != nil {
		return err
	}

	// 5. Physical validation of added SSTables (if dbPath configured)
	if vs.dbPath != "" {
		if err := vs.validatePhysicalAddedFiles(edit.AddedFiles()); err != nil {
			newVersion.Unref()
			return err
		}
	}

	// 6. Persist to MANIFEST with durability barrier (fdatasync)
	if err := vs.manifest.LogEditPtr(edit); err != nil {
		newVersion.Unref()
		return err
	}

	// 7. Atomic publication under vs.mu
	var oldCurrent *Version
	func() {
		vs.mu.Lock()
		defer vs.mu.Unlock()

		if newNextFile > vs.nextFileNum {
			vs.nextFileNum = newNextFile
		}
		if newLastSeq > vs.lastSeqNum {
			vs.lastSeqNum = newLastSeq
		}

		for _, d := range edit.DeletedFiles() {
			vs.obsoleteFiles[d.FileNum] = struct{}{}
		}

		newVersion.vset = vs
		vs.nextID++
		newVersion.id = vs.nextID

		newVersion.prev = vs.dummy.prev
		newVersion.next = &vs.dummy
		vs.dummy.prev.next = newVersion
		vs.dummy.prev = newVersion

		oldCurrent = vs.current
		vs.current = newVersion
	}()

	// 8. Release VersionSet's ownership reference on superseded Current outside vs.mu
	if oldCurrent != nil {
		oldCurrent.Unref()
	}

	return nil
}

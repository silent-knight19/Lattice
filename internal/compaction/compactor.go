package compaction

import (
	"bytes"
	stdErrors "errors"
	"sync"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// TableOpener is an optional resolver function that opens or retrieves a TableReader
// for a designated SSTable file number. When configured on a Compactor, it enables
// exact physical existence verification for candidate files whose user-key ranges overlap.
type TableOpener func(fileNum uint64) (*sstable.TableReader, error)

// CompactorOption defines functional configuration options for Compactor construction.
type CompactorOption func(*Compactor)

// WithTableOpener configures a TableOpener for exact physical SSTable existence checks.
// When omitted, Compactor operates in pure metadata-only conservative mode.
// Note: If opener allocates a new TableReader on each call (unpooled), callers must also
// configure WithCloseTableReaders(true) or use WithTransientTableOpener to prevent file
// descriptor leaks.
func WithTableOpener(opener TableOpener) CompactorOption {
	return func(c *Compactor) {
		c.tableOpener = opener
	}
}

// WithTransientTableOpener configures an unpooled TableOpener that allocates a new TableReader
// per call. It automatically configures the compactor to close the TableReader immediately
// after checking key existence, preventing file descriptor leaks.
func WithTransientTableOpener(opener TableOpener) CompactorOption {
	return func(c *Compactor) {
		c.tableOpener = opener
		c.closeTableReaders = true
	}
}

// WithCloseTableReaders configures whether Compactor should close TableReaders obtained
// from TableOpener immediately after checking key existence. Defaults to false (assuming
// readers are pooled or cached by the opener).
func WithCloseTableReaders(closeReaders bool) CompactorOption {
	return func(c *Compactor) {
		c.closeTableReaders = closeReaders
	}
}

// Compactor orchestrates leveled compaction operations and enforces storage-engine invariants.
// In Phase 08 Sub-Phase 08.2 (P08-S02-M02), Compactor serves as the tombstone purge safety
// evaluation layer, proving whether a tombstone (OpTypeDelete) can be discarded without risking
// ghost-key resurrection from deeper LSM levels.
//
// Invariants & Safety Contract:
//  1. Tombstone Purge Invariant: A tombstone targeting targetLevel may be purged IF AND ONLY IF
//     no relevant older version of that UserKey can survive in any level deeper than targetLevel
//     (i.e. levels targetLevel+1 through NumLevels-1).
//  2. Asymmetric Error Contract:
//     - False Negative (retaining a tombstone unnecessarily): Safe space overhead; acceptable.
//     - False Positive (dropping a tombstone prematurely): Data corruption / ghost-key resurrection; strictly prohibited.
//     If existence is uncertain, corrupt, or unprovable, CanDropTombstone returns false.
//  3. Bottom-Level Invariant: When targetLevel == version.NumLevels - 1 (the deepest level L_max),
//     no deeper levels exist. Hence, no older version can resurrect, and the tombstone is eligible
//     for purging (returns true).
//  4. Model Modes:
//     - Metadata-Only (Conservative): Uses file user-key bounds [SmallestKey, LargestKey]. If any
//     deeper file's range covers userKey, returns false without disk I/O.
//     - Hybrid (Exact): Range pruning identifies candidate files; TableOpener seeks into candidate
//     SSTables to verify physical key existence. Returns false only if key actually exists in a deeper SSTable.
//  5. Version Pinning: Compactor pins its Version snapshot via v.TryRef() upon construction and
//     releases it via v.Unref() upon Close(). A pinned Version cannot be unlinked or reclaimed
//     by VersionSet while Compactor is alive.
//  6. Concurrency & Immutability: CanDropTombstone is safe for concurrent read-only calls across
//     multiple goroutines. It performs zero filesystem mutations and zero VersionSet mutations.
type Compactor struct {
	mu                sync.RWMutex
	v                 *version.Version
	tableOpener       TableOpener
	closeTableReaders bool
	closed            bool
}

// NewCompactor constructs a Compactor pinned to the immutable point-in-time Version snapshot.
// The provided Version must be live (refCount > 0); NewCompactor retains ownership of one
// reference until Close() is called.
func NewCompactor(v *version.Version, opts ...CompactorOption) (*Compactor, error) {
	if v == nil {
		return nil, errors.ErrNilReceiver
	}

	// Atomically pin the Version snapshot to guarantee stability across compaction lifecycle
	if !v.TryRef() {
		return nil, stdErrors.New("cannot construct compactor: version is dead or reference count exhausted")
	}

	c := &Compactor{
		v: v,
	}

	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}

	return c, nil
}

// CanDropTombstone reports whether a tombstone record for userKey can be safely purged
// during a compaction targeting targetLevel without allowing an older version of the key
// in a deeper level to become visible (ghost-key resurrection).
//
// Parameters:
//   - userKey: The raw user key bytes of the tombstone record.
//   - targetLevel: The target level of the compaction (0 <= targetLevel < version.NumLevels).
//
// Returns:
//   - true:  It is mathematically proven that no deeper level (targetLevel+1 ... NumLevels-1)
//     contains any record for userKey. The tombstone may be omitted from compaction output.
//   - false: A deeper level contains (or conservatively may contain) a version of userKey,
//     or input parameters / metadata are invalid. The tombstone must be preserved.
func (c *Compactor) CanDropTombstone(userKey []byte, targetLevel int) bool {
	if c == nil {
		return false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.closed || c.v == nil {
		return false
	}

	// Validate userKey bounds (must be non-empty and <= MaxKeyLen)
	if err := binary.ValidateKey(userKey); err != nil {
		return false
	}

	// Validate targetLevel bounds
	if targetLevel < 0 || targetLevel >= version.NumLevels {
		return false
	}

	// Bottom-level invariant: If targeting the deepest level (NumLevels-1), no deeper levels exist.
	// Therefore, no older revision can possibly exist below targetLevel to resurrect.
	if targetLevel == version.NumLevels-1 {
		return true
	}

	// Inspect all levels strictly deeper than targetLevel
	for lvl := targetLevel + 1; lvl < version.NumLevels; lvl++ {
		files := c.v.Files(lvl)
		if len(files) == 0 {
			continue
		}

		for _, f := range files {
			minUser, maxUser, err := ExtractUserKeyRange(f)
			if err != nil {
				// Corrupt or unparseable metadata: fail closed to preserve data safety
				return false
			}

			// Check if userKey falls within [minUser, maxUser]
			if bytes.Compare(userKey, minUser) >= 0 && bytes.Compare(userKey, maxUser) <= 0 {
				// Candidate file found!
				if c.tableOpener != nil {
					// Exact physical lookup path
					exists, lookupErr := c.checkKeyInTable(f.FileNum, userKey)
					if lookupErr != nil {
						// On I/O or decode error, fail closed
						return false
					}
					if exists {
						// Key actually exists in a deeper SSTable! Tombstone cannot be dropped.
						return false
					}
					// Key does not exist in this candidate table; continue checking other candidates
				} else {
					// Conservative metadata-only path: range overlap implies possible existence
					return false
				}
			}
		}
	}

	// No candidate file in any deeper level contains userKey
	return true
}

// checkKeyInTable performs an exact physical check for userKey in the specified SSTable file.
func (c *Compactor) checkKeyInTable(fileNum uint64, userKey []byte) (bool, error) {
	reader, err := c.tableOpener(fileNum)
	if err != nil {
		return false, err
	}
	if reader == nil {
		return false, errors.ErrNilReceiver
	}

	if c.closeTableReaders {
		defer func() { _ = reader.Close() }()
	}

	it, err := reader.NewIterator()
	if err != nil {
		return false, err
	}
	defer func() { _ = it.Close() }()

	if err := it.Seek(userKey); err != nil {
		return false, err
	}

	// In TableIterator, Seek positions at the first record with UserKey >= userKey.
	if it.Valid() && bytes.Equal(it.Key().UserKey, userKey) {
		return true, nil
	}

	return false, nil
}

// CanDropTombstoneInVersion evaluates tombstone purge safety directly against an immutable Version
// using conservative range analysis without requiring Compactor lifecycle management.
func CanDropTombstoneInVersion(v *version.Version, userKey []byte, targetLevel int) bool {
	if v == nil {
		return false
	}
	if err := binary.ValidateKey(userKey); err != nil {
		return false
	}
	if targetLevel < 0 || targetLevel >= version.NumLevels {
		return false
	}
	if targetLevel == version.NumLevels-1 {
		return true
	}

	for lvl := targetLevel + 1; lvl < version.NumLevels; lvl++ {
		files := v.Files(lvl)
		if len(files) == 0 {
			continue
		}
		for _, f := range files {
			minUser, maxUser, err := ExtractUserKeyRange(f)
			if err != nil {
				return false
			}
			if bytes.Compare(userKey, minUser) >= 0 && bytes.Compare(userKey, maxUser) <= 0 {
				return false
			}
		}
	}
	return true
}

// Version returns the pinned Version snapshot held by the Compactor.
// Returns nil if the Compactor is closed.
func (c *Compactor) Version() *version.Version {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.v
}

// Close releases the Compactor's pinned reference on its Version snapshot.
// Calling Close multiple times is safe and strictly idempotent.
func (c *Compactor) Close() error {
	if c == nil {
		return errors.ErrNilReceiver
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	if c.v != nil {
		c.v.Unref()
		c.v = nil
	}
	return nil
}

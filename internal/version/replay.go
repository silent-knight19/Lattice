package version

import (
	"bytes"
	"cmp"
	stdErrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// TableFilenamePattern defines the standard zero-padded 6-digit SSTable filename format.
	// Example: 000001.sst
	TableFilenamePattern = "%06d.sst"
)

// TableFilename returns the canonical SSTable filename for a given sequential file number.
// In accordance with Lattice architecture, the filename is formatted as "%06d.sst" with 6 zero-padded digits.
func TableFilename(fileNum uint64) string {
	return fmt.Sprintf(TableFilenamePattern, fileNum)
}

// TablePath returns the platform-aware path to an SSTable file within the designated dbPath:
//
//	<db_path>/<000001>.sst
func TablePath(dbPath string, fileNum uint64) string {
	return filepath.Join(filepath.Clean(dbPath), TableFilename(fileNum))
}

// ReplayError provides structured diagnostic context for MANIFEST replay failures.
// It tracks the manifest number, 0-based record index, byte offset within the MANIFEST file,
// and the underlying cause of failure.
type ReplayError struct {
	ManifestNum uint64
	RecordIndex int
	Offset      int64
	Err         error
}

func (e *ReplayError) Error() string {
	if e == nil {
		return "manifest replay error"
	}
	if e.RecordIndex < 0 && e.Offset < 0 {
		return fmt.Sprintf("manifest replay error in MANIFEST-%06d: %v", e.ManifestNum, e.Err)
	}
	return fmt.Sprintf("manifest replay error in MANIFEST-%06d at record %d (offset %d): %v",
		e.ManifestNum, e.RecordIndex, e.Offset, e.Err)
}

func (e *ReplayError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *ReplayError) Is(target error) bool {
	if e == nil {
		return false
	}
	return stdErrors.Is(e.Err, target)
}

// ReplayResult encapsulates the reconstructed version state resulting from a complete MANIFEST replay.
type ReplayResult struct {
	// Version is the reconstructed immutable Version snapshot with refCount = 1.
	// The caller assumes ownership of this reference and may pass it to
	// VersionSet.AppendVersion() or call Unref() when finished.
	Version *Version

	// NextFileNum is the highest observed next file number across all replayed edits.
	NextFileNum uint64

	// LastSeqNum is the highest observed sequence number across all replayed edits.
	LastSeqNum binary.SeqNum

	// ValidRecords is the count of VersionEdit records successfully decoded and applied.
	ValidRecords int

	// FinalOffset is the byte offset reached at the end of the clean replay stream.
	FinalOffset int64
}

// versionBuilder provides an isolated in-memory reconstruction workspace for accumulating
// VersionEdit state deltas before constructing an immutable Version snapshot.
type versionBuilder struct {
	levels      [NumLevels]map[uint64]FileMetadata
	nextFileNum uint64
	lastSeqNum  binary.SeqNum
}

func newVersionBuilder() *versionBuilder {
	b := &versionBuilder{}
	for i := 0; i < NumLevels; i++ {
		b.levels[i] = make(map[uint64]FileMetadata)
	}
	return b
}

// applyEdit applies a validated VersionEdit delta to the in-memory reconstruction state.
func (b *versionBuilder) applyEdit(edit *VersionEdit) error {
	if edit == nil {
		return nil
	}

	// 1. Process file deletions (idempotent no-op if file does not exist)
	for _, d := range edit.DeletedFiles() {
		if d.Level < NumLevels {
			delete(b.levels[d.Level], d.FileNum)
		}
	}

	// 2. Process file additions
	for _, a := range edit.AddedFiles() {
		if a.Level >= NumLevels {
			return &errors.InvalidLevelError{Level: a.Level, MaxLevel: NumLevels - 1}
		}

		// Enforce single-level identity: a file number cannot be active in multiple levels simultaneously
		for lvl := 0; lvl < NumLevels; lvl++ {
			if uint32(lvl) != a.Level {
				if _, exists := b.levels[lvl][a.Meta.FileNum]; exists {
					return fmt.Errorf("%w: file %d already exists at level %d, cannot add to level %d",
						errors.ErrCorruptedVersionEdit, a.Meta.FileNum, lvl, a.Level)
				}
			}
		}

		// Store defensive copy of metadata
		b.levels[a.Level][a.Meta.FileNum] = a.Meta.Clone()
	}

	// 3. Monotonic scalar progression
	if nextNum, ok := edit.NextFileNum(); ok {
		if nextNum > b.nextFileNum {
			b.nextFileNum = nextNum
		}
	}
	if lastSeq, ok := edit.LastSeqNum(); ok {
		if lastSeq > b.lastSeqNum {
			b.lastSeqNum = lastSeq
		}
	}

	return nil
}

// Test seam for physical SSTable filesystem inspection
var replayLstatFn = os.Lstat

// ReplayManifest executes the sequential VersionEdit replay engine according to the P07-S01-M02 specification.
//
// Invariants & Operational Semantics:
//  1. Reuses the already-pinned MANIFEST file descriptor from DiscoveredManifest without reopening by pathname (P07-S01-M02-INV-12).
//  2. Descriptor ownership: ReplayManifest borrows discovered.File and does NOT close it. Ownership remains with caller (Section 26).
//  3. Sequentially streams records from byte offset 0, verifying 8-byte framing header and untrusted payload length bound (INV-01, INV-11).
//  4. Enforces CRC32-IEEE checksum verification over [PayloadLen || Payload] before attempting VersionEdit decode (INV-02).
//  5. Decodes VersionEdit and verifies all structural and semantic invariants via edit.Validate() (INV-03).
//  6. Replays state deltas into an isolated in-memory reconstruction builder (INV-04, INV-10).
//  7. Reconstructs all 7 LSM levels (L0..L6), sorting L0 by FileNum and L1..L6 canonically by SmallestKey with non-overlapping range checks (INV-07).
//  8. Validates physical on-disk existence for every final referenced SSTable file using os.Lstat (INV-05).
//     - Missing SSTable -> ErrMissingSSTable.
//     - Symlink SSTable -> ErrSSTableSymlink.
//     - Directory SSTable -> ErrNotADirectory.
//     - Non-regular SSTable -> fail-closed error.
//  9. Historically deleted SSTables are not required to exist on disk (INV-06).
//  10. Strict fail-closed semantics on any truncation or corruption without mutating MANIFEST, CURRENT, or live VersionSet (INV-03, INV-09, INV-10).
//  11. Deterministic across repeated replays on unchanged disk state (INV-08).
func ReplayManifest(discovered *DiscoveredManifest) (*ReplayResult, error) {
	if discovered == nil || discovered.File == nil {
		return nil, fmt.Errorf("%w: discovered manifest descriptor is nil", os.ErrInvalid)
	}
	if discovered.Dir == "" {
		return nil, fmt.Errorf("%w: discovered manifest directory cannot be empty", os.ErrInvalid)
	}

	builder := newVersionBuilder()
	var offset int64
	recordIndex := 0

	// Stream and replay all records sequentially from offset 0
	for {
		payload, recordLen, err := decodeOneManifestRecord(discovered.File, offset)
		if err != nil {
			if stdErrors.Is(err, io.EOF) {
				// Clean EOF between complete records
				break
			}
			// Fail-closed on truncated header, truncated payload, CRC mismatch, or framing violation
			return nil, &ReplayError{
				ManifestNum: discovered.ManifestNum,
				RecordIndex: recordIndex,
				Offset:      offset,
				Err:         err,
			}
		}

		// Decode VersionEdit
		edit, decErr := DecodeVersionEdit(payload)
		if decErr != nil {
			return nil, &ReplayError{
				ManifestNum: discovered.ManifestNum,
				RecordIndex: recordIndex,
				Offset:      offset,
				Err:         decErr,
			}
		}

		// Validate VersionEdit structure and invariants
		if valErr := edit.Validate(); valErr != nil {
			return nil, &ReplayError{
				ManifestNum: discovered.ManifestNum,
				RecordIndex: recordIndex,
				Offset:      offset,
				Err:         valErr,
			}
		}

		// Apply edit to reconstruction state
		if applyErr := builder.applyEdit(edit); applyErr != nil {
			return nil, &ReplayError{
				ManifestNum: discovered.ManifestNum,
				RecordIndex: recordIndex,
				Offset:      offset,
				Err:         applyErr,
			}
		}

		offset += recordLen
		recordIndex++
	}

	// Reconstruct and validate level slices
	var finalLevels [NumLevels][]FileMetadata

	// Level 0: Sort deterministically by FileNum ASC
	l0Files := make([]FileMetadata, 0, len(builder.levels[0]))
	for _, meta := range builder.levels[0] {
		l0Files = append(l0Files, meta)
	}
	slices.SortFunc(l0Files, func(a, b FileMetadata) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})
	finalLevels[0] = l0Files

	// Levels 1..6: Sort canonically by SmallestKey and assert non-overlapping key ranges
	for lvl := 1; lvl < NumLevels; lvl++ {
		files := make([]FileMetadata, 0, len(builder.levels[lvl]))
		for _, meta := range builder.levels[lvl] {
			files = append(files, meta)
		}

		slices.SortFunc(files, func(a, b FileMetadata) int {
			ikA := decodeInternalKeyNoAlloc(a.SmallestKey)
			ikB := decodeInternalKeyNoAlloc(b.SmallestKey)
			return binary.CompareInternalKey(ikA, ikB)
		})

		for i := 0; i < len(files)-1; i++ {
			prevLargest := decodeInternalKeyNoAlloc(files[i].LargestKey)
			currSmallest := decodeInternalKeyNoAlloc(files[i+1].SmallestKey)
			if bytes.Compare(prevLargest.UserKey, currSmallest.UserKey) >= 0 || binary.CompareInternalKey(prevLargest, currSmallest) >= 0 {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: recordIndex,
					Offset:      offset,
					Err: fmt.Errorf("%w: overlapping key ranges at level %d between file %d and file %d",
						errors.ErrInvalidKeyRange, lvl, files[i].FileNum, files[i+1].FileNum),
				}
			}
		}

		finalLevels[lvl] = files
	}

	// Physical SSTable existence and file-type validation on disk
	cleanDir := filepath.Clean(discovered.Dir)
	for lvl := 0; lvl < NumLevels; lvl++ {
		for _, meta := range finalLevels[lvl] {
			sstPath := TablePath(cleanDir, meta.FileNum)
			info, err := replayLstatFn(sstPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, &ReplayError{
						ManifestNum: discovered.ManifestNum,
						RecordIndex: recordIndex,
						Offset:      offset,
						Err:         fmt.Errorf("%w: %s", errors.ErrMissingSSTable, sstPath),
					}
				}
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: recordIndex,
					Offset:      offset,
					Err:         fmt.Errorf("replay: failed to inspect sstable %s: %w", sstPath, err),
				}
			}

			if info.Mode()&os.ModeSymlink != 0 {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: recordIndex,
					Offset:      offset,
					Err:         fmt.Errorf("%w: sstable %s is a symlink", errors.ErrSSTableSymlink, sstPath),
				}
			}

			if info.IsDir() {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: recordIndex,
					Offset:      offset,
					Err:         &errors.NotADirectoryError{Path: sstPath, Mode: info.Mode()},
				}
			}

			if !info.Mode().IsRegular() {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: recordIndex,
					Offset:      offset,
					Err:         fmt.Errorf("%w: sstable %s is not a regular file (mode: %s)", os.ErrInvalid, sstPath, info.Mode()),
				}
			}
		}
	}

	// Construct final immutable Version snapshot (initial refCount = 1)
	finalVersion := NewVersion(finalLevels)

	return &ReplayResult{
		Version:      finalVersion,
		NextFileNum:  builder.nextFileNum,
		LastSeqNum:   builder.lastSeqNum,
		ValidRecords: recordIndex,
		FinalOffset:  offset,
	}, nil
}

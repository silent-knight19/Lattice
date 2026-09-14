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
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

const (
	// TableFilenamePattern defines the standard zero-padded 6-digit SSTable filename format.
	// Example: 000001.sst
	TableFilenamePattern = "%06d.sst"

	// MaxManifestReplayBytes defines the global maximum physical byte stream budget
	// permitted during MANIFEST replay (64 MiB). This bounds the physical data read
	// and processed before failing closed against replay resource exhaustion.
	MaxManifestReplayBytes int64 = 64 * 1024 * 1024

	// MaxManifestReplayRecords defines the maximum count of VersionEdit records
	// permitted during a single sequential MANIFEST replay pass (100,000 edits).
	MaxManifestReplayRecords = 100000

	// MaxManifestLiveFiles defines the maximum count of live SSTable files permitted
	// across all levels in the reconstructed Version (100,000 active files).
	MaxManifestLiveFiles = 100000
)

// Pluggable replay limits for testing boundary conditions
var (
	manifestReplayMaxBytes   = MaxManifestReplayBytes
	manifestReplayMaxRecords = MaxManifestReplayRecords
	manifestReplayMaxFiles   = MaxManifestLiveFiles
)

// SetManifestReplayLimitsForTesting configures custom limits for testing and returns a restore function.
func SetManifestReplayLimitsForTesting(maxBytes int64, maxRecords int, maxFiles int) func() {
	prevBytes := manifestReplayMaxBytes
	prevRecords := manifestReplayMaxRecords
	prevFiles := manifestReplayMaxFiles
	manifestReplayMaxBytes = maxBytes
	manifestReplayMaxRecords = maxRecords
	manifestReplayMaxFiles = maxFiles
	return func() {
		manifestReplayMaxBytes = prevBytes
		manifestReplayMaxRecords = prevRecords
		manifestReplayMaxFiles = prevFiles
	}
}

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

// ParseTableFilename parses a canonical SSTable filename (e.g. "000001.sst") and returns
// the 64-bit file number. Returns (0, false) if name does not match the canonical format.
func ParseTableFilename(name string) (uint64, bool) {
	if !strings.HasSuffix(name, ".sst") {
		return 0, false
	}
	base := strings.TrimSuffix(name, ".sst")
	if len(base) < 6 {
		return 0, false
	}
	for i := 0; i < len(base); i++ {
		if base[i] < '0' || base[i] > '9' {
			return 0, false
		}
	}
	num, err := strconv.ParseUint(base, 10, 64)
	if err != nil || num == 0 {
		return 0, false
	}
	if name != TableFilename(num) {
		return 0, false
	}
	return num, true
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

// fileProvenance preserves the origin VersionEdit metadata, record index, and byte stream offset
// for an added SSTable to enable precise post-scan diagnostic reporting (P07-SEC-012).
type fileProvenance struct {
	Meta        FileMetadata
	RecordIndex int
	Offset      int64
}

// versionBuilder provides an isolated in-memory reconstruction workspace for accumulating
// VersionEdit state deltas before constructing an immutable Version snapshot.
type versionBuilder struct {
	levels      [NumLevels]map[uint64]fileProvenance
	nextFileNum uint64
	lastSeqNum  binary.SeqNum
}

func newVersionBuilder() *versionBuilder {
	b := &versionBuilder{}
	for i := 0; i < NumLevels; i++ {
		b.levels[i] = make(map[uint64]fileProvenance)
	}
	return b
}

func (b *versionBuilder) liveFileCount() int {
	total := 0
	for lvl := 0; lvl < NumLevels; lvl++ {
		total += len(b.levels[lvl])
	}
	return total
}

// applyEdit applies a validated VersionEdit delta to the in-memory reconstruction state.
func (b *versionBuilder) applyEdit(edit *VersionEdit, recordIndex int, offset int64) error {
	if edit == nil {
		return nil
	}

	// 1. Process file deletions (idempotent no-op if file does not exist)
	for _, d := range edit.DeletedFiles() {
		if d.Level < NumLevels {
			delete(b.levels[d.Level], d.FileNum)
		}
	}

	// 2. Validate live file budget before allocating additions
	currentFiles := b.liveFileCount()
	newAdditions := 0
	for _, a := range edit.AddedFiles() {
		if a.Level < NumLevels {
			if _, exists := b.levels[a.Level][a.Meta.FileNum]; !exists {
				newAdditions++
			}
		}
	}
	if currentFiles+newAdditions > manifestReplayMaxFiles {
		var curFiles, limFiles uint64
		if currentFiles > 0 {
			curFiles += uint64(currentFiles)
		}
		if newAdditions > 0 {
			curFiles += uint64(newAdditions)
		}
		if manifestReplayMaxFiles > 0 {
			limFiles = uint64(manifestReplayMaxFiles)
		}
		return &errors.ManifestReplayLimitError{
			Resource: "live_files",
			Current:  curFiles,
			Limit:    limFiles,
		}
	}

	// 3. Process file additions
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

		// Store defensive copy of metadata with origin provenance (P07-SEC-012)
		b.levels[a.Level][a.Meta.FileNum] = fileProvenance{
			Meta:        a.Meta.Clone(),
			RecordIndex: recordIndex,
			Offset:      offset,
		}
	}

	// 4. Monotonic scalar progression (P07-SEC-011)
	if nextNum, ok := edit.NextFileNum(); ok {
		if nextNum < b.nextFileNum {
			return fmt.Errorf("%w: next file num regressed from %d to %d",
				errors.ErrCorruptedVersionEdit, b.nextFileNum, nextNum)
		}
		b.nextFileNum = nextNum
	}
	if lastSeq, ok := edit.LastSeqNum(); ok {
		if lastSeq < b.lastSeqNum {
			return fmt.Errorf("%w: last seq num regressed from %d to %d",
				errors.ErrCorruptedVersionEdit, b.lastSeqNum, lastSeq)
		}
		b.lastSeqNum = lastSeq
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
		if recordIndex >= manifestReplayMaxRecords {
			// Check if at clean EOF
			var peek [ManifestHeaderSize]byte
			n, peekErr := readFullAt(discovered.File, peek[:], offset)
			if peekErr != nil && stdErrors.Is(peekErr, io.EOF) && n == 0 {
				break
			}
			var limRec uint64
			if manifestReplayMaxRecords > 0 {
				limRec = uint64(manifestReplayMaxRecords)
			}
			return nil, &ReplayError{
				ManifestNum: discovered.ManifestNum,
				RecordIndex: recordIndex,
				Offset:      offset,
				Err: &errors.ManifestReplayLimitError{
					Resource: "records",
					Current:  uint64(recordIndex) + 1,
					Limit:    limRec,
					Offset:   offset,
					Record:   recordIndex,
				},
			}
		}

		maxRemaining := manifestReplayMaxBytes - offset
		if maxRemaining < int64(ManifestHeaderSize) {
			var peek [1]byte
			n, _ := readFullAt(discovered.File, peek[:], offset)
			if n == 0 {
				break
			}
			var curBytes, limBytes uint64
			if offset > 0 {
				curBytes = uint64(offset)
			}
			if n > 0 {
				curBytes += uint64(n)
			}
			if manifestReplayMaxBytes > 0 {
				limBytes = uint64(manifestReplayMaxBytes)
			}
			return nil, &ReplayError{
				ManifestNum: discovered.ManifestNum,
				RecordIndex: recordIndex,
				Offset:      offset,
				Err: &errors.ManifestReplayLimitError{
					Resource: "bytes",
					Current:  curBytes,
					Limit:    limBytes,
					Offset:   offset,
					Record:   recordIndex,
				},
			}
		}

		payload, recordLen, err := decodeOneManifestRecordBounded(discovered.File, offset, maxRemaining)
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

		// Apply edit to reconstruction state with record index and offset provenance
		if applyErr := builder.applyEdit(edit, recordIndex, offset); applyErr != nil {
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
	var finalProvs [NumLevels][]fileProvenance

	// Level 0: Sort deterministically by FileNum ASC
	l0Provs := make([]fileProvenance, 0, len(builder.levels[0]))
	for _, prov := range builder.levels[0] {
		l0Provs = append(l0Provs, prov)
	}
	slices.SortFunc(l0Provs, func(a, b fileProvenance) int {
		return cmp.Compare(a.Meta.FileNum, b.Meta.FileNum)
	})
	l0Files := make([]FileMetadata, len(l0Provs))
	for i, p := range l0Provs {
		l0Files[i] = p.Meta
	}
	finalLevels[0] = l0Files
	finalProvs[0] = l0Provs

	// Levels 1..6: Sort canonically by SmallestKey and assert non-overlapping key ranges
	for lvl := 1; lvl < NumLevels; lvl++ {
		provs := make([]fileProvenance, 0, len(builder.levels[lvl]))
		for _, p := range builder.levels[lvl] {
			provs = append(provs, p)
		}

		slices.SortFunc(provs, func(a, b fileProvenance) int {
			ikA := decodeInternalKeyNoAlloc(a.Meta.SmallestKey)
			ikB := decodeInternalKeyNoAlloc(b.Meta.SmallestKey)
			return binary.CompareInternalKey(ikA, ikB)
		})

		for i := 0; i < len(provs)-1; i++ {
			prevLargest := decodeInternalKeyNoAlloc(provs[i].Meta.LargestKey)
			currSmallest := decodeInternalKeyNoAlloc(provs[i+1].Meta.SmallestKey)
			if bytes.Compare(prevLargest.UserKey, currSmallest.UserKey) >= 0 || binary.CompareInternalKey(prevLargest, currSmallest) >= 0 {
				offending := provs[i+1]
				if provs[i].RecordIndex > provs[i+1].RecordIndex {
					offending = provs[i]
				}
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: offending.RecordIndex,
					Offset:      offending.Offset,
					Err: fmt.Errorf("%w: overlapping key ranges at level %d between file %d and file %d",
						errors.ErrInvalidKeyRange, lvl, provs[i].Meta.FileNum, provs[i+1].Meta.FileNum),
				}
			}
		}

		files := make([]FileMetadata, len(provs))
		for i, p := range provs {
			files[i] = p.Meta
		}
		finalLevels[lvl] = files
		finalProvs[lvl] = provs
	}

	// Physical SSTable existence, file-type, and size validation on disk (P07-SEC-008, P07-SEC-012)
	cleanDir := filepath.Clean(discovered.Dir)
	for lvl := 0; lvl < NumLevels; lvl++ {
		for _, prov := range finalProvs[lvl] {
			meta := prov.Meta
			sstPath := TablePath(cleanDir, meta.FileNum)
			info, err := replayLstatFn(sstPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, &ReplayError{
						ManifestNum: discovered.ManifestNum,
						RecordIndex: prov.RecordIndex,
						Offset:      prov.Offset,
						Err:         fmt.Errorf("%w: %s", errors.ErrMissingSSTable, sstPath),
					}
				}
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: prov.RecordIndex,
					Offset:      prov.Offset,
					Err:         fmt.Errorf("replay: failed to inspect sstable %s: %w", sstPath, err),
				}
			}

			if info.Mode()&os.ModeSymlink != 0 {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: prov.RecordIndex,
					Offset:      prov.Offset,
					Err:         fmt.Errorf("%w: sstable %s is a symlink", errors.ErrSSTableSymlink, sstPath),
				}
			}

			if info.IsDir() {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: prov.RecordIndex,
					Offset:      prov.Offset,
					Err:         &errors.NotADirectoryError{Path: sstPath, Mode: info.Mode()},
				}
			}

			if !info.Mode().IsRegular() {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: prov.RecordIndex,
					Offset:      prov.Offset,
					Err:         fmt.Errorf("%w: sstable %s is not a regular file (mode: %s)", os.ErrInvalid, sstPath, info.Mode()),
				}
			}

			// SSTable physical size validation against authoritative manifest metadata (P07-SEC-008)
			if uint64(info.Size()) != meta.FileSize {
				return nil, &ReplayError{
					ManifestNum: discovered.ManifestNum,
					RecordIndex: prov.RecordIndex,
					Offset:      prov.Offset,
					Err: &errors.SSTableSizeMismatchError{
						Path:     sstPath,
						FileNum:  meta.FileNum,
						Expected: meta.FileSize,
						Actual:   info.Size(),
					},
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

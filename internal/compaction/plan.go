package compaction

import (
	"bytes"
	"fmt"
	"math"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// CompactionPlan represents an immutable, deterministic blueprint for a leveled compaction work unit.
//
// Invariants & Operational Semantics:
//  1. Snapshot Immutability: A CompactionPlan is derived against one immutable Version snapshot.
//     All internal file slices and byte buffers are defensively cloned.
//  2. Level Transitions: Compactions strictly progress from sourceLevel to targetLevel = sourceLevel + 1.
//     Direct multi-level hops are prohibited.
//  3. Non-Empty Source: sourceFiles must contain at least one SSTable from sourceLevel.
//     targetFiles may be empty if no files in targetLevel intersect the source key range.
//  4. Disjoint Identities: A FileNum cannot appear more than once within the plan, nor can it
//     exist simultaneously in sourceFiles and targetFiles.
//  5. Range Coverage: SmallestKey and LargestKey strictly encompass all keys present across all input files.
//  6. Arithmetic Safety: Input byte sizes are strictly checked against 64-bit integer overflow.
type CompactionPlan struct {
	sourceLevel      int
	targetLevel      int
	sourceFiles      []version.FileMetadata
	targetFiles      []version.FileMetadata
	smallestKey      binary.InternalKey
	largestKey       binary.InternalKey
	smallestUserKey  []byte
	largestUserKey   []byte
	sourceInputBytes uint64
	targetInputBytes uint64
	totalInputBytes  uint64
	score            float64
}

// NewCompactionPlan constructs and validates an immutable CompactionPlan.
func NewCompactionPlan(
	sourceLevel int,
	targetLevel int,
	sourceFiles []version.FileMetadata,
	targetFiles []version.FileMetadata,
	smallestKey binary.InternalKey,
	largestKey binary.InternalKey,
	smallestUserKey []byte,
	largestUserKey []byte,
	sourceInputBytes uint64,
	targetInputBytes uint64,
	score float64,
) (*CompactionPlan, error) {
	if math.MaxUint64-sourceInputBytes < targetInputBytes {
		return nil, errors.ErrByteCountOverflow
	}

	srcClones := make([]version.FileMetadata, len(sourceFiles))
	for i, f := range sourceFiles {
		srcClones[i] = f.Clone()
	}

	tgtClones := make([]version.FileMetadata, len(targetFiles))
	for i, f := range targetFiles {
		tgtClones[i] = f.Clone()
	}

	plan := &CompactionPlan{
		sourceLevel:      sourceLevel,
		targetLevel:      targetLevel,
		sourceFiles:      srcClones,
		targetFiles:      tgtClones,
		smallestKey:      smallestKey.Clone(),
		largestKey:       largestKey.Clone(),
		smallestUserKey:  bytes.Clone(smallestUserKey),
		largestUserKey:   bytes.Clone(largestUserKey),
		sourceInputBytes: sourceInputBytes,
		targetInputBytes: targetInputBytes,
		totalInputBytes:  sourceInputBytes + targetInputBytes,
		score:            score,
	}

	if err := plan.Validate(); err != nil {
		return nil, err
	}

	return plan, nil
}

// SourceLevel returns the 0-based source level of the compaction.
func (p *CompactionPlan) SourceLevel() int {
	return p.sourceLevel
}

// TargetLevel returns the 0-based target level of the compaction (SourceLevel + 1).
func (p *CompactionPlan) TargetLevel() int {
	return p.targetLevel
}

// SourceFiles returns an independent defensive copy of the source SSTable metadata slice.
func (p *CompactionPlan) SourceFiles() []version.FileMetadata {
	out := make([]version.FileMetadata, len(p.sourceFiles))
	for i, f := range p.sourceFiles {
		out[i] = f.Clone()
	}
	return out
}

// TargetFiles returns an independent defensive copy of the target SSTable metadata slice.
func (p *CompactionPlan) TargetFiles() []version.FileMetadata {
	out := make([]version.FileMetadata, len(p.targetFiles))
	for i, f := range p.targetFiles {
		out[i] = f.Clone()
	}
	return out
}

// AllInputFiles returns an independent defensive copy of all input files (source files followed by target files).
func (p *CompactionPlan) AllInputFiles() []version.FileMetadata {
	total := len(p.sourceFiles) + len(p.targetFiles)
	out := make([]version.FileMetadata, 0, total)
	for _, f := range p.sourceFiles {
		out = append(out, f.Clone())
	}
	for _, f := range p.targetFiles {
		out = append(out, f.Clone())
	}
	return out
}

// SmallestKey returns a clone of the lowest InternalKey across all compaction inputs.
func (p *CompactionPlan) SmallestKey() binary.InternalKey {
	return p.smallestKey.Clone()
}

// LargestKey returns a clone of the highest InternalKey across all compaction inputs.
func (p *CompactionPlan) LargestKey() binary.InternalKey {
	return p.largestKey.Clone()
}

// SmallestUserKey returns an independent copy of the minimum user key across all compaction inputs.
func (p *CompactionPlan) SmallestUserKey() []byte {
	return bytes.Clone(p.smallestUserKey)
}

// LargestUserKey returns an independent copy of the maximum user key across all compaction inputs.
func (p *CompactionPlan) LargestUserKey() []byte {
	return bytes.Clone(p.largestUserKey)
}

// SourceInputBytes returns the sum of file sizes across all source files.
func (p *CompactionPlan) SourceInputBytes() uint64 {
	return p.sourceInputBytes
}

// TargetInputBytes returns the sum of file sizes across all target files.
func (p *CompactionPlan) TargetInputBytes() uint64 {
	return p.targetInputBytes
}

// TotalInputBytes returns the cumulative byte size of all participating source and target files.
func (p *CompactionPlan) TotalInputBytes() uint64 {
	return p.totalInputBytes
}

// Score returns the compaction priority score that triggered this plan.
func (p *CompactionPlan) Score() float64 {
	return p.score
}

// Validate verifies that the CompactionPlan satisfies all structural and semantic invariants.
func (p *CompactionPlan) Validate() error {
	if p == nil {
		return errors.ErrNilReceiver
	}

	// 1. Level bounds: 0 <= sourceLevel < version.NumLevels - 1
	if p.sourceLevel < 0 || p.sourceLevel >= version.NumLevels-1 {
		var lvl uint32
		if p.sourceLevel > 0 {
			lvl = uint32(p.sourceLevel) // #nosec G115 -- guarded by p.sourceLevel > 0
		}
		return &errors.InvalidLevelError{Level: lvl, MaxLevel: uint32(version.NumLevels - 2)}
	}
	if p.targetLevel != p.sourceLevel+1 {
		return fmt.Errorf("%w: target level %d must be exactly source level %d + 1", errors.ErrInvalidCompactionPlan, p.targetLevel, p.sourceLevel)
	}

	// 2. Source files must be non-empty
	if len(p.sourceFiles) == 0 {
		return fmt.Errorf("%w: source files cannot be empty", errors.ErrInvalidCompactionPlan)
	}

	srcLvl := uint32(p.sourceLevel) // #nosec G115 -- validated 0 <= sourceLevel < NumLevels-1
	tgtLvl := uint32(p.targetLevel) // #nosec G115 -- validated targetLevel = sourceLevel+1 <= NumLevels-1

	// 3. Unique file numbers and metadata validation
	seen := make(map[uint64]struct{}, len(p.sourceFiles)+len(p.targetFiles))
	for _, f := range p.sourceFiles {
		if err := version.ValidateFileMetadata(srcLvl, f); err != nil {
			return fmt.Errorf("source file %d validation failed: %w", f.FileNum, err)
		}
		if _, exists := seen[f.FileNum]; exists {
			return fmt.Errorf("%w: duplicate file %d in source files", errors.ErrInvalidCompactionPlan, f.FileNum)
		}
		seen[f.FileNum] = struct{}{}
	}

	for _, f := range p.targetFiles {
		if err := version.ValidateFileMetadata(tgtLvl, f); err != nil {
			return fmt.Errorf("target file %d validation failed: %w", f.FileNum, err)
		}
		if _, exists := seen[f.FileNum]; exists {
			return fmt.Errorf("%w: file %d appears in both source and target files or duplicate in target", errors.ErrInvalidCompactionPlan, f.FileNum)
		}
		seen[f.FileNum] = struct{}{}
	}

	// 4. Non-overlapping checks for L1+ source files
	if p.sourceLevel >= 1 && len(p.sourceFiles) > 1 {
		for i := 0; i < len(p.sourceFiles)-1; i++ {
			prevMax, _, err := ExtractUserKeyRange(p.sourceFiles[i])
			if err != nil {
				return err
			}
			currMin, _, err := ExtractUserKeyRange(p.sourceFiles[i+1])
			if err != nil {
				return err
			}
			if bytes.Compare(prevMax, currMin) >= 0 {
				return fmt.Errorf("%w: source files %d and %d have overlapping user keys at level %d", errors.ErrInvalidKeyRange, p.sourceFiles[i].FileNum, p.sourceFiles[i+1].FileNum, p.sourceLevel)
			}
		}
	}

	// 5. Non-overlapping checks for target files (targetLevel >= 1 always)
	if len(p.targetFiles) > 1 {
		for i := 0; i < len(p.targetFiles)-1; i++ {
			prevMax, _, err := ExtractUserKeyRange(p.targetFiles[i])
			if err != nil {
				return err
			}
			currMin, _, err := ExtractUserKeyRange(p.targetFiles[i+1])
			if err != nil {
				return err
			}
			if bytes.Compare(prevMax, currMin) >= 0 {
				return fmt.Errorf("%w: target files %d and %d have overlapping user keys at level %d", errors.ErrInvalidKeyRange, p.targetFiles[i].FileNum, p.targetFiles[i+1].FileNum, p.targetLevel)
			}
		}
	}

	// 6. Target overlap verification: every target file must overlap source user key range
	_, _, srcMinUser, srcMaxUser, err := ComputeKeyRange(p.sourceFiles)
	if err != nil {
		return err
	}
	for _, f := range p.targetFiles {
		overlaps, err := FileOverlapsRange(f, srcMinUser, srcMaxUser)
		if err != nil {
			return err
		}
		if !overlaps {
			return fmt.Errorf("%w: target file %d does not overlap source range", errors.ErrInvalidCompactionPlan, f.FileNum)
		}
	}

	// 7. Overall key bounds validation
	if binary.CompareInternalKey(p.smallestKey, p.largestKey) > 0 {
		return fmt.Errorf("%w: plan smallest key exceeds largest key", errors.ErrInvalidKeyRange)
	}
	if bytes.Compare(p.smallestUserKey, p.largestUserKey) > 0 {
		return fmt.Errorf("%w: plan smallest user key exceeds largest user key", errors.ErrInvalidKeyRange)
	}

	// 8. Byte size accumulator check
	var actualSrcBytes, actualTgtBytes uint64
	for _, f := range p.sourceFiles {
		if math.MaxUint64-actualSrcBytes < f.FileSize {
			return errors.ErrByteCountOverflow
		}
		actualSrcBytes += f.FileSize
	}
	for _, f := range p.targetFiles {
		if math.MaxUint64-actualTgtBytes < f.FileSize {
			return errors.ErrByteCountOverflow
		}
		actualTgtBytes += f.FileSize
	}

	if actualSrcBytes != p.sourceInputBytes || actualTgtBytes != p.targetInputBytes {
		return fmt.Errorf("%w: plan byte size mismatch", errors.ErrInvalidCompactionPlan)
	}
	if math.MaxUint64-actualSrcBytes < actualTgtBytes {
		return errors.ErrByteCountOverflow
	}
	if p.totalInputBytes != actualSrcBytes+actualTgtBytes {
		return fmt.Errorf("%w: plan total byte size mismatch", errors.ErrInvalidCompactionPlan)
	}

	// 9. Priority score validation
	if math.IsNaN(p.score) || math.IsInf(p.score, 0) || p.score < 0.0 {
		return fmt.Errorf("%w: invalid compaction score %f", errors.ErrInvalidCompactionPlan, p.score)
	}

	return nil
}

// ValidateAgainstVersion verifies that the CompactionPlan is valid against the provided Version snapshot:
//  1. The plan satisfies Validate().
//  2. All source files exist in v.Files(sourceLevel).
//  3. All target files exist in v.Files(targetLevel).
//  4. If sourceLevel == 0: no file in v.Files(0) outside sourceFiles overlaps the source key range (transitive closure).
func (p *CompactionPlan) ValidateAgainstVersion(v *version.Version) error {
	if v == nil {
		return errors.ErrNilReceiver
	}
	if err := p.Validate(); err != nil {
		return err
	}

	vSrcFiles := v.Files(p.sourceLevel)
	vSrcMap := make(map[uint64]version.FileMetadata, len(vSrcFiles))
	for _, f := range vSrcFiles {
		vSrcMap[f.FileNum] = f
	}

	for _, f := range p.sourceFiles {
		vf, exists := vSrcMap[f.FileNum]
		if !exists || !f.Equal(vf) {
			return fmt.Errorf("%w: source file %d missing or mismatched in Version level %d", errors.ErrInvalidCompactionPlan, f.FileNum, p.sourceLevel)
		}
	}

	vTgtFiles := v.Files(p.targetLevel)
	vTgtMap := make(map[uint64]version.FileMetadata, len(vTgtFiles))
	for _, f := range vTgtFiles {
		vTgtMap[f.FileNum] = f
	}

	for _, f := range p.targetFiles {
		vf, exists := vTgtMap[f.FileNum]
		if !exists || !f.Equal(vf) {
			return fmt.Errorf("%w: target file %d missing or mismatched in Version level %d", errors.ErrInvalidCompactionPlan, f.FileNum, p.targetLevel)
		}
	}

	// If sourceLevel == 0, verify transitive closure: no unselected L0 file may overlap source range
	if p.sourceLevel == 0 {
		_, _, srcMinUser, srcMaxUser, err := ComputeKeyRange(p.sourceFiles)
		if err != nil {
			return err
		}
		pSrcMap := make(map[uint64]struct{}, len(p.sourceFiles))
		for _, f := range p.sourceFiles {
			pSrcMap[f.FileNum] = struct{}{}
		}

		for _, f := range vSrcFiles {
			if _, inPlan := pSrcMap[f.FileNum]; inPlan {
				continue
			}
			overlaps, err := FileOverlapsRange(f, srcMinUser, srcMaxUser)
			if err != nil {
				return err
			}
			if overlaps {
				return fmt.Errorf("%w: unselected L0 file %d overlaps source compaction range (transitive closure violated)", errors.ErrInvalidCompactionPlan, f.FileNum)
			}
		}
	}

	return nil
}

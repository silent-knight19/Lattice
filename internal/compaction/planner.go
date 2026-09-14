package compaction

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// Planner implements the pure compaction planning engine.
// It determines whether compaction should run, selects eligible levels using policy heuristics,
// computes transitive L0 overlap closures, identifies overlapping target runs, and constructs
// immutable, validated CompactionPlan snapshots.
//
// The Planner is stateless, deterministic, performs zero filesystem I/O, and does not mutate Versions.
type Planner struct {
	policy CompactionPolicy
}

// NewPlanner constructs an initialized Planner configured with the given CompactionPolicy.
func NewPlanner(policy CompactionPolicy) (*Planner, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &Planner{policy: policy}, nil
}

// Policy returns a copy of the active CompactionPolicy.
func (p *Planner) Policy() CompactionPolicy {
	return p.policy
}

// PickCompactionLevel evaluates compaction scores across all candidate source levels (0 through MaxLevels-2).
//
// Selection & Prioritization Invariants:
//  1. Level 0 is strictly prioritized over deeper levels if Score(L0) >= 1.0 (bounding read amplification).
//  2. If Score(L0) < 1.0, the level with the highest score >= 1.0 across L1 through MaxLevels-2 is chosen.
//  3. Deterministic Tie-Breaking: If multiple levels share the exact same maximum score >= 1.0,
//     the lower level index is prioritized (e.g. L1 before L2).
//  4. If no level achieves a score >= 1.0, shouldCompact returns false.
func (p *Planner) PickCompactionLevel(v *version.Version) (level int, score float64, shouldCompact bool, err error) {
	if v == nil {
		return -1, 0, false, errors.ErrNilReceiver
	}

	maxSourceLevel := p.policy.MaxLevels - 2

	// 1. Evaluate Level 0 priority
	l0Score, err := p.policy.ScoreLevel(v, 0)
	if err != nil {
		return -1, 0, false, err
	}
	if l0Score >= 1.0 && v.NumFiles(0) > 0 {
		return 0, l0Score, true, nil
	}

	// 2. Evaluate Levels 1 through maxSourceLevel
	bestLevel := -1
	bestScore := 0.0

	for lvl := 1; lvl <= maxSourceLevel; lvl++ {
		lvlScore, err := p.policy.ScoreLevel(v, lvl)
		if err != nil {
			return -1, 0, false, err
		}

		if lvlScore >= 1.0 && v.NumFiles(lvl) > 0 {
			if lvlScore > bestScore {
				bestScore = lvlScore
				bestLevel = lvl
			}
		}
	}

	if bestLevel >= 0 {
		return bestLevel, bestScore, true, nil
	}

	return -1, 0, false, nil
}

// PickCompaction evaluates the Version snapshot against policy heuristics and returns a deterministic CompactionPlan.
// If no level requires compaction (all scores < 1.0), it returns (nil, nil).
func (p *Planner) PickCompaction(v *version.Version) (*CompactionPlan, error) {
	if v == nil {
		return nil, errors.ErrNilReceiver
	}

	level, _, shouldCompact, err := p.PickCompactionLevel(v)
	if err != nil {
		return nil, err
	}
	if !shouldCompact {
		return nil, nil
	}

	files := v.Files(level)
	if len(files) == 0 {
		return nil, nil
	}

	// Deterministic sorting of candidate files to guarantee map/slice order independence:
	// For L0: sort by FileNum ascending (oldest file first)
	// For L1+: sort canonically by SmallestKey ascending
	sortedFiles := make([]version.FileMetadata, len(files))
	copy(sortedFiles, files)
	if level == 0 {
		slices.SortFunc(sortedFiles, func(a, b version.FileMetadata) int {
			return cmp.Compare(a.FileNum, b.FileNum)
		})
	} else {
		slices.SortFunc(sortedFiles, func(a, b version.FileMetadata) int {
			ikA, _ := binary.DecodeInternalKey(a.SmallestKey)
			ikB, _ := binary.DecodeInternalKey(b.SmallestKey)
			return binary.CompareInternalKey(ikA, ikB)
		})
	}

	seed := []version.FileMetadata{sortedFiles[0]}

	return p.PlanCompaction(v, level, seed)
}

// PlanCompaction constructs an immutable CompactionPlan for an explicitly specified source level and seed files.
//
// Invariants & Operational Semantics:
//  1. Source Level Bounds: 0 <= sourceLevel < policy.MaxLevels - 1.
//  2. Target Level: sourceLevel + 1.
//  3. L0 Transitive Closure: If sourceLevel == 0, seedFiles is expanded via ExpandL0OverlapClosure
//     across all L0 files to ensure closure under overlap.
//  4. L1+ Non-Overlap: If sourceLevel >= 1, seed files must be non-overlapping and canonically sorted.
//  5. Target Run Overlap: Target files in sourceLevel + 1 are identified by user key range intersection.
//  6. Arithmetic Safety: Cumulative byte sizes guard against 64-bit integer overflow.
//  7. Defensive Immutability: All output structures and keys are cloned.
func (p *Planner) PlanCompaction(v *version.Version, sourceLevel int, seedFiles []version.FileMetadata) (*CompactionPlan, error) {
	if v == nil {
		return nil, errors.ErrNilReceiver
	}
	if sourceLevel < 0 || sourceLevel >= p.policy.MaxLevels-1 {
		var lvl uint32
		if sourceLevel > 0 {
			lvl = uint32(sourceLevel) // #nosec G115 -- guarded by sourceLevel > 0
		}
		var maxLvl uint32
		if p.policy.MaxLevels > 1 {
			maxLvl = uint32(p.policy.MaxLevels - 2) // #nosec G115 -- guarded by p.policy.MaxLevels > 1
		}
		return nil, &errors.InvalidLevelError{Level: lvl, MaxLevel: maxLvl}
	}
	if len(seedFiles) == 0 {
		return nil, fmt.Errorf("%w: seed files cannot be empty", errors.ErrInvalidCompactionPlan)
	}

	targetLevel := sourceLevel + 1

	// Validate that all seed files actually exist in Version level sourceLevel
	vSrcFiles := v.Files(sourceLevel)
	vSrcMap := make(map[uint64]version.FileMetadata, len(vSrcFiles))
	for _, f := range vSrcFiles {
		vSrcMap[f.FileNum] = f
	}

	for _, f := range seedFiles {
		vf, exists := vSrcMap[f.FileNum]
		if !exists || !f.Equal(vf) {
			return nil, fmt.Errorf("%w: seed file %d does not exist in Version level %d", errors.ErrInvalidCompactionPlan, f.FileNum, sourceLevel)
		}
	}

	var finalSourceFiles []version.FileMetadata

	if sourceLevel == 0 {
		// Expand transitive overlap closure across all L0 files
		closedL0, err := ExpandL0OverlapClosure(vSrcFiles, seedFiles)
		if err != nil {
			return nil, err
		}
		finalSourceFiles = closedL0
	} else {
		// Deduplicate seed files by FileNum
		dedupMap := make(map[uint64]version.FileMetadata, len(seedFiles))
		for _, f := range seedFiles {
			dedupMap[f.FileNum] = f.Clone()
		}
		finalSourceFiles = make([]version.FileMetadata, 0, len(dedupMap))
		for _, f := range dedupMap {
			finalSourceFiles = append(finalSourceFiles, f)
		}

		// Sort canonically by SmallestKey
		slices.SortFunc(finalSourceFiles, func(a, b version.FileMetadata) int {
			ikA, _ := binary.DecodeInternalKey(a.SmallestKey)
			ikB, _ := binary.DecodeInternalKey(b.SmallestKey)
			return binary.CompareInternalKey(ikA, ikB)
		})

		// Assert non-overlapping invariant for source files at L1+
		if len(finalSourceFiles) > 1 {
			for i := 0; i < len(finalSourceFiles)-1; i++ {
				prevMax, _, err := ExtractUserKeyRange(finalSourceFiles[i])
				if err != nil {
					return nil, err
				}
				currMin, _, err := ExtractUserKeyRange(finalSourceFiles[i+1])
				if err != nil {
					return nil, err
				}
				if bytes.Compare(prevMax, currMin) >= 0 {
					return nil, fmt.Errorf("%w: seed files %d and %d have overlapping user keys at level %d", errors.ErrInvalidKeyRange, finalSourceFiles[i].FileNum, finalSourceFiles[i+1].FileNum, sourceLevel)
				}
			}
		}
	}

	// Compute source user key range
	_, _, srcMinUser, srcMaxUser, err := ComputeKeyRange(finalSourceFiles)
	if err != nil {
		return nil, err
	}

	// Identify all overlapping files in target level
	vTgtFiles := v.Files(targetLevel)
	targetFiles, err := GetOverlappingInputs(vTgtFiles, srcMinUser, srcMaxUser)
	if err != nil {
		return nil, err
	}

	// Sort target files canonically by SmallestKey
	slices.SortFunc(targetFiles, func(a, b version.FileMetadata) int {
		ikA, _ := binary.DecodeInternalKey(a.SmallestKey)
		ikB, _ := binary.DecodeInternalKey(b.SmallestKey)
		return binary.CompareInternalKey(ikA, ikB)
	})

	// Compute overall key range across all participating inputs
	allInputs := make([]version.FileMetadata, 0, len(finalSourceFiles)+len(targetFiles))
	allInputs = append(allInputs, finalSourceFiles...)
	allInputs = append(allInputs, targetFiles...)

	minIK, maxIK, minUser, maxUser, err := ComputeKeyRange(allInputs)
	if err != nil {
		return nil, err
	}

	// Compute byte estimates with overflow protection
	var srcBytes, tgtBytes uint64
	for _, f := range finalSourceFiles {
		if math.MaxUint64-srcBytes < f.FileSize {
			return nil, errors.ErrByteCountOverflow
		}
		srcBytes += f.FileSize
	}
	for _, f := range targetFiles {
		if math.MaxUint64-tgtBytes < f.FileSize {
			return nil, errors.ErrByteCountOverflow
		}
		tgtBytes += f.FileSize
	}

	score, err := p.policy.ScoreLevel(v, sourceLevel)
	if err != nil {
		return nil, err
	}

	return NewCompactionPlan(
		sourceLevel,
		targetLevel,
		finalSourceFiles,
		targetFiles,
		minIK,
		maxIK,
		minUser,
		maxUser,
		srcBytes,
		tgtBytes,
		score,
	)
}

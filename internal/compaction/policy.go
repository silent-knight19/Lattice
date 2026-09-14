package compaction

import (
	"fmt"
	"math"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

const (
	// DefaultL0TriggerCount is the standard threshold of L0 SSTables that triggers compaction (4 files).
	DefaultL0TriggerCount = 4

	// DefaultL1TargetBytes is the baseline byte size capacity for Level 1 (10 MiB).
	DefaultL1TargetBytes uint64 = 10 * 1024 * 1024

	// DefaultLevelMultiplier is the standard exponential capacity factor between adjacent levels (10.0x).
	DefaultLevelMultiplier = 10.0
)

// CompactionPolicy configures the heuristics and sizing thresholds for leveled compaction.
//
// Invariants:
//  1. Level 0 capacity is governed strictly by file count (L0TriggerCount).
//  2. Levels 1 through MaxLevels-1 are governed by total byte capacity:
//     Capacity(L_i) = L1TargetBytes * (LevelMultiplier)^(i-1).
//  3. Determinism: Given identical Version state and policy, all scoring decisions are reproducible.
type CompactionPolicy struct {
	// L0TriggerCount is the file count in L0 required to trigger compaction.
	L0TriggerCount int

	// L1TargetBytes is the target byte capacity for Level 1.
	L1TargetBytes uint64

	// LevelMultiplier is the size growth factor applied to each subsequent level.
	LevelMultiplier float64

	// MaxLevels is the total number of LSM-tree levels supported (default: version.NumLevels = 7).
	MaxLevels int
}

// DefaultCompactionPolicy returns the production default compaction configuration matching ADR-005.
func DefaultCompactionPolicy() CompactionPolicy {
	return CompactionPolicy{
		L0TriggerCount:  DefaultL0TriggerCount,
		L1TargetBytes:   DefaultL1TargetBytes,
		LevelMultiplier: DefaultLevelMultiplier,
		MaxLevels:       version.NumLevels,
	}
}

// Validate verifies that the compaction policy parameters satisfy all structural bounds.
func (p CompactionPolicy) Validate() error {
	if p.L0TriggerCount <= 0 {
		return fmt.Errorf("%w: L0TriggerCount must be positive (got %d)", errors.ErrInvalidCompactionPlan, p.L0TriggerCount)
	}
	if p.L1TargetBytes == 0 {
		return fmt.Errorf("%w: L1TargetBytes must be positive", errors.ErrInvalidCompactionPlan)
	}
	if p.LevelMultiplier < 1.0 || math.IsNaN(p.LevelMultiplier) || math.IsInf(p.LevelMultiplier, 0) {
		return fmt.Errorf("%w: LevelMultiplier must be >= 1.0 (got %f)", errors.ErrInvalidCompactionPlan, p.LevelMultiplier)
	}
	if p.MaxLevels < 2 || p.MaxLevels > version.NumLevels {
		return fmt.Errorf("%w: MaxLevels must be in [2, %d] (got %d)", errors.ErrInvalidLevel, version.NumLevels, p.MaxLevels)
	}
	return nil
}

// TargetBytesForLevel calculates the total byte threshold for the given level.
// Level 0 returns 0 because L0 capacity is governed by file count.
// For level >= 1, it computes L1TargetBytes * (LevelMultiplier)^(level-1) with math.MaxUint64 overflow guards.
func (p CompactionPolicy) TargetBytesForLevel(level int) uint64 {
	if level <= 0 {
		return 0
	}
	if level >= p.MaxLevels {
		return math.MaxUint64
	}

	mult := math.Pow(p.LevelMultiplier, float64(level-1))
	if math.IsInf(mult, 0) || mult > float64(math.MaxUint64) {
		return math.MaxUint64
	}

	targetFloat := float64(p.L1TargetBytes) * mult
	if targetFloat >= float64(math.MaxUint64) {
		return math.MaxUint64
	}

	return uint64(targetFloat)
}

// ScoreLevel evaluates the compaction score for the designated level against the provided Version snapshot.
// Returns a float64 score where score >= 1.0 signifies that the level has reached or exceeded capacity.
func (p CompactionPolicy) ScoreLevel(v *version.Version, level int) (float64, error) {
	if v == nil {
		return 0, errors.ErrNilReceiver
	}
	if err := p.Validate(); err != nil {
		return 0, err
	}
	if level < 0 || level >= p.MaxLevels {
		var lvl uint32
		if level > 0 {
			lvl = uint32(level) // #nosec G115 -- guarded by level > 0
		}
		var maxLvl uint32
		if p.MaxLevels > 0 {
			maxLvl = uint32(p.MaxLevels - 1) // #nosec G115 -- guarded by p.MaxLevels > 0
		}
		return 0, &errors.InvalidLevelError{Level: lvl, MaxLevel: maxLvl}
	}

	if level == 0 {
		count := v.NumFiles(0)
		return float64(count) / float64(p.L0TriggerCount), nil
	}

	files := v.Files(level)
	var totalBytes uint64
	for _, f := range files {
		if math.MaxUint64-totalBytes < f.FileSize {
			return 0, errors.ErrByteCountOverflow
		}
		totalBytes += f.FileSize
	}

	target := p.TargetBytesForLevel(level)
	if target == 0 {
		return 0, nil
	}

	return float64(totalBytes) / float64(target), nil
}

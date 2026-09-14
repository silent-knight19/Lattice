package version

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// applyEditToVersion derives a new immutable Version snapshot from a base Version
// and a state-delta VersionEdit, verifying all level, scalar, uniqueness, and ordering invariants.
//
// Invariants & Parity Contract:
//  1. Rejects nil edit with ErrNilVersionEdit.
//  2. Validates edit structural invariants via edit.Validate().
//  3. Enforces monotonic scalar progression: NextFileNum and LastSeqNum cannot regress.
//  4. Enforces file number upper bound: all added files must satisfy FileNum < nextFileNum (if known).
//  5. Enforces valid deletions: every deleted file must exist at the specified level in the base Version.
//  6. Enforces live file limit budget: total live files across all levels cannot exceed manifestReplayMaxFiles.
//  7. Enforces single-level identity and identity uniqueness: an added file cannot already exist in any level.
//  8. Prohibits delete/add collision: a file cannot be both deleted and added in the same edit.
//  9. Level 0 sorting: sorted deterministically by FileNum ASC.
//
// 10. Levels 1..6 canonical sorting: sorted canonically by SmallestKey ascending.
// 11. Levels 1..6 non-overlapping ranges: enforces that no two files at levels 1..6 overlap in key range.
// 12. Returns a new immutable Version with refCount = 1 (caller ownership).
func applyEditToVersion(base *Version, edit *VersionEdit, curNextFile uint64, curLastSeq binary.SeqNum) (*Version, uint64, binary.SeqNum, error) {
	if edit == nil {
		return nil, 0, 0, errors.ErrNilVersionEdit
	}

	if err := edit.Validate(); err != nil {
		return nil, 0, 0, err
	}

	// 1. Monotonic scalar progression
	newNextFile := curNextFile
	if nextNum, ok := edit.NextFileNum(); ok {
		if nextNum < curNextFile {
			return nil, 0, 0, fmt.Errorf("%w: next file num regressed from %d to %d",
				errors.ErrCorruptedVersionEdit, curNextFile, nextNum)
		}
		newNextFile = nextNum
	}

	newLastSeq := curLastSeq
	if lastSeq, ok := edit.LastSeqNum(); ok {
		if lastSeq < curLastSeq {
			return nil, 0, 0, fmt.Errorf("%w: last seq num regressed from %d to %d",
				errors.ErrCorruptedVersionEdit, curLastSeq, lastSeq)
		}
		newLastSeq = lastSeq
	}

	// 2. Added files allocator bound
	for _, a := range edit.AddedFiles() {
		if newNextFile > 0 && a.Meta.FileNum >= newNextFile {
			return nil, 0, 0, fmt.Errorf("%w: added file %d exceeds or equals next file num %d",
				errors.ErrCorruptedVersionEdit, a.Meta.FileNum, newNextFile)
		}
	}

	// 3. Clone base files into working level maps
	workingLevels := [NumLevels]map[uint64]FileMetadata{}
	for lvl := 0; lvl < NumLevels; lvl++ {
		workingLevels[lvl] = make(map[uint64]FileMetadata)
		if base != nil {
			for _, f := range base.levels[lvl] {
				workingLevels[lvl][f.FileNum] = f.Clone()
			}
		}
	}

	// 4. Prohibit delete/add collision across the edit
	deletedMap := make(map[uint64]uint32, len(edit.DeletedFiles()))
	for _, d := range edit.DeletedFiles() {
		deletedMap[d.FileNum] = d.Level
	}
	for _, a := range edit.AddedFiles() {
		if delLevel, exists := deletedMap[a.Meta.FileNum]; exists {
			return nil, 0, 0, fmt.Errorf("%w: file %d cannot be both deleted (level %d) and added (level %d) in the same edit",
				errors.ErrCorruptedVersionEdit, a.Meta.FileNum, delLevel, a.Level)
		}
	}

	// 5. Apply deletions (must exist in base)
	for _, d := range edit.DeletedFiles() {
		if d.Level >= NumLevels {
			return nil, 0, 0, &errors.InvalidLevelError{Level: d.Level, MaxLevel: NumLevels - 1}
		}
		if base == nil {
			return nil, 0, 0, fmt.Errorf("%w: cannot delete file %d at level %d from uninitialized Version",
				errors.ErrCorruptedVersionEdit, d.FileNum, d.Level)
		}
		if _, exists := workingLevels[d.Level][d.FileNum]; !exists {
			return nil, 0, 0, fmt.Errorf("%w: file %d not found at level %d for deletion",
				errors.ErrCorruptedVersionEdit, d.FileNum, d.Level)
		}
		delete(workingLevels[d.Level], d.FileNum)
	}

	// 6. Live file count budget check
	currentFiles := 0
	for lvl := 0; lvl < NumLevels; lvl++ {
		currentFiles += len(workingLevels[lvl])
	}
	newAdditions := 0
	for _, a := range edit.AddedFiles() {
		if a.Level < NumLevels {
			if _, exists := workingLevels[a.Level][a.Meta.FileNum]; !exists {
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
		return nil, 0, 0, &errors.ManifestReplayLimitError{
			Resource: "live_files",
			Current:  curFiles,
			Limit:    limFiles,
		}
	}

	// 7. Apply additions with single-level identity and uniqueness checks
	for _, a := range edit.AddedFiles() {
		if a.Level >= NumLevels {
			return nil, 0, 0, &errors.InvalidLevelError{Level: a.Level, MaxLevel: NumLevels - 1}
		}

		// Check if file already exists at target level
		if _, exists := workingLevels[a.Level][a.Meta.FileNum]; exists {
			return nil, 0, 0, fmt.Errorf("%w: file %d already exists at level %d, cannot add",
				errors.ErrCorruptedVersionEdit, a.Meta.FileNum, a.Level)
		}

		// Enforce single-level identity: cannot exist at any other level
		for lvl := 0; lvl < NumLevels; lvl++ {
			if uint32(lvl) != a.Level {
				if _, exists := workingLevels[lvl][a.Meta.FileNum]; exists {
					return nil, 0, 0, fmt.Errorf("%w: file %d already exists at level %d, cannot add to level %d",
						errors.ErrCorruptedVersionEdit, a.Meta.FileNum, lvl, a.Level)
				}
			}
		}

		workingLevels[a.Level][a.Meta.FileNum] = a.Meta.Clone()
	}

	// 8. Reconstruct and validate level slices
	var finalLevels [NumLevels][]FileMetadata

	// Level 0: Sort deterministically by FileNum ASC
	l0 := make([]FileMetadata, 0, len(workingLevels[0]))
	for _, f := range workingLevels[0] {
		l0 = append(l0, f)
	}
	slices.SortFunc(l0, func(a, b FileMetadata) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})
	finalLevels[0] = l0

	// Levels 1..6: Sort canonically by SmallestKey and assert non-overlapping key ranges
	for lvl := 1; lvl < NumLevels; lvl++ {
		lvlFiles := make([]FileMetadata, 0, len(workingLevels[lvl]))
		for _, f := range workingLevels[lvl] {
			lvlFiles = append(lvlFiles, f)
		}

		slices.SortFunc(lvlFiles, func(a, b FileMetadata) int {
			ikA := decodeInternalKeyNoAlloc(a.SmallestKey)
			ikB := decodeInternalKeyNoAlloc(b.SmallestKey)
			return binary.CompareInternalKey(ikA, ikB)
		})

		for i := 0; i < len(lvlFiles)-1; i++ {
			prevLargest := decodeInternalKeyNoAlloc(lvlFiles[i].LargestKey)
			currSmallest := decodeInternalKeyNoAlloc(lvlFiles[i+1].SmallestKey)
			if bytes.Compare(prevLargest.UserKey, currSmallest.UserKey) >= 0 || binary.CompareInternalKey(prevLargest, currSmallest) >= 0 {
				return nil, 0, 0, fmt.Errorf("%w: overlapping key ranges at level %d between file %d and file %d",
					errors.ErrInvalidKeyRange, lvl, lvlFiles[i].FileNum, lvlFiles[i+1].FileNum)
			}
		}

		finalLevels[lvl] = lvlFiles
	}

	return NewVersion(finalLevels), newNextFile, newLastSeq, nil
}

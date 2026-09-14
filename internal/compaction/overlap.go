package compaction

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// ExtractUserKeyRange validates and extracts the user key bounds from an SSTable's persistent FileMetadata.
// The returned byte slices are defensive copies owned by the caller.
func ExtractUserKeyRange(meta version.FileMetadata) (minUser, maxUser []byte, err error) {
	if meta.FileNum == 0 {
		return nil, nil, errors.ErrInvalidFileNum
	}
	if meta.FileSize == 0 {
		return nil, nil, errors.ErrInvalidFileSize
	}
	if meta.SmallestSeqNum > meta.LargestSeqNum {
		return nil, nil, errors.ErrInvalidSeqNumRange
	}

	ikSmall, err := binary.DecodeInternalKey(meta.SmallestKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode smallest internal key for file %d: %w", meta.FileNum, err)
	}
	ikLarge, err := binary.DecodeInternalKey(meta.LargestKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decode largest internal key for file %d: %w", meta.FileNum, err)
	}

	if binary.CompareInternalKey(ikSmall, ikLarge) > 0 {
		return nil, nil, fmt.Errorf("%w: file %d smallest key exceeds largest key", errors.ErrInvalidKeyRange, meta.FileNum)
	}

	return bytes.Clone(ikSmall.UserKey), bytes.Clone(ikLarge.UserKey), nil
}

// KeyRangesOverlap evaluates whether two closed user key intervals [minA, maxA] and [minB, maxB] intersect.
//
// In Lattice's leveled storage engine, adjacent files at L1+ cannot even share a user key.
// Therefore, ranges that touch at boundary endpoints (e.g. [a, b] and [b, c] sharing "b") are defined
// as overlapping and must participate in the same compaction run to prevent duplicate user keys in L1+.
func KeyRangesOverlap(minA, maxA, minB, maxB []byte) bool {
	return bytes.Compare(maxA, minB) >= 0 && bytes.Compare(maxB, minA) >= 0
}

// FileOverlapsRange reports whether the given file's user key range intersects with [minUserKey, maxUserKey].
func FileOverlapsRange(f version.FileMetadata, minUserKey, maxUserKey []byte) (bool, error) {
	fMin, fMax, err := ExtractUserKeyRange(f)
	if err != nil {
		return false, err
	}
	return KeyRangesOverlap(fMin, fMax, minUserKey, maxUserKey), nil
}

// FilesOverlap reports whether two SSTable files have overlapping user key ranges.
func FilesOverlap(a, b version.FileMetadata) (bool, error) {
	aMin, aMax, err := ExtractUserKeyRange(a)
	if err != nil {
		return false, err
	}
	bMin, bMax, err := ExtractUserKeyRange(b)
	if err != nil {
		return false, err
	}
	return KeyRangesOverlap(aMin, aMax, bMin, bMax), nil
}

// GetOverlappingInputs scans a slice of SSTable files and returns all files whose user key ranges
// intersect with [minUserKey, maxUserKey].
//
// The returned slice is sorted deterministically:
//   - If files are non-overlapping (L1+), sorted canonically by SmallestKey.
//   - If files may overlap (L0), sorted by FileNum ascending.
func GetOverlappingInputs(files []version.FileMetadata, minUserKey, maxUserKey []byte) ([]version.FileMetadata, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if bytes.Compare(minUserKey, maxUserKey) > 0 {
		return nil, fmt.Errorf("%w: minUserKey exceeds maxUserKey", errors.ErrInvalidKeyRange)
	}

	var result []version.FileMetadata
	for _, f := range files {
		overlaps, err := FileOverlapsRange(f, minUserKey, maxUserKey)
		if err != nil {
			return nil, err
		}
		if overlaps {
			result = append(result, f.Clone())
		}
	}

	return result, nil
}

// ComputeKeyRange calculates the overall InternalKey bounds and UserKey bounds across a set of SSTables.
func ComputeKeyRange(files []version.FileMetadata) (minIK, maxIK binary.InternalKey, minUser, maxUser []byte, err error) {
	if len(files) == 0 {
		return binary.InternalKey{}, binary.InternalKey{}, nil, nil, fmt.Errorf("%w: cannot compute key range for empty file set", errors.ErrInvalidCompactionPlan)
	}

	var (
		first      = true
		resMinIK   binary.InternalKey
		resMaxIK   binary.InternalKey
		resMinUser []byte
		resMaxUser []byte
	)

	for _, f := range files {
		ikSmall, err := binary.DecodeInternalKey(f.SmallestKey)
		if err != nil {
			return binary.InternalKey{}, binary.InternalKey{}, nil, nil, fmt.Errorf("file %d smallest key decode: %w", f.FileNum, err)
		}
		ikLarge, err := binary.DecodeInternalKey(f.LargestKey)
		if err != nil {
			return binary.InternalKey{}, binary.InternalKey{}, nil, nil, fmt.Errorf("file %d largest key decode: %w", f.FileNum, err)
		}
		if binary.CompareInternalKey(ikSmall, ikLarge) > 0 {
			return binary.InternalKey{}, binary.InternalKey{}, nil, nil, fmt.Errorf("%w: file %d smallest key exceeds largest key", errors.ErrInvalidKeyRange, f.FileNum)
		}

		if first {
			resMinIK = ikSmall.Clone()
			resMaxIK = ikLarge.Clone()
			resMinUser = bytes.Clone(ikSmall.UserKey)
			resMaxUser = bytes.Clone(ikLarge.UserKey)
			first = false
			continue
		}

		if binary.CompareInternalKey(ikSmall, resMinIK) < 0 {
			resMinIK = ikSmall.Clone()
		}
		if binary.CompareInternalKey(ikLarge, resMaxIK) > 0 {
			resMaxIK = ikLarge.Clone()
		}
		if bytes.Compare(ikSmall.UserKey, resMinUser) < 0 {
			resMinUser = bytes.Clone(ikSmall.UserKey)
		}
		if bytes.Compare(ikLarge.UserKey, resMaxUser) > 0 {
			resMaxUser = bytes.Clone(ikLarge.UserKey)
		}
	}

	return resMinIK, resMaxIK, resMinUser, resMaxUser, nil
}

// ExpandL0OverlapClosure computes the complete transitive closure of overlapping L0 SSTables.
//
// In Level 0, files may overlap arbitrarily. If file A overlaps file B, and file B overlaps file C,
// selecting A for compaction must transitively include both B and C.
//
// Algorithm:
//  1. Initialize selected set S with initialSet.
//  2. Compute [minUser, maxUser] across all files currently in S.
//  3. Find all candidate files in allL0 (not yet in S) whose user key ranges overlap [minUser, maxUser].
//  4. If new overlapping candidates are found, append them to S and repeat from step 2.
//  5. When an iteration discovers zero new overlapping files, closure is established.
//  6. Sort the final closed set deterministically by FileNum ascending.
func ExpandL0OverlapClosure(allL0 []version.FileMetadata, initialSet []version.FileMetadata) ([]version.FileMetadata, error) {
	if len(initialSet) == 0 {
		return nil, nil
	}

	// Validate all L0 candidate metadata upfront to fail-closed on corrupt metadata
	for _, f := range allL0 {
		if _, _, err := ExtractUserKeyRange(f); err != nil {
			return nil, err
		}
	}

	selectedMap := make(map[uint64]version.FileMetadata, len(allL0))
	for _, f := range initialSet {
		selectedMap[f.FileNum] = f.Clone()
	}

	for {
		// 1. Compute current bounding range of selected set
		currentSlice := make([]version.FileMetadata, 0, len(selectedMap))
		for _, f := range selectedMap {
			currentSlice = append(currentSlice, f)
		}
		_, _, minUser, maxUser, err := ComputeKeyRange(currentSlice)
		if err != nil {
			return nil, err
		}

		// 2. Scan allL0 for any unselected files overlapping [minUser, maxUser]
		added := false
		for _, f := range allL0 {
			if _, exists := selectedMap[f.FileNum]; exists {
				continue
			}

			overlaps, err := FileOverlapsRange(f, minUser, maxUser)
			if err != nil {
				return nil, err
			}
			if overlaps {
				selectedMap[f.FileNum] = f.Clone()
				added = true
			}
		}

		// 3. If no new overlapping files were discovered, transitive closure is complete
		if !added {
			break
		}
	}

	// Produce deterministically sorted slice ordered by FileNum ascending (canonical L0 order)
	result := make([]version.FileMetadata, 0, len(selectedMap))
	for _, f := range selectedMap {
		result = append(result, f)
	}
	slices.SortFunc(result, func(a, b version.FileMetadata) int {
		return cmp.Compare(a.FileNum, b.FileNum)
	})

	return result, nil
}

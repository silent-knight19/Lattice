package version

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// refVersion is an independent state oracle for differential testing.
type refVersion struct {
	levels      [NumLevels]map[uint64]FileMetadata
	nextFileNum uint64
	lastSeqNum  binary.SeqNum
}

func newRefVersion() *refVersion {
	ref := &refVersion{}
	for lvl := 0; lvl < NumLevels; lvl++ {
		ref.levels[lvl] = make(map[uint64]FileMetadata)
	}
	ref.nextFileNum = 1
	return ref
}

func (r *refVersion) clone() *refVersion {
	cp := &refVersion{
		nextFileNum: r.nextFileNum,
		lastSeqNum:  r.lastSeqNum,
	}
	for lvl := 0; lvl < NumLevels; lvl++ {
		cp.levels[lvl] = make(map[uint64]FileMetadata, len(r.levels[lvl]))
		for k, v := range r.levels[lvl] {
			cp.levels[lvl][k] = v.Clone()
		}
	}
	return cp
}

// apply attempts to apply an edit to the reference model.
// Returns true if the edit is valid according to the reference model, or false if invalid.
func (r *refVersion) apply(edit *VersionEdit) bool {
	if edit == nil {
		return false
	}
	if err := edit.Validate(); err != nil {
		return false
	}

	// Monotonic scalar checks
	newNextFile := r.nextFileNum
	if nextNum, ok := edit.NextFileNum(); ok {
		if nextNum < r.nextFileNum {
			return false
		}
		newNextFile = nextNum
	}

	newLastSeq := r.lastSeqNum
	if lastSeq, ok := edit.LastSeqNum(); ok {
		if lastSeq < r.lastSeqNum {
			return false
		}
		newLastSeq = lastSeq
	}

	// Added file scalar bounds
	for _, a := range edit.AddedFiles() {
		if newNextFile > 0 && a.Meta.FileNum >= newNextFile {
			return false
		}
	}

	// Deleted files must exist in current reference model
	tempLevels := [NumLevels]map[uint64]FileMetadata{}
	for lvl := 0; lvl < NumLevels; lvl++ {
		tempLevels[lvl] = make(map[uint64]FileMetadata, len(r.levels[lvl]))
		for k, v := range r.levels[lvl] {
			tempLevels[lvl][k] = v
		}
	}

	for _, d := range edit.DeletedFiles() {
		if d.Level >= NumLevels {
			return false
		}
		if _, exists := tempLevels[d.Level][d.FileNum]; !exists {
			return false
		}
		delete(tempLevels[d.Level], d.FileNum)
	}

	// Added files must not exist in any level
	for _, a := range edit.AddedFiles() {
		if a.Level >= NumLevels {
			return false
		}
		for lvl := 0; lvl < NumLevels; lvl++ {
			if _, exists := tempLevels[lvl][a.Meta.FileNum]; exists {
				return false
			}
		}
		tempLevels[a.Level][a.Meta.FileNum] = a.Meta
	}

	// Validate L1..L6 non-overlapping
	for lvl := 1; lvl < NumLevels; lvl++ {
		lvlFiles := make([]FileMetadata, 0, len(tempLevels[lvl]))
		for _, f := range tempLevels[lvl] {
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
			if binary.CompareInternalKey(prevLargest, currSmallest) >= 0 {
				return false
			}
		}
	}

	// Commit to reference model
	r.levels = tempLevels
	r.nextFileNum = newNextFile
	r.lastSeqNum = newLastSeq
	return true
}

func TestLogAndApply_DifferentialStateModel(t *testing.T) {
	const iterations = 2000

	dir := t.TempDir()
	manPath := filepath.Join(dir, ManifestFilename(1))
	w, err := CreateManifestWriter(manPath)
	if err != nil {
		t.Fatalf("failed to create manifest writer: %v", err)
	}
	defer func() { _ = w.Close() }()

	vs := NewVersionSetWithOptions(VersionSetOptions{
		DBPath:         dir,
		ManifestWriter: w,
		NextFileNum:    1,
		LastSeqNum:     0,
	})

	ref := newRefVersion()
	rng := rand.New(rand.NewSource(42)) // #nosec G404 -- deterministic pseudo-random generator for testing

	var currentAllocFileNum uint64 = 1
	var currentSeqNum uint64 = 1

	for iter := 0; iter < iterations; iter++ {
		action := rng.Intn(4) // 0: add file, 1: delete file, 2: compaction replacement, 3: empty or scalar bump

		edit := NewVersionEdit()

		switch action {
		case 0: // Add file
			lvl := rng.Intn(NumLevels)
			var targetLevel uint32
			if lvl > 0 {
				targetLevel = uint32(lvl) // #nosec G115 - bounded by NumLevels
			}
			fileNum := currentAllocFileNum
			currentAllocFileNum++
			currentSeqNum += 2
			edit.SetNextFileNum(currentAllocFileNum)
			edit.SetLastSeqNum(binary.SeqNum(currentSeqNum))

			// Key range
			startKey := fmt.Sprintf("k%06d_a", iter)
			endKey := fmt.Sprintf("k%06d_z", iter)
			meta := makeTestFileMeta(fileNum, 512, startKey, endKey, currentSeqNum-1, currentSeqNum)
			createMockSSTable(t, dir, fileNum, 512)
			_ = edit.AddFile(targetLevel, meta)

		case 1: // Delete file from existing
			var liveFiles []DeleteFileEntry
			for lvl := 0; lvl < NumLevels; lvl++ {
				for fNum := range ref.levels[lvl] {
					liveFiles = append(liveFiles, DeleteFileEntry{Level: uint32(lvl), FileNum: fNum})
				}
			}
			if len(liveFiles) > 0 {
				picked := liveFiles[rng.Intn(len(liveFiles))]
				_ = edit.DeleteFile(picked.Level, picked.FileNum)
			}

		case 2: // Compaction replacement: delete 1 file from L0 and add 1 file to L1
			var l0Files []uint64
			for fNum := range ref.levels[0] {
				l0Files = append(l0Files, fNum)
			}
			if len(l0Files) > 0 {
				delNum := l0Files[rng.Intn(len(l0Files))]
				_ = edit.DeleteFile(0, delNum)

				fileNum := currentAllocFileNum
				currentAllocFileNum++
				currentSeqNum += 2
				edit.SetNextFileNum(currentAllocFileNum)
				edit.SetLastSeqNum(binary.SeqNum(currentSeqNum))

				startKey := fmt.Sprintf("k%06d_a", iter)
				endKey := fmt.Sprintf("k%06d_z", iter)
				meta := makeTestFileMeta(fileNum, 1024, startKey, endKey, currentSeqNum-1, currentSeqNum)
				createMockSSTable(t, dir, fileNum, 1024)
				_ = edit.AddFile(1, meta)
			}

		case 3: // Scalar advance or empty
			currentAllocFileNum++
			currentSeqNum++
			edit.SetNextFileNum(currentAllocFileNum)
			edit.SetLastSeqNum(binary.SeqNum(currentSeqNum))
		}

		refCopy := ref.clone()
		refValid := refCopy.apply(edit)

		latticeErr := vs.LogAndApply(edit)
		latticeValid := (latticeErr == nil)

		if refValid != latticeValid {
			t.Fatalf("iteration %d: validity mismatch! refValid=%v, latticeValid=%v (err=%v)",
				iter, refValid, latticeValid, latticeErr)
		}

		if refValid {
			ref = refCopy

			// Verify state equivalence
			cur := vs.Current()
			if cur == nil {
				t.Fatalf("iteration %d: lattice Current is nil", iter)
			}

			if vs.NextFileNum() != ref.nextFileNum {
				t.Fatalf("iteration %d: nextFileNum mismatch: lattice %d vs ref %d",
					iter, vs.NextFileNum(), ref.nextFileNum)
			}
			if vs.LastSeqNum() != ref.lastSeqNum {
				t.Fatalf("iteration %d: lastSeqNum mismatch: lattice %d vs ref %d",
					iter, vs.LastSeqNum(), ref.lastSeqNum)
			}

			for lvl := 0; lvl < NumLevels; lvl++ {
				latFiles := cur.Files(lvl)
				if len(latFiles) != len(ref.levels[lvl]) {
					t.Fatalf("iteration %d: level %d file count mismatch: lattice %d vs ref %d",
						iter, lvl, len(latFiles), len(ref.levels[lvl]))
				}
				for _, f := range latFiles {
					refMeta, exists := ref.levels[lvl][f.FileNum]
					if !exists {
						t.Fatalf("iteration %d: level %d file %d not in ref", iter, lvl, f.FileNum)
					}
					if !f.Equal(refMeta) {
						t.Fatalf("iteration %d: level %d file %d metadata mismatch", iter, lvl, f.FileNum)
					}
				}
			}

			cur.Unref()
		}
	}
}

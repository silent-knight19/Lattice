package compaction

import (
	"bytes"
	stdErrors "errors"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// helper to create valid FileMetadata for testing
func makeTestFile(
	t *testing.T,
	fileNum uint64,
	fileSize uint64,
	minUser string,
	minSeq uint64,
	minOp binary.OpType,
	maxUser string,
	maxSeq uint64,
	maxOp binary.OpType,
) version.FileMetadata {
	t.Helper()
	ikSmall, err := binary.NewInternalKey([]byte(minUser), binary.SeqNum(minSeq), minOp)
	if err != nil {
		t.Fatalf("failed to create SmallestKey: %v", err)
	}
	ikLarge, err := binary.NewInternalKey([]byte(maxUser), binary.SeqNum(maxSeq), maxOp)
	if err != nil {
		t.Fatalf("failed to create LargestKey: %v", err)
	}
	encSmall := binary.EncodeInternalKey(ikSmall)
	encLarge := binary.EncodeInternalKey(ikLarge)

	sSeq := minSeq
	lSeq := maxSeq
	if sSeq > lSeq {
		sSeq, lSeq = lSeq, sSeq
	}

	return version.FileMetadata{
		FileNum:        fileNum,
		FileSize:       fileSize,
		SmallestKey:    encSmall,
		LargestKey:     encLarge,
		SmallestSeqNum: sSeq,
		LargestSeqNum:  lSeq,
	}
}

// Test A — No compaction: Version is below all thresholds. Expected: no plan
func TestCompaction_TestA_NoCompaction(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	var levels [version.NumLevels][]version.FileMetadata
	// L0 has 3 files (below threshold 4)
	levels[0] = []version.FileMetadata{
		makeTestFile(t, 1, 1024, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut),
		makeTestFile(t, 2, 1024, "c", 8, binary.OpTypePut, "d", 7, binary.OpTypePut),
		makeTestFile(t, 3, 1024, "e", 6, binary.OpTypePut, "f", 5, binary.OpTypePut),
	}
	// L1 has 1 MB (below threshold 10 MB)
	levels[1] = []version.FileMetadata{
		makeTestFile(t, 4, 1024*1024, "a", 4, binary.OpTypePut, "m", 3, binary.OpTypePut),
	}

	v := version.NewVersion(levels)
	defer v.Unref()

	plan, err := planner.PickCompaction(v)
	if err != nil {
		t.Fatalf("unexpected error from PickCompaction: %v", err)
	}
	if plan != nil {
		t.Fatalf("expected nil plan when below threshold, got: %+v", plan)
	}
}

// Test B — L0 trigger: Construct enough L0 pressure (>= 4 files). Expected: L0 -> L1 plan
func TestCompaction_TestB_L0Trigger(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	var levels [version.NumLevels][]version.FileMetadata
	// L0 has 4 files (meets trigger 4)
	levels[0] = []version.FileMetadata{
		makeTestFile(t, 1, 1000, "a", 10, binary.OpTypePut, "c", 9, binary.OpTypePut),
		makeTestFile(t, 2, 2000, "b", 8, binary.OpTypePut, "d", 7, binary.OpTypePut),
		makeTestFile(t, 3, 3000, "e", 6, binary.OpTypePut, "g", 5, binary.OpTypePut),
		makeTestFile(t, 4, 4000, "x", 4, binary.OpTypePut, "z", 3, binary.OpTypePut),
	}
	// L1 has non-overlapping files
	levels[1] = []version.FileMetadata{
		makeTestFile(t, 10, 5000, "a", 2, binary.OpTypePut, "b", 1, binary.OpTypePut),
		makeTestFile(t, 11, 5000, "c", 2, binary.OpTypePut, "d", 1, binary.OpTypePut),
		makeTestFile(t, 12, 5000, "m", 2, binary.OpTypePut, "n", 1, binary.OpTypePut),
	}

	v := version.NewVersion(levels)
	defer v.Unref()

	plan, err := planner.PickCompaction(v)
	if err != nil {
		t.Fatalf("PickCompaction failed: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan when L0 meets trigger")
	}

	if plan.SourceLevel() != 0 {
		t.Errorf("expected SourceLevel 0, got %d", plan.SourceLevel())
	}
	if plan.TargetLevel() != 1 {
		t.Errorf("expected TargetLevel 1, got %d", plan.TargetLevel())
	}

	// Verify plan is valid against version
	if err := plan.ValidateAgainstVersion(v); err != nil {
		t.Fatalf("plan failed validation against Version: %v", err)
	}
}

// Test C — L0 overlap closure:
// A = [a, d], B = [c, f], C = [e, h], and disjoint D = [x, z]
// Ensure closure selects A, B, and C, and excludes D.
func TestCompaction_TestC_L0OverlapClosure(t *testing.T) {
	policy := DefaultCompactionPolicy()
	policy.L0TriggerCount = 4
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	fileA := makeTestFile(t, 1, 100, "a", 10, binary.OpTypePut, "d", 9, binary.OpTypePut)
	fileB := makeTestFile(t, 2, 200, "c", 8, binary.OpTypePut, "f", 7, binary.OpTypePut)
	fileC := makeTestFile(t, 3, 300, "e", 6, binary.OpTypePut, "h", 5, binary.OpTypePut)
	fileD := makeTestFile(t, 4, 400, "x", 4, binary.OpTypePut, "z", 3, binary.OpTypePut)

	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{fileA, fileB, fileC, fileD}

	v := version.NewVersion(levels)
	defer v.Unref()

	// Seed with fileA
	plan, err := planner.PlanCompaction(v, 0, []version.FileMetadata{fileA})
	if err != nil {
		t.Fatalf("PlanCompaction failed: %v", err)
	}

	srcFiles := plan.SourceFiles()
	if len(srcFiles) != 3 {
		t.Fatalf("expected 3 source files under overlap closure (A, B, C), got %d", len(srcFiles))
	}
	expectedFileNums := []uint64{1, 2, 3}
	for i, f := range srcFiles {
		if f.FileNum != expectedFileNums[i] {
			t.Errorf("srcFiles[%d] FileNum mismatch: got %d, want %d", i, f.FileNum, expectedFileNums[i])
		}
	}

	// Verify user key bounds of selected source range: min="a", max="h"
	if string(plan.SmallestUserKey()) != "a" {
		t.Errorf("expected SmallestUserKey 'a', got %q", string(plan.SmallestUserKey()))
	}
	if string(plan.LargestUserKey()) != "h" {
		t.Errorf("expected LargestUserKey 'h', got %q", string(plan.LargestUserKey()))
	}

	if err := plan.ValidateAgainstVersion(v); err != nil {
		t.Fatalf("plan failed ValidateAgainstVersion: %v", err)
	}
}

// Test D — L1 target overlap:
// Source range: [b, g]
// Target files: [a, c], [d, f], [h, j]
// Expected: first two selected, third excluded.
func TestCompaction_TestD_L1TargetOverlap(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	srcFile := makeTestFile(t, 1, 1000, "b", 10, binary.OpTypePut, "g", 9, binary.OpTypePut)

	tgt1 := makeTestFile(t, 10, 500, "a", 5, binary.OpTypePut, "c", 4, binary.OpTypePut) // overlaps [b, g]
	tgt2 := makeTestFile(t, 11, 500, "d", 5, binary.OpTypePut, "f", 4, binary.OpTypePut) // overlaps [b, g]
	tgt3 := makeTestFile(t, 12, 500, "h", 5, binary.OpTypePut, "j", 4, binary.OpTypePut) // strictly after "g"

	var levels [version.NumLevels][]version.FileMetadata
	levels[1] = []version.FileMetadata{srcFile}
	levels[2] = []version.FileMetadata{tgt1, tgt2, tgt3}

	v := version.NewVersion(levels)
	defer v.Unref()

	plan, err := planner.PlanCompaction(v, 1, []version.FileMetadata{srcFile})
	if err != nil {
		t.Fatalf("PlanCompaction failed: %v", err)
	}

	tgtFiles := plan.TargetFiles()
	if len(tgtFiles) != 2 {
		t.Fatalf("expected 2 target files, got %d", len(tgtFiles))
	}
	if tgtFiles[0].FileNum != 10 || tgtFiles[1].FileNum != 11 {
		t.Errorf("target files mismatch: got [%d, %d], want [10, 11]", tgtFiles[0].FileNum, tgtFiles[1].FileNum)
	}

	if err := plan.ValidateAgainstVersion(v); err != nil {
		t.Fatalf("plan validation failed: %v", err)
	}
}

// Test E — Boundary-touching ranges:
// Explicitly test ranges that meet at the boundary.
// [a, b] and [b, c] share key "b", therefore they overlap.
// [a, b] and [c, d] are disjoint, therefore they do not overlap.
func TestCompaction_TestE_BoundaryTouchingRanges(t *testing.T) {
	file1 := makeTestFile(t, 1, 100, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut)
	file2 := makeTestFile(t, 2, 100, "b", 8, binary.OpTypePut, "c", 7, binary.OpTypePut)
	file3 := makeTestFile(t, 3, 100, "c", 6, binary.OpTypePut, "d", 5, binary.OpTypePut)

	// file1 and file2 share boundary "b" -> must overlap
	overlaps12, err := FilesOverlap(file1, file2)
	if err != nil {
		t.Fatalf("FilesOverlap failed: %v", err)
	}
	if !overlaps12 {
		t.Errorf("expected file1 [a, b] and file2 [b, c] to overlap on boundary 'b'")
	}

	// file2 and file3 share boundary "c" -> must overlap
	overlaps23, err := FilesOverlap(file2, file3)
	if err != nil {
		t.Fatalf("FilesOverlap failed: %v", err)
	}
	if !overlaps23 {
		t.Errorf("expected file2 [b, c] and file3 [c, d] to overlap on boundary 'c'")
	}

	// file1 [a, b] and file3 [c, d] are disjoint -> must not overlap
	overlaps13, err := FilesOverlap(file1, file3)
	if err != nil {
		t.Fatalf("FilesOverlap failed: %v", err)
	}
	if overlaps13 {
		t.Errorf("expected file1 [a, b] and file3 [c, d] NOT to overlap")
	}
}

// Test F — Duplicate selection:
// Cause one target file to be discovered through multiple planning paths.
// Expected: appears exactly once in the plan.
func TestCompaction_TestF_DuplicateSelection(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	// Two source files in L0:
	// srcA = [b, d]
	// srcB = [c, e] (overlaps srcA)
	srcA := makeTestFile(t, 1, 100, "b", 10, binary.OpTypePut, "d", 9, binary.OpTypePut)
	srcB := makeTestFile(t, 2, 100, "c", 8, binary.OpTypePut, "e", 7, binary.OpTypePut)

	// Target file tgt in L1 covers [a, f], which overlaps BOTH srcA and srcB
	tgt := makeTestFile(t, 10, 500, "a", 5, binary.OpTypePut, "f", 4, binary.OpTypePut)

	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{srcA, srcB}
	levels[1] = []version.FileMetadata{tgt}

	v := version.NewVersion(levels)
	defer v.Unref()

	// Plan starting with srcA
	plan, err := planner.PlanCompaction(v, 0, []version.FileMetadata{srcA})
	if err != nil {
		t.Fatalf("PlanCompaction failed: %v", err)
	}

	// Target files must contain tgt exactly once
	tgtFiles := plan.TargetFiles()
	if len(tgtFiles) != 1 {
		t.Fatalf("expected exactly 1 target file, got %d", len(tgtFiles))
	}
	if tgtFiles[0].FileNum != 10 {
		t.Errorf("expected target file 10, got %d", tgtFiles[0].FileNum)
	}

	if err := plan.ValidateAgainstVersion(v); err != nil {
		t.Fatalf("plan validation failed: %v", err)
	}
}

// Test G — Determinism:
// Run the same Version and policy repeatedly (100 times).
// Expected: byte-for-byte / structurally identical plan every time.
func TestCompaction_TestG_Determinism(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{
		makeTestFile(t, 1, 1000, "a", 10, binary.OpTypePut, "c", 9, binary.OpTypePut),
		makeTestFile(t, 2, 2000, "b", 8, binary.OpTypePut, "d", 7, binary.OpTypePut),
		makeTestFile(t, 3, 3000, "c", 6, binary.OpTypePut, "e", 5, binary.OpTypePut),
		makeTestFile(t, 4, 4000, "x", 4, binary.OpTypePut, "z", 3, binary.OpTypePut),
	}
	levels[1] = []version.FileMetadata{
		makeTestFile(t, 10, 5000, "b", 2, binary.OpTypePut, "d", 1, binary.OpTypePut),
	}

	v := version.NewVersion(levels)
	defer v.Unref()

	var baselinePlan *CompactionPlan
	for i := 0; i < 100; i++ {
		plan, err := planner.PickCompaction(v)
		if err != nil {
			t.Fatalf("iteration %d: PickCompaction failed: %v", i, err)
		}
		if plan == nil {
			t.Fatalf("iteration %d: expected non-nil plan", i)
		}

		if i == 0 {
			baselinePlan = plan
			continue
		}

		// Assert structural equality
		if plan.SourceLevel() != baselinePlan.SourceLevel() {
			t.Fatalf("iteration %d: SourceLevel mismatch", i)
		}
		if plan.TargetLevel() != baselinePlan.TargetLevel() {
			t.Fatalf("iteration %d: TargetLevel mismatch", i)
		}
		if plan.TotalInputBytes() != baselinePlan.TotalInputBytes() {
			t.Fatalf("iteration %d: TotalInputBytes mismatch", i)
		}
		if !plan.SmallestKey().Equal(baselinePlan.SmallestKey()) {
			t.Fatalf("iteration %d: SmallestKey mismatch", i)
		}
		if !plan.LargestKey().Equal(baselinePlan.LargestKey()) {
			t.Fatalf("iteration %d: LargestKey mismatch", i)
		}
		if !bytes.Equal(plan.SmallestUserKey(), baselinePlan.SmallestUserKey()) {
			t.Fatalf("iteration %d: SmallestUserKey mismatch", i)
		}
		if !bytes.Equal(plan.LargestUserKey(), baselinePlan.LargestUserKey()) {
			t.Fatalf("iteration %d: LargestUserKey mismatch", i)
		}

		src1 := plan.SourceFiles()
		src2 := baselinePlan.SourceFiles()
		if len(src1) != len(src2) {
			t.Fatalf("iteration %d: SourceFiles count mismatch", i)
		}
		for j := range src1 {
			if !src1[j].Equal(src2[j]) {
				t.Fatalf("iteration %d: SourceFiles[%d] mismatch", i, j)
			}
		}

		tgt1 := plan.TargetFiles()
		tgt2 := baselinePlan.TargetFiles()
		if len(tgt1) != len(tgt2) {
			t.Fatalf("iteration %d: TargetFiles count mismatch", i)
		}
		for j := range tgt1 {
			if !tgt1[j].Equal(tgt2[j]) {
				t.Fatalf("iteration %d: TargetFiles[%d] mismatch", i, j)
			}
		}
	}
}

// Test H — Map-order independence:
// Construct logically identical Versions from different insertion orders.
// Expected: identical plan.
func TestCompaction_TestH_MapOrderIndependence(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	f1 := makeTestFile(t, 1, 1000, "a", 10, binary.OpTypePut, "c", 9, binary.OpTypePut)
	f2 := makeTestFile(t, 2, 2000, "b", 8, binary.OpTypePut, "d", 7, binary.OpTypePut)
	f3 := makeTestFile(t, 3, 3000, "e", 6, binary.OpTypePut, "g", 5, binary.OpTypePut)
	f4 := makeTestFile(t, 4, 4000, "h", 4, binary.OpTypePut, "j", 3, binary.OpTypePut)

	// Order 1: f1, f2, f3, f4
	var levels1 [version.NumLevels][]version.FileMetadata
	levels1[0] = []version.FileMetadata{f1, f2, f3, f4}
	v1 := version.NewVersion(levels1)
	defer v1.Unref()

	// Order 2: f4, f3, f2, f1 in input slice before sort
	var levels2 [version.NumLevels][]version.FileMetadata
	levels2[0] = []version.FileMetadata{f4, f3, f2, f1}
	v2 := version.NewVersion(levels2)
	defer v2.Unref()

	// Notice that for L0, PickCompaction evaluates files.
	// Both v1 and v2 should produce identical plans.
	plan1, err := planner.PickCompaction(v1)
	if err != nil {
		t.Fatalf("PickCompaction v1 failed: %v", err)
	}
	plan2, err := planner.PickCompaction(v2)
	if err != nil {
		t.Fatalf("PickCompaction v2 failed: %v", err)
	}

	if plan1.TotalInputBytes() != plan2.TotalInputBytes() {
		t.Errorf("TotalInputBytes mismatch: %d vs %d", plan1.TotalInputBytes(), plan2.TotalInputBytes())
	}
	if !bytes.Equal(plan1.SmallestUserKey(), plan2.SmallestUserKey()) {
		t.Errorf("SmallestUserKey mismatch: %q vs %q", plan1.SmallestUserKey(), plan2.SmallestUserKey())
	}
	if !bytes.Equal(plan1.LargestUserKey(), plan2.LargestUserKey()) {
		t.Errorf("LargestUserKey mismatch: %q vs %q", plan1.LargestUserKey(), plan2.LargestUserKey())
	}
}

// Test I — Invalid Version metadata:
// Inject invalid metadata (zero FileNum, zero FileSize, SmallestKey > LargestKey).
// Expected: fail closed with error.
func TestCompaction_TestI_InvalidMetadata(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	// 1. Zero FileNum
	badFile1 := makeTestFile(t, 1, 100, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut)
	badFile1.FileNum = 0
	_, _, err = ExtractUserKeyRange(badFile1)
	if !stdErrors.Is(err, errors.ErrInvalidFileNum) {
		t.Errorf("expected ErrInvalidFileNum for zero FileNum, got: %v", err)
	}

	// 2. Zero FileSize
	badFile2 := makeTestFile(t, 2, 0, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut)
	_, _, err = ExtractUserKeyRange(badFile2)
	if !stdErrors.Is(err, errors.ErrInvalidFileSize) {
		t.Errorf("expected ErrInvalidFileSize for zero FileSize, got: %v", err)
	}

	// 3. SmallestKey > LargestKey
	badFile3 := makeTestFile(t, 3, 100, "z", 10, binary.OpTypePut, "a", 9, binary.OpTypePut)
	_, _, err = ExtractUserKeyRange(badFile3)
	if !stdErrors.Is(err, errors.ErrInvalidKeyRange) {
		t.Errorf("expected ErrInvalidKeyRange when SmallestKey > LargestKey, got: %v", err)
	}

	// 4. Corrupt internal key bytes
	badFile4 := makeTestFile(t, 4, 100, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut)
	badFile4.SmallestKey = []byte{0x01, 0x02} // truncated
	_, _, err = ExtractUserKeyRange(badFile4)
	if err == nil {
		t.Errorf("expected error for corrupted SmallestKey, got nil")
	}

	// Planner fails closed on invalid metadata
	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{badFile1, badFile1, badFile1, badFile1}
	v := version.NewVersion(levels)
	defer v.Unref()

	_, err = planner.PickCompaction(v)
	if err == nil {
		t.Fatal("expected PickCompaction to fail closed on corrupt file metadata")
	}
}

// Test J — Invalid level:
// Expected: validation failure.
func TestCompaction_TestJ_InvalidLevel(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	var levels [version.NumLevels][]version.FileMetadata
	v := version.NewVersion(levels)
	defer v.Unref()

	dummySeed := []version.FileMetadata{
		makeTestFile(t, 1, 100, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut),
	}

	// Negative level
	_, err = planner.PlanCompaction(v, -1, dummySeed)
	if !stdErrors.Is(err, errors.ErrInvalidLevel) {
		t.Errorf("expected ErrInvalidLevel for negative level, got: %v", err)
	}

	// Level 6 as source level (target level would be 7, exceeding NumLevels-1)
	_, err = planner.PlanCompaction(v, 6, dummySeed)
	if !stdErrors.Is(err, errors.ErrInvalidLevel) {
		t.Errorf("expected ErrInvalidLevel for source level 6, got: %v", err)
	}

	// Level 7 as source level
	_, err = planner.PlanCompaction(v, 7, dummySeed)
	if !stdErrors.Is(err, errors.ErrInvalidLevel) {
		t.Errorf("expected ErrInvalidLevel for source level 7, got: %v", err)
	}
}

// Test K — Overflow in byte estimate:
// Use metadata whose summed FileSize would overflow the accumulator.
// Expected: error (ErrByteCountOverflow), not wraparound.
func TestCompaction_TestK_ByteCountOverflow(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	// Construct two files whose sizes sum > math.MaxUint64
	hugeFile1 := makeTestFile(t, 1, math.MaxUint64-10, "a", 10, binary.OpTypePut, "d", 9, binary.OpTypePut)
	hugeFile2 := makeTestFile(t, 2, 20, "c", 8, binary.OpTypePut, "f", 7, binary.OpTypePut)

	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{hugeFile1, hugeFile2, hugeFile1, hugeFile2}

	v := version.NewVersion(levels)
	defer v.Unref()

	_, err = planner.PlanCompaction(v, 0, []version.FileMetadata{hugeFile1})
	if !stdErrors.Is(err, errors.ErrByteCountOverflow) {
		t.Fatalf("expected ErrByteCountOverflow, got: %v", err)
	}
}

// Test L — Empty target level:
// Valid compaction with: source inputs present, target inputs absent.
// Expected: valid plan.
func TestCompaction_TestL_EmptyTargetLevel(t *testing.T) {
	policy := DefaultCompactionPolicy()
	policy.L0TriggerCount = 4
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	// L0 has 4 files, L1 is completely empty
	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{
		makeTestFile(t, 1, 1000, "a", 10, binary.OpTypePut, "c", 9, binary.OpTypePut),
		makeTestFile(t, 2, 2000, "b", 8, binary.OpTypePut, "d", 7, binary.OpTypePut),
		makeTestFile(t, 3, 3000, "e", 6, binary.OpTypePut, "g", 5, binary.OpTypePut),
		makeTestFile(t, 4, 4000, "h", 4, binary.OpTypePut, "j", 3, binary.OpTypePut),
	}
	// levels[1] is nil (empty)

	v := version.NewVersion(levels)
	defer v.Unref()

	plan, err := planner.PickCompaction(v)
	if err != nil {
		t.Fatalf("PickCompaction failed: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}

	if len(plan.SourceFiles()) == 0 {
		t.Fatal("expected non-empty source files")
	}
	if len(plan.TargetFiles()) != 0 {
		t.Fatalf("expected empty target files, got %d", len(plan.TargetFiles()))
	}
	if plan.TargetInputBytes() != 0 {
		t.Errorf("expected TargetInputBytes 0, got %d", plan.TargetInputBytes())
	}
	if plan.TotalInputBytes() != plan.SourceInputBytes() {
		t.Errorf("expected TotalInputBytes == SourceInputBytes (%d vs %d)", plan.TotalInputBytes(), plan.SourceInputBytes())
	}

	if err := plan.ValidateAgainstVersion(v); err != nil {
		t.Fatalf("plan validation failed: %v", err)
	}
}

// Test M — Multiple levels eligible:
// Selection must be deterministic, documented, and consistent with policy:
// 1. L0 prioritized over deeper levels if score >= 1.0.
// 2. Highest score selected across L1..L5.
// 3. Lower level index wins ties.
func TestCompaction_TestM_MultipleLevelsEligible(t *testing.T) {
	policy := DefaultCompactionPolicy()
	policy.L0TriggerCount = 4
	policy.L1TargetBytes = 1000
	policy.LevelMultiplier = 10.0

	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	// Case 1: L0 has score 1.0 (4 files), L1 has score 2.0 (2000 bytes vs 1000 target).
	// L0 must be prioritized!
	var levelsCase1 [version.NumLevels][]version.FileMetadata
	levelsCase1[0] = []version.FileMetadata{
		makeTestFile(t, 1, 100, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut),
		makeTestFile(t, 2, 100, "c", 8, binary.OpTypePut, "d", 7, binary.OpTypePut),
		makeTestFile(t, 3, 100, "e", 6, binary.OpTypePut, "f", 5, binary.OpTypePut),
		makeTestFile(t, 4, 100, "g", 4, binary.OpTypePut, "h", 3, binary.OpTypePut),
	}
	levelsCase1[1] = []version.FileMetadata{
		makeTestFile(t, 10, 2000, "a", 2, binary.OpTypePut, "z", 1, binary.OpTypePut),
	}

	v1 := version.NewVersion(levelsCase1)
	defer v1.Unref()

	lvl1, _, should1, err := planner.PickCompactionLevel(v1)
	if err != nil || !should1 {
		t.Fatalf("PickCompactionLevel failed: %v, should: %v", err, should1)
	}
	if lvl1 != 0 {
		t.Errorf("expected L0 priority over L1 even with lower score, got level %d", lvl1)
	}

	// Case 2: L0 below threshold (3 files). L1 has score 1.5 (1500B / 1000B), L2 has score 2.5 (25000B / 10000B).
	// L2 has higher score -> L2 must be selected!
	var levelsCase2 [version.NumLevels][]version.FileMetadata
	levelsCase2[0] = []version.FileMetadata{
		makeTestFile(t, 1, 100, "a", 10, binary.OpTypePut, "b", 9, binary.OpTypePut),
		makeTestFile(t, 2, 100, "c", 8, binary.OpTypePut, "d", 7, binary.OpTypePut),
		makeTestFile(t, 3, 100, "e", 6, binary.OpTypePut, "f", 5, binary.OpTypePut),
	}
	levelsCase2[1] = []version.FileMetadata{
		makeTestFile(t, 10, 1500, "a", 2, binary.OpTypePut, "z", 1, binary.OpTypePut),
	}
	levelsCase2[2] = []version.FileMetadata{
		makeTestFile(t, 20, 25000, "a", 2, binary.OpTypePut, "z", 1, binary.OpTypePut),
	}

	v2 := version.NewVersion(levelsCase2)
	defer v2.Unref()

	lvl2, score2, should2, err := planner.PickCompactionLevel(v2)
	if err != nil || !should2 {
		t.Fatalf("PickCompactionLevel failed: %v, should: %v", err, should2)
	}
	if lvl2 != 2 {
		t.Errorf("expected L2 with score 2.5 to be selected over L1 with score 1.5, got level %d (score %f)", lvl2, score2)
	}

	// Case 3: Tied score between L1 and L2 (both score 2.0).
	// Lower level index L1 must be selected.
	var levelsCase3 [version.NumLevels][]version.FileMetadata
	levelsCase3[1] = []version.FileMetadata{
		makeTestFile(t, 10, 2000, "a", 2, binary.OpTypePut, "z", 1, binary.OpTypePut), // 2000 / 1000 = 2.0
	}
	levelsCase3[2] = []version.FileMetadata{
		makeTestFile(t, 20, 20000, "a", 2, binary.OpTypePut, "z", 1, binary.OpTypePut), // 20000 / 10000 = 2.0
	}

	v3 := version.NewVersion(levelsCase3)
	defer v3.Unref()

	lvl3, score3, should3, err := planner.PickCompactionLevel(v3)
	if err != nil || !should3 {
		t.Fatalf("PickCompactionLevel failed: %v, should: %v", err, should3)
	}
	if lvl3 != 1 {
		t.Errorf("expected tie-break to pick lower level L1, got level %d (score %f)", lvl3, score3)
	}
}

// Test Invariants: AllInputFiles, Defensive Cloning, TargetFile validation
func TestCompaction_InvariantsAndCloning(t *testing.T) {
	policy := DefaultCompactionPolicy()
	planner, err := NewPlanner(policy)
	if err != nil {
		t.Fatalf("NewPlanner failed: %v", err)
	}

	f1 := makeTestFile(t, 1, 500, "b", 10, binary.OpTypePut, "d", 9, binary.OpTypePut)
	f2 := makeTestFile(t, 10, 600, "c", 5, binary.OpTypePut, "e", 4, binary.OpTypePut)

	var levels [version.NumLevels][]version.FileMetadata
	levels[1] = []version.FileMetadata{f1}
	levels[2] = []version.FileMetadata{f2}

	v := version.NewVersion(levels)
	defer v.Unref()

	plan, err := planner.PlanCompaction(v, 1, []version.FileMetadata{f1})
	if err != nil {
		t.Fatalf("PlanCompaction failed: %v", err)
	}

	// 1. Invariant: Every selected input physically belongs to intended level
	for _, f := range plan.SourceFiles() {
		if f.FileNum != 1 {
			t.Errorf("unexpected source file: %d", f.FileNum)
		}
	}
	for _, f := range plan.TargetFiles() {
		if f.FileNum != 10 {
			t.Errorf("unexpected target file: %d", f.FileNum)
		}
	}

	// 2. Invariant: AllInputFiles returns all inputs
	all := plan.AllInputFiles()
	if len(all) != 2 {
		t.Fatalf("expected 2 total input files, got %d", len(all))
	}
	if all[0].FileNum != 1 || all[1].FileNum != 10 {
		t.Errorf("AllInputFiles mismatch: got [%d, %d]", all[0].FileNum, all[1].FileNum)
	}

	// 3. Invariant: Mutating returned slices does not mutate plan
	all[0].FileNum = 999
	if plan.SourceFiles()[0].FileNum != 1 {
		t.Errorf("plan internal state was mutated via AllInputFiles slice")
	}

	// 4. Invariant: Mutating returned user key slice does not mutate plan
	userKey := plan.SmallestUserKey()
	userKey[0] = 'z'
	if plan.SmallestUserKey()[0] == 'z' {
		t.Errorf("plan SmallestUserKey was mutated")
	}
}

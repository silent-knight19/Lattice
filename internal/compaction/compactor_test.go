package compaction

import (
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// Helper to construct a FileMetadata with encoded InternalKey bounds
func makeTestFileMeta(fileNum uint64, minUserKey, maxUserKey string, minSeq, maxSeq uint64) version.FileMetadata {
	smallIK := binary.InternalKey{
		UserKey: []byte(minUserKey),
		SeqNum:  binary.SeqNum(maxSeq), // In canonical order, higher seq sorts earlier
		OpType:  binary.OpTypePut,
	}
	largeIK := binary.InternalKey{
		UserKey: []byte(maxUserKey),
		SeqNum:  binary.SeqNum(minSeq), // Lower seq sorts later
		OpType:  binary.OpTypePut,
	}

	return version.FileMetadata{
		FileNum:        fileNum,
		FileSize:       4096,
		SmallestKey:    binary.EncodeInternalKey(smallIK),
		LargestKey:     binary.EncodeInternalKey(largeIK),
		SmallestSeqNum: minSeq,
		LargestSeqNum:  maxSeq,
	}
}

// Helper to write a real SSTable file and return reader
func buildTestSSTableFile(t *testing.T, dir string, fileNum uint64, recs []testRecord) (*sstable.TableReader, string) {
	t.Helper()
	filename := fmt.Sprintf("%06d.sst", fileNum)
	path := filepath.Join(dir, filename)

	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 512

	w, err := sstable.NewTableWriter(path, opts)
	if err != nil {
		t.Fatalf("failed to create TableWriter for %s: %v", filename, err)
	}

	for _, r := range recs {
		if err := w.Add(r.key, r.value); err != nil {
			t.Fatalf("failed to add record to %s: %v", filename, err)
		}
	}

	if _, err := w.Finish(); err != nil {
		t.Fatalf("failed to finish TableWriter for %s: %v", filename, err)
	}

	reader, err := sstable.NewTableReader(path)
	if err != nil {
		t.Fatalf("failed to open TableReader for %s: %v", filename, err)
	}

	return reader, path
}

// Test A: No deeper versions
func TestCompactor_A_NoDeeperVersions(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	// L1 has a file
	levels[1] = []version.FileMetadata{
		makeTestFileMeta(101, "a", "z", 100, 200),
	}
	// L2..L6 are empty
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Target level = 1. Since L2..L6 are empty, tombstone can be dropped.
	if !c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == true when deeper levels have no files")
	}
}

// Test B: Older PUT in L2
func TestCompactor_B_OlderPutInL2(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	// L2 contains a file covering ["g", "m"]
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "g", "m", 50, 80),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Tombstone for "k" targeting level 1: L2 overlaps "k"
	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == false when L2 contains overlapping file")
	}
}

// Test C: Older PUT in L6 (bottom level)
func TestCompactor_C_OlderPutInL6(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	// L2..L5 empty, L6 contains file covering ["j", "p"]
	levels[6] = []version.FileMetadata{
		makeTestFileMeta(601, "j", "p", 10, 20),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Target level 1: L6 contains overlapping file
	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == false when L6 contains overlapping file")
	}
}

// Test D: Wrong key in deeper level (disjoint range)
func TestCompactor_D_WrongKeyDeeper(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	// L2 contains files covering ["a", "c"] and ["x", "z"]
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "a", "c", 10, 20),
		makeTestFileMeta(202, "x", "z", 10, 20),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Tombstone for "k": outside ["a", "c"] and ["x", "z"]
	if !c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == true when all deeper files are disjoint")
	}
}

// Test E: Same key across multiple deeper levels
func TestCompactor_E_SameKeyAcrossMultipleDeeperLevels(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{makeTestFileMeta(201, "g", "m", 50, 60)}
	levels[4] = []version.FileMetadata{makeTestFileMeta(401, "h", "l", 30, 40)}
	levels[6] = []version.FileMetadata{makeTestFileMeta(601, "i", "k", 10, 20)}

	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Target level 1: multiple deeper levels overlap "k"
	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == false")
	}

	// Target level 3: L4 and L6 still overlap "k"
	if c.CanDropTombstone([]byte("k"), 3) {
		t.Fatalf("expected CanDropTombstone == false for targetLevel 3")
	}

	// Target level 5: L6 still overlaps "k"
	if c.CanDropTombstone([]byte("k"), 5) {
		t.Fatalf("expected CanDropTombstone == false for targetLevel 5")
	}

	// Target level 6 (deepest level): no levels deeper than L6
	if !c.CanDropTombstone([]byte("k"), 6) {
		t.Fatalf("expected CanDropTombstone == true for targetLevel 6 (bottom level)")
	}
}

// Test F: Same key / different sequence
func TestCompactor_F_SameKeyDifferentSequence(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "k", "k", 50, 50),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == false when key exists in L2")
	}
}

// Test G: Deeper tombstone
func TestCompactor_G_DeeperTombstone(t *testing.T) {
	// Even if deeper record is a tombstone, it is an active record at a deeper level
	var levels [version.NumLevels][]version.FileMetadata
	levels[3] = []version.FileMetadata{
		makeTestFileMeta(301, "tomb", "tomb", 30, 30),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.CanDropTombstone([]byte("tomb"), 1) {
		t.Fatalf("expected CanDropTombstone == false when deeper level contains key")
	}
}

// Test H: Bottom-level target (L6)
func TestCompactor_H_BottomLevelTarget(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	// Even if L6 itself has a file covering "k", compaction targeting L6 is rewriting L6.
	// No deeper levels exist below L6 (NumLevels - 1).
	levels[6] = []version.FileMetadata{
		makeTestFileMeta(601, "a", "z", 10, 20),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.CanDropTombstone([]byte("k"), version.NumLevels-1) {
		t.Fatalf("expected CanDropTombstone == true when targeting deepest level")
	}
}

// Test I: Empty Version
func TestCompactor_I_EmptyVersion(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	for lvl := 0; lvl < version.NumLevels; lvl++ {
		if !c.CanDropTombstone([]byte("any-key"), lvl) {
			t.Fatalf("expected CanDropTombstone == true on empty version for level %d", lvl)
		}
	}
}

// Test J: Invalid target level
func TestCompactor_J_InvalidTargetLevel(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	invalidLevels := []int{-1, -10, version.NumLevels, version.NumLevels + 1, 100}
	for _, lvl := range invalidLevels {
		if c.CanDropTombstone([]byte("k"), lvl) {
			t.Fatalf("expected CanDropTombstone == false for invalid level %d", lvl)
		}
	}
}

// Test K: Invalid deeper metadata (SmallestKey > LargestKey)
func TestCompactor_K_InvalidDeeperMetadata(t *testing.T) {
	smallIK := binary.InternalKey{UserKey: []byte("z"), SeqNum: 10, OpType: binary.OpTypePut}
	largeIK := binary.InternalKey{UserKey: []byte("a"), SeqNum: 20, OpType: binary.OpTypePut}

	corruptMeta := version.FileMetadata{
		FileNum:        999,
		FileSize:       4096,
		SmallestKey:    binary.EncodeInternalKey(smallIK), // "z" > "a"
		LargestKey:     binary.EncodeInternalKey(largeIK),
		SmallestSeqNum: 10,
		LargestSeqNum:  20,
	}

	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{corruptMeta}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Corrupt metadata must fail closed (return false)
	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == false on corrupt metadata")
	}
}

// Test L: Nil or empty key
func TestCompactor_L_NilOrEmptyKey(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.CanDropTombstone(nil, 1) {
		t.Fatalf("expected CanDropTombstone == false for nil key")
	}
	if c.CanDropTombstone([]byte{}, 1) {
		t.Fatalf("expected CanDropTombstone == false for empty key")
	}

	oversized := make([]byte, binary.MaxKeyLen+1)
	if c.CanDropTombstone(oversized, 1) {
		t.Fatalf("expected CanDropTombstone == false for oversized key")
	}
}

// Test M: Metadata range false-positive case (Model A conservative vs Model C exact)
func TestCompactor_M_RangeFalsePositive(t *testing.T) {
	dir := t.TempDir()

	// File 201 covers range ["apple", "zebra"], but contains ONLY "apple" and "zebra" (lacks "mango")
	recs := []testRecord{
		{key: makeKey("apple", 100, binary.OpTypePut), value: []byte("val1")},
		{key: makeKey("zebra", 100, binary.OpTypePut), value: []byte("val2")},
	}
	reader, _ := buildTestSSTableFile(t, dir, 201, recs)
	defer func() { _ = reader.Close() }()

	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "apple", "zebra", 100, 100),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	// Part 1: Conservative mode (no TableOpener)
	// Range ["apple", "zebra"] covers "mango", so conservative mode must return false
	cCons, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = cCons.Close() }()

	if cCons.CanDropTombstone([]byte("mango"), 1) {
		t.Fatalf("conservative mode must return false for overlapping range")
	}

	// Part 2: Exact mode (with TableOpener)
	// Range covers "mango", but TableReader physical seek proves "mango" does NOT exist!
	// Therefore, exact mode returns true (tombstone can be dropped).
	opener := func(fileNum uint64) (*sstable.TableReader, error) {
		if fileNum == 201 {
			return reader, nil
		}
		return nil, os.ErrNotExist
	}

	cExact, err := NewCompactor(v, WithTableOpener(opener))
	if err != nil {
		t.Fatalf("NewCompactor with opener failed: %v", err)
	}
	defer func() { _ = cExact.Close() }()

	if !cExact.CanDropTombstone([]byte("mango"), 1) {
		t.Fatalf("exact mode must return true when candidate file physically lacks key")
	}

	// Part 3: Exact mode when key IS physically in the table ("apple")
	if cExact.CanDropTombstone([]byte("apple"), 1) {
		t.Fatalf("exact mode must return false when key physically exists in table")
	}
}

// Test N: Disjoint range
func TestCompactor_N_DisjointRange(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "a", "c", 10, 20),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	if !c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == true for disjoint file range")
	}
}

// Test O: Version immutability
func TestCompactor_O_VersionImmutability(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	file := makeTestFileMeta(201, "g", "m", 10, 20)
	levels[2] = []version.FileMetadata{file}

	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Mutate caller's original slice and buffer
	file.SmallestKey[0] = 'z'
	levels[2][0] = file

	// Compactor must continue operating against its defensively cloned immutable Version
	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("expected CanDropTombstone == false despite caller mutating original slice")
	}
}

// Test P: Version lifetime and refcount
func TestCompactor_P_VersionLifetimeAndRefcount(t *testing.T) {
	v := version.NewVersion([version.NumLevels][]version.FileMetadata{})
	initialRef := v.RefCount() // 1

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}

	// Refcount must be incremented by Compactor
	if v.RefCount() != initialRef+1 {
		t.Fatalf("expected refCount == %d, got %d", initialRef+1, v.RefCount())
	}

	// Close compactor
	if err := c.Close(); err != nil {
		t.Fatalf("c.Close() failed: %v", err)
	}

	// Refcount must be decremented
	if v.RefCount() != initialRef {
		t.Fatalf("expected refCount == %d after Close(), got %d", initialRef, v.RefCount())
	}

	// Calling CanDropTombstone after Close() returns false
	if c.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("CanDropTombstone after Close() must return false")
	}

	// Idempotent Close()
	if err := c.Close(); err != nil {
		t.Fatalf("repeated c.Close() failed: %v", err)
	}

	// Release creator's reference
	v.Unref()
	if v.RefCount() != 0 {
		t.Fatalf("expected refCount == 0, got %d", v.RefCount())
	}

	// Attempting to construct Compactor with dead Version must fail
	deadC, err := NewCompactor(v)
	if err == nil {
		_ = deadC.Close()
		t.Fatalf("expected error constructing Compactor with dead Version")
	}
}

// Test Q: Concurrent checks under race detector
func TestCompactor_Q_ConcurrentChecks(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "a", "m", 10, 20),
		makeTestFileMeta(202, "n", "z", 10, 20),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	c, err := NewCompactor(v)
	if err != nil {
		t.Fatalf("NewCompactor failed: %v", err)
	}
	defer func() { _ = c.Close() }()

	var wg sync.WaitGroup
	const numGoroutines = 16
	const numOps = 100

	keys := [][]byte{
		[]byte("apple"),
		[]byte("banana"),
		[]byte("mango"),
		[]byte("orange"),
		[]byte("zebra"),
	}

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < numOps; i++ {
				k := keys[(gid+i)%len(keys)]
				_ = c.CanDropTombstone(k, 1)
				_ = c.CanDropTombstone(k, 6) // bottom level
			}
		}(g)
	}
	wg.Wait()
}

// Test R: Stateless helper CanDropTombstoneInVersion
func TestCompactor_R_StatelessHelper(t *testing.T) {
	var levels [version.NumLevels][]version.FileMetadata
	levels[2] = []version.FileMetadata{
		makeTestFileMeta(201, "g", "m", 10, 20),
	}
	v := version.NewVersion(levels)
	defer v.Unref()

	if CanDropTombstoneInVersion(v, []byte("k"), 1) {
		t.Fatalf("expected false for overlapping key")
	}
	if !CanDropTombstoneInVersion(v, []byte("z"), 1) {
		t.Fatalf("expected true for non-overlapping key")
	}
	if !CanDropTombstoneInVersion(v, []byte("k"), 6) {
		t.Fatalf("expected true for bottom level")
	}
	if CanDropTombstoneInVersion(nil, []byte("k"), 1) {
		t.Fatalf("expected false for nil version")
	}
}

// Test S: Panic-freedom
func TestCompactor_S_PanicFreedom(t *testing.T) {
	var nilC *Compactor
	if nilC.CanDropTombstone([]byte("k"), 1) {
		t.Fatalf("nil compactor CanDropTombstone must return false")
	}
	if nilC.Version() != nil {
		t.Fatalf("nil compactor Version() must be nil")
	}
	if !stdErrors.Is(nilC.Close(), errors.ErrNilReceiver) {
		t.Fatalf("nil compactor Close() must return ErrNilReceiver")
	}

	_, err := NewCompactor(nil)
	if !stdErrors.Is(err, errors.ErrNilReceiver) {
		t.Fatalf("NewCompactor(nil) must return ErrNilReceiver")
	}
}

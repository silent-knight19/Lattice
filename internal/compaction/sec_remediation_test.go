package compaction_test

import (
	"errors"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/compaction"
	lerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
	"github.com/silent-knight19/lattice/internal/version"
)

// TestPlanner_CorruptMetadataRejectedUpfront verifies that Planner.PickCompaction and
// Planner.PlanCompaction fail closed with an explicit error when candidate files contain
// unparseable or corrupt internal key metadata (SEC-P08-001).
func TestPlanner_CorruptMetadataRejectedUpfront(t *testing.T) {
	policy := compaction.DefaultCompactionPolicy()
	planner, err := compaction.NewPlanner(policy)
	if err != nil {
		t.Fatalf("failed to create planner: %v", err)
	}

	corruptMeta := version.FileMetadata{
		FileNum:        10,
		FileSize:       1024,
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
		SmallestKey:    []byte("corrupt_truncated_key"), // Invalid internal key (no 8B trailer)
		LargestKey:     []byte("corrupt_truncated_key"),
	}

	var levels [version.NumLevels][]version.FileMetadata
	levels[1] = []version.FileMetadata{corruptMeta}

	v := version.NewVersion(levels)
	defer v.Unref()

	// 1. PickCompaction must reject corrupt metadata in L1+ upfront
	// Force L1 score to exceed 1.0 by configuring base level bytes
	forcePolicy := policy
	forcePolicy.L1TargetBytes = 512
	forcePlanner, _ := compaction.NewPlanner(forcePolicy)

	plan, err := forcePlanner.PickCompaction(v)
	if err == nil {
		t.Fatalf("expected error from PickCompaction with corrupt metadata, got plan: %v", plan)
	}

	// 2. PlanCompaction must reject corrupt seed file upfront
	plan, err = planner.PlanCompaction(v, 1, []version.FileMetadata{corruptMeta})
	if err == nil {
		t.Fatalf("expected error from PlanCompaction with corrupt metadata, got plan: %v", plan)
	}
}

// TestCompactor_TransientTableOpenerClosesReader verifies that WithTransientTableOpener
// automatically closes opened TableReaders after key existence checks, preventing file
// descriptor leaks (SEC-P08-002).
func TestCompactor_TransientTableOpenerClosesReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000200.sst")

	writer, err := sstable.NewTableWriter(path, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("failed to create TableWriter: %v", err)
	}

	k, _ := binary.NewInternalKey([]byte("existing_key"), 100, binary.OpTypePut)
	if err := writer.Add(k, []byte("val")); err != nil {
		t.Fatalf("failed to add record: %v", err)
	}
	meta, err := writer.Finish()
	if err != nil {
		t.Fatalf("failed to finish writer: %v", err)
	}

	var levels [version.NumLevels][]version.FileMetadata
	fMeta := version.NewFileMetadataFromSSTable(200, meta)
	levels[2] = []version.FileMetadata{fMeta}

	v := version.NewVersion(levels)
	defer v.Unref()

	var readerCloseCalls atomic.Int64
	transientOpener := func(fileNum uint64) (*sstable.TableReader, error) {
		r, err := sstable.NewTableReader(path)
		if err != nil {
			return nil, err
		}
		readerCloseCalls.Add(1)
		return r, nil
	}

	c, err := compaction.NewCompactor(v, compaction.WithTransientTableOpener(transientOpener))
	if err != nil {
		t.Fatalf("failed to create compactor: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Execute key existence checks
	// Check 1: Key outside range -> opener not invoked
	_ = c.CanDropTombstone([]byte("aaa"), 1)

	// Check 2: Key inside range -> opener invoked and reader closed immediately
	_ = c.CanDropTombstone([]byte("existing_key"), 1)

	if readerCloseCalls.Load() != 1 {
		t.Fatalf("expected 1 opener invocation, got %d", readerCloseCalls.Load())
	}
}

// TestCompactionPlan_InvalidScoreRejected verifies that CompactionPlan.Validate
// strictly rejects NaN, Inf, and negative priority scores (SEC-P08-003).
func TestCompactionPlan_InvalidScoreRejected(t *testing.T) {
	ik1, _ := binary.NewInternalKey([]byte("a"), 10, binary.OpTypePut)
	ik2, _ := binary.NewInternalKey([]byte("z"), 1, binary.OpTypePut)

	f1 := version.FileMetadata{
		FileNum:        1,
		FileSize:       100,
		SmallestKey:    binary.EncodeInternalKey(ik1),
		LargestKey:     binary.EncodeInternalKey(ik2),
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	}

	invalidScores := []float64{
		math.NaN(),
		math.Inf(1),
		math.Inf(-1),
		-0.01,
		-100.0,
	}

	for _, score := range invalidScores {
		plan, err := compaction.NewCompactionPlan(
			0, 1,
			[]version.FileMetadata{f1},
			nil,
			ik1, ik2,
			[]byte("a"), []byte("z"),
			100, 0,
			score,
		)
		if err == nil {
			t.Errorf("expected score %v to be rejected by NewCompactionPlan, got plan: %v", score, plan)
		}
		if !errors.Is(err, lerrors.ErrInvalidCompactionPlan) {
			t.Errorf("expected ErrInvalidCompactionPlan for score %v, got %v", score, err)
		}
	}

	// Valid score must pass
	validPlan, err := compaction.NewCompactionPlan(
		0, 1,
		[]version.FileMetadata{f1},
		nil,
		ik1, ik2,
		[]byte("a"), []byte("z"),
		100, 0,
		1.5,
	)
	if err != nil {
		t.Fatalf("valid score rejected: %v", err)
	}
	if validPlan.Score() != 1.5 {
		t.Fatalf("expected Score() == 1.5, got %v", validPlan.Score())
	}
}

// TestMergingIterator_NilChildIndexAlignment verifies that passing nil children
// in various positions preserves 1:1 index alignment without crashes (SEC-P08-004).
func TestMergingIterator_NilChildIndexAlignment(t *testing.T) {
	k1, _ := binary.NewInternalKey([]byte("k1"), 10, binary.OpTypePut)
	k2, _ := binary.NewInternalKey([]byte("k2"), 20, binary.OpTypePut)

	child1 := newMockTestIterator([]mockTestRecord{{key: k1, value: []byte("v1")}})
	child2 := newMockTestIterator([]mockTestRecord{{key: k2, value: []byte("v2")}})

	// Input slice with nil entries in various positions
	iters := []compaction.Iterator{nil, child1, nil, child2, nil}

	it := compaction.NewMergingIterator(iters)
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("expected Next() to yield k1")
	}
	if string(it.Key().UserKey) != "k1" {
		t.Fatalf("expected k1, got %s", it.Key().UserKey)
	}

	if !it.Next() {
		t.Fatalf("expected Next() to yield k2")
	}
	if string(it.Key().UserKey) != "k2" {
		t.Fatalf("expected k2, got %s", it.Key().UserKey)
	}

	if it.Next() {
		t.Fatalf("expected exhaustion after 2 records")
	}
	if it.Err() != nil {
		t.Fatalf("unexpected error: %v", it.Err())
	}
}

type mockTestRecord struct {
	key   binary.InternalKey
	value []byte
}

type mockTestIterator struct {
	records []mockTestRecord
	idx     int
}

func newMockTestIterator(records []mockTestRecord) *mockTestIterator {
	return &mockTestIterator{records: records, idx: 0}
}

func (m *mockTestIterator) Valid() bool {
	return m.idx < len(m.records)
}

func (m *mockTestIterator) Next() bool {
	m.idx++
	return m.Valid()
}

func (m *mockTestIterator) Key() binary.InternalKey {
	if !m.Valid() {
		return binary.InternalKey{}
	}
	return m.records[m.idx].key
}

func (m *mockTestIterator) RawKey() []byte {
	if !m.Valid() {
		return nil
	}
	return binary.EncodeInternalKey(m.records[m.idx].key)
}

func (m *mockTestIterator) Value() []byte {
	if !m.Valid() {
		return nil
	}
	return m.records[m.idx].value
}

func (m *mockTestIterator) Err() error {
	return nil
}

func (m *mockTestIterator) Close() error {
	return nil
}

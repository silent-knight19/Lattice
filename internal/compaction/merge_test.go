package compaction

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

type testRecord struct {
	key   binary.InternalKey
	value []byte
}

type mockIterator struct {
	records []testRecord
	pos     int
	err     error
	failAt  int // fail when Next() would advance to or past failAt
	closed  bool
}

func newMockIterator(records []testRecord) *mockIterator {
	m := &mockIterator{
		records: records,
		pos:     0,
		failAt:  -1,
	}
	return m
}

func (m *mockIterator) Valid() bool {
	if m == nil || m.closed || m.err != nil {
		return false
	}
	return m.pos >= 0 && m.pos < len(m.records)
}

func (m *mockIterator) Next() bool {
	if m == nil || m.closed || m.err != nil {
		return false
	}
	nextPos := m.pos + 1
	if m.failAt >= 0 && nextPos >= m.failAt {
		m.err = stdErrors.New("mock child corruption error")
		return false
	}
	m.pos = nextPos
	return m.pos < len(m.records)
}

func (m *mockIterator) Key() binary.InternalKey {
	if !m.Valid() {
		return binary.InternalKey{}
	}
	return m.records[m.pos].key.Clone()
}

func (m *mockIterator) Value() []byte {
	if !m.Valid() || m.records[m.pos].value == nil {
		return nil
	}
	out := make([]byte, len(m.records[m.pos].value))
	copy(out, m.records[m.pos].value)
	return out
}

func (m *mockIterator) Err() error {
	if m == nil {
		return errors.ErrNilReceiver
	}
	return m.err
}

func (m *mockIterator) Close() error {
	if m == nil {
		return errors.ErrNilReceiver
	}
	m.closed = true
	return nil
}

// Helper to make an InternalKey
func makeKey(userKey string, seq uint64, op binary.OpType) binary.InternalKey {
	return binary.InternalKey{
		UserKey: []byte(userKey),
		SeqNum:  binary.SeqNum(seq),
		OpType:  op,
	}
}

// Test A: Empty inputs
func TestMergingIterator_A_EmptyMerge(t *testing.T) {
	cases := []struct {
		name  string
		iters []Iterator
	}{
		{name: "nil slice", iters: nil},
		{name: "empty slice", iters: []Iterator{}},
		{name: "all nil elements", iters: []Iterator{nil, nil}},
		{name: "all exhausted elements", iters: []Iterator{
			newMockIterator(nil),
			newMockIterator(nil),
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := NewMergingIterator(tc.iters)
			if it.Valid() {
				t.Fatalf("expected Valid() == false on empty merge")
			}
			if it.Next() {
				t.Fatalf("expected Next() == false on empty merge")
			}
			if it.Valid() {
				t.Fatalf("expected Valid() == false after Next()")
			}
			if err := it.Err(); err != nil {
				t.Fatalf("expected nil Err(), got %v", err)
			}
			if it.Key().UserKey != nil {
				t.Fatalf("expected empty key, got %v", it.Key())
			}
			if it.Value() != nil {
				t.Fatalf("expected nil value, got %v", it.Value())
			}
			if it.RawKey() != nil {
				t.Fatalf("expected nil raw key, got %v", it.RawKey())
			}
			if err := it.Close(); err != nil {
				t.Fatalf("expected nil error on Close(), got %v", err)
			}
		})
	}
}

// Test B: Single iterator
func TestMergingIterator_B_SingleIterator(t *testing.T) {
	recs := []testRecord{
		{key: makeKey("apple", 100, binary.OpTypePut), value: []byte("val1")},
		{key: makeKey("banana", 90, binary.OpTypePut), value: []byte("val2")},
		{key: makeKey("cherry", 80, binary.OpTypePut), value: []byte("val3")},
	}
	child := newMockIterator(recs)
	it := NewMergingIterator([]Iterator{child})
	defer func() { _ = it.Close() }()

	var collected []testRecord
	for it.Next() {
		if !it.Valid() {
			t.Fatalf("expected Valid() == true when Next() returns true")
		}
		collected = append(collected, testRecord{
			key:   it.Key(),
			value: it.Value(),
		})
	}

	if it.Next() {
		t.Fatalf("Next() should return false after EOF")
	}
	if err := it.Err(); err != nil {
		t.Fatalf("unexpected Err(): %v", err)
	}
	if len(collected) != len(recs) {
		t.Fatalf("expected %d records, got %d", len(recs), len(collected))
	}
	for i, want := range recs {
		got := collected[i]
		if binary.CompareInternalKey(got.key, want.key) != 0 {
			t.Fatalf("record %d key mismatch: got %v, want %v", i, got.key, want.key)
		}
		if !bytes.Equal(got.value, want.value) {
			t.Fatalf("record %d value mismatch: got %s, want %s", i, got.value, want.value)
		}
	}
}

// Test C: Two iterators with disjoint keys
func TestMergingIterator_C_TwoIteratorsDisjoint(t *testing.T) {
	childA := newMockIterator([]testRecord{
		{key: makeKey("a", 10, binary.OpTypePut), value: []byte("va")},
		{key: makeKey("c", 10, binary.OpTypePut), value: []byte("vc")},
		{key: makeKey("e", 10, binary.OpTypePut), value: []byte("ve")},
	})
	childB := newMockIterator([]testRecord{
		{key: makeKey("b", 10, binary.OpTypePut), value: []byte("vb")},
		{key: makeKey("d", 10, binary.OpTypePut), value: []byte("vd")},
		{key: makeKey("f", 10, binary.OpTypePut), value: []byte("vf")},
	})

	it := NewMergingIterator([]Iterator{childA, childB})
	defer func() { _ = it.Close() }()

	expected := []string{"a", "b", "c", "d", "e", "f"}
	var gotKeys []string
	for it.Next() {
		gotKeys = append(gotKeys, string(it.Key().UserKey))
	}
	if err := it.Err(); err != nil {
		t.Fatalf("unexpected Err(): %v", err)
	}

	if len(gotKeys) != len(expected) {
		t.Fatalf("expected keys %v, got %v", expected, gotKeys)
	}
	for i := range expected {
		if gotKeys[i] != expected[i] {
			t.Fatalf("key %d: got %s, want %s", i, gotKeys[i], expected[i])
		}
	}
}

// Test D: Four overlapping iterators with revisions and tombstones
func TestMergingIterator_D_FourOverlappingIterators(t *testing.T) {
	// Child 0: a@100 PUT, b@80 PUT, d@70 PUT
	child0 := newMockIterator([]testRecord{
		{key: makeKey("a", 100, binary.OpTypePut), value: []byte("a100")},
		{key: makeKey("b", 80, binary.OpTypePut), value: []byte("b80")},
		{key: makeKey("d", 70, binary.OpTypePut), value: []byte("d70")},
	})
	// Child 1: a@90 PUT, c@95 PUT, d@60 PUT
	child1 := newMockIterator([]testRecord{
		{key: makeKey("a", 90, binary.OpTypePut), value: []byte("a90")},
		{key: makeKey("c", 95, binary.OpTypePut), value: []byte("c95")},
		{key: makeKey("d", 60, binary.OpTypePut), value: []byte("d60")},
	})
	// Child 2: b@75 PUT, c@90 DELETE, e@50 PUT
	child2 := newMockIterator([]testRecord{
		{key: makeKey("b", 75, binary.OpTypePut), value: []byte("b75")},
		{key: makeKey("c", 90, binary.OpTypeDelete), value: nil},
		{key: makeKey("e", 50, binary.OpTypePut), value: []byte("e50")},
	})
	// Child 3: c@110 DELETE, d@85 PUT
	child3 := newMockIterator([]testRecord{
		{key: makeKey("c", 110, binary.OpTypeDelete), value: nil},
		{key: makeKey("d", 85, binary.OpTypePut), value: []byte("d85")},
	})

	it := NewMergingIterator([]Iterator{child0, child1, child2, child3})
	defer func() { _ = it.Close() }()

	// Expected newest revisions:
	// "a": a@100 PUT ("a100") [a@90 suppressed]
	// "b": b@80 PUT ("b80")   [b@75 suppressed]
	// "c": c@110 DELETE (nil) [c@95, c@90 suppressed]
	// "d": d@85 PUT ("d85")   [d@70, d@60 suppressed]
	// "e": e@50 PUT ("e50")
	type expectedItem struct {
		userKey string
		seq     uint64
		op      binary.OpType
		value   string
	}
	expected := []expectedItem{
		{"a", 100, binary.OpTypePut, "a100"},
		{"b", 80, binary.OpTypePut, "b80"},
		{"c", 110, binary.OpTypeDelete, ""},
		{"d", 85, binary.OpTypePut, "d85"},
		{"e", 50, binary.OpTypePut, "e50"},
	}

	var count int
	for it.Next() {
		if count >= len(expected) {
			t.Fatalf("emitted more records than expected (%d)", count)
		}
		want := expected[count]
		k := it.Key()
		if string(k.UserKey) != want.userKey {
			t.Fatalf("record %d key mismatch: got %s, want %s", count, k.UserKey, want.userKey)
		}
		if uint64(k.SeqNum) != want.seq {
			t.Fatalf("record %d seq mismatch: got %d, want %d", count, k.SeqNum, want.seq)
		}
		if k.OpType != want.op {
			t.Fatalf("record %d op mismatch: got %v, want %v", count, k.OpType, want.op)
		}
		if want.op == binary.OpTypeDelete {
			if it.Value() != nil {
				t.Fatalf("record %d tombstone expected nil value, got %s", count, it.Value())
			}
		} else {
			if string(it.Value()) != want.value {
				t.Fatalf("record %d value mismatch: got %s, want %s", count, it.Value(), want.value)
			}
		}
		count++
	}

	if err := it.Err(); err != nil {
		t.Fatalf("unexpected Err(): %v", err)
	}
	if count != len(expected) {
		t.Fatalf("expected %d records, got %d", len(expected), count)
	}
}

// Test E: Sixteen iterators
func TestMergingIterator_E_SixteenIterators(t *testing.T) {
	const numIters = 16
	iters := make([]Iterator, numIters)
	for i := 0; i < numIters; i++ {
		recs := []testRecord{
			{key: makeKey(fmt.Sprintf("key-%02d", i), uint64(100+i), binary.OpTypePut), value: []byte(fmt.Sprintf("val-%d", i))},
			{key: makeKey(fmt.Sprintf("key-%02d", i+20), uint64(50+i), binary.OpTypePut), value: []byte(fmt.Sprintf("val2-%d", i))},
		}
		iters[i] = newMockIterator(recs)
	}

	it := NewMergingIterator(iters)
	defer func() { _ = it.Close() }()

	var prevKey string
	count := 0
	for it.Next() {
		currKey := string(it.Key().UserKey)
		if count > 0 && currKey <= prevKey {
			t.Fatalf("keys out of order or duplicate not suppressed: prev=%s, curr=%s", prevKey, currKey)
		}
		prevKey = currKey
		count++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("unexpected Err(): %v", err)
	}
	if count != numIters*2 {
		t.Fatalf("expected %d records, got %d", numIters*2, count)
	}
}

// Test G: Same user key across many iterators with multiple revisions
func TestMergingIterator_G_SameUserKeyAcrossAllChildren(t *testing.T) {
	childA := newMockIterator([]testRecord{
		{key: makeKey("k", 100, binary.OpTypePut), value: []byte("x100")},
		{key: makeKey("k", 70, binary.OpTypePut), value: []byte("x70")},
	})
	childB := newMockIterator([]testRecord{
		{key: makeKey("k", 90, binary.OpTypePut), value: []byte("x90")},
	})
	childC := newMockIterator([]testRecord{
		{key: makeKey("k", 80, binary.OpTypePut), value: []byte("x80")},
	})
	childD := newMockIterator([]testRecord{
		{key: makeKey("k", 60, binary.OpTypeDelete), value: nil},
	})

	it := NewMergingIterator([]Iterator{childA, childB, childC, childD})
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("expected Next() == true for winning record")
	}
	if string(it.Key().UserKey) != "k" || it.Key().SeqNum != 100 {
		t.Fatalf("expected k@100, got %v", it.Key())
	}
	if string(it.Value()) != "x100" {
		t.Fatalf("expected value x100, got %s", it.Value())
	}

	// All other revisions must be suppressed
	if it.Next() {
		t.Fatalf("expected no more records, got %v", it.Key())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("unexpected Err(): %v", err)
	}
}

// Test H: Same key, same sequence number across children (stable tie-breaking)
func TestMergingIterator_H_SameKeySameSeqTieBreaking(t *testing.T) {
	child0 := newMockIterator([]testRecord{
		{key: makeKey("dup", 100, binary.OpTypePut), value: []byte("child-0-val")},
	})
	child1 := newMockIterator([]testRecord{
		{key: makeKey("dup", 100, binary.OpTypePut), value: []byte("child-1-val")},
	})

	// Child 0 has index 0, Child 1 has index 1. Child 0 must win tie-break.
	it := NewMergingIterator([]Iterator{child0, child1})
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("expected Next() == true")
	}
	if string(it.Value()) != "child-0-val" {
		t.Fatalf("expected child-0-val to win tie-break, got %s", it.Value())
	}
	if it.Next() {
		t.Fatalf("expected duplicate to be suppressed, got %v", it.Key())
	}
}

// Test J: Tombstone as newest revision is surfaced verbatim
func TestMergingIterator_J_TombstoneSurfacing(t *testing.T) {
	childA := newMockIterator([]testRecord{
		{key: makeKey("user-1", 100, binary.OpTypeDelete), value: nil},
	})
	childB := newMockIterator([]testRecord{
		{key: makeKey("user-1", 90, binary.OpTypePut), value: []byte("older-value")},
	})

	it := NewMergingIterator([]Iterator{childA, childB})
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("expected Next() == true")
	}
	k := it.Key()
	if k.OpType != binary.OpTypeDelete {
		t.Fatalf("expected tombstone OpTypeDelete, got %v", k.OpType)
	}
	if it.Value() != nil {
		t.Fatalf("expected nil value for tombstone, got %v", it.Value())
	}
	if it.Next() {
		t.Fatalf("expected older revision to be suppressed, got %v", it.Key())
	}
}

// Test M: Nil children in input slice are ignored
func TestMergingIterator_M_NilChildHandling(t *testing.T) {
	child := newMockIterator([]testRecord{
		{key: makeKey("a", 10, binary.OpTypePut), value: []byte("val")},
	})
	it := NewMergingIterator([]Iterator{nil, child, nil})
	defer func() { _ = it.Close() }()

	if !it.Next() {
		t.Fatalf("expected Next() == true")
	}
	if string(it.Key().UserKey) != "a" {
		t.Fatalf("expected 'a', got %s", it.Key().UserKey)
	}
	if it.Next() {
		t.Fatalf("expected EOF")
	}
}

// Test O: Child corruption on initial state
func TestMergingIterator_O_ChildCorruptionOnInit(t *testing.T) {
	corruptChild := &mockIterator{
		err: stdErrors.New("corrupted on open"),
	}
	goodChild := newMockIterator([]testRecord{
		{key: makeKey("a", 10, binary.OpTypePut), value: []byte("val")},
	})

	it := NewMergingIterator([]Iterator{corruptChild, goodChild})
	defer func() { _ = it.Close() }()

	if it.Valid() {
		t.Fatalf("expected Valid() == false when child is corrupt")
	}
	if it.Next() {
		t.Fatalf("expected Next() == false when child is corrupt")
	}
	if it.Err() == nil {
		t.Fatalf("expected non-nil Err() on child corruption")
	}
}

// Test P: Error after valid prefix (child corrupts mid-iteration)
func TestMergingIterator_P_ErrorAfterValidPrefix(t *testing.T) {
	childA := newMockIterator([]testRecord{
		{key: makeKey("a", 10, binary.OpTypePut), value: []byte("val-a")},
		{key: makeKey("c", 10, binary.OpTypePut), value: []byte("val-c")},
		{key: makeKey("e", 10, binary.OpTypePut), value: []byte("val-e")},
	})
	childB := newMockIterator([]testRecord{
		{key: makeKey("b", 10, binary.OpTypePut), value: []byte("val-b")},
		{key: makeKey("d", 10, binary.OpTypePut), value: []byte("val-d")},
	})
	// Fail childB when advancing to record index 1 ("d")
	childB.failAt = 1

	it := NewMergingIterator([]Iterator{childA, childB})
	defer func() { _ = it.Close() }()

	// First record: "a" from childA
	if !it.Next() || string(it.Key().UserKey) != "a" {
		t.Fatalf("expected 'a', got %s", it.Key().UserKey)
	}

	// Second record: "b" from childB
	if !it.Next() || string(it.Key().UserKey) != "b" {
		t.Fatalf("expected 'b', got %s", it.Key().UserKey)
	}

	// Advancing childB to "d" triggers corruption!
	// Next() must detect corruption, transition to failed, and return false!
	for it.Next() {
		// If it emits more records, it must eventually fail before completing
	}

	if it.Valid() {
		t.Fatalf("expected Valid() == false after corruption")
	}
	if it.Err() == nil {
		t.Fatalf("expected Err() != nil due to childB corruption")
	}
}

// Test Q: Close idempotence
func TestMergingIterator_Q_CloseIdempotence(t *testing.T) {
	child := newMockIterator([]testRecord{
		{key: makeKey("k", 10, binary.OpTypePut), value: []byte("v")},
	})
	it := NewMergingIterator([]Iterator{child})

	if err := it.Close(); err != nil {
		t.Fatalf("first Close() failed: %v", err)
	}
	if !child.closed {
		t.Fatalf("expected child to be closed")
	}

	// Second Close() must be idempotent and return nil
	if err := it.Close(); err != nil {
		t.Fatalf("second Close() failed: %v", err)
	}
}

// Test R: Next after close
func TestMergingIterator_R_NextAfterClose(t *testing.T) {
	child := newMockIterator([]testRecord{
		{key: makeKey("k", 10, binary.OpTypePut), value: []byte("v")},
	})
	it := NewMergingIterator([]Iterator{child})
	_ = it.Close()

	if it.Next() {
		t.Fatalf("Next() after Close() returned true")
	}
	if it.Valid() {
		t.Fatalf("Valid() after Close() returned true")
	}
}

// Test S: RawMergingIterator yields all revisions without suppression
func TestMergingIterator_S_RawMergingIterator(t *testing.T) {
	childA := newMockIterator([]testRecord{
		{key: makeKey("k", 100, binary.OpTypePut), value: []byte("v100")},
		{key: makeKey("k", 70, binary.OpTypePut), value: []byte("v70")},
	})
	childB := newMockIterator([]testRecord{
		{key: makeKey("k", 90, binary.OpTypePut), value: []byte("v90")},
	})

	it := NewRawMergingIterator([]Iterator{childA, childB})
	defer func() { _ = it.Close() }()

	var seqs []uint64
	for it.Next() {
		seqs = append(seqs, uint64(it.Key().SeqNum))
	}

	// All 3 revisions must be yielded in descending order: 100, 90, 70
	expected := []uint64{100, 90, 70}
	if len(seqs) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, seqs)
	}
	for i := range expected {
		if seqs[i] != expected[i] {
			t.Fatalf("seq %d: got %d, want %d", i, seqs[i], expected[i])
		}
	}
}

// Test T: Real SSTables generated via TableWriter and merged via MergingIterator
func TestMergingIterator_T_RealSSTablesMerge(t *testing.T) {
	dir := t.TempDir()

	writeSSTable := func(filename string, recs []testRecord) *sstable.TableReader {
		path := filepath.Join(dir, filename)
		opts := sstable.DefaultTableWriterOptions()
		opts.TargetBlockSize = 256 // small blocks to exercise multi-block traversal

		w, err := sstable.NewTableWriter(path, opts)
		if err != nil {
			t.Fatalf("failed to create TableWriter: %v", err)
		}
		for _, r := range recs {
			if err := w.Add(r.key, r.value); err != nil {
				t.Fatalf("failed to add record: %v", err)
			}
		}
		if _, err := w.Finish(); err != nil {
			t.Fatalf("failed to finish TableWriter: %v", err)
		}

		reader, err := sstable.NewTableReader(path)
		if err != nil {
			t.Fatalf("failed to open TableReader: %v", err)
		}
		return reader
	}

	// Table 1: a@100, c@100, e@100
	r1 := writeSSTable("table1.sst", []testRecord{
		{key: makeKey("a", 100, binary.OpTypePut), value: []byte("val-a1")},
		{key: makeKey("c", 100, binary.OpTypePut), value: []byte("val-c1")},
		{key: makeKey("e", 100, binary.OpTypePut), value: []byte("val-e1")},
	})
	defer func() { _ = r1.Close() }()

	// Table 2: b@90, c@80, d@90
	r2 := writeSSTable("table2.sst", []testRecord{
		{key: makeKey("b", 90, binary.OpTypePut), value: []byte("val-b2")},
		{key: makeKey("c", 80, binary.OpTypePut), value: []byte("val-c2")},
		{key: makeKey("d", 90, binary.OpTypePut), value: []byte("val-d2")},
	})
	defer func() { _ = r2.Close() }()

	// Table 3: a@90, d@110 (DELETE), f@70
	r3 := writeSSTable("table3.sst", []testRecord{
		{key: makeKey("a", 90, binary.OpTypePut), value: []byte("val-a3")},
		{key: makeKey("d", 110, binary.OpTypeDelete), value: nil},
		{key: makeKey("f", 70, binary.OpTypePut), value: []byte("val-f3")},
	})
	defer func() { _ = r3.Close() }()

	// Table 4: b@120, e@95, g@50
	r4 := writeSSTable("table4.sst", []testRecord{
		{key: makeKey("b", 120, binary.OpTypePut), value: []byte("val-b4")},
		{key: makeKey("e", 95, binary.OpTypePut), value: []byte("val-e4")},
		{key: makeKey("g", 50, binary.OpTypePut), value: []byte("val-g4")},
	})
	defer func() { _ = r4.Close() }()

	// Create TableIterators positioned at first record
	it1, err := r1.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator 1: %v", err)
	}
	if err := it1.SeekToFirst(); err != nil {
		t.Fatalf("failed to SeekToFirst 1: %v", err)
	}

	it2, err := r2.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator 2: %v", err)
	}
	if err := it2.SeekToFirst(); err != nil {
		t.Fatalf("failed to SeekToFirst 2: %v", err)
	}

	it3, err := r3.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator 3: %v", err)
	}
	if err := it3.SeekToFirst(); err != nil {
		t.Fatalf("failed to SeekToFirst 3: %v", err)
	}

	it4, err := r4.NewIterator()
	if err != nil {
		t.Fatalf("failed to create iterator 4: %v", err)
	}
	if err := it4.SeekToFirst(); err != nil {
		t.Fatalf("failed to SeekToFirst 4: %v", err)
	}

	mergeIt := NewMergingIterator([]Iterator{it1, it2, it3, it4})
	defer func() { _ = mergeIt.Close() }()

	// Expected results:
	// "a": a@100 PUT ("val-a1") [a@90 suppressed]
	// "b": b@120 PUT ("val-b4") [b@90 suppressed]
	// "c": c@100 PUT ("val-c1") [c@80 suppressed]
	// "d": d@110 DELETE (nil)   [d@90 suppressed]
	// "e": e@100 PUT ("val-e1") [e@95 suppressed]
	// "f": f@70 PUT ("val-f3")
	// "g": g@50 PUT ("val-g4")

	type expItem struct {
		userKey string
		seq     uint64
		op      binary.OpType
		val     string
	}
	expected := []expItem{
		{"a", 100, binary.OpTypePut, "val-a1"},
		{"b", 120, binary.OpTypePut, "val-b4"},
		{"c", 100, binary.OpTypePut, "val-c1"},
		{"d", 110, binary.OpTypeDelete, ""},
		{"e", 100, binary.OpTypePut, "val-e1"},
		{"f", 70, binary.OpTypePut, "val-f3"},
		{"g", 50, binary.OpTypePut, "val-g4"},
	}

	var count int
	for mergeIt.Next() {
		if count >= len(expected) {
			t.Fatalf("emitted more records than expected (%d)", count)
		}
		want := expected[count]
		k := mergeIt.Key()
		if string(k.UserKey) != want.userKey {
			t.Fatalf("record %d key mismatch: got %s, want %s", count, k.UserKey, want.userKey)
		}
		if uint64(k.SeqNum) != want.seq {
			t.Fatalf("record %d seq mismatch: got %d, want %d", count, k.SeqNum, want.seq)
		}
		if k.OpType != want.op {
			t.Fatalf("record %d op mismatch: got %v, want %v", count, k.OpType, want.op)
		}
		if want.op == binary.OpTypeDelete {
			if mergeIt.Value() != nil {
				t.Fatalf("record %d tombstone expected nil value, got %s", count, mergeIt.Value())
			}
		} else {
			if string(mergeIt.Value()) != want.val {
				t.Fatalf("record %d value mismatch: got %s, want %s", count, mergeIt.Value(), want.val)
			}
		}
		count++
	}

	if err := mergeIt.Err(); err != nil {
		t.Fatalf("unexpected Err(): %v", err)
	}
	if count != len(expected) {
		t.Fatalf("expected %d records, got %d", len(expected), count)
	}
}

// Test U: Panic-freedom across lifecycle and malformed usage
func TestMergingIterator_U_PanicFreedom(t *testing.T) {
	// 1. Nil receiver
	var nilIt *MergingIterator
	if nilIt.Valid() {
		t.Fatalf("nil receiver Valid() must be false")
	}
	if nilIt.Next() {
		t.Fatalf("nil receiver Next() must be false")
	}
	if nilIt.Key().UserKey != nil {
		t.Fatalf("nil receiver Key() must be empty")
	}
	if nilIt.RawKey() != nil {
		t.Fatalf("nil receiver RawKey() must be nil")
	}
	if nilIt.Value() != nil {
		t.Fatalf("nil receiver Value() must be nil")
	}
	if !stdErrors.Is(nilIt.Err(), errors.ErrNilReceiver) {
		t.Fatalf("nil receiver Err() must be ErrNilReceiver, got %v", nilIt.Err())
	}
	if !stdErrors.Is(nilIt.Close(), errors.ErrNilReceiver) {
		t.Fatalf("nil receiver Close() must be ErrNilReceiver, got %v", nilIt.Close())
	}

	// 2. Key/Value before Valid
	child := newMockIterator([]testRecord{
		{key: makeKey("k", 10, binary.OpTypePut), value: []byte("v")},
	})
	it := NewMergingIterator([]Iterator{child})
	if it.Valid() {
		t.Fatalf("expected Valid() == false before first Next()")
	}
	if it.Key().UserKey != nil {
		t.Fatalf("Key() before Valid() must return empty InternalKey")
	}
	if it.Value() != nil {
		t.Fatalf("Value() before Valid() must return nil")
	}
	if it.RawKey() != nil {
		t.Fatalf("RawKey() before Valid() must return nil")
	}

	// 3. Next until EOF then check Key/Value again
	if !it.Next() {
		t.Fatalf("Next() should be true")
	}
	if it.Next() {
		t.Fatalf("Next() should be false at EOF")
	}
	if it.Valid() {
		t.Fatalf("Valid() at EOF must be false")
	}
	if it.Key().UserKey != nil {
		t.Fatalf("Key() at EOF must return empty InternalKey")
	}
	if it.Value() != nil {
		t.Fatalf("Value() at EOF must return nil")
	}

	// 4. Repeated Close
	if err := it.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("Repeated Close() failed: %v", err)
	}
}

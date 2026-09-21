package raft_test

import (
	"bytes"
	stdErrors "errors"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
	"math"
	"testing"
)

func TestLogEntry_Validation(t *testing.T) {
	tests := []struct {
		name    string
		entry   raft.LogEntry
		wantErr bool
	}{
		{
			name: "valid normal entry",
			entry: raft.LogEntry{
				Index: 1,
				Term:  1,
				Type:  transport.PeerEntryNormal,
				Data:  []byte("command-data"),
			},
			wantErr: false,
		},
		{
			name: "valid empty data normal entry",
			entry: raft.LogEntry{
				Index: 1,
				Term:  1,
				Type:  transport.PeerEntryNormal,
				Data:  nil,
			},
			wantErr: false,
		},
		{
			name: "valid configuration entry",
			entry: raft.LogEntry{
				Index: 5,
				Term:  2,
				Type:  transport.PeerEntryConfiguration,
				Data:  []byte("config-delta"),
			},
			wantErr: false,
		},
		{
			name: "valid noop entry",
			entry: raft.LogEntry{
				Index: 10,
				Term:  3,
				Type:  transport.PeerEntryNoop,
				Data:  nil,
			},
			wantErr: false,
		},
		{
			name: "zero index rejected",
			entry: raft.LogEntry{
				Index: 0,
				Term:  1,
				Type:  transport.PeerEntryNormal,
			},
			wantErr: true,
		},
		{
			name: "zero term rejected",
			entry: raft.LogEntry{
				Index: 1,
				Term:  0,
				Type:  transport.PeerEntryNormal,
			},
			wantErr: true,
		},
		{
			name: "invalid entry type rejected",
			entry: raft.LogEntry{
				Index: 1,
				Term:  1,
				Type:  transport.PeerEntryType(0xFF),
			},
			wantErr: true,
		},
		{
			name: "oversized payload rejected",
			entry: raft.LogEntry{
				Index: 1,
				Term:  1,
				Type:  transport.PeerEntryNormal,
				Data:  make([]byte, raft.MaxLogEntryDataSize+1),
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.entry.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestLogEntry_DefensiveClone(t *testing.T) {
	orig := raft.LogEntry{
		Index: 1,
		Term:  1,
		Type:  transport.PeerEntryNormal,
		Data:  []byte("original-bytes"),
	}

	cloned := orig.Clone()
	cloned.Data[0] = 'X'

	if orig.Data[0] == 'X' {
		t.Fatalf("Clone did not isolate underlying byte slice")
	}
}

func TestHardState_Basics(t *testing.T) {
	hs := raft.HardState{
		Term:     10,
		VotedFor: 3,
	}
	cp := hs.Clone()
	if cp != hs {
		t.Fatalf("Clone mismatch: got %+v, want %+v", cp, hs)
	}
	if err := hs.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
}

func TestInMemLog_EmptyLog(t *testing.T) {
	l := raft.NewInMemLog()
	if l.Len() != 0 {
		t.Fatalf("expected empty log length 0, got %d", l.Len())
	}
	if l.LastIndex() != 0 {
		t.Fatalf("expected last index 0, got %d", l.LastIndex())
	}

	idx, term := l.LastIndexAndTerm()
	if idx != 0 || term != 0 {
		t.Fatalf("expected (0, 0), got (%d, %d)", idx, term)
	}

	termZero, err := l.Term(0)
	if err != nil || termZero != 0 {
		t.Fatalf("Term(0) should return 0, nil; got (%d, %v)", termZero, err)
	}

	_, err = l.Term(1)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftLogIndexOutOfBounds) {
		t.Fatalf("expected ErrRaftLogIndexOutOfBounds, got %v", err)
	}

	_, err = l.Entry(0)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftLogIndexOutOfBounds) {
		t.Fatalf("Entry(0) should error with ErrRaftLogIndexOutOfBounds, got %v", err)
	}

	_, err = l.Entry(1)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftLogIndexOutOfBounds) {
		t.Fatalf("Entry(1) on empty log should error, got %v", err)
	}
}

func TestInMemLog_AppendAndLookup(t *testing.T) {
	l := raft.NewInMemLog()

	entries := []raft.LogEntry{
		{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("cmd1")},
		{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("cmd2")},
		{Index: 3, Term: 2, Type: transport.PeerEntryConfiguration, Data: []byte("cfg3")},
	}

	if err := l.Append(entries...); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	if l.Len() != 3 {
		t.Fatalf("expected len 3, got %d", l.Len())
	}
	if l.LastIndex() != 3 {
		t.Fatalf("expected last index 3, got %d", l.LastIndex())
	}

	idx, term := l.LastIndexAndTerm()
	if idx != 3 || term != 2 {
		t.Fatalf("expected (3, 2), got (%d, %d)", idx, term)
	}

	// Verify lookups
	for i, exp := range entries {
		got, err := l.Entry(raft.LogIndex(i + 1))
		if err != nil {
			t.Fatalf("Entry(%d) failed: %v", i+1, err)
		}
		if got.Index != exp.Index || got.Term != exp.Term || got.Type != exp.Type || !bytes.Equal(got.Data, exp.Data) {
			t.Fatalf("Entry(%d) mismatch: got %+v, want %+v", i+1, got, exp)
		}

		gotTerm, err := l.Term(raft.LogIndex(i + 1))
		if err != nil || gotTerm != exp.Term {
			t.Fatalf("Term(%d) got (%d, %v), want %d", i+1, gotTerm, err, exp.Term)
		}
	}

	// Test slice range lookup
	slice, err := l.Entries(1, 3) // [1, 3) -> indices 1 and 2
	if err != nil {
		t.Fatalf("Entries(1, 3) failed: %v", err)
	}
	if len(slice) != 2 {
		t.Fatalf("expected 2 entries in slice, got %d", len(slice))
	}
	if slice[0].Index != 1 || slice[1].Index != 2 {
		t.Fatalf("slice indices mismatch: %d, %d", slice[0].Index, slice[1].Index)
	}

	// Slice empty range
	emptySlice, err := l.Entries(2, 2)
	if err != nil || len(emptySlice) != 0 {
		t.Fatalf("Entries(2, 2) expected (empty, nil), got (%v, %v)", emptySlice, err)
	}

	// Out of bounds range
	_, err = l.Entries(1, 5)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftLogIndexOutOfBounds) {
		t.Fatalf("expected ErrRaftLogIndexOutOfBounds, got %v", err)
	}
}

func TestInMemLog_AppendContiguityAndOverflow(t *testing.T) {
	l := raft.NewInMemLog()

	// Gap at start (expecting 1, got 2)
	err := l.Append(raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal})
	if err == nil || !stdErrors.Is(err, errors.ErrRaftLogIndexGap) {
		t.Fatalf("expected ErrRaftLogIndexGap, got %v", err)
	}

	// Valid append of 1
	if err := l.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal}); err != nil {
		t.Fatalf("Append(1) failed: %v", err)
	}

	// Gap inside batch (2 then 4)
	err = l.Append(
		raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal},
		raft.LogEntry{Index: 4, Term: 1, Type: transport.PeerEntryNormal},
	)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftLogIndexGap) {
		t.Fatalf("expected ErrRaftLogIndexGap for non-contiguous batch, got %v", err)
	}
	// Verify log length unchanged after failed append
	if l.Len() != 1 {
		t.Fatalf("log length should remain 1, got %d", l.Len())
	}
}

func TestInMemLog_TruncateSuffix(t *testing.T) {
	l := raft.NewInMemLog()
	_ = l.Append(
		raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal},
		raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal},
		raft.LogEntry{Index: 3, Term: 2, Type: transport.PeerEntryNormal},
		raft.LogEntry{Index: 4, Term: 2, Type: transport.PeerEntryNormal},
	)

	// Truncate above last index (no-op)
	l.TruncateSuffix(5)
	if l.Len() != 4 {
		t.Fatalf("expected len 4, got %d", l.Len())
	}

	// Truncate at index 3 (removes 3 and 4)
	l.TruncateSuffix(3)
	if l.Len() != 2 {
		t.Fatalf("expected len 2, got %d", l.Len())
	}
	if l.LastIndex() != 2 {
		t.Fatalf("expected last index 2, got %d", l.LastIndex())
	}

	// Truncate at index 1 (clears entire log)
	l.TruncateSuffix(1)
	if l.Len() != 0 {
		t.Fatalf("expected len 0, got %d", l.Len())
	}
}

func TestCheckContiguity_Overflow(t *testing.T) {
	entries := []raft.LogEntry{
		{Index: raft.LogIndex(math.MaxUint64), Term: 1, Type: transport.PeerEntryNormal},
		{Index: 1, Term: 1, Type: transport.PeerEntryNormal},
	}
	err := raft.CheckContiguity(entries, raft.LogIndex(math.MaxUint64))
	if err == nil {
		t.Fatalf("expected overflow error")
	}
}

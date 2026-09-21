package raft_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

func TestStorage_ColdBootInitialization(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	term, err := s.Term()
	if err != nil || term != 0 {
		t.Fatalf("Term() = (%d, %v), want (0, nil)", term, err)
	}

	vote, err := s.VotedFor()
	if err != nil || vote != cluster.NodeIDNil {
		t.Fatalf("VotedFor() = (%d, %v), want (0, nil)", vote, err)
	}

	lastIdx, lastTerm, err := s.LastIndexAndTerm()
	if err != nil || lastIdx != 0 || lastTerm != 0 {
		t.Fatalf("LastIndexAndTerm() = (%d, %d, %v), want (0, 0, nil)", lastIdx, lastTerm, err)
	}
}

func TestStorage_TermMonotonicityAndVoteSafety(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Advance to term 1
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm(1) failed: %v", err)
	}
	term, _ := s.Term()
	if term != 1 {
		t.Fatalf("Term() = %d, want 1", term)
	}

	// Cast vote for candidate 2
	if err := s.SetVote(2); err != nil {
		t.Fatalf("SetVote(2) failed: %v", err)
	}
	vote, _ := s.VotedFor()
	if vote != 2 {
		t.Fatalf("VotedFor() = %d, want 2", vote)
	}

	// Idempotent vote for same candidate in same term succeeds
	if err := s.SetVote(2); err != nil {
		t.Fatalf("SetVote(2) idempotent re-vote failed: %v", err)
	}

	// Attempting to vote for candidate 3 in same term must fail
	err = s.SetVote(3)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftDuplicateVote) {
		t.Fatalf("expected ErrRaftDuplicateVote, got %v", err)
	}

	// Attempting term regression (1 -> 0) must fail
	err = s.SetTerm(0)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftTermRegressed) {
		t.Fatalf("expected ErrRaftTermRegressed, got %v", err)
	}

	// Advance term to 2 clears vote automatically
	if err := s.SetTerm(2); err != nil {
		t.Fatalf("SetTerm(2) failed: %v", err)
	}
	term, _ = s.Term()
	vote, _ = s.VotedFor()
	if term != 2 || vote != cluster.NodeIDNil {
		t.Fatalf("Term=%d (want 2), VotedFor=%d (want 0)", term, vote)
	}

	// In new term 2, voting for candidate 3 now succeeds
	if err := s.SetVote(3); err != nil {
		t.Fatalf("SetVote(3) in term 2 failed: %v", err)
	}
}

func TestStorage_RestartRecoveryDeterminism(t *testing.T) {
	dir := t.TempDir()

	// 1. First session: populate metadata and entries
	s1, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage 1 failed: %v", err)
	}

	if err := s1.SetTerm(3); err != nil {
		t.Fatalf("SetTerm(3) failed: %v", err)
	}
	if err := s1.SetVote(5); err != nil {
		t.Fatalf("SetVote(5) failed: %v", err)
	}

	entries := []raft.LogEntry{
		{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("k1=v1")},
		{Index: 2, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("k2=v2")},
		{Index: 3, Term: 3, Type: transport.PeerEntryConfiguration, Data: []byte("peers")},
	}
	if err := s1.Append(entries...); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	if err := s1.Close(); err != nil {
		t.Fatalf("Close 1 failed: %v", err)
	}

	// 2. Second session: reopen and verify full recovery
	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage 2 failed: %v", err)
	}
	defer func() { _ = s2.Close() }()

	term, _ := s2.Term()
	vote, _ := s2.VotedFor()
	if term != 3 || vote != 5 {
		t.Fatalf("recovered metadata mismatch: term=%d (want 3), vote=%d (want 5)", term, vote)
	}

	lastIdx, lastTerm, _ := s2.LastIndexAndTerm()
	if lastIdx != 3 || lastTerm != 3 {
		t.Fatalf("recovered last index/term mismatch: (%d, %d), want (3, 3)", lastIdx, lastTerm)
	}

	for _, exp := range entries {
		got, err := s2.Entry(exp.Index)
		if err != nil {
			t.Fatalf("Entry(%d) failed: %v", exp.Index, err)
		}
		if got.Index != exp.Index || got.Term != exp.Term || got.Type != exp.Type || !bytes.Equal(got.Data, exp.Data) {
			t.Fatalf("Entry(%d) mismatch: got %+v, want %+v", exp.Index, got, exp)
		}
	}
}

func TestStorage_TruncateSuffixAndReopen(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	entries := []raft.LogEntry{
		{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
		{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")},
		{Index: 3, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("3")},
		{Index: 4, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("4")},
	}
	_ = s.Append(entries...)

	// Truncate at index 3 (drops 3 and 4)
	if err := s.TruncateSuffix(3); err != nil {
		t.Fatalf("TruncateSuffix(3) failed: %v", err)
	}

	lastIdx, _ := s.LastIndex()
	if lastIdx != 2 {
		t.Fatalf("expected last index 2, got %d", lastIdx)
	}

	// Append replacement entry at index 3 with higher term
	newEntry := raft.LogEntry{Index: 3, Term: 3, Type: transport.PeerEntryNormal, Data: []byte("new-3")}
	if err := s.Append(newEntry); err != nil {
		t.Fatalf("Append newEntry failed: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen and verify durable truncated state
	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer func() { _ = s2.Close() }()

	lastIdx, lastTerm, _ := s2.LastIndexAndTerm()
	if lastIdx != 3 || lastTerm != 3 {
		t.Fatalf("expected (3, 3), got (%d, %d)", lastIdx, lastTerm)
	}

	rec3, _ := s2.Entry(3)
	if !bytes.Equal(rec3.Data, []byte("new-3")) {
		t.Fatalf("expected new-3, got %s", string(rec3.Data))
	}
}

func TestStorage_TornWriteAtEOFRecovery(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	entries := []raft.LogEntry{
		{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("valid-entry-1")},
		{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("valid-entry-2")},
	}
	if err := s.Append(entries...); err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	_ = s.Close()

	// Simulate torn write by appending 7 garbage bytes to the physical log file
	logPath := filepath.Join(dir, raft.LogFilename)
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	if _, err := f.Write([]byte("tornby!")); err != nil {
		t.Fatalf("Write torn bytes failed: %v", err)
	}
	_ = f.Close()

	// Reopening must detect torn write at physical EOF, truncate it, and recover the 2 valid entries
	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage after torn write failed: %v", err)
	}
	defer func() { _ = s2.Close() }()

	lastIdx, _ := s2.LastIndex()
	if lastIdx != 2 {
		t.Fatalf("expected 2 valid entries after torn-write recovery, got %d", lastIdx)
	}

	// Can safely append entry 3 after recovery
	e3 := raft.LogEntry{Index: 3, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("entry-3")}
	if err := s2.Append(e3); err != nil {
		t.Fatalf("Append after torn recovery failed: %v", err)
	}
}

func TestStorage_MidLogCorruptionFailsClosed(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	_ = s.Append(
		raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("entry-1")},
		raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("entry-2")},
	)
	_ = s.Close()

	// Corrupt a byte in record 1's payload
	logPath := filepath.Join(dir, raft.LogFilename)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	// Flip bit in the first record
	data[raft.LogRecordHeaderSize] ^= 0xFF
	if err := os.WriteFile(logPath, data, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Reopening must fail closed with ErrRaftCorruptedState
	_, err = raft.OpenStorage(dir)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
		t.Fatalf("expected ErrRaftCorruptedState on mid-log corruption, got %v", err)
	}
}

func TestStorage_StateFileCorruptionFailsClosed(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	_ = s.Close()

	statePath := filepath.Join(dir, raft.StateFilename)
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	// Flip byte in term payload
	data[10] ^= 0xFF
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err = raft.OpenStorage(dir)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
		t.Fatalf("expected ErrRaftCorruptedState on state CRC mismatch, got %v", err)
	}
}

func TestStorage_StateFileVersionMismatch(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	_ = s.Close()

	statePath := filepath.Join(dir, raft.StateFilename)
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	// Set version to 999
	binary.PutUint16(data[4:6], 999)
	// Recompute CRC so it triggers version error instead of CRC error
	crc := binary.Checksum(data[0:raft.StatePayloadSize])
	binary.PutUint32(data[raft.StatePayloadSize:raft.StateRecordSize], crc)
	_ = os.WriteFile(statePath, data, 0600)

	_, err = raft.OpenStorage(dir)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
		t.Fatalf("expected ErrRaftCorruptedState on unknown version, got %v", err)
	}
}

func TestStorage_ClosedStateEnforcement(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	// Repeated close is idempotent
	if err := s.Close(); err != nil {
		t.Fatalf("Repeated close failed: %v", err)
	}

	// All operations must return ErrRaftStateClosed
	if _, err := s.Term(); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got %v", err)
	}
	if _, err := s.VotedFor(); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got %v", err)
	}
	if err := s.SetTerm(1); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got %v", err)
	}
	if err := s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal}); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got %v", err)
	}
	if err := s.TruncateSuffix(1); !stdErrors.Is(err, errors.ErrRaftStateClosed) {
		t.Fatalf("expected ErrRaftStateClosed, got %v", err)
	}
}

func TestStorage_ConcurrencyRace(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	var wg sync.WaitGroup
	const readers = 10
	const appends = 50

	// Concurrent readers
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = s.Term()
				_, _ = s.VotedFor()
				_, _, _ = s.LastIndexAndTerm()
			}
		}()
	}

	// Serialized appenders
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= appends; i++ {
			e := raft.LogEntry{
				Index: raft.LogIndex(i),
				Term:  1,
				Type:  transport.PeerEntryNormal,
				Data:  []byte(fmt.Sprintf("entry-%d", i)),
			}
			if err := s.Append(e); err != nil {
				t.Errorf("concurrent append failed: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	lastIdx, _ := s.LastIndex()
	if lastIdx != appends {
		t.Fatalf("expected last index %d, got %d", appends, lastIdx)
	}
}

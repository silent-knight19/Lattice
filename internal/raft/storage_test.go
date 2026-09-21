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

	// Calling SetTerm(1) with current term 1 must NOT clear the existing vote for node 2
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm(1) same-term failed: %v", err)
	}
	vote, _ = s.VotedFor()
	if vote != 2 {
		t.Fatalf("SetTerm(1) cleared vote in same term! got %d, want 2", vote)
	}
	// Attempting to vote for candidate 3 after SetTerm(1) must still be rejected
	err = s.SetVote(3)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftDuplicateVote) {
		t.Fatalf("expected ErrRaftDuplicateVote after SetTerm(1), got %v", err)
	}

	// Calling SetHardState in same term with votedFor=NodeIDNil must fail closed
	err = s.SetHardState(raft.HardState{Term: 1, VotedFor: cluster.NodeIDNil})
	if err == nil || !stdErrors.Is(err, errors.ErrRaftVoteClearedInSameTerm) {
		t.Fatalf("expected ErrRaftVoteClearedInSameTerm, got %v", err)
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

	if err := s.SetTerm(2); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

	entries := []raft.LogEntry{
		{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
		{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")},
		{Index: 3, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("3")},
		{Index: 4, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("4")},
	}
	if err := s.Append(entries...); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// Truncate at index 3 (drops 3 and 4)
	if err := s.TruncateSuffix(3); err != nil {
		t.Fatalf("TruncateSuffix(3) failed: %v", err)
	}

	lastIdx, _ := s.LastIndex()
	if lastIdx != 2 {
		t.Fatalf("expected last index 2, got %d", lastIdx)
	}

	// Advance term to 3 before appending entry with term 3
	if err := s.SetTerm(3); err != nil {
		t.Fatalf("SetTerm(3) failed: %v", err)
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

func TestStorage_TruncateSuffix_FaultInjectionBoundaries(t *testing.T) {
	// 1. Temp creation failure -> storage remains coherent and open
	t.Run("temp_creation_failure", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		restore := raft.SetRaftCreateTmpFnForTesting(func(path string, flag int, perm os.FileMode) (*os.File, error) {
			return nil, fmt.Errorf("injected create tmp error")
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on tmp create failure")
		}

		if s.LogFileDescriptorNilForTesting() {
			t.Fatalf("logFile descriptor became nil on non-terminal truncate failure")
		}

		// Storage remains coherent and usable: can append entry 3
		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err != nil {
			t.Fatalf("expected Append to succeed after non-terminal truncate failure, got %v", err)
		}
	})

	// 2. Temp write failure -> storage remains coherent and open
	t.Run("temp_write_failure", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		restore := raft.SetRaftWriteTmpFnForTesting(func(f *os.File, b []byte) (int, error) {
			return 0, fmt.Errorf("injected write tmp error")
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on tmp write failure")
		}

		if s.LogFileDescriptorNilForTesting() {
			t.Fatalf("logFile descriptor became nil on non-terminal truncate failure")
		}

		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err != nil {
			t.Fatalf("expected Append to succeed after non-terminal truncate failure, got %v", err)
		}
	})

	// 3. Temp sync failure -> storage remains coherent and open
	t.Run("temp_sync_failure", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		restore := raft.SetRaftSyncTmpFnForTesting(func(f *os.File) error {
			return fmt.Errorf("injected sync tmp error")
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on tmp sync failure")
		}

		if s.LogFileDescriptorNilForTesting() {
			t.Fatalf("logFile descriptor became nil on non-terminal truncate failure")
		}

		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err != nil {
			t.Fatalf("expected Append to succeed after non-terminal truncate failure, got %v", err)
		}
	})

	// 4. Temp close failure -> storage remains coherent and open
	t.Run("temp_close_failure", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		restore := raft.SetRaftCloseTmpFnForTesting(func(f *os.File) error {
			_ = f.Close()
			return fmt.Errorf("injected close tmp error")
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on tmp close failure")
		}

		if s.LogFileDescriptorNilForTesting() {
			t.Fatalf("logFile descriptor became nil on non-terminal truncate failure")
		}

		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err != nil {
			t.Fatalf("expected Append to succeed after non-terminal truncate failure, got %v", err)
		}
	})

	// 5. Rename failure -> storage remains coherent and open
	t.Run("rename_failure", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		restore := raft.SetRaftRenameFnForTesting(func(oldpath, newpath string) error {
			return fmt.Errorf("injected rename error")
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on rename failure")
		}

		if s.LogFileDescriptorNilForTesting() {
			t.Fatalf("logFile descriptor became nil on non-terminal truncate failure")
		}

		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err != nil {
			t.Fatalf("expected Append to succeed after non-terminal truncate failure, got %v", err)
		}
	})

	// 6. Directory sync failure (after rename) -> terminal fail-closed state
	t.Run("directory_sync_failure_terminal", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		failSync := true
		restore := raft.SetRaftSyncDirFnForTesting(func(dirPath string) error {
			if failSync {
				return fmt.Errorf("injected sync dir error")
			}
			return nil
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on sync dir failure")
		}

		// Storage is now terminally failed (disk was replaced, but dir sync failed)
		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err == nil || !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed after terminal truncation failure, got %v", err)
		}

		failSync = false // Stop failing so OpenStorage can reopen

		// Reopen storage: disk state is deterministic (entry 1 recovered)
		s2, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("recovery OpenStorage failed: %v", err)
		}
		defer func() { _ = s2.Close() }()

		lastIdx, _ := s2.LastIndex()
		if lastIdx != 1 {
			t.Fatalf("recovered last index = %d, want 1", lastIdx)
		}
	})

	// 7. Replacement open failure (after rename) -> terminal fail-closed state
	t.Run("replacement_open_failure_terminal", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		defer func() { _ = s.Close() }()

		_ = s.SetTerm(1)
		_ = s.Append(raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("1")},
			raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("2")})

		failReopen := true
		restore := raft.SetRaftOpenFileFnForTesting(func(name string, flag int, perm os.FileMode) (*os.File, error) {
			if failReopen && flag&os.O_CREATE == 0 {
				return nil, fmt.Errorf("injected reopen error")
			}
			return os.OpenFile(name, flag, perm)
		})
		defer restore()

		err = s.TruncateSuffix(2)
		if err == nil {
			t.Fatalf("expected error from TruncateSuffix on replacement open failure")
		}

		// Storage is now terminally failed
		err = s.Append(raft.LogEntry{Index: 3, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("3")})
		if err == nil || !stdErrors.Is(err, errors.ErrRaftStateClosed) {
			t.Fatalf("expected ErrRaftStateClosed after terminal truncation failure, got %v", err)
		}

		failReopen = false

		// Reopen storage: disk state is deterministic
		s2, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("recovery OpenStorage failed: %v", err)
		}
		defer func() { _ = s2.Close() }()

		lastIdx, _ := s2.LastIndex()
		if lastIdx != 1 {
			t.Fatalf("recovered last index = %d, want 1", lastIdx)
		}
	})
}

func TestStorage_TornWriteAtEOFRecovery(t *testing.T) {
	dir := t.TempDir()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
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

	// Can safely append entry 3 after recovery (advancing term to 2)
	if err := s2.SetTerm(2); err != nil {
		t.Fatalf("SetTerm(2) failed: %v", err)
	}
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
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
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
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm failed: %v", err)
	}

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

func TestStorage_LogFileCreationDurability(t *testing.T) {
	dir := t.TempDir()

	var syncedDirs []string
	restore := raft.SetRaftSyncDirFnForTesting(func(dirPath string) error {
		syncedDirs = append(syncedDirs, dirPath)
		return nil
	})
	defer restore()

	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Verify that parent directory sync occurred on initial log creation
	if len(syncedDirs) == 0 {
		t.Fatalf("expected parent directory sync on initial log creation")
	}

	// Test failure injection during initial log creation directory sync
	t.Run("initial_log_dir_sync_failure", func(t *testing.T) {
		dirFail := t.TempDir()
		callCount := 0
		restoreFail := raft.SetRaftSyncDirFnForTesting(func(dirPath string) error {
			callCount++
			// Allow raft_state sync (call 1), but fail directory sync on initial raft.log creation (call 2)
			if callCount > 1 {
				return fmt.Errorf("injected dir sync error on log creation")
			}
			return nil
		})
		defer restoreFail()

		_, err = raft.OpenStorage(dirFail)
		if err == nil {
			t.Fatalf("expected OpenStorage to fail when directory sync fails during log creation")
		}
	})
}

func TestStorage_LogEntryTermRelationship(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Start at term 1
	if err := s.SetTerm(1); err != nil {
		t.Fatalf("SetTerm(1) failed: %v", err)
	}

	// 1. currentTerm = 1, append entry term 1 -> success
	e1 := raft.LogEntry{Index: 1, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("e1")}
	if err := s.Append(e1); err != nil {
		t.Fatalf("Append term 1 in term 1 failed: %v", err)
	}

	// 2. currentTerm = 1, append entry term 2 -> reject
	logPath := filepath.Join(dir, raft.LogFilename)
	fiBefore, _ := os.Stat(logPath)

	e2Invalid := raft.LogEntry{Index: 2, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("e2")}
	err = s.Append(e2Invalid)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftEntryTermExceedsCurrentTerm) {
		t.Fatalf("expected ErrRaftEntryTermExceedsCurrentTerm, got %v", err)
	}

	// Verify rejection happened before disk mutation
	fiAfter, _ := os.Stat(logPath)
	if fiBefore.Size() != fiAfter.Size() {
		t.Fatalf("disk was mutated on rejected entry: size before=%d, after=%d", fiBefore.Size(), fiAfter.Size())
	}
	lastIdx, _ := s.LastIndex()
	if lastIdx != 1 {
		t.Fatalf("lastIndex mutated on rejected entry: got %d, want 1", lastIdx)
	}

	// Advance term to 2
	if err := s.SetTerm(2); err != nil {
		t.Fatalf("SetTerm(2) failed: %v", err)
	}

	// 3. currentTerm = 2, append entry term 1 -> success
	e2Valid := raft.LogEntry{Index: 2, Term: 1, Type: transport.PeerEntryNormal, Data: []byte("e2")}
	if err := s.Append(e2Valid); err != nil {
		t.Fatalf("Append term 1 in term 2 failed: %v", err)
	}

	// 4. currentTerm = 2, append entry term 2 -> success
	e3Valid := raft.LogEntry{Index: 3, Term: 2, Type: transport.PeerEntryNormal, Data: []byte("e3")}
	if err := s.Append(e3Valid); err != nil {
		t.Fatalf("Append term 2 in term 2 failed: %v", err)
	}

	_ = s.Close()

	// Tamper state file: regress term back to 1 while log has entry with term 2
	// OpenStorage recovery must fail closed with ErrRaftCorruptedState
	statePath := filepath.Join(dir, raft.StateFilename)
	data, _ := os.ReadFile(statePath)
	binary.PutUint64(data[8:16], 1) // Set term to 1
	crc := binary.Checksum(data[0:raft.StatePayloadSize])
	binary.PutUint32(data[raft.StatePayloadSize:raft.StateRecordSize], crc)
	_ = os.WriteFile(statePath, data, 0600)

	_, err = raft.OpenStorage(dir)
	if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
		t.Fatalf("expected ErrRaftCorruptedState on recovered entry term > hard state term, got %v", err)
	}
}

func TestStorage_StateFileTrailingGarbage(t *testing.T) {
	// 1. Exact valid size (28 bytes) -> recovers OK
	t.Run("exact_valid_size", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		_ = s.SetTerm(3)
		_ = s.Close()

		s2, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage reopen failed: %v", err)
		}
		_ = s2.Close()
	})

	// 2. Valid record + 1 byte -> fails closed with ErrRaftCorruptedState
	t.Run("valid_record_plus_one_byte", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		_ = s.Close()

		statePath := filepath.Join(dir, raft.StateFilename)
		data, _ := os.ReadFile(statePath)
		data = append(data, 0x00) // 29 bytes
		_ = os.WriteFile(statePath, data, 0600)

		_, err = raft.OpenStorage(dir)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
			t.Fatalf("expected ErrRaftCorruptedState for state file + 1 byte, got %v", err)
		}
	})

	// 3. Valid record + random suffix -> fails closed with ErrRaftCorruptedState
	t.Run("valid_record_plus_random_suffix", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		_ = s.Close()

		statePath := filepath.Join(dir, raft.StateFilename)
		data, _ := os.ReadFile(statePath)
		data = append(data, []byte("unexpected_trailing_garbage_data!")...)
		_ = os.WriteFile(statePath, data, 0600)

		_, err = raft.OpenStorage(dir)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
			t.Fatalf("expected ErrRaftCorruptedState for state file with suffix, got %v", err)
		}
	})

	// 4. Truncated record -> fails closed with ErrRaftCorruptedState
	t.Run("truncated_record", func(t *testing.T) {
		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			t.Fatalf("OpenStorage failed: %v", err)
		}
		_ = s.Close()

		statePath := filepath.Join(dir, raft.StateFilename)
		data, _ := os.ReadFile(statePath)
		data = data[:15] // Only 15 bytes
		_ = os.WriteFile(statePath, data, 0600)

		_, err = raft.OpenStorage(dir)
		if err == nil || !stdErrors.Is(err, errors.ErrRaftCorruptedState) {
			t.Fatalf("expected ErrRaftCorruptedState for truncated state file, got %v", err)
		}
	})
}

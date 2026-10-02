package raft_test

import (
	"encoding/binary"
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
)

// =============================================================================
// HardState durability: terminal poisoning after a post-rename directory-sync
// failure (regression).
//
// writeStateFile renames raft_state.tmp over raft_state and only then syncs the
// parent directory. When that final sync failed, the function reported failure
// while the durable file had already been replaced. SetHardState returned without
// advancing its in-memory HardState, so memory and disk disagreed about the term.
// Because monotonicity is validated against MEMORY, a subsequent SetVote wrote a
// LOWER term than the one already durable (observed: 6 -> 5), leaving a node that
// would re-announce and vote in a term it had already abandoned.
// =============================================================================

// durableTerm reads the term straight out of raft_state, observing on-disk truth
// independently of any live Storage instance.
func durableTerm(t *testing.T, dir string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, raft.StateFilename))
	if err != nil {
		t.Fatalf("read raft_state: %v", err)
	}
	if len(raw) != raft.StateRecordSize {
		t.Fatalf("raft_state is %d bytes; want %d", len(raw), raft.StateRecordSize)
	}
	return binary.BigEndian.Uint64(raw[8:16])
}

// durableVotedFor reads the votedFor field straight out of raft_state.
func durableVotedFor(t *testing.T, dir string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, raft.StateFilename))
	if err != nil {
		t.Fatalf("read raft_state: %v", err)
	}
	return binary.BigEndian.Uint64(raw[16:24])
}

// failOnceDirSync arms exactly one parent-directory sync failure.
func failOnceDirSync(t *testing.T) {
	t.Helper()
	armed := false
	restore := raft.SetRaftSyncDirFnForTesting(func(string) error {
		if !armed {
			armed = true
			return os.ErrInvalid
		}
		return nil
	})
	t.Cleanup(restore)
}

// TestHardState_DirSyncFailureCannotRegressDurableTerm is the primary regression.
//
// Pre-fix, the durable term went 6 -> 5 after a single SetVote.
func TestHardState_DirSyncFailureCannotRegressDurableTerm(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	if err := s.SetTerm(5); err != nil {
		t.Fatalf("SetTerm(5): %v", err)
	}
	if got := durableTerm(t, dir); got != 5 {
		t.Fatalf("durable term = %d; want 5", got)
	}

	failOnceDirSync(t)

	// The rename commits term 6; the directory sync then fails.
	if err := s.SetTerm(6); err == nil {
		t.Fatal("expected the injected directory-sync failure to surface")
	}
	if !s.IsPoisoned() {
		t.Fatal("Storage is not poisoned after a post-rename directory-sync failure")
	}
	if got := durableTerm(t, dir); got != 6 {
		t.Fatalf("durable term = %d; the rename should already have committed 6", got)
	}

	// The regression vector. Every subsequent write must fail closed.
	if err := s.SetVote(3); !stderrors.Is(err, latticeerrors.ErrRaftStoragePoisoned) {
		t.Errorf("SetVote after poisoning returned %v; want ErrRaftStoragePoisoned", err)
	}
	if err := s.SetTerm(7); !stderrors.Is(err, latticeerrors.ErrRaftStoragePoisoned) {
		t.Errorf("SetTerm after poisoning returned %v; want ErrRaftStoragePoisoned", err)
	}

	// The durable term must still be 6, never lower.
	if got := durableTerm(t, dir); got != 6 {
		t.Errorf("durable term = %d; must never regress below the committed 6", got)
	}

	if err := s.Close(); !stderrors.Is(err, latticeerrors.ErrRaftStoragePoisoned) {
		t.Errorf("Close returned %v; want the terminal poison error", err)
	}
}

// TestHardState_PreRenameFailureStaysRecoverable is the counterpart: a failure
// BEFORE the rename must leave the Storage fully usable. Poisoning that case
// would turn an ordinary transient error into a terminal node failure.
func TestHardState_PreRenameFailureStaysRecoverable(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetTerm(3); err != nil {
		t.Fatalf("SetTerm(3): %v", err)
	}

	// Fail the staging write, which happens well before the rename.
	restore := raft.SetRaftWriteStateTmpFnForTesting(func(f *os.File, b []byte) (int, error) {
		return 0, os.ErrPermission
	})
	err = s.SetTerm(4)
	restore()

	if err == nil {
		t.Fatal("expected the injected staging-write failure to surface")
	}
	if s.IsPoisoned() {
		t.Error("Storage poisoned on a pre-rename failure; nothing was committed, so it must stay usable")
	}
	if got := durableTerm(t, dir); got != 3 {
		t.Errorf("durable term = %d; want 3 (unchanged)", got)
	}

	// Must still be fully operational.
	if err := s.SetTerm(4); err != nil {
		t.Fatalf("SetTerm(4) after a recoverable failure: %v", err)
	}
	if got := durableTerm(t, dir); got != 4 {
		t.Errorf("durable term = %d; want 4", got)
	}
}

// TestHardState_RenameFailureStaysRecoverable covers the other pre-commit step.
func TestHardState_RenameFailureStaysRecoverable(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetTerm(3); err != nil {
		t.Fatalf("SetTerm(3): %v", err)
	}

	restore := raft.SetRaftRenameFnForTesting(func(oldpath, newpath string) error {
		return os.ErrPermission
	})
	err = s.SetTerm(4)
	restore()

	if err == nil {
		t.Fatal("expected the injected rename failure to surface")
	}
	if s.IsPoisoned() {
		t.Error("Storage poisoned on a rename failure; the durable file was never replaced")
	}
	if got := durableTerm(t, dir); got != 3 {
		t.Errorf("durable term = %d; want 3 (unchanged)", got)
	}
	if err := s.SetTerm(4); err != nil {
		t.Fatalf("SetTerm(4) after a recoverable rename failure: %v", err)
	}
}

// TestHardState_StagingSyncFailureStaysRecoverable covers the staging fsync,
// which also precedes the rename.
func TestHardState_StagingSyncFailureStaysRecoverable(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetTerm(2); err != nil {
		t.Fatalf("SetTerm(2): %v", err)
	}

	restore := raft.SetRaftSyncStateTmpFnForTesting(func(*os.File) error {
		return os.ErrInvalid
	})
	err = s.SetTerm(3)
	restore()

	if err == nil {
		t.Fatal("expected the injected staging-sync failure to surface")
	}
	if s.IsPoisoned() {
		t.Error("Storage poisoned on a staging-sync failure; the rename never happened")
	}
	if got := durableTerm(t, dir); got != 2 {
		t.Errorf("durable term = %d; want 2 (unchanged)", got)
	}
}

// TestHardState_DurableVoteSurvivesPoisoning verifies the poison decision does not
// corrupt the vote record: the rename committed, so the vote is durable and must
// be honored after recovery.
func TestHardState_DurableVoteSurvivesPoisoning(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	if err := s.SetTerm(4); err != nil {
		t.Fatalf("SetTerm(4): %v", err)
	}
	if err := s.SetVote(2); err != nil {
		t.Fatalf("SetVote(2): %v", err)
	}
	if got := durableVotedFor(t, dir); got != 2 {
		t.Fatalf("durable votedFor = %d; want 2", got)
	}

	failOnceDirSync(t)
	_ = s.SetTerm(5)
	_ = s.Close()

	// Recovery must observe the committed term and the committed vote.
	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("reopen after poisoned hard state write: %v", err)
	}
	defer func() { _ = s2.Close() }()

	hs, err := s2.HardState()
	if err != nil {
		t.Fatalf("HardState: %v", err)
	}
	if hs.Term != 5 {
		t.Errorf("recovered term = %d; want 5", hs.Term)
	}
	// VotedFor is reset by a term advance, so it must be nil, and the node must
	// not remember a stale vote from term 4.
	if hs.VotedFor != 0 {
		t.Errorf("recovered votedFor = %d; want 0 after a term advance", hs.VotedFor)
	}
	if s2.IsPoisoned() {
		t.Error("a freshly opened Storage must not be poisoned")
	}
}

// TestHardState_ColdBootStillPersistsInitialState guards the OpenStorage cold-boot
// call site, whose signature also changed.
func TestHardState_ColdBootStillPersistsInitialState(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("cold OpenStorage: %v", err)
	}
	defer func() { _ = s.Close() }()

	hs, err := s.HardState()
	if err != nil {
		t.Fatalf("HardState: %v", err)
	}
	if hs.Term != 0 || hs.VotedFor != 0 {
		t.Errorf("cold-boot HardState = %+v; want zero value", hs)
	}
	if got := durableTerm(t, dir); got != 0 {
		t.Errorf("cold-boot durable term = %d; want 0", got)
	}
}

// TestHardState_HealthyWritesUnaffected is the no-regression control.
func TestHardState_HealthyWritesUnaffected(t *testing.T) {
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}

	for term := raft.Term(1); term <= 10; term++ {
		if err := s.SetTerm(term); err != nil {
			t.Fatalf("SetTerm(%d): %v", term, err)
		}
		if err := s.SetVote(cluster.NodeID(term%3 + 1)); err != nil {
			t.Fatalf("SetVote at term %d: %v", term, err)
		}
	}
	if s.IsPoisoned() {
		t.Fatal("healthy writes poisoned the Storage")
	}
	if got := durableTerm(t, dir); got != 10 {
		t.Errorf("durable term = %d; want 10", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

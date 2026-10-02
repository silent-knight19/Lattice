package raft_test

import (
	stderrors "errors"
	"os"
	"testing"

	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// =============================================================================
// Storage poisoning on append write / durability-barrier failure (regression).
//
// Storage.Append wrote bytes into an O_APPEND descriptor and then failed the
// fdatasync barrier, leaving the on-disk log ahead of memLog with no way to
// resynchronize them. A caller retry then wrote the same index twice, and
// recovery rejected the non-contiguous sequence, permanently bricking the Raft
// directory. The retry even reported success, so the follower looked healthy.
// =============================================================================

func entry(idx raft.LogIndex, term raft.Term, data string) raft.LogEntry {
	return raft.LogEntry{Index: idx, Term: term, Type: transport.PeerEntryNormal, Data: []byte(data)}
}

func seededStorage(t *testing.T, dir string, n int) (*raft.Storage, raft.Term) {
	t.Helper()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage: %v", err)
	}
	const term = raft.Term(1)
	if err := s.SetTerm(term); err != nil {
		t.Fatalf("SetTerm: %v", err)
	}
	for i := 1; i <= n; i++ {
		if err := s.Append(entry(raft.LogIndex(i), term, "e")); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	return s, term
}

// failOnceSync arms the log durability barrier to fail exactly one time.
func failOnceSync(t *testing.T) {
	t.Helper()
	armed := false
	restore := raft.SetRaftSyncLogFnForTesting(func(f *os.File) error {
		if !armed {
			armed = true
			return os.ErrInvalid
		}
		return f.Sync()
	})
	t.Cleanup(restore)
}

// TestStoragePoison_AppendFsyncFailureDoesNotBrick is the primary regression.
//
// Pre-fix, the retry below returned nil and the directory became permanently
// unopenable with "index gap at offset N: expected M, got M-1".
func TestStoragePoison_AppendFsyncFailureDoesNotBrick(t *testing.T) {
	dir := t.TempDir()
	s, term := seededStorage(t, dir, 3)
	failOnceSync(t)

	if err := s.Append(entry(4, term, "e4")); err == nil {
		t.Fatal("expected the injected durability-barrier failure to surface")
	}
	if !s.IsPoisoned() {
		t.Fatal("Storage is not poisoned after a failed durability barrier")
	}

	// The realistic Raft retry must now fail closed instead of duplicating.
	retryErr := s.Append(entry(4, term, "e4"))
	if retryErr == nil {
		t.Fatal("retry after a poisoned append returned nil; it would duplicate the index")
	}
	if !stderrors.Is(retryErr, latticeerrors.ErrRaftStoragePoisoned) {
		t.Errorf("retry returned %v; want errors.ErrRaftStoragePoisoned", retryErr)
	}
	if stderrors.Is(retryErr, latticeerrors.ErrRaftStateClosed) {
		t.Error("poison surfaced as ErrRaftStateClosed; callers would report a clean shutdown")
	}

	closeErr := s.Close()
	if !stderrors.Is(closeErr, latticeerrors.ErrRaftStoragePoisoned) {
		t.Errorf("Close returned %v; want the terminal poison error so a clean shutdown "+
			"is not reported over a data-integrity event", closeErr)
	}

	// The directory must still be openable with a contiguous log. The orphan
	// record from the failed attempt is itself CRC-valid and contiguous, so
	// recovery legitimately adopts it as index 4; what matters is that recovery
	// succeeds and the log has no duplicate or gap.
	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("reopen after a poisoned append failed: %v; the directory must remain recoverable", err)
	}

	recovered, _, err := s2.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm after reopen: %v", err)
	}
	if recovered < 3 {
		t.Errorf("recovered LastIndex = %d; the committed prefix must survive", recovered)
	}
	// The log must be usable from wherever it actually ends.
	if err := s2.Append(entry(recovered+1, term, "e-next")); err != nil {
		t.Fatalf("Append(%d) on a fresh Storage over the same directory: %v", recovered+1, err)
	}
	want := recovered + 1
	if err := s2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s3, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("final reopen failed: %v", err)
	}
	defer func() { _ = s3.Close() }()
	if got, _, err := s3.LastIndexAndTerm(); err != nil || got != want {
		t.Errorf("final recovered LastIndex = %d (err %v); want %d", got, err, want)
	}
}

// TestStoragePoison_AppendWriteFailurePoisons covers the write step, not just the
// barrier. A short or failed write leaves a partial record in the file.
func TestStoragePoison_AppendWriteFailurePoisons(t *testing.T) {
	dir := t.TempDir()
	s, term := seededStorage(t, dir, 2)

	restore := raft.SetRaftWriteLogFnForTesting(func(f *os.File, b []byte) (int, error) {
		return 0, os.ErrPermission
	})
	defer restore()

	if err := s.Append(entry(3, term, "e3")); err == nil {
		t.Fatal("expected the injected write failure to surface")
	}
	if !s.IsPoisoned() {
		t.Error("Storage is not poisoned after a failed append write")
	}
	if err := s.Append(entry(3, term, "e3")); !stderrors.Is(err, latticeerrors.ErrRaftStoragePoisoned) {
		t.Errorf("post-poison Append returned %v; want ErrRaftStoragePoisoned", err)
	}
}

// TestStoragePoison_AllMutatorsFailClosed verifies no mutator can still write
// after poisoning. A single surviving mutator would reintroduce divergence.
func TestStoragePoison_AllMutatorsFailClosed(t *testing.T) {
	dir := t.TempDir()
	s, term := seededStorage(t, dir, 2)
	failOnceSync(t)

	if err := s.Append(entry(3, term, "e3")); err == nil {
		t.Fatal("expected the injected failure to surface")
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"Append", func() error { return s.Append(entry(3, term, "e3")) }},
		{"TruncateSuffix", func() error { return s.TruncateSuffix(2) }},
		{"SetHardState", func() error {
			return s.SetHardState(raft.HardState{Term: term + 1, VotedFor: 1})
		}},
		{"SetTerm (advance)", func() error { return s.SetTerm(term + 1) }},
		{"SetTerm (no-op advance)", func() error { return s.SetTerm(term) }},
		{"SetVote", func() error { return s.SetVote(1) }},
	}
	for _, tc := range cases {
		err := tc.call()
		if !stderrors.Is(err, latticeerrors.ErrRaftStoragePoisoned) {
			t.Errorf("%s after poisoning returned %v; want errors.ErrRaftStoragePoisoned", tc.name, err)
		}
	}
}

// TestStoragePoison_IsIdempotentAndReportsCause verifies the first root-cause
// error is preserved and that repeated observation is stable.
func TestStoragePoison_IsIdempotentAndReportsCause(t *testing.T) {
	dir := t.TempDir()
	s, term := seededStorage(t, dir, 1)

	armed := 0
	restore := raft.SetRaftSyncLogFnForTesting(func(f *os.File) error {
		armed++
		return os.ErrInvalid
	})
	defer restore()

	_ = s.Append(entry(2, term, "e2"))
	_ = s.Append(entry(2, term, "e2"))

	var pe *latticeerrors.RaftStoragePoisonedError
	err := s.Append(entry(2, term, "e2"))
	if !stderrors.As(err, &pe) {
		t.Fatalf("Append returned %T; want *errors.RaftStoragePoisonedError", err)
	}
	if pe.Op == "" {
		t.Error("poison error does not name the failing operation")
	}
	if pe.Unwrap() == nil {
		t.Error("poison error does not wrap the underlying io cause")
	}
	if armed != 1 {
		t.Errorf("durability barrier was invoked %d times; must not be retried after poisoning", armed)
	}
}

// TestStoragePoison_ClosedStorageStillReportsClosed confirms the poison change did
// not blur the ordinary close path: a cleanly closed Storage reports
// ErrRaftStateClosed, not a poison error.
func TestStoragePoison_ClosedStorageStillReportsClosed(t *testing.T) {
	dir := t.TempDir()
	s, _ := seededStorage(t, dir, 2)

	if err := s.Close(); err != nil {
		t.Fatalf("clean Close returned %v; want nil", err)
	}
	if err := s.SetTerm(5); !stderrors.Is(err, latticeerrors.ErrRaftStateClosed) {
		t.Errorf("SetTerm after clean Close returned %v; want ErrRaftStateClosed", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close returned %v; want nil", err)
	}
	if s.IsPoisoned() {
		t.Error("a cleanly closed Storage must not report poisoned")
	}
}

// TestStoragePoison_HealthyAppendsUnaffected is the no-regression control: with
// no injected failure, the new gates and seams must be completely transparent.
func TestStoragePoison_HealthyAppendsUnaffected(t *testing.T) {
	dir := t.TempDir()
	s, term := seededStorage(t, dir, 1)

	for i := 2; i <= 20; i++ {
		if err := s.Append(entry(raft.LogIndex(i), term, "e")); err != nil {
			t.Fatalf("healthy Append %d: %v", i, err)
		}
	}
	if s.IsPoisoned() {
		t.Fatal("healthy appends poisoned the Storage")
	}
	last, lastTerm, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm: %v", err)
	}
	if last != 20 || lastTerm != term {
		t.Errorf("LastIndexAndTerm = (%d, %d); want (20, %d)", last, lastTerm, term)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if l, _, err := s2.LastIndexAndTerm(); err != nil || l != 20 {
		t.Errorf("recovered LastIndex = %d (err %v); want 20", l, err)
	}
}

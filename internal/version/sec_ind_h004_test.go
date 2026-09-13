package version_test

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/version"
)

// h004FileMetadata builds a valid FileMetadata with properly encoded internal keys.
func h004FileMetadata(t *testing.T, fileNum uint64, smallest, largest string) version.FileMetadata {
	t.Helper()
	sk, err := binary.NewInternalKey([]byte(smallest), binary.SeqNum(10), binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey(smallest) failed: %v", err)
	}
	lk, err := binary.NewInternalKey([]byte(largest), binary.SeqNum(1), binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey(largest) failed: %v", err)
	}
	return version.FileMetadata{
		FileNum:        fileNum,
		FileSize:       4096,
		SmallestKey:    binary.EncodeInternalKey(sk),
		LargestKey:     binary.EncodeInternalKey(lk),
		SmallestSeqNum: 1,
		LargestSeqNum:  10,
	}
}

// TestINDH004_HeldFileSnapshotSurvivesSupersede proves the IND-H-004 verdict:
// there is no iterator-invalidation callback because none is needed — the pin
// IS the invalidation guard. A reader that pins Current() and snapshots
// Files() keeps fully valid metadata across supersede + set-side finalize,
// since cleanup cannot run while the pin is held.
func TestINDH004_HeldFileSnapshotSurvivesSupersede(t *testing.T) {
	vs := version.NewVersionSet()

	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{h004FileMetadata(t, 101, "a", "m")}
	v1 := version.NewVersion(levels)
	cleaned := 0
	v1.SetCleanupFnForTesting(func() { cleaned++ })
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}

	// Reader opens its "iterator": pin + snapshot, exactly as engine reads do.
	it := vs.Current()
	if it == nil {
		t.Fatalf("Current() returned nil")
	}
	snapshot := it.Files(0)
	if len(snapshot) != 1 || snapshot[0].FileNum != 101 {
		t.Fatalf("snapshot = %+v, want single file 101", snapshot)
	}

	// Writer burst supersedes v1 twice (flush + compaction), dropping the
	// set's references — the finalize window from the audit scenario.
	for i := 0; i < 2; i++ {
		var lv [version.NumLevels][]version.FileMetadata
		lv[1] = []version.FileMetadata{h004FileMetadata(t, 200+uint64(i), "a", "z")}
		if err := vs.AppendVersion(version.NewVersion(lv)); err != nil {
			it.Unref()
			t.Fatalf("AppendVersion failed: %v", err)
		}
	}

	// The held snapshot must be fully intact — no invalidation lost.
	if len(snapshot) != 1 {
		t.Fatalf("snapshot length changed across supersede: %d", len(snapshot))
	}
	if snapshot[0].FileNum != 101 || snapshot[0].FileSize != 4096 {
		t.Errorf("snapshot scalar corrupted: %+v", snapshot[0])
	}
	sk, err := binary.DecodeInternalKey(snapshot[0].SmallestKey)
	if err != nil {
		t.Fatalf("snapshot smallest key undecodable: %v", err)
	}
	if !bytes.Equal(sk.UserKey, []byte("a")) {
		t.Errorf("snapshot smallest key = %q, want %q", sk.UserKey, "a")
	}
	if cleaned != 0 {
		t.Errorf("cleanup ran %d times while iterator pin held (want 0)", cleaned)
	}

	// Mutating the returned slice must not corrupt the version (defensive copy).
	snapshot[0].FileNum = 999
	if again := it.Files(0); again[0].FileNum != 101 {
		t.Errorf("Files() not defensively copied: FileNum = %d", again[0].FileNum)
	}

	it.Unref()
	if cleaned != 1 {
		t.Errorf("cleanup ran %d times after iterator release (want exactly 1)", cleaned)
	}
}

// TestINDH004_UnpinnedUseAfterSupersedeReclaimsDeterministically documents the
// mandatory side of the discipline: a version used WITHOUT a pin is reclaimed
// deterministically on supersede (levels cleared, cleanup once) — never left
// as a dangling iterator over freed memory. Callers must pin; the garbage
// collector then makes any residual pointer safe to retain but empty.
func TestINDH004_UnpinnedUseAfterSupersedeReclaimsDeterministically(t *testing.T) {
	vs := version.NewVersionSet()

	var levels [version.NumLevels][]version.FileMetadata
	levels[0] = []version.FileMetadata{h004FileMetadata(t, 101, "a", "m")}
	v1 := version.NewVersion(levels)
	cleaned := 0
	v1.SetCleanupFnForTesting(func() { cleaned++ })
	if err := vs.AppendVersion(v1); err != nil {
		t.Fatalf("AppendVersion(v1) failed: %v", err)
	}
	// Deliberately take NO pin — the audit's buggy caller pattern.

	var lv [version.NumLevels][]version.FileMetadata
	lv[0] = []version.FileMetadata{h004FileMetadata(t, 102, "a", "m")}
	if err := vs.AppendVersion(version.NewVersion(lv)); err != nil {
		t.Fatalf("AppendVersion(v2) failed: %v", err)
	}

	// Reclamation is immediate and exactly-once — observable, never dangling.
	if cleaned != 1 {
		t.Fatalf("cleanup ran %d times on unpinned supersede (want exactly 1)", cleaned)
	}
	if n := v1.NumFiles(0); n != 0 {
		t.Errorf("unpinned superseded version still reports %d files (want 0, reclaimed)", n)
	}
	if vs.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 (dead version unlinked)", vs.ActiveCount())
	}
}

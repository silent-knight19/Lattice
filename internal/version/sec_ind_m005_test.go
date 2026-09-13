package version_test

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	stdErrors "errors"

	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// m005WalkLog parses the raw MANIFEST framing and returns the NextFileNum
// watermark of every CRC-valid record, failing on any torn framing.
func m005WalkLog(t *testing.T, path string) []uint64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	var marks []uint64
	off := 0
	for off < len(raw) {
		if len(raw)-off < version.ManifestHeaderSize {
			t.Fatalf("torn framing at offset %d: %d bytes remain", off, len(raw)-off)
		}
		wantCRC := binary.BigEndian.Uint32(raw[off : off+4])
		length := binary.BigEndian.Uint32(raw[off+4 : off+8])
		if uint64(len(raw)-off-8) < uint64(length) {
			t.Fatalf("torn payload at offset %d: need %d, have %d", off, length, len(raw)-off-8)
		}
		if got := crc32.ChecksumIEEE(raw[off+4 : off+8+int(length)]); got != wantCRC {
			t.Fatalf("record at offset %d fails CRC: got %08x want %08x", off, got, wantCRC)
		}
		edit, err := version.DecodeVersionEdit(raw[off+8 : off+8+int(length)])
		if err != nil {
			t.Fatalf("record at offset %d undecodable: %v", off, err)
		}
		num, ok := edit.NextFileNum()
		if !ok {
			t.Fatalf("record at offset %d missing NextFileNum watermark", off)
		}
		marks = append(marks, num)
		off += version.ManifestHeaderSize + int(length)
	}
	return marks
}

func m005WatermarkedEdit(num uint64) version.VersionEdit {
	edit := version.NewVersionEdit()
	edit.SetNextFileNum(num)
	return *edit
}

// TestINDM005_LogIsCompletePrefixAcrossRestart proves the IND-M-005
// foundation: the durable log is always a complete, ordered, CRC-verified
// prefix across close/reopen cycles. A crash between a MANIFEST append and
// the in-memory VersionSet update therefore leaves recovery a well-defined
// job — replay the complete prefix from the last applied watermark — rather
// than a torn log.
func TestINDM005_LogIsCompletePrefixAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := version.CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	for i := uint64(1); i <= 5; i++ {
		if err := w.LogEdit(m005WatermarkedEdit(i)); err != nil {
			t.Fatalf("LogEdit %d failed: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Simulate restart: reopen must preserve the prefix non-destructively.
	w2, err := version.OpenManifestWriter(path)
	if err != nil {
		t.Fatalf("OpenManifestWriter failed: %v", err)
	}
	if err := w2.LogEdit(m005WatermarkedEdit(6)); err != nil {
		t.Fatalf("post-reopen LogEdit failed: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	marks := m005WalkLog(t, path)
	if len(marks) != 6 {
		t.Fatalf("walked %d records, want 6 (complete prefix)", len(marks))
	}
	for i, m := range marks {
		if m != uint64(i+1) {
			t.Fatalf("watermark[%d] = %d, want %d (ordered prefix)", i, m, i+1)
		}
	}
}

// TestINDM005_FailedAppendLeavesCompletePrefix proves the crash-window bound:
// the write-all loop finishes before the sync barrier, so a barrier failure
// poisons the writer fail-closed while the record bytes remain fully framed
// on disk. The log therefore still walks as a COMPLETE prefix (4 ordered,
// CRC-valid records — never a torn half-record), and no divergent suffix can
// follow because the poisoned writer rejects everything after. Replay converges
// by re-applying from the watermark; the failed record is simply replayed too.
func TestINDM005_FailedAppendLeavesCompletePrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")

	w, err := version.CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	for i := uint64(1); i <= 3; i++ {
		if err := w.LogEdit(m005WatermarkedEdit(i)); err != nil {
			t.Fatalf("LogEdit %d failed: %v", i, err)
		}
	}

	injected := stdErrors.New("simulated IND-M-005 barrier failure")
	w.SetSyncFnForTesting(func(f *os.File) error {
		return injected
	})
	if err := w.LogEdit(m005WatermarkedEdit(4)); err == nil {
		t.Fatalf("failed-barrier LogEdit unexpectedly succeeded")
	} else if !stdErrors.Is(err, injected) {
		t.Fatalf("want root cause %v, got %v", injected, err)
	}
	// Poisoned writer must fail closed on all subsequent appends: no suffix.
	if err := w.LogEdit(m005WatermarkedEdit(5)); err == nil {
		t.Fatalf("poisoned writer accepted an edit")
	} else if !stdErrors.Is(err, latticeErrors.ErrManifestWriterPoisoned) {
		t.Fatalf("want poison rejection, got %v", err)
	}

	// The log walks as a complete 4-record prefix — framed, ordered, CRC-valid.
	marks := m005WalkLog(t, path)
	if len(marks) != 4 {
		t.Fatalf("walked %d records, want 4 (complete prefix, never torn)", len(marks))
	}
	for i, m := range marks {
		if m != uint64(i+1) {
			t.Fatalf("watermark[%d] = %d, want %d", i, m, i+1)
		}
	}
	_ = w.Close() // poisoned close still releases the descriptor
}

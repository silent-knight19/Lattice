package sstable_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	stdErrors "errors"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// m003FirstDataBlock builds a single-block SSTable and returns a copy of its
// first data block plus a known-present user key.
func m003FirstDataBlock(t *testing.T) (block []byte, presentKey []byte) {
	t.Helper()
	dir := t.TempDir()
	dst := filepath.Join(dir, "restart.sst")

	w, err := sstable.NewTableWriter(dst, sstable.DefaultTableWriterOptions())
	if err != nil {
		t.Fatalf("NewTableWriter failed: %v", err)
	}
	for i := 0; i < 20; i++ {
		ik, err := binary.NewInternalKey(
			[]byte(fmt.Sprintf("key:%04d", i)),
			binary.SeqNum(uint64(100-i)),
			binary.OpTypePut,
		)
		if err != nil {
			t.Fatalf("NewInternalKey failed: %v", err)
		}
		if err := w.Add(ik, []byte(fmt.Sprintf("val:%04d", i))); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	_ = w.Close()

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	footer, err := sstable.DecodeFooter(raw[len(raw)-sstable.FooterSize:])
	if err != nil {
		t.Fatalf("DecodeFooter failed: %v", err)
	}
	indexBlock := raw[footer.IndexHandle.Offset : footer.IndexHandle.Offset+footer.IndexHandle.Size]
	idx, err := sstable.DecodeBlockIndex(indexBlock)
	if err != nil {
		t.Fatalf("DecodeBlockIndex failed: %v", err)
	}
	entries := idx.Entries()
	if len(entries) == 0 {
		t.Fatalf("index has no entries")
	}
	h := entries[0].Handle
	block = append([]byte(nil), raw[h.Offset:h.Offset+h.Size]...)
	return block, []byte("key:0005")
}

// m003Search runs searchDataBlock with a panic guard: corruption must produce
// errors, never panics.
func m003Search(t *testing.T, block, key []byte) (val []byte, err error) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("searchDataBlock panicked on corrupted block (IND-M-003): %v", rec)
		}
	}()
	return sstable.SearchDataBlockForTesting(block, key, 0)
}

// m003RefixCRC recomputes the trailing CRC after a surgical corruption so the
// test exercises restart validation rather than the checksum gate.
func m003RefixCRC(block []byte) {
	binary.PutUint32(block[len(block)-4:], binary.Checksum(block[:len(block)-4]))
}

// TestINDM003_RestartCorruptionFailClosed proves the IND-M-003 verdict: a
// corrupted restart point cannot send the parser skipping into adjacent
// structures. Each corruption class is rejected with a structured error.
func TestINDM003_RestartCorruptionFailClosed(t *testing.T) {
	block, key := m003FirstDataBlock(t)

	// Control: uncorrupted block resolves the key.
	if _, err := m003Search(t, append([]byte(nil), block...), key); err != nil {
		t.Fatalf("control lookup failed: %v", err)
	}

	t.Run("ZeroRestartCount", func(t *testing.T) {
		bad := append([]byte(nil), block...)
		binary.PutUint32(bad[len(bad)-8:len(bad)-4], 0)
		m003RefixCRC(bad)
		_, err := m003Search(t, bad, key)
		if err == nil {
			t.Fatalf("accepted block with zero restart count")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("want ErrDataBlockCorrupted, got %T (%v)", err, err)
		}
	})

	t.Run("FirstRestartOffsetNonZero", func(t *testing.T) {
		bad := append([]byte(nil), block...)
		restartCount := binary.GetUint32(bad[len(bad)-8 : len(bad)-4])
		if restartCount == 0 {
			t.Fatalf("control block has zero restarts; test vacuous")
		}
		offsetsStart := len(bad) - 8 - int(restartCount)*4
		binary.PutUint32(bad[offsetsStart:offsetsStart+4], 1)
		m003RefixCRC(bad)
		_, err := m003Search(t, bad, key)
		if err == nil {
			t.Fatalf("accepted block with first restart offset != 0")
		}
		if !stdErrors.Is(err, errors.ErrDataBlockCorrupted) {
			t.Fatalf("want ErrDataBlockCorrupted, got %T (%v)", err, err)
		}
	})

	t.Run("TornWriteDetectedByCRC", func(t *testing.T) {
		bad := append([]byte(nil), block...)
		bad[0] ^= 0xFF // single-bit flip in entry data, CRC left stale
		_, err := m003Search(t, bad, key)
		if err == nil {
			t.Fatalf("accepted torn block with stale CRC")
		}
		if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
			t.Fatalf("want ErrChecksumMismatch, got %T (%v)", err, err)
		}
	})
}

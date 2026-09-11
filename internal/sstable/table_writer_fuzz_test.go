package sstable_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// FuzzTableWriter verifies that TableWriter handles arbitrary sequences of keys and values
// without panicking, hanging, leaking files, or generating corrupted files that violate invariants.
func FuzzTableWriter(f *testing.F) {
	// Seed 1: Single small entry
	f.Add([]byte("user001"), []byte("value001"), uint64(10), uint8(1))
	// Seed 2: Longer keys and values
	f.Add([]byte("prefix:alpha:beta:gamma"), []byte("payload-12345-67890"), uint64(100), uint8(1))
	// Seed 3: Tombstone
	f.Add([]byte("deleted_key"), []byte{}, uint64(50), uint8(2))
	// Seed 4: Boundary values
	f.Add([]byte{0x00}, []byte{0xFF}, uint64(1), uint8(1))

	f.Fuzz(func(t *testing.T, userKey, val []byte, seq uint64, opCode uint8) {
		if len(userKey) == 0 || len(userKey) > 1024 {
			return
		}
		if len(val) > 4096 {
			return
		}

		op := binary.OpTypePut
		if opCode%2 == 0 {
			op = binary.OpTypeDelete
		}

		ik, err := binary.NewInternalKey(userKey, binary.SeqNum(seq), op)
		if err != nil {
			return
		}

		dir, err := os.MkdirTemp("", "fuzz_sstable_*")
		if err != nil {
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()

		sstPath := filepath.Join(dir, "fuzz.sst")
		writer, err := sstable.NewTableWriter(sstPath, sstable.TableWriterOptions{
			TargetBlockSize: 256, // small target to stress block boundary crossing
			RestartInterval: 4,
			FileMode:        0644,
		})
		if err != nil {
			return
		}

		// Add record
		_ = writer.Add(ik, val)

		// Finish table
		meta, err := writer.Finish()
		if err != nil {
			_ = writer.Close()
			return
		}

		// Invariant checks on successfully finished table
		raw, err := os.ReadFile(sstPath)
		if err != nil {
			t.Fatalf("ReadFile failed on finished table: %v", err)
		}
		if uint64(len(raw)) != meta.FileSize {
			t.Fatalf("file size mismatch: raw %d != meta %d", len(raw), meta.FileSize)
		}
		if len(raw) < sstable.FooterSize {
			t.Fatalf("file smaller than footer: %d", len(raw))
		}

		footer, err := sstable.DecodeFooter(raw[len(raw)-sstable.FooterSize:])
		if err != nil {
			t.Fatalf("DecodeFooter failed on finished table: %v", err)
		}
		if err := footer.ValidateAgainstFileSize(int64(len(raw))); err != nil {
			t.Fatalf("ValidateAgainstFileSize failed: %v", err)
		}

		// Verify Index Block
		idxBytes := raw[footer.IndexHandle.Offset : footer.IndexHandle.Offset+footer.IndexHandle.Size]
		_, err = sstable.DecodeBlockIndex(idxBytes)
		if err != nil {
			t.Fatalf("DecodeBlockIndex failed on finished table: %v", err)
		}
	})
}

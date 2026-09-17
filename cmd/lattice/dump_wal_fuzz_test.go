package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// FuzzDumpWAL fuzz tests DumpWAL against arbitrary mutated byte sequences
// to ensure zero panics, bounded execution, and safe failure termination.
func FuzzDumpWAL(f *testing.F) {
	// Seed 1: Empty byte slice
	f.Add([]byte{})

	// Seed 2: Too small for header (< 21 bytes)
	f.Add([]byte("short_header"))

	// Seed 3: Exactly 21 zero bytes
	f.Add(make([]byte, wal.HeaderSize))

	// Seed 4: Exactly 27 zero bytes (MinRecordSize)
	f.Add(make([]byte, wal.MinRecordSize))

	// Seed 5: Valid PUT record
	putRec := wal.Record{
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(1),
		Timestamp: 1000,
		Key:       []byte("fuzz_key"),
		Value:     []byte("fuzz_value"),
	}
	if putBytes, err := wal.EncodeRecord(putRec); err == nil {
		f.Add(putBytes)
	}

	// Seed 6: Valid DELETE record
	delRec := wal.Record{
		Type:      wal.RecordTypeDelete,
		SeqNum:    binary.SeqNum(2),
		Timestamp: 2000,
		Key:       []byte("fuzz_delete_key"),
		Value:     nil,
	}
	if delBytes, err := wal.EncodeRecord(delRec); err == nil {
		f.Add(delBytes)
	}

	// Seed 7: Valid Batch markers
	batchStartRec := wal.Record{
		Type:      wal.RecordTypeBatchStart,
		SeqNum:    binary.SeqNum(3),
		Timestamp: 3000,
	}
	if bStartBytes, err := wal.EncodeRecord(batchStartRec); err == nil {
		f.Add(bStartBytes)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		tempDir := t.TempDir()
		testPath := filepath.Join(tempDir, "fuzz.log")

		if err := os.WriteFile(testPath, data, 0600); err != nil {
			t.Fatalf("failed to write fuzz file: %v", err)
		}

		// DumpWAL must never panic under any arbitrary input bytes
		code := DumpWAL(testPath, true, io.Discard, io.Discard)
		if code != ExitDumpSuccess && code != ExitDumpCorruptError {
			t.Fatalf("unexpected exit code %d on fuzz data", code)
		}
	})
}

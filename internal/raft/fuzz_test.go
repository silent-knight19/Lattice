package raft_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/raft"
)

func FuzzRecoverStorage(f *testing.F) {
	// Seed 1: Empty state
	f.Add([]byte{})

	// Seed 2: Minimal valid state record (28 bytes)
	seedState := make([]byte, raft.StateRecordSize)
	f.Add(seedState)

	// Seed 3: Arbitrary random sequence
	f.Add([]byte("random corrupt bytes with headers and invalid sizes"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1024*1024 { // Bound memory to 1 MiB
			return
		}

		dir := t.TempDir()

		// Test state file recovery fuzzing
		stateFile := filepath.Join(dir, raft.StateFilename)
		if err := os.WriteFile(stateFile, data, 0600); err != nil {
			return
		}

		// OpenStorage must either succeed or return a clean error without panic
		s, err := raft.OpenStorage(dir)
		if err == nil && s != nil {
			_ = s.Close()
		}

		// Test log file recovery fuzzing
		os.Remove(stateFile)
		logFile := filepath.Join(dir, raft.LogFilename)
		if err := os.WriteFile(logFile, data, 0600); err != nil {
			return
		}

		s2, err := raft.OpenStorage(dir)
		if err == nil && s2 != nil {
			_ = s2.Close()
		}
	})
}

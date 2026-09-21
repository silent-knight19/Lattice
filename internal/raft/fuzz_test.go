package raft_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
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

func FuzzRoleTransitions(f *testing.F) {
	// Seed sequences of operations: 0=BecomeCandidate, 1=BecomeLeader, 2=StepDownSameTerm, 3=ObserveHigherTerm
	f.Add(uint64(1), []byte{0, 1, 2, 3})
	f.Add(uint64(5), []byte{0, 0, 1, 0, 3})
	f.Add(uint64(10), []byte{3, 2, 1, 0})

	f.Fuzz(func(t *testing.T, localIDRaw uint64, ops []byte) {
		if localIDRaw == 0 {
			return
		}
		if len(ops) > 20 { // Bound iteration length
			return
		}

		dir := t.TempDir()
		s, err := raft.OpenStorage(dir)
		if err != nil {
			return
		}
		defer func() { _ = s.Close() }()

		node, err := raft.NewNode(raft.NodeConfig{
			LocalID: cluster.NodeID(localIDRaw),
			Storage: s,
		})
		if err != nil {
			return
		}
		defer func() { _ = node.Close() }()

		for _, op := range ops {
			switch op % 4 {
			case 0:
				_ = node.BecomeCandidate()
			case 1:
				_ = node.BecomeLeader()
			case 2:
				_ = node.StepDownSameTerm(cluster.NodeID(1))
			case 3:
				term, err := node.Term()
				if err == nil {
					_, _ = node.ObserveHigherTerm(term + 1)
				}
			}

			// Invariant verification: role must always be valid
			r := node.Role()
			if !r.Valid() {
				t.Fatalf("invalid role observed: %v", r)
			}
		}
	})
}

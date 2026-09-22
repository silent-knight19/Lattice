package raft_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/raft"
	"github.com/silent-knight19/lattice/internal/transport"
)

// bulkSeedReplLog appends specs in a single Storage.Append call (one fsync)
// continuing the existing log tail. The storage term must already cover the
// entry terms.
func bulkSeedReplLog(t *testing.T, s *raft.Storage, term uint64, datas [][]byte) {
	t.Helper()
	base, _, err := s.LastIndexAndTerm()
	if err != nil {
		t.Fatalf("LastIndexAndTerm failed: %v", err)
	}
	entries := make([]raft.LogEntry, len(datas))
	for i, d := range datas {
		cp := append([]byte(nil), d...)
		entries[i] = raft.LogEntry{
			Index: base + raft.LogIndex(i) + 1,
			Term:  raft.Term(term),
			Type:  transport.PeerEntryNormal,
			Data:  cp,
		}
	}
	if err := s.Append(entries...); err != nil {
		t.Fatalf("bulk Append failed: %v", err)
	}
}

func newSenderNode(t *testing.T, sender *auditSender) (*raft.Node, *raft.Storage) {
	t.Helper()
	dir := t.TempDir()
	s, err := raft.OpenStorage(dir)
	if err != nil {
		t.Fatalf("OpenStorage failed: %v", err)
	}
	n, err := raft.NewNode(raft.NodeConfig{
		LocalID:    1,
		Storage:    s,
		Peers:      []cluster.NodeID{2},
		PeerSender: sender,
	})
	if err != nil {
		_ = s.Close()
		t.Fatalf("NewNode failed: %v", err)
	}
	t.Cleanup(func() {
		_ = n.Close()
		_ = s.Close()
	})
	return n, s
}

// pollBatch waits for a newly arriving frame whose entry count matches want.
// Returns the decoded request. Fails on timeout.
func pollBatch(t *testing.T, sender *auditSender, seen int, wantEntries int, timeout time.Duration) *transport.AppendEntriesRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frames := sender.GetSent(2)
		for i := seen; i < len(frames); i++ {
			req, err := transport.DecodeAppendEntries(frames[i])
			if err != nil {
				continue
			}
			if len(req.Entries) == wantEntries {
				return req
			}
		}
		seen = len(frames)
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d-entry batch (saw past %d frames)", wantEntries, seen)
	return nil
}

// A 1025-entry suffix must partition into 1024 + 1 preserving order.
func TestSender_BatchCountBoundary1024(t *testing.T) {
	sender := newAuditSender()
	n, s := newSenderNode(t, sender)
	mustCommitLead(t, n)
	term, _ := n.Term()

	const total = 1025
	datas := make([][]byte, total)
	for i := range datas {
		datas[i] = []byte{byte(i), byte(i >> 8)}
	}
	// Append after election so nextIndex (2) leaves the whole suffix pending.
	bulkSeedReplLog(t, s, uint64(term), datas)

	first := pollBatch(t, sender, 0, transport.MaxPeerEntries, 5*time.Second)
	if first.PrevLogIndex != 1 || first.PrevLogTerm != uint64(term) {
		t.Fatalf("batch1 PrevLog = %d/%d, want 1/%d", first.PrevLogIndex, first.PrevLogTerm, term)
	}
	for i, e := range first.Entries {
		if e.Term != uint64(term) || !bytes.Equal(e.Data, datas[i]) {
			t.Fatalf("batch1 entry %d corrupted", i)
		}
	}
	// Acknowledge the first batch: the remainder must follow with coherent
	// PrevLog, proving retry progress after partial batching.
	if err := n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, 1+uint64(transport.MaxPeerEntries))); err != nil {
		t.Fatalf("ack failed: %v", err)
	}
	second := pollBatch(t, sender, 0, 1, 5*time.Second)
	if second.PrevLogIndex != 1+uint64(transport.MaxPeerEntries) {
		t.Fatalf("batch2 PrevLogIndex = %d, want %d", second.PrevLogIndex, 1+uint64(transport.MaxPeerEntries))
	}
	if !bytes.Equal(second.Entries[0].Data, datas[transport.MaxPeerEntries]) {
		t.Fatalf("batch2 payload mismatch")
	}
}

// Exactly-at-limit batches send whole; one byte over splits without loss.
func TestSender_BatchPayloadBoundaries(t *testing.T) {
	const overhead = 52 + 2*13 // request header + two entry headers
	const limit = 5 * 1024 * 1024
	const atSize = (limit - overhead) / 2 // exact fit for two entries

	cases := []struct {
		name      string
		size      int
		wantFrame []int // expected entry counts per emitted batch
	}{
		{"under by one", atSize - 1, []int{2}},
		{"exactly at", atSize, []int{2}},
		{"over by one", atSize + 1, []int{1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := newAuditSender()
			n, s := newSenderNode(t, sender)
			mustCommitLead(t, n)
			term, _ := n.Term()
			bulkSeedReplLog(t, s, uint64(term), [][]byte{
				bytes.Repeat([]byte{0xA5}, tc.size),
				bytes.Repeat([]byte{0x5A}, tc.size),
			})
			// nextIndex is 2 (post-no-op election); pending suffix = 2 entries.
			var batches [][]transport.PeerLogEntry
			deadline := time.Now().Add(5 * time.Second)
			seen := 0
			for time.Now().Before(deadline) && len(batches) < len(tc.wantFrame) {
				frames := sender.GetSent(2)
				for i := seen; i < len(frames); i++ {
					req, err := transport.DecodeAppendEntries(frames[i])
					if err != nil || len(req.Entries) == 0 {
						continue
					}
					batches = append(batches, req.Entries)
					// Advance nextIndex like a follower ack so the sender
					// progresses to the remainder.
					_ = n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, req.PrevLogIndex+uint64(len(req.Entries))))
				}
				seen = len(frames)
				time.Sleep(5 * time.Millisecond)
			}
			if len(batches) != len(tc.wantFrame) {
				t.Fatalf("got %d batches, want %d", len(batches), len(tc.wantFrame))
			}
			got := 0
			for i, b := range batches {
				if len(b) != tc.wantFrame[i] {
					t.Fatalf("batch %d has %d entries, want %d", i, len(b), tc.wantFrame[i])
				}
				got += len(b)
			}
			if got != 2 {
				t.Fatalf("entries lost/duplicated across batches: %d", got)
			}
		})
	}
}

// One 4 MiB entry (maximum legal size) must replicate in a single frame.
func TestSender_SingleLargeEntry(t *testing.T) {
	sender := newAuditSender()
	n, s := newSenderNode(t, sender)
	mustCommitLead(t, n)
	term, _ := n.Term()
	big := bytes.Repeat([]byte{0x77}, raft.MaxLogEntryDataSize)
	bulkSeedReplLog(t, s, uint64(term), [][]byte{big})

	req := pollBatch(t, sender, 0, 1, 5*time.Second)
	if !bytes.Equal(req.Entries[0].Data, big) {
		t.Fatalf("large entry payload corrupted (got %d bytes)", len(req.Entries[0].Data))
	}
}

// Three 2 MiB entries partition into 2+1 with byte-exact payloads.
func TestSender_MultipleLargeEntries(t *testing.T) {
	sender := newAuditSender()
	n, s := newSenderNode(t, sender)
	mustCommitLead(t, n)
	term, _ := n.Term()
	const twoMiB = 2 * 1024 * 1024
	datas := [][]byte{
		bytes.Repeat([]byte{0x11}, twoMiB),
		bytes.Repeat([]byte{0x22}, twoMiB),
		bytes.Repeat([]byte{0x33}, twoMiB),
	}
	bulkSeedReplLog(t, s, uint64(term), datas)

	first := pollBatch(t, sender, 0, 2, 5*time.Second)
	if !bytes.Equal(first.Entries[0].Data, datas[0]) || !bytes.Equal(first.Entries[1].Data, datas[1]) {
		t.Fatalf("first batch payloads corrupted")
	}
	_ = n.HandleAppendEntriesResponse(2, commitResp(uint64(term), true, first.PrevLogIndex+2))
	second := pollBatch(t, sender, 0, 1, 5*time.Second)
	_ = second
	// The remainder must eventually arrive intact (poll for its content).
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		for _, f := range sender.GetSent(2) {
			req, err := transport.DecodeAppendEntries(f)
			if err != nil {
				continue
			}
			for _, e := range req.Entries {
				if bytes.Equal(e.Data, datas[2]) {
					found = true
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !found {
		t.Fatalf("third large entry never replicated")
	}
}

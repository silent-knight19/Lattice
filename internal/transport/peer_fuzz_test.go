package transport

import (
	"bytes"
	"testing"

	"github.com/silent-knight19/lattice/internal/cluster"
)

// FuzzDecodePeerFrame tests the full frame-level and peer message decoder pipeline against arbitrary byte streams.
// Invariants:
//   - Must never panic.
//   - Must never hang.
//   - Must never allocate unbounded memory from attacker-controlled length prefixes.
//   - If a message decodes successfully, re-encoding and re-decoding must succeed and yield an identical object.
func FuzzDecodePeerFrame(f *testing.F) {
	// Seed 1: RequestVote
	rvReq := &RequestVoteRequest{Term: 1, CandidateID: cluster.NodeID(1), LastLogIndex: 10, LastLogTerm: 1, Nonce: 999}
	f1, _ := EncodeRequestVote(rvReq, 1)
	var b1 bytes.Buffer
	_ = EncodeFrame(&b1, f1)
	f.Add(b1.Bytes())

	// Seed 2: RequestVoteResponse
	rvResp := &RequestVoteResponse{Term: 1, VoteGranted: true}
	f2, _ := EncodeRequestVoteResponse(rvResp, 2)
	var b2 bytes.Buffer
	_ = EncodeFrame(&b2, f2)
	f.Add(b2.Bytes())

	// Seed 3: AppendEntries Heartbeat
	aeReq := &AppendEntriesRequest{Term: 2, LeaderID: cluster.NodeID(2), PrevLogIndex: 20, PrevLogTerm: 2, LeaderCommit: 20, Nonce: 888}
	f3, _ := EncodeAppendEntries(aeReq, 3)
	var b3 bytes.Buffer
	_ = EncodeFrame(&b3, f3)
	f.Add(b3.Bytes())

	// Seed 4: AppendEntries with entry
	aeReqEntries := &AppendEntriesRequest{
		Term:         3,
		LeaderID:     cluster.NodeID(3),
		PrevLogIndex: 30,
		PrevLogTerm:  3,
		LeaderCommit: 30,
		Nonce:        777,
		Entries: []PeerLogEntry{
			{Term: 3, Type: PeerEntryNormal, Data: []byte("payload")},
		},
	}
	f4, _ := EncodeAppendEntries(aeReqEntries, 4)
	var b4 bytes.Buffer
	_ = EncodeFrame(&b4, f4)
	f.Add(b4.Bytes())

	// Seed 5: AppendEntriesResponse
	aeResp := &AppendEntriesResponse{Term: 3, Success: true, MatchIndex: 31}
	f5, _ := EncodeAppendEntriesResponse(aeResp, 5)
	var b5 bytes.Buffer
	_ = EncodeFrame(&b5, f5)
	f.Add(b5.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		frame, err := DecodeFrame(r)
		if err != nil {
			return
		}

		msg, err := DecodePeerMessage(frame)
		if err != nil {
			return
		}

		// If successfully decoded, re-encode and re-decode must succeed
		switch m := msg.(type) {
		case *RequestVoteRequest:
			reEncoded, err := EncodeRequestVote(m, frame.Header.SeqID)
			if err != nil {
				t.Fatalf("re-encode RequestVote failed: %v", err)
			}
			reDecoded, err := DecodeRequestVote(reEncoded)
			if err != nil {
				t.Fatalf("re-decode RequestVote failed: %v", err)
			}
			if *reDecoded != *m {
				t.Fatalf("RequestVote re-decode mismatch:\ngot  %+v\nwant %+v", *reDecoded, *m)
			}

		case *RequestVoteResponse:
			reEncoded, err := EncodeRequestVoteResponse(m, frame.Header.SeqID)
			if err != nil {
				t.Fatalf("re-encode RequestVoteResponse failed: %v", err)
			}
			reDecoded, err := DecodeRequestVoteResponse(reEncoded)
			if err != nil {
				t.Fatalf("re-decode RequestVoteResponse failed: %v", err)
			}
			if *reDecoded != *m {
				t.Fatalf("RequestVoteResponse re-decode mismatch:\ngot  %+v\nwant %+v", *reDecoded, *m)
			}

		case *AppendEntriesRequest:
			reEncoded, err := EncodeAppendEntries(m, frame.Header.SeqID)
			if err != nil {
				t.Fatalf("re-encode AppendEntries failed: %v", err)
			}
			reDecoded, err := DecodeAppendEntries(reEncoded)
			if err != nil {
				t.Fatalf("re-decode AppendEntries failed: %v", err)
			}
			if reDecoded.Term != m.Term || reDecoded.LeaderID != m.LeaderID ||
				reDecoded.PrevLogIndex != m.PrevLogIndex || reDecoded.PrevLogTerm != m.PrevLogTerm ||
				reDecoded.LeaderCommit != m.LeaderCommit || reDecoded.Nonce != m.Nonce ||
				len(reDecoded.Entries) != len(m.Entries) {
				t.Fatalf("AppendEntries re-decode mismatch")
			}

		case *AppendEntriesResponse:
			reEncoded, err := EncodeAppendEntriesResponse(m, frame.Header.SeqID)
			if err != nil {
				t.Fatalf("re-encode AppendEntriesResponse failed: %v", err)
			}
			reDecoded, err := DecodeAppendEntriesResponse(reEncoded)
			if err != nil {
				t.Fatalf("re-decode AppendEntriesResponse failed: %v", err)
			}
			if *reDecoded != *m {
				t.Fatalf("AppendEntriesResponse re-decode mismatch:\ngot  %+v\nwant %+v", *reDecoded, *m)
			}
		}
	})
}

// FuzzDecodeAppendEntriesPayload tests the AppendEntries payload decoder in isolation.
func FuzzDecodeAppendEntriesPayload(f *testing.F) {
	// Seed valid heartbeat
	req := &AppendEntriesRequest{
		Term:         1,
		LeaderID:     cluster.NodeID(1),
		PrevLogIndex: 10,
		PrevLogTerm:  1,
		LeaderCommit: 10,
		Nonce:        123,
	}
	f1, _ := EncodeAppendEntries(req, 1)
	f.Add(f1.Payload)

	// Seed with entries
	req2 := &AppendEntriesRequest{
		Term:         2,
		LeaderID:     cluster.NodeID(2),
		PrevLogIndex: 20,
		PrevLogTerm:  2,
		LeaderCommit: 20,
		Nonce:        456,
		Entries: []PeerLogEntry{
			{Term: 2, Type: PeerEntryNormal, Data: []byte("mutation")},
			{Term: 2, Type: PeerEntryNoop, Data: nil},
		},
	}
	f2, _ := EncodeAppendEntries(req2, 2)
	f.Add(f2.Payload)

	f.Fuzz(func(t *testing.T, payload []byte) {
		frame := &Frame{
			Header: Header{
				Magic:         Magic,
				OpCode:        OpCode(PeerOpAppendEntries),
				Flags:         FlagNone,
				PayloadLength: uint32(len(payload)),
			},
			Payload: payload,
		}
		// Must never panic or hang
		_, _ = DecodeAppendEntries(frame)
	})
}

package transport

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	errs "github.com/silent-knight19/lattice/internal/errors"
)

// TestPeerMessageType_ValidAndString validates the peer message type enumeration.
func TestPeerMessageType_ValidAndString(t *testing.T) {
	validTypes := []struct {
		msgType PeerMessageType
		name    string
	}{
		{PeerOpRequestVote, "PEER_REQUEST_VOTE"},
		{PeerOpRequestVoteResponse, "PEER_REQUEST_VOTE_RESPONSE"},
		{PeerOpAppendEntries, "PEER_APPEND_ENTRIES"},
		{PeerOpAppendEntriesResponse, "PEER_APPEND_ENTRIES_RESPONSE"},
	}

	for _, tc := range validTypes {
		if !tc.msgType.Valid() {
			t.Errorf("expected %v to be valid", tc.msgType)
		}
		if tc.msgType.String() != tc.name {
			t.Errorf("got %q, want %q", tc.msgType.String(), tc.name)
		}
	}

	invalidTypes := []PeerMessageType{
		PeerOpUnknown,
		0x01, // OpPut
		0x02, // OpGet
		0x06, // OpStats
		0x80, // boundary below
		0x85, // boundary above
		0xFF, // max
	}
	for _, it := range invalidTypes {
		if it.Valid() {
			t.Errorf("expected 0x%02x to be invalid", byte(it))
		}
	}
}

// TestPeerEntryType_ValidAndString validates the peer log entry type enumeration.
func TestPeerEntryType_ValidAndString(t *testing.T) {
	valid := []struct {
		entryType PeerEntryType
		name      string
	}{
		{PeerEntryNormal, "ENTRY_NORMAL"},
		{PeerEntryConfiguration, "ENTRY_CONFIGURATION"},
		{PeerEntryNoop, "ENTRY_NOOP"},
	}
	for _, tc := range valid {
		if !tc.entryType.Valid() {
			t.Errorf("expected %v to be valid", tc.entryType)
		}
		if tc.entryType.String() != tc.name {
			t.Errorf("got %q, want %q", tc.entryType.String(), tc.name)
		}
	}

	invalid := []PeerEntryType{0x00, 0x04, 0x10, 0xFF}
	for _, it := range invalid {
		if it.Valid() {
			t.Errorf("expected 0x%02x to be invalid", byte(it))
		}
	}
}

// TestRequestVote_RoundTrip tests round-trip encoding and decoding of RequestVote requests.
func TestRequestVote_RoundTrip(t *testing.T) {
	testCases := []struct {
		name string
		req  RequestVoteRequest
	}{
		{
			name: "typical_request",
			req: RequestVoteRequest{
				Term:         10,
				CandidateID:  cluster.NodeID(1),
				LastLogIndex: 100,
				LastLogTerm:  9,
				Nonce:        1234567890123456789,
			},
		},
		{
			name: "boundary_zeroes_and_ones",
			req: RequestVoteRequest{
				Term:         0,
				CandidateID:  cluster.NodeID(1),
				LastLogIndex: 0,
				LastLogTerm:  0,
				Nonce:        0,
			},
		},
		{
			name: "boundary_maximum_values",
			req: RequestVoteRequest{
				Term:         math.MaxUint64,
				CandidateID:  cluster.NodeID(math.MaxUint64),
				LastLogIndex: math.MaxUint64,
				LastLogTerm:  math.MaxUint64,
				Nonce:        math.MaxUint64,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := EncodeRequestVote(&tc.req, 42)
			if err != nil {
				t.Fatalf("EncodeRequestVote failed: %v", err)
			}
			if frame.Header.SeqID != 42 {
				t.Errorf("got SeqID %d, want 42", frame.Header.SeqID)
			}
			if frame.Header.PayloadLength != RequestVoteRequestSize {
				t.Errorf("got payload length %d, want %d", frame.Header.PayloadLength, RequestVoteRequestSize)
			}

			// Wire frame round trip through EncodeFrame / DecodeFrame
			var buf bytes.Buffer
			if err := EncodeFrame(&buf, frame); err != nil {
				t.Fatalf("EncodeFrame failed: %v", err)
			}
			decodedFrame, err := DecodeFrame(&buf)
			if err != nil {
				t.Fatalf("DecodeFrame failed: %v", err)
			}

			decodedReq, err := DecodeRequestVote(decodedFrame)
			if err != nil {
				t.Fatalf("DecodeRequestVote failed: %v", err)
			}
			if *decodedReq != tc.req {
				t.Errorf("decoded request mismatch:\ngot  %+v\nwant %+v", *decodedReq, tc.req)
			}
		})
	}
}

// TestRequestVoteResponse_RoundTrip tests round-trip encoding and decoding of RequestVote responses.
func TestRequestVoteResponse_RoundTrip(t *testing.T) {
	testCases := []struct {
		name string
		resp RequestVoteResponse
	}{
		{name: "vote_granted", resp: RequestVoteResponse{Term: 5, VoteGranted: true}},
		{name: "vote_rejected", resp: RequestVoteResponse{Term: 6, VoteGranted: false}},
		{name: "max_term_granted", resp: RequestVoteResponse{Term: math.MaxUint64, VoteGranted: true}},
		{name: "zero_term_rejected", resp: RequestVoteResponse{Term: 0, VoteGranted: false}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := EncodeRequestVoteResponse(&tc.resp, 99)
			if err != nil {
				t.Fatalf("EncodeRequestVoteResponse failed: %v", err)
			}
			if frame.Header.SeqID != 99 {
				t.Errorf("got SeqID %d, want 99", frame.Header.SeqID)
			}

			var buf bytes.Buffer
			if err := EncodeFrame(&buf, frame); err != nil {
				t.Fatalf("EncodeFrame failed: %v", err)
			}
			decodedFrame, err := DecodeFrame(&buf)
			if err != nil {
				t.Fatalf("DecodeFrame failed: %v", err)
			}

			decodedResp, err := DecodeRequestVoteResponse(decodedFrame)
			if err != nil {
				t.Fatalf("DecodeRequestVoteResponse failed: %v", err)
			}
			if *decodedResp != tc.resp {
				t.Errorf("decoded response mismatch:\ngot  %+v\nwant %+v", *decodedResp, tc.resp)
			}
		})
	}
}

// TestAppendEntries_RoundTrip tests round-trip encoding and decoding of AppendEntries requests.
func TestAppendEntries_RoundTrip(t *testing.T) {
	testCases := []struct {
		name string
		req  AppendEntriesRequest
	}{
		{
			name: "heartbeat_zero_entries",
			req: AppendEntriesRequest{
				Term:         1,
				LeaderID:     cluster.NodeID(1),
				PrevLogIndex: 10,
				PrevLogTerm:  1,
				LeaderCommit: 10,
				Nonce:        999,
				Entries:      nil,
			},
		},
		{
			name: "single_entry_normal",
			req: AppendEntriesRequest{
				Term:         2,
				LeaderID:     cluster.NodeID(2),
				PrevLogIndex: 20,
				PrevLogTerm:  2,
				LeaderCommit: 20,
				Nonce:        888,
				Entries: []PeerLogEntry{
					{
						Term: 2,
						Type: PeerEntryNormal,
						Data: []byte("SET k1 v1"),
					},
				},
			},
		},
		{
			name: "multiple_mixed_entries_with_binary_data",
			req: AppendEntriesRequest{
				Term:         5,
				LeaderID:     cluster.NodeID(3),
				PrevLogIndex: 100,
				PrevLogTerm:  4,
				LeaderCommit: 95,
				Nonce:        777,
				Entries: []PeerLogEntry{
					{
						Term: 5,
						Type: PeerEntryNormal,
						Data: []byte{0x00, 0xFF, 0x0A, 0x0D, 0x42},
					},
					{
						Term: 5,
						Type: PeerEntryConfiguration,
						Data: []byte("config_change_payload"),
					},
					{
						Term: 5,
						Type: PeerEntryNoop,
						Data: nil,
					},
				},
			},
		},
		{
			name: "max_entries_count",
			req: func() AppendEntriesRequest {
				entries := make([]PeerLogEntry, MaxPeerEntries)
				for i := 0; i < MaxPeerEntries; i++ {
					entries[i] = PeerLogEntry{
						Term: uint64(i + 1),
						Type: PeerEntryNormal,
						Data: []byte{byte(i % 256)},
					}
				}
				return AppendEntriesRequest{
					Term:         10,
					LeaderID:     cluster.NodeID(1),
					PrevLogIndex: 50,
					PrevLogTerm:  9,
					LeaderCommit: 50,
					Nonce:        12345,
					Entries:      entries,
				}
			}(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := EncodeAppendEntries(&tc.req, 101)
			if err != nil {
				t.Fatalf("EncodeAppendEntries failed: %v", err)
			}
			if frame.Header.SeqID != 101 {
				t.Errorf("got SeqID %d, want 101", frame.Header.SeqID)
			}

			var buf bytes.Buffer
			if err := EncodeFrame(&buf, frame); err != nil {
				t.Fatalf("EncodeFrame failed: %v", err)
			}
			decodedFrame, err := DecodeFrame(&buf)
			if err != nil {
				t.Fatalf("DecodeFrame failed: %v", err)
			}

			decodedReq, err := DecodeAppendEntries(decodedFrame)
			if err != nil {
				t.Fatalf("DecodeAppendEntries failed: %v", err)
			}

			if decodedReq.Term != tc.req.Term ||
				decodedReq.LeaderID != tc.req.LeaderID ||
				decodedReq.PrevLogIndex != tc.req.PrevLogIndex ||
				decodedReq.PrevLogTerm != tc.req.PrevLogTerm ||
				decodedReq.LeaderCommit != tc.req.LeaderCommit ||
				decodedReq.Nonce != tc.req.Nonce {
				t.Fatalf("fixed fields mismatch:\ngot  %+v\nwant %+v", decodedReq, tc.req)
			}

			if len(decodedReq.Entries) != len(tc.req.Entries) {
				t.Fatalf("entry count mismatch: got %d, want %d", len(decodedReq.Entries), len(tc.req.Entries))
			}
			for i := range tc.req.Entries {
				if decodedReq.Entries[i].Term != tc.req.Entries[i].Term {
					t.Errorf("entry %d term mismatch: got %d, want %d", i, decodedReq.Entries[i].Term, tc.req.Entries[i].Term)
				}
				if decodedReq.Entries[i].Type != tc.req.Entries[i].Type {
					t.Errorf("entry %d type mismatch: got %v, want %v", i, decodedReq.Entries[i].Type, tc.req.Entries[i].Type)
				}
				if !bytes.Equal(decodedReq.Entries[i].Data, tc.req.Entries[i].Data) {
					t.Errorf("entry %d data mismatch: got %v, want %v", i, decodedReq.Entries[i].Data, tc.req.Entries[i].Data)
				}
			}
		})
	}
}

// TestAppendEntriesResponse_RoundTrip tests round-trip encoding and decoding of AppendEntries responses.
func TestAppendEntriesResponse_RoundTrip(t *testing.T) {
	testCases := []struct {
		name string
		resp AppendEntriesResponse
	}{
		{name: "success_response", resp: AppendEntriesResponse{Term: 5, Success: true, MatchIndex: 120}},
		{name: "rejected_response", resp: AppendEntriesResponse{Term: 6, Success: false, MatchIndex: 80}},
		{name: "max_boundaries", resp: AppendEntriesResponse{Term: math.MaxUint64, Success: true, MatchIndex: math.MaxUint64}},
		{name: "zero_boundaries", resp: AppendEntriesResponse{Term: 0, Success: false, MatchIndex: 0}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			frame, err := EncodeAppendEntriesResponse(&tc.resp, 202)
			if err != nil {
				t.Fatalf("EncodeAppendEntriesResponse failed: %v", err)
			}
			if frame.Header.SeqID != 202 {
				t.Errorf("got SeqID %d, want 202", frame.Header.SeqID)
			}

			var buf bytes.Buffer
			if err := EncodeFrame(&buf, frame); err != nil {
				t.Fatalf("EncodeFrame failed: %v", err)
			}
			decodedFrame, err := DecodeFrame(&buf)
			if err != nil {
				t.Fatalf("DecodeFrame failed: %v", err)
			}

			decodedResp, err := DecodeAppendEntriesResponse(decodedFrame)
			if err != nil {
				t.Fatalf("DecodeAppendEntriesResponse failed: %v", err)
			}
			if *decodedResp != tc.resp {
				t.Errorf("decoded response mismatch:\ngot  %+v\nwant %+v", *decodedResp, tc.resp)
			}
		})
	}
}

// TestGoldenWire_RequestVote verifies byte-for-byte correctness against a known golden sequence.
func TestGoldenWire_RequestVote(t *testing.T) {
	req := &RequestVoteRequest{
		Term:         0x0102030405060708,
		CandidateID:  cluster.NodeID(0x1112131415161718),
		LastLogIndex: 0x2122232425262728,
		LastLogTerm:  0x3132333435363738,
		Nonce:        0x4142434445464748,
	}

	frame, err := EncodeRequestVote(req, 0xA1A2A3A4A5A6A7A8)
	if err != nil {
		t.Fatalf("EncodeRequestVote failed: %v", err)
	}

	expectedPayload := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // Term
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, // CandidateID
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, // LastLogIndex
		0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, // LastLogTerm
		0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, // Nonce
	}

	if !bytes.Equal(frame.Payload, expectedPayload) {
		t.Fatalf("RequestVote golden payload mismatch:\ngot  %x\nwant %x", frame.Payload, expectedPayload)
	}

	if frame.Header.OpCode != OpCode(PeerOpRequestVote) {
		t.Errorf("got OpCode 0x%02x, want 0x%02x", byte(frame.Header.OpCode), byte(PeerOpRequestVote))
	}
	if frame.Header.PayloadLength != 40 {
		t.Errorf("got PayloadLength %d, want 40", frame.Header.PayloadLength)
	}
}

// TestGoldenWire_RequestVoteResponse verifies byte-for-byte correctness against a known golden sequence.
func TestGoldenWire_RequestVoteResponse(t *testing.T) {
	resp := &RequestVoteResponse{
		Term:        0x0102030405060708,
		VoteGranted: true,
	}

	frame, err := EncodeRequestVoteResponse(resp, 1)
	if err != nil {
		t.Fatalf("EncodeRequestVoteResponse failed: %v", err)
	}

	expectedPayload := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // Term
		0x01, // VoteGranted = true (1)
	}

	if !bytes.Equal(frame.Payload, expectedPayload) {
		t.Fatalf("RequestVoteResponse golden payload mismatch:\ngot  %x\nwant %x", frame.Payload, expectedPayload)
	}
}

// TestGoldenWire_AppendEntriesHeartbeat verifies byte-for-byte correctness of a 0-entry heartbeat.
func TestGoldenWire_AppendEntriesHeartbeat(t *testing.T) {
	req := &AppendEntriesRequest{
		Term:         0x0102030405060708,
		LeaderID:     cluster.NodeID(0x1112131415161718),
		PrevLogIndex: 0x2122232425262728,
		PrevLogTerm:  0x3132333435363738,
		LeaderCommit: 0x4142434445464748,
		Nonce:        0x5152535455565758,
		Entries:      nil,
	}

	frame, err := EncodeAppendEntries(req, 1)
	if err != nil {
		t.Fatalf("EncodeAppendEntries failed: %v", err)
	}

	expectedPayload := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // Term
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, // LeaderID
		0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, // PrevLogIndex
		0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38, // PrevLogTerm
		0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, // LeaderCommit
		0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, // Nonce
		0x00, 0x00, 0x00, 0x00, // EntryCount = 0
	}

	if !bytes.Equal(frame.Payload, expectedPayload) {
		t.Fatalf("AppendEntries heartbeat golden payload mismatch:\ngot  %x\nwant %x", frame.Payload, expectedPayload)
	}
}

// TestGoldenWire_AppendEntriesResponse verifies byte-for-byte correctness against a known golden sequence.
func TestGoldenWire_AppendEntriesResponse(t *testing.T) {
	resp := &AppendEntriesResponse{
		Term:       0x0102030405060708,
		Success:    false,
		MatchIndex: 0x1112131415161718,
	}

	frame, err := EncodeAppendEntriesResponse(resp, 1)
	if err != nil {
		t.Fatalf("EncodeAppendEntriesResponse failed: %v", err)
	}

	expectedPayload := []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // Term
		0x00,                                           // Success = false (0)
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, // MatchIndex
	}

	if !bytes.Equal(frame.Payload, expectedPayload) {
		t.Fatalf("AppendEntriesResponse golden payload mismatch:\ngot  %x\nwant %x", frame.Payload, expectedPayload)
	}
}

// TestRequestVote_MalformedMatrix tests fail-closed rejection across malformed RequestVote frames.
func TestRequestVote_MalformedMatrix(t *testing.T) {
	t.Run("nil_frame", func(t *testing.T) {
		_, err := DecodeRequestVote(nil)
		if !errors.Is(err, errs.ErrNilReceiver) {
			t.Errorf("got %v, want ErrNilReceiver", err)
		}
	})

	t.Run("invalid_candidate_id_zero", func(t *testing.T) {
		req := &RequestVoteRequest{
			Term:        1,
			CandidateID: cluster.NodeID(0), // zero is invalid
		}
		_, err := EncodeRequestVote(req, 1)
		if !errors.Is(err, errs.ErrInvalidNodeID) {
			t.Errorf("got %v, want ErrInvalidNodeID", err)
		}
	})

	t.Run("wrong_opcode", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: 40},
			Payload: make([]byte, 40),
		}
		_, err := DecodeRequestVote(f)
		if !errors.Is(err, errs.ErrInvalidPeerMessage) {
			t.Errorf("got %v, want ErrInvalidPeerMessage", err)
		}
	})

	t.Run("unsupported_flags", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVote), Flags: 0x01, PayloadLength: 40},
			Payload: make([]byte, 40),
		}
		_, err := DecodeRequestVote(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("payload_truncated_less_than_40", func(t *testing.T) {
		for size := 0; size < 40; size++ {
			f := &Frame{
				Header:  Header{OpCode: OpCode(PeerOpRequestVote), PayloadLength: uint32(size)},
				Payload: make([]byte, size),
			}
			_, err := DecodeRequestVote(f)
			if !errors.Is(err, errs.ErrInvalidPeerPayload) {
				t.Errorf("size %d: got %v, want ErrInvalidPeerPayload", size, err)
			}
		}
	})

	t.Run("payload_too_long_greater_than_40", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVote), PayloadLength: 41},
			Payload: make([]byte, 41),
		}
		_, err := DecodeRequestVote(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("candidate_id_zero_on_wire", func(t *testing.T) {
		payload := make([]byte, 40)
		// CandidateID at 8..16 is 0
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVote), PayloadLength: 40},
			Payload: payload,
		}
		_, err := DecodeRequestVote(f)
		if !errors.Is(err, errs.ErrInvalidNodeID) {
			t.Errorf("got %v, want ErrInvalidNodeID", err)
		}
	})
}

// TestRequestVoteResponse_MalformedMatrix tests fail-closed rejection for malformed vote responses.
func TestRequestVoteResponse_MalformedMatrix(t *testing.T) {
	t.Run("nil_frame", func(t *testing.T) {
		_, err := DecodeRequestVoteResponse(nil)
		if !errors.Is(err, errs.ErrNilReceiver) {
			t.Errorf("got %v, want ErrNilReceiver", err)
		}
	})

	t.Run("wrong_opcode", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVote), PayloadLength: 9},
			Payload: make([]byte, 9),
		}
		_, err := DecodeRequestVoteResponse(f)
		if !errors.Is(err, errs.ErrInvalidPeerMessage) {
			t.Errorf("got %v, want ErrInvalidPeerMessage", err)
		}
	})

	t.Run("payload_truncated", func(t *testing.T) {
		for size := 0; size < 9; size++ {
			f := &Frame{
				Header:  Header{OpCode: OpCode(PeerOpRequestVoteResponse), PayloadLength: uint32(size)},
				Payload: make([]byte, size),
			}
			_, err := DecodeRequestVoteResponse(f)
			if !errors.Is(err, errs.ErrInvalidPeerPayload) {
				t.Errorf("size %d: got %v, want ErrInvalidPeerPayload", size, err)
			}
		}
	})

	t.Run("payload_too_long", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVoteResponse), PayloadLength: 10},
			Payload: make([]byte, 10),
		}
		_, err := DecodeRequestVoteResponse(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("invalid_boolean_values", func(t *testing.T) {
		for _, b := range []byte{2, 3, 0x80, 0xFF} {
			payload := make([]byte, 9)
			payload[8] = b
			f := &Frame{
				Header:  Header{OpCode: OpCode(PeerOpRequestVoteResponse), PayloadLength: 9},
				Payload: payload,
			}
			_, err := DecodeRequestVoteResponse(f)
			if !errors.Is(err, errs.ErrInvalidPeerBoolean) {
				t.Errorf("byte 0x%02x: got %v, want ErrInvalidPeerBoolean", b, err)
			}
		}
	})
}

// TestAppendEntries_MalformedMatrix tests fail-closed rejection for malformed AppendEntries requests.
func TestAppendEntries_MalformedMatrix(t *testing.T) {
	t.Run("nil_frame", func(t *testing.T) {
		_, err := DecodeAppendEntries(nil)
		if !errors.Is(err, errs.ErrNilReceiver) {
			t.Errorf("got %v, want ErrNilReceiver", err)
		}
	})

	t.Run("invalid_leader_id_zero", func(t *testing.T) {
		req := &AppendEntriesRequest{
			Term:     1,
			LeaderID: cluster.NodeID(0),
		}
		_, err := EncodeAppendEntries(req, 1)
		if !errors.Is(err, errs.ErrInvalidNodeID) {
			t.Errorf("got %v, want ErrInvalidNodeID", err)
		}
	})

	t.Run("wrong_opcode", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVote), PayloadLength: 52},
			Payload: make([]byte, 52),
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerMessage) {
			t.Errorf("got %v, want ErrInvalidPeerMessage", err)
		}
	})

	t.Run("unsupported_flags", func(t *testing.T) {
		payload := make([]byte, 52)
		binary.PutUint64(payload[8:16], 1) // LeaderID = 1
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), Flags: 0x02, PayloadLength: 52},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("header_truncated_less_than_52", func(t *testing.T) {
		for size := 0; size < 52; size++ {
			f := &Frame{
				Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: uint32(size)},
				Payload: make([]byte, size),
			}
			_, err := DecodeAppendEntries(f)
			if !errors.Is(err, errs.ErrInvalidPeerPayload) {
				t.Errorf("size %d: got %v, want ErrInvalidPeerPayload", size, err)
			}
		}
	})

	t.Run("leader_id_zero_on_wire", func(t *testing.T) {
		payload := make([]byte, 52)
		// LeaderID at 8..16 is 0
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: 52},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidNodeID) {
			t.Errorf("got %v, want ErrInvalidNodeID", err)
		}
	})

	t.Run("entry_count_exceeds_max", func(t *testing.T) {
		payload := make([]byte, 52)
		binary.PutUint64(payload[8:16], 1) // LeaderID = 1
		binary.PutUint32(payload[48:52], MaxPeerEntries+1)
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: 52},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("payload_insufficient_for_declared_entry_count", func(t *testing.T) {
		payload := make([]byte, 52+10) // 10 bytes after header, but count says 1 (needs 13)
		binary.PutUint64(payload[8:16], 1)
		binary.PutUint32(payload[48:52], 1)
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: uint32(len(payload))},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("entry_type_invalid", func(t *testing.T) {
		payload := make([]byte, 52+13)
		binary.PutUint64(payload[8:16], 1)
		binary.PutUint32(payload[48:52], 1)
		payload[52+8] = 0x99 // invalid entry type
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: uint32(len(payload))},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerEntry) {
			t.Errorf("got %v, want ErrInvalidPeerEntry", err)
		}
	})

	t.Run("entry_data_length_exceeds_remaining", func(t *testing.T) {
		payload := make([]byte, 52+13+5) // only 5 bytes of data
		binary.PutUint64(payload[8:16], 1)
		binary.PutUint32(payload[48:52], 1)
		payload[52+8] = byte(PeerEntryNormal)
		binary.PutUint32(payload[52+9:52+13], 10) // claims 10 bytes of data
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: uint32(len(payload))},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerEntry) {
			t.Errorf("got %v, want ErrInvalidPeerEntry", err)
		}
	})

	t.Run("unexpected_trailing_bytes", func(t *testing.T) {
		payload := make([]byte, 52+13+4+2) // 2 extra bytes at end
		binary.PutUint64(payload[8:16], 1)
		binary.PutUint32(payload[48:52], 1)
		payload[52+8] = byte(PeerEntryNormal)
		binary.PutUint32(payload[52+9:52+13], 4) // 4 bytes of data
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntries), PayloadLength: uint32(len(payload))},
			Payload: payload,
		}
		_, err := DecodeAppendEntries(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("encode_entry_count_exceeds_max", func(t *testing.T) {
		req := &AppendEntriesRequest{
			LeaderID: cluster.NodeID(1),
			Entries:  make([]PeerLogEntry, MaxPeerEntries+1),
		}
		_, err := EncodeAppendEntries(req, 1)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("encode_invalid_entry_type", func(t *testing.T) {
		req := &AppendEntriesRequest{
			LeaderID: cluster.NodeID(1),
			Entries: []PeerLogEntry{
				{Type: PeerEntryType(0xEE)},
			},
		}
		_, err := EncodeAppendEntries(req, 1)
		if !errors.Is(err, errs.ErrInvalidPeerEntry) {
			t.Errorf("got %v, want ErrInvalidPeerEntry", err)
		}
	})
}

// TestAppendEntriesResponse_MalformedMatrix tests fail-closed rejection for malformed AppendEntries responses.
func TestAppendEntriesResponse_MalformedMatrix(t *testing.T) {
	t.Run("nil_frame", func(t *testing.T) {
		_, err := DecodeAppendEntriesResponse(nil)
		if !errors.Is(err, errs.ErrNilReceiver) {
			t.Errorf("got %v, want ErrNilReceiver", err)
		}
	})

	t.Run("wrong_opcode", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpRequestVoteResponse), PayloadLength: 17},
			Payload: make([]byte, 17),
		}
		_, err := DecodeAppendEntriesResponse(f)
		if !errors.Is(err, errs.ErrInvalidPeerMessage) {
			t.Errorf("got %v, want ErrInvalidPeerMessage", err)
		}
	})

	t.Run("payload_truncated", func(t *testing.T) {
		for size := 0; size < 17; size++ {
			f := &Frame{
				Header:  Header{OpCode: OpCode(PeerOpAppendEntriesResponse), PayloadLength: uint32(size)},
				Payload: make([]byte, size),
			}
			_, err := DecodeAppendEntriesResponse(f)
			if !errors.Is(err, errs.ErrInvalidPeerPayload) {
				t.Errorf("size %d: got %v, want ErrInvalidPeerPayload", size, err)
			}
		}
	})

	t.Run("payload_too_long", func(t *testing.T) {
		f := &Frame{
			Header:  Header{OpCode: OpCode(PeerOpAppendEntriesResponse), PayloadLength: 18},
			Payload: make([]byte, 18),
		}
		_, err := DecodeAppendEntriesResponse(f)
		if !errors.Is(err, errs.ErrInvalidPeerPayload) {
			t.Errorf("got %v, want ErrInvalidPeerPayload", err)
		}
	})

	t.Run("invalid_boolean_values", func(t *testing.T) {
		for _, b := range []byte{2, 3, 0x80, 0xFF} {
			payload := make([]byte, 17)
			payload[8] = b
			f := &Frame{
				Header:  Header{OpCode: OpCode(PeerOpAppendEntriesResponse), PayloadLength: 17},
				Payload: payload,
			}
			_, err := DecodeAppendEntriesResponse(f)
			if !errors.Is(err, errs.ErrInvalidPeerBoolean) {
				t.Errorf("byte 0x%02x: got %v, want ErrInvalidPeerBoolean", b, err)
			}
		}
	})
}

// TestLargeSingleEntry_BoundaryLimits tests the maximum legal single entry and rejection of oversized entries.
func TestLargeSingleEntry_BoundaryLimits(t *testing.T) {
	// Max legal data length that fits within MaxPayloadLength (5 MiB = 5,242,880)
	// AppendEntriesRequestHeaderSize (52) + PeerLogEntryHeaderSize (13) + DataLen = 5,242,880
	// maxDataLen = 5,242,880 - 65 = 5,242,815
	maxLegalDataLen := int(MaxPayloadLength) - AppendEntriesRequestHeaderSize - PeerLogEntryHeaderSize

	req := &AppendEntriesRequest{
		Term:     1,
		LeaderID: cluster.NodeID(1),
		Entries: []PeerLogEntry{
			{
				Term: 1,
				Type: PeerEntryNormal,
				Data: make([]byte, maxLegalDataLen),
			},
		},
	}

	frame, err := EncodeAppendEntries(req, 1)
	if err != nil {
		t.Fatalf("EncodeAppendEntries for max legal entry failed: %v", err)
	}
	if frame.Header.PayloadLength != MaxPayloadLength {
		t.Errorf("got payload length %d, want %d", frame.Header.PayloadLength, MaxPayloadLength)
	}

	// 1 byte over max payload length must fail closed
	oversizedReq := &AppendEntriesRequest{
		Term:     1,
		LeaderID: cluster.NodeID(1),
		Entries: []PeerLogEntry{
			{
				Term: 1,
				Type: PeerEntryNormal,
				Data: make([]byte, maxLegalDataLen+1),
			},
		},
	}
	_, err = EncodeAppendEntries(oversizedReq, 1)
	if !errors.Is(err, errs.ErrFrameTooLarge) {
		t.Errorf("got %v, want ErrFrameTooLarge", err)
	}
}

// TestDeterministicEncoding proves that encoding identical message structures produces byte-for-byte identical wire frames.
func TestDeterministicEncoding(t *testing.T) {
	req1 := &AppendEntriesRequest{
		Term:         7,
		LeaderID:     cluster.NodeID(2),
		PrevLogIndex: 88,
		PrevLogTerm:  6,
		LeaderCommit: 80,
		Nonce:        555,
		Entries: []PeerLogEntry{
			{Term: 7, Type: PeerEntryNormal, Data: []byte("alpha")},
			{Term: 7, Type: PeerEntryConfiguration, Data: []byte("beta")},
		},
	}
	req2 := &AppendEntriesRequest{
		Term:         7,
		LeaderID:     cluster.NodeID(2),
		PrevLogIndex: 88,
		PrevLogTerm:  6,
		LeaderCommit: 80,
		Nonce:        555,
		Entries: []PeerLogEntry{
			{Term: 7, Type: PeerEntryNormal, Data: []byte("alpha")},
			{Term: 7, Type: PeerEntryConfiguration, Data: []byte("beta")},
		},
	}

	f1, err1 := EncodeAppendEntries(req1, 42)
	f2, err2 := EncodeAppendEntries(req2, 42)
	if err1 != nil || err2 != nil {
		t.Fatalf("unexpected encode errors: %v, %v", err1, err2)
	}

	var b1, b2 bytes.Buffer
	_ = EncodeFrame(&b1, f1)
	_ = EncodeFrame(&b2, f2)

	if !bytes.Equal(b1.Bytes(), b2.Bytes()) {
		t.Error("encoding is not deterministic")
	}
}

// TestGenericPeerHelpers tests EncodePeerRequest, DecodePeerRequest, EncodePeerResponse, DecodePeerResponse, and DecodePeerMessage.
func TestGenericPeerHelpers(t *testing.T) {
	// RequestVote Request
	rvReq := &RequestVoteRequest{Term: 1, CandidateID: cluster.NodeID(2), Nonce: 3}
	f1, err := EncodePeerRequest(rvReq, 10)
	if err != nil {
		t.Fatalf("EncodePeerRequest failed: %v", err)
	}
	dec1, err := DecodePeerRequest(f1)
	if err != nil {
		t.Fatalf("DecodePeerRequest failed: %v", err)
	}
	if !reflect.DeepEqual(dec1, rvReq) {
		t.Errorf("DecodePeerRequest mismatch: got %+v, want %+v", dec1, rvReq)
	}

	// AppendEntries Request
	aeReq := &AppendEntriesRequest{Term: 1, LeaderID: cluster.NodeID(1), Nonce: 4}
	f2, err := EncodePeerRequest(aeReq, 11)
	if err != nil {
		t.Fatalf("EncodePeerRequest failed: %v", err)
	}
	dec2, err := DecodePeerRequest(f2)
	if err != nil {
		t.Fatalf("DecodePeerRequest failed: %v", err)
	}
	if !reflect.DeepEqual(dec2, aeReq) {
		t.Errorf("DecodePeerRequest mismatch: got %+v, want %+v", dec2, aeReq)
	}

	// RequestVote Response
	rvResp := &RequestVoteResponse{Term: 1, VoteGranted: true}
	f3, err := EncodePeerResponse(rvResp, 12)
	if err != nil {
		t.Fatalf("EncodePeerResponse failed: %v", err)
	}
	dec3, err := DecodePeerResponse(f3)
	if err != nil {
		t.Fatalf("DecodePeerResponse failed: %v", err)
	}
	if !reflect.DeepEqual(dec3, rvResp) {
		t.Errorf("DecodePeerResponse mismatch: got %+v, want %+v", dec3, rvResp)
	}

	// AppendEntries Response
	aeResp := &AppendEntriesResponse{Term: 1, Success: true, MatchIndex: 10}
	f4, err := EncodePeerResponse(aeResp, 13)
	if err != nil {
		t.Fatalf("EncodePeerResponse failed: %v", err)
	}
	dec4, err := DecodePeerResponse(f4)
	if err != nil {
		t.Fatalf("DecodePeerResponse failed: %v", err)
	}
	if !reflect.DeepEqual(dec4, aeResp) {
		t.Errorf("DecodePeerResponse mismatch: got %+v, want %+v", dec4, aeResp)
	}

	// DecodePeerMessage on all 4
	for _, f := range []*Frame{f1, f2, f3, f4} {
		msg, err := DecodePeerMessage(f)
		if err != nil {
			t.Errorf("DecodePeerMessage failed for OpCode 0x%02x: %v", byte(f.Header.OpCode), err)
		}
		if msg == nil {
			t.Errorf("expected non-nil message")
		}
	}

	// Unsupported types
	_, err = EncodePeerRequest("invalid", 1)
	if !errors.Is(err, errs.ErrInvalidPeerMessage) {
		t.Errorf("got %v, want ErrInvalidPeerMessage", err)
	}
	_, err = EncodePeerResponse("invalid", 1)
	if !errors.Is(err, errs.ErrInvalidPeerMessage) {
		t.Errorf("got %v, want ErrInvalidPeerMessage", err)
	}
	unknownFrame := &Frame{Header: Header{OpCode: OpCode(0x01)}}
	_, err = DecodePeerMessage(unknownFrame)
	if !errors.Is(err, errs.ErrInvalidPeerMessage) {
		t.Errorf("got %v, want ErrInvalidPeerMessage", err)
	}
}

// TestClientOpcodeNonCollision verifies that client opcodes (0x01..0x06) and peer opcodes (0x81..0x84)
// strictly fail-closed when passed across namespaces.
func TestClientOpcodeNonCollision(t *testing.T) {
	// 1. Client opcodes passed to peer decoder
	for op := OpPut; op <= OpStats; op++ {
		f := &Frame{
			Header:  Header{OpCode: op, PayloadLength: 40},
			Payload: make([]byte, 40),
		}
		_, err := DecodePeerMessage(f)
		if !errors.Is(err, errs.ErrInvalidPeerMessage) {
			t.Errorf("client opcode 0x%02x passed to DecodePeerMessage did not return ErrInvalidPeerMessage: %v", byte(op), err)
		}
	}

	// 2. Peer opcodes passed to client decoder (DecodeRequest)
	peerOps := []PeerMessageType{PeerOpRequestVote, PeerOpRequestVoteResponse, PeerOpAppendEntries, PeerOpAppendEntriesResponse}
	for _, pop := range peerOps {
		f := &Frame{
			Header:  Header{OpCode: OpCode(pop), PayloadLength: 10},
			Payload: make([]byte, 10),
		}
		_, err := DecodeRequest(f)
		if !errors.Is(err, errs.ErrInvalidOpCode) {
			t.Errorf("peer opcode 0x%02x passed to DecodeRequest did not return ErrInvalidOpCode: %v", byte(pop), err)
		}
	}
}

// Benchmarks for Peer Protocol framing
func BenchmarkEncodeRequestVote(b *testing.B) {
	req := &RequestVoteRequest{
		Term:         10,
		CandidateID:  cluster.NodeID(1),
		LastLogIndex: 100,
		LastLogTerm:  9,
		Nonce:        123456789,
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = EncodeRequestVote(req, uint64(i))
	}
}

func BenchmarkDecodeRequestVote(b *testing.B) {
	req := &RequestVoteRequest{
		Term:         10,
		CandidateID:  cluster.NodeID(1),
		LastLogIndex: 100,
		LastLogTerm:  9,
		Nonce:        123456789,
	}
	f, _ := EncodeRequestVote(req, 1)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeRequestVote(f)
	}
}

func BenchmarkEncodeAppendEntries_Heartbeat(b *testing.B) {
	req := &AppendEntriesRequest{
		Term:         10,
		LeaderID:     cluster.NodeID(1),
		PrevLogIndex: 100,
		PrevLogTerm:  9,
		LeaderCommit: 100,
		Nonce:        123456789,
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = EncodeAppendEntries(req, uint64(i))
	}
}

func BenchmarkDecodeAppendEntries_Heartbeat(b *testing.B) {
	req := &AppendEntriesRequest{
		Term:         10,
		LeaderID:     cluster.NodeID(1),
		PrevLogIndex: 100,
		PrevLogTerm:  9,
		LeaderCommit: 100,
		Nonce:        123456789,
	}
	f, _ := EncodeAppendEntries(req, 1)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeAppendEntries(f)
	}
}

func BenchmarkEncodeAppendEntries_Batch(b *testing.B) {
	entries := make([]PeerLogEntry, 16)
	data := []byte("mutation_command_payload_data")
	for i := 0; i < 16; i++ {
		entries[i] = PeerLogEntry{
			Term: 10,
			Type: PeerEntryNormal,
			Data: data,
		}
	}
	req := &AppendEntriesRequest{
		Term:         10,
		LeaderID:     cluster.NodeID(1),
		PrevLogIndex: 100,
		PrevLogTerm:  9,
		LeaderCommit: 95,
		Nonce:        123456789,
		Entries:      entries,
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = EncodeAppendEntries(req, uint64(i))
	}
}

func BenchmarkDecodeAppendEntries_Batch(b *testing.B) {
	entries := make([]PeerLogEntry, 16)
	data := []byte("mutation_command_payload_data")
	for i := 0; i < 16; i++ {
		entries[i] = PeerLogEntry{
			Term: 10,
			Type: PeerEntryNormal,
			Data: data,
		}
	}
	req := &AppendEntriesRequest{
		Term:         10,
		LeaderID:     cluster.NodeID(1),
		PrevLogIndex: 100,
		PrevLogTerm:  9,
		LeaderCommit: 95,
		Nonce:        123456789,
		Entries:      entries,
	}
	f, _ := EncodeAppendEntries(req, 1)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = DecodeAppendEntries(f)
	}
}

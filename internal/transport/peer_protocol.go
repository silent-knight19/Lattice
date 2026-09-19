package transport

import (
	"fmt"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	"github.com/silent-knight19/lattice/internal/errors"
)

// PeerMessageType identifies the RPC message type for peer-to-peer communication.
// Occupies the 1-byte OpCode field in standard protocol Frame headers (byte offset 4).
//
// Namespace Architecture:
//
//	0x00       : Reserved / Invalid
//	0x01..0x06 : Client Data Plane Operations (OpPut..OpStats)
//	0x07..0x80 : Reserved
//	0x81..0x84 : Peer Control Plane RPC Operations (Raft)
type PeerMessageType byte

const (
	// PeerOpUnknown represents an uninitialized or unrecognized peer message opcode.
	PeerOpUnknown PeerMessageType = 0x00

	// PeerOpRequestVote represents a candidate requesting votes from peers (0x81).
	PeerOpRequestVote PeerMessageType = 0x81

	// PeerOpRequestVoteResponse represents a peer's response to a RequestVote RPC (0x82).
	PeerOpRequestVoteResponse PeerMessageType = 0x82

	// PeerOpAppendEntries represents a leader replicating entries or sending heartbeats (0x83).
	PeerOpAppendEntries PeerMessageType = 0x83

	// PeerOpAppendEntriesResponse represents a follower's response to an AppendEntries RPC (0x84).
	PeerOpAppendEntriesResponse PeerMessageType = 0x84
)

// Valid reports whether the peer message type is one of the recognized operations (0x81..0x84).
func (t PeerMessageType) Valid() bool {
	return t >= PeerOpRequestVote && t <= PeerOpAppendEntriesResponse
}

// String returns the human-readable string representation of the peer message type.
func (t PeerMessageType) String() string {
	switch t {
	case PeerOpRequestVote:
		return "PEER_REQUEST_VOTE"
	case PeerOpRequestVoteResponse:
		return "PEER_REQUEST_VOTE_RESPONSE"
	case PeerOpAppendEntries:
		return "PEER_APPEND_ENTRIES"
	case PeerOpAppendEntriesResponse:
		return "PEER_APPEND_ENTRIES_RESPONSE"
	default:
		return fmt.Sprintf("PEER_OP_UNKNOWN(0x%02x)", byte(t))
	}
}

const (
	// RequestVoteRequestSize is the fixed size in bytes of a RequestVote request payload (40 bytes).
	// Layout: Term (8B) | CandidateID (8B) | LastLogIndex (8B) | LastLogTerm (8B) | Nonce (8B).
	RequestVoteRequestSize = 40

	// RequestVoteResponseSize is the fixed size in bytes of a RequestVote response payload (9 bytes).
	// Layout: Term (8B) | VoteGranted (1B: 0x00=false, 0x01=true).
	RequestVoteResponseSize = 9

	// AppendEntriesRequestHeaderSize is the fixed size in bytes of the AppendEntries request payload header (52 bytes).
	// Layout: Term (8B) | LeaderID (8B) | PrevLogIndex (8B) | PrevLogTerm (8B) | LeaderCommit (8B) | Nonce (8B) | EntryCount (4B).
	AppendEntriesRequestHeaderSize = 52

	// AppendEntriesResponseSize is the fixed size in bytes of an AppendEntries response payload (17 bytes).
	// Layout: Term (8B) | Success (1B: 0x00=false, 0x01=true) | MatchIndex (8B).
	AppendEntriesResponseSize = 17

	// PeerLogEntryHeaderSize is the fixed size in bytes of each log entry header within AppendEntries (13 bytes).
	// Layout: Term (8B) | Type (1B) | DataLen (4B) | Data (DataLen B).
	PeerLogEntryHeaderSize = 13

	// MaxPeerEntries is the maximum number of log entries allowed in a single AppendEntries request.
	// Bounded to 1024 entries to prevent memory amplification while accommodating Raft batch replication.
	MaxPeerEntries = 1024
)

// PeerEntryType identifies the category of a peer log entry on the wire.
type PeerEntryType uint8

const (
	// PeerEntryNormal represents an application state-machine proposal command (0x01).
	PeerEntryNormal PeerEntryType = 0x01

	// PeerEntryConfiguration represents a cluster configuration / topology change command (0x02).
	PeerEntryConfiguration PeerEntryType = 0x02

	// PeerEntryNoop represents a leader commit alignment no-op entry upon election (0x03).
	PeerEntryNoop PeerEntryType = 0x03
)

// Valid reports whether the peer entry type is recognized.
func (t PeerEntryType) Valid() bool {
	return t >= PeerEntryNormal && t <= PeerEntryNoop
}

// String returns the human-readable string representation of the entry type.
func (t PeerEntryType) String() string {
	switch t {
	case PeerEntryNormal:
		return "ENTRY_NORMAL"
	case PeerEntryConfiguration:
		return "ENTRY_CONFIGURATION"
	case PeerEntryNoop:
		return "ENTRY_NOOP"
	default:
		return fmt.Sprintf("ENTRY_UNKNOWN(0x%02x)", byte(t))
	}
}

// RequestVoteRequest represents a Raft RequestVote RPC request on the wire.
//
// Wire Layout (40 bytes fixed):
//
//	Offset  0..7  : Term (8B uint64 Big-Endian)
//	Offset  8..15 : CandidateID (8B uint64 Big-Endian, must be > 0)
//	Offset 16..23 : LastLogIndex (8B uint64 Big-Endian)
//	Offset 24..31 : LastLogTerm (8B uint64 Big-Endian)
//	Offset 32..39 : Nonce (8B uint64 Big-Endian)
type RequestVoteRequest struct {
	Term         uint64
	CandidateID  cluster.NodeID
	LastLogIndex uint64
	LastLogTerm  uint64
	Nonce        uint64
}

// RequestVoteResponse represents a Raft RequestVote RPC response on the wire.
//
// Wire Layout (9 bytes fixed):
//
//	Offset 0..7 : Term (8B uint64 Big-Endian)
//	Offset 8    : VoteGranted (1B uint8: 0x00=false, 0x01=true)
type RequestVoteResponse struct {
	Term        uint64
	VoteGranted bool
}

// PeerLogEntry represents an individual log entry carried on the wire in an AppendEntries RPC.
//
// Wire Layout:
//
//	Offset 0..7  : Term (8B uint64 Big-Endian)
//	Offset 8     : Type (1B uint8 / PeerEntryType)
//	Offset 9..12 : DataLen (4B uint32 Big-Endian)
//	Offset 13..  : Data (DataLen bytes)
type PeerLogEntry struct {
	Term uint64
	Type PeerEntryType
	Data []byte
}

// AppendEntriesRequest represents a Raft AppendEntries RPC request on the wire.
//
// Wire Layout:
//
//	Offset  0..7  : Term (8B uint64 Big-Endian)
//	Offset  8..15 : LeaderID (8B uint64 Big-Endian, must be > 0)
//	Offset 16..23 : PrevLogIndex (8B uint64 Big-Endian)
//	Offset 24..31 : PrevLogTerm (8B uint64 Big-Endian)
//	Offset 32..39 : LeaderCommit (8B uint64 Big-Endian)
//	Offset 40..47 : Nonce (8B uint64 Big-Endian)
//	Offset 48..51 : EntryCount (4B uint32 Big-Endian)
//	Offset 52..   : Entries...
type AppendEntriesRequest struct {
	Term         uint64
	LeaderID     cluster.NodeID
	PrevLogIndex uint64
	PrevLogTerm  uint64
	LeaderCommit uint64
	Nonce        uint64
	Entries      []PeerLogEntry
}

// AppendEntriesResponse represents a Raft AppendEntries RPC response on the wire.
//
// Wire Layout (17 bytes fixed):
//
//	Offset  0..7  : Term (8B uint64 Big-Endian)
//	Offset  8     : Success (1B uint8: 0x00=false, 0x01=true)
//	Offset  9..16 : MatchIndex (8B uint64 Big-Endian)
type AppendEntriesResponse struct {
	Term       uint64
	Success    bool
	MatchIndex uint64
}

// EncodeRequestVote serializes a RequestVoteRequest into a protocol Frame.
//
// Invariants:
//   - CandidateID must be valid (> 0).
//   - Allocates exactly RequestVoteRequestSize (40B) payload.
//   - Encodes all integers Big-Endian.
//   - Sets OpCode to PeerOpRequestVote (0x81).
func EncodeRequestVote(req *RequestVoteRequest, seqID uint64) (*Frame, error) {
	if req == nil {
		return nil, errors.ErrNilReceiver
	}
	if !req.CandidateID.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(req.CandidateID), Reason: "candidate ID in RequestVote must be greater than zero"}
	}
	payload := make([]byte, RequestVoteRequestSize)
	binary.PutUint64(payload[0:8], req.Term)
	binary.PutUint64(payload[8:16], uint64(req.CandidateID))
	binary.PutUint64(payload[16:24], req.LastLogIndex)
	binary.PutUint64(payload[24:32], req.LastLogTerm)
	binary.PutUint64(payload[32:40], req.Nonce)

	return &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        OpCode(PeerOpRequestVote),
			Flags:         FlagNone,
			SeqID:         seqID,
			PayloadLength: RequestVoteRequestSize,
		},
		Payload: payload,
	}, nil
}

// DecodeRequestVote unpacks and validates a RequestVoteRequest from a Frame.
//
// Invariants:
//   - OpCode must equal PeerOpRequestVote (0x81).
//   - Flags must be 0x00.
//   - Payload length must be exactly RequestVoteRequestSize (40B).
//   - CandidateID must be valid (> 0).
func DecodeRequestVote(f *Frame) (*RequestVoteRequest, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	if PeerMessageType(f.Header.OpCode) != PeerOpRequestVote {
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
	if f.Header.Flags != FlagNone {
		return nil, &errors.InvalidPeerPayloadError{Reason: fmt.Sprintf("unsupported peer frame flags: 0x%02x", f.Header.Flags)}
	}
	if len(f.Payload) != RequestVoteRequestSize {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("RequestVote payload size %d does not match expected %d", len(f.Payload), RequestVoteRequestSize),
		}
	}
	cid := cluster.NodeID(binary.GetUint64(f.Payload[8:16]))
	if !cid.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(cid), Reason: "candidate ID in RequestVote must be greater than zero"}
	}
	return &RequestVoteRequest{
		Term:         binary.GetUint64(f.Payload[0:8]),
		CandidateID:  cid,
		LastLogIndex: binary.GetUint64(f.Payload[16:24]),
		LastLogTerm:  binary.GetUint64(f.Payload[24:32]),
		Nonce:        binary.GetUint64(f.Payload[32:40]),
	}, nil
}

// EncodeRequestVoteResponse serializes a RequestVoteResponse into a protocol Frame.
//
// Invariants:
//   - Encodes Term (8B Big-Endian).
//   - Encodes VoteGranted strictly as 0x00 or 0x01.
//   - Sets OpCode to PeerOpRequestVoteResponse (0x82).
func EncodeRequestVoteResponse(resp *RequestVoteResponse, seqID uint64) (*Frame, error) {
	if resp == nil {
		return nil, errors.ErrNilReceiver
	}
	payload := make([]byte, RequestVoteResponseSize)
	binary.PutUint64(payload[0:8], resp.Term)
	if resp.VoteGranted {
		payload[8] = 0x01
	} else {
		payload[8] = 0x00
	}
	return &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        OpCode(PeerOpRequestVoteResponse),
			Status:        StatusOk,
			SeqID:         seqID,
			PayloadLength: RequestVoteResponseSize,
		},
		Payload: payload,
	}, nil
}

// DecodeRequestVoteResponse unpacks and validates a RequestVoteResponse from a Frame.
//
// Invariants:
//   - OpCode must equal PeerOpRequestVoteResponse (0x82).
//   - Payload length must be exactly RequestVoteResponseSize (9B).
//   - VoteGranted wire byte must be strictly 0x00 or 0x01.
func DecodeRequestVoteResponse(f *Frame) (*RequestVoteResponse, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	if PeerMessageType(f.Header.OpCode) != PeerOpRequestVoteResponse {
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
	if len(f.Payload) != RequestVoteResponseSize {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("RequestVoteResponse payload size %d does not match expected %d", len(f.Payload), RequestVoteResponseSize),
		}
	}
	b := f.Payload[8]
	if b > 1 {
		return nil, &errors.InvalidPeerBooleanError{Field: "VoteGranted", Value: b}
	}
	return &RequestVoteResponse{
		Term:        binary.GetUint64(f.Payload[0:8]),
		VoteGranted: b == 1,
	}, nil
}

// EncodeAppendEntries serializes an AppendEntriesRequest into a protocol Frame.
//
// Invariants:
//   - LeaderID must be valid (> 0).
//   - Entry count must not exceed MaxPeerEntries (1024).
//   - Total wire payload must not exceed MaxPayloadLength (5 MiB).
//   - All entry types must be valid.
//   - Exact Big-Endian encoding.
func EncodeAppendEntries(req *AppendEntriesRequest, seqID uint64) (*Frame, error) {
	if req == nil {
		return nil, errors.ErrNilReceiver
	}
	if !req.LeaderID.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(req.LeaderID), Reason: "leader ID in AppendEntries must be greater than zero"}
	}
	if len(req.Entries) > MaxPeerEntries {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("AppendEntries entry count %d exceeds maximum %d", len(req.Entries), MaxPeerEntries),
		}
	}

	// Safe length pre-computation with overflow checks
	totalLen := uint64(AppendEntriesRequestHeaderSize)
	for i, entry := range req.Entries {
		if !entry.Type.Valid() {
			return nil, &errors.InvalidPeerEntryError{Index: i, Reason: fmt.Sprintf("invalid entry type: 0x%02x", byte(entry.Type))}
		}
		entryLen := uint64(PeerLogEntryHeaderSize) + uint64(len(entry.Data))
		totalLen += entryLen
		if totalLen > uint64(MaxPayloadLength) {
			return nil, &errors.FrameTooLargeError{PayloadSize: uint32(totalLen), MaxSize: MaxPayloadLength}
		}
	}

	payload := make([]byte, totalLen)
	binary.PutUint64(payload[0:8], req.Term)
	binary.PutUint64(payload[8:16], uint64(req.LeaderID))
	binary.PutUint64(payload[16:24], req.PrevLogIndex)
	binary.PutUint64(payload[24:32], req.PrevLogTerm)
	binary.PutUint64(payload[32:40], req.LeaderCommit)
	binary.PutUint64(payload[40:48], req.Nonce)
	binary.PutUint32(payload[48:52], uint32(len(req.Entries)))

	offset := AppendEntriesRequestHeaderSize
	for _, entry := range req.Entries {
		binary.PutUint64(payload[offset:offset+8], entry.Term)
		payload[offset+8] = byte(entry.Type)
		binary.PutUint32(payload[offset+9:offset+13], uint32(len(entry.Data)))
		offset += PeerLogEntryHeaderSize
		if len(entry.Data) > 0 {
			copy(payload[offset:offset+len(entry.Data)], entry.Data)
			offset += len(entry.Data)
		}
	}

	return &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        OpCode(PeerOpAppendEntries),
			Flags:         FlagNone,
			SeqID:         seqID,
			PayloadLength: uint32(totalLen),
		},
		Payload: payload,
	}, nil
}

// DecodeAppendEntries unpacks and validates an AppendEntriesRequest from a Frame.
//
// Invariants:
//   - OpCode must equal PeerOpAppendEntries (0x83).
//   - Flags must be 0x00.
//   - Payload size must be at least AppendEntriesRequestHeaderSize (52B).
//   - Entry count <= MaxPeerEntries (1024).
//   - Remaining bytes must be validated BEFORE slice allocation.
//   - Per-entry DataLen must be validated against remaining payload BEFORE allocation.
//   - Exact payload consumption: no trailing or truncated bytes.
//   - Memory isolation: independent defensive copies for all entry data slices.
func DecodeAppendEntries(f *Frame) (*AppendEntriesRequest, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	if PeerMessageType(f.Header.OpCode) != PeerOpAppendEntries {
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
	if f.Header.Flags != FlagNone {
		return nil, &errors.InvalidPeerPayloadError{Reason: fmt.Sprintf("unsupported peer frame flags: 0x%02x", f.Header.Flags)}
	}
	payload := f.Payload
	if len(payload) < AppendEntriesRequestHeaderSize {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("AppendEntries payload size %d shorter than header %d", len(payload), AppendEntriesRequestHeaderSize),
		}
	}

	lid := cluster.NodeID(binary.GetUint64(payload[8:16]))
	if !lid.IsValid() {
		return nil, &errors.InvalidNodeIDError{NodeID: uint64(lid), Reason: "leader ID in AppendEntries must be greater than zero"}
	}

	entryCount := binary.GetUint32(payload[48:52])
	if entryCount > MaxPeerEntries {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("AppendEntries entry count %d exceeds maximum %d", entryCount, MaxPeerEntries),
		}
	}

	// Bounded pre-allocation check: ensure remaining bytes can at least cover entry headers
	remainingPayload := uint64(len(payload) - AppendEntriesRequestHeaderSize)
	minRequired := uint64(entryCount) * uint64(PeerLogEntryHeaderSize)
	if remainingPayload < minRequired {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("AppendEntries payload size %d insufficient for %d entries (minimum required %d bytes)", len(payload), entryCount, minRequired+AppendEntriesRequestHeaderSize),
		}
	}

	var entries []PeerLogEntry
	if entryCount > 0 {
		entries = make([]PeerLogEntry, 0, entryCount)
	}

	offset := AppendEntriesRequestHeaderSize
	for i := uint32(0); i < entryCount; i++ {
		if offset+PeerLogEntryHeaderSize > len(payload) {
			return nil, &errors.InvalidPeerEntryError{Index: int(i), Reason: "entry header truncated"}
		}
		term := binary.GetUint64(payload[offset : offset+8])
		entryType := PeerEntryType(payload[offset+8])
		if !entryType.Valid() {
			return nil, &errors.InvalidPeerEntryError{Index: int(i), Reason: fmt.Sprintf("invalid entry type 0x%02x", byte(entryType))}
		}
		dataLen := binary.GetUint32(payload[offset+9 : offset+13])
		offset += PeerLogEntryHeaderSize

		// Safe check against remaining payload bytes
		if uint64(dataLen) > uint64(len(payload)-offset) {
			return nil, &errors.InvalidPeerEntryError{
				Index:  int(i),
				Reason: fmt.Sprintf("entry data length %d exceeds remaining payload bytes %d", dataLen, len(payload)-offset),
			}
		}

		var data []byte
		if dataLen > 0 {
			data = make([]byte, dataLen)
			copy(data, payload[offset:offset+int(dataLen)])
			offset += int(dataLen)
		}

		entries = append(entries, PeerLogEntry{
			Term: term,
			Type: entryType,
			Data: data,
		})
	}

	// Exact consumption invariant: reject unexpected trailing bytes
	if offset != len(payload) {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("AppendEntries payload has %d unexpected trailing bytes", len(payload)-offset),
		}
	}

	return &AppendEntriesRequest{
		Term:         binary.GetUint64(payload[0:8]),
		LeaderID:     lid,
		PrevLogIndex: binary.GetUint64(payload[16:24]),
		PrevLogTerm:  binary.GetUint64(payload[24:32]),
		LeaderCommit: binary.GetUint64(payload[32:40]),
		Nonce:        binary.GetUint64(payload[40:48]),
		Entries:      entries,
	}, nil
}

// EncodeAppendEntriesResponse serializes an AppendEntriesResponse into a protocol Frame.
//
// Invariants:
//   - Encodes Term (8B Big-Endian).
//   - Encodes Success strictly as 0x00 or 0x01.
//   - Encodes MatchIndex (8B Big-Endian).
//   - Sets OpCode to PeerOpAppendEntriesResponse (0x84).
func EncodeAppendEntriesResponse(resp *AppendEntriesResponse, seqID uint64) (*Frame, error) {
	if resp == nil {
		return nil, errors.ErrNilReceiver
	}
	payload := make([]byte, AppendEntriesResponseSize)
	binary.PutUint64(payload[0:8], resp.Term)
	if resp.Success {
		payload[8] = 0x01
	} else {
		payload[8] = 0x00
	}
	binary.PutUint64(payload[9:17], resp.MatchIndex)

	return &Frame{
		Header: Header{
			Magic:         Magic,
			OpCode:        OpCode(PeerOpAppendEntriesResponse),
			Status:        StatusOk,
			SeqID:         seqID,
			PayloadLength: AppendEntriesResponseSize,
		},
		Payload: payload,
	}, nil
}

// DecodeAppendEntriesResponse unpacks and validates an AppendEntriesResponse from a Frame.
//
// Invariants:
//   - OpCode must equal PeerOpAppendEntriesResponse (0x84).
//   - Payload length must be exactly AppendEntriesResponseSize (17B).
//   - Success wire byte must be strictly 0x00 or 0x01.
func DecodeAppendEntriesResponse(f *Frame) (*AppendEntriesResponse, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	if PeerMessageType(f.Header.OpCode) != PeerOpAppendEntriesResponse {
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
	if len(f.Payload) != AppendEntriesResponseSize {
		return nil, &errors.InvalidPeerPayloadError{
			Reason: fmt.Sprintf("AppendEntriesResponse payload size %d does not match expected %d", len(f.Payload), AppendEntriesResponseSize),
		}
	}
	b := f.Payload[8]
	if b > 1 {
		return nil, &errors.InvalidPeerBooleanError{Field: "Success", Value: b}
	}
	return &AppendEntriesResponse{
		Term:       binary.GetUint64(f.Payload[0:8]),
		Success:    b == 1,
		MatchIndex: binary.GetUint64(f.Payload[9:17]),
	}, nil
}

// EncodePeerRequest serializes any valid peer request (*RequestVoteRequest or *AppendEntriesRequest) into a Frame.
func EncodePeerRequest(req any, seqID uint64) (*Frame, error) {
	switch r := req.(type) {
	case *RequestVoteRequest:
		return EncodeRequestVote(r, seqID)
	case *AppendEntriesRequest:
		return EncodeAppendEntries(r, seqID)
	default:
		return nil, fmt.Errorf("%w: unsupported peer request type %T", errors.ErrInvalidPeerMessage, req)
	}
}

// DecodePeerRequest decodes a Frame into either *RequestVoteRequest or *AppendEntriesRequest based on its opcode.
func DecodePeerRequest(f *Frame) (any, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	switch PeerMessageType(f.Header.OpCode) {
	case PeerOpRequestVote:
		return DecodeRequestVote(f)
	case PeerOpAppendEntries:
		return DecodeAppendEntries(f)
	default:
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
}

// EncodePeerResponse serializes any valid peer response (*RequestVoteResponse or *AppendEntriesResponse) into a Frame.
func EncodePeerResponse(resp any, seqID uint64) (*Frame, error) {
	switch r := resp.(type) {
	case *RequestVoteResponse:
		return EncodeRequestVoteResponse(r, seqID)
	case *AppendEntriesResponse:
		return EncodeAppendEntriesResponse(r, seqID)
	default:
		return nil, fmt.Errorf("%w: unsupported peer response type %T", errors.ErrInvalidPeerMessage, resp)
	}
}

// DecodePeerResponse decodes a Frame into either *RequestVoteResponse or *AppendEntriesResponse based on its opcode.
func DecodePeerResponse(f *Frame) (any, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	switch PeerMessageType(f.Header.OpCode) {
	case PeerOpRequestVoteResponse:
		return DecodeRequestVoteResponse(f)
	case PeerOpAppendEntriesResponse:
		return DecodeAppendEntriesResponse(f)
	default:
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
}

// DecodePeerMessage decodes any recognized peer message (request or response) from a Frame.
func DecodePeerMessage(f *Frame) (any, error) {
	if f == nil {
		return nil, errors.ErrNilReceiver
	}
	switch PeerMessageType(f.Header.OpCode) {
	case PeerOpRequestVote:
		return DecodeRequestVote(f)
	case PeerOpRequestVoteResponse:
		return DecodeRequestVoteResponse(f)
	case PeerOpAppendEntries:
		return DecodeAppendEntries(f)
	case PeerOpAppendEntriesResponse:
		return DecodeAppendEntriesResponse(f)
	default:
		return nil, &errors.InvalidPeerMessageError{OpCode: byte(f.Header.OpCode)}
	}
}

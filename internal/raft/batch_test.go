package raft

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/cluster"
	latticeErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

type batchMockSM struct {
	mu    sync.RWMutex
	store map[string][]byte
}

func newBatchMockSM() *batchMockSM {
	return &batchMockSM{store: make(map[string][]byte)}
}

func (m *batchMockSM) Put(ctx context.Context, key, val []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(val))
	copy(cp, val)
	m.store[string(key)] = cp
	return nil
}

func (m *batchMockSM) Get(key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	val, ok := m.store[string(key)]
	if !ok {
		return nil, latticeErrors.ErrKeyNotFound
	}
	cp := make([]byte, len(val))
	copy(cp, val)
	return cp, nil
}

func (m *batchMockSM) Exists(key []byte) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[string(key)]
	return ok, nil
}

func (m *batchMockSM) Delete(ctx context.Context, key []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, string(key))
	return nil
}

func (m *batchMockSM) Batch(ctx context.Context, ops []binary.BatchOp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, op := range ops {
		if op.Type == binary.OpTypePut {
			cp := make([]byte, len(op.Value))
			copy(cp, op.Value)
			m.store[string(op.Key)] = cp
		} else if op.Type == binary.OpTypeDelete {
			delete(m.store, string(op.Key))
		}
	}
	return nil
}

// TestRaftBatch_CommandCodec verifies encoding and decoding of Raft batch commands.
func TestRaftBatch_CommandCodec(t *testing.T) {
	cmd := Command{
		Op: binary.OpTypeBatch,
		Batch: []CommandOp{
			{Op: binary.OpTypePut, Key: []byte("k1"), Value: []byte("v1")},
			{Op: binary.OpTypeDelete, Key: []byte("k2")},
			{Op: binary.OpTypePut, Key: []byte("k3"), Value: []byte("v3")},
		},
	}

	encoded, err := EncodeCommand(cmd)
	if err != nil {
		t.Fatalf("EncodeCommand failed: %v", err)
	}

	decoded, err := DecodeCommand(encoded)
	if err != nil {
		t.Fatalf("DecodeCommand failed: %v", err)
	}

	if decoded.Op != binary.OpTypeBatch {
		t.Fatalf("expected OpTypeBatch, got %v", decoded.Op)
	}
	if len(decoded.Batch) != 3 {
		t.Fatalf("expected 3 batch ops, got %d", len(decoded.Batch))
	}

	if decoded.Batch[0].Op != binary.OpTypePut || string(decoded.Batch[0].Key) != "k1" || string(decoded.Batch[0].Value) != "v1" {
		t.Fatalf("unexpected op 0: %+v", decoded.Batch[0])
	}
	if decoded.Batch[1].Op != binary.OpTypeDelete || string(decoded.Batch[1].Key) != "k2" || len(decoded.Batch[1].Value) != 0 {
		t.Fatalf("unexpected op 1: %+v", decoded.Batch[1])
	}
	if decoded.Batch[2].Op != binary.OpTypePut || string(decoded.Batch[2].Key) != "k3" || string(decoded.Batch[2].Value) != "v3" {
		t.Fatalf("unexpected op 2: %+v", decoded.Batch[2])
	}
}

// TestRaftBatch_RouterWriteAndLinearizableRead tests proposing a batch via ProposalRouter,
// applying it through the Raft apply loop to the StateMachine, and reading via ReadIndex.
func TestRaftBatch_RouterWriteAndLinearizableRead(t *testing.T) {
	sm := newBatchMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 1, map[cluster.NodeID]string{1: "127.0.0.1:9001"})
	n.SetTopology(top)
	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	router := NewProposalRouter(n, top, sm)

	// Issue BATCH proposal via router
	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  100,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("batch_r1"), Value: []byte("val_r1")},
			{Type: transport.BatchOpPut, Key: []byte("batch_r2"), Value: []byte("val_r2")},
			{Type: transport.BatchOpDelete, Key: []byte("batch_r1")},
		},
	}

	resp, err := router.RouteWrite(context.Background(), req)
	if err != nil {
		t.Fatalf("RouteWrite failed: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk, got %s: %s", resp.Status, resp.Message)
	}

	// Wait for entry to be applied to StateMachine
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := n.WaitForApplied(ctx, 2); err != nil {
		t.Fatalf("WaitForApplied failed: %v", err)
	}

	// Verify state machine state
	val2, err := sm.Get([]byte("batch_r2"))
	if err != nil || string(val2) != "val_r2" {
		t.Fatalf("batch_r2 expected val_r2, got %q, err=%v", string(val2), err)
	}

	// batch_r1 was deleted by the last op in the batch
	_, err = sm.Get([]byte("batch_r1"))
	if err == nil {
		t.Fatalf("batch_r1 was expected deleted, but found")
	}

	// Verify linearizable read through RouteRead
	readReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  101,
		Key:    []byte("batch_r2"),
	}
	readResp, err := router.RouteRead(context.Background(), readReq)
	if err != nil {
		t.Fatalf("RouteRead failed: %v", err)
	}
	if readResp.Status != transport.StatusOk {
		t.Fatalf("expected StatusOk on read, got %s: %s", readResp.Status, readResp.Message)
	}
	if string(readResp.Value) != "val_r2" {
		t.Fatalf("read value mismatch: got %q, want val_r2", string(readResp.Value))
	}
}

// TestRaftBatch_FollowerRedirects verifies that a follower returns StatusNotLeader with leader details for BATCH.
func TestRaftBatch_FollowerRedirects(t *testing.T) {
	sm := newBatchMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
		2: "127.0.0.1:9002",
	})
	n.SetTopology(top)

	_, _ = n.HandleAppendEntries(cluster.NodeID(2), &transport.AppendEntriesRequest{
		Term:     1,
		LeaderID: 2,
	})

	router := NewProposalRouter(n, top, sm)

	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  200,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("k"), Value: []byte("v")},
		},
	}

	resp, err := router.RouteWrite(context.Background(), req)
	if err != nil {
		t.Fatalf("RouteWrite failed: %v", err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader on follower, got: %s", resp.Status)
	}
	if resp.LeaderID != 2 || resp.LeaderAddr != "127.0.0.1:9002" {
		t.Fatalf("unexpected redirect leader: id=%d addr=%s", resp.LeaderID, resp.LeaderAddr)
	}
}

// TestRaftBatch_ApplyAtomicityUnderConcurrentReads tests that a committed batch is applied
// as one logical transition so that concurrent state-machine readers never observe an intermediate state.
func TestRaftBatch_ApplyAtomicityUnderConcurrentReads(t *testing.T) {
	sm := newBatchMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 1, map[cluster.NodeID]string{1: "127.0.0.1:9001"})
	n.SetTopology(top)
	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	router := NewProposalRouter(n, top, sm)

	const numBatches = 20
	const keysPerBatch = 5

	var wg sync.WaitGroup
	var stop atomic.Bool
	var violations atomic.Int64

	// Concurrently read state machine
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			firstK := []byte("atomic_k_0")
			lastK := []byte(fmt.Sprintf("atomic_k_%d", keysPerBatch-1))

			for !stop.Load() {
				v1, err1 := sm.Get(firstK)
				vLast, errLast := sm.Get(lastK)

				if err1 == nil {
					if errLast != nil {
						violations.Add(1)
					} else {
						var n1, nLast int
						_, _ = fmt.Sscanf(string(v1), "val_%d", &n1)
						_, _ = fmt.Sscanf(string(vLast), "val_%d", &nLast)
						if nLast < n1 {
							violations.Add(1)
						}
					}
				}
				time.Sleep(10 * time.Microsecond)
			}
		}(r)
	}

	for b := 1; b <= numBatches; b++ {
		val := fmt.Sprintf("val_%d", b)
		ops := make([]transport.BatchOp, keysPerBatch)
		for k := 0; k < keysPerBatch; k++ {
			ops[k] = transport.BatchOp{
				Type:  transport.BatchOpPut,
				Key:   []byte(fmt.Sprintf("atomic_k_%d", k)),
				Value: []byte(val),
			}
		}
		req := &transport.Request{
			OpCode: transport.OpBatch,
			SeqID:  uint64(1000 + b),
			Batch:  ops,
		}
		resp, err := router.RouteWrite(context.Background(), req)
		if err != nil || resp.Status != transport.StatusOk {
			t.Fatalf("batch %d failed: resp=%+v err=%v", b, resp, err)
		}
		_ = n.WaitForApplied(context.Background(), LogIndex(b+1))
	}

	stop.Store(true)
	wg.Wait()

	if v := violations.Load(); v > 0 {
		t.Fatalf("Raft batch apply atomicity violated %d times!", v)
	}
}

// TestRaftBatch_FollowerCatchupWithCommittedBatch tests a follower receiving and applying
// a committed batch log entry from the leader.
func TestRaftBatch_FollowerCatchupWithCommittedBatch(t *testing.T) {
	sm := newBatchMockSM()
	follower, _, cleanup := newTestNodeWithSM(t, 2, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 2, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
		2: "127.0.0.1:9002",
	})
	follower.SetTopology(top)

	// Construct batch command
	cmd := Command{
		Op: binary.OpTypeBatch,
		Batch: []CommandOp{
			{Op: binary.OpTypePut, Key: []byte("f_key1"), Value: []byte("f_val1")},
			{Op: binary.OpTypePut, Key: []byte("f_key2"), Value: []byte("f_val2")},
			{Op: binary.OpTypeDelete, Key: []byte("f_key1")},
		},
	}
	cmdBytes, err := EncodeCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}

	// Leader sends AppendEntries to follower with batch entry at index 1, committed
	aeReq := &transport.AppendEntriesRequest{
		Term:         1,
		LeaderID:     1,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		LeaderCommit: 1,
		Entries: []transport.PeerLogEntry{
			{
				Term: 1,
				Type: transport.PeerEntryNormal,
				Data: cmdBytes,
			},
		},
	}

	resp, err := follower.HandleAppendEntries(1, aeReq)
	if err != nil {
		t.Fatalf("HandleAppendEntries failed: %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected AppendEntries success on follower")
	}

	// Wait for follower's apply loop to apply entry 1
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := follower.WaitForApplied(ctx, 1); err != nil {
		t.Fatalf("WaitForApplied failed on follower: %v", err)
	}

	// Verify state machine on follower
	val2, err := sm.Get([]byte("f_key2"))
	if err != nil || string(val2) != "f_val2" {
		t.Fatalf("expected f_val2 on follower, got %q, err=%v", string(val2), err)
	}
	_, err = sm.Get([]byte("f_key1"))
	if err == nil {
		t.Fatalf("expected f_key1 deleted on follower")
	}
}

// TestRaftBatch_LeadershipLossDuringProposal tests that when a leader discovers a higher term
// or steps down during proposal processing, the batch returns appropriate non-leader error or redirect.
func TestRaftBatch_LeadershipLossDuringProposal(t *testing.T) {
	sm := newBatchMockSM()
	n, _, cleanup := newTestNodeWithSM(t, 1, sm, 10)
	defer cleanup()

	top := newTestTopology(t, 1, map[cluster.NodeID]string{
		1: "127.0.0.1:9001",
		2: "127.0.0.1:9002",
	})
	n.SetTopology(top)
	if err := n.BecomeCandidate(); err != nil {
		t.Fatal(err)
	}
	if err := n.BecomeLeader(); err != nil {
		t.Fatal(err)
	}

	router := NewProposalRouter(n, top, sm)

	// Step down to follower by receiving higher term
	_, _ = n.HandleAppendEntries(2, &transport.AppendEntriesRequest{
		Term:     2,
		LeaderID: 2,
	})

	req := &transport.Request{
		OpCode: transport.OpBatch,
		SeqID:  301,
		Batch: []transport.BatchOp{
			{Type: transport.BatchOpPut, Key: []byte("k"), Value: []byte("v")},
		},
	}

	resp, err := router.RouteWrite(context.Background(), req)
	if err != nil {
		t.Fatalf("RouteWrite failed: %v", err)
	}
	if resp.Status != transport.StatusNotLeader {
		t.Fatalf("expected StatusNotLeader after leadership loss, got %s", resp.Status)
	}
	if resp.LeaderID != 2 {
		t.Fatalf("expected redirected leader 2, got %d", resp.LeaderID)
	}
}

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

var (
	// ErrPipelineEmpty indicates that Execute was called on an empty pipeline.
	ErrPipelineEmpty = errors.New("pipeline is empty")

	// ErrPipelineTooLarge indicates that a pipeline contains more operations than the permitted maximum.
	ErrPipelineTooLarge = errors.New("pipeline exceeds maximum allowed operations")
)

// DefaultMaxPipelineOps is the maximum number of requests permitted in a single pipeline execution (default: 64).
const DefaultMaxPipelineOps = 64

// Pipeline coordinates bounded TCP request pipelining over a single connection,
// dispatching requests back-to-back and correlating responses out-of-order via SeqID.
type Pipeline struct {
	client *Client
	ops    []pipelineOp
	maxOps int
}

type pipelineOp interface {
	request() (*transport.Request, error)
	fulfill(resp *transport.Response, err error)
}

// PutFuture represents the pending asynchronous outcome of a pipelined Put operation.
type PutFuture struct {
	key   []byte
	value []byte
	err   error
	done  chan struct{}
}

func (f *PutFuture) request() (*transport.Request, error) {
	if len(f.key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(f.key) > transport.MaxKeyLength {
		return nil, ErrKeyTooLarge
	}
	if len(f.value) > transport.MaxValueLength {
		return nil, ErrValueTooLarge
	}
	return &transport.Request{
		OpCode: transport.OpPut,
		Key:    f.key,
		Value:  f.value,
	}, nil
}

func (f *PutFuture) fulfill(resp *transport.Response, err error) {
	defer close(f.done)
	if err != nil {
		f.err = err
		return
	}
	if resp.Status == transport.StatusOk {
		return
	}
	f.err = fmt.Errorf("put failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Result waits for the pipeline execution to complete and returns any error encountered.
func (f *PutFuture) Result() error {
	<-f.done
	return f.err
}

// GetFuture represents the pending asynchronous outcome of a pipelined Get operation.
type GetFuture struct {
	key   []byte
	value []byte
	err   error
	done  chan struct{}
}

func (f *GetFuture) request() (*transport.Request, error) {
	if len(f.key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(f.key) > transport.MaxKeyLength {
		return nil, ErrKeyTooLarge
	}
	return &transport.Request{
		OpCode: transport.OpGet,
		Key:    f.key,
	}, nil
}

func (f *GetFuture) fulfill(resp *transport.Response, err error) {
	defer close(f.done)
	if err != nil {
		f.err = err
		return
	}
	if resp.Status == transport.StatusOk {
		val := make([]byte, len(resp.Value))
		copy(val, resp.Value)
		f.value = val
		return
	}
	if resp.Status == transport.StatusKeyNotFound {
		f.err = ErrKeyNotFound
		return
	}
	f.err = fmt.Errorf("get failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Result waits for the pipeline execution to complete and returns the retrieved value or error.
func (f *GetFuture) Result() ([]byte, error) {
	<-f.done
	return f.value, f.err
}

// DeleteFuture represents the pending asynchronous outcome of a pipelined Delete operation.
type DeleteFuture struct {
	key  []byte
	err  error
	done chan struct{}
}

func (f *DeleteFuture) request() (*transport.Request, error) {
	if len(f.key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(f.key) > transport.MaxKeyLength {
		return nil, ErrKeyTooLarge
	}
	return &transport.Request{
		OpCode: transport.OpDelete,
		Key:    f.key,
	}, nil
}

func (f *DeleteFuture) fulfill(resp *transport.Response, err error) {
	defer close(f.done)
	if err != nil {
		f.err = err
		return
	}
	if resp.Status == transport.StatusOk || resp.Status == transport.StatusKeyNotFound {
		return
	}
	f.err = fmt.Errorf("delete failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Result waits for the pipeline execution to complete and returns any error encountered.
func (f *DeleteFuture) Result() error {
	<-f.done
	return f.err
}

// ExistsFuture represents the pending asynchronous outcome of a pipelined Exists operation.
type ExistsFuture struct {
	key    []byte
	exists bool
	err    error
	done   chan struct{}
}

func (f *ExistsFuture) request() (*transport.Request, error) {
	if len(f.key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(f.key) > transport.MaxKeyLength {
		return nil, ErrKeyTooLarge
	}
	return &transport.Request{
		OpCode: transport.OpExists,
		Key:    f.key,
	}, nil
}

func (f *ExistsFuture) fulfill(resp *transport.Response, err error) {
	defer close(f.done)
	if err != nil {
		f.err = err
		return
	}
	if resp.Status == transport.StatusOk {
		f.exists = resp.Exists
		return
	}
	f.err = fmt.Errorf("exists failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Result waits for the pipeline execution to complete and returns whether the key exists or error.
func (f *ExistsFuture) Result() (bool, error) {
	<-f.done
	return f.exists, f.err
}

// BatchFuture represents the pending asynchronous outcome of a pipelined Batch operation.
type BatchFuture struct {
	batch WriteBatch
	err   error
	done  chan struct{}
}

func (f *BatchFuture) request() (*transport.Request, error) {
	if len(f.batch.ops) == 0 {
		return nil, ErrBatchEmpty
	}
	if len(f.batch.ops) > transport.MaxBatchOps {
		return nil, ErrBatchTooLarge
	}
	tOps := make([]transport.BatchOp, len(f.batch.ops))
	for i, op := range f.batch.ops {
		tOps[i] = transport.BatchOp{
			Type:  transport.BatchOpType(op.Type),
			Key:   op.Key,
			Value: op.Value,
		}
	}
	return &transport.Request{
		OpCode: transport.OpBatch,
		Batch:  tOps,
	}, nil
}

func (f *BatchFuture) fulfill(resp *transport.Response, err error) {
	defer close(f.done)
	if err != nil {
		f.err = err
		return
	}
	if resp.Status == transport.StatusOk {
		return
	}
	f.err = fmt.Errorf("batch failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Result waits for the pipeline execution to complete and returns any error encountered.
func (f *BatchFuture) Result() error {
	<-f.done
	return f.err
}

// StatsFuture represents the pending asynchronous outcome of a pipelined Stats operation.
type StatsFuture struct {
	stats *StatsSnapshot
	err   error
	done  chan struct{}
}

func (f *StatsFuture) request() (*transport.Request, error) {
	return &transport.Request{
		OpCode: transport.OpStats,
	}, nil
}

func (f *StatsFuture) fulfill(resp *transport.Response, err error) {
	defer close(f.done)
	if err != nil {
		f.err = err
		return
	}
	if resp.Status != transport.StatusOk {
		f.err = fmt.Errorf("stats failed (status 0x%02x): %s", resp.Status, resp.Message)
		return
	}
	if len(resp.Value) == 0 {
		f.err = errors.New("empty stats response payload")
		return
	}
	if len(resp.Value) > MaxStatsPayloadLength {
		f.err = fmt.Errorf("stats payload size %d exceeds maximum %d", len(resp.Value), MaxStatsPayloadLength)
		return
	}
	var snap StatsSnapshot
	if err := json.Unmarshal(resp.Value, &snap); err != nil {
		f.err = fmt.Errorf("failed to decode stats JSON payload: %w", err)
		return
	}
	f.stats = &snap
}

// Result waits for the pipeline execution to complete and returns the diagnostic snapshot or error.
func (f *StatsFuture) Result() (*StatsSnapshot, error) {
	<-f.done
	return f.stats, f.err
}

// Put enqueues a key-value write operation into the pipeline.
func (p *Pipeline) Put(key, value []byte) *PutFuture {
	fut := &PutFuture{
		key:   key,
		value: value,
		done:  make(chan struct{}),
	}
	p.ops = append(p.ops, fut)
	return fut
}

// Get enqueues a point lookup operation into the pipeline.
func (p *Pipeline) Get(key []byte) *GetFuture {
	fut := &GetFuture{
		key:  key,
		done: make(chan struct{}),
	}
	p.ops = append(p.ops, fut)
	return fut
}

// Delete enqueues a key deletion operation into the pipeline.
func (p *Pipeline) Delete(key []byte) *DeleteFuture {
	fut := &DeleteFuture{
		key:  key,
		done: make(chan struct{}),
	}
	p.ops = append(p.ops, fut)
	return fut
}

// Exists enqueues a key existence check into the pipeline.
func (p *Pipeline) Exists(key []byte) *ExistsFuture {
	fut := &ExistsFuture{
		key:  key,
		done: make(chan struct{}),
	}
	p.ops = append(p.ops, fut)
	return fut
}

// Batch enqueues an atomic WriteBatch operation into the pipeline.
func (p *Pipeline) Batch(batch WriteBatch) *BatchFuture {
	fut := &BatchFuture{
		batch: batch,
		done:  make(chan struct{}),
	}
	p.ops = append(p.ops, fut)
	return fut
}

// Stats enqueues a diagnostic statistics request into the pipeline.
func (p *Pipeline) Stats() *StatsFuture {
	fut := &StatsFuture{
		done: make(chan struct{}),
	}
	p.ops = append(p.ops, fut)
	return fut
}

// Len reports the number of operations currently enqueued in the pipeline.
func (p *Pipeline) Len() int {
	return len(p.ops)
}

// Execute transmits all enqueued requests back-to-back over the TCP connection,
// awaits all responses, and maps each response to its corresponding Future by SeqID.
// Does NOT impose artificial send-order execution on independent requests.
func (p *Pipeline) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.client.closed.Load() {
		return ErrClientClosed
	}
	if len(p.ops) == 0 {
		return ErrPipelineEmpty
	}
	if len(p.ops) > p.maxOps {
		return ErrPipelineTooLarge
	}

	p.client.mu.Lock()
	defer p.client.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if p.client.closed.Load() {
		return ErrClientClosed
	}

	type preparedOp struct {
		req *transport.Request
		op  pipelineOp
	}
	prepared := make([]preparedOp, 0, len(p.ops))
	pending := make(map[uint64]pipelineOp, len(p.ops))

	for _, op := range p.ops {
		req, err := op.request()
		if err != nil {
			for _, o := range p.ops {
				o.fulfill(nil, err)
			}
			p.ops = nil
			return err
		}
		req.SeqID = p.client.seqID.Add(1)
		prepared = append(prepared, preparedOp{req: req, op: op})
		pending[req.SeqID] = op
	}

	deadline := time.Now().Add(p.client.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = p.client.conn.SetDeadline(deadline)

	// Phase 1: Transmit all requests sequentially without waiting for responses
	for _, pop := range prepared {
		if err := transport.WriteRequest(p.client.conn, pop.req); err != nil {
			sendErr := fmt.Errorf("send pipelined request (seq %d): %w", pop.req.SeqID, err)
			for _, o := range pending {
				o.fulfill(nil, sendErr)
			}
			p.ops = nil
			return sendErr
		}
	}

	// Phase 2: Read exactly len(prepared) responses, correlating each by SeqID
	var firstErr error
	for i := 0; i < len(prepared); i++ {
		resp, err := transport.ReadResponse(p.client.conn)
		if err != nil {
			readErr := fmt.Errorf("read pipelined response (%d/%d): %w", i+1, len(prepared), err)
			for _, o := range pending {
				o.fulfill(nil, readErr)
			}
			p.ops = nil
			return readErr
		}

		op, exists := pending[resp.SeqID]
		if !exists {
			continue
		}
		delete(pending, resp.SeqID)
		op.fulfill(resp, nil)
	}

	for _, o := range pending {
		missingErr := errors.New("missing response for pipelined request")
		o.fulfill(nil, missingErr)
		if firstErr == nil {
			firstErr = missingErr
		}
	}

	p.ops = nil
	return firstErr
}

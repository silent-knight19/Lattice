package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

var (
	// ErrKeyNotFound indicates that the requested key does not exist.
	ErrKeyNotFound = errors.New("key not found")

	// ErrEmptyKey indicates that a zero-length key was provided.
	ErrEmptyKey = errors.New("key cannot be empty")

	// ErrKeyTooLarge indicates that a key exceeds the maximum allowed size.
	ErrKeyTooLarge = errors.New("key exceeds maximum allowed size")

	// ErrValueTooLarge indicates that a value exceeds the maximum allowed size.
	ErrValueTooLarge = errors.New("value exceeds maximum allowed size")

	// ErrBatchEmpty indicates that a WriteBatch contains no operations.
	ErrBatchEmpty = errors.New("batch cannot be empty")

	// ErrBatchTooLarge indicates that a WriteBatch exceeds the maximum operation limit.
	ErrBatchTooLarge = errors.New("batch exceeds maximum allowed operations")

	// ErrClientClosed indicates that an operation was attempted on a closed Client.
	ErrClientClosed = errors.New("client is closed")
)

// Options configures the Lattice client connection.
type Options struct {
	Timeout time.Duration
}

// DefaultOptions returns production default client options.
func DefaultOptions() Options {
	return Options{
		Timeout: 5 * time.Second,
	}
}

// Client provides a thread-safe connection to a Lattice cluster or standalone node over TCP.
type Client struct {
	conn    net.Conn
	timeout time.Duration
	seqID   atomic.Uint64
	mu      sync.Mutex
	closed  atomic.Bool
}

// Dial connects to a Lattice database node at the given TCP address using default options.
func Dial(address string) (*Client, error) {
	return DialWithOptions(address, DefaultOptions())
}

// DialWithOptions connects to a Lattice database node with custom options.
func DialWithOptions(address string, opts Options) (*Client, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}

	conn, err := net.DialTimeout("tcp", address, opts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", address, err)
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	return &Client{
		conn:    conn,
		timeout: opts.Timeout,
	}, nil
}

// Put writes or overwrites a key-value pair.
func (c *Client) Put(ctx context.Context, key, value []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > transport.MaxKeyLength {
		return ErrKeyTooLarge
	}
	if len(value) > transport.MaxValueLength {
		return ErrValueTooLarge
	}

	req := &transport.Request{
		OpCode: transport.OpPut,
		Key:    key,
		Value:  value,
	}

	resp, err := c.execute(ctx, req)
	if err != nil {
		return err
	}

	if resp.Status == transport.StatusOk {
		return nil
	}
	return fmt.Errorf("put failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Get retrieves the value associated with key.
// Returns ErrKeyNotFound if the key does not exist.
func (c *Client) Get(ctx context.Context, key []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, ErrEmptyKey
	}
	if len(key) > transport.MaxKeyLength {
		return nil, ErrKeyTooLarge
	}

	req := &transport.Request{
		OpCode: transport.OpGet,
		Key:    key,
	}

	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Status == transport.StatusOk {
		val := make([]byte, len(resp.Value))
		copy(val, resp.Value)
		return val, nil
	}
	if resp.Status == transport.StatusKeyNotFound {
		return nil, ErrKeyNotFound
	}
	return nil, fmt.Errorf("get failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Delete removes a key and its value.
func (c *Client) Delete(ctx context.Context, key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > transport.MaxKeyLength {
		return ErrKeyTooLarge
	}

	req := &transport.Request{
		OpCode: transport.OpDelete,
		Key:    key,
	}

	resp, err := c.execute(ctx, req)
	if err != nil {
		return err
	}

	if resp.Status == transport.StatusOk || resp.Status == transport.StatusKeyNotFound {
		return nil
	}
	return fmt.Errorf("delete failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Exists reports whether the specified key exists in the database.
// Returns (true, nil) if the key exists, (false, nil) if missing, or (false, err) on error.
func (c *Client) Exists(ctx context.Context, key []byte) (bool, error) {
	if len(key) == 0 {
		return false, ErrEmptyKey
	}
	if len(key) > transport.MaxKeyLength {
		return false, ErrKeyTooLarge
	}

	req := &transport.Request{
		OpCode: transport.OpExists,
		Key:    key,
	}

	resp, err := c.execute(ctx, req)
	if err != nil {
		return false, err
	}

	if resp.Status == transport.StatusOk {
		return resp.Exists, nil
	}
	if resp.Status == transport.StatusKeyNotFound {
		return false, nil
	}
	return false, fmt.Errorf("exists failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Batch atomically applies an ordered sequence of PUT and DELETE mutations in WriteBatch.
// Sends one logical OP_BATCH frame to the server.
func (c *Client) Batch(ctx context.Context, batch *WriteBatch) error {
	if batch == nil || len(batch.ops) == 0 {
		return ErrBatchEmpty
	}
	if len(batch.ops) > transport.MaxBatchOps {
		return ErrBatchTooLarge
	}

	tOps := make([]transport.BatchOp, len(batch.ops))
	for i, op := range batch.ops {
		if len(op.Key) == 0 {
			return fmt.Errorf("%w at index %d", ErrEmptyKey, i)
		}
		if len(op.Key) > transport.MaxKeyLength {
			return fmt.Errorf("%w at index %d", ErrKeyTooLarge, i)
		}
		if len(op.Value) > transport.MaxValueLength {
			return fmt.Errorf("%w at index %d", ErrValueTooLarge, i)
		}

		var topType transport.BatchOpType
		switch op.Type {
		case OpPut:
			topType = transport.BatchOpPut
		case OpDelete:
			topType = transport.BatchOpDelete
		default:
			return fmt.Errorf("invalid batch operation type 0x%02x at index %d", op.Type, i)
		}

		tOps[i] = transport.BatchOp{
			Type:  topType,
			Key:   op.Key,
			Value: op.Value,
		}
	}

	req := &transport.Request{
		OpCode: transport.OpBatch,
		Batch:  tOps,
	}

	resp, err := c.execute(ctx, req)
	if err != nil {
		return err
	}

	if resp.Status == transport.StatusOk {
		return nil
	}
	return fmt.Errorf("batch failed (status 0x%02x): %s", resp.Status, resp.Message)
}

// Stats retrieves an authoritative point-in-time diagnostic telemetry snapshot from the server.
func (c *Client) Stats(ctx context.Context) (*StatsSnapshot, error) {
	req := &transport.Request{
		OpCode: transport.OpStats,
	}

	resp, err := c.execute(ctx, req)
	if err != nil {
		return nil, err
	}

	if resp.Status != transport.StatusOk {
		return nil, fmt.Errorf("stats failed (status 0x%02x): %s", resp.Status, resp.Message)
	}

	if len(resp.Value) == 0 {
		return nil, errors.New("empty stats response payload")
	}
	if len(resp.Value) > MaxStatsPayloadLength {
		return nil, fmt.Errorf("stats payload size %d exceeds maximum %d", len(resp.Value), MaxStatsPayloadLength)
	}

	var snap StatsSnapshot
	if err := json.Unmarshal(resp.Value, &snap); err != nil {
		return nil, fmt.Errorf("failed to decode stats JSON payload: %w", err)
	}

	return &snap, nil
}

// Pipeline creates a new request pipeline for bounded multiplexed execution.
func (c *Client) Pipeline() *Pipeline {
	return c.PipelineWithMaxOps(DefaultMaxPipelineOps)
}

// PipelineWithMaxOps creates a new request pipeline with an explicit operations bound.
func (c *Client) PipelineWithMaxOps(maxOps int) *Pipeline {
	if maxOps <= 0 {
		maxOps = DefaultMaxPipelineOps
	}
	return &Pipeline{
		client: c,
		maxOps: maxOps,
	}
}

// Close closes the underlying TCP connection.
func (c *Client) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		if c.conn != nil {
			return c.conn.Close()
		}
	}
	return nil
}

// execute serializes a Request onto the wire and parses the matching Response.
func (c *Client) execute(ctx context.Context, req *transport.Request) (*transport.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed.Load() {
		return nil, ErrClientClosed
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed.Load() {
		return nil, ErrClientClosed
	}

	req.SeqID = c.seqID.Add(1)

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)

	if err := transport.WriteRequest(c.conn, req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	resp, err := transport.ReadResponse(c.conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.SeqID != req.SeqID {
		return nil, fmt.Errorf("response sequence ID mismatch: expected %d, got %d", req.SeqID, resp.SeqID)
	}
	if resp.OpCode != req.OpCode {
		return nil, fmt.Errorf("response opcode mismatch: expected %s, got %s", req.OpCode, resp.OpCode)
	}

	return resp, nil
}

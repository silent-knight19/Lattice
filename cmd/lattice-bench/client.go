package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

// BenchClient manages a single, persistent, worker-local TCP connection to the
// Lattice server for low-latency synchronous request-response benchmarking.
type BenchClient struct {
	conn    net.Conn
	timeout time.Duration
	seqID   uint64
}

// Dial establishes a persistent TCP connection to address with the specified timeout.
// Configures TCP_NODELAY and Keep-Alive to minimize latency distortion.
func Dial(address string, timeout time.Duration) (*BenchClient, error) {
	return DialContext(context.Background(), address, timeout)
}

// DialContext establishes a persistent TCP connection respecting ctx and timeout.
// Configures TCP_NODELAY and Keep-Alive to minimize latency distortion.
func DialContext(ctx context.Context, address string, timeout time.Duration) (*BenchClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var dialer net.Dialer
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		dialer.Deadline = ctxDeadline
	} else {
		dialer.Timeout = timeout
	}

	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", address, err)
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	return &BenchClient{
		conn:    conn,
		timeout: timeout,
	}, nil
}

// effectiveDeadline calculates the earliest deadline between per-operation timeout
// and the benchmark context deadline, ensuring network I/O never overruns duration.
func (c *BenchClient) effectiveDeadline(ctx context.Context) time.Time {
	opDeadline := time.Now().Add(c.timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(opDeadline) {
		return ctxDeadline
	}
	return opDeadline
}

// Get issues an OpGet request for key and awaits the verified server response.
// Network deadlines strictly respect min(now + timeout, ctx.Deadline()).
func (c *BenchClient) Get(ctx context.Context, key []byte) (*transport.Response, error) {
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("bench client is nil or closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.seqID++
	req := transport.Request{
		OpCode: transport.OpGet,
		SeqID:  c.seqID,
		Key:    key,
	}

	if err := c.conn.SetDeadline(c.effectiveDeadline(ctx)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if err := transport.WriteRequest(c.conn, &req); err != nil {
		return nil, fmt.Errorf("write get request: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.conn.SetDeadline(c.effectiveDeadline(ctx)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := transport.ReadResponse(c.conn)
	if err != nil {
		return nil, fmt.Errorf("read get response: %w", err)
	}

	if resp.SeqID != req.SeqID {
		return nil, fmt.Errorf("sequence ID mismatch: expected %d, got %d", req.SeqID, resp.SeqID)
	}
	if resp.OpCode != req.OpCode {
		return nil, fmt.Errorf("opcode mismatch: expected %s, got %s", req.OpCode, resp.OpCode)
	}

	return resp, nil
}

// Put issues an OpPut request for key and val and awaits the verified server response.
// Network deadlines strictly respect min(now + timeout, ctx.Deadline()).
func (c *BenchClient) Put(ctx context.Context, key, val []byte) (*transport.Response, error) {
	if c == nil || c.conn == nil {
		return nil, fmt.Errorf("bench client is nil or closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.seqID++
	req := transport.Request{
		OpCode: transport.OpPut,
		SeqID:  c.seqID,
		Key:    key,
		Value:  val,
	}

	if err := c.conn.SetDeadline(c.effectiveDeadline(ctx)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	if err := transport.WriteRequest(c.conn, &req); err != nil {
		return nil, fmt.Errorf("write put request: %w", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.conn.SetDeadline(c.effectiveDeadline(ctx)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	resp, err := transport.ReadResponse(c.conn)
	if err != nil {
		return nil, fmt.Errorf("read put response: %w", err)
	}

	if resp.SeqID != req.SeqID {
		return nil, fmt.Errorf("sequence ID mismatch: expected %d, got %d", req.SeqID, resp.SeqID)
	}
	if resp.OpCode != req.OpCode {
		return nil, fmt.Errorf("opcode mismatch: expected %s, got %s", req.OpCode, resp.OpCode)
	}

	return resp, nil
}

// Close terminates the client connection.
func (c *BenchClient) Close() error {
	if c != nil && c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

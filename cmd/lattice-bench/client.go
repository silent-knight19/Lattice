package main

import (
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
	conn, err := net.DialTimeout("tcp", address, timeout)
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

// Get issues an OpGet request for key and awaits the verified server response.
func (c *BenchClient) Get(key []byte) (*transport.Response, error) {
	c.seqID++
	req := transport.Request{
		OpCode: transport.OpGet,
		SeqID:  c.seqID,
		Key:    key,
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set write deadline: %w", err)
	}
	if err := transport.WriteRequest(c.conn, &req); err != nil {
		return nil, fmt.Errorf("write get request: %w", err)
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
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
func (c *BenchClient) Put(key, val []byte) (*transport.Response, error) {
	c.seqID++
	req := transport.Request{
		OpCode: transport.OpPut,
		SeqID:  c.seqID,
		Key:    key,
		Value:  val,
	}

	if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set write deadline: %w", err)
	}
	if err := transport.WriteRequest(c.conn, &req); err != nil {
		return nil, fmt.Errorf("write put request: %w", err)
	}

	if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
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
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

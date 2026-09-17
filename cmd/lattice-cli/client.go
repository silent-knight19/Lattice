package main

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
)

// LatticeClient defines the operational interface for communicating with a Lattice server.
type LatticeClient interface {
	// Execute transmits a typed Request and awaits a validated matching Response.
	Execute(req *transport.Request) (*transport.Response, error)

	// Close terminates the client connection.
	Close() error
}

// TCPClient manages a persistent TCP connection to a Lattice node, correlating
// requests and responses via monotonic sequence IDs.
type TCPClient struct {
	conn    net.Conn
	timeout time.Duration
	seqID   atomic.Uint64
}

// Dial establishes a TCP connection to the Lattice server at address with the specified timeout.
func Dial(address string, timeout time.Duration) (*TCPClient, error) {
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial tcp %s: %w", address, err)
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	return &TCPClient{
		conn:    conn,
		timeout: timeout,
	}, nil
}

// Execute assigns a monotonic sequence ID to req, writes it over TCP, reads
// the corresponding response, and validates that sequence IDs match.
func (c *TCPClient) Execute(req *transport.Request) (*transport.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("request cannot be nil")
	}

	// Assign monotonic sequence correlation ID
	req.SeqID = c.seqID.Add(1)

	// Write request with deadline
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set write deadline: %w", err)
	}

	if err := transport.WriteRequest(c.conn, req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Read response with deadline
	if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}

	resp, err := transport.ReadResponse(c.conn)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Verify sequence ID correlation
	if resp.SeqID != req.SeqID {
		return nil, fmt.Errorf("response sequence ID mismatch: expected %d, got %d", req.SeqID, resp.SeqID)
	}

	// Verify opcode correlation
	if resp.OpCode != req.OpCode {
		return nil, fmt.Errorf("response opcode mismatch: expected %s, got %s", req.OpCode, resp.OpCode)
	}

	return resp, nil
}

// Close closes the underlying TCP connection.
func (c *TCPClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

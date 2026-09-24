package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/pkg/client"
)

func TestPipeline_BasicEndToEnd(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// 1. Initial write through pipeline
	pipe := c.Pipeline()
	fPut1 := pipe.Put([]byte("p_key1"), []byte("p_val1"))
	fPut2 := pipe.Put([]byte("p_key2"), []byte("p_val2"))
	fStats := pipe.Stats()

	if pipe.Len() != 3 {
		t.Fatalf("expected len 3, got %d", pipe.Len())
	}

	if err := pipe.Execute(ctx); err != nil {
		t.Fatalf("pipeline execute failed: %v", err)
	}

	if err := fPut1.Result(); err != nil {
		t.Errorf("fPut1 result error: %v", err)
	}
	if err := fPut2.Result(); err != nil {
		t.Errorf("fPut2 result error: %v", err)
	}
	snap, err := fStats.Result()
	if err != nil {
		t.Errorf("fStats result error: %v", err)
	}
	if snap == nil || snap.Engine.State != "open" {
		t.Errorf("unexpected stats snapshot: %+v", snap)
	}

	// 2. Read back, check existence, and delete via second pipeline
	pipe2 := c.Pipeline()
	fGet1 := pipe2.Get([]byte("p_key1"))
	fExists1 := pipe2.Exists([]byte("p_key1"))
	fExistsMissing := pipe2.Exists([]byte("missing_key"))
	fDel := pipe2.Delete([]byte("p_key2"))

	if err := pipe2.Execute(ctx); err != nil {
		t.Fatalf("pipeline2 execute failed: %v", err)
	}

	val1, err := fGet1.Result()
	if err != nil || !bytes.Equal(val1, []byte("p_val1")) {
		t.Errorf("fGet1 mismatch: err=%v, val=%s", err, string(val1))
	}
	exists1, err := fExists1.Result()
	if err != nil || !exists1 {
		t.Errorf("fExists1 mismatch: err=%v, exists=%v", err, exists1)
	}
	existsMissing, err := fExistsMissing.Result()
	if err != nil || existsMissing {
		t.Errorf("fExistsMissing mismatch: err=%v, exists=%v", err, existsMissing)
	}
	if err := fDel.Result(); err != nil {
		t.Errorf("fDel error: %v", err)
	}

	// 3. Batch operation in pipeline
	pipe3 := c.Pipeline()
	batch := client.NewWriteBatch()
	batch.Put([]byte("b_k1"), []byte("b_v1"))
	batch.Put([]byte("b_k2"), []byte("b_v2"))
	fBatch := pipe3.Batch(*batch)
	fGetMissing := pipe3.Get([]byte("p_key2")) // deleted in pipe2

	if err := pipe3.Execute(ctx); err != nil {
		t.Fatalf("pipeline3 execute failed: %v", err)
	}

	if err := fBatch.Result(); err != nil {
		t.Errorf("fBatch error: %v", err)
	}
	_, err = fGetMissing.Result()
	if !errors.Is(err, client.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for deleted key, got: %v", err)
	}
}

func TestPipeline_EmptyPipelineError(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	err = pipe.Execute(context.Background())
	if !errors.Is(err, client.ErrPipelineEmpty) {
		t.Fatalf("expected ErrPipelineEmpty, got %v", err)
	}
}

func TestPipeline_TooLargeLimit(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Create pipeline with maxOps = 3
	pipe := c.PipelineWithMaxOps(3)
	pipe.Put([]byte("k1"), []byte("v1"))
	pipe.Put([]byte("k2"), []byte("v2"))
	pipe.Put([]byte("k3"), []byte("v3"))
	pipe.Put([]byte("k4"), []byte("v4"))

	err = pipe.Execute(context.Background())
	if !errors.Is(err, client.ErrPipelineTooLarge) {
		t.Fatalf("expected ErrPipelineTooLarge, got %v", err)
	}
}

func TestPipeline_ContextCancellation(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	pipe := c.Pipeline()
	pipe.Put([]byte("k"), []byte("v"))

	err = pipe.Execute(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestPipeline_ClosedClient(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()

	pipe := c.Pipeline()
	pipe.Put([]byte("k"), []byte("v"))

	err = pipe.Execute(context.Background())
	if !errors.Is(err, client.ErrClientClosed) {
		t.Fatalf("expected ErrClientClosed, got %v", err)
	}
}

func TestPipeline_ConcurrentPipelinesOnSameClient(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	const goroutines = 8
	const opsPerPipe = 10

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		gid := g
		go func() {
			defer wg.Done()

			// Phase 1: Pipeline of PUT mutations
			pipePut := c.Pipeline()
			var putFuts []*client.PutFuture
			for i := 0; i < opsPerPipe; i++ {
				k := []byte(fmt.Sprintf("conc_%d_%d", gid, i))
				v := []byte(fmt.Sprintf("val_%d_%d", gid, i))
				putFuts = append(putFuts, pipePut.Put(k, v))
			}

			if err := pipePut.Execute(context.Background()); err != nil {
				t.Errorf("goroutine %d pipePut.Execute: %v", gid, err)
				return
			}
			for i, fut := range putFuts {
				if err := fut.Result(); err != nil {
					t.Errorf("goroutine %d putFut %d error: %v", gid, i, err)
				}
			}

			// Phase 2: Pipeline of GET queries
			pipeGet := c.Pipeline()
			var getFuts []*client.GetFuture
			for i := 0; i < opsPerPipe; i++ {
				k := []byte(fmt.Sprintf("conc_%d_%d", gid, i))
				getFuts = append(getFuts, pipeGet.Get(k))
			}

			if err := pipeGet.Execute(context.Background()); err != nil {
				t.Errorf("goroutine %d pipeGet.Execute: %v", gid, err)
				return
			}

			for i, fut := range getFuts {
				val, err := fut.Result()
				expected := fmt.Sprintf("val_%d_%d", gid, i)
				if err != nil || string(val) != expected {
					t.Errorf("goroutine %d fut %d mismatch: err=%v, val=%s", gid, i, err, string(val))
				}
			}
		}()
	}

	wg.Wait()
}

func TestPipeline_Failure_PartialWrite(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Read first frame then close immediately to cause partial write on subsequent frame
		_, _ = transport.ReadRequest(conn)
		_ = conn.Close()
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	f1 := pipe.Put([]byte("k1"), []byte("v1"))
	f2 := pipe.Put([]byte("k2"), bytes.Repeat([]byte("v2"), 4096))
	f3 := pipe.Put([]byte("k3"), bytes.Repeat([]byte("v3"), 4096))

	err = pipe.Execute(context.Background())
	if err == nil {
		t.Fatalf("expected write error, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client connection to be marked closed")
	}
	if f1.Result() == nil || f2.Result() == nil || f3.Result() == nil {
		t.Errorf("expected all futures to terminate with error")
	}

	// Subsequent client operation must immediately fail with ErrClientClosed
	_, getErr := c.Get(context.Background(), []byte("k1"))
	if !errors.Is(getErr, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed for subsequent operation, got %v", getErr)
	}
}

func TestPipeline_Failure_ResponseTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = transport.ReadRequest(conn)
		// Delay longer than client timeout
		time.Sleep(300 * time.Millisecond)
	}()

	c, err := client.DialWithOptions(ln.Addr().String(), client.Options{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	fut := pipe.Put([]byte("k"), []byte("v"))

	err = pipe.Execute(context.Background())
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client connection to be invalidated")
	}
	if fut.Result() == nil {
		t.Errorf("expected future to terminate with error")
	}

	// Ensure subsequent operation fails closed
	getErr := c.Put(context.Background(), []byte("k2"), []byte("v2"))
	if !errors.Is(getErr, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", getErr)
	}
}

func TestPipeline_Failure_ContextCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = transport.ReadRequest(conn)
		time.Sleep(300 * time.Millisecond)
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	pipe := c.Pipeline()
	fut := pipe.Put([]byte("k"), []byte("v"))

	err = pipe.Execute(ctx)
	if err == nil {
		t.Fatalf("expected context error, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client connection to be marked closed")
	}
	if fut.Result() == nil {
		t.Errorf("expected future to terminate with error")
	}
}

func TestPipeline_Failure_ServerDisconnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req1, _ := transport.ReadRequest(conn)
		_, _ = transport.ReadRequest(conn)

		// Send 1 response then close abruptly
		_ = transport.WriteResponse(conn, &transport.Response{
			OpCode: req1.OpCode,
			SeqID:  req1.SeqID,
			Status: transport.StatusOk,
		})
		_ = conn.Close()
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	f1 := pipe.Put([]byte("k1"), []byte("v1"))
	f2 := pipe.Put([]byte("k2"), []byte("v2"))

	err = pipe.Execute(context.Background())
	if err == nil {
		t.Fatalf("expected read error due to disconnect, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client to be closed")
	}
	if f1.Result() != nil {
		t.Errorf("expected f1 to succeed before disconnect, got: %v", f1.Result())
	}
	if f2.Result() == nil {
		t.Errorf("expected f2 to terminate with error")
	}

	_, getErr := c.Get(context.Background(), []byte("k1"))
	if !errors.Is(getErr, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", getErr)
	}
}

func TestPipeline_Failure_UnexpectedSeqID(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, _ := transport.ReadRequest(conn)
		// Respond with unexpected SeqID
		_ = transport.WriteResponse(conn, &transport.Response{
			OpCode: req.OpCode,
			SeqID:  req.SeqID + 9999,
			Status: transport.StatusOk,
		})
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	fut := pipe.Put([]byte("k"), []byte("v"))

	err = pipe.Execute(context.Background())
	if err == nil {
		t.Fatalf("expected unexpected seq ID error, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client to fail closed")
	}
	if fut.Result() == nil {
		t.Errorf("expected future to terminate with error")
	}
}

func TestPipeline_Failure_WrongOpCode(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req, _ := transport.ReadRequest(conn)
		// Sent OpPut (0x01), server responds with OpDelete (0x03)
		_ = transport.WriteResponse(conn, &transport.Response{
			OpCode: transport.OpDelete,
			SeqID:  req.SeqID,
			Status: transport.StatusOk,
		})
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	fut := pipe.Put([]byte("k"), []byte("v"))

	err = pipe.Execute(context.Background())
	if err == nil {
		t.Fatalf("expected opcode mismatch error, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client to fail closed on opcode mismatch")
	}
	if fut.Result() == nil {
		t.Errorf("expected future to terminate with error")
	}
}

func TestPipeline_Failure_DuplicateSeqIDResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		req1, _ := transport.ReadRequest(conn)
		_, _ = transport.ReadRequest(conn)
		// Send response for req1
		_ = transport.WriteResponse(conn, &transport.Response{
			OpCode: req1.OpCode,
			SeqID:  req1.SeqID,
			Status: transport.StatusOk,
		})
		// Send DUPLICATE response for req1 instead of req2
		_ = transport.WriteResponse(conn, &transport.Response{
			OpCode: req1.OpCode,
			SeqID:  req1.SeqID,
			Status: transport.StatusOk,
		})
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	pipe := c.Pipeline()
	f1 := pipe.Put([]byte("k1"), []byte("v1"))
	f2 := pipe.Put([]byte("k2"), []byte("v2"))

	err = pipe.Execute(context.Background())
	if err == nil {
		t.Fatalf("expected duplicate SeqID error, got nil")
	}
	if !c.IsClosed() {
		t.Errorf("expected client to fail closed on duplicate response SeqID")
	}
	if f1.Result() != nil {
		t.Errorf("f1 should have succeeded, got %v", f1.Result())
	}
	if f2.Result() == nil {
		t.Errorf("f2 should have failed with error")
	}
}

func TestPipeline_SubsequentOperationNeverConsumesStaleResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()

	c, err := client.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	serverConn := <-serverConnCh
	defer serverConn.Close()

	// Enqueue 2 requests in pipeline
	pipe := c.Pipeline()
	f1 := pipe.Put([]byte("poison_k1"), []byte("poison_v1"))
	f2 := pipe.Put([]byte("poison_k2"), []byte("poison_v2"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var execErr error
	execDone := make(chan struct{})
	go func() {
		defer close(execDone)
		execErr = pipe.Execute(ctx)
	}()

	// Read both requests on server side
	req1, err := transport.ReadRequest(serverConn)
	if err != nil {
		t.Fatalf("read req1: %v", err)
	}
	req2, err := transport.ReadRequest(serverConn)
	if err != nil {
		t.Fatalf("read req2: %v", err)
	}

	// Client execute will time out because server has not responded
	<-execDone
	if execErr == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !c.IsClosed() {
		t.Fatalf("client connection must be marked closed")
	}

	// Server now emits response for req1 and req2
	_ = transport.WriteResponse(serverConn, &transport.Response{
		OpCode: req1.OpCode,
		SeqID:  req1.SeqID,
		Status: transport.StatusOk,
	})
	_ = transport.WriteResponse(serverConn, &transport.Response{
		OpCode: req2.OpCode,
		SeqID:  req2.SeqID,
		Status: transport.StatusOk,
	})

	// Subsequent client operation must immediately fail with ErrClientClosed
	// and NEVER read req1 or req2 response off the socket
	_, getErr := c.Get(context.Background(), []byte("poison_k1"))
	if !errors.Is(getErr, client.ErrClientClosed) {
		t.Fatalf("expected ErrClientClosed preventing stale response consumption, got: %v", getErr)
	}
	_ = f1.Result()
	_ = f2.Result()
}

func TestPipeline_ModelB_SingleOwnerContract(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Model B contract:
	// 1. Client is thread-safe: multiple goroutines can safely construct and execute their own independent pipelines.
	// 2. An individual *Pipeline instance is NOT thread-safe: it has a single owner.
	const concurrentClients = 4
	var wg sync.WaitGroup
	wg.Add(concurrentClients)

	for g := 0; g < concurrentClients; g++ {
		gid := g
		go func() {
			defer wg.Done()
			pipe := c.Pipeline() // Single owner: each goroutine has its own pipeline
			pipe.Put([]byte(fmt.Sprintf("mb_%d", gid)), []byte("val"))
			if err := pipe.Execute(context.Background()); err != nil {
				t.Errorf("goroutine %d execute: %v", gid, err)
			}
		}()
	}

	wg.Wait()
}

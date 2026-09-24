package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

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

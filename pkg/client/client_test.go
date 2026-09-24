package client_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/transport"
	"github.com/silent-knight19/lattice/pkg/client"
)

func startServer(t *testing.T) (*transport.Server, string, func()) {
	t.Helper()
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}

	cfg := transport.DefaultServerConfig()
	srv, err := transport.NewServer(cfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := srv.Addr().String()

	cleanup := func() {
		_ = srv.Close()
		_ = eng.Close()
	}

	return srv, addr, cleanup
}

func TestWriteBatch_Builder(t *testing.T) {
	b := client.NewWriteBatch()
	if b.Len() != 0 {
		t.Errorf("expected len 0, got %d", b.Len())
	}
	if b.Ops() != nil {
		t.Errorf("expected nil ops, got %v", b.Ops())
	}

	key := []byte("key1")
	val := []byte("val1")
	b.Put(key, val)
	b.Delete([]byte("key2"))

	if b.Len() != 2 {
		t.Fatalf("expected len 2, got %d", b.Len())
	}

	// Mutating original slice shouldn't mutate batch
	key[0] = 'X'
	ops := b.Ops()
	if string(ops[0].Key) != "key1" {
		t.Errorf("expected defensive copy for key, got %s", string(ops[0].Key))
	}
	if ops[0].Type != client.OpPut {
		t.Errorf("expected OpPut, got %v", ops[0].Type)
	}
	if ops[1].Type != client.OpDelete {
		t.Errorf("expected OpDelete, got %v", ops[1].Type)
	}

	b.Clear()
	if b.Len() != 0 {
		t.Errorf("expected len 0 after clear, got %d", b.Len())
	}
}

func TestClient_Validation(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// Empty key
	if err := c.Put(ctx, nil, []byte("v")); !errors.Is(err, client.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
	if _, err := c.Get(ctx, []byte{}); !errors.Is(err, client.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
	if err := c.Delete(ctx, []byte{}); !errors.Is(err, client.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}

	// Empty batch
	if err := c.Batch(ctx, nil); !errors.Is(err, client.ErrBatchEmpty) {
		t.Errorf("expected ErrBatchEmpty, got %v", err)
	}
	if err := c.Batch(ctx, client.NewWriteBatch()); !errors.Is(err, client.ErrBatchEmpty) {
		t.Errorf("expected ErrBatchEmpty, got %v", err)
	}

	// Batch op empty key
	b := client.NewWriteBatch().Put([]byte{}, []byte("v"))
	if err := c.Batch(ctx, b); !errors.Is(err, client.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
}

func TestClient_EndToEnd_CRUD_And_Batch(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// 1. Initial Put / Get
	if err := c.Put(ctx, []byte("user:1"), []byte("Alice")); err != nil {
		t.Fatalf("put failed: %v", err)
	}
	val, err := c.Get(ctx, []byte("user:1"))
	if err != nil || !bytes.Equal(val, []byte("Alice")) {
		t.Fatalf("get failed: val=%s, err=%v", string(val), err)
	}

	// 2. Atomic Batch
	batch := client.NewWriteBatch().
		Put([]byte("user:2"), []byte("Bob")).
		Put([]byte("user:3"), []byte("Charlie")).
		Delete([]byte("user:1")).
		Put([]byte("user:2"), []byte("Robert")) // Overwrites Bob within same batch

	if err := c.Batch(ctx, batch); err != nil {
		t.Fatalf("batch failed: %v", err)
	}

	// user:1 should be deleted
	_, err = c.Get(ctx, []byte("user:1"))
	if !errors.Is(err, client.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for user:1, got %v", err)
	}

	// user:2 should be Robert
	val2, err := c.Get(ctx, []byte("user:2"))
	if err != nil || !bytes.Equal(val2, []byte("Robert")) {
		t.Errorf("expected Robert for user:2, got %s (err=%v)", string(val2), err)
	}

	// user:3 should be Charlie
	val3, err := c.Get(ctx, []byte("user:3"))
	if err != nil || !bytes.Equal(val3, []byte("Charlie")) {
		t.Errorf("expected Charlie for user:3, got %s (err=%v)", string(val3), err)
	}

	// 3. Delete user:2
	if err := c.Delete(ctx, []byte("user:2")); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	_, err = c.Get(ctx, []byte("user:2"))
	if !errors.Is(err, client.ErrKeyNotFound) {
		t.Errorf("expected ErrKeyNotFound for user:2, got %v", err)
	}
}

func TestClient_ConcurrentAccess(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		workerID := byte(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				key := []byte{workerID, byte(j)}
				b := client.NewWriteBatch().
					Put(key, []byte("val1")).
					Put(key, []byte("val2"))

				if err := c.Batch(ctx, b); err != nil {
					t.Errorf("concurrent batch failed: %v", err)
					return
				}

				val, err := c.Get(ctx, key)
				if err != nil || !bytes.Equal(val, []byte("val2")) {
					t.Errorf("concurrent get mismatch: %v (val=%s)", err, string(val))
					return
				}
			}
		}()
	}

	wg.Wait()
}

func TestClient_Close(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	ctx := context.Background()
	if err := c.Put(ctx, []byte("k"), []byte("v")); !errors.Is(err, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", err)
	}
	if _, err := c.Get(ctx, []byte("k")); !errors.Is(err, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", err)
	}
	if err := c.Delete(ctx, []byte("k")); !errors.Is(err, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", err)
	}
	b := client.NewWriteBatch().Put([]byte("k"), []byte("v"))
	if err := c.Batch(ctx, b); !errors.Is(err, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", err)
	}
	if _, err := c.Exists(ctx, []byte("k")); !errors.Is(err, client.ErrClientClosed) {
		t.Errorf("expected ErrClientClosed, got %v", err)
	}
}

func TestClient_Exists(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// Missing key returns false
	exists, err := c.Exists(ctx, []byte("absent"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Errorf("expected false for absent key")
	}

	// Put key
	if err := c.Put(ctx, []byte("present"), []byte("val")); err != nil {
		t.Fatalf("put failed: %v", err)
	}

	// Existing key returns true
	exists, err = c.Exists(ctx, []byte("present"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Errorf("expected true for present key")
	}

	// Delete key
	if err := c.Delete(ctx, []byte("present")); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	// Deleted key returns false
	exists, err = c.Exists(ctx, []byte("present"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Errorf("expected false for deleted key")
	}

	// Binary key
	binKey := []byte{0x00, 0xff, 0x01, 0xfe}
	if err := c.Put(ctx, binKey, []byte("binval")); err != nil {
		t.Fatalf("put bin key failed: %v", err)
	}
	exists, err = c.Exists(ctx, binKey)
	if err != nil || !exists {
		t.Errorf("expected true for bin key, got %v, err=%v", exists, err)
	}

	// Validation
	if _, err := c.Exists(ctx, nil); !errors.Is(err, client.ErrEmptyKey) {
		t.Errorf("expected ErrEmptyKey, got %v", err)
	}
	if _, err := c.Exists(ctx, make([]byte, 65536)); !errors.Is(err, client.ErrKeyTooLarge) {
		t.Errorf("expected ErrKeyTooLarge, got %v", err)
	}
}

func TestClient_Stats(t *testing.T) {
	_, addr, cleanup := startServer(t)
	defer cleanup()

	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer c.Close()

	ctx := context.Background()

	// 1. Initial Stats
	snap, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats failed: %v", err)
	}
	if snap == nil {
		t.Fatalf("expected non-nil StatsSnapshot")
	}
	if snap.Engine.State != "open" {
		t.Errorf("expected engine state 'open', got %q", snap.Engine.State)
	}
	if snap.Connections.Active < 1 {
		t.Errorf("expected active connections >= 1, got %d", snap.Connections.Active)
	}

	// 2. Put some keys and verify memory stats reflect
	for i := 0; i < 5; i++ {
		if err := c.Put(ctx, []byte(fmt.Sprintf("k%d", i)), []byte("val")); err != nil {
			t.Fatalf("Put failed: %v", err)
		}
	}
	snap2, err := c.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after Put failed: %v", err)
	}
	if snap2.Memory.ActiveMemTableEntries < 5 {
		t.Errorf("expected >= 5 active entries, got %d", snap2.Memory.ActiveMemTableEntries)
	}
	if snap2.Storage.ActiveWALSegmentBytes == 0 {
		t.Errorf("expected ActiveWALSegmentBytes > 0 after Put, got 0")
	}
	if snap2.Storage.WALBytesWritten == 0 {
		t.Errorf("expected WALBytesWritten > 0 after Put, got 0")
	}

	// 3. Context cancelled
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Stats(canceledCtx); err == nil {
		t.Errorf("expected error with canceled context, got nil")
	}

	// 4. Closed client returns error
	c2, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	_ = c2.Close()
	if _, err := c2.Stats(ctx); err == nil {
		t.Errorf("expected error on closed client, got nil")
	}
}

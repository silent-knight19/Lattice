package engine_test

import (
	"bytes"
	"context"
	stdErrors "errors"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
)

func testCtx() context.Context { return context.Background() }

func newMemEngine() *engine.Engine {
	return engine.NewEngineWithOptions(engine.EngineOptions{
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 64 * 1024 * 1024,
			HighWatermark:  0.80,
			HardWatermark:  0.95,
			MaxWaitTimeout: 100 * time.Millisecond,
		},
	})
}

func mustGet(t *testing.T, eng *engine.Engine, key string) []byte {
	t.Helper()
	val, err := eng.Get([]byte(key))
	if err != nil {
		t.Fatalf("Get(%q) failed: %v", key, err)
	}
	return val
}

func mustNotFound(t *testing.T, eng *engine.Engine, key string) {
	t.Helper()
	_, err := eng.Get([]byte(key))
	if err == nil {
		t.Fatalf("Get(%q) expected not-found, got success", key)
	}
	if !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("Get(%q) expected ErrKeyNotFound, got %v", key, err)
	}
}

func TestEngineM01_BasicCRUD(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Put -> Get
	if err := eng.Put(ctx, []byte("k1"), []byte("v1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if got := mustGet(t, eng, "k1"); !bytes.Equal(got, []byte("v1")) {
		t.Fatalf("Get mismatch: got %q", got)
	}

	// Put -> Put -> Get returns V2
	if err := eng.Put(ctx, []byte("k1"), []byte("v2")); err != nil {
		t.Fatalf("Put overwrite failed: %v", err)
	}
	if got := mustGet(t, eng, "k1"); !bytes.Equal(got, []byte("v2")) {
		t.Fatalf("overwrite mismatch: got %q", got)
	}

	// Put -> Delete -> Get => not found
	if err := eng.Put(ctx, []byte("k2"), []byte("vx")); err != nil {
		t.Fatalf("Put k2 failed: %v", err)
	}
	if err := eng.Delete(ctx, []byte("k2")); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	mustNotFound(t, eng, "k2")

	// Delete nonexistent still succeeds (tombstone) and reads as not-found
	if err := eng.Delete(ctx, []byte("ghost")); err != nil {
		t.Fatalf("Delete nonexistent failed: %v", err)
	}
	mustNotFound(t, eng, "ghost")

	// Put/Delete/Put => V2
	if err := eng.Put(ctx, []byte("k3"), []byte("v1")); err != nil {
		t.Fatalf("Put k3 failed: %v", err)
	}
	if err := eng.Delete(ctx, []byte("k3")); err != nil {
		t.Fatalf("Delete k3 failed: %v", err)
	}
	mustNotFound(t, eng, "k3")
	if err := eng.Put(ctx, []byte("k3"), []byte("v2")); err != nil {
		t.Fatalf("re-Put k3 failed: %v", err)
	}
	if got := mustGet(t, eng, "k3"); !bytes.Equal(got, []byte("v2")) {
		t.Fatalf("re-Put mismatch: got %q", got)
	}

	// Put/Put/Delete => not found
	if err := eng.Put(ctx, []byte("k4"), []byte("a")); err != nil {
		t.Fatalf("Put k4 failed: %v", err)
	}
	if err := eng.Put(ctx, []byte("k4"), []byte("b")); err != nil {
		t.Fatalf("Put k4 overwrite failed: %v", err)
	}
	if err := eng.Delete(ctx, []byte("k4")); err != nil {
		t.Fatalf("Delete k4 failed: %v", err)
	}
	mustNotFound(t, eng, "k4")
}

func TestEngineM01_KeyValidation(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// nil / empty key rejected on all paths
	if err := eng.Put(ctx, nil, []byte("v")); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("Put nil key expected ErrEmptyKey, got %v", err)
	}
	if err := eng.Put(ctx, []byte{}, []byte("v")); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("Put empty key expected ErrEmptyKey, got %v", err)
	}
	if err := eng.Delete(ctx, nil); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("Delete nil key expected ErrEmptyKey, got %v", err)
	}
	if _, err := eng.Get(nil); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("Get nil key expected ErrEmptyKey, got %v", err)
	}

	// oversized key rejected
	big := make([]byte, binary.MaxKeyLen+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := eng.Put(ctx, big, []byte("v")); !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("Put oversized expected ErrKeyTooLarge, got %v", err)
	}

	// max valid key accepted
	maxK := make([]byte, binary.MaxKeyLen)
	for i := range maxK {
		maxK[i] = 'm'
	}
	if err := eng.Put(ctx, maxK, []byte("ok")); err != nil {
		t.Fatalf("Put max key failed: %v", err)
	}
	val, err := eng.Get(maxK)
	if err != nil || !bytes.Equal(val, []byte("ok")) {
		t.Fatalf("Get max key failed: val=%q err=%v", val, err)
	}

	// binary keys (non-UTF8) round-trip
	binK := []byte{0x00, 0x01, 0xff, 0xfe, 0x80}
	if err := eng.Put(ctx, binK, []byte("bin")); err != nil {
		t.Fatalf("Put binary key failed: %v", err)
	}
	if got := mustGet(t, eng, string(binK)); !bytes.Equal(got, []byte("bin")) {
		t.Fatalf("binary key mismatch: %q", got)
	}

	// oversized value rejected, not treated as delete
	bigV := make([]byte, binary.MaxValueLen+1)
	if err := eng.Put(ctx, []byte("kv"), bigV); !stdErrors.Is(err, errors.ErrValueTooLarge) {
		t.Fatalf("Put oversized value expected ErrValueTooLarge, got %v", err)
	}
}

func TestEngineM01_EmptyValuesAreNotDeletes(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	if err := eng.Put(ctx, []byte("e1"), nil); err != nil {
		t.Fatalf("Put nil value failed: %v", err)
	}
	val, err := eng.Get([]byte("e1"))
	if err != nil {
		t.Fatalf("Get empty value failed: %v", err)
	}
	if len(val) != 0 {
		t.Fatalf("expected empty value, got %q", val)
	}

	if err := eng.Put(ctx, []byte("e2"), []byte{}); err != nil {
		t.Fatalf("Put empty slice failed: %v", err)
	}
	val, err = eng.Get([]byte("e2"))
	if err != nil {
		t.Fatalf("Get e2 failed: %v", err)
	}
	if len(val) != 0 {
		t.Fatalf("expected empty value for e2, got %q", val)
	}
}

func TestEngineM01_BufferOwnership(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// Mutating input after Put must not affect stored state.
	key := []byte("own")
	val := []byte("original")
	if err := eng.Put(ctx, key, val); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	val[0] = 'X'
	key[0] = 'X'
	got, err := eng.Get([]byte("own"))
	if err != nil || !bytes.Equal(got, []byte("original")) {
		t.Fatalf("input aliasing: got %q err %v", got, err)
	}

	// Mutating Get output must not corrupt stored state.
	out, err := eng.Get([]byte("own"))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if len(out) > 0 {
		out[0] = 'Z'
	}
	again, err := eng.Get([]byte("own"))
	if err != nil || !bytes.Equal(again, []byte("original")) {
		t.Fatalf("output aliasing: got %q err %v", again, err)
	}
}

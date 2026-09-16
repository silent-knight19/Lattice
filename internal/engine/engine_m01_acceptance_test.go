package engine_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/cache"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
)

// newAcceptanceEngine builds a WAL-less Engine with a real DB directory,
// shared block cache, and no recovery overhead for the 50k bulk workload.
// WAL durability is proven separately by small WAL-backed tests; the bulk
// workload still exercises real SSTable files + VersionSet + TableReader +
// BlockCache for the disk path (no mocks).
func newAcceptanceEngine(t *testing.T) (*engine.Engine, string) {
	t.Helper()
	dir := t.TempDir()
	bc, err := cache.NewShardedCache(512)
	if err != nil {
		t.Fatalf("NewShardedCache failed: %v", err)
	}
	eng := engine.NewEngineWithOptions(engine.EngineOptions{
		DBPath: dir,
		Backpressure: engine.BackpressureConfig{
			MaxMemoryBytes: 256 * 1024 * 1024,
			HighWatermark:  0.80,
			HardWatermark:  0.95,
			MaxWaitTimeout: 5 * time.Second,
		},
		BlockCache: bc,
	})
	return eng, dir
}

// TestEngineM01_Acceptance50k runs a deterministic 50,000-operation mixed
// workload against a reference model, exercising both memory and real
// persisted SSTable state (no mocks for the disk path).
//
// Workload: 40% PUT, 40% GET, 20% DELETE over 3000 hot keys plus 500 cold
// disk-only keys. Pre-populated L0 SSTable holds 500 cold keys (never mutated,
// proving disk-only reads) and 500 overlapping keys (proving mem-shadows-disk
// and tombstone-shadows-disk).
func TestEngineM01_Acceptance50k(t *testing.T) {
	eng, dir := newAcceptanceEngine(t)
	defer func() { _ = eng.Close() }()
	ctx := testCtx()

	// --- Persist cold + overlapping keys to L0 ---
	coldKeys := make([]string, 0, 500)
	coldVals := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		coldKeys = append(coldKeys, fmt.Sprintf("cold_%04d", i))
		coldVals = append(coldVals, fmt.Sprintf("coldval_%04d", i))
	}
	buildSSTable(t, eng, dir, 10, 0, coldKeys, coldVals, 1)

	overlapKeys := make([]string, 0, 500)
	overlapVals := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		overlapKeys = append(overlapKeys, fmt.Sprintf("key_%05d", i))
		overlapVals = append(overlapVals, fmt.Sprintf("disk_v0_%d", i))
	}
	buildSSTable(t, eng, dir, 11, 0, overlapKeys, overlapVals, 1000)

	// Reference model: independent map (copies, no Engine internals).
	ref := make(map[string][]byte, 4000)
	for i := range coldKeys {
		ref[coldKeys[i]] = []byte(coldVals[i])
	}
	for i := range overlapKeys {
		ref[overlapKeys[i]] = []byte(overlapVals[i])
	}

	const total = 50000
	var puts, gets, dels, getHits, getMisses int
	for i := 0; i < total; i++ {
		keyIdx := (i * 7919) % 3000
		key := fmt.Sprintf("key_%05d", keyIdx)
		op := i % 10
		switch {
		case op < 4: // PUT 40%
			puts++
			val := fmt.Sprintf("val_%d_%d", i, keyIdx)
			if err := eng.Put(ctx, []byte(key), []byte(val)); err != nil {
				t.Fatalf("op %d Put(%s) failed: %v", i, key, err)
			}
			cp := make([]byte, len(val))
			copy(cp, val)
			ref[key] = []byte(cp)
		case op < 8: // GET 40%
			gets++
			got, err := eng.Get([]byte(key))
			want, ok := ref[key]
			if !ok {
				getMisses++
				if !stdErrors.Is(err, errors.ErrKeyNotFound) {
					t.Fatalf("op %d Get(%s): want NotFound, got val=%q err=%v", i, key, got, err)
				}
			} else {
				getHits++
				if err != nil {
					t.Fatalf("op %d Get(%s) failed: %v", i, key, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("op %d Get(%s)=%q want %q", i, key, got, want)
				}
			}
		default: // DELETE 20%
			dels++
			if err := eng.Delete(ctx, []byte(key)); err != nil {
				t.Fatalf("op %d Delete(%s) failed: %v", i, key, err)
			}
			delete(ref, key)
		}
	}
	t.Logf("50k workload: puts=%d gets=%d (hits=%d misses=%d) dels=%d", puts, gets, getHits, getMisses, dels)
	if puts != 20000 || gets != 20000 || dels != 10000 {
		t.Fatalf("unexpected distribution: puts=%d gets=%d dels=%d", puts, gets, dels)
	}

	// Final state verification across all keys (cold + workload range).
	for i := 0; i < 500; i++ {
		k := fmt.Sprintf("cold_%04d", i)
		got, err := eng.Get([]byte(k))
		if err != nil || string(got) != fmt.Sprintf("coldval_%04d", i) {
			t.Fatalf("cold key %s corrupted: got %q err %v", k, got, err)
		}
	}
	for idx := 0; idx < 3000; idx++ {
		k := fmt.Sprintf("key_%05d", idx)
		want, ok := ref[k]
		got, err := eng.Get([]byte(k))
		if !ok {
			if !stdErrors.Is(err, errors.ErrKeyNotFound) {
				t.Fatalf("final %s: want NotFound, got %q err %v", k, got, err)
			}
		} else {
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("final %s: got %q err %v want %q", k, got, err, want)
			}
		}
	}
}

// TestEngineM01_MemoryDiskParity verifies the same logical dataset reads
// identically from memory (fresh Puts) and from persisted SSTables.
func TestEngineM01_MemoryDiskParity(t *testing.T) {
	// Engine A: pure memory.
	memEng := newMemEngine()
	defer func() { _ = memEng.Close() }()
	ctx := testCtx()
	dataset := map[string]string{"p1": "v1", "p2": "v2", "p3": ""}
	for k, v := range dataset {
		if err := memEng.Put(ctx, []byte(k), []byte(v)); err != nil {
			t.Fatalf("mem Put failed: %v", err)
		}
	}
	if err := memEng.Delete(ctx, []byte("gone")); err != nil {
		t.Fatalf("mem Delete failed: %v", err)
	}

	// Engine B: same logical dataset persisted to SSTable.
	diskEng, dir := newDiskEngine(t, 128)
	defer func() { _ = diskEng.Close() }()
	keys := []string{"p1", "p2", "p3"}
	vals := []string{"v1", "v2", ""}
	buildSSTable(t, diskEng, dir, 20, 0, keys, vals, 1)
	// Tombstone for "gone" in memory shadows (no disk value) => NotFound on both.
	if err := diskEng.Delete(ctx, []byte("gone")); err != nil {
		t.Fatalf("disk Delete failed: %v", err)
	}

	for k, want := range dataset {
		a, errA := memEng.Get([]byte(k))
		b, errB := diskEng.Get([]byte(k))
		if (errA == nil) != (errB == nil) || string(a) != want || string(b) != want {
			t.Fatalf("parity %s: mem=(%q,%v) disk=(%q,%v) want %q", k, a, errA, b, errB, want)
		}
	}
	if _, err := memEng.Get([]byte("gone")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("mem gone should be NotFound")
	}
	if _, err := diskEng.Get([]byte("gone")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("disk gone should be NotFound")
	}
	if _, err := memEng.Get([]byte("absent")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("mem absent should be NotFound")
	}
	if _, err := diskEng.Get([]byte("absent")); !stdErrors.Is(err, errors.ErrKeyNotFound) {
		t.Fatalf("disk absent should be NotFound")
	}
}

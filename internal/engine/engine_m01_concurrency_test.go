package engine_test

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEngineM01_ConcurrentMixed(t *testing.T) {
	eng, _ := newDiskEngine(t, 128)
	defer func() { _ = eng.Close() }()

	const writers = 4
	const readers = 4
	const perWriter = 200

	var wg sync.WaitGroup
	errCh := make(chan error, writers*perWriter+readers*perWriter)

	// Writers: disjoint key ranges to keep assertions deterministic.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ctx := testCtx()
			for i := 0; i < perWriter; i++ {
				k := []byte(fmt.Sprintf("w%d/key_%04d", w, i))
				v := []byte(fmt.Sprintf("v%d_%d", w, i))
				if err := eng.Put(ctx, k, v); err != nil {
					errCh <- err
					return
				}
				// Immediate read-your-write (sequential per goroutine).
				got, err := eng.Get(k)
				if err != nil {
					errCh <- err
					return
				}
				if string(got) != string(v) {
					errCh <- fmt.Errorf("read-your-write mismatch for %s", k)
					return
				}
			}
		}(w)
	}
	// Readers: read keys being written (may legitimately miss if not yet written).
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				k := []byte(fmt.Sprintf("w%d/key_%04d", r%writers, i))
				_, _ = eng.Get(k)
				time.Sleep(time.Microsecond)
			}
		}(r)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent error: %v", err)
	}

	// Final verification: all written keys present.
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			k := fmt.Sprintf("w%d/key_%04d", w, i)
			want := fmt.Sprintf("v%d_%d", w, i)
			got, err := eng.Get([]byte(k))
			if err != nil || string(got) != want {
				t.Fatalf("final Get(%s)=%q err=%v want %q", k, got, err, want)
			}
		}
	}
}

func TestEngineM01_SameKeyContention(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()

	const n = 8
	const ops = 100
	var wg sync.WaitGroup
	for g := 0; g < n; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := testCtx()
			for i := 0; i < ops; i++ {
				_ = eng.Put(ctx, []byte("hot"), []byte(fmt.Sprintf("g%d_i%d", g, i)))
				_, _ = eng.Get([]byte("hot"))
				if i%10 == 0 {
					_ = eng.Delete(ctx, []byte("hot"))
				}
			}
		}(g)
	}
	wg.Wait()
	// No deterministic value assertion: only invariant is no race/panic and
	// Get returns either a value or NotFound without corruption error.
	_, _ = eng.Get([]byte("hot"))
}

func TestEngineM01_PutDeleteGetContention(t *testing.T) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := testCtx()
			k := []byte(fmt.Sprintf("k%d", g))
			for i := 0; i < 100; i++ {
				_ = eng.Put(ctx, k, []byte("v"))
				_, _ = eng.Get(k)
				_ = eng.Delete(ctx, k)
				_, _ = eng.Get(k)
			}
		}(g)
	}
	wg.Wait()
}

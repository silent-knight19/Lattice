package engine_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/errors"
)

// FuzzEngineM04_Lifecycle exercises Put/Get/Delete interleaved with
// Close+reopen cycles on one directory, verifying no panic, no deadlock, no
// lost accepted data, and no tombstone resurrection. Workloads are bounded
// with a small flush threshold so the Close drain path runs every reopen.
func FuzzEngineM04_Lifecycle(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	f.Add([]byte("m04-lifecycle-close-reopen-seed"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			t.Skip()
		}
		dir := t.TempDir()
		newEngine := func() *engine.Engine {
			eng := engine.NewEngineWithOptions(engine.EngineOptions{
				DBPath: dir,
				Backpressure: engine.BackpressureConfig{
					MaxMemoryBytes: 256 * 1024 * 1024, HighWatermark: 0.80, HardWatermark: 0.95, MaxWaitTimeout: 5000000000,
				},
			})
			eng.SetFlushThresholdForTesting(2048)
			if err := eng.Open(); err != nil {
				t.Fatalf("Open: %v", err)
			}
			return eng
		}
		eng := newEngine()
		ref := make(map[string][]byte)
		ctx := testCtx()

		ops := len(data)
		if ops > 40 {
			ops = 40
		}
		for i := 0; i < ops; i++ {
			b := data[i]
			// Every ~10 ops, cycle Close+reopen on the same directory.
			if i > 0 && i%10 == 0 {
				if err := eng.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
				eng = newEngine()
			}
			key := string([]byte{byte('l'), byte('0' + (b % 5))})
			switch b % 4 {
			case 0, 1:
				val := []byte{byte(i), b}
				if err := eng.Put(ctx, []byte(key), val); err != nil {
					t.Fatalf("Put: %v", err)
				}
				ref[key] = append([]byte(nil), val...)
			case 2:
				if err := eng.Delete(ctx, []byte(key)); err != nil {
					t.Fatalf("Delete: %v", err)
				}
				delete(ref, key)
			default:
				got, err := eng.Get([]byte(key))
				want, ok := ref[key]
				if !ok {
					if !stdErrors.Is(err, errors.ErrKeyNotFound) {
						t.Fatalf("Get(%q): want NotFound got %q %v", key, got, err)
					}
				} else if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("Get(%q)=%q want %q err %v", key, got, want, err)
				}
			}
		}
		if err := eng.Close(); err != nil {
			t.Fatalf("final Close: %v", err)
		}
		eng = newEngine()
		defer func() { _ = eng.Close() }()
		for k := 0; k < 5; k++ {
			key := fmt.Sprintf("l%d", k)
			want, ok := ref[key]
			got, err := eng.Get([]byte(key))
			if !ok {
				if !stdErrors.Is(err, errors.ErrKeyNotFound) {
					t.Fatalf("final Get(%q): want NotFound got %q", key, got)
				}
			} else if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("final Get(%q)=%q want %q err %v", key, got, want, err)
			}
		}
	})
}

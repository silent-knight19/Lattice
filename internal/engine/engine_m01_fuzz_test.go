package engine_test

import (
	"bytes"
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

// FuzzEngineM01_CRUD fuzzes mixed Put/Get/Delete sequences against an
// independent reference map. Input bytes are decoded deterministically into
// up to 32 operations over a small key space to force overwrites, deletes,
// and missing-key reads.
func FuzzEngineM01_CRUD(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15})
	f.Add([]byte("hello-world-fuzz-seed-for-engine-crud-paths"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			t.Skip()
		}
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		ctx := testCtx()
		ref := make(map[string][]byte)

		ops := len(data)
		if ops > 32 {
			ops = 32
		}
		for i := 0; i < ops; i++ {
			b := data[i]
			key := string([]byte{byte('k'), byte('0' + (b % 5))})
			op := b % 4
			switch op {
			case 0, 1: // PUT (50%)
				val := []byte{byte(i), b, b ^ 0xff}
				if b%17 == 0 {
					val = nil // exercise empty values
				}
				if err := eng.Put(ctx, []byte(key), val); err != nil {
					t.Fatalf("Put failed: %v", err)
				}
				cp := append([]byte(nil), val...)
				// Normalize nil vs empty: Engine treats both as empty PUT.
				ref[key] = cp
			case 2: // DELETE (25%)
				if err := eng.Delete(ctx, []byte(key)); err != nil {
					t.Fatalf("Delete failed: %v", err)
				}
				delete(ref, key)
			default: // GET (25%)
				got, err := eng.Get([]byte(key))
				want, ok := ref[key]
				if !ok {
					if !stdErrors.Is(err, errors.ErrKeyNotFound) {
						t.Fatalf("Get(%q): want NotFound, got %q err %v", key, got, err)
					}
				} else {
					if err != nil {
						t.Fatalf("Get(%q) failed: %v", key, err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("Get(%q)=%q want %q", key, got, want)
					}
				}
			}
		}
		// Final parity over key space.
		for k := 0; k < 5; k++ {
			key := string([]byte{byte('k'), byte('0' + k)})
			want, ok := ref[key]
			got, err := eng.Get([]byte(key))
			if !ok {
				if !stdErrors.Is(err, errors.ErrKeyNotFound) {
					t.Fatalf("final Get(%q): want NotFound, got %q", key, got)
				}
			} else if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("final Get(%q)=%q want %q err %v", key, got, want, err)
			}
		}
	})
}

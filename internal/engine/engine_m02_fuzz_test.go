package engine_test

import (
	"bytes"
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

// FuzzEngineM02_Flush forces threshold crossings, repeated updates, deletes,
// and rotations, verifying no data loss, no panic, and correct final state.
func FuzzEngineM02_Flush(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	f.Add([]byte("flush-fuzz-seed-m02-rotations-tombstones"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			t.Skip()
		}
		eng, _ := newFlushEngine(t, 2048)
		defer func() { _ = eng.Close() }()
		ctx := testCtx()
		ref := make(map[string][]byte)

		ops := len(data)
		if ops > 48 {
			ops = 48
		}
		for i := 0; i < ops; i++ {
			b := data[i]
			key := string([]byte{byte('f'), byte('0' + (b % 6))})
			switch b % 4 {
			case 0, 1:
				val := []byte{byte(i), b}
				if err := eng.Put(ctx, []byte(key), val); err != nil {
					t.Fatalf("Put failed: %v", err)
				}
				ref[key] = append([]byte(nil), val...)
			case 2:
				if err := eng.Delete(ctx, []byte(key)); err != nil {
					t.Fatalf("Delete failed: %v", err)
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
		waitFlushEmpty(t, eng, 15000)
		if err := eng.FlushError(); err != nil {
			t.Fatalf("background flush error: %v", err)
		}
		for k := 0; k < 6; k++ {
			key := string([]byte{byte('f'), byte('0' + k)})
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

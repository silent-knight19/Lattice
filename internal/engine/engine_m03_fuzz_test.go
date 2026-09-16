package engine_test

import (
	"bytes"
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

// FuzzEngineM03_Pacing exercises mixed CRUD under cycling L0 pressure using
// fast overrides only ({0, 8, 9}: max 1ms pacing, never stall), verifying no
// panic, no lost writes, and correct final state across pressure transitions.
func FuzzEngineM03_Pacing(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	f.Add([]byte("m03-pacing-pressure-transitions-seed"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			t.Skip()
		}
		eng := newMemEngine()
		defer func() { _ = eng.Close() }()
		ctx := testCtx()
		ref := make(map[string][]byte)
		levels := []int{0, 8, 9, 0, 9, 8}

		ops := len(data)
		if ops > 24 {
			ops = 24
		}
		for i := 0; i < ops; i++ {
			b := data[i]
			if i%4 == 0 {
				eng.SetL0CountOverrideForTesting(levels[int(b)%len(levels)])
			}
			key := string([]byte{byte('p'), byte('0' + (b % 4))})
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
		eng.SetL0CountOverrideForTesting(0)
		for k := 0; k < 4; k++ {
			key := string([]byte{byte('p'), byte('0' + k)})
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

func BenchmarkEngineM03_PutNoPressure(b *testing.B) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	ctx := testCtx()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Put(ctx, []byte("bench_nopressure"), []byte("v"))
	}
}

func BenchmarkEngineM03_PutPacedL012(b *testing.B) {
	eng := newMemEngine()
	defer func() { _ = eng.Close() }()
	eng.SetL0CountOverrideForTesting(12)
	ctx := testCtx()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = eng.Put(ctx, []byte("bench_paced"), []byte("v"))
	}
}

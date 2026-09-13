package binary_test

import (
	"bytes"
	stdErrors "errors"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// decode64Bounded runs GetVarint64 with a wall-clock guard so a
// non-terminating decoder fails the test instead of hanging the suite.
func decode64Bounded(t *testing.T, buf []byte) (uint64, int, error) {
	t.Helper()
	type result struct {
		val uint64
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		val, n, err := binary.GetVarint64(buf)
		done <- result{val, n, err}
	}()
	select {
	case r := <-done:
		return r.val, r.n, r.err
	case <-time.After(5 * time.Second):
		t.Fatalf("GetVarint64 did not terminate within 5s on %d-byte hostile input (IND-H-005)", len(buf))
		return 0, 0, nil
	}
}

// TestINDH005_VarintFloodTerminates proves the IND-H-005 verdict using the
// audit's exact attack input: 1000 consecutive 0xFF bytes. The decoder must
// terminate promptly with ErrVarintOverflow — never spin, never read past the
// buffer, never return success.
func TestINDH005_VarintFloodTerminates(t *testing.T) {
	flood := bytes.Repeat([]byte{0xFF}, 1000)

	start := time.Now()
	val, n, err := decode64Bounded(t, flood)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("accepted 1000-byte 0xFF flood as value=%d n=%d", val, n)
	}
	if !stdErrors.Is(err, errors.ErrVarintOverflow) {
		t.Fatalf("expected ErrVarintOverflow, got %v", err)
	}
	if n != 0 || val != 0 {
		t.Fatalf("expected zero value/consumed on error, got val=%d n=%d", val, n)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("decode took %v, want bounded fast rejection", elapsed)
	}
}

// TestINDH005_VarintHostileMatrix covers the remaining non-terminating shapes:
// endless continuations at every truncation length, oversized 32-bit floods,
// and empty input — all must fail closed with the documented sentinel.
func TestINDH005_VarintHostileMatrix(t *testing.T) {
	t.Run("ContinuationFlood_AllLengths", func(t *testing.T) {
		for _, length := range []int{1, 2, 5, 9, 10, 11, 64} {
			buf := bytes.Repeat([]byte{0x80}, length)
			_, _, err := decode64Bounded(t, buf)
			if err == nil {
				t.Fatalf("length %d: accepted endless continuations", length)
			}
			// Lengths 1..9 end mid-varint (truncated); >=10 exceed 64 bits (overflow).
			if length <= 9 && !stdErrors.Is(err, errors.ErrVarintTruncated) {
				t.Errorf("length %d: want ErrVarintTruncated, got %v", length, err)
			}
			if length >= 10 && !stdErrors.Is(err, errors.ErrVarintOverflow) {
				t.Errorf("length %d: want ErrVarintOverflow, got %v", length, err)
			}
		}
	})

	t.Run("Varint32Flood", func(t *testing.T) {
		flood := bytes.Repeat([]byte{0xFF}, 1000)
		done := make(chan error, 1)
		go func() {
			_, _, err := binary.GetVarint32(flood)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("GetVarint32 accepted 1000-byte flood")
			}
			if !stdErrors.Is(err, errors.ErrVarintOverflow) {
				t.Fatalf("want ErrVarintOverflow, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("GetVarint32 did not terminate on flood (IND-H-005)")
		}
	})

	t.Run("Empty", func(t *testing.T) {
		if _, _, err := binary.GetVarint64(nil); !stdErrors.Is(err, errors.ErrVarintTruncated) {
			t.Fatalf("want ErrVarintTruncated on empty, got %v", err)
		}
	})
}

package binary_test

import (
	"bytes"
	stdBinary "encoding/binary"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
)

// ============================================================================
// 1. Exact Byte Representation & Round-Trip Tests
// ============================================================================

func TestUint16ExactBytesAndRoundTrip(t *testing.T) {
	testCases := []struct {
		name     string
		value    uint16
		expected []byte
	}{
		{name: "zero", value: 0, expected: []byte{0x00, 0x00}},
		{name: "one", value: 1, expected: []byte{0x00, 0x01}},
		{name: "127", value: 127, expected: []byte{0x00, 0x7F}},
		{name: "128", value: 128, expected: []byte{0x00, 0x80}},
		{name: "255", value: 255, expected: []byte{0x00, 0xFF}},
		{name: "256", value: 256, expected: []byte{0x01, 0x00}},
		{name: "arbitrary_0x1234", value: 0x1234, expected: []byte{0x12, 0x34}},
		{name: "arbitrary_0xABCD", value: 0xABCD, expected: []byte{0xAB, 0xCD}},
		{name: "max_uint16", value: math.MaxUint16, expected: []byte{0xFF, 0xFF}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, 2)
			binary.PutUint16(buf, tc.value)

			// 1. Invariant: Exact big-endian byte sequence
			if !bytes.Equal(buf, tc.expected) {
				t.Fatalf("PutUint16(%d) byte mismatch: got %x, expected %x", tc.value, buf, tc.expected)
			}

			// 2. Invariant: Round-trip decoding recovers original value
			decoded := binary.GetUint16(buf)
			if decoded != tc.value {
				t.Fatalf("GetUint16 round-trip failed: got %d, expected %d", decoded, tc.value)
			}

			// 3. Invariant: Independent differential verification against encoding/binary
			stdBuf := make([]byte, 2)
			stdBinary.BigEndian.PutUint16(stdBuf, tc.value)
			if !bytes.Equal(buf, stdBuf) {
				t.Fatalf("differential mismatch against stdlib: got %x, stdlib produced %x", buf, stdBuf)
			}
			if binary.GetUint16(stdBuf) != tc.value {
				t.Fatalf("GetUint16 failed to decode stdlib buffer")
			}
		})
	}
}

func TestUint32ExactBytesAndRoundTrip(t *testing.T) {
	testCases := []struct {
		name     string
		value    uint32
		expected []byte
	}{
		{name: "zero", value: 0, expected: []byte{0x00, 0x00, 0x00, 0x00}},
		{name: "one", value: 1, expected: []byte{0x00, 0x00, 0x00, 0x01}},
		{name: "255", value: 255, expected: []byte{0x00, 0x00, 0x00, 0xFF}},
		{name: "256", value: 256, expected: []byte{0x00, 0x00, 0x01, 0x00}},
		{name: "65535", value: 65535, expected: []byte{0x00, 0x00, 0xFF, 0xFF}},
		{name: "65536", value: 65536, expected: []byte{0x00, 0x01, 0x00, 0x00}},
		{name: "arbitrary_0x12345678", value: 0x12345678, expected: []byte{0x12, 0x34, 0x56, 0x78}},
		{name: "arbitrary_0xDEADBEEF", value: 0xDEADBEEF, expected: []byte{0xDE, 0xAD, 0xBE, 0xEF}},
		{name: "high_bit_0x80000000", value: 0x80000000, expected: []byte{0x80, 0x00, 0x00, 0x00}},
		{name: "max_uint32", value: math.MaxUint32, expected: []byte{0xFF, 0xFF, 0xFF, 0xFF}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, 4)
			binary.PutUint32(buf, tc.value)

			// 1. Invariant: Exact big-endian byte sequence
			if !bytes.Equal(buf, tc.expected) {
				t.Fatalf("PutUint32(%d) byte mismatch: got %x, expected %x", tc.value, buf, tc.expected)
			}

			// 2. Invariant: Round-trip decoding recovers original value
			decoded := binary.GetUint32(buf)
			if decoded != tc.value {
				t.Fatalf("GetUint32 round-trip failed: got %d, expected %d", decoded, tc.value)
			}

			// 3. Invariant: Independent differential verification against encoding/binary
			stdBuf := make([]byte, 4)
			stdBinary.BigEndian.PutUint32(stdBuf, tc.value)
			if !bytes.Equal(buf, stdBuf) {
				t.Fatalf("differential mismatch against stdlib: got %x, stdlib produced %x", buf, stdBuf)
			}
			if binary.GetUint32(stdBuf) != tc.value {
				t.Fatalf("GetUint32 failed to decode stdlib buffer")
			}
		})
	}
}

func TestUint64ExactBytesAndRoundTrip(t *testing.T) {
	testCases := []struct {
		name     string
		value    uint64
		expected []byte
	}{
		{name: "zero", value: 0, expected: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{name: "one", value: 1, expected: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}},
		{name: "255", value: 255, expected: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF}},
		{name: "256", value: 256, expected: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00}},
		{name: "65535", value: 65535, expected: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF}},
		{name: "65536", value: 65536, expected: []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}},
		{name: "uint32_max", value: 0xFFFFFFFF, expected: []byte{0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF}},
		{name: "uint32_max_plus_1", value: 0x100000000, expected: []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}},
		{name: "arbitrary_sequential", value: 0x0102030405060708, expected: []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}},
		{name: "arbitrary_descending", value: 0xFEDCBA9876543210, expected: []byte{0xFE, 0xDC, 0xBA, 0x98, 0x76, 0x54, 0x32, 0x10}},
		{name: "alternating_bits_aa", value: 0xAAAAAAAAAAAAAAAA, expected: []byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}},
		{name: "alternating_bits_55", value: 0x5555555555555555, expected: []byte{0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55}},
		{name: "high_bit_only", value: 0x8000000000000000, expected: []byte{0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		{name: "max_uint64", value: math.MaxUint64, expected: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, 8)
			binary.PutUint64(buf, tc.value)

			// 1. Invariant: Exact big-endian byte sequence
			if !bytes.Equal(buf, tc.expected) {
				t.Fatalf("PutUint64(%d) byte mismatch: got %x, expected %x", tc.value, buf, tc.expected)
			}

			// 2. Invariant: Round-trip decoding recovers original value
			decoded := binary.GetUint64(buf)
			if decoded != tc.value {
				t.Fatalf("GetUint64 round-trip failed: got %d, expected %d", decoded, tc.value)
			}

			// 3. Invariant: Independent differential verification against encoding/binary
			stdBuf := make([]byte, 8)
			stdBinary.BigEndian.PutUint64(stdBuf, tc.value)
			if !bytes.Equal(buf, stdBuf) {
				t.Fatalf("differential mismatch against stdlib: got %x, stdlib produced %x", buf, stdBuf)
			}
			if binary.GetUint64(stdBuf) != tc.value {
				t.Fatalf("GetUint64 failed to decode stdlib buffer")
			}
		})
	}
}

// ============================================================================
// 2. Oversized Buffer Isolation Tests (Canary Checks)
// ============================================================================

func TestOversizedBufferIsolation(t *testing.T) {
	t.Run("uint16_oversized", func(t *testing.T) {
		buf := []byte{0x00, 0x00, 0xCC, 0xDD, 0xEE}
		binary.PutUint16(buf, 0x1234)

		if buf[0] != 0x12 || buf[1] != 0x34 {
			t.Fatalf("written bytes corrupted: got [%x, %x]", buf[0], buf[1])
		}
		// Canaries: indices 2, 3, 4 must remain untouched
		if buf[2] != 0xCC || buf[3] != 0xDD || buf[4] != 0xEE {
			t.Fatalf("canary bytes modified: got [%x, %x, %x]", buf[2], buf[3], buf[4])
		}

		decoded := binary.GetUint16(buf)
		if decoded != 0x1234 {
			t.Fatalf("GetUint16 read corrupted value: %x", decoded)
		}
	})

	t.Run("uint32_oversized", func(t *testing.T) {
		buf := []byte{0x00, 0x00, 0x00, 0x00, 0xCC, 0xDD, 0xEE}
		binary.PutUint32(buf, 0x12345678)

		if !bytes.Equal(buf[:4], []byte{0x12, 0x34, 0x56, 0x78}) {
			t.Fatalf("written bytes corrupted: got %x", buf[:4])
		}
		// Canaries: indices 4, 5, 6 must remain untouched
		if buf[4] != 0xCC || buf[5] != 0xDD || buf[6] != 0xEE {
			t.Fatalf("canary bytes modified: got [%x, %x, %x]", buf[4], buf[5], buf[6])
		}

		decoded := binary.GetUint32(buf)
		if decoded != 0x12345678 {
			t.Fatalf("GetUint32 read corrupted value: %x", decoded)
		}
	})

	t.Run("uint64_oversized", func(t *testing.T) {
		buf := make([]byte, 12)
		for i := range buf {
			buf[i] = 0xAA
		}
		binary.PutUint64(buf, 0x0102030405060708)

		expectedPrefix := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
		if !bytes.Equal(buf[:8], expectedPrefix) {
			t.Fatalf("written bytes corrupted: got %x", buf[:8])
		}
		// Canaries: indices 8..11 must remain 0xAA
		for i := 8; i < 12; i++ {
			if buf[i] != 0xAA {
				t.Fatalf("canary byte at index %d corrupted: %x", i, buf[i])
			}
		}

		decoded := binary.GetUint64(buf)
		if decoded != 0x0102030405060708 {
			t.Fatalf("GetUint64 read corrupted value: %x", decoded)
		}
	})
}

// ============================================================================
// 3. Boundary & Negative Buffer Contract Tests (Deterministic Panic & No Torn Writes)
// ============================================================================

func assertPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("%s: expected panic, but function executed without panicking", name)
		}
	}()
	f()
}

func TestBoundaryAndInsufficientBuffers(t *testing.T) {
	t.Run("uint16_panics_on_insufficient_buffer", func(t *testing.T) {
		assertPanic(t, "PutUint16_nil", func() { binary.PutUint16(nil, 0x1234) })
		assertPanic(t, "PutUint16_empty", func() { binary.PutUint16([]byte{}, 0x1234) })
		assertPanic(t, "PutUint16_len_1", func() {
			buf := []byte{0xFF}
			binary.PutUint16(buf, 0x1234)
		})

		assertPanic(t, "GetUint16_nil", func() { binary.GetUint16(nil) })
		assertPanic(t, "GetUint16_empty", func() { binary.GetUint16([]byte{}) })
		assertPanic(t, "GetUint16_len_1", func() { binary.GetUint16([]byte{0x12}) })
	})

	t.Run("uint16_no_torn_write_on_panic", func(t *testing.T) {
		buf := []byte{0xAA} // 1-byte buffer: insufficient for 2-byte uint16
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("expected panic on undersized buffer")
			}
			// Invariant: Because of eager bounds check `_ = buf[1]`, buf[0] must NOT be mutated!
			if buf[0] != 0xAA {
				t.Fatalf("torn write detected: buf[0] was mutated to %x before panic!", buf[0])
			}
		}()
		binary.PutUint16(buf, 0x1234)
	})

	t.Run("uint32_panics_on_insufficient_buffer", func(t *testing.T) {
		assertPanic(t, "PutUint32_nil", func() { binary.PutUint32(nil, 0x12345678) })
		assertPanic(t, "PutUint32_empty", func() { binary.PutUint32([]byte{}, 0x12345678) })
		assertPanic(t, "PutUint32_len_1", func() { binary.PutUint32([]byte{0}, 0x12345678) })
		assertPanic(t, "PutUint32_len_2", func() { binary.PutUint32([]byte{0, 0}, 0x12345678) })
		assertPanic(t, "PutUint32_len_3", func() { binary.PutUint32([]byte{0, 0, 0}, 0x12345678) })

		assertPanic(t, "GetUint32_nil", func() { binary.GetUint32(nil) })
		assertPanic(t, "GetUint32_empty", func() { binary.GetUint32([]byte{}) })
		assertPanic(t, "GetUint32_len_1", func() { binary.GetUint32([]byte{0}) })
		assertPanic(t, "GetUint32_len_2", func() { binary.GetUint32([]byte{0, 0}) })
		assertPanic(t, "GetUint32_len_3", func() { binary.GetUint32([]byte{0, 0, 0}) })
	})

	t.Run("uint32_no_torn_write_on_panic", func(t *testing.T) {
		buf := []byte{0xAA, 0xBB, 0xCC} // 3-byte buffer: insufficient for 4-byte uint32
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("expected panic on undersized buffer")
			}
			// Invariant: Because of eager bounds check `_ = buf[3]`, none of buf[0..2] may be mutated!
			if buf[0] != 0xAA || buf[1] != 0xBB || buf[2] != 0xCC {
				t.Fatalf("torn write detected: buf was mutated to [%x, %x, %x] before panic!", buf[0], buf[1], buf[2])
			}
		}()
		binary.PutUint32(buf, 0x12345678)
	})

	t.Run("uint64_panics_on_insufficient_buffer", func(t *testing.T) {
		assertPanic(t, "PutUint64_nil", func() { binary.PutUint64(nil, 0x12345678) })
		assertPanic(t, "PutUint64_empty", func() { binary.PutUint64([]byte{}, 0x12345678) })
		for l := 1; l < 8; l++ {
			assertPanic(t, "PutUint64_undersized", func() { binary.PutUint64(make([]byte, l), 0x12345678) })
			assertPanic(t, "GetUint64_undersized", func() { binary.GetUint64(make([]byte, l)) })
		}
		assertPanic(t, "GetUint64_nil", func() { binary.GetUint64(nil) })
	})

	t.Run("uint64_no_torn_write_on_panic", func(t *testing.T) {
		buf := []byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70} // 7-byte buffer: insufficient for 8-byte uint64
		defer func() {
			r := recover()
			if r == nil {
				t.Fatalf("expected panic on undersized buffer")
			}
			// Invariant: Because of eager bounds check `_ = buf[7]`, buf must remain unmodified
			expected := []byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70}
			if !bytes.Equal(buf, expected) {
				t.Fatalf("torn write detected: buf mutated to %x before panic!", buf)
			}
		}()
		binary.PutUint64(buf, 0x0102030405060708)
	})
}

// ============================================================================
// 4. Property-Based Randomized Differential Testing
// ============================================================================

func TestPropertyRandomizedDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // PRNG appropriate for property test generation
	const iterations = 10000

	t.Run("uint16_randomized_10000", func(t *testing.T) {
		buf := make([]byte, 2)
		stdBuf := make([]byte, 2)

		for i := 0; i < iterations; i++ {
			v := uint16(rng.Intn(math.MaxUint16 + 1))
			binary.PutUint16(buf, v)
			stdBinary.BigEndian.PutUint16(stdBuf, v)

			if !bytes.Equal(buf, stdBuf) {
				t.Fatalf("iteration %d: mismatch against stdlib for value %d: %x vs %x", i, v, buf, stdBuf)
			}

			got := binary.GetUint16(buf)
			if got != v {
				t.Fatalf("iteration %d: round-trip mismatch for value %d: got %d", i, v, got)
			}
		}
	})

	t.Run("uint32_randomized_10000", func(t *testing.T) {
		buf := make([]byte, 4)
		stdBuf := make([]byte, 4)

		for i := 0; i < iterations; i++ {
			v := rng.Uint32()
			binary.PutUint32(buf, v)
			stdBinary.BigEndian.PutUint32(stdBuf, v)

			if !bytes.Equal(buf, stdBuf) {
				t.Fatalf("iteration %d: mismatch against stdlib for value %d: %x vs %x", i, v, buf, stdBuf)
			}

			got := binary.GetUint32(buf)
			if got != v {
				t.Fatalf("iteration %d: round-trip mismatch for value %d: got %d", i, v, got)
			}
		}
	})

	t.Run("uint64_randomized_10000", func(t *testing.T) {
		buf := make([]byte, 8)
		stdBuf := make([]byte, 8)

		for i := 0; i < iterations; i++ {
			v := rng.Uint64()
			binary.PutUint64(buf, v)
			stdBinary.BigEndian.PutUint64(stdBuf, v)

			if !bytes.Equal(buf, stdBuf) {
				t.Fatalf("iteration %d: mismatch against stdlib for value %d: %x vs %x", i, v, buf, stdBuf)
			}

			got := binary.GetUint64(buf)
			if got != v {
				t.Fatalf("iteration %d: round-trip mismatch for value %d: got %d", i, v, got)
			}
		}
	})
}

// ============================================================================
// 5. Go Native Fuzzing
// ============================================================================

func FuzzUint16(f *testing.F) {
	// Corpus seeds
	seeds := []uint16{0, 1, 127, 128, 255, 256, 0x1234, 0xABCD, math.MaxUint16}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, v uint16) {
		buf := make([]byte, 2)
		binary.PutUint16(buf, v)

		// 1. Check against differential stdlib oracle
		stdBuf := make([]byte, 2)
		stdBinary.BigEndian.PutUint16(stdBuf, v)
		if !bytes.Equal(buf, stdBuf) {
			t.Fatalf("Fuzz mismatch for %d: got %x, stdlib=%x", v, buf, stdBuf)
		}

		// 2. Round-trip recovery
		recovered := binary.GetUint16(buf)
		if recovered != v {
			t.Fatalf("Fuzz round-trip failed for %d: recovered %d", v, recovered)
		}
	})
}

func FuzzUint32(f *testing.F) {
	// Corpus seeds
	seeds := []uint32{0, 1, 255, 256, 65535, 65536, 0x12345678, 0xDEADBEEF, 0x80000000, math.MaxUint32}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, v uint32) {
		buf := make([]byte, 4)
		binary.PutUint32(buf, v)

		// 1. Check against differential stdlib oracle
		stdBuf := make([]byte, 4)
		stdBinary.BigEndian.PutUint32(stdBuf, v)
		if !bytes.Equal(buf, stdBuf) {
			t.Fatalf("Fuzz mismatch for %d: got %x, stdlib=%x", v, buf, stdBuf)
		}

		// 2. Round-trip recovery
		recovered := binary.GetUint32(buf)
		if recovered != v {
			t.Fatalf("Fuzz round-trip failed for %d: recovered %d", v, recovered)
		}
	})
}

func FuzzUint64(f *testing.F) {
	// Corpus seeds
	seeds := []uint64{
		0, 1, 255, 256, 65535, 65536, 0xFFFFFFFF, 0x100000000,
		0x0102030405060708, 0xFEDCBA9876543210, 0xAAAAAAAAAAAAAAAA,
		0x5555555555555555, 0x8000000000000000, math.MaxUint64,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, v uint64) {
		buf := make([]byte, 8)
		binary.PutUint64(buf, v)

		// 1. Check against differential stdlib oracle
		stdBuf := make([]byte, 8)
		stdBinary.BigEndian.PutUint64(stdBuf, v)
		if !bytes.Equal(buf, stdBuf) {
			t.Fatalf("Fuzz mismatch for %d: got %x, stdlib=%x", v, buf, stdBuf)
		}

		// 2. Round-trip recovery
		recovered := binary.GetUint64(buf)
		if recovered != v {
			t.Fatalf("Fuzz round-trip failed for %d: recovered %d", v, recovered)
		}
	})
}

// ============================================================================
// 6. Zero-Allocation Benchmarks
// ============================================================================

// Global sink variables prevent compiler dead-code elimination of benchmarks
var (
	sinkUint16 uint16
	sinkUint32 uint32
	sinkUint64 uint64
)

func BenchmarkPutUint16(b *testing.B) {
	buf := make([]byte, 2)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		binary.PutUint16(buf, uint16(i))
	}
}

func BenchmarkGetUint16(b *testing.B) {
	buf := []byte{0x12, 0x34}
	b.ReportAllocs()
	b.ResetTimer()

	var v uint16
	for i := 0; i < b.N; i++ {
		v = binary.GetUint16(buf)
	}
	sinkUint16 = v
}

func BenchmarkPutUint32(b *testing.B) {
	buf := make([]byte, 4)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		binary.PutUint32(buf, uint32(i))
	}
}

func BenchmarkGetUint32(b *testing.B) {
	buf := []byte{0x12, 0x34, 0x56, 0x78}
	b.ReportAllocs()
	b.ResetTimer()

	var v uint32
	for i := 0; i < b.N; i++ {
		v = binary.GetUint32(buf)
	}
	sinkUint32 = v
}

func BenchmarkPutUint64(b *testing.B) {
	buf := make([]byte, 8)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		binary.PutUint64(buf, uint64(i))
	}
}

func BenchmarkGetUint64(b *testing.B) {
	buf := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	b.ReportAllocs()
	b.ResetTimer()

	var v uint64
	for i := 0; i < b.N; i++ {
		v = binary.GetUint64(buf)
	}
	sinkUint64 = v
}

func BenchmarkRoundTripUint64(b *testing.B) {
	buf := make([]byte, 8)
	b.ReportAllocs()
	b.ResetTimer()

	var v uint64
	for i := 0; i < b.N; i++ {
		binary.PutUint64(buf, uint64(i))
		v = binary.GetUint64(buf)
	}
	sinkUint64 = v
}

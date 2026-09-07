package binary_test

import (
	"bytes"
	stdBinary "encoding/binary"
	stdErrors "errors"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// ============================================================================
// 1. Exact Byte Representation & Known Boundary Vectors
// ============================================================================

func TestVarint64ExactBytesAndBoundaries(t *testing.T) {
	testCases := []struct {
		name        string
		value       uint64
		expectedLen int
		expected    []byte
	}{
		{
			name:        "zero",
			value:       0,
			expectedLen: 1,
			expected:    []byte{0x00},
		},
		{
			name:        "one",
			value:       1,
			expectedLen: 1,
			expected:    []byte{0x01},
		},
		{
			name:        "127_max_1_byte",
			value:       127,
			expectedLen: 1,
			expected:    []byte{0x7F},
		},
		{
			name:        "128_min_2_byte",
			value:       128,
			expectedLen: 2,
			expected:    []byte{0x80, 0x01},
		},
		{
			name:        "129",
			value:       129,
			expectedLen: 2,
			expected:    []byte{0x81, 0x01},
		},
		{
			name:        "255",
			value:       255,
			expectedLen: 2,
			expected:    []byte{0xFF, 0x01},
		},
		{
			name:        "256",
			value:       256,
			expectedLen: 2,
			expected:    []byte{0x80, 0x02},
		},
		{
			name:        "16383_max_2_byte",
			value:       16383,
			expectedLen: 2,
			expected:    []byte{0xFF, 0x7F},
		},
		{
			name:        "16384_min_3_byte",
			value:       16384,
			expectedLen: 3,
			expected:    []byte{0x80, 0x80, 0x01},
		},
		{
			name:        "2097151_max_3_byte",
			value:       2097151,
			expectedLen: 3,
			expected:    []byte{0xFF, 0xFF, 0x7F},
		},
		{
			name:        "2097152_min_4_byte",
			value:       2097152,
			expectedLen: 4,
			expected:    []byte{0x80, 0x80, 0x80, 0x01},
		},
		{
			name:        "268435455_max_4_byte",
			value:       268435455,
			expectedLen: 4,
			expected:    []byte{0xFF, 0xFF, 0xFF, 0x7F},
		},
		{
			name:        "268435456_min_5_byte",
			value:       268435456,
			expectedLen: 5,
			expected:    []byte{0x80, 0x80, 0x80, 0x80, 0x01},
		},
		{
			name:        "math_MaxUint32",
			value:       math.MaxUint32,
			expectedLen: 5,
			expected:    []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F},
		},
		{
			name:        "uint32_max_plus_1",
			value:       1 << 32,
			expectedLen: 5,
			expected:    []byte{0x80, 0x80, 0x80, 0x80, 0x10},
		},
		{
			name:        "2_pow_63_minus_1_max_9_byte",
			value:       (1 << 63) - 1,
			expectedLen: 9,
			expected:    []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F},
		},
		{
			name:        "2_pow_63_min_10_byte",
			value:       1 << 63,
			expectedLen: 10,
			expected:    []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01},
		},
		{
			name:        "math_MaxUint64_max_10_byte",
			value:       math.MaxUint64,
			expectedLen: 10,
			expected:    []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 1. Invariant: VarintLen matches expected length
			if l := binary.VarintLen(tc.value); l != tc.expectedLen {
				t.Fatalf("VarintLen(%d) = %d, expected %d", tc.value, l, tc.expectedLen)
			}

			// 2. Invariant: PutVarint64 writes exact canonical byte sequence
			buf := make([]byte, binary.MaxVarintLen64)
			n := binary.PutVarint64(buf, tc.value)
			if n != tc.expectedLen {
				t.Fatalf("PutVarint64 wrote %d bytes, expected %d", n, tc.expectedLen)
			}
			if !bytes.Equal(buf[:n], tc.expected) {
				t.Fatalf("PutVarint64 byte mismatch: got %x, expected %x", buf[:n], tc.expected)
			}

			// 3. Invariant: GetVarint64 round-trip recovers original value
			decoded, readN, err := binary.GetVarint64(buf[:n])
			if err != nil {
				t.Fatalf("GetVarint64 returned unexpected error: %v", err)
			}
			if readN != tc.expectedLen {
				t.Fatalf("GetVarint64 consumed %d bytes, expected %d", readN, tc.expectedLen)
			}
			if decoded != tc.value {
				t.Fatalf("GetVarint64 decoded %d, expected %d", decoded, tc.value)
			}

			// 4. Invariant: Differential verification against standard library encoding/binary
			stdBuf := make([]byte, stdBinary.MaxVarintLen64)
			stdN := stdBinary.PutUvarint(stdBuf, tc.value)
			if stdN != tc.expectedLen {
				t.Fatalf("stdlib PutUvarint wrote %d bytes, expected %d", stdN, tc.expectedLen)
			}
			if !bytes.Equal(buf[:n], stdBuf[:stdN]) {
				t.Fatalf("differential mismatch against stdlib: got %x, stdlib=%x", buf[:n], stdBuf[:stdN])
			}

			stdDecoded, stdReadN := stdBinary.Uvarint(buf[:n])
			if stdReadN != tc.expectedLen || stdDecoded != tc.value {
				t.Fatalf("stdlib failed to decode our output: got (%d, %d)", stdDecoded, stdReadN)
			}
		})
	}
}

// ============================================================================
// 2. Oversized Buffer Isolation & Trailing Byte Invariance
// ============================================================================

func TestVarintOversizedBufferIsolation(t *testing.T) {
	// Buffer with pre-filled canaries
	buf := make([]byte, 20)
	for i := range buf {
		buf[i] = 0xAA
	}

	value := uint64(0x12345678)
	n := binary.PutVarint64(buf, value)

	// Canaries after index n must be strictly unmutated
	for i := n; i < len(buf); i++ {
		if buf[i] != 0xAA {
			t.Fatalf("canary byte at index %d corrupted by PutVarint64: got %x", i, buf[i])
		}
	}

	// GetVarint64 must read only the required bytes from an oversized slice
	decoded, readN, err := binary.GetVarint64(buf)
	if err != nil {
		t.Fatalf("GetVarint64 failed on oversized buffer: %v", err)
	}
	if readN != n {
		t.Fatalf("GetVarint64 reported %d bytes consumed, expected %d", readN, n)
	}
	if decoded != value {
		t.Fatalf("GetVarint64 decoded %d, expected %d", decoded, value)
	}
}

// ============================================================================
// 3. Negative Boundary Tests: Insufficient Buffers & Anti-Tear Protection
// ============================================================================

func TestPutVarint64InsufficientBufferPanics(t *testing.T) {
	// Value 16384 requires 3 bytes
	val := uint64(16384)

	assertPanic(t, "nil_buffer", func() {
		binary.PutVarint64(nil, val)
	})

	assertPanic(t, "empty_buffer", func() {
		binary.PutVarint64([]byte{}, val)
	})

	assertPanic(t, "1_byte_buffer", func() {
		binary.PutVarint64(make([]byte, 1), val)
	})

	assertPanic(t, "2_byte_buffer", func() {
		binary.PutVarint64(make([]byte, 2), val)
	})

	// Anti-torn write verification:
	// A 2-byte buffer is provided for a 3-byte varint.
	// Thanks to early bounds check `_ = buf[needed-1]`, none of buf[0..1] may be mutated!
	buf := []byte{0xDE, 0xAD}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on undersized buffer")
		}
		if buf[0] != 0xDE || buf[1] != 0xAD {
			t.Fatalf("torn write detected: buf was mutated to [%x, %x] before panic!", buf[0], buf[1])
		}
	}()
	binary.PutVarint64(buf, val)
}

// ============================================================================
// 4. Truncation Tests for GetVarint64
// ============================================================================

func TestGetVarint64Truncation(t *testing.T) {
	truncationCases := []struct {
		name string
		buf  []byte
	}{
		{name: "nil_slice", buf: nil},
		{name: "empty_slice", buf: []byte{}},
		{name: "single_byte_continuation", buf: []byte{0x80}},
		{name: "two_bytes_continuation", buf: []byte{0x80, 0x80}},
		{name: "three_bytes_continuation", buf: []byte{0x80, 0x80, 0x80}},
		{name: "nine_bytes_continuation", buf: []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
		{name: "truncated_16384_need_3_got_2", buf: []byte{0x80, 0x80}},
		{name: "truncated_MaxUint64_need_10_got_9", buf: bytes.Repeat([]byte{0xFF}, 9)},
	}

	for _, tc := range truncationCases {
		t.Run(tc.name, func(t *testing.T) {
			val, n, err := binary.GetVarint64(tc.buf)
			if err == nil {
				t.Fatalf("expected error on truncated input, got value=%d, n=%d", val, n)
			}
			if !stdErrors.Is(err, errors.ErrVarintTruncated) {
				t.Fatalf("expected ErrVarintTruncated, got %v", err)
			}
			if n != 0 {
				t.Fatalf("expected 0 bytes consumed on truncation error, got %d", n)
			}
			if val != 0 {
				t.Fatalf("expected 0 value on truncation error, got %d", val)
			}
		})
	}
}

// ============================================================================
// 5. Overflow & Malformed Input Tests for GetVarint64
// ============================================================================

func TestGetVarint64Overflow(t *testing.T) {
	overflowCases := []struct {
		name string
		buf  []byte
	}{
		{
			name: "tenth_byte_continuation_bit_set",
			// 10 bytes all with continuation bit set -> requests 11th byte, exceeding 64 bits
			buf: bytes.Repeat([]byte{0x80}, 10),
		},
		{
			name: "tenth_byte_payload_bits_exceed_64_bits_0x02",
			// 9 bytes of 0x80 followed by 0x02 (bit 1 set, representing 2^64)
			buf: append(bytes.Repeat([]byte{0x80}, 9), 0x02),
		},
		{
			name: "tenth_byte_payload_bits_exceed_64_bits_0x7E",
			// 9 bytes of 0x80 followed by 0x7E (payload bits 1..6 set)
			buf: append(bytes.Repeat([]byte{0x80}, 9), 0x7E),
		},
		{
			name: "tenth_byte_all_bits_set_0xFF",
			buf:  bytes.Repeat([]byte{0xFF}, 10),
		},
		{
			name: "eleven_bytes_all_continuation",
			buf:  bytes.Repeat([]byte{0x80}, 11),
		},
		{
			name: "fifteen_bytes_all_continuation",
			buf:  bytes.Repeat([]byte{0x80}, 15),
		},
	}

	for _, tc := range overflowCases {
		t.Run(tc.name, func(t *testing.T) {
			val, n, err := binary.GetVarint64(tc.buf)
			if err == nil {
				t.Fatalf("expected ErrVarintOverflow, got value=%d, n=%d", val, n)
			}
			if !stdErrors.Is(err, errors.ErrVarintOverflow) {
				t.Fatalf("expected ErrVarintOverflow, got %v", err)
			}
			if n != 0 {
				t.Fatalf("expected 0 bytes consumed on overflow error, got %d", n)
			}
			if val != 0 {
				t.Fatalf("expected 0 value on overflow error, got %d", val)
			}
		})
	}
}

// ============================================================================
// 6. Security Defense: Varint Bomb DoS Attack Immunity
// ============================================================================

func TestVarintBombDoSImmunity(t *testing.T) {
	// Attacker sends 1,000,000 bytes with continuation bit set (0x80)
	// The decoder must strictly terminate at index 9 and reject in sub-microsecond time.
	hugeBuffer := make([]byte, 1_000_000)
	for i := range hugeBuffer {
		hugeBuffer[i] = 0x80
	}

	start := time.Now()
	val, n, err := binary.GetVarint64(hugeBuffer)
	elapsed := time.Since(start)

	if err == nil || !stdErrors.Is(err, errors.ErrVarintOverflow) {
		t.Fatalf("expected ErrVarintOverflow on varint bomb, got val=%d, n=%d, err=%v", val, n, err)
	}
	if n != 0 || val != 0 {
		t.Fatalf("expected 0 return values on error, got val=%d, n=%d", val, n)
	}

	// Must execute in less than 1 millisecond (in reality, less than a few nanoseconds)
	if elapsed > 10*time.Millisecond {
		t.Fatalf("varint bomb execution took too long (%v), potential unbounded loop", elapsed)
	}
}

// ============================================================================
// 7. Non-Canonical / Overlong Varint Handling Policy
// ============================================================================

func TestNonCanonicalVarintPolicy(t *testing.T) {
	// Standard policy: Encoder always produces canonical minimal encodings.
	// Decoder accepts mathematically valid non-canonical encodings up to 10 bytes
	// (matching encoding/binary.Uvarint compatibility).

	// Value 0 encoded non-canonically as 2 bytes: [0x80, 0x00]
	overlongZero := []byte{0x80, 0x00}
	val, n, err := binary.GetVarint64(overlongZero)
	if err != nil {
		t.Fatalf("GetVarint64 rejected non-canonical zero: %v", err)
	}
	if val != 0 || n != 2 {
		t.Fatalf("expected val=0, n=2, got val=%d, n=%d", val, n)
	}

	// Value 1 encoded non-canonically as 2 bytes: [0x81, 0x00]
	overlongOne := []byte{0x81, 0x00}
	val, n, err = binary.GetVarint64(overlongOne)
	if err != nil {
		t.Fatalf("GetVarint64 rejected non-canonical one: %v", err)
	}
	if val != 1 || n != 2 {
		t.Fatalf("expected val=1, n=2, got val=%d, n=%d", val, n)
	}

	// Verify our encoder NEVER produces non-canonical representations
	buf := make([]byte, 10)
	nZero := binary.PutVarint64(buf, 0)
	if nZero != 1 || buf[0] != 0x00 {
		t.Fatalf("PutVarint64 produced non-canonical zero: %x", buf[:nZero])
	}
}

// ============================================================================
// 8. Property-Based Randomized Differential Testing
// ============================================================================

func TestVarintPropertyRandomizedDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec // PRNG appropriate for property test generation
	const iterations = 10000

	buf := make([]byte, binary.MaxVarintLen64)
	stdBuf := make([]byte, stdBinary.MaxVarintLen64)

	for i := 0; i < iterations; i++ {
		// Generate varied numbers across magnitude distributions
		var v uint64
		switch i % 4 {
		case 0:
			v = uint64(rng.Intn(128)) // 1-byte range
		case 1:
			v = uint64(rng.Intn(16384)) // 1..2-byte range
		case 2:
			v = uint64(rng.Uint32()) // 1..5-byte range
		default:
			v = rng.Uint64() // full uint64 range
		}

		// 1. PutVarint64 vs stdlib PutUvarint
		n := binary.PutVarint64(buf, v)
		stdN := stdBinary.PutUvarint(stdBuf, v)

		if n != stdN {
			t.Fatalf("iteration %d (v=%d): length mismatch: got %d, stdlib=%d", i, v, n, stdN)
		}
		if !bytes.Equal(buf[:n], stdBuf[:stdN]) {
			t.Fatalf("iteration %d (v=%d): byte mismatch: got %x, stdlib=%x", i, v, buf[:n], stdBuf[:stdN])
		}

		// 2. GetVarint64 vs stdlib Uvarint
		decoded, readN, err := binary.GetVarint64(buf[:n])
		if err != nil {
			t.Fatalf("iteration %d (v=%d): unexpected error: %v", i, v, err)
		}
		if readN != n || decoded != v {
			t.Fatalf("iteration %d (v=%d): roundtrip mismatch: got (%d, %d)", i, v, decoded, readN)
		}

		// 3. Verify GetVarint64 parses stdlib-generated bytes
		decodedStd, readNStd, err := binary.GetVarint64(stdBuf[:stdN])
		if err != nil || readNStd != stdN || decodedStd != v {
			t.Fatalf("iteration %d (v=%d): failed to parse stdlib buffer", i, v)
		}
	}
}

// ============================================================================
// 9. Go Native Fuzzing
// ============================================================================

func FuzzGetVarint64(f *testing.F) {
	// Corpus seeds
	seeds := [][]byte{
		{},
		{0x00},
		{0x01},
		{0x7F},
		{0x80, 0x01},
		{0xFF, 0x01},
		{0x80, 0x80, 0x01},
		{0xFF, 0xFF, 0x7F},
		{0x80, 0x80, 0x80, 0x80, 0x08},
		bytes.Repeat([]byte{0xFF}, 9),
		append(bytes.Repeat([]byte{0xFF}, 9), 0x01),
		append(bytes.Repeat([]byte{0xFF}, 9), 0x02),
		bytes.Repeat([]byte{0x80}, 10),
		bytes.Repeat([]byte{0x80}, 20),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		val, n, err := binary.GetVarint64(data)

		if err != nil {
			// On error, n and val must strictly be 0
			if n != 0 || val != 0 {
				t.Fatalf("expected (0, 0) on error, got val=%d, n=%d", val, n)
			}
			// Error must match either ErrVarintTruncated or ErrVarintOverflow
			if !stdErrors.Is(err, errors.ErrVarintTruncated) && !stdErrors.Is(err, errors.ErrVarintOverflow) {
				t.Fatalf("unexpected error type returned: %v", err)
			}
			return
		}

		// On success, n must be bounded: 1 <= n <= 10 and n <= len(data)
		if n < 1 || n > 10 || n > len(data) {
			t.Fatalf("invalid bytesConsumed reported: n=%d for len(data)=%d", n, len(data))
		}

		// Re-encoding val must produce valid varint with length <= n
		// (less than or equal because data could contain non-canonical encodings)
		reencoded := make([]byte, binary.MaxVarintLen64)
		reN := binary.PutVarint64(reencoded, val)
		if reN > n {
			t.Fatalf("canonical encoding length (%d) exceeds decoded length (%d)", reN, n)
		}
	})
}

func FuzzRoundTripVarint64(f *testing.F) {
	seeds := []uint64{
		0, 1, 127, 128, 129, 255, 256, 16383, 16384,
		math.MaxUint32, 1 << 32, (1 << 63) - 1, 1 << 63, math.MaxUint64,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, v uint64) {
		buf := make([]byte, binary.MaxVarintLen64)
		n := binary.PutVarint64(buf, v)

		if n < 1 || n > 10 {
			t.Fatalf("PutVarint64 returned invalid byte count: %d", n)
		}

		decoded, readN, err := binary.GetVarint64(buf[:n])
		if err != nil {
			t.Fatalf("GetVarint64 failed on validly encoded uint64 (%d): %v", v, err)
		}
		if readN != n {
			t.Fatalf("byte count mismatch: Put=%d, Get=%d", n, readN)
		}
		if decoded != v {
			t.Fatalf("value mismatch: Put=%d, Get=%d", v, decoded)
		}
	})
}

// ============================================================================
// 10. Zero-Allocation Benchmarks
// ============================================================================

var (
	sinkVarintVal uint64
	sinkVarintN   int
)

func BenchmarkPutVarint64_1Byte(b *testing.B) {
	buf := make([]byte, binary.MaxVarintLen64)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sinkVarintN = binary.PutVarint64(buf, 100)
	}
}

func BenchmarkPutVarint64_2Bytes(b *testing.B) {
	buf := make([]byte, binary.MaxVarintLen64)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sinkVarintN = binary.PutVarint64(buf, 10000)
	}
}

func BenchmarkPutVarint64_5Bytes(b *testing.B) {
	buf := make([]byte, binary.MaxVarintLen64)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sinkVarintN = binary.PutVarint64(buf, 3000000000)
	}
}

func BenchmarkPutVarint64_10Bytes(b *testing.B) {
	buf := make([]byte, binary.MaxVarintLen64)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		sinkVarintN = binary.PutVarint64(buf, math.MaxUint64)
	}
}

func BenchmarkGetVarint64_1Byte(b *testing.B) {
	buf := []byte{0x64}
	b.ReportAllocs()
	b.ResetTimer()

	var val uint64
	var n int
	for i := 0; i < b.N; i++ {
		val, n, _ = binary.GetVarint64(buf)
	}
	sinkVarintVal = val
	sinkVarintN = n
}

func BenchmarkGetVarint64_2Bytes(b *testing.B) {
	buf := []byte{0x90, 0x4E} // 10000
	b.ReportAllocs()
	b.ResetTimer()

	var val uint64
	var n int
	for i := 0; i < b.N; i++ {
		val, n, _ = binary.GetVarint64(buf)
	}
	sinkVarintVal = val
	sinkVarintN = n
}

func BenchmarkGetVarint64_5Bytes(b *testing.B) {
	buf := []byte{0x80, 0x88, 0xDC, 0x96, 0x0B} // 3000000000
	b.ReportAllocs()
	b.ResetTimer()

	var val uint64
	var n int
	for i := 0; i < b.N; i++ {
		val, n, _ = binary.GetVarint64(buf)
	}
	sinkVarintVal = val
	sinkVarintN = n
}

func BenchmarkGetVarint64_10Bytes(b *testing.B) {
	buf := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x01} // math.MaxUint64
	b.ReportAllocs()
	b.ResetTimer()

	var val uint64
	var n int
	for i := 0; i < b.N; i++ {
		val, n, _ = binary.GetVarint64(buf)
	}
	sinkVarintVal = val
	sinkVarintN = n
}

func BenchmarkRoundTripVarint64(b *testing.B) {
	buf := make([]byte, binary.MaxVarintLen64)
	b.ReportAllocs()
	b.ResetTimer()

	var val uint64
	var n int
	for i := 0; i < b.N; i++ {
		n = binary.PutVarint64(buf, uint64(i))
		val, _, _ = binary.GetVarint64(buf[:n])
	}
	sinkVarintVal = val
	sinkVarintN = n
}

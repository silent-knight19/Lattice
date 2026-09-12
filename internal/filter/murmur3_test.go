package filter_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/silent-knight19/lattice/internal/filter"
)

// TestMurmur3_KnownVectors verifies Murmur3_128 against canonical test vectors.
func TestMurmur3_KnownVectors(t *testing.T) {
	testCases := []struct {
		name       string
		input      []byte
		seed       uint64
		expectedH1 uint64
		expectedH2 uint64
	}{
		{
			name:       "empty input seed 0",
			input:      []byte{},
			seed:       0,
			expectedH1: 0x0000000000000000,
			expectedH2: 0x0000000000000000,
		},
		{
			name:       "nil input seed 0",
			input:      nil,
			seed:       0,
			expectedH1: 0x0000000000000000,
			expectedH2: 0x0000000000000000,
		},
		{
			name:       "short string 'test'",
			input:      []byte("test"),
			seed:       0,
			expectedH1: 0xac7d28cc74bde19d,
			expectedH2: 0x9a128231f9bd4d82,
		},
		{
			name:       "short string 'hello'",
			input:      []byte("hello"),
			seed:       0,
			expectedH1: 0xcbd8a7b341bd9b02,
			expectedH2: 0x5b1e906a48ae1d19,
		},
		{
			name:       "standard sentence 'The quick brown fox jumps over the lazy dog'",
			input:      []byte("The quick brown fox jumps over the lazy dog"),
			seed:       0,
			expectedH1: 0xe34bbc7bbc071b6c,
			expectedH2: 0x7a433ca9c49a9347,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h1, h2 := filter.Murmur3_128(tc.input, tc.seed)
			if h1 != tc.expectedH1 || h2 != tc.expectedH2 {
				t.Fatalf("Murmur3_128(%q, %d) mismatch:\n  got:  h1=0x%016x, h2=0x%016x\n  want: h1=0x%016x, h2=0x%016x",
					tc.input, tc.seed, h1, h2, tc.expectedH1, tc.expectedH2)
			}
		})
	}
}

// TestMurmur3_SMHasherVerificationTest implements Austin Appleby's canonical VerificationTest
// from SMHasher (KeysetTest.cpp) for MurmurHash3_x64_128.
//
// The test hashes keys of length i in [0, 255] with seed (256 - i). The resulting 256 128-bit
// hashes (4096 bytes) are then hashed with seed 0. The first 4 bytes of that hash (as a little-endian uint32)
// MUST equal the canonical verification constant 0x6384BA69.
func TestMurmur3_SMHasherVerificationTest(t *testing.T) {
	const expectedVerification uint32 = 0x6384BA69

	key := make([]byte, 256)
	hashes := make([]byte, 16*256)

	for i := 0; i < 256; i++ {
		key[i] = byte(i)
		h1, h2 := filter.Murmur3_128(key[:i], uint64(256-i))
		binary.LittleEndian.PutUint64(hashes[i*16:i*16+8], h1)
		binary.LittleEndian.PutUint64(hashes[i*16+8:i*16+16], h2)
	}

	finalH1, finalH2 := filter.Murmur3_128(hashes, 0)
	var finalBytes [16]byte
	binary.LittleEndian.PutUint64(finalBytes[0:8], finalH1)
	binary.LittleEndian.PutUint64(finalBytes[8:16], finalH2)

	verification := binary.LittleEndian.Uint32(finalBytes[0:4])
	if verification != expectedVerification {
		t.Fatalf("SMHasher verification failed: got 0x%08X, want 0x%08X", verification, expectedVerification)
	}
}

// TestMurmur3_TailCoverage exercises all 16 possible tail lengths (0 through 15 bytes)
// to verify that little-endian packing and shift math operate cleanly for every tail branch.
func TestMurmur3_TailCoverage(t *testing.T) {
	buf := make([]byte, 64)
	for i := range buf {
		buf[i] = byte((i * 37) ^ 0xAA)
	}

	for length := 0; length <= 33; length++ {
		t.Run(fmt.Sprintf("len_%d_tail_%d", length, length%16), func(t *testing.T) {
			sub := buf[:length]
			h1A, h2A := filter.Murmur3_128(sub, 42)
			h1B, h2B := filter.Murmur3_128(sub, 42)

			if h1A != h1B || h2A != h2B {
				t.Fatalf("non-deterministic output for length %d: (%x, %x) vs (%x, %x)", length, h1A, h2A, h1B, h2B)
			}
		})
	}
}

// TestMurmur3_LargeInputs verifies that large buffers (up to 1 MB) process safely without panic,
// integer wrap, or memory corruption.
func TestMurmur3_LargeInputs(t *testing.T) {
	sizes := []int{
		1024,        // 1 KB
		4096,        // 4 KB
		16384,       // 16 KB
		65535,       // 64 KB - 1 (Max Lattice Storage Key size)
		65536,       // 64 KB
		1024 * 1024, // 1 MB
	}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			buf := make([]byte, size)
			for i := range buf {
				buf[i] = byte(i & 0xFF)
			}

			h1, h2 := filter.Murmur3_128(buf, filter.DefaultMurmur3Seed)
			if h1 == 0 && h2 == 0 {
				t.Fatalf("unexpected all-zero hash for non-empty %d byte buffer", size)
			}
		})
	}
}

// TestMurmur3_CapacityInvariance ensures that slice capacity does not affect hash results.
func TestMurmur3_CapacityInvariance(t *testing.T) {
	content := []byte("deterministic-key-slice-capacity-test")

	slice1 := content
	slice2 := make([]byte, len(content), len(content)+1024)
	copy(slice2, content)

	h1A, h2A := filter.Murmur3_128(slice1, 0)
	h1B, h2B := filter.Murmur3_128(slice2, 0)

	if h1A != h1B || h2A != h2B {
		t.Fatalf("hash varied with slice capacity: (%x, %x) != (%x, %x)", h1A, h2A, h1B, h2B)
	}
}

// TestMurmur3_ZeroAllocations verifies that Murmur3_128 executes with zero heap allocations.
func TestMurmur3_ZeroAllocations(t *testing.T) {
	data := []byte("zero-allocation-check-for-hot-path")
	allocs := testing.AllocsPerRun(1000, func() {
		filter.Murmur3_128(data, 0)
	})
	if allocs > 0 {
		t.Fatalf("Murmur3_128 allocated %f objects/op; want 0", allocs)
	}
}

// TestMurmur3_BinarySafety verifies that arbitrary non-UTF8 byte patterns, null bytes,
// and alternating high bits hash safely and deterministically.
func TestMurmur3_BinarySafety(t *testing.T) {
	testPatterns := [][]byte{
		{0x00},
		{0xFF},
		{0x00, 0x00, 0x00, 0x00},
		{0xFF, 0xFF, 0xFF, 0xFF},
		{0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF},
		bytes.Repeat([]byte{0x00}, 128),
		bytes.Repeat([]byte{0xFF}, 128),
	}

	for i, pattern := range testPatterns {
		t.Run(fmt.Sprintf("pattern_%d", i), func(t *testing.T) {
			h1, h2 := filter.Murmur3_128(pattern, 12345)
			h1Repeat, h2Repeat := filter.Murmur3_128(pattern, 12345)
			if h1 != h1Repeat || h2 != h2Repeat {
				t.Fatalf("determinism failure for binary pattern %d", i)
			}
		})
	}
}

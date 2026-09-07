package binary

import (
	"bytes"
	"encoding/hex"
	"hash/crc32"
	"sync"
	"testing"
)

// referenceCRC32IEEE computes CRC32-IEEE using a classic bit-by-bit software simulation.
// This serves as an independent correctness oracle completely separate from hash/crc32
// and any hardware instructions.
func referenceCRC32IEEE(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xEDB88320
			} else {
				crc >>= 1
			}
		}
	}
	return crc ^ 0xFFFFFFFF
}

func TestKnownVectors(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected uint32
	}{
		{
			name:     "nil input",
			input:    nil,
			expected: 0x00000000,
		},
		{
			name:     "empty byte slice",
			input:    []byte{},
			expected: 0x00000000,
		},
		{
			name:     "canonical ASCII 123456789",
			input:    []byte("123456789"),
			expected: 0xCBF43926,
		},
		{
			name:     "single character 'a'",
			input:    []byte("a"),
			expected: 0xE8B7BE43,
		},
		{
			name:     "three characters 'abc'",
			input:    []byte("abc"),
			expected: 0x352441C2,
		},
		{
			name:     "string 'message digest'",
			input:    []byte("message digest"),
			expected: 0x20159D7F,
		},
		{
			name:     "fox pangram",
			input:    []byte("The quick brown fox jumps over the lazy dog"),
			expected: 0x414FA339,
		},
		{
			name:     "Lattice identifier",
			input:    []byte("Lattice"),
			expected: 0x016E3054,
		},
		{
			name:     "single zero byte 0x00",
			input:    []byte{0x00},
			expected: 0xD202EF8D,
		},
		{
			name:     "single one byte 0xFF",
			input:    []byte{0xFF},
			expected: 0xFF000000,
		},
		{
			name:     "four zero bytes",
			input:    []byte{0x00, 0x00, 0x00, 0x00},
			expected: 0x2144DF1C,
		},
		{
			name:     "four 0xFF bytes",
			input:    []byte{0xFF, 0xFF, 0xFF, 0xFF},
			expected: 0xFFFFFFFF,
		},
		{
			name:     "256 sequential bytes 0x00..0xFF",
			input:    sequentialBytes(256),
			expected: 0x29058C73,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Test independent oracle first
			oracleVal := referenceCRC32IEEE(tc.input)
			if oracleVal != tc.expected {
				t.Fatalf("reference oracle mismatch on %s: expected 0x%08X, got 0x%08X",
					tc.name, tc.expected, oracleVal)
			}

			// Test Checksum
			actual := Checksum(tc.input)
			if actual != tc.expected {
				t.Errorf("Checksum() = 0x%08X, want 0x%08X", actual, tc.expected)
			}

			// Test Verify with correct expected checksum
			if !Verify(tc.input, tc.expected) {
				t.Errorf("Verify(input, 0x%08X) = false, want true", tc.expected)
			}

			// Test Verify with inverted expected checksum
			if Verify(tc.input, tc.expected^0xFFFFFFFF) {
				t.Errorf("Verify(input, inverted) = true, want false")
			}
		})
	}
}

func TestVerify_Semantics(t *testing.T) {
	data := []byte("database storage engine durability invariant")
	correct := Checksum(data)

	// Exact match must return true
	if !Verify(data, correct) {
		t.Fatalf("Verify with exact checksum must be true")
	}

	// Single-bit modification in expected checksum must return false
	for bit := 0; bit < 32; bit++ {
		mutated := correct ^ (1 << bit)
		if Verify(data, mutated) {
			t.Errorf("Verify with bit %d flipped in checksum should be false", bit)
		}
	}

	// Arithmetic offsets
	if Verify(data, correct+1) {
		t.Errorf("Verify with correct+1 should be false")
	}
	if Verify(data, correct-1) {
		t.Errorf("Verify with correct-1 should be false")
	}
}

func TestChecksum_DataIsolation(t *testing.T) {
	// Verify that neither Checksum nor Verify mutates the input slice or adjacent buffer memory.
	raw := []byte("CANARY_HEAD:persistent record payload containing arbitrary binary 0x00 0xFF:CANARY_TAIL")
	clone := make([]byte, len(raw))
	copy(clone, raw)

	c := Checksum(raw)
	if !bytes.Equal(raw, clone) {
		t.Fatalf("Checksum mutated input data: got %q, want %q", raw, clone)
	}

	v := Verify(raw, c)
	if !v {
		t.Fatalf("Verify failed on unmodified data")
	}
	if !bytes.Equal(raw, clone) {
		t.Fatalf("Verify mutated input data: got %q, want %q", raw, clone)
	}
}

func TestChecksum_SingleBitCorruption(t *testing.T) {
	// A 1024-byte payload representing a data block.
	block := make([]byte, 1024)
	for i := range block {
		block[i] = byte(i * 31)
	}
	origCRC := Checksum(block)

	// Flip single bits across multiple sample indices (beginning, middle, end).
	sampleIndices := []int{0, 1, 2, 15, 63, 128, 511, 512, 1022, 1023}
	for _, idx := range sampleIndices {
		for bit := 0; bit < 8; bit++ {
			corrupted := make([]byte, len(block))
			copy(corrupted, block)
			corrupted[idx] ^= (1 << bit)

			corruptCRC := Checksum(corrupted)
			if corruptCRC == origCRC {
				t.Fatalf("CRC collision on single-bit flip at byte %d, bit %d: 0x%08X", idx, bit, corruptCRC)
			}
			if Verify(corrupted, origCRC) {
				t.Fatalf("Verify accepted corrupted block with bit flip at byte %d, bit %d", idx, bit)
			}
		}
	}
}

func TestChecksum_MultiByteCorruption(t *testing.T) {
	payload := []byte("WAL record header + sequence number + commit timestamp + key/value")
	origCRC := Checksum(payload)

	// Truncation detection
	for i := 0; i < len(payload); i++ {
		truncated := payload[:i]
		if Verify(truncated, origCRC) {
			t.Fatalf("Verify accepted truncated payload of length %d", i)
		}
	}

	// Trailing byte append detection
	appended := append(payload, 0x00)
	if Verify(appended, origCRC) {
		t.Fatalf("Verify accepted payload with appended zero byte")
	}

	// Byte swap corruption (adjacent bytes swapped)
	swapped := make([]byte, len(payload))
	copy(swapped, payload)
	swapped[10], swapped[11] = swapped[11], swapped[10]
	if Verify(swapped, origCRC) {
		t.Fatalf("Verify accepted payload with swapped adjacent bytes")
	}
}

func TestChecksum_DifferentialRandomized(t *testing.T) {
	// Cross-verify against both the independent bit-by-bit reference oracle
	// and Go standard library crc32.ChecksumIEEE across varying sizes.
	lengths := []int{0, 1, 2, 3, 7, 8, 15, 16, 31, 32, 63, 64, 127, 128, 255, 256, 512, 1024, 4096}
	for _, l := range lengths {
		buf := make([]byte, l)
		for iter := 0; iter < 100; iter++ {
			if l > 0 {
				fillPseudoRandom(buf, int64(iter*1000+l))
			}

			wantOracle := referenceCRC32IEEE(buf)
			wantStdLib := crc32.ChecksumIEEE(buf)
			gotChecksum := Checksum(buf)

			if gotChecksum != wantOracle {
				t.Fatalf("differential mismatch with oracle at len %d (iter %d): got 0x%08X, want 0x%08X",
					l, iter, gotChecksum, wantOracle)
			}
			if gotChecksum != wantStdLib {
				t.Fatalf("differential mismatch with stdlib at len %d (iter %d): got 0x%08X, want 0x%08X",
					l, iter, gotChecksum, wantStdLib)
			}
			if !Verify(buf, gotChecksum) {
				t.Fatalf("Verify returned false for valid checksum at len %d", l)
			}
		}
	}
}

func TestChecksum_Concurrent(t *testing.T) {
	// Verify that Checksum and Verify have zero shared mutable state and can be called
	// concurrently across 100 goroutines without race conditions.
	const goroutines = 100
	const iterations = 500

	sharedData := []byte("shared concurrent block data across goroutines")
	sharedExpected := Checksum(sharedData)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			// Shared buffer verification
			for i := 0; i < iterations; i++ {
				c := Checksum(sharedData)
				if c != sharedExpected {
					t.Errorf("goroutine %d: checksum mismatch: got 0x%08X, want 0x%08X", id, c, sharedExpected)
					return
				}
				if !Verify(sharedData, sharedExpected) {
					t.Errorf("goroutine %d: verify failed on shared data", id)
					return
				}
			}

			// Goroutine-private buffer calculation
			local := []byte{byte(id), byte(id >> 8), 0xAA, 0x55}
			localCRC := Checksum(local)
			if !Verify(local, localCRC) {
				t.Errorf("goroutine %d: verify failed on local data", id)
			}
		}(g)
	}

	wg.Wait()
}

func FuzzChecksum(f *testing.F) {
	// Seed corpus with edge cases and known strings
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte("123456789"))
	f.Add([]byte("a"))
	f.Add([]byte("Lattice LSM storage engine"))
	f.Add([]byte{0x00})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(sequentialBytes(64))

	f.Fuzz(func(t *testing.T, data []byte) {
		got := Checksum(data)

		// Independent oracle equivalence
		oracle := referenceCRC32IEEE(data)
		if got != oracle {
			t.Fatalf("Checksum(%s) = 0x%08X, oracle = 0x%08X", hex.EncodeToString(data), got, oracle)
		}

		// Standard library equivalence
		stdlib := crc32.ChecksumIEEE(data)
		if got != stdlib {
			t.Fatalf("Checksum(%s) = 0x%08X, stdlib = 0x%08X", hex.EncodeToString(data), got, stdlib)
		}

		// Verify must succeed on exact match
		if !Verify(data, got) {
			t.Fatalf("Verify(data, Checksum(data)) must return true")
		}

		// Mutated checksums must fail verification
		if Verify(data, got^0x00000001) {
			t.Fatalf("Verify accepted checksum with bit 0 flipped")
		}
		if Verify(data, got^0x80000000) {
			t.Fatalf("Verify accepted checksum with bit 31 flipped")
		}
	})
}

// Benchmark suites measuring ns/op, B/op, and allocs/op across payload sizes.

func BenchmarkChecksum_Empty(b *testing.B) {
	data := []byte{}
	b.SetBytes(0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Checksum(data)
	}
}

func BenchmarkChecksum_64B(b *testing.B) {
	data := make([]byte, 64)
	fillPseudoRandom(data, 64)
	b.SetBytes(64)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Checksum(data)
	}
}

func BenchmarkChecksum_1KB(b *testing.B) {
	data := make([]byte, 1024)
	fillPseudoRandom(data, 1024)
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Checksum(data)
	}
}

func BenchmarkChecksum_4KB(b *testing.B) {
	data := make([]byte, 4096)
	fillPseudoRandom(data, 4096)
	b.SetBytes(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Checksum(data)
	}
}

func BenchmarkChecksum_64KB(b *testing.B) {
	data := make([]byte, 64*1024)
	fillPseudoRandom(data, 64*1024)
	b.SetBytes(64 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Checksum(data)
	}
}

func BenchmarkChecksum_1MB(b *testing.B) {
	data := make([]byte, 1024*1024)
	fillPseudoRandom(data, 1024*1024)
	b.SetBytes(1024 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Checksum(data)
	}
}

func BenchmarkVerify_4KB(b *testing.B) {
	data := make([]byte, 4096)
	fillPseudoRandom(data, 4096)
	expected := Checksum(data)
	b.SetBytes(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Verify(data, expected)
	}
}

// Helpers

func sequentialBytes(n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i++ {
		b[i] = byte(i)
	}
	return b
}

func fillPseudoRandom(buf []byte, seed int64) {
	// Simple deterministic linear congruential generator for test reproducibility
	s := uint64(seed)
	for i := range buf {
		s = s*6364136223846793005 + 1442695040888963407
		buf[i] = byte(s >> 33)
	}
}

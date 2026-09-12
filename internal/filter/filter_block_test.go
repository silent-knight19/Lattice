package filter_test

import (
	"bytes"
	stdErrors "errors"
	"fmt"
	"hash/crc32"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/filter"
)

// independentOracleEncode constructs expected serialized bytes independently of BloomFilter.Encode.
func independentOracleEncode(bitset []byte, bitCount uint64, k byte) []byte {
	out := make([]byte, len(bitset)+filter.FilterBlockTrailerSize)
	copy(out, bitset)
	binary.PutUint64(out[len(bitset):len(bitset)+8], bitCount)
	out[len(bitset)+8] = k
	crc := crc32.ChecksumIEEE(out[:len(bitset)+9])
	binary.PutUint32(out[len(bitset)+9:], crc)
	return out
}

// independentOracleDecode parses wire bytes independently of DecodeFilterBlock.
func independentOracleDecode(t *testing.T, data []byte) ([]byte, uint64, int) {
	t.Helper()
	if len(data) < filter.FilterBlockTrailerSize {
		t.Fatalf("independentOracleDecode: data too short (%d bytes)", len(data))
	}
	expectedCRC := binary.GetUint32(data[len(data)-4:])
	actualCRC := crc32.ChecksumIEEE(data[:len(data)-4])
	if expectedCRC != actualCRC {
		t.Fatalf("independentOracleDecode: checksum mismatch (expected %08x, got %08x)", expectedCRC, actualCRC)
	}
	k := int(data[len(data)-5])
	bitCount := binary.GetUint64(data[len(data)-13 : len(data)-5])
	bitset := make([]byte, len(data)-13)
	copy(bitset, data[:len(data)-13])
	return bitset, bitCount, k
}

// TestFilterBlock_ExactByteFixtures tests serialization against independently calculated exact byte sequences.
func TestFilterBlock_ExactByteFixtures(t *testing.T) {
	// Fixture 1: Empty filter (0 keys, 0 bits, k=7)
	// Layout: [0 bits: 0 bytes] + [BitCount: 8 bytes 0x00] + [k: 1 byte 0x07] + [CRC32: 4 bytes]
	emptyExpectedCRC := crc32.ChecksumIEEE([]byte{0, 0, 0, 0, 0, 0, 0, 0, 7})
	expectedEmptyBytes := []byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // BitCount = 0 (uint64)
		0x07, // k = 7
		byte(emptyExpectedCRC >> 24),
		byte(emptyExpectedCRC >> 16),
		byte(emptyExpectedCRC >> 8),
		byte(emptyExpectedCRC),
	}

	fEmpty := filter.NewBloomFilter(0)
	emptyEncoded := fEmpty.Encode()
	if !bytes.Equal(emptyEncoded, expectedEmptyBytes) {
		t.Fatalf("empty filter exact byte mismatch:\nexpected: %x\ngot:      %x", expectedEmptyBytes, emptyEncoded)
	}

	// Fixture 2: 1-key filter (m=10 bits, 2 bytes bitset, k=7)
	// Manually construct filter and set known bits
	f1 := filter.NewBloomFilter(1)
	if f1.ByteSize() != 2 || f1.BitCount() != 10 {
		t.Fatalf("expected 2 bytes and 10 bits, got %d bytes and %d bits", f1.ByteSize(), f1.BitCount())
	}
	f1.Bitset()[0] = 0x55 // bits 0, 2, 4, 6
	f1.Bitset()[1] = 0x02 // bit 9

	expectedF1Bytes := independentOracleEncode([]byte{0x55, 0x02}, 10, 7)
	f1Encoded := f1.Encode()
	if !bytes.Equal(f1Encoded, expectedF1Bytes) {
		t.Fatalf("1-key filter exact byte mismatch:\nexpected: %x\ngot:      %x", expectedF1Bytes, f1Encoded)
	}

	// Verify independent oracle decodes it correctly
	oracleBitset, oracleBits, oracleK := independentOracleDecode(t, f1Encoded)
	if !bytes.Equal(oracleBitset, []byte{0x55, 0x02}) {
		t.Errorf("oracle bitset mismatch: got %x", oracleBitset)
	}
	if oracleBits != 10 {
		t.Errorf("oracle bitCount mismatch: got %d, want 10", oracleBits)
	}
	if oracleK != 7 {
		t.Errorf("oracle k mismatch: got %d, want 7", oracleK)
	}
}

// TestFilterBlock_RoundTrip tests round-trip encoding and decoding across various key counts and sets.
func TestFilterBlock_RoundTrip(t *testing.T) {
	keyCounts := []int{0, 1, 2, 7, 8, 9, 10, 16, 50, 100, 1000, 5000}

	for _, n := range keyCounts {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			f := filter.NewBloomFilter(n)
			if f == nil {
				t.Fatalf("failed to allocate filter for n=%d", n)
			}

			// Add distinct deterministic keys
			keys := make([][]byte, n)
			for i := 0; i < n; i++ {
				keys[i] = []byte(fmt.Sprintf("test_key_%08d", i))
				f.Add(keys[i])
			}

			// Encode
			encoded := f.Encode()
			if len(encoded) != f.ByteSize()+filter.FilterBlockTrailerSize {
				t.Fatalf("encoded length mismatch: got %d, want %d", len(encoded), f.ByteSize()+filter.FilterBlockTrailerSize)
			}

			// Decode
			decoded, err := filter.DecodeFilterBlock(encoded)
			if err != nil {
				t.Fatalf("unexpected decode error: %v", err)
			}

			// Verify structural preservation
			if decoded.KeyCount() != f.KeyCount() {
				t.Errorf("keyCount mismatch: got %d, want %d", decoded.KeyCount(), f.KeyCount())
			}
			if decoded.BitCount() != f.BitCount() {
				t.Errorf("bitCount mismatch: got %d, want %d", decoded.BitCount(), f.BitCount())
			}
			if decoded.HashCount() != f.HashCount() {
				t.Errorf("hashCount mismatch: got %d, want %d", decoded.HashCount(), f.HashCount())
			}
			if decoded.ByteSize() != f.ByteSize() {
				t.Errorf("byteSize mismatch: got %d, want %d", decoded.ByteSize(), f.ByteSize())
			}
			if !bytes.Equal(decoded.Bitset(), f.Bitset()) {
				t.Errorf("bitset contents mismatch")
			}

			// Verify membership equivalence (Zero False Negatives)
			for _, k := range keys {
				if !decoded.MayContain(k) {
					t.Errorf("round-trip membership failure: key %q present in original but MayContain=false in decoded", k)
				}
				if !f.MayContain(k) {
					t.Errorf("original filter MayContain=false for added key %q", k)
				}
			}

			// Verify probe agreement
			for _, k := range keys {
				origProbes := f.Probes(k)
				decProbes := decoded.Probes(k)
				if len(origProbes) != len(decProbes) {
					t.Fatalf("probe length mismatch for key %q", k)
				}
				for pIdx := range origProbes {
					if origProbes[pIdx] != decProbes[pIdx] {
						t.Errorf("probe[%d] mismatch for key %q: got %d, want %d", pIdx, k, decProbes[pIdx], origProbes[pIdx])
					}
				}
			}

			// Absent key check
			absentKey := []byte("definitely_absent_key_123456789")
			if f.MayContain(absentKey) != decoded.MayContain(absentKey) {
				t.Errorf("absent key disagreement between original and decoded filter")
			}
		})
	}
}

// TestFilterBlock_NilFilterEncode verifies encoding of a nil BloomFilter safely produces an empty filter block.
func TestFilterBlock_NilFilterEncode(t *testing.T) {
	var f *filter.BloomFilter
	encoded := f.Encode()
	if len(encoded) != filter.FilterBlockTrailerSize {
		t.Fatalf("expected %d bytes for nil filter, got %d", filter.FilterBlockTrailerSize, len(encoded))
	}

	decoded, err := filter.DecodeFilterBlock(encoded)
	if err != nil {
		t.Fatalf("failed to decode nil-encoded filter: %v", err)
	}
	if !decoded.IsEmpty() {
		t.Errorf("expected decoded nil filter to be empty")
	}
	if decoded.MayContain([]byte("key")) {
		t.Errorf("empty filter must return MayContain=false")
	}
}

// TestFilterBlock_Determinism asserts byte-for-byte reproducibility of serialization.
func TestFilterBlock_Determinism(t *testing.T) {
	keys := [][]byte{
		[]byte("alpha"),
		[]byte("beta"),
		[]byte("gamma"),
		[]byte("delta"),
	}

	// Two separately constructed filters
	f1 := filter.NewBloomFilter(len(keys))
	f2 := filter.NewBloomFilter(len(keys))

	for _, k := range keys {
		f1.Add(k)
		f2.Add(k)
	}

	enc1 := f1.Encode()
	enc2 := f2.Encode()

	if !bytes.Equal(enc1, enc2) {
		t.Fatalf("deterministic serialization failed: enc1 != enc2")
	}

	// Re-serializing the same filter produces byte-identical output
	enc1Again := f1.Encode()
	if !bytes.Equal(enc1, enc1Again) {
		t.Fatalf("idempotent serialization failed on same filter")
	}
}

// TestFilterBlock_OwnershipIsolation verifies that mutations to output bytes do not affect
// the source filter, and mutations to the filter do not retroactively alter output bytes.
func TestFilterBlock_OwnershipIsolation(t *testing.T) {
	f := filter.NewBloomFilter(10)
	key := []byte("isolated_key")
	f.Add(key)

	encoded := f.Encode()
	encodedSnapshot := make([]byte, len(encoded))
	copy(encodedSnapshot, encoded)

	// 1. Mutate encoded slice; verify filter is unchanged
	for i := range encoded {
		encoded[i] = 0xFF
	}
	if !f.MayContain(key) {
		t.Fatalf("mutating serialized bytes corrupted source BloomFilter")
	}

	// 2. Decode from snapshot; mutate snapshot; verify decoded filter is unchanged
	decoded, err := filter.DecodeFilterBlock(encodedSnapshot)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	for i := range encodedSnapshot {
		encodedSnapshot[i] = 0xAA
	}
	if !decoded.MayContain(key) {
		t.Fatalf("mutating source byte buffer corrupted decoded BloomFilter")
	}

	// 3. Mutate decoded filter bitset; verify encodedSnapshot is unchanged
	decoded.Bitset()[0] ^= 0xFF
	if bytes.Equal(decoded.Bitset(), encodedSnapshot[:decoded.ByteSize()]) {
		t.Fatalf("mutating decoded bitset reflected in source buffer")
	}
}

// TestFilterBlock_CorruptionMatrix systematically tests truncation, bit flips, and invalid metadata.
func TestFilterBlock_CorruptionMatrix(t *testing.T) {
	f := filter.NewBloomFilter(50)
	for i := 0; i < 50; i++ {
		f.Add([]byte(fmt.Sprintf("key_%d", i)))
	}
	validBytes := f.Encode()

	t.Run("Truncation", func(t *testing.T) {
		for l := 0; l < filter.FilterBlockTrailerSize; l++ {
			truncated := validBytes[:l]
			_, err := filter.DecodeFilterBlock(truncated)
			if !stdErrors.Is(err, errors.ErrFilterBlockTruncated) {
				t.Errorf("length %d: expected ErrFilterBlockTruncated, got %v", l, err)
			}
		}

		// Truncated by 1 byte from full size
		if len(validBytes) > filter.FilterBlockTrailerSize {
			truncatedOne := validBytes[:len(validBytes)-1]
			_, err := filter.DecodeFilterBlock(truncatedOne)
			if err == nil {
				t.Fatalf("expected error on 1-byte truncated filter block")
			}
		}
	})

	t.Run("CRC_BitFlips", func(t *testing.T) {
		// Flip every bit of the CRC field
		for bit := 0; bit < 32; bit++ {
			corrupted := make([]byte, len(validBytes))
			copy(corrupted, validBytes)
			byteIdx := len(corrupted) - 4 + (bit / 8)
			corrupted[byteIdx] ^= 1 << (bit % 8)

			_, err := filter.DecodeFilterBlock(corrupted)
			if err == nil {
				t.Fatalf("bit %d flip in CRC went undetected", bit)
			}
			var crcErr *errors.ChecksumMismatchError
			if !stdErrors.As(err, &crcErr) {
				t.Errorf("expected ChecksumMismatchError, got %v", err)
			}
		}
	})

	t.Run("Bitset_Payload_BitFlips", func(t *testing.T) {
		// Flip bits in the bitset payload
		for bit := 0; bit < 16; bit++ {
			corrupted := make([]byte, len(validBytes))
			copy(corrupted, validBytes)
			byteIdx := bit / 8
			corrupted[byteIdx] ^= 1 << (bit % 8)

			_, err := filter.DecodeFilterBlock(corrupted)
			if err == nil {
				t.Fatalf("bit %d flip in bitset payload went undetected", bit)
			}
			if !stdErrors.Is(err, errors.ErrChecksumMismatch) {
				t.Errorf("expected ErrChecksumMismatch, got %v", err)
			}
		}
	})

	t.Run("Unsupported_HashCount_k", func(t *testing.T) {
		badKs := []byte{0, 1, 2, 3, 4, 5, 6, 8, 9, 10, 16, 255}
		for _, badK := range badKs {
			corrupted := independentOracleEncode(f.Bitset(), f.BitCount(), badK)
			_, err := filter.DecodeFilterBlock(corrupted)
			if !stdErrors.Is(err, errors.ErrUnsupportedHashCount) {
				t.Errorf("k=%d: expected ErrUnsupportedHashCount, got %v", badK, err)
			}
		}
	})

	t.Run("BitCount_PayloadLength_Mismatch", func(t *testing.T) {
		// Declare bitCount=1000, but payload is only 5 bytes
		fakeBitset := make([]byte, 5)
		corrupted := independentOracleEncode(fakeBitset, 1000, 7)
		_, err := filter.DecodeFilterBlock(corrupted)
		if !stdErrors.Is(err, errors.ErrFilterBlockCorrupted) {
			t.Errorf("expected ErrFilterBlockCorrupted on length mismatch, got %v", err)
		}
	})

	t.Run("BitCount_Exceeds_MaxCapacity", func(t *testing.T) {
		// Declare bitCount greater than MaxKeyCount * BitsPerKey
		oversizedBits := uint64(filter.MaxKeyCount)*uint64(filter.BitsPerKey) + 100
		fakeBitset := make([]byte, 10)
		corrupted := independentOracleEncode(fakeBitset, oversizedBits, 7)
		_, err := filter.DecodeFilterBlock(corrupted)
		if !stdErrors.Is(err, errors.ErrFilterBlockCorrupted) {
			t.Errorf("expected ErrFilterBlockCorrupted on oversized bitCount, got %v", err)
		}
	})

	t.Run("Buffer_Exceeds_MaxBitsetBytes", func(t *testing.T) {
		// Mock oversized buffer check
		oversizedLen := filter.MaxBitsetBytes + filter.FilterBlockTrailerSize + 1
		// We don't allocate 256MB slice, but we can verify the boundary check handles MaxBitsetBytes safely
		buf := make([]byte, filter.FilterBlockTrailerSize+10)
		// Set bitCount to require massive allocation
		binary.PutUint64(buf[len(buf)-13:len(buf)-5], uint64(filter.MaxBitsetBytes)*8+8)
		buf[len(buf)-5] = 7
		crc := crc32.ChecksumIEEE(buf[:len(buf)-4])
		binary.PutUint32(buf[len(buf)-4:], crc)

		_, err := filter.DecodeFilterBlock(buf)
		if !stdErrors.Is(err, errors.ErrFilterBlockCorrupted) {
			t.Errorf("expected ErrFilterBlockCorrupted, got %v", err)
		}
		_ = oversizedLen
	})
}

// TestFilterBlockBuilder_Lifecycle tests the state machine and invariants of FilterBlockBuilder.
func TestFilterBlockBuilder_Lifecycle(t *testing.T) {
	builder := filter.NewFilterBlockBuilder(10)
	if builder == nil {
		t.Fatalf("expected non-nil builder")
	}
	if !builder.IsEmpty() {
		t.Errorf("expected freshly created builder to be empty")
	}
	if builder.AddedKeys() != 0 {
		t.Errorf("expected 0 added keys, got %d", builder.AddedKeys())
	}
	if builder.Finished() {
		t.Errorf("expected builder not to be finished initially")
	}

	// Add keys
	k1 := []byte("key1")
	k2 := []byte("key2")
	if err := builder.AddKey(k1); err != nil {
		t.Fatalf("unexpected AddKey error: %v", err)
	}
	if err := builder.AddKey(k2); err != nil {
		t.Fatalf("unexpected AddKey error: %v", err)
	}
	if builder.AddedKeys() != 2 {
		t.Errorf("expected 2 added keys, got %d", builder.AddedKeys())
	}
	if builder.IsEmpty() {
		t.Errorf("builder must not be empty after AddKey")
	}

	// Finish
	bytes1 := builder.Finish()
	if !builder.Finished() {
		t.Errorf("expected Finished()=true after Finish()")
	}

	// Idempotency: multiple Finish calls return identical bytes
	bytes2 := builder.Finish()
	if !bytes.Equal(bytes1, bytes2) {
		t.Fatalf("Finish is not idempotent")
	}

	// Mutation of returned slice must not affect builder
	bytes1[0] ^= 0xFF
	bytes3 := builder.Finish()
	if bytes.Equal(bytes1, bytes3) {
		t.Fatalf("mutating returned bytes corrupted builder's cached finished buffer")
	}

	// AddKey after Finish is rejected with ErrFilterFinished
	if err := builder.AddKey([]byte("key3")); !stdErrors.Is(err, errors.ErrFilterFinished) {
		t.Errorf("expected ErrFilterFinished, got %v", err)
	}

	// Verify decoded filter
	decoded, err := filter.DecodeFilterBlock(bytes2)
	if err != nil {
		t.Fatalf("failed to decode finished bytes: %v", err)
	}
	if !decoded.MayContain(k1) || !decoded.MayContain(k2) {
		t.Errorf("decoded filter missing added keys")
	}

	// Reset
	builder.Reset()
	if builder.Finished() {
		t.Errorf("expected Finished()=false after Reset()")
	}
	if builder.AddedKeys() != 0 {
		t.Errorf("expected 0 added keys after Reset, got %d", builder.AddedKeys())
	}

	// Can add keys again after Reset
	k4 := []byte("key4")
	if err := builder.AddKey(k4); err != nil {
		t.Fatalf("AddKey failed after Reset: %v", err)
	}
	bytesAfterReset := builder.Finish()
	decodedAfterReset, err := filter.DecodeFilterBlock(bytesAfterReset)
	if err != nil {
		t.Fatalf("failed to decode after reset: %v", err)
	}
	if !decodedAfterReset.MayContain(k4) {
		t.Errorf("decoded filter missing key added after reset")
	}
}

// TestFilterBlockBuilder_WrapExistingFilter verifies NewFilterBlockBuilderWithFilter.
func TestFilterBlockBuilder_WrapExistingFilter(t *testing.T) {
	if filter.NewFilterBlockBuilderWithFilter(nil) != nil {
		t.Errorf("expected nil builder when wrapping nil filter")
	}

	bf := filter.NewBloomFilter(5)
	k := []byte("wrap_test")
	bf.Add(k)

	b := filter.NewFilterBlockBuilderWithFilter(bf)
	if b == nil {
		t.Fatalf("expected non-nil builder")
	}
	if b.Filter() != bf {
		t.Errorf("wrapped filter mismatch")
	}

	out := b.Finish()
	decoded, err := filter.DecodeFilterBlock(out)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !decoded.MayContain(k) {
		t.Errorf("wrapped filter key missing in decoded filter")
	}
}

// BenchmarkFilterBlockCodec benchmarks serialization and deserialization across small and large filters.
func BenchmarkFilterBlockCodec(b *testing.B) {
	sizes := []int{10, 100, 1000, 10000}

	for _, n := range sizes {
		bf := filter.NewBloomFilter(n)
		for i := 0; i < n; i++ {
			bf.Add([]byte(fmt.Sprintf("bench_key_%d", i)))
		}
		serialized := bf.Encode()

		b.Run(fmt.Sprintf("Serialize_N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = bf.Encode()
			}
		})

		b.Run(fmt.Sprintf("Deserialize_N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = filter.DecodeFilterBlock(serialized)
			}
		})

		b.Run(fmt.Sprintf("RoundTrip_N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				enc := bf.Encode()
				_, _ = filter.DecodeFilterBlock(enc)
			}
		})
	}
}

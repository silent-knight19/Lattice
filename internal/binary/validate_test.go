package binary

import (
	"bytes"
	stdErrors "errors"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

func TestValidateKey_Boundaries(t *testing.T) {
	// Reusable buffer to avoid repeated large allocations
	buf65k := make([]byte, MaxKeyLen+10)

	tests := []struct {
		name          string
		key           []byte
		wantErr       error
		expectIsError error
		wantTypedSize uint32
	}{
		{
			name:          "nil key",
			key:           nil,
			wantErr:       errors.ErrEmptyKey,
			expectIsError: errors.ErrEmptyKey,
		},
		{
			name:          "empty byte slice",
			key:           []byte{},
			wantErr:       errors.ErrEmptyKey,
			expectIsError: errors.ErrEmptyKey,
		},
		{
			name:          "minimum valid length 1 byte",
			key:           []byte("k"),
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "typical 16-byte key",
			key:           []byte("user:10001:email"),
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "boundary max - 1 (65,534 bytes)",
			key:           buf65k[:MaxKeyLen-1],
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "boundary max (65,535 bytes)",
			key:           buf65k[:MaxKeyLen],
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "boundary max + 1 (65,536 bytes)",
			key:           buf65k[:MaxKeyLen+1],
			wantErr:       errors.ErrKeyTooLarge,
			expectIsError: errors.ErrKeyTooLarge,
			wantTypedSize: MaxKeyLen + 1,
		},
		{
			name:          "oversized 70,000-byte key",
			key:           make([]byte, 70000),
			wantErr:       errors.ErrKeyTooLarge,
			expectIsError: errors.ErrKeyTooLarge,
			wantTypedSize: 70000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKey(tc.key)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected valid key, got error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error matching %v, got nil", tc.wantErr)
			}

			if !stdErrors.Is(err, tc.expectIsError) {
				t.Errorf("errors.Is(err, %v) = false, got %v", tc.expectIsError, err)
			}

			if tc.wantTypedSize > 0 {
				var typedErr *errors.KeyTooLargeError
				if !stdErrors.As(err, &typedErr) {
					t.Fatalf("expected *errors.KeyTooLargeError, got %T", err)
				}
				if typedErr.KeySize != tc.wantTypedSize {
					t.Errorf("KeySize = %d, want %d", typedErr.KeySize, tc.wantTypedSize)
				}
				if typedErr.MaxSize != MaxKeyLen {
					t.Errorf("MaxSize = %d, want %d", typedErr.MaxSize, MaxKeyLen)
				}
			}
		})
	}
}

func TestValidateValue_Boundaries(t *testing.T) {
	// Reusable 4 MiB buffer (+10 bytes) to test boundaries without GC churn
	buf4M := make([]byte, MaxValueLen+10)

	tests := []struct {
		name          string
		val           []byte
		wantErr       error
		expectIsError error
		wantTypedSize uint32
	}{
		{
			name:          "nil value (valid valueless marker)",
			val:           nil,
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "empty byte slice (valid valueless marker)",
			val:           []byte{},
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "1-byte value",
			val:           []byte{0x42},
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "typical 1KB value",
			val:           buf4M[:1024],
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "typical 64KB value",
			val:           buf4M[:64*1024],
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "boundary max - 1 (4,194,303 bytes)",
			val:           buf4M[:MaxValueLen-1],
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "boundary max (4,194,304 bytes / 4 MiB)",
			val:           buf4M[:MaxValueLen],
			wantErr:       nil,
			expectIsError: nil,
		},
		{
			name:          "boundary max + 1 (4,194,305 bytes)",
			val:           buf4M[:MaxValueLen+1],
			wantErr:       errors.ErrValueTooLarge,
			expectIsError: errors.ErrValueTooLarge,
			wantTypedSize: MaxValueLen + 1,
		},
		{
			name:          "oversized 4,194,314-byte value",
			val:           buf4M[:MaxValueLen+10],
			wantErr:       errors.ErrValueTooLarge,
			expectIsError: errors.ErrValueTooLarge,
			wantTypedSize: MaxValueLen + 10,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateValue(tc.val)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("expected valid value, got error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected error matching %v, got nil", tc.wantErr)
			}

			if !stdErrors.Is(err, tc.expectIsError) {
				t.Errorf("errors.Is(err, %v) = false, got %v", tc.expectIsError, err)
			}

			if tc.wantTypedSize > 0 {
				var typedErr *errors.ValueTooLargeError
				if !stdErrors.As(err, &typedErr) {
					t.Fatalf("expected *errors.ValueTooLargeError, got %T", err)
				}
				if typedErr.ValueSize != tc.wantTypedSize {
					t.Errorf("ValueSize = %d, want %d", typedErr.ValueSize, tc.wantTypedSize)
				}
				if typedErr.MaxSize != MaxValueLen {
					t.Errorf("MaxSize = %d, want %d", typedErr.MaxSize, MaxValueLen)
				}
			}
		})
	}
}

func TestValidate_InputImmutability(t *testing.T) {
	key := []byte("immutability-check-key-bytes")
	keyClone := make([]byte, len(key))
	copy(keyClone, key)

	if err := ValidateKey(key); err != nil {
		t.Fatalf("ValidateKey failed: %v", err)
	}
	if !bytes.Equal(key, keyClone) {
		t.Fatalf("ValidateKey mutated key: got %q, want %q", key, keyClone)
	}

	val := []byte("immutability-check-value-bytes-containing-arbitrary-data")
	valClone := make([]byte, len(val))
	copy(valClone, val)

	if err := ValidateValue(val); err != nil {
		t.Fatalf("ValidateValue failed: %v", err)
	}
	if !bytes.Equal(val, valClone) {
		t.Fatalf("ValidateValue mutated value: got %q, want %q", val, valClone)
	}
}

func TestValidate_BinarySafety(t *testing.T) {
	// Keys and values can contain arbitrary byte patterns (null bytes, high bytes 0xFF, control characters).
	binaryKey := []byte{0x00, 0xFF, 0x00, 0x7F, 0x80, 0x01, 0x00}
	if err := ValidateKey(binaryKey); err != nil {
		t.Fatalf("ValidateKey rejected binary key containing null and 0xFF: %v", err)
	}

	binaryVal := []byte{0x00, 0x00, 0xFF, 0xFF, 0x00, 0xFE}
	if err := ValidateValue(binaryVal); err != nil {
		t.Fatalf("ValidateValue rejected binary value: %v", err)
	}
}

func TestValidate_UTF8ByteLengthInvariant(t *testing.T) {
	// Proves that key limits are byte-based, NOT rune-based.
	// The rune '🚀' (U+1F680) is 1 character/rune, but encodes to 4 UTF-8 bytes: [0xF0, 0x9F, 0x9A, 0x80].
	rocketRune := "🚀"
	rocketBytes := []byte(rocketRune)
	if len(rocketBytes) != 4 {
		t.Fatalf("expected rocket rune to be 4 bytes, got %d", len(rocketBytes))
	}

	// 16,384 rocket runes = 16,384 runes, but 16,384 * 4 = 65,536 bytes (exceeds MaxKeyLen 65,535).
	// A naive character-count check would think len <= 65,535, but byte-length enforcement correctly rejects it.
	repeatedRockets := bytes.Repeat(rocketBytes, 16384)
	if len(repeatedRockets) != 65536 {
		t.Fatalf("expected 65536 bytes, got %d", len(repeatedRockets))
	}

	err := ValidateKey(repeatedRockets)
	if err == nil {
		t.Fatalf("ValidateKey must reject 65,536-byte multibyte key")
	}
	if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("expected ErrKeyTooLarge for 65,536-byte UTF-8 key, got %v", err)
	}

	// 16,383 rocket runes + 3 ASCII bytes = 65,532 + 3 = 65,535 bytes (exact maximum, valid).
	validMultibyte := append(bytes.Repeat(rocketBytes, 16383), []byte("abc")...)
	if len(validMultibyte) != MaxKeyLen {
		t.Fatalf("expected %d bytes, got %d", MaxKeyLen, len(validMultibyte))
	}
	if err := ValidateKey(validMultibyte); err != nil {
		t.Fatalf("ValidateKey rejected exact MaxKeyLen multibyte key: %v", err)
	}
}

func TestValidate_ConstantAliases(t *testing.T) {
	if MaxKeyLen != 65535 {
		t.Errorf("MaxKeyLen = %d, want 65535", MaxKeyLen)
	}
	if MaxKeyBytes != MaxKeyLen {
		t.Errorf("MaxKeyBytes = %d, want %d", MaxKeyBytes, MaxKeyLen)
	}
	if MinKeyLen != 1 {
		t.Errorf("MinKeyLen = %d, want 1", MinKeyLen)
	}
	if MinValueLen != 0 {
		t.Errorf("MinValueLen = %d, want 0", MinValueLen)
	}
	if MaxValueLen != 4194304 {
		t.Errorf("MaxValueLen = %d, want 4194304", MaxValueLen)
	}
	if MaxValueBytes != MaxValueLen {
		t.Errorf("MaxValueBytes = %d, want %d", MaxValueBytes, MaxValueLen)
	}
}

func TestSafeUint32_Edges(t *testing.T) {
	if safeUint32(-1) != 0 {
		t.Errorf("safeUint32(-1) != 0")
	}
	if safeUint32(0) != 0 {
		t.Errorf("safeUint32(0) != 0")
	}
	if safeUint32(12345) != 12345 {
		t.Errorf("safeUint32(12345) != 12345")
	}
	// Simulate 64-bit int overflow beyond math.MaxUint32
	huge := int(uint64(math.MaxUint32) + 100)
	if safeUint32(huge) != math.MaxUint32 {
		t.Errorf("safeUint32(huge) = %d, want %d", safeUint32(huge), uint32(math.MaxUint32))
	}
}

func FuzzValidateKey(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte("a"))
	f.Add([]byte("standard-key"))
	f.Add(make([]byte, 100))
	f.Add(make([]byte, 65535))
	f.Add(make([]byte, 65536))

	f.Fuzz(func(t *testing.T, key []byte) {
		err := ValidateKey(key)
		l := len(key)

		if l == 0 {
			if !stdErrors.Is(err, errors.ErrEmptyKey) {
				t.Fatalf("expected ErrEmptyKey for len 0, got %v", err)
			}
		} else if l <= MaxKeyLen {
			if err != nil {
				t.Fatalf("expected valid key for len %d, got %v", l, err)
			}
		} else {
			if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
				t.Fatalf("expected ErrKeyTooLarge for len %d, got %v", l, err)
			}
		}
	})
}

func FuzzValidateValue(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte("v"))
	f.Add([]byte("payload"))
	f.Add(make([]byte, 1024))

	f.Fuzz(func(t *testing.T, val []byte) {
		err := ValidateValue(val)
		l := len(val)

		if l <= MaxValueLen {
			if err != nil {
				t.Fatalf("expected valid value for len %d, got %v", l, err)
			}
		} else {
			if !stdErrors.Is(err, errors.ErrValueTooLarge) {
				t.Fatalf("expected ErrValueTooLarge for len %d, got %v", l, err)
			}
		}
	})
}

// Benchmarks

func BenchmarkValidateKey_Empty(b *testing.B) {
	key := []byte{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateKey(key)
	}
}

func BenchmarkValidateKey_16B(b *testing.B) {
	key := make([]byte, 16)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateKey(key)
	}
}

func BenchmarkValidateKey_1KB(b *testing.B) {
	key := make([]byte, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateKey(key)
	}
}

func BenchmarkValidateKey_65535B(b *testing.B) {
	key := make([]byte, MaxKeyLen)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateKey(key)
	}
}

func BenchmarkValidateValue_Empty(b *testing.B) {
	val := []byte{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateValue(val)
	}
}

func BenchmarkValidateValue_1KB(b *testing.B) {
	val := make([]byte, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateValue(val)
	}
}

func BenchmarkValidateValue_1MB(b *testing.B) {
	val := make([]byte, 1024*1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateValue(val)
	}
}

func BenchmarkValidateValue_4MB(b *testing.B) {
	val := make([]byte, MaxValueLen)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateValue(val)
	}
}

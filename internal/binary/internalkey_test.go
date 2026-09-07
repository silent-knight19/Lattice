package binary_test

import (
	"bytes"
	stdErrors "errors"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

func TestNewInternalKey(t *testing.T) {
	// 1. Valid construction
	validCases := []struct {
		name    string
		userKey []byte
		seqNum  binary.SeqNum
		opType  binary.OpType
	}{
		{"1-byte key", []byte("a"), 1, binary.OpTypePut},
		{"typical key", []byte("user:12345:profile"), 42, binary.OpTypeDelete},
		{"binary key", []byte{0x00, 0xFF, 0x42, 0x00}, 1000, binary.OpTypeTombstone},
		{"max key size", bytes.Repeat([]byte("k"), binary.MaxKeyLen), binary.MaxSeqNum, binary.OpTypePut},
	}

	for _, tc := range validCases {
		t.Run(tc.name, func(t *testing.T) {
			ik, err := binary.NewInternalKey(tc.userKey, tc.seqNum, tc.opType)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(ik.UserKey, tc.userKey) {
				t.Errorf("UserKey mismatch: got %v, expected %v", ik.UserKey, tc.userKey)
			}
			if ik.SeqNum != tc.seqNum {
				t.Errorf("SeqNum mismatch: got %d, expected %d", ik.SeqNum, tc.seqNum)
			}
			if ik.OpType != tc.opType {
				t.Errorf("OpType mismatch: got 0x%02x, expected 0x%02x", byte(ik.OpType), byte(tc.opType))
			}
		})
	}

	// 2. Invalid inputs
	t.Run("empty user key", func(t *testing.T) {
		_, err := binary.NewInternalKey(nil, 1, binary.OpTypePut)
		if !stdErrors.Is(err, errors.ErrEmptyKey) {
			t.Errorf("expected ErrEmptyKey for nil key, got %v", err)
		}

		_, err = binary.NewInternalKey([]byte{}, 1, binary.OpTypePut)
		if !stdErrors.Is(err, errors.ErrEmptyKey) {
			t.Errorf("expected ErrEmptyKey for empty slice, got %v", err)
		}
	})

	t.Run("oversized user key", func(t *testing.T) {
		oversized := make([]byte, binary.MaxKeyLen+1)
		_, err := binary.NewInternalKey(oversized, 1, binary.OpTypePut)
		if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
			t.Errorf("expected ErrKeyTooLarge, got %v", err)
		}
		var typedErr *errors.KeyTooLargeError
		if !stdErrors.As(err, &typedErr) || typedErr.KeySize != uint32(len(oversized)) {
			t.Errorf("failed to extract typed KeyTooLargeError: %+v", typedErr)
		}
	})

	t.Run("invalid op type", func(t *testing.T) {
		_, err := binary.NewInternalKey([]byte("key"), 1, binary.OpTypeInvalid)
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Errorf("expected ErrInvalidOpType for 0x00, got %v", err)
		}

		_, err = binary.NewInternalKey([]byte("key"), 1, binary.OpType(0x03))
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Errorf("expected ErrInvalidOpType for 0x03, got %v", err)
		}
	})

	// 3. Defensive copy / Immutability
	t.Run("defensive copy protects against caller mutation", func(t *testing.T) {
		original := []byte("immutable-key")
		ik, err := binary.NewInternalKey(original, 10, binary.OpTypePut)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Mutate original slice
		original[0] = 'X'

		if ik.UserKey[0] == 'X' {
			t.Fatalf("InternalKey.UserKey was mutated by caller! Expected 'i', got 'X'")
		}
		if string(ik.UserKey) != "immutable-key" {
			t.Fatalf("expected 'immutable-key', got %q", string(ik.UserKey))
		}
	})
}

func TestInternalKey_CloneStringEqual(t *testing.T) {
	k1, err := binary.NewInternalKey([]byte("test-key"), 42, binary.OpTypePut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. Clone
	clone := k1.Clone()
	if !k1.Equal(clone) {
		t.Fatalf("clone must be equal to original")
	}

	// Mutate clone UserKey backing array
	clone.UserKey[0] = 'Z'
	if k1.UserKey[0] == 'Z' {
		t.Fatalf("mutating clone modified original!")
	}

	// Clone nil UserKey
	var zeroKey binary.InternalKey
	zeroClone := zeroKey.Clone()
	if zeroClone.UserKey != nil {
		t.Errorf("cloning nil UserKey should preserve nil")
	}

	// 2. String representation
	str := k1.String()
	if !strings.Contains(str, `"test-key"`) || !strings.Contains(str, "seq=42") || !strings.Contains(str, "op=PUT") {
		t.Errorf("unexpected string output: %q", str)
	}

	// Binary characters in String()
	binKey := binary.InternalKey{
		UserKey: []byte{0x00, 0xFF, 'A'},
		SeqNum:  10,
		OpType:  binary.OpTypeDelete,
	}
	binStr := binKey.String()
	if !strings.Contains(binStr, `\x00\xffA`) && !strings.Contains(binStr, `op=DELETE`) {
		t.Errorf("unexpected binary string output: %q", binStr)
	}

	// 3. Equal and Compare receiver
	k2 := binary.InternalKey{
		UserKey: []byte("test-key"),
		SeqNum:  42,
		OpType:  binary.OpTypePut,
	}
	if !k1.Equal(k2) {
		t.Errorf("expected k1 and k2 to be equal")
	}
	if k1.Compare(k2) != 0 {
		t.Errorf("expected k1.Compare(k2) == 0, got %d", k1.Compare(k2))
	}
}

func TestCompareInternalKey_Ordering(t *testing.T) {
	// Tier 1: UserKey ascending
	t.Run("UserKey ascending", func(t *testing.T) {
		k1 := binary.InternalKey{UserKey: []byte("a"), SeqNum: 100, OpType: binary.OpTypePut}
		k2 := binary.InternalKey{UserKey: []byte("b"), SeqNum: 100, OpType: binary.OpTypePut}
		if binary.CompareInternalKey(k1, k2) >= 0 {
			t.Errorf("expected 'a' < 'b'")
		}
		if binary.CompareInternalKey(k2, k1) <= 0 {
			t.Errorf("expected 'b' > 'a'")
		}

		// Prefix ordering
		kPrefix := binary.InternalKey{UserKey: []byte("abc"), SeqNum: 100, OpType: binary.OpTypePut}
		kLonger := binary.InternalKey{UserKey: []byte("abcd"), SeqNum: 100, OpType: binary.OpTypePut}
		if binary.CompareInternalKey(kPrefix, kLonger) >= 0 {
			t.Errorf("expected 'abc' < 'abcd'")
		}

		// Raw byte ordering (0x00 < 0xFF)
		k0 := binary.InternalKey{UserKey: []byte{0x00}, SeqNum: 10, OpType: binary.OpTypePut}
		kFF := binary.InternalKey{UserKey: []byte{0xFF}, SeqNum: 10, OpType: binary.OpTypePut}
		if binary.CompareInternalKey(k0, kFF) >= 0 {
			t.Errorf("expected 0x00 < 0xFF")
		}
	})

	// Tier 2: SeqNum descending for identical UserKeys
	t.Run("SeqNum descending for same UserKey", func(t *testing.T) {
		// Newer sequence number (100) must sort BEFORE older sequence number (99)
		kNewer := binary.InternalKey{UserKey: []byte("apple"), SeqNum: 100, OpType: binary.OpTypePut}
		kOlder := binary.InternalKey{UserKey: []byte("apple"), SeqNum: 99, OpType: binary.OpTypePut}

		if cmp := binary.CompareInternalKey(kNewer, kOlder); cmp != -1 {
			t.Fatalf("expected newer seq (100) to sort BEFORE older seq (99), got cmp=%d", cmp)
		}
		if cmp := binary.CompareInternalKey(kOlder, kNewer); cmp != 1 {
			t.Fatalf("expected older seq (99) to sort AFTER newer seq (100), got cmp=%d", cmp)
		}

		// Boundary sequence comparison
		kMax := binary.InternalKey{UserKey: []byte("apple"), SeqNum: binary.MaxSeqNum, OpType: binary.OpTypePut}
		kZero := binary.InternalKey{UserKey: []byte("apple"), SeqNum: binary.MinSeqNum, OpType: binary.OpTypePut}

		if binary.CompareInternalKey(kMax, kZero) != -1 {
			t.Errorf("expected MaxSeqNum to sort before MinSeqNum")
		}
		if binary.CompareInternalKey(kZero, kMax) != 1 {
			t.Errorf("expected MinSeqNum to sort after MaxSeqNum")
		}
	})

	// Tier 3: OpType descending for identical UserKey and identical SeqNum
	t.Run("OpType descending for same UserKey and SeqNum", func(t *testing.T) {
		// OpTypeDelete (0x02) > OpTypePut (0x01) -> Delete sorts BEFORE Put (-1)
		kDel := binary.InternalKey{UserKey: []byte("apple"), SeqNum: 50, OpType: binary.OpTypeDelete}
		kPut := binary.InternalKey{UserKey: []byte("apple"), SeqNum: 50, OpType: binary.OpTypePut}

		if cmp := binary.CompareInternalKey(kDel, kPut); cmp != -1 {
			t.Fatalf("expected DELETE to sort before PUT for same key and seq, got %d", cmp)
		}
		if cmp := binary.CompareInternalKey(kPut, kDel); cmp != 1 {
			t.Fatalf("expected PUT to sort after DELETE for same key and seq, got %d", cmp)
		}
	})

	// Identity: All fields identical
	t.Run("Identical InternalKeys return 0", func(t *testing.T) {
		k1 := binary.InternalKey{UserKey: []byte("apple"), SeqNum: 50, OpType: binary.OpTypePut}
		k2 := binary.InternalKey{UserKey: []byte("apple"), SeqNum: 50, OpType: binary.OpTypePut}

		if cmp := binary.CompareInternalKey(k1, k2); cmp != 0 {
			t.Errorf("expected identical keys to compare 0, got %d", cmp)
		}
	})
}

func TestCompareInternalKey_MathematicalLaws(t *testing.T) {
	// Construct a heterogeneous set of keys
	sampleKeys := []binary.InternalKey{
		{UserKey: []byte("a"), SeqNum: 10, OpType: binary.OpTypePut},
		{UserKey: []byte("a"), SeqNum: 20, OpType: binary.OpTypePut},
		{UserKey: []byte("a"), SeqNum: 20, OpType: binary.OpTypeDelete},
		{UserKey: []byte("b"), SeqNum: 5, OpType: binary.OpTypePut},
		{UserKey: []byte("b"), SeqNum: 50, OpType: binary.OpTypeTombstone},
		{UserKey: []byte("c"), SeqNum: binary.MinSeqNum, OpType: binary.OpTypePut},
		{UserKey: []byte("c"), SeqNum: binary.MaxSeqNum, OpType: binary.OpTypePut},
		{UserKey: []byte{0x00}, SeqNum: 1, OpType: binary.OpTypePut},
		{UserKey: []byte{0xFF}, SeqNum: 1, OpType: binary.OpTypePut},
	}

	// 1. Reflexivity: Compare(x, x) == 0
	for i, x := range sampleKeys {
		if cmp := binary.CompareInternalKey(x, x); cmp != 0 {
			t.Fatalf("Reflexivity violated for sample[%d]: got %d", i, cmp)
		}
	}

	// 2. Antisymmetry: sign(Compare(x, y)) == -sign(Compare(y, x))
	for i, x := range sampleKeys {
		for j, y := range sampleKeys {
			cXY := binary.CompareInternalKey(x, y)
			cYX := binary.CompareInternalKey(y, x)

			if cXY == 0 && cYX != 0 {
				t.Fatalf("Antisymmetry violated: cXY=0 but cYX=%d (i=%d, j=%d)", cYX, i, j)
			}
			if cXY > 0 && cYX >= 0 {
				t.Fatalf("Antisymmetry violated: cXY=%d, cYX=%d (i=%d, j=%d)", cXY, cYX, i, j)
			}
			if cXY < 0 && cYX <= 0 {
				t.Fatalf("Antisymmetry violated: cXY=%d, cYX=%d (i=%d, j=%d)", cXY, cYX, i, j)
			}
		}
	}

	// 3. Transitivity: If x < y and y < z, then x < z
	for _, x := range sampleKeys {
		for _, y := range sampleKeys {
			for _, z := range sampleKeys {
				if binary.CompareInternalKey(x, y) < 0 && binary.CompareInternalKey(y, z) < 0 {
					if cmpXZ := binary.CompareInternalKey(x, z); cmpXZ >= 0 {
						t.Fatalf("Transitivity violated: x=%v, y=%v, z=%v, cmp(x,z)=%d", x, y, z, cmpXZ)
					}
				}
			}
		}
	}
}

func TestCompareInternalKey_SortIntegration(t *testing.T) {
	// A collection of multi-version records for multiple keys
	keys := []binary.InternalKey{
		{UserKey: []byte("apple"), SeqNum: 5, OpType: binary.OpTypePut},
		{UserKey: []byte("apple"), SeqNum: 10, OpType: binary.OpTypeDelete},
		{UserKey: []byte("apple"), SeqNum: 1, OpType: binary.OpTypePut},
		{UserKey: []byte("banana"), SeqNum: 2, OpType: binary.OpTypePut},
		{UserKey: []byte("banana"), SeqNum: 7, OpType: binary.OpTypePut},
		{UserKey: []byte("cherry"), SeqNum: 3, OpType: binary.OpTypeDelete},
	}

	// Sort using canonical CompareInternalKey
	sort.Slice(keys, func(i, j int) bool {
		return binary.CompareInternalKey(keys[i], keys[j]) < 0
	})

	// Expected canonical order:
	// 1. "apple" seq=10 (newest apple first)
	// 2. "apple" seq=5
	// 3. "apple" seq=1
	// 4. "banana" seq=7 (newest banana first)
	// 5. "banana" seq=2
	// 6. "cherry" seq=3
	expected := []struct {
		userKey string
		seqNum  binary.SeqNum
		opType  binary.OpType
	}{
		{"apple", 10, binary.OpTypeDelete},
		{"apple", 5, binary.OpTypePut},
		{"apple", 1, binary.OpTypePut},
		{"banana", 7, binary.OpTypePut},
		{"banana", 2, binary.OpTypePut},
		{"cherry", 3, binary.OpTypeDelete},
	}

	for i, exp := range expected {
		if string(keys[i].UserKey) != exp.userKey || keys[i].SeqNum != exp.seqNum || keys[i].OpType != exp.opType {
			t.Fatalf("sorted order mismatch at index %d:\n got:      %s\n expected: %v", i, keys[i].String(), exp)
		}
	}
}

func TestEncodeDecodeInternalKey(t *testing.T) {
	testCases := []struct {
		name    string
		userKey []byte
		seqNum  binary.SeqNum
		opType  binary.OpType
	}{
		{"1-byte key", []byte("k"), 1, binary.OpTypePut},
		{"typical user key", []byte("users:session:98765"), 123456789, binary.OpTypeDelete},
		{"binary bytes", []byte{0x00, 0x01, 0xFF, 0xFE, 0x00}, 42, binary.OpTypeTombstone},
		{"max key size", bytes.Repeat([]byte("x"), binary.MaxKeyLen), binary.MaxSeqNum, binary.OpTypePut},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ik, err := binary.NewInternalKey(tc.userKey, tc.seqNum, tc.opType)
			if err != nil {
				t.Fatalf("failed to create InternalKey: %v", err)
			}

			// Encode
			encoded := binary.EncodeInternalKey(ik)
			expectedLen := len(tc.userKey) + binary.InternalKeyTrailerLen
			if len(encoded) != expectedLen {
				t.Fatalf("encoded length mismatch: got %d, expected %d", len(encoded), expectedLen)
			}

			// Decode
			decoded, err := binary.DecodeInternalKey(encoded)
			if err != nil {
				t.Fatalf("failed to decode internal key: %v", err)
			}

			if !decoded.Equal(ik) {
				t.Fatalf("decoded key does not equal original:\n got:  %s\n want: %s", decoded.String(), ik.String())
			}
			if !bytes.Equal(decoded.UserKey, ik.UserKey) {
				t.Fatalf("UserKey mismatch after decode")
			}
			if decoded.SeqNum != ik.SeqNum {
				t.Fatalf("SeqNum mismatch after decode: got %d, want %d", decoded.SeqNum, ik.SeqNum)
			}
			if decoded.OpType != ik.OpType {
				t.Fatalf("OpType mismatch after decode: got %v, want %v", decoded.OpType, ik.OpType)
			}
		})
	}

	// AppendInternalKey reuse
	t.Run("AppendInternalKey with preallocated buffer", func(t *testing.T) {
		ik := binary.InternalKey{UserKey: []byte("append-test"), SeqNum: 99, OpType: binary.OpTypePut}
		prefix := []byte("prefix-header:")
		combined := binary.AppendInternalKey(prefix, ik)

		expectedLen := len(prefix) + len(ik.UserKey) + binary.InternalKeyTrailerLen
		if len(combined) != expectedLen {
			t.Fatalf("expected len %d, got %d", expectedLen, len(combined))
		}

		// Decode the appended portion
		decoded, err := binary.DecodeInternalKey(combined[len(prefix):])
		if err != nil {
			t.Fatalf("failed to decode appended portion: %v", err)
		}
		if !decoded.Equal(ik) {
			t.Fatalf("decoded appended key mismatch: got %s, want %s", decoded.String(), ik.String())
		}
	})

	// Decode error cases
	t.Run("Decode errors", func(t *testing.T) {
		// 1. Truncated buffers (< 10 bytes)
		for l := 0; l < binary.MinKeyLen+binary.InternalKeyTrailerLen; l++ {
			truncBuf := make([]byte, l)
			_, err := binary.DecodeInternalKey(truncBuf)
			if !stdErrors.Is(err, errors.ErrInternalKeyTruncated) {
				t.Errorf("expected ErrInternalKeyTruncated for length %d, got %v", l, err)
			}
		}

		// 2. Oversized data (> MaxKeyLen + 9 = 65544)
		oversized := make([]byte, binary.MaxKeyLen+binary.InternalKeyTrailerLen+1)
		_, err := binary.DecodeInternalKey(oversized)
		if !stdErrors.Is(err, errors.ErrKeyTooLarge) {
			t.Errorf("expected ErrKeyTooLarge for oversized data, got %v", err)
		}

		// 3. Invalid OpType in trailer (e.g. 0x00 or 0x03)
		validKey := binary.InternalKey{UserKey: []byte("valid"), SeqNum: 10, OpType: binary.OpTypePut}
		encoded := binary.EncodeInternalKey(validKey)

		// Corrupt OpType byte at the end
		encoded[len(encoded)-1] = 0x00
		_, err = binary.DecodeInternalKey(encoded)
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Errorf("expected ErrInvalidOpType for corrupted 0x00 opcode, got %v", err)
		}

		encoded[len(encoded)-1] = 0xFE
		_, err = binary.DecodeInternalKey(encoded)
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Errorf("expected ErrInvalidOpType for corrupted 0xFE opcode, got %v", err)
		}
	})

	// Immutability: Decoded UserKey is an independent copy
	t.Run("Decode UserKey immutability", func(t *testing.T) {
		ik := binary.InternalKey{UserKey: []byte("decode-immune"), SeqNum: 1, OpType: binary.OpTypePut}
		buf := binary.EncodeInternalKey(ik)

		decoded, err := binary.DecodeInternalKey(buf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Mutate source buffer
		buf[0] = 'Z'
		if decoded.UserKey[0] == 'Z' {
			t.Fatalf("decoded UserKey shares memory with input buffer!")
		}
	})
}

// -----------------------------------------------------------------------------
// Fuzz Testing
// -----------------------------------------------------------------------------

func FuzzCompareInternalKey(f *testing.F) {
	// Seed corpus with distinct keys and boundaries
	f.Add([]byte("a"), uint64(1), byte(0x01), []byte("b"), uint64(2), byte(0x01))
	f.Add([]byte("k"), uint64(100), byte(0x01), []byte("k"), uint64(50), byte(0x02))
	f.Add([]byte("k"), uint64(100), byte(0x02), []byte("k"), uint64(100), byte(0x01))
	f.Add([]byte{0x00}, uint64(0), byte(0x01), []byte{0xFF}, uint64(math.MaxUint64), byte(0x02))

	f.Fuzz(func(t *testing.T, k1Bytes []byte, s1 uint64, op1 byte, k2Bytes []byte, s2 uint64, op2 byte) {
		// Bound key lengths for practical fuzz performance
		if len(k1Bytes) > 1024 {
			k1Bytes = k1Bytes[:1024]
		}
		if len(k2Bytes) > 1024 {
			k2Bytes = k2Bytes[:1024]
		}

		ik1 := binary.InternalKey{UserKey: k1Bytes, SeqNum: binary.SeqNum(s1), OpType: binary.OpType(op1)}
		ik2 := binary.InternalKey{UserKey: k2Bytes, SeqNum: binary.SeqNum(s2), OpType: binary.OpType(op2)}

		// 1. Reflexivity
		if cmp := binary.CompareInternalKey(ik1, ik1); cmp != 0 {
			t.Fatalf("reflexivity violated: %d", cmp)
		}
		if cmp := binary.CompareInternalKey(ik2, ik2); cmp != 0 {
			t.Fatalf("reflexivity violated: %d", cmp)
		}

		// 2. Antisymmetry
		cmp12 := binary.CompareInternalKey(ik1, ik2)
		cmp21 := binary.CompareInternalKey(ik2, ik1)

		if cmp12 != -cmp21 {
			t.Fatalf("antisymmetry violated: cmp(1,2)=%d, cmp(2,1)=%d", cmp12, cmp21)
		}
	})
}

func FuzzInternalKeyRoundTrip(f *testing.F) {
	f.Add([]byte("user:fuzz:key"), uint64(42), byte(0x01))
	f.Add([]byte("tombstone:key"), uint64(100000), byte(0x02))
	f.Add([]byte{0x00, 0x01, 0x02}, uint64(0), byte(0x01))
	f.Add([]byte{0xFF, 0xFE, 0xFD}, uint64(math.MaxUint64), byte(0x02))

	f.Fuzz(func(t *testing.T, userKey []byte, seqVal uint64, opByte byte) {
		// Only test valid inputs
		if len(userKey) < binary.MinKeyLen || len(userKey) > 1024 {
			return
		}
		op := binary.OpType(opByte)
		if !op.Valid() {
			return
		}

		ik := binary.InternalKey{
			UserKey: userKey,
			SeqNum:  binary.SeqNum(seqVal),
			OpType:  op,
		}

		encoded := binary.EncodeInternalKey(ik)
		decoded, err := binary.DecodeInternalKey(encoded)
		if err != nil {
			t.Fatalf("unexpected decode failure: %v", err)
		}

		if !decoded.Equal(ik) {
			t.Fatalf("round-trip inequality:\n got:  %s\n want: %s", decoded.String(), ik.String())
		}
	})
}

// -----------------------------------------------------------------------------
// Benchmarks
// -----------------------------------------------------------------------------

func BenchmarkCompareInternalKey_16B(b *testing.B) {
	k1 := binary.InternalKey{UserKey: []byte("0123456789abcdef"), SeqNum: 100, OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: []byte("0123456789abcdeg"), SeqNum: 100, OpType: binary.OpTypePut}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = binary.CompareInternalKey(k1, k2)
	}
}

func BenchmarkCompareInternalKey_1KB(b *testing.B) {
	u1 := bytes.Repeat([]byte("a"), 1024)
	u2 := bytes.Repeat([]byte("a"), 1023)
	u2 = append(u2, 'b')

	k1 := binary.InternalKey{UserKey: u1, SeqNum: 100, OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: u2, SeqNum: 100, OpType: binary.OpTypePut}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = binary.CompareInternalKey(k1, k2)
	}
}

func BenchmarkCompareInternalKey_64KB(b *testing.B) {
	u1 := bytes.Repeat([]byte("a"), 65535)
	u2 := bytes.Repeat([]byte("a"), 65534)
	u2 = append(u2, 'b')

	k1 := binary.InternalKey{UserKey: u1, SeqNum: 100, OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: u2, SeqNum: 100, OpType: binary.OpTypePut}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = binary.CompareInternalKey(k1, k2)
	}
}

func BenchmarkCompareInternalKey_SameKey_DifferentSeq(b *testing.B) {
	key := []byte("frequent-hot-user-key")
	k1 := binary.InternalKey{UserKey: key, SeqNum: 1000, OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: key, SeqNum: 999, OpType: binary.OpTypePut}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = binary.CompareInternalKey(k1, k2)
	}
}

func BenchmarkAppendInternalKey(b *testing.B) {
	ik := binary.InternalKey{UserKey: []byte("benchmark-key-payload"), SeqNum: 42, OpType: binary.OpTypePut}
	buf := make([]byte, 0, 128)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = binary.AppendInternalKey(buf[:0], ik)
	}
}

func BenchmarkDecodeInternalKey(b *testing.B) {
	ik := binary.InternalKey{UserKey: []byte("benchmark-key-payload"), SeqNum: 42, OpType: binary.OpTypePut}
	encoded := binary.EncodeInternalKey(ik)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := binary.DecodeInternalKey(encoded)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestRandomizedComparatorTriples(t *testing.T) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	keys := make([]binary.InternalKey, 100)

	for i := 0; i < 100; i++ {
		keyLen := 1 + rng.Intn(32)
		uKey := make([]byte, keyLen)
		rng.Read(uKey)

		op := binary.OpTypePut
		if rng.Intn(2) == 1 {
			op = binary.OpTypeDelete
		}

		keys[i] = binary.InternalKey{
			UserKey: uKey,
			SeqNum:  binary.SeqNum(rng.Uint64()),
			OpType:  op,
		}
	}

	// Verify transitivity on 5000 random triples
	for i := 0; i < 5000; i++ {
		x := keys[rng.Intn(100)]
		y := keys[rng.Intn(100)]
		z := keys[rng.Intn(100)]

		if binary.CompareInternalKey(x, y) < 0 && binary.CompareInternalKey(y, z) < 0 {
			if cmpXZ := binary.CompareInternalKey(x, z); cmpXZ >= 0 {
				t.Fatalf("Transitivity failure on randomized triple:\n x=%s\n y=%s\n z=%s\n cmp(x,z)=%d",
					x.String(), y.String(), z.String(), cmpXZ)
			}
		}
	}
}

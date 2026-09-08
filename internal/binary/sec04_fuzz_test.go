package binary_test

import (
	"bytes"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// SEC-04: In-Memory Storage Subsystem Fuzz Testing
//
// Fuzz targets:
//   1. FuzzInternalKeyComparator: Verifies strict weak ordering, anti-symmetry, and reflexivity under arbitrary byte sequences.
//   2. FuzzInternalKeyCodec: Verifies robust deserialization, truncation handling, and roundtrip consistency.
//   3. FuzzKeyValidation: Verifies hard boundary enforcement on arbitrary inputs.

// FuzzInternalKeyComparator tests that CompareInternalKey satisfies mathematical strict weak ordering
// across arbitrary randomized keys, sequence numbers, and operation types.
func FuzzInternalKeyComparator(f *testing.F) {
	// Seed inputs
	seeds := []struct {
		k1, k2 []byte
		s1, s2 uint64
		op1    byte
		op2    byte
	}{
		{[]byte("keyA"), []byte("keyB"), 10, 20, 1, 2},
		{[]byte("key"), []byte("key"), 10, 10, 1, 1},
		{[]byte("key"), []byte("key"), 10, 20, 1, 1},
		{[]byte("key"), []byte("key"), 10, 10, 1, 2},
		{[]byte{0xFF}, []byte{0x00}, 0, math.MaxUint64, 2, 1},
	}
	for _, s := range seeds {
		f.Add(s.k1, s.k2, s.s1, s.s2, s.op1, s.op2)
	}

	f.Fuzz(func(t *testing.T, k1, k2 []byte, s1, s2 uint64, op1, op2 byte) {
		ikA := binary.InternalKey{UserKey: k1, SeqNum: binary.SeqNum(s1), OpType: binary.OpType(op1)}
		ikB := binary.InternalKey{UserKey: k2, SeqNum: binary.SeqNum(s2), OpType: binary.OpType(op2)}

		// 1. Reflexivity: cmp(A, A) == 0
		if cmp := binary.CompareInternalKey(ikA, ikA); cmp != 0 {
			t.Fatalf("Reflexivity failure on ikA: cmp=%d", cmp)
		}
		if cmp := binary.CompareInternalKey(ikB, ikB); cmp != 0 {
			t.Fatalf("Reflexivity failure on ikB: cmp=%d", cmp)
		}

		// 2. Anti-symmetry: cmp(A, B) == -cmp(B, A)
		cmpAB := binary.CompareInternalKey(ikA, ikB)
		cmpBA := binary.CompareInternalKey(ikB, ikA)
		if cmpAB != -cmpBA {
			t.Fatalf("Anti-symmetry violation: cmp(A,B)=%d, cmp(B,A)=%d", cmpAB, cmpBA)
		}

		// 3. UserKey ascending consistency
		userKeyCmp := bytes.Compare(ikA.UserKey, ikB.UserKey)
		if userKeyCmp != 0 {
			if cmpAB != userKeyCmp {
				t.Fatalf("UserKey ordering mismatch: bytes.Compare=%d, CompareInternalKey=%d", userKeyCmp, cmpAB)
			}
		} else {
			// Identical UserKeys: SeqNum descending takes precedence
			if ikA.SeqNum > ikB.SeqNum && cmpAB != -1 {
				t.Fatalf("SeqNum descending violation: %d > %d, but cmp=%d", ikA.SeqNum, ikB.SeqNum, cmpAB)
			}
			if ikA.SeqNum < ikB.SeqNum && cmpAB != 1 {
				t.Fatalf("SeqNum descending violation: %d < %d, but cmp=%d", ikA.SeqNum, ikB.SeqNum, cmpAB)
			}
		}
	})
}

// FuzzInternalKeyCodec tests DecodeInternalKey on arbitrary byte inputs to guarantee
// no panics, proper error rejection, and strict roundtripping on valid decodes.
func FuzzInternalKeyCodec(f *testing.F) {
	// Seed inputs
	f.Add([]byte{})
	f.Add(make([]byte, binary.InternalKeyTrailerLen))
	f.Add(make([]byte, binary.MinKeyLen+binary.InternalKeyTrailerLen))

	validKey, _ := binary.NewInternalKey([]byte("valid-seed-key"), 42, binary.OpTypePut)
	f.Add(binary.EncodeInternalKey(validKey))

	f.Fuzz(func(t *testing.T, data []byte) {
		ik, err := binary.DecodeInternalKey(data)
		if err == nil {
			// If decoding succeeded, key must satisfy invariants
			if valErr := binary.ValidateKey(ik.UserKey); valErr != nil {
				t.Fatalf("SECURITY VIOLATION: DecodeInternalKey returned invalid UserKey: %v", valErr)
			}
			if opErr := ik.OpType.Validate(); opErr != nil {
				t.Fatalf("SECURITY VIOLATION: DecodeInternalKey returned invalid OpType: %v", opErr)
			}

			// Roundtrip re-encode must match decoded key exactly
			encoded := binary.EncodeInternalKey(ik)
			reDecoded, reErr := binary.DecodeInternalKey(encoded)
			if reErr != nil {
				t.Fatalf("failed to re-decode encoded key: %v", reErr)
			}
			if !reDecoded.Equal(ik) {
				t.Fatalf("roundtrip mismatch: got %v, want %v", reDecoded, ik)
			}
		}
	})
}

// FuzzKeyValidation exercises ValidateKey and ValidateValue with arbitrary byte inputs.
func FuzzKeyValidation(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("normal-key"))
	f.Add(bytes.Repeat([]byte("k"), 1000))

	f.Fuzz(func(t *testing.T, data []byte) {
		err := binary.ValidateKey(data)
		if len(data) == 0 || len(data) > binary.MaxKeyLen {
			if err == nil {
				t.Fatalf("SECURITY VIOLATION: ValidateKey accepted invalid key of length %d", len(data))
			}
		} else {
			if err != nil {
				t.Fatalf("ValidateKey rejected valid key of length %d: %v", len(data), err)
			}
		}

		valErr := binary.ValidateValue(data)
		if len(data) > binary.MaxValueLen {
			if valErr == nil {
				t.Fatalf("SECURITY VIOLATION: ValidateValue accepted value of length %d > MaxValueLen", len(data))
			}
		} else {
			if valErr != nil {
				t.Fatalf("ValidateValue rejected valid value of length %d: %v", len(data), valErr)
			}
		}
	})
}

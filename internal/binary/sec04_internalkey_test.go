package binary_test

import (
	stdErrors "errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// SEC-04: In-Memory Storage & InternalKey Dynamic Security Audit
//
// Invariants tested:
//   1. Memory Ownership: NewInternalKey defensively copies UserKey; external slice mutation cannot corrupt stored key.
//   2. Deep Copying: Clone() produces an independent backing array; mutating the clone does not affect the source.
//   3. Canonical Multi-Version Ordering: UserKey ASC, SeqNum DESC, OpType DESC.
//   4. Strict Weak Ordering: Reflexivity, anti-symmetry, and transitivity hold across all inputs.
//   5. Concurrency Safety: Comparator, constructor, clone, and serialization are race-free under high concurrency.
//   6. Information Disclosure: String representation safely Go-quotes binary bytes and prevents unescaped control characters.

// TestSEC04_Ownership_01_InputSliceMutationDoesNotCorruptInternalKey verifies that mutating
// the caller's slice after calling NewInternalKey has ZERO effect on the internal key state.
func TestSEC04_Ownership_01_InputSliceMutationDoesNotCorruptInternalKey(t *testing.T) {
	originalKey := []byte("user-account-key-001")
	ik, err := binary.NewInternalKey(originalKey, 100, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}

	// Adversarial attack: caller mutates original buffer
	for i := range originalKey {
		originalKey[i] = 0xFF
	}

	expectedKey := "user-account-key-001"
	if string(ik.UserKey) != expectedKey {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-01]: Caller slice mutation corrupted internal key! got %q, want %q", ik.UserKey, expectedKey)
	}
}

// TestSEC04_Ownership_02_CloneDeepCopyIsolation verifies that mutating the cloned
// InternalKey does not alias or mutate the original InternalKey.
func TestSEC04_Ownership_02_CloneDeepCopyIsolation(t *testing.T) {
	orig, err := binary.NewInternalKey([]byte("original-key"), 50, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}

	cloned := orig.Clone()
	if !orig.Equal(cloned) {
		t.Fatalf("expected cloned key to be equal to original")
	}

	// Adversarial mutation of cloned buffer
	cloned.UserKey[0] = 'Z'

	if orig.UserKey[0] == 'Z' {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-01]: Mutating clone mutated original key! Backing arrays aliased!")
	}
	if string(orig.UserKey) != "original-key" {
		t.Fatalf("original key corrupted: %s", orig.UserKey)
	}
}

// TestSEC04_Comparator_01_UserKeyAscending verifies that UserKey comparison is strictly
// unsigned byte-by-byte lexicographical ascending.
func TestSEC04_Comparator_01_UserKeyAscending(t *testing.T) {
	cases := []struct {
		keyA string
		keyB string
		want int // -1 if A < B, 0 if A == B, +1 if A > B
	}{
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a", 0},
		{"apple", "apply", -1},
		{"prefix", "prefix_extension", -1},
		{"", "", 0},
	}

	for _, tc := range cases {
		ikA := binary.InternalKey{UserKey: []byte(tc.keyA), SeqNum: 10, OpType: binary.OpTypePut}
		ikB := binary.InternalKey{UserKey: []byte(tc.keyB), SeqNum: 10, OpType: binary.OpTypePut}

		got := binary.CompareInternalKey(ikA, ikB)
		if got != tc.want {
			t.Errorf("CompareInternalKey(%q, %q) = %d, want %d", tc.keyA, tc.keyB, got, tc.want)
		}
	}

	// High-bit unsigned byte test: 0x7F must sort BEFORE 0x80, and 0x80 must sort BEFORE 0xFF
	ik7F := binary.InternalKey{UserKey: []byte{0x7F}, SeqNum: 1, OpType: binary.OpTypePut}
	ik80 := binary.InternalKey{UserKey: []byte{0x80}, SeqNum: 1, OpType: binary.OpTypePut}
	ikFF := binary.InternalKey{UserKey: []byte{0xFF}, SeqNum: 1, OpType: binary.OpTypePut}

	if cmp := binary.CompareInternalKey(ik7F, ik80); cmp >= 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: Signed byte comparison bug! 0x7F sorted >= 0x80 (cmp=%d)", cmp)
	}
	if cmp := binary.CompareInternalKey(ik80, ikFF); cmp >= 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: 0x80 sorted >= 0xFF (cmp=%d)", cmp)
	}
}

// TestSEC04_Comparator_02_SeqNumDescending verifies that for identical UserKeys,
// newer/higher sequence numbers sort strictly BEFORE older/lower sequence numbers.
func TestSEC04_Comparator_02_SeqNumDescending(t *testing.T) {
	userKey := []byte("shared-key")

	ikNewer := binary.InternalKey{UserKey: userKey, SeqNum: 100, OpType: binary.OpTypePut}
	ikOlder := binary.InternalKey{UserKey: userKey, SeqNum: 50, OpType: binary.OpTypePut}
	ikZero := binary.InternalKey{UserKey: userKey, SeqNum: 0, OpType: binary.OpTypePut}
	ikMax := binary.InternalKey{UserKey: userKey, SeqNum: math.MaxUint64, OpType: binary.OpTypePut}

	// Newer must sort BEFORE older (-1)
	if cmp := binary.CompareInternalKey(ikNewer, ikOlder); cmp != -1 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: Newer SeqNum 100 did not sort before older SeqNum 50! cmp=%d", cmp)
	}
	if cmp := binary.CompareInternalKey(ikOlder, ikNewer); cmp != 1 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: Older SeqNum 50 did not sort after newer SeqNum 100! cmp=%d", cmp)
	}

	// MaxUint64 must sort BEFORE SeqNum 100
	if cmp := binary.CompareInternalKey(ikMax, ikNewer); cmp != -1 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: MaxUint64 did not sort before SeqNum 100! cmp=%d", cmp)
	}

	// SeqNum 0 must sort AFTER SeqNum 50
	if cmp := binary.CompareInternalKey(ikZero, ikOlder); cmp != 1 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: SeqNum 0 did not sort after SeqNum 50! cmp=%d", cmp)
	}
}

// TestSEC04_Comparator_03_OpTypeTieBreakerDescending verifies that for identical UserKey
// and identical SeqNum, OpTypeDelete (0x02) sorts strictly BEFORE OpTypePut (0x01).
func TestSEC04_Comparator_03_OpTypeTieBreakerDescending(t *testing.T) {
	userKey := []byte("tie-key")
	seqNum := binary.SeqNum(42)

	ikDelete := binary.InternalKey{UserKey: userKey, SeqNum: seqNum, OpType: binary.OpTypeDelete}
	ikPut := binary.InternalKey{UserKey: userKey, SeqNum: seqNum, OpType: binary.OpTypePut}

	// Delete must sort BEFORE Put (-1)
	if cmp := binary.CompareInternalKey(ikDelete, ikPut); cmp != -1 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: OpTypeDelete did not sort before OpTypePut! cmp=%d", cmp)
	}
	if cmp := binary.CompareInternalKey(ikPut, ikDelete); cmp != 1 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: OpTypePut did not sort after OpTypeDelete! cmp=%d", cmp)
	}

	// Identical keys must return 0
	if cmp := binary.CompareInternalKey(ikDelete, ikDelete); cmp != 0 {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-02]: Identical key comparison returned non-zero! cmp=%d", cmp)
	}
}

// TestSEC04_Comparator_04_StrictWeakOrdering verifies mathematical comparator properties:
// Reflexivity, Anti-symmetry, and Transitivity across multi-version internal keys.
func TestSEC04_Comparator_04_StrictWeakOrdering(t *testing.T) {
	keys := []binary.InternalKey{
		{UserKey: []byte("a"), SeqNum: 10, OpType: binary.OpTypePut},
		{UserKey: []byte("a"), SeqNum: 20, OpType: binary.OpTypePut},
		{UserKey: []byte("a"), SeqNum: 20, OpType: binary.OpTypeDelete},
		{UserKey: []byte("b"), SeqNum: 5, OpType: binary.OpTypePut},
		{UserKey: []byte("b"), SeqNum: 15, OpType: binary.OpTypePut},
		{UserKey: []byte("c"), SeqNum: 1, OpType: binary.OpTypePut},
		{UserKey: []byte("c"), SeqNum: math.MaxUint64, OpType: binary.OpTypePut},
	}

	// 1. Reflexivity: cmp(x, x) == 0
	for i, k := range keys {
		if cmp := binary.CompareInternalKey(k, k); cmp != 0 {
			t.Fatalf("Reflexivity violation at index %d: cmp(k, k) = %d", i, cmp)
		}
	}

	// 2. Anti-symmetry: cmp(x, y) == -cmp(y, x)
	for i := range keys {
		for j := range keys {
			cmpXY := binary.CompareInternalKey(keys[i], keys[j])
			cmpYX := binary.CompareInternalKey(keys[j], keys[i])
			if cmpXY != -cmpYX {
				t.Fatalf("Anti-symmetry violation between %d and %d: cmp(x, y)=%d, cmp(y, x)=%d", i, j, cmpXY, cmpYX)
			}
		}
	}

	// 3. Transitivity: if cmp(x, y) <= 0 and cmp(y, z) <= 0 => cmp(x, z) <= 0
	for i := range keys {
		for j := range keys {
			for k := range keys {
				cmpIJ := binary.CompareInternalKey(keys[i], keys[j])
				cmpJK := binary.CompareInternalKey(keys[j], keys[k])
				cmpIK := binary.CompareInternalKey(keys[i], keys[k])

				if cmpIJ < 0 && cmpJK < 0 && cmpIK >= 0 {
					t.Fatalf("Transitivity violation: cmp(i, j)=%d, cmp(j, k)=%d, but cmp(i, k)=%d", cmpIJ, cmpJK, cmpIK)
				}
			}
		}
	}
}

// TestSEC04_Validation_01_KeyBoundsEnforced verifies that ValidateKey strictly rejects
// keys of length 0 or > 65,535 bytes without allocating heap memory.
func TestSEC04_Validation_01_KeyBoundsEnforced(t *testing.T) {
	// Empty keys rejected
	if err := binary.ValidateKey(nil); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected ErrEmptyKey for nil, got: %v", err)
	}
	if err := binary.ValidateKey([]byte{}); !stdErrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("expected ErrEmptyKey for empty slice, got: %v", err)
	}

	// Maximum allowed key (65,535 bytes) accepted
	maxKey := make([]byte, binary.MaxKeyLen)
	if err := binary.ValidateKey(maxKey); err != nil {
		t.Fatalf("ValidateKey rejected valid 65535-byte key: %v", err)
	}

	// Oversized key (65,536 bytes) rejected
	oversizedKey := make([]byte, binary.MaxKeyLen+1)
	err := binary.ValidateKey(oversizedKey)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-03]: ValidateKey accepted key exceeding MaxKeyLen!")
	}
	var keyTooLargeErr *errors.KeyTooLargeError
	if !stdErrors.As(err, &keyTooLargeErr) {
		t.Fatalf("expected *errors.KeyTooLargeError, got: %T (%v)", err, err)
	}
	if keyTooLargeErr.KeySize != uint32(len(oversizedKey)) {
		t.Errorf("key size mismatch: got %d, want %d", keyTooLargeErr.KeySize, len(oversizedKey))
	}
}

// TestSEC04_Validation_02_ValueBoundsEnforced verifies that ValidateValue strictly rejects
// values > 4,194,304 bytes.
func TestSEC04_Validation_02_ValueBoundsEnforced(t *testing.T) {
	// Empty / nil value accepted
	if err := binary.ValidateValue(nil); err != nil {
		t.Fatalf("ValidateValue rejected nil value: %v", err)
	}
	if err := binary.ValidateValue([]byte{}); err != nil {
		t.Fatalf("ValidateValue rejected empty value: %v", err)
	}

	// Max value (4 MB) accepted
	maxVal := make([]byte, binary.MaxValueLen)
	if err := binary.ValidateValue(maxVal); err != nil {
		t.Fatalf("ValidateValue rejected valid 4MB value: %v", err)
	}

	// Oversized value (4 MB + 1 byte) rejected
	oversizedVal := make([]byte, binary.MaxValueLen+1)
	err := binary.ValidateValue(oversizedVal)
	if err == nil {
		t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-03]: ValidateValue accepted value exceeding MaxValueLen!")
	}
	var valTooLargeErr *errors.ValueTooLargeError
	if !stdErrors.As(err, &valTooLargeErr) {
		t.Fatalf("expected *errors.ValueTooLargeError, got: %T (%v)", err, err)
	}
}

// TestSEC04_Codec_01_TrailerRoundtripAndTruncation verifies trailer encoding and decoding.
func TestSEC04_Codec_01_TrailerRoundtripAndTruncation(t *testing.T) {
	ik, err := binary.NewInternalKey([]byte("payload-key"), 99999, binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey failed: %v", err)
	}

	encoded := binary.EncodeInternalKey(ik)
	if len(encoded) != len(ik.UserKey)+binary.InternalKeyTrailerLen {
		t.Fatalf("encoded length mismatch: got %d, want %d", len(encoded), len(ik.UserKey)+binary.InternalKeyTrailerLen)
	}

	decoded, err := binary.DecodeInternalKey(encoded)
	if err != nil {
		t.Fatalf("DecodeInternalKey failed: %v", err)
	}
	if !decoded.Equal(ik) {
		t.Fatalf("decoded key not equal to original key: got %v, want %v", decoded, ik)
	}

	// Adversarial truncation: buffer shorter than trailer (9 bytes)
	for i := 0; i < binary.InternalKeyTrailerLen; i++ {
		truncated := make([]byte, i)
		_, err := binary.DecodeInternalKey(truncated)
		if err == nil {
			t.Fatalf("SECURITY VIOLATION [SEC-MEM-INV-04]: DecodeInternalKey accepted truncated buffer of len %d!", i)
		}
		if !stdErrors.Is(err, errors.ErrInternalKeyTruncated) {
			t.Errorf("expected ErrInternalKeyTruncated, got: %v", err)
		}
	}
}

// TestSEC04_Concurrency_01_HighConcurrencyReadWrite verifies that InternalKey construction,
// comparison, cloning, and encoding remain 100% race-free under 64 concurrent goroutines.
func TestSEC04_Concurrency_01_HighConcurrencyReadWrite(t *testing.T) {
	const numGoroutines = 64
	const opsPerGoroutine = 500

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				keyStr := fmt.Sprintf("concurrent-key-%d-%d", id, i%20)
				ik, err := binary.NewInternalKey([]byte(keyStr), binary.SeqNum(i+1), binary.OpTypePut)
				if err != nil {
					t.Errorf("NewInternalKey failed: %v", err)
					return
				}

				cloned := ik.Clone()
				if !ik.Equal(cloned) {
					t.Errorf("cloned not equal to ik")
					return
				}

				encoded := binary.EncodeInternalKey(ik)
				decoded, decErr := binary.DecodeInternalKey(encoded)
				if decErr != nil || !decoded.Equal(ik) {
					t.Errorf("codec mismatch in concurrent loop: %v", decErr)
					return
				}

				// Self-comparison
				if cmp := binary.CompareInternalKey(ik, decoded); cmp != 0 {
					t.Errorf("CompareInternalKey returned non-zero for equal keys: %d", cmp)
					return
				}
			}
		}(g)
	}

	wg.Wait()
}

// TestSEC04_Disclosure_01_StringFormatSafelyQuotesBinary verifies that InternalKey.DebugString()
// quotes arbitrary binary bytes safely without producing unescaped control characters,
// while InternalKey.String() redacts the key payload by default.
func TestSEC04_Disclosure_01_StringFormatSafelyQuotesBinary(t *testing.T) {
	binaryKey := []byte{0x00, 0x07, 0x1B, 0xFF, 'h', 'e', 'l', 'l', 'o'}
	ik := binary.InternalKey{
		UserKey: binaryKey,
		SeqNum:  42,
		OpType:  binary.OpTypePut,
	}

	// Default String() must redact raw key bytes
	defaultStr := ik.String()
	if strings.Contains(defaultStr, "\x00") || strings.Contains(defaultStr, "hello") {
		t.Fatalf("InternalKey.String() leaked raw key bytes: %s", defaultStr)
	}
	if !strings.Contains(defaultStr, "len=9") || !strings.Contains(defaultStr, "seq=42") || !strings.Contains(defaultStr, "op=PUT") {
		t.Fatalf("InternalKey.String() missing metadata: %s", defaultStr)
	}

	// fmt.Sprintf("%v") must also be redacted
	fmtStr := fmt.Sprintf("%v", ik)
	if strings.Contains(fmtStr, "\x00") || strings.Contains(fmtStr, "hello") {
		t.Fatalf("fmt.Sprintf(%%v) leaked raw key bytes: %s", fmtStr)
	}

	// Forensic DebugString() must contain Go-quoted string representation \x00, \x07, \x1b, \xff
	debugStr := ik.DebugString()
	if !strings.Contains(debugStr, `\x00`) || !strings.Contains(debugStr, `\xff`) {
		t.Fatalf("InternalKey.DebugString() did not properly escape binary characters: %s", debugStr)
	}
	if !strings.Contains(debugStr, "seq=42") || !strings.Contains(debugStr, "op=PUT") {
		t.Fatalf("InternalKey.DebugString() missing metadata: %s", debugStr)
	}
}

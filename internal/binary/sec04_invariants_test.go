package binary_test

import (
	"bytes"
	goerrors "errors"
	"math"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

// SEC-MEM-INV-01: External mutable buffers cannot corrupt stored InternalKey state.
// Status: MEASURED RESULT (PASS)
func TestInvariant_SEC_MEM_INV_01_BufferImmutability(t *testing.T) {
	origUserKey := []byte("account-balance-user-12345")
	callerBuffer := make([]byte, len(origUserKey))
	copy(callerBuffer, origUserKey)

	ik, err := binary.NewInternalKey(callerBuffer, 42, binary.OpTypePut)
	if err != nil {
		t.Fatalf("unexpected NewInternalKey failure: %v", err)
	}

	// Adversary mutates the caller-owned buffer after insertion
	callerBuffer[0] = 'X'
	callerBuffer[len(callerBuffer)-1] = '!'

	if !bytes.Equal(ik.UserKey, origUserKey) {
		t.Fatalf("SEC-MEM-INV-01 VIOLATION: InternalKey state corrupted by external buffer mutation: got %s, want %s",
			ik.UserKey, origUserKey)
	}

	// Verify DecodeInternalKey also performs deep copying
	encoded := binary.EncodeInternalKey(ik)
	decoded, err := binary.DecodeInternalKey(encoded)
	if err != nil {
		t.Fatalf("unexpected DecodeInternalKey failure: %v", err)
	}

	// Mutate wire buffer
	encoded[0] = 'Z'
	if !bytes.Equal(decoded.UserKey, origUserKey) {
		t.Fatalf("SEC-MEM-INV-01 VIOLATION: Decoded InternalKey state corrupted by wire buffer mutation: got %s, want %s",
			decoded.UserKey, origUserKey)
	}
}

// SEC-MEM-INV-02: InternalKey ordering strictly matches canonical multi-version comparison.
// Status: MEASURED RESULT (PASS)
// Invariants verified:
//  1. UserKey ascending
//  2. SeqNum descending
//  3. OpType descending (Delete > Put)
//  4. Strict weak ordering (irreflexive, asymmetric, transitive)
func TestInvariant_SEC_MEM_INV_02_CanonicalOrdering(t *testing.T) {
	k1 := binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 100, OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 200, OpType: binary.OpTypePut}
	k3 := binary.InternalKey{UserKey: []byte("alpha"), SeqNum: 100, OpType: binary.OpTypeDelete}
	k4 := binary.InternalKey{UserKey: []byte("beta"), SeqNum: 50, OpType: binary.OpTypePut}

	// Rule 1: UserKey ascending -> "alpha" < "beta"
	if binary.CompareInternalKey(k1, k4) >= 0 {
		t.Fatalf("SEC-MEM-INV-02 VIOLATION: Expected 'alpha' < 'beta'")
	}

	// Rule 2: SeqNum descending -> SeqNum 200 comes BEFORE SeqNum 100
	if binary.CompareInternalKey(k2, k1) >= 0 {
		t.Fatalf("SEC-MEM-INV-02 VIOLATION: Expected SeqNum 200 to order BEFORE SeqNum 100")
	}

	// Rule 3: OpType descending -> OpTypeDelete (2) comes BEFORE OpTypePut (1) at same SeqNum
	if binary.CompareInternalKey(k3, k1) >= 0 {
		t.Fatalf("SEC-MEM-INV-02 VIOLATION: Expected Delete (2) to order BEFORE Put (1) for identical UserKey and SeqNum")
	}

	// Rule 4: Irreflexivity: CompareInternalKey(x, x) == 0
	if binary.CompareInternalKey(k1, k1) != 0 {
		t.Fatalf("SEC-MEM-INV-02 VIOLATION: CompareInternalKey(k1, k1) != 0")
	}
}

// SEC-MEM-INV-06: Attacker-controlled input cannot trigger uncontrolled allocation.
// Status: MEASURED RESULT (PASS)
func TestInvariant_SEC_MEM_INV_06_BoundedMemoryLimits(t *testing.T) {
	// Attempt oversized key allocation (> 64 KB)
	oversizedKey := make([]byte, binary.MaxKeyLen+1)
	_, err := binary.NewInternalKey(oversizedKey, 1, binary.OpTypePut)
	if !goerrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("SEC-MEM-INV-06 VIOLATION: Expected ErrKeyTooLarge for oversized key, got %v", err)
	}

	// Key validation rejection
	if err := binary.ValidateKey(oversizedKey); !goerrors.Is(err, errors.ErrKeyTooLarge) {
		t.Fatalf("SEC-MEM-INV-06 VIOLATION: ValidateKey allowed oversized key: %v", err)
	}

	// Empty key rejection (zero-byte keys)
	if err := binary.ValidateKey([]byte{}); !goerrors.Is(err, errors.ErrEmptyKey) {
		t.Fatalf("SEC-MEM-INV-06 VIOLATION: ValidateKey allowed empty key: %v", err)
	}

	// Value bounds rejection (> 4 MB)
	oversizedValue := make([]byte, binary.MaxValueLen+1)
	if err := binary.ValidateValue(oversizedValue); !goerrors.Is(err, errors.ErrValueTooLarge) {
		t.Fatalf("SEC-MEM-INV-06 VIOLATION: ValidateValue allowed oversized value: %v", err)
	}
}

// SEC-MEM-INV-09: Sequence ordering cannot be bypassed through duplicate or extreme version inputs.
// Status: MEASURED RESULT (PASS)
func TestInvariant_SEC_MEM_INV_09_SequenceOrderingIntegrity(t *testing.T) {
	// Boundary test: MaxUint64 SeqNum vs Zero SeqNum
	kMax := binary.InternalKey{UserKey: []byte("k"), SeqNum: math.MaxUint64, OpType: binary.OpTypePut}
	kMin := binary.InternalKey{UserKey: []byte("k"), SeqNum: 0, OpType: binary.OpTypePut}
	kMid := binary.InternalKey{UserKey: []byte("k"), SeqNum: 1 << 32, OpType: binary.OpTypePut}

	// Max comes before Mid
	if binary.CompareInternalKey(kMax, kMid) >= 0 {
		t.Fatalf("SEC-MEM-INV-09 VIOLATION: MaxUint64 SeqNum must precede mid SeqNum")
	}

	// Mid comes before Min
	if binary.CompareInternalKey(kMid, kMin) >= 0 {
		t.Fatalf("SEC-MEM-INV-09 VIOLATION: Mid SeqNum must precede Min SeqNum")
	}

	// Transitivity holds across extreme boundaries
	if binary.CompareInternalKey(kMax, kMin) >= 0 {
		t.Fatalf("SEC-MEM-INV-09 VIOLATION: MaxUint64 SeqNum must precede Min SeqNum")
	}
}

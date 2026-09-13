package version_test

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/version"
)

// h008AppendVarint appends v as a 7-bit varint.
func h008AppendVarint(t *testing.T, dst []byte, v uint64) []byte {
	t.Helper()
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint64(buf[:], v)
	return append(dst, buf[:n]...)
}

// h008CraftAddFileEdit hand-encodes a VersionEdit containing one AddFile
// entry with attacker-chosen scalars, bypassing the validating AddFile API —
// exactly the malicious-MANIFEST-bytes threat from the audit.
func h008CraftAddFileEdit(t *testing.T, level uint32, fileNum, fileSize, sSeq, lSeq uint64) []byte {
	t.Helper()
	sk, err := binary.NewInternalKey([]byte("a"), binary.SeqNum(10), binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey(smallest) failed: %v", err)
	}
	lk, err := binary.NewInternalKey([]byte("m"), binary.SeqNum(1), binary.OpTypePut)
	if err != nil {
		t.Fatalf("NewInternalKey(largest) failed: %v", err)
	}
	skEnc := binary.EncodeInternalKey(sk)
	lkEnc := binary.EncodeInternalKey(lk)

	var payload []byte
	payload = h008AppendVarint(t, payload, uint64(level))
	payload = h008AppendVarint(t, payload, fileNum)
	payload = h008AppendVarint(t, payload, fileSize)
	payload = h008AppendVarint(t, payload, sSeq)
	payload = h008AppendVarint(t, payload, lSeq)
	payload = h008AppendVarint(t, payload, uint64(len(skEnc)))
	payload = append(payload, skEnc...)
	payload = h008AppendVarint(t, payload, uint64(len(lkEnc)))
	payload = append(payload, lkEnc...)

	out := []byte{version.VersionEditFormatV1}
	out = h008AppendVarint(t, out, version.TagAddFile)
	out = h008AppendVarint(t, out, uint64(len(payload)))
	return append(out, payload...)
}

// TestINDH008_SwappedSeqNumsRejectedAtDecode proves the IND-H-008 verdict
// using the audit's exact attack input: SmallestSeqNum=100, LargestSeqNum=50.
// The decoder must reject the edit with ErrInvalidSeqNumRange — returning a
// nil edit — so corrupt metadata can never reach MANIFEST replay.
func TestINDH008_SwappedSeqNumsRejectedAtDecode(t *testing.T) {
	raw := h008CraftAddFileEdit(t, 0, 7, 4096, 100, 50)

	decoded, err := version.DecodeVersionEdit(raw)
	if err == nil {
		t.Fatalf("accepted logically impossible edit (smallest=100 > largest=50)")
	}
	if decoded != nil {
		t.Fatalf("expected nil edit on error, got %+v", decoded)
	}
	if !stdErrors.Is(err, errors.ErrInvalidSeqNumRange) {
		t.Fatalf("expected ErrInvalidSeqNumRange, got %T (%v)", err, err)
	}
}

// TestINDH008_ValidControlDecodes verifies the test is not vacuous: the same
// hand-encoded framing with a sane range (1..10) must decode successfully.
func TestINDH008_ValidControlDecodes(t *testing.T) {
	raw := h008CraftAddFileEdit(t, 0, 7, 4096, 1, 10)

	decoded, err := version.DecodeVersionEdit(raw)
	if err != nil {
		t.Fatalf("valid hand-encoded edit rejected: %v", err)
	}
	if decoded == nil {
		t.Fatalf("expected non-nil edit")
	}
	if got := decoded.NumAddedFiles(); got != 1 {
		t.Fatalf("NumAddedFiles = %d, want 1", got)
	}
}

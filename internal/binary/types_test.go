package binary_test

import (
	stdErrors "errors"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
)

func TestOpTypeValidAndValidate(t *testing.T) {
	// Exhaustively verify all 256 possible byte values.
	for b := 0; b <= 255; b++ {
		op := binary.OpType(b)
		err := op.Validate()

		switch op {
		case binary.OpTypePut:
			if !op.Valid() {
				t.Fatalf("expected OpTypePut (0x01) to be valid")
			}
			if err != nil {
				t.Fatalf("expected OpTypePut Validate() to return nil, got %v", err)
			}
		case binary.OpTypeDelete:
			if !op.Valid() {
				t.Fatalf("expected OpTypeDelete (0x02) to be valid")
			}
			if err != nil {
				t.Fatalf("expected OpTypeDelete Validate() to return nil, got %v", err)
			}
		default:
			if op.Valid() {
				t.Fatalf("expected byte 0x%02x to be invalid", b)
			}
			if err == nil {
				t.Fatalf("expected byte 0x%02x Validate() to return error", b)
			}
			if !stdErrors.Is(err, errors.ErrInvalidOpType) {
				t.Fatalf("expected error to match ErrInvalidOpType via errors.Is, got %v", err)
			}

			var typedErr *errors.InvalidOpTypeError
			if !stdErrors.As(err, &typedErr) {
				t.Fatalf("expected error to unpack into *InvalidOpTypeError via errors.As")
			}
			if typedErr.Op != byte(b) {
				t.Fatalf("expected typedErr.Op=0x%02x, got 0x%02x", b, typedErr.Op)
			}
		}
	}
}

func TestOpTypeZeroValue(t *testing.T) {
	var op binary.OpType

	if op != binary.OpTypeInvalid {
		t.Fatalf("expected zero-value OpType to equal OpTypeInvalid, got 0x%02x", byte(op))
	}
	if op != 0x00 {
		t.Fatalf("expected zero-value OpType to be 0x00, got 0x%02x", byte(op))
	}
	if op.Valid() {
		t.Fatalf("zero-value OpType must not be valid")
	}

	err := op.Validate()
	if err == nil {
		t.Fatalf("expected error for zero-value OpType Validate()")
	}
	if !stdErrors.Is(err, errors.ErrInvalidOpType) {
		t.Fatalf("expected ErrInvalidOpType, got %v", err)
	}

	str := op.String()
	if str != "UNKNOWN(0x00)" {
		t.Fatalf("expected 'UNKNOWN(0x00)', got %q", str)
	}
}

func TestOpTypeString(t *testing.T) {
	tests := []struct {
		op       binary.OpType
		expected string
	}{
		{binary.OpTypePut, "PUT"},
		{binary.OpTypeDelete, "DELETE"},
		{binary.OpTypeTombstone, "DELETE"},
		{binary.OpTypeInvalid, "UNKNOWN(0x00)"},
		{binary.OpType(0x03), "UNKNOWN(0x03)"},
		{binary.OpType(0x7F), "UNKNOWN(0x7f)"},
		{binary.OpType(0xFF), "UNKNOWN(0xff)"},
	}

	for _, tc := range tests {
		got := tc.op.String()
		if got != tc.expected {
			t.Errorf("op 0x%02x String() = %q, expected %q", byte(tc.op), got, tc.expected)
		}
	}
}

func TestParseOpType(t *testing.T) {
	// Valid cases
	op, err := binary.ParseOpType(0x01)
	if err != nil || op != binary.OpTypePut {
		t.Fatalf("ParseOpType(0x01) failed: got (%v, %v)", op, err)
	}

	op, err = binary.ParseOpType(0x02)
	if err != nil || op != binary.OpTypeDelete {
		t.Fatalf("ParseOpType(0x02) failed: got (%v, %v)", op, err)
	}

	// Invalid cases
	invalidBytes := []byte{0x00, 0x03, 0x04, 0x10, 0x7F, 0xFF}
	for _, b := range invalidBytes {
		op, err = binary.ParseOpType(b)
		if err == nil {
			t.Fatalf("ParseOpType(0x%02x) expected error, got op=%v", b, op)
		}
		if op != binary.OpTypeInvalid {
			t.Fatalf("ParseOpType(0x%02x) expected OpTypeInvalid, got op=%v", b, op)
		}
		if !stdErrors.Is(err, errors.ErrInvalidOpType) {
			t.Fatalf("ParseOpType(0x%02x) expected ErrInvalidOpType, got %v", b, err)
		}

		var typedErr *errors.InvalidOpTypeError
		if !stdErrors.As(err, &typedErr) || typedErr.Op != b {
			t.Fatalf("ParseOpType(0x%02x) failed to extract typed error or Op mismatch: %+v", b, typedErr)
		}
	}
}

func TestOpTypeAliasesAndConstants(t *testing.T) {
	if binary.OpTypePut != 0x01 {
		t.Errorf("OpTypePut must equal 0x01, got 0x%02x", byte(binary.OpTypePut))
	}
	if binary.OpTypeDelete != 0x02 {
		t.Errorf("OpTypeDelete must equal 0x02, got 0x%02x", byte(binary.OpTypeDelete))
	}
	if binary.OpTypeTombstone != binary.OpTypeDelete {
		t.Errorf("OpTypeTombstone must be identical to OpTypeDelete")
	}
	if binary.OpTypeInvalid != 0x00 {
		t.Errorf("OpTypeInvalid must equal 0x00, got 0x%02x", byte(binary.OpTypeInvalid))
	}
}

func TestSeqNumConstantsAndBounds(t *testing.T) {
	if binary.MinSeqNum != 0 {
		t.Errorf("MinSeqNum must be 0, got %d", binary.MinSeqNum)
	}
	if binary.MaxSeqNum != math.MaxUint64 {
		t.Errorf("MaxSeqNum must be %d, got %d", uint64(math.MaxUint64), binary.MaxSeqNum)
	}
	if uint64(binary.MaxSeqNum) != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("MaxSeqNum must be 0xFFFFFFFFFFFFFFFF, got 0x%x", uint64(binary.MaxSeqNum))
	}
}

func TestSeqNumNext(t *testing.T) {
	// Normal transitions
	tests := []struct {
		current  binary.SeqNum
		expected binary.SeqNum
	}{
		{0, 1},
		{1, 2},
		{42, 43},
		{1000, 1001},
		{1000000, 1000001},
		{binary.MaxSeqNum - 1, binary.MaxSeqNum},
	}

	for _, tc := range tests {
		next, err := tc.current.Next()
		if err != nil {
			t.Errorf("Next() on %d returned unexpected error: %v", tc.current, err)
		}
		if next != tc.expected {
			t.Errorf("Next() on %d = %d, expected %d", tc.current, next, tc.expected)
		}
	}

	// Boundary: MaxSeqNum overflow prevention
	next, err := binary.MaxSeqNum.Next()
	if err == nil {
		t.Fatalf("expected error on MaxSeqNum.Next(), got next=%d", next)
	}
	if next != binary.MaxSeqNum {
		t.Fatalf("expected next to remain MaxSeqNum on overflow error, got %d", next)
	}
	if !stdErrors.Is(err, errors.ErrSeqNumOverflow) {
		t.Fatalf("expected ErrSeqNumOverflow, got %v", err)
	}

	var typedErr *errors.SeqNumOverflowError
	if !stdErrors.As(err, &typedErr) {
		t.Fatalf("expected error to unpack into *SeqNumOverflowError")
	}
	if typedErr.Current != uint64(binary.MaxSeqNum) {
		t.Fatalf("expected typedErr.Current=%d, got %d", uint64(binary.MaxSeqNum), typedErr.Current)
	}
	if !strings.Contains(err.Error(), "18446744073709551615") {
		t.Fatalf("expected error string to contain MaxUint64 representation, got %q", err.Error())
	}
}

func TestSeqNumOrdering(t *testing.T) {
	s0 := binary.SeqNum(0)
	s1 := binary.SeqNum(1)
	s2 := binary.SeqNum(2)
	sMax := binary.MaxSeqNum

	// Strict total ordering verification
	if s0 >= s1 || s1 >= s2 || s2 >= sMax {
		t.Errorf("sequence numbers do not satisfy strict total ordering: s0=%d, s1=%d, s2=%d, sMax=%d", s0, s1, s2, sMax)
	}
	if sMax <= s2 || s2 <= s1 || s1 <= s0 {
		t.Errorf("sequence numbers do not satisfy descending order relation")
	}
	if s1 == s2 {
		t.Errorf("equality relation violation on SeqNum")
	}

	// Verify slice sorting descending (as required by MemTable & compaction K-way merge)
	nums := []binary.SeqNum{42, 5, 100, 0, binary.MaxSeqNum, 17}
	sort.Slice(nums, func(i, j int) bool {
		return nums[i] > nums[j] // descending order
	})

	expected := []binary.SeqNum{binary.MaxSeqNum, 100, 42, 17, 5, 0}
	for i, v := range nums {
		if v != expected[i] {
			t.Fatalf("sort descending mismatch at index %d: got %d, expected %d", i, v, expected[i])
		}
	}
}

func TestSeqNumString(t *testing.T) {
	tests := []struct {
		s        binary.SeqNum
		expected string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{1000, "1000"},
		{18446744073709551615, "18446744073709551615"},
		{binary.MaxSeqNum, "18446744073709551615"},
	}

	for _, tc := range tests {
		got := tc.s.String()
		if got != tc.expected {
			t.Errorf("SeqNum(%d).String() = %q, expected %q", tc.s, got, tc.expected)
		}
	}
}

// -----------------------------------------------------------------------------
// Fuzz Testing
// -----------------------------------------------------------------------------

func FuzzParseOpType(f *testing.F) {
	// Seed corpus with boundaries and valid values
	f.Add(byte(0x00))
	f.Add(byte(0x01))
	f.Add(byte(0x02))
	f.Add(byte(0x03))
	f.Add(byte(0x7F))
	f.Add(byte(0x80))
	f.Add(byte(0xFE))
	f.Add(byte(0xFF))

	f.Fuzz(func(t *testing.T, b byte) {
		op, err := binary.ParseOpType(b)

		switch b {
		case 0x01:
			if err != nil || op != binary.OpTypePut || !op.Valid() {
				t.Fatalf("expected valid OpTypePut for 0x01, got (%v, %v)", op, err)
			}
		case 0x02:
			if err != nil || op != binary.OpTypeDelete || !op.Valid() {
				t.Fatalf("expected valid OpTypeDelete for 0x02, got (%v, %v)", op, err)
			}
		default:
			if err == nil || op != binary.OpTypeInvalid || op.Valid() {
				t.Fatalf("expected invalid OpType for 0x%02x, got (%v, %v)", b, op, err)
			}
			if !stdErrors.Is(err, errors.ErrInvalidOpType) {
				t.Fatalf("expected ErrInvalidOpType for 0x%02x, got %v", b, err)
			}
		}

		// Ensure String() never panics on any byte
		str := op.String()
		if len(str) == 0 {
			t.Fatalf("String() must never return empty string")
		}
	})
}

func FuzzSeqNumNext(f *testing.F) {
	// Seed corpus with boundaries
	f.Add(uint64(0))
	f.Add(uint64(1))
	f.Add(uint64(42))
	f.Add(uint64(1000000))
	f.Add(uint64(math.MaxUint64 - 1))
	f.Add(uint64(math.MaxUint64))

	f.Fuzz(func(t *testing.T, val uint64) {
		s := binary.SeqNum(val)
		next, err := s.Next()

		if s == binary.MaxSeqNum {
			if err == nil {
				t.Fatalf("expected error on MaxSeqNum.Next()")
			}
			if next != binary.MaxSeqNum {
				t.Fatalf("expected next to remain MaxSeqNum on error")
			}
			if !stdErrors.Is(err, errors.ErrSeqNumOverflow) {
				t.Fatalf("expected ErrSeqNumOverflow, got %v", err)
			}
		} else {
			if err != nil {
				t.Fatalf("unexpected error for %d.Next(): %v", s, err)
			}
			if next != s+1 {
				t.Fatalf("expected %d + 1 = %d, got %d", s, s+1, next)
			}
			if next <= s {
				t.Fatalf("monotonicity violation: next %d <= current %d", next, s)
			}
		}

		// Ensure String() never panics
		str := s.String()
		if len(str) == 0 {
			t.Fatalf("SeqNum String() must not be empty")
		}
	})
}

// -----------------------------------------------------------------------------
// Zero-Allocation Benchmarks
// -----------------------------------------------------------------------------

func BenchmarkOpType_Valid(b *testing.B) {
	op := binary.OpTypePut
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if !op.Valid() {
			b.Fatal("unexpected invalid")
		}
	}
}

func BenchmarkOpType_Validate_Valid(b *testing.B) {
	op := binary.OpTypeDelete
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := op.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpType_String_Put(b *testing.B) {
	op := binary.OpTypePut
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = op.String()
	}
}

func BenchmarkParseOpType_Valid(b *testing.B) {
	raw := byte(0x01)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		op, err := binary.ParseOpType(raw)
		if err != nil || op != binary.OpTypePut {
			b.Fatal("unexpected parse failure")
		}
	}
}

func BenchmarkSeqNum_Next_Valid(b *testing.B) {
	s := binary.SeqNum(100)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		next, err := s.Next()
		if err != nil || next != 101 {
			b.Fatal("unexpected next failure")
		}
	}
}

func BenchmarkSeqNum_String(b *testing.B) {
	s := binary.SeqNum(1234567890123456)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = s.String()
	}
}

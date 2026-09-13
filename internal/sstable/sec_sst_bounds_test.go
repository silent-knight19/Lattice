package sstable

import (
	"testing"
)

// SEC-SST-01: block handles ending exactly at footer start pass; +1 byte fails;
// overflow wraps fail.
func TestSEC_SST01_BlockHandleBounds(t *testing.T) {
	f := Footer{
		MetaIndexHandle: BlockHandle{Offset: 0, Size: 8},
		IndexHandle:     BlockHandle{Offset: 8, Size: 8},
	}
	const fileSize int64 = 8 + 8 + FooterSize // 64
	if err := f.ValidateAgainstFileSize(fileSize); err != nil {
		t.Fatalf("boundary-exact footer rejected: %v", err)
	}
	bad := Footer{
		MetaIndexHandle: BlockHandle{Offset: 0, Size: 8},
		IndexHandle:     BlockHandle{Offset: 8, Size: 9}, // 8+9=17 > limit 16
	}
	if err := bad.ValidateAgainstFileSize(fileSize); err == nil {
		t.Fatal("overlapping footer boundary accepted")
	}
	// Overflow handle rejected by Validate.
	overflow := BlockHandle{Offset: ^uint64(0) - 5, Size: 10}
	if err := overflow.Validate(); err == nil {
		t.Fatal("overflowing block handle accepted")
	}
	// Zero size rejected.
	zero := BlockHandle{Offset: 0, Size: 0}
	if err := zero.Validate(); err == nil {
		t.Fatal("zero-size block handle accepted")
	}
}

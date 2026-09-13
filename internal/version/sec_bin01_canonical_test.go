package version

import (
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

// SEC-BIN-01: overlong varint encodings must be rejected on untrusted decode paths.
func TestSEC_BIN01_OverlongTagRejected(t *testing.T) {
	// Valid edit: version(0x01) + tag(0x01) + len(0x01) + payload(0x01) => NextFileNum=1
	valid := NewVersionEdit()
	valid.SetNextFileNum(1)
	enc := valid.Encode()
	if _, err := DecodeVersionEdit(enc); err != nil {
		t.Fatalf("valid canonical edit rejected: %v", err)
	}
	// Overlong tag: 1 encoded as [0x81 0x00] instead of [0x01]
	corrupted := []byte{0x01, 0x81, 0x00, 0x01, 0x01}
	if _, err := DecodeVersionEdit(corrupted); err == nil {
		t.Fatal("overlong TLV tag accepted, want rejection")
	}
	// Overlong payload: NextFileNum=1 as [0x81 0x00]
	corrupted2 := []byte{0x01, 0x01, 0x02, 0x81, 0x00}
	if _, err := DecodeVersionEdit(corrupted2); err == nil {
		t.Fatal("overlong scalar payload accepted, want rejection")
	}
	// Sanity: canonical decoder itself rejects overlong zero
	if _, _, err := binary.GetVarint64Canonical([]byte{0x80, 0x00}); err == nil {
		t.Fatal("GetVarint64Canonical accepted overlong zero")
	}
}

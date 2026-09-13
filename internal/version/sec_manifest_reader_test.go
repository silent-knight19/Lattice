package version

import (
	"os"
	"path/filepath"
	"testing"
)

// Production ManifestReader must verify CRC framing; single bit-flip fails closed,
// torn tail truncates via RecoverManifest, clean prefix reads.
func TestSEC_ManifestReader_CRCProtection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MANIFEST-000001")
	w, err := CreateManifestWriter(path)
	if err != nil {
		t.Fatalf("CreateManifestWriter: %v", err)
	}
	for i := uint64(1); i <= 3; i++ {
		e := NewVersionEdit()
		e.SetNextFileNum(i)
		if err := w.LogEdit(*e); err != nil {
			t.Fatalf("LogEdit %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	edits, err := ReadManifest(path)
	if err != nil {
		t.Fatalf("ReadManifest clean: %v", err)
	}
	if len(edits) != 3 {
		t.Fatalf("got %d edits, want 3", len(edits))
	}

	// Single bit-flip in payload must fail closed.
	raw, _ := os.ReadFile(path)
	raw[10] ^= 0x01
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("bit-flipped manifest accepted, want CRC failure")
	}

	// Torn tail truncates via RecoverManifest.
	dir2 := t.TempDir()
	path2 := filepath.Join(dir2, "MANIFEST-000001")
	w2, _ := CreateManifestWriter(path2)
	e := NewVersionEdit()
	e.SetNextFileNum(9)
	_ = w2.LogEdit(*e)
	_ = w2.Close()
	f, _ := os.OpenFile(path2, os.O_WRONLY|os.O_APPEND, 0600)
	_, _ = f.Write([]byte{0x00, 0x01})
	_ = f.Close()
	res, err := RecoverManifest(path2)
	if err != nil {
		t.Fatalf("RecoverManifest torn tail: %v", err)
	}
	if !res.Truncated || res.ValidRecords != 1 {
		t.Fatalf("want truncated with 1 valid, got %+v", res)
	}
}

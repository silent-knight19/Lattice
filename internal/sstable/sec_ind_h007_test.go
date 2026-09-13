package sstable_test

import (
	stdErrors "errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// openNoPanic opens path and fails the test on panic instead of crashing the suite.
func openNoPanic(t *testing.T, path string) (r *sstable.TableReader, err error) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("NewTableReader panicked on %s (IND-H-007): %v", path, rec)
		}
	}()
	r, err = sstable.NewTableReader(path)
	return r, err
}

// TestINDH007_TruncatedFilesFailClosed proves the IND-H-007 verdict: files
// smaller than the 48-byte footer — including empty, 15-byte, and 47-byte
// inputs — must be rejected with a corruption error, never panic with a slice
// bounds failure.
func TestINDH007_TruncatedFilesFailClosed(t *testing.T) {
	sizes := []int{0, 1, 15, 16, 47}
	for _, size := range sizes {
		t.Run(filepath.Join("size", itoa(size)), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "tiny.sst")
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(0xA5 + i)
			}
			if err := os.WriteFile(path, payload, 0600); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}

			r, err := openNoPanic(t, path)
			if err == nil {
				_ = r.Close()
				t.Fatalf("size %d: accepted file smaller than footer", size)
			}
			if !stdErrors.Is(err, errors.ErrInvalidFooterSize) {
				t.Errorf("size %d: want ErrInvalidFooterSize, got %T (%v)", size, err, err)
			}
		})
	}
}

// TestINDH007_FullSizeBadMagicFailClosed verifies the adjacent boundary: a
// full 48-byte file with a bad magic number is also rejected cleanly.
func TestINDH007_FullSizeBadMagicFailClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "badmagic.sst")
	payload := make([]byte, sstable.FooterSize)
	for i := range payload {
		payload[i] = byte(i + 1)
	}
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	r, err := openNoPanic(t, path)
	if err == nil {
		_ = r.Close()
		t.Fatalf("accepted 48-byte file with bad magic")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

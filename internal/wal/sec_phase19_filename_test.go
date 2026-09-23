package wal_test

import (
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/wal"
)

// TestPhase19_WAL_InvalidFileNameRejection verifies that OpenWriter, CreateWriter,
// and OpenReader reject invalid filenames containing traversal tokens or invalid characters.
func TestPhase19_WAL_InvalidFileNameRejection(t *testing.T) {
	dir := t.TempDir()

	invalidNames := []string{
		"..",
		".",
		"bad file with space.wal",
		"bad\x00null.wal",
		"bad;injection.wal",
	}

	for _, name := range invalidNames {
		p := filepath.Join(dir, name)

		if _, err := wal.CreateWriter(p); err == nil {
			t.Errorf("CreateWriter expected error for %q, got nil", name)
		}
		if _, err := wal.OpenWriter(p); err == nil {
			t.Errorf("OpenWriter expected error for %q, got nil", name)
		}
		if _, err := wal.OpenReader(p); err == nil {
			t.Errorf("OpenReader expected error for %q, got nil", name)
		}
	}
}

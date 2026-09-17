package main

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzInspectSSTable fuzz tests InspectSSTable against arbitrary mutated byte sequences
// to ensure zero panics, bounded execution, and safe failure termination.
func FuzzInspectSSTable(f *testing.F) {
	// Seed 1: Empty byte slice
	f.Add([]byte{})

	// Seed 2: Too small for footer
	f.Add([]byte("short_bytes"))

	// Seed 3: Exactly 48 zero bytes
	f.Add(make([]byte, 48))

	// Seed 4: Realistic footer candidate
	seedFooter := make([]byte, 64)
	copy(seedFooter[16:64], make([]byte, 48))
	f.Add(seedFooter)

	f.Fuzz(func(t *testing.T, data []byte) {
		tempDir := t.TempDir()
		testPath := filepath.Join(tempDir, "fuzz.sst")

		if err := os.WriteFile(testPath, data, 0600); err != nil {
			t.Fatalf("failed to write fuzz file: %v", err)
		}

		var report ForensicReport
		// InspectSSTable must not panic under any input bytes
		_ = InspectSSTable(testPath, &report)
	})
}

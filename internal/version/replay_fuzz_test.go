package version

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzManifestReplayResourceBudget fuzzes ReplayManifest over arbitrary binary file contents
// and varying budgets (maxBytes, maxRecords, maxFiles) to ensure no panics, memory bounds
// are respected, and no unreferenced Version escapes.
func FuzzManifestReplayResourceBudget(f *testing.F) {
	// Seed with typical byte fragments and limit parameters
	f.Add([]byte{}, int64(1024), 10, 10)
	f.Add([]byte("MANIFEST-000001\n"), int64(64), 1, 1)
	f.Add([]byte("\x00\x00\x00\x00\x00\x00\x00\x00"), int64(512), 5, 5)

	f.Fuzz(func(t *testing.T, data []byte, maxBytes int64, maxRecords int, maxFiles int) {
		if maxBytes <= 0 || maxRecords <= 0 || maxFiles <= 0 {
			return
		}
		if maxBytes > 10*1024*1024 || maxRecords > 1000 || maxFiles > 1000 {
			return
		}

		restore := SetManifestReplayLimitsForTesting(maxBytes, maxRecords, maxFiles)
		defer restore()

		dir := t.TempDir()
		manifestPath := filepath.Join(dir, "MANIFEST-000001")
		if err := os.WriteFile(manifestPath, data, 0600); err != nil {
			return
		}

		file, err := os.Open(manifestPath)
		if err != nil {
			return
		}
		defer func() { _ = file.Close() }()

		disc := &DiscoveredManifest{
			Dir:         dir,
			ManifestNum: 1,
			Path:        manifestPath,
			File:        file,
			FileSize:    int64(len(data)),
		}

		res, replayErr := ReplayManifest(disc)
		if replayErr == nil && res != nil {
			// If replay succeeded, invariants must hold
			if res.Version == nil {
				t.Fatal("ReplayResult.Version is nil on success")
			}
			if res.ValidRecords > maxRecords {
				t.Fatalf("ValidRecords %d exceeds maxRecords %d", res.ValidRecords, maxRecords)
			}
			res.Version.Unref()
		}
	})
}

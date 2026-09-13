package engine_test

import (
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/engine"
)

// FuzzCleanerFilenameGrammar verifies that IsOrphanStagingFile never panics
// on arbitrary input strings, and strictly adheres to allowlist rules:
// - Must start with ".tmp_"
// - Must contain ".sst_"
// - Must NOT contain path separators or null bytes
// - Must NEVER match persistent files ("CURRENT", "MANIFEST-*", "*.sst", "wal")
func FuzzCleanerFilenameGrammar(f *testing.F) {
	// Seed corpus with valid, borderline, and malicious filenames
	seeds := []string{
		".tmp_000001.sst_123456",
		".tmp_000042.sst_abcdef",
		"CURRENT",
		"CURRENT.tmp",
		"MANIFEST-000001",
		"000001.sst",
		"wal",
		".tmp_",
		".tmp_.sst_",
		".tmp_../etc/passwd.sst_1",
		".tmp_foo\x00bar.sst_1",
		"../../.tmp_000001.sst_123",
		".tmp_000001.sst_123/victim",
		strings.Repeat("a", 1000),
	}

	for _, s := range seeds {
		f.Add(s)
	}

	persistentPrefixes := []string{"CURRENT", "MANIFEST", "wal"}

	f.Fuzz(func(t *testing.T, name string) {
		isOrphan := engine.IsOrphanStagingFile(name)

		if isOrphan {
			// Invariant 1: Must start with ".tmp_"
			if !strings.HasPrefix(name, ".tmp_") {
				t.Fatalf("IsOrphanStagingFile(%q) = true but lacks .tmp_ prefix", name)
			}
			// Invariant 2: Must contain ".sst_"
			if !strings.Contains(name, ".sst_") {
				t.Fatalf("IsOrphanStagingFile(%q) = true but lacks .sst_ infix", name)
			}
			// Invariant 3: No path separators or null bytes
			if strings.ContainsAny(name, "/\\\x00") {
				t.Fatalf("IsOrphanStagingFile(%q) = true but contains illegal path characters", name)
			}
			// Invariant 4: Must never match persistent prefixes
			for _, p := range persistentPrefixes {
				if strings.HasPrefix(name, p) {
					t.Fatalf("IsOrphanStagingFile(%q) = true matched persistent file prefix %q", name, p)
				}
			}
		}
	})
}

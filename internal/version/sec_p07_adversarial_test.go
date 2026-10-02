package version

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
)

func makeAdvTestKey(userKey string, seq uint64) []byte {
	ik, err := binary.NewInternalKey([]byte(userKey), binary.SeqNum(seq), binary.OpTypePut)
	if err != nil {
		panic(err)
	}
	return binary.EncodeInternalKey(ik)
}

// TestCURRENT_AdversarialParsingMatrix covers the complete negative and boundary test matrix
// for CURRENT file discovery and validation.
func TestCURRENT_AdversarialParsingMatrix(t *testing.T) {
	testCases := []struct {
		name        string
		content     string
		mustFail    bool
		description string
	}{
		{
			name:        "EmptyFile",
			content:     "",
			mustFail:    true,
			description: "empty CURRENT file must fail closed",
		},
		{
			name:        "MissingNewline",
			content:     "MANIFEST-000001",
			mustFail:    true,
			description: "CURRENT without trailing newline must fail closed",
		},
		{
			name:        "ExtraNewline",
			content:     "MANIFEST-000001\n\n",
			mustFail:    true,
			description: "CURRENT with multiple newlines must fail closed",
		},
		{
			name:        "CRLFLineEnding",
			content:     "MANIFEST-000001\r\n",
			mustFail:    true,
			description: "CURRENT with CRLF line ending must fail closed",
		},
		{
			name:        "LeadingWhitespace",
			content:     " MANIFEST-000001\n",
			mustFail:    true,
			description: "CURRENT with leading space must fail closed",
		},
		{
			name:        "TrailingWhitespace",
			content:     "MANIFEST-000001 \n",
			mustFail:    true,
			description: "CURRENT with trailing space before newline must fail closed",
		},
		{
			name:        "InvalidPrefixUnderscore",
			content:     "MANIFEST_000001\n",
			mustFail:    true,
			description: "CURRENT with MANIFEST_ prefix must fail closed",
		},
		{
			name:        "InvalidPrefixWrongName",
			content:     "CURRENT-000001\n",
			mustFail:    true,
			description: "CURRENT with wrong prefix must fail closed",
		},
		{
			name:        "NonDigitChars",
			content:     "MANIFEST-00001a\n",
			mustFail:    true,
			description: "CURRENT with non-digit suffix must fail closed",
		},
		{
			name:        "NegativeValue",
			content:     "MANIFEST--1\n",
			mustFail:    true,
			description: "CURRENT with negative value must fail closed",
		},
		{
			name:        "PlusSign",
			content:     "MANIFEST-+1\n",
			mustFail:    true,
			description: "CURRENT with plus sign must fail closed",
		},
		{
			name:        "ZeroManifestID",
			content:     "MANIFEST-000000\n",
			mustFail:    true,
			description: "CURRENT with manifest ID 0 must fail closed",
		},
		{
			name:        "SuperfluousLeadingZeros",
			content:     "MANIFEST-0000001\n",
			mustFail:    true,
			description: "CURRENT with >6 digits must fail closed",
		},
		{
			name:        "IntegerOverflow",
			content:     "MANIFEST-99999999999999999999999999999999\n",
			mustFail:    true,
			description: "CURRENT with integer overflow must fail closed",
		},
		{
			name:        "OversizedContent",
			content:     string(make([]byte, 1024)),
			mustFail:    true,
			description: "CURRENT exceeding maximum byte limit must fail closed",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			currentPath := filepath.Join(dir, "CURRENT")
			if err := os.WriteFile(currentPath, []byte(tc.content), 0644); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}

			_, err := ReadCurrentManifest(dir)
			if tc.mustFail && err == nil {
				t.Fatalf("expected failure for %s, but succeeded", tc.description)
			}
		})
	}
}

// TestCURRENT_SymlinkAndDirectoryRejected verifies that symlinks, directories,
// and special device files are strictly rejected as CURRENT.
func TestCURRENT_SymlinkAndDirectoryRejected(t *testing.T) {
	t.Run("CURRENT is a directory", func(t *testing.T) {
		dir := t.TempDir()
		currentPath := filepath.Join(dir, "CURRENT")
		if err := os.Mkdir(currentPath, 0755); err != nil {
			t.Fatalf("Mkdir failed: %v", err)
		}

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatal("expected failure when CURRENT is a directory, got nil")
		}
	})

	t.Run("CURRENT is a symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real_current")
		if err := os.WriteFile(target, []byte("MANIFEST-000001\n"), 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		currentPath := filepath.Join(dir, "CURRENT")
		if err := os.Symlink(target, currentPath); err != nil {
			t.Fatalf("Symlink failed: %v", err)
		}

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatal("expected failure when CURRENT is a symlink, got nil")
		}
	})

	t.Run("CURRENT is a FIFO", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("FIFOs not supported on Windows")
		}
		dir := t.TempDir()
		currentPath := filepath.Join(dir, "CURRENT")
		if err := syscall.Mkfifo(currentPath, 0644); err != nil {
			t.Skipf("Mkfifo not supported on filesystem: %v", err)
		}

		_, err := ReadCurrentManifest(dir)
		if err == nil {
			t.Fatal("expected failure when CURRENT is a FIFO, got nil")
		}
	})
}

// TestActiveManifest_SymlinkAndSpecialFilesRejected verifies that the MANIFEST file
// referenced by CURRENT cannot be a symlink, directory, or special device.
func TestActiveManifest_SymlinkAndSpecialFilesRejected(t *testing.T) {
	t.Run("MANIFEST is a symlink", func(t *testing.T) {
		dir := t.TempDir()
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest failed: %v", err)
		}

		target := filepath.Join(dir, "real_manifest")
		if err := os.WriteFile(target, []byte{0x01}, 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		manifestPath := filepath.Join(dir, "MANIFEST-000001")
		if err := os.Symlink(target, manifestPath); err != nil {
			t.Fatalf("Symlink failed: %v", err)
		}

		_, err := DiscoverActiveManifest(dir)
		if err == nil {
			t.Fatal("expected DiscoverActiveManifest to reject symlink MANIFEST, got nil")
		}
	})

	t.Run("MANIFEST is a directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := SetCurrentManifest(dir, 1); err != nil {
			t.Fatalf("SetCurrentManifest failed: %v", err)
		}

		manifestPath := filepath.Join(dir, "MANIFEST-000001")
		if err := os.Mkdir(manifestPath, 0755); err != nil {
			t.Fatalf("Mkdir failed: %v", err)
		}

		_, err := DiscoverActiveManifest(dir)
		if err == nil {
			t.Fatal("expected DiscoverActiveManifest to reject directory MANIFEST, got nil")
		}
	})
}

// TestReplayManifest_ActiveSSTableValidationMatrix verifies all physical checks on active SSTables:
// missing SSTable, wrong physical size, SSTable symlink, SSTable directory.
func TestReplayManifest_ActiveSSTableValidationMatrix(t *testing.T) {
	t.Run("Missing active SSTable fails closed", func(t *testing.T) {
		dir := t.TempDir()

		edit := NewVersionEdit()
		edit.SetNextFileNum(2)
		_ = edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       512,
			SmallestKey:    makeAdvTestKey("a", 1),
			LargestKey:     makeAdvTestKey("z", 1),
			SmallestSeqNum: 1,
			LargestSeqNum:  1,
		})

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{edit})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatal("expected ReplayManifest to fail closed when active SSTable is missing")
		}
	})

	t.Run("SSTable size mismatch fails closed", func(t *testing.T) {
		dir := t.TempDir()

		// Physical file is 256 bytes, but manifest claims 512 bytes
		sstPath := filepath.Join(dir, "000001.sst")
		if err := os.WriteFile(sstPath, make([]byte, 256), 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}

		edit := NewVersionEdit()
		edit.SetNextFileNum(2)
		_ = edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       512,
			SmallestKey:    makeAdvTestKey("a", 1),
			LargestKey:     makeAdvTestKey("z", 1),
			SmallestSeqNum: 1,
			LargestSeqNum:  1,
		})

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{edit})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatal("expected ReplayManifest to fail closed on SSTable size mismatch")
		}
	})

	t.Run("SSTable is a symlink fails closed", func(t *testing.T) {
		dir := t.TempDir()

		target := filepath.Join(dir, "real_table.sst")
		if err := os.WriteFile(target, make([]byte, 512), 0644); err != nil {
			t.Fatalf("WriteFile failed: %v", err)
		}
		sstPath := filepath.Join(dir, "000001.sst")
		if err := os.Symlink(target, sstPath); err != nil {
			t.Fatalf("Symlink failed: %v", err)
		}

		edit := NewVersionEdit()
		edit.SetNextFileNum(2)
		_ = edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       512,
			SmallestKey:    makeAdvTestKey("a", 1),
			LargestKey:     makeAdvTestKey("z", 1),
			SmallestSeqNum: 1,
			LargestSeqNum:  1,
		})

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{edit})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatal("expected ReplayManifest to fail closed when SSTable is a symlink")
		}
	})

	t.Run("SSTable is a directory fails closed", func(t *testing.T) {
		dir := t.TempDir()

		sstPath := filepath.Join(dir, "000001.sst")
		if err := os.Mkdir(sstPath, 0755); err != nil {
			t.Fatalf("Mkdir failed: %v", err)
		}

		edit := NewVersionEdit()
		edit.SetNextFileNum(2)
		_ = edit.AddFile(0, FileMetadata{
			FileNum:        1,
			FileSize:       512,
			SmallestKey:    makeAdvTestKey("a", 1),
			LargestKey:     makeAdvTestKey("z", 1),
			SmallestSeqNum: 1,
			LargestSeqNum:  1,
		})

		disc := setupTestManifest(t, dir, 1, []*VersionEdit{edit})
		defer func() { _ = disc.Close() }()

		_, err := ReplayManifest(disc)
		if err == nil {
			t.Fatal("expected ReplayManifest to fail closed when SSTable is a directory")
		}
	})
}

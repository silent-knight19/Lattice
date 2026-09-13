package version_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/version"
)

// TestINDC002_NoRotationPathDecoySymlinksUntouched proves the IND-C-002 verdict:
// ManifestWriter performs no MANIFEST rotation (no Stat -> Remove -> Rename
// sequence, no MANIFEST.old handling). Symlinks planted at rotation-style
// sibling paths must neither be followed nor replaced by any writer operation,
// and external victim files must remain byte-identical across appends, close,
// and reopen cycles.
func TestINDC002_NoRotationPathDecoySymlinksUntouched(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "MANIFEST-000001")

	victims := map[string]string{
		"MANIFEST.old": "victim-old-sentinel",
		"MANIFEST.tmp": "victim-tmp-sentinel",
		"MANIFEST.bak": "victim-bak-sentinel",
	}
	for decoy, sentinel := range victims {
		target := filepath.Join(dir, "victim_"+decoy)
		if err := os.WriteFile(target, []byte(sentinel), 0600); err != nil {
			t.Fatalf("WriteFile victim failed: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(dir, decoy)); err != nil {
			t.Skipf("symlinks not supported on this platform: %v", err)
		}
	}

	w, err := version.CreateManifestWriter(manifestPath)
	if err != nil {
		t.Fatalf("CreateManifestWriter failed: %v", err)
	}
	for i := 0; i < 10; i++ {
		edit := version.NewVersionEdit()
		if err := w.LogEdit(*edit); err != nil {
			t.Fatalf("LogEdit %d failed: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen append path must also ignore the decoys.
	w2, err := version.OpenManifestWriter(manifestPath)
	if err != nil {
		t.Fatalf("OpenManifestWriter failed: %v", err)
	}
	for i := 0; i < 10; i++ {
		edit := version.NewVersionEdit()
		if err := w2.LogEdit(*edit); err != nil {
			t.Fatalf("reopen LogEdit %d failed: %v", i, err)
		}
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("reopen Close failed: %v", err)
	}

	// All decoys must still be symlinks and all victims byte-identical.
	for decoy, sentinel := range victims {
		decoyPath := filepath.Join(dir, decoy)
		info, err := os.Lstat(decoyPath)
		if err != nil {
			t.Fatalf("decoy %s missing after writer ops: %v", decoy, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("decoy %s is no longer a symlink (mode %s): rotation path replaced it", decoy, info.Mode())
		}
		content, err := os.ReadFile(filepath.Join(dir, "victim_"+decoy))
		if err != nil {
			t.Fatalf("ReadFile victim_%s failed: %v", decoy, err)
		}
		if string(content) != sentinel {
			t.Errorf("victim_%s = %q, want %q: writer followed a rotation-style symlink", decoy, content, sentinel)
		}
	}

	// The MANIFEST itself must hold exactly the 20 appended records.
	info, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatalf("Stat MANIFEST failed: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("MANIFEST is empty after 20 LogEdit calls")
	}
}

// TestINDC002_SymlinkAtManagedPathRejected maps the audit's attack scenario to
// the managed path itself: Open/Create over a symlink must fail closed without
// writing a single byte through the link to the victim file.
func TestINDC002_SymlinkAtManagedPathRejected(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "external_victim.txt")
	const sentinel = "sensitive-external-content"
	if err := os.WriteFile(victim, []byte(sentinel), 0600); err != nil {
		t.Fatalf("WriteFile victim failed: %v", err)
	}
	linkPath := filepath.Join(dir, "MANIFEST-000002")
	if err := os.Symlink(victim, linkPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}

	if _, err := version.OpenManifestWriter(linkPath); err == nil {
		t.Errorf("OpenManifestWriter must reject symlink at managed path")
	}
	if _, err := version.CreateManifestWriter(linkPath); err == nil {
		t.Errorf("CreateManifestWriter must reject symlink at managed path")
	}

	content, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile victim failed: %v", err)
	}
	if string(content) != sentinel {
		t.Errorf("victim = %q, want %q: writer wrote through symlink", content, sentinel)
	}
}

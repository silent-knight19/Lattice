package web

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestHasAssetsRejectsPlaceholder(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":             {Data: []byte("<html>")},
		"assets/placeholder.txt": {Data: []byte("x")},
	}
	if HasAssets(fsys) {
		t.Error("placeholder-only build must NOT report a real build")
	}
}

func TestHasAssetsAcceptsRealBundle(t *testing.T) {
	for _, name := range []string{"assets/index-abc123.js", "assets/index-def456.CSS"} {
		fsys := fstest.MapFS{name: {Data: []byte("x")}}
		if !HasAssets(fsys) {
			t.Errorf("%s must report a real build", name)
		}
	}
}

// TestHasAssetsEmbeddedDistIsRealBuild asserts the committed bundle is a REAL build.
//
// This inverts an earlier assertion of this file, which expected the placeholder. That was
// correct while web/dist shipped only index.html plus assets/placeholder.txt, and it is what
// kept the startup warning honest. Now that FE-1 produces a real Vite bundle, the invariant
// worth protecting is the opposite one: that the embedded bundle stays a real build, so the
// daemon never silently serves the placeholder to an operator who believes they have a UI.
//
// The placeholder-detection behaviour itself is still covered by
// TestHasAssetsRejectsPlaceholder above.
func TestHasAssetsEmbeddedDistIsRealBuild(t *testing.T) {
	sub, err := Sub()
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if !HasAssets(sub) {
		entries, _ := fs.ReadDir(sub, "assets")
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("committed dist must be a real build; assets dir holds %v", names)
	}

	// A real bundle must actually carry an entry point, not just an assets directory.
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		t.Errorf("committed dist must contain index.html: %v", err)
	}
	var _ fs.FS = sub
}

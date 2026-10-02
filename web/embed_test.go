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

func TestHasAssetsEmbeddedDistIsPlaceholder(t *testing.T) {
	sub, err := Sub()
	if err != nil {
		t.Fatalf("Sub: %v", err)
	}
	if HasAssets(sub) {
		t.Error("committed dist is a placeholder; startup warning must remain visible")
	}
	var _ fs.FS = sub
}

// Package web embeds the compiled Lattice Console single-page application.
//
// The embed directive lives here, in the parent of dist/, because go:embed can only
// reference files in its own package directory or below it. Putting the embed in
// internal/admin would require referencing ../../web/dist, which the toolchain rejects.
//
// The committed web/dist/index.html is a minimal PLACEHOLDER. It exists so `go build ./...`
// succeeds for anyone who has not run `npm ci && npm run build`. The real bundle overwrites
// it. See ui-console-tasks.md SEC-11.5 and FE-1.
package web

import (
	"embed"
	"io/fs"
	"path/filepath"
	"strings"
)

// Dist holds the compiled console assets.
//
// `all:` is used so that files whose names begin with `_` or `.` (Vite emits some) are
// included rather than silently dropped by the default pattern.
//
//go:embed all:dist
var Dist embed.FS

// Sub returns the dist/ subtree rooted at itself.
//
// Returning a sub-FS (rather than passing paths with a "dist/" prefix around) is what makes
// the fs.ValidPath containment rules apply to paths exactly as a caller wrote them, so
// "../../etc/passwd" is rejected by the standard library rather than by ad-hoc string checks
// in the handler.
func Sub() (fs.FS, error) {
	return fs.Sub(Dist, "dist")
}

// HasAssets reports whether a real (non-placeholder) console bundle appears to be embedded.
//
// A real Vite build emits hashed JavaScript and CSS under dist/assets/. The committed
// placeholder contains only dist/assets/placeholder.txt, so merely finding that the assets
// directory exists would wrongly report "real" and silently suppress the startup warning
// telling the operator to build the UI. The check therefore looks for the file TYPES a real
// bundle contains rather than for the directory alone.
//
// It is used only to emit a startup warning, never to change access control, so a false
// positive costs a confusing message but never a permission.
func HasAssets(fsys fs.FS) bool {
	if fsys == nil {
		return false
	}
	entries, err := fs.ReadDir(fsys, "assets")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".js", ".mjs", ".css":
			return true
		}
	}
	return false
}

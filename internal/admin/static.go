package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/silent-knight19/lattice/web"
)

// This file implements SEC-11: safe static asset serving for the console SPA.
//
// The central design decision is that NO filesystem path derived from user input is ever
// opened. Assets are served from an embed.FS, and the only paths that can be opened are ones
// that already satisfy fs.ValidPath — a standard-library contract that rejects ".", "..",
// leading/trailing slashes, and backslashes.
//
// That makes traversal structurally impossible rather than filtered: there is no code path
// that concatenates a request path onto a root directory, which is the mistake that produces
// most path-traversal vulnerabilities.

// indexFile is the SPA entry document.
const indexFile = "index.html"

// immutableMaxAge is applied to content-hashed build output under assets/.
//
// Vite emits names like app-9f2c1a.js, so a changed file always has a changed URL. A year of
// immutable caching is therefore safe and removes revalidation traffic entirely.
const immutableMaxAge = 31536000 * time.Second

// noCacheControl is used for index.html and for anything not under assets/.
const noCacheControl = "no-store"

// StaticHandler serves the embedded console assets.
type StaticHandler struct {
	fsys fs.FS
	// indexETag is computed once at construction rather than per request.
	indexETag string
	// hasAssets reports whether a real build (not the placeholder) is embedded.
	hasAssets bool
}

// NewStaticHandler builds a handler over the embedded dist/ tree.
//
// It returns an error only when the embedded tree is unusable, which would be a build-time
// packaging fault rather than a runtime condition.
func NewStaticHandler() (*StaticHandler, error) {
	fsys, err := web.Sub()
	if err != nil {
		return nil, err
	}
	h := &StaticHandler{fsys: fsys, hasAssets: web.HasAssets(fsys)}
	if sum, err := hashFile(fsys, indexFile); err == nil {
		h.indexETag = sum
	}
	return h, nil
}

// NewStaticHandlerFS builds a handler over an arbitrary fs.FS, for tests.
func NewStaticHandlerFS(fsys fs.FS) *StaticHandler {
	// hasAssets must be populated here too, exactly as NewStaticHandler does. Omitting it
	// left every FS-constructed handler reporting HasRealBuild()==false regardless of its
	// contents, so the real-build branch of the startup warning could not be exercised.
	h := &StaticHandler{fsys: fsys, hasAssets: web.HasAssets(fsys)}
	if sum, err := hashFile(fsys, indexFile); err == nil {
		h.indexETag = sum
	}
	return h
}

// HasRealBuild reports whether a real console build is embedded.
func (h *StaticHandler) HasRealBuild() bool { return h != nil && h.hasAssets }

// ServeHTTP implements asset serving with SPA fallback.
func (h *StaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeMethodNotAllowed(w)
		return
	}
	if h == nil || h.fsys == nil {
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, messageFor(CodeUnavailable))
		return
	}

	// The API surface is never served from the SPA: a mistyped API call must produce a JSON
	// 404, not an HTML document that would confuse both a human and a client library.
	if isAPIPath(r.URL.Path) {
		writeNotFound(w)
		return
	}

	// RawPath is used when it differs from Path, so percent-encoded traversal such as
	// %2e%2e%2f is validated too rather than being silently decoded into Path.
	raw := r.URL.Path
	if r.URL.RawPath != "" {
		raw = r.URL.RawPath
	}
	name, ok := safeAssetPath(raw)
	if !ok {
		// Reject rather than normalise: a request containing traversal is not a request for
		// a missing file, and answering 404 hides nothing useful from the caller.
		writeNotFound(w)
		return
	}

	if serveFile(w, r, h.fsys, name) {
		return
	}

	// SPA fallback: any other non-API path renders the shell so client-side routing works
	// on a hard refresh or a deep link.
	if serveFile(w, r, h.fsys, indexFile) {
		return
	}
	writeNotFound(w)
}

// safeAssetPath validates and canonicalises a request path into an embed-relative name.
//
// It returns ok=false for anything that must not be resolved. The checks are deliberately
// redundant: fs.ValidPath is the authority, and the earlier checks exist so the intent is
// explicit and so a future refactor that changes how name is used cannot silently widen it.
func safeAssetPath(raw string) (string, bool) {
	if raw == "" {
		return indexFile, true
	}
	// Null byte: truncation attacks against C-based consumers, and never legitimate.
	if strings.ContainsRune(raw, 0) {
		return "", false
	}
	// Backslash: not a separator in URL paths, but treated as one by some Windows filesystems.
	if strings.Contains(raw, `\`) {
		return "", false
	}
	// Any traversal element, before cleaning, so ".." is rejected rather than resolved.
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", false
		}
	}
	name := strings.TrimPrefix(path.Clean("/"+raw), "/")
	if name == "" || name == "." {
		return indexFile, true
	}
	// The authoritative check: fs.ValidPath rejects ".", "..", leading or trailing slashes,
	// and any path that is not unrooted-and-clean.
	if !fs.ValidPath(name) {
		return "", false
	}
	return name, true
}

// serveFile writes one embedded file, reporting whether it existed.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}

	// A symlink cannot appear inside an embed.FS, but a custom fs.FS in tests could supply
	// one. ModeSymlink is checked explicitly so the handler is safe for any fs.FS.
	if info.Mode()&fs.ModeSymlink != 0 {
		return false
	}

	etag := etagFor(fsys, name)
	w.Header().Set("Content-Type", contentTypeByExtension(name))
	w.Header().Set("ETag", etag)

	// Conditional request: the browser revalidates with If-None-Match.
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}

	if isImmutableAsset(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// index.html must never be cached, or a redeploy is invisible until a hard refresh.
		w.Header().Set("Cache-Control", noCacheControl)
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.WriteHeader(http.StatusOK)
		return true
	}

	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
	return true
}

// isImmutableAsset reports whether a name is content-hashed build output.
func isImmutableAsset(name string) bool {
	return strings.HasPrefix(name, "assets/") || strings.HasPrefix(name, "static/")
}

// etagMatches reports whether an If-None-Match header matches the current ETag.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// etagFor computes a strong ETag from file content.
//
// Hashing content rather than mtime matters because embed.FS has a fixed zero mtime, so a
// mtime-based ETag would never change across rebuilds and a redeploy would be invisible.
func etagFor(fsys fs.FS, name string) string {
	if sum, err := hashFile(fsys, name); err == nil {
		return sum
	}
	return `""`
}

// hashFile returns a quoted hex SHA-256 ETag for a file's contents.
func hashFile(fsys fs.FS, name string) (string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`, nil
}

// ErrStaticNotFound signals a missing asset; kept for callers that need the distinction.
var ErrStaticNotFound = errors.New("admin: static asset not found")

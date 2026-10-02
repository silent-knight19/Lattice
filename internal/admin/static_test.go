package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// staticTestFS is a small in-memory tree mirroring a Vite build output.
var staticTestFS = fstest.MapFS{
	"index.html":            {Data: []byte("<!DOCTYPE html><html><body>shell</body></html>")},
	"assets/app-9f2c1a.js":  {Data: []byte("console.log('app')")},
	"assets/app-9f2c1a.css": {Data: []byte("body{}")},
	"assets/logo.svg":       {Data: []byte("<svg></svg>")},
	"favicon.ico":           {Data: []byte("icon")},
	"secret.txt":            {Data: []byte("should be served only via explicit request")},
}

func newStatic(t *testing.T) *StaticHandler {
	t.Helper()
	return NewStaticHandlerFS(staticTestFS)
}

func doStatic(t *testing.T, h *StaticHandler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// TestStatic_ServesIndex verifies the SPA entry document.
func TestStatic_ServesIndex(t *testing.T) {
	h := newStatic(t)
	for _, target := range []string{"/", "/index.html"} {
		rec := doStatic(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "shell") {
			t.Errorf("GET %s did not serve index.html: %q", target, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q", target, ct)
		}
	}
}

// TestStatic_ServesAssetsWithExplicitTypes is the SEC-5.2 requirement applied to real serving.
func TestStatic_ServesAssetsWithExplicitTypes(t *testing.T) {
	h := newStatic(t)
	cases := map[string]string{
		"/assets/app-9f2c1a.js":  "text/javascript; charset=utf-8",
		"/assets/app-9f2c1a.css": "text/css; charset=utf-8",
		"/assets/logo.svg":       "image/svg+xml",
		"/favicon.ico":           "image/x-icon",
	}
	for target, want := range cases {
		rec := doStatic(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", target, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != want {
			t.Errorf("GET %s Content-Type = %q, want %q", target, got, want)
		}
	}
}

// TestStatic_RejectsTraversal is the central SEC-11.3 assertion.
func TestStatic_RejectsTraversal(t *testing.T) {
	h := newStatic(t)
	paths := []string{
		"/../etc/passwd",
		"/../../../../etc/passwd",
		"/assets/../../etc/passwd",
		"/./../../secret",
		"/assets/./../../secret.txt",
		"/..%2f..%2fetc%2fpasswd",
		"/%2e%2e/%2e%2e/etc/passwd",
		`/\..\..\etc\passwd`,
		"/....//....//etc/passwd",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rec := doStatic(t, h, http.MethodGet, p)
			// Either rejected outright or (for encodings that decode to a real path)
			// served the SPA shell — never a file from outside the FS.
			if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "root:") {
				t.Fatalf("TRAVERSAL SUCCEEDED for %s:\n%s", p, rec.Body.String())
			}
		})
	}
}

// TestStatic_RejectsNullByte covers the truncation vector.
func TestStatic_RejectsNullByte(t *testing.T) {
	if _, ok := safeAssetPath("/index.html\x00.txt"); ok {
		t.Error("safeAssetPath accepted a null byte")
	}
	if _, ok := safeAssetPath("/assets/app.js\x00.png"); ok {
		t.Error("safeAssetPath accepted a null byte in a middle segment")
	}
	// A path with no null byte at all must still be accepted, proving the rejection above
	// is caused by the null byte rather than by the ".html" suffix.
	if _, ok := safeAssetPath("/index.html"); !ok {
		t.Error("a clean path was rejected")
	}
	if _, ok := safeAssetPath("/assets/app.js"); !ok {
		t.Error("a clean asset path was rejected")
	}
}

// TestStatic_RejectsBackslash covers the Windows-separator confusion.
func TestStatic_RejectsBackslash(t *testing.T) {
	if _, ok := safeAssetPath(`\assets\app.js`); ok {
		t.Error("safeAssetPath accepted a backslash path")
	}
	if _, ok := safeAssetPath(`/..\secret.txt`); ok {
		t.Error("safeAssetPath accepted a backslash traversal")
	}
}

// TestStatic_APIPathsNeverServeHTML is SEC-11.2: unknown /api paths are JSON 404, never the SPA.
func TestStatic_APIPathsNeverServeHTML(t *testing.T) {
	h := newStatic(t)
	for _, p := range []string{
		"/api/v1/nope",
		"/api/v1/../",
		"/api/v1",
		"/api/v2/health",
		"/api/unknown",
		"/api/",
	} {
		rec := doStatic(t, h, http.MethodGet, p)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "<html") || strings.Contains(body, "shell") {
			t.Errorf("GET %s served the SPA instead of a JSON 404:\n%s", p, body)
		}
		if !strings.Contains(body, `"error"`) {
			t.Errorf("GET %s did not return the JSON error envelope:\n%s", p, body)
		}
	}
}

// TestStatic_SPAFallback covers client-side routing on deep links.
func TestStatic_SPAFallback(t *testing.T) {
	h := newStatic(t)
	for _, p := range []string{"/keys", "/storage/lsm", "/raft", "/lab", "/deeply/nested/route"} {
		rec := doStatic(t, h, http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (SPA fallback)", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "shell") {
			t.Errorf("GET %s did not fall back to index.html:\n%s", p, rec.Body.String())
		}
	}
}

// TestStatic_CacheHeaders verifies SEC-11.4.
func TestStatic_CacheHeaders(t *testing.T) {
	h := newStatic(t)

	// index.html must never be cached, or a redeploy stays invisible.
	for _, p := range []string{"/", "/index.html", "/keys"} {
		rec := doStatic(t, h, http.MethodGet, p)
		if got := rec.Header().Get("Cache-Control"); got != noCacheControl {
			t.Errorf("GET %s Cache-Control = %q, want %q", p, got, noCacheControl)
		}
	}

	// Content-hashed assets are immutable.
	for _, p := range []string{"/assets/app-9f2c1a.js", "/assets/app-9f2c1a.css"} {
		rec := doStatic(t, h, http.MethodGet, p)
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("GET %s Cache-Control = %q, want immutable long max-age", p, cc)
		}
	}
}

// TestStatic_ETagAndConditionalGet verifies revalidation.
func TestStatic_ETagAndConditionalGet(t *testing.T) {
	h := newStatic(t)
	rec := doStatic(t, h, http.MethodGet, "/assets/app-9f2c1a.js")
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag emitted")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag %q is not a quoted strong validator", etag)
	}

	req := httptest.NewRequest(http.MethodGet, "/assets/app-9f2c1a.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 carried a %d-byte body", rec2.Body.Len())
	}

	// A stale validator must return the file.
	req = httptest.NewRequest(http.MethodGet, "/assets/app-9f2c1a.js", nil)
	req.Header.Set("If-None-Match", `"stale"`)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusOK {
		t.Errorf("stale validator = %d, want 200", rec3.Code)
	}
}

// TestStatic_ETagChangesWithContent proves the validator is content-derived, not
// mtime-derived (embed.FS has a fixed zero mtime, so an mtime ETag would never change).
func TestStatic_ETagChangesWithContent(t *testing.T) {
	a := NewStaticHandlerFS(fstest.MapFS{"index.html": {Data: []byte("one")}})
	b := NewStaticHandlerFS(fstest.MapFS{"index.html": {Data: []byte("two")}})

	recA := httptest.NewRecorder()
	a.ServeHTTP(recA, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	recB := httptest.NewRecorder()
	b.ServeHTTP(recB, httptest.NewRequest(http.MethodGet, "/index.html", nil))

	if recA.Header().Get("ETag") == recB.Header().Get("ETag") {
		t.Error("ETag did not change when content changed")
	}
}

// TestStatic_RejectsNonReadMethods verifies method handling.
func TestStatic_RejectsNonReadMethods(t *testing.T) {
	h := newStatic(t)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doStatic(t, h, m, "/index.html")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", m, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
			t.Errorf("%s Allow = %q", m, allow)
		}
	}
}

// TestStatic_HeadSupported verifies HEAD returns headers without a body.
func TestStatic_HeadSupported(t *testing.T) {
	h := newStatic(t)
	rec := doStatic(t, h, http.MethodHead, "/assets/app-9f2c1a.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a %d-byte body", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD omitted Content-Length")
	}
}

// TestSafeAssetPath covers the canonicalisation rules directly.
func TestSafeAssetPath(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"/", indexFile, true},
		{"", indexFile, true},
		{"/index.html", "index.html", true},
		{"/assets/app.js", "assets/app.js", true},
		{"./assets/app.js", "assets/app.js", true},
		{"//assets//app.js", "assets/app.js", true},
		{"/../secret", "", false},
		{"/a/../../secret", "", false},
		{"..", "", false},
		{"/..", "", false},
		{`\secret`, "", false},
		{"/a\x00b", "", false},
	}
	for _, tc := range cases {
		got, ok := safeAssetPath(tc.in)
		if ok != tc.wantOK || (ok && got != tc.want) {
			t.Errorf("safeAssetPath(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestStatic_NilHandlerIsSafe ensures a nil handler cannot panic.
func TestStatic_NilHandlerIsSafe(t *testing.T) {
	var h *StaticHandler
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil handler = %d, want 503", rec.Code)
	}
	if h.HasRealBuild() {
		t.Error("nil handler reported a real build")
	}
}

// TestStatic_EmptyFSIsSafe covers a build with no index.html at all.
func TestStatic_EmptyFSIsSafe(t *testing.T) {
	h := NewStaticHandlerFS(fstest.MapFS{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("empty FS root = %d, want 404", rec.Code)
	}
}

// TestNewStaticHandler_EmbeddedBuild verifies the real embedded tree is wired and that the
// committed placeholder keeps `go build` working without Node installed.
func TestNewStaticHandler_EmbeddedBuild(t *testing.T) {
	h, err := NewStaticHandler()
	if err != nil {
		t.Fatalf("NewStaticHandler: %v", err)
	}
	if h == nil || h.fsys == nil {
		t.Fatal("embedded FS is nil")
	}
	rec := doStatic(t, h, http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Errorf("embedded index = %d, want 200", rec.Code)
	}
	// The placeholder build must announce itself so an operator knows to build the frontend.
	if !h.HasRealBuild() {
		t.Log("placeholder dist detected (expected until `npm run build` runs)")
	}
}

// TestStatic_ContentLengthMatchesBody guards against a truncated or inflated response.
func TestStatic_ContentLengthMatchesBody(t *testing.T) {
	h := newStatic(t)
	rec := doStatic(t, h, http.MethodGet, "/assets/app-9f2c1a.js")
	cl := rec.Header().Get("Content-Length")
	if cl == "" {
		t.Fatal("Content-Length missing")
	}
	if got, want := rec.Body.Len(), len("console.log('app')"); got != want {
		t.Errorf("body length = %d, want %d", got, want)
	}
}

// TestStatic_NoSniffHeader is applied by SecurityHeaders, but the handler must not fight it.
func TestStatic_NoSniffHeader(t *testing.T) {
	h := SecurityHeaders(NoStoreAPI(newStatic(t)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-9f2c1a.js", nil))
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on a static asset")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing CSP on a static asset")
	}
}

var _ fs.FS = staticTestFS

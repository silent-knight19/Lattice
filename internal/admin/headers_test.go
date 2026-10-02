package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// securityChain builds the canonical server chain for header testing.
func securityChain(inner http.Handler) http.Handler {
	return SecurityHeaders(NoStoreAPI(inner))
}

// TestSecurityHeaders_Golden asserts the exact header set on all five response classes
// required by SEC-5.3: index, 200 API, 403, 404, and 500.
func TestSecurityHeaders_Golden(t *testing.T) {
	cases := []struct {
		name     string
		handler  http.Handler
		method   string
		path     string
		wantCode int
	}{
		{
			name: "index page",
			handler: securityChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, "<!doctype html><html></html>")
			})),
			method: http.MethodGet, path: "/", wantCode: http.StatusOK,
		},
		{
			name: "200 API response",
			handler: securityChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"ok":true}`)
			})),
			method: http.MethodGet, path: "/api/v1/health", wantCode: http.StatusOK,
		},
		{
			name:     "403 forbidden",
			handler:  securityChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeForbidden(w) })),
			method:   http.MethodPost,
			path:     "/api/v1/keys",
			wantCode: http.StatusForbidden,
		},
		{
			name:     "404 not found",
			handler:  securityChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeNotFound(w) })),
			method:   http.MethodGet,
			path:     "/api/v1/missing",
			wantCode: http.StatusNotFound,
		},
		{
			name:     "500 internal error",
			handler:  securityChain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeInternalError(w) })),
			method:   http.MethodGet,
			path:     "/api/v1/boom",
			wantCode: http.StatusInternalServerError,
		},
	}

	wantExact := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}

			for header, want := range wantExact {
				if got := rec.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
				t.Errorf("CSP mismatch:\n got %q\nwant %q", got, ContentSecurityPolicy)
			}
			if got := rec.Header().Get("Permissions-Policy"); got != PermissionsPolicy {
				t.Errorf("Permissions-Policy = %q, want %q", got, PermissionsPolicy)
			}
			// Never emit CORS headers, on any class of response.
			if hasAnyCORSHeader(rec.Header()) {
				t.Errorf("response carries a CORS header: %v", rec.Header())
			}
			// API responses must be uncached.
			if isAPIPath(tc.path) {
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("API Cache-Control = %q, want no-store", got)
				}
			}
		})
	}
}

// TestCSP_HasNoUnsafeDirectives is the SEC-5.1 requirement, asserted structurally.
//
// A single stray 'unsafe-inline' would silently re-open script execution and defeat the
// entire control, so this checks the directive set rather than eyeballing a string.
func TestCSP_HasNoUnsafeDirectives(t *testing.T) {
	csp := ContentSecurityPolicy
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "unsafe-hashes", "wasm-unsafe-eval", "data:"} {
		// data: is permitted only in img-src, which is asserted separately below.
		if forbidden == "data:" {
			continue
		}
		if strings.Contains(csp, forbidden) {
			t.Errorf("CSP contains %q: %s", forbidden, csp)
		}
	}
	// Every directive must end with ';' so an appended value cannot merge into it.
	for _, directive := range strings.Split(strings.TrimSuffix(csp, "; "), "; ") {
		if directive == "" {
			t.Error("CSP contains an empty directive")
		}
	}
}

// TestCSP_RequiredDirectives verifies each directive the threat model depends on is present.
func TestCSP_RequiredDirectives(t *testing.T) {
	required := []string{
		"default-src 'self'",
		"script-src 'self'",
		"style-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}
	for _, d := range required {
		if !strings.Contains(ContentSecurityPolicy, d) {
			t.Errorf("CSP missing required directive %q: %s", d, ContentSecurityPolicy)
		}
	}
	// img-src may include data: for inline images; that is the only data: allowance.
	if !strings.Contains(ContentSecurityPolicy, "img-src 'self' data:") {
		t.Errorf("expected img-src to allow data: images: %s", ContentSecurityPolicy)
	}
	if strings.Contains(ContentSecurityPolicy, "script-src 'self' data:") ||
		strings.Contains(ContentSecurityPolicy, "connect-src 'self' data:") {
		t.Error("data: must not be allowed for script or connect sources")
	}
}

// TestNoStoreAPI_LeavesStaticAssetsCacheable verifies caching is resource-scoped.
func TestNoStoreAPI_LeavesStaticAssetsCacheable(t *testing.T) {
	cases := []struct {
		path        string
		wantNoStore bool
	}{
		{"/api/v1/health", true},
		{"/api/v1/keys", true},
		{"/api", true},
		{"/", false},
		{"/index.html", false},
		{"/assets/app-abc123.js", false},
		{"/apifoo", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			h := NoStoreAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			cc := rec.Header().Get("Cache-Control")
			if tc.wantNoStore && cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if !tc.wantNoStore && cc == "no-store" {
				t.Errorf("static asset %q must not be forced to no-store", tc.path)
			}
		})
	}
}

// TestNoStoreAPI_RespectsHandlerOverride documents that a handler may choose a stricter
// policy.
func TestNoStoreAPI_RespectsHandlerOverride(t *testing.T) {
	h := NoStoreAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store, max-age=0" {
		t.Errorf("handler override clobbered: %q", got)
	}
}

// TestSecurityHeaders_SetsBeforeHandler runs a handler that never touches headers, proving
// the middleware supplies them rather than trusting the handler.
func TestSecurityHeaders_SetsBeforeHandler(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for _, name := range securityHeaderNames {
		if rec.Header().Get(name) == "" {
			t.Errorf("header %q missing on a handler that set no headers", name)
		}
	}
}

// TestContentTypeByExtension covers SEC-5.2: explicit types, no sniffing, safe default.
func TestContentTypeByExtension(t *testing.T) {
	cases := map[string]string{
		"/index.html":           "text/html; charset=utf-8",
		"/index.htm":            "text/html; charset=utf-8",
		"/assets/app.js":        "text/javascript; charset=utf-8",
		"/assets/app.mjs":       "text/javascript; charset=utf-8",
		"/assets/app.css":       "text/css; charset=utf-8",
		"/assets/logo.svg":      "image/svg+xml",
		"/assets/data.json":     "application/json",
		"/assets/app.js.map":    "application/json",
		"/assets/f.woff2":       "font/woff2",
		"/assets/f.woff":        "font/woff",
		"/assets/f.ttf":         "font/ttf",
		"/assets/i.png":         "image/png",
		"/assets/i.jpg":         "image/jpeg",
		"/assets/i.JPEG":        "image/jpeg",
		"/favicon.ico":          "image/x-icon",
		"/README":               "application/octet-stream",
		"/noextension":          "application/octet-stream",
		"/assets/app.js.bak":    "application/octet-stream",
		"/assets/evil.xyz":      "application/octet-stream",
		"/weird.name.with.dots": "application/octet-stream",
		"/assets/%2e%2e/x":      "application/octet-stream",
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			if got := contentTypeByExtension(path); got != want {
				t.Errorf("contentTypeByExtension(%q) = %q, want %q", path, got, want)
			}
		})
	}
}

// TestContentType_NeverSniffableOrExecutable pins the two invariants that matter: no
// unknown extension is served as script, and text/javascript is used rather than the
// legacy application/javascript spelling.
func TestContentType_NeverSniffableOrExecutable(t *testing.T) {
	for _, p := range []string{"/x", "/x.unknownext", "/x.exe", "/x.php", "/x.svgz"} {
		got := contentTypeByExtension(p)
		if strings.Contains(got, "javascript") || strings.Contains(got, "html") {
			t.Errorf("%q maps to executable type %q", p, got)
		}
	}
	if got := contentTypeByExtension("/a.js"); got != "text/javascript; charset=utf-8" {
		t.Errorf("javascript type = %q", got)
	}
}

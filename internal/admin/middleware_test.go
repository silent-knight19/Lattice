package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newGuarded returns a handler behind OriginGuard plus a counter that records whether the
// wrapped (inner) handler actually executed.
//
// The counter is the whole point. Per SEC-0 finding F12, a browser reports the same opaque
// network error whether a cross-origin request was blocked or delivered, so client-side
// observation cannot distinguish "attack prevented" from "attack executed". These tests
// therefore assert on whether the inner handler ran.
func newGuarded(allow *OriginAllowlist) (http.Handler, *atomic.Int64) {
	var executed atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executed.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	return OriginGuard(allow)(inner), &executed
}

func do(t *testing.T, h http.Handler, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/crash", strings.NewReader("crash"))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestOriginGuard_AttackCases is the core SEC-1 assertion set.
func TestOriginGuard_AttackCases(t *testing.T) {
	const self = "http://127.0.0.1:7070"
	allow := NewOriginAllowlist(self)

	tests := []struct {
		name        string
		origin      string
		wantStatus  int
		wantBlocked bool
	}{
		// The SEC-0 reproduction: a foreign origin must not reach the handler.
		{"SEC-0 attacker origin", "http://127.0.0.1:9099", http.StatusForbidden, true},
		{"arbitrary attacker site", "http://evil.com", http.StatusForbidden, true},
		{"attacker with our host as prefix", "http://127.0.0.1:7070.evil.com", http.StatusForbidden, true},
		{"attacker with our host as suffix", "http://evil-127.0.0.1:7070", http.StatusForbidden, true},
		{"scheme upgrade attempt", "https://127.0.0.1:7070", http.StatusForbidden, true},
		{"port shifted", "http://127.0.0.1:9099", http.StatusForbidden, true},
		{"opaque null origin", "null", http.StatusForbidden, true},
		{"malformed origin", "http://", http.StatusForbidden, true},
		{"origin with path", "http://127.0.0.1:7070/api", http.StatusForbidden, true},
		{"userinfo smuggling", "http://127.0.0.1:7070@evil.com", http.StatusForbidden, true},

		// Legitimate traffic.
		{"exact self origin", self, http.StatusOK, false},
		{"case-insensitive self origin", "HTTP://127.0.0.1:7070", http.StatusOK, false},
		{"absent origin (curl / CLI / same-origin navigation)", "", http.StatusOK, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, executed := newGuarded(allow)
			rec := do(t, h, http.MethodPost, tc.origin)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantBlocked && executed.Load() != 0 {
				t.Errorf("SECURITY: handler executed %d time(s) for forbidden origin %q", executed.Load(), tc.origin)
			}
			if !tc.wantBlocked && executed.Load() != 1 {
				t.Errorf("handler executed %d time(s), want 1", executed.Load())
			}
		})
	}
}

// TestOriginGuard_AllMethodsAreGuarded ensures the guard is not accidentally applied only to
// a subset of verbs. A future mutating verb must not slip past.
func TestOriginGuard_AllMethodsAreGuarded(t *testing.T) {
	allow := NewOriginAllowlist("http://127.0.0.1:7070")
	methods := []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions,
	}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			h, executed := newGuarded(allow)
			rec := do(t, h, m, "http://evil.com")
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: status = %d, want 403", m, rec.Code)
			}
			if executed.Load() != 0 {
				t.Errorf("%s: SECURITY handler executed for forbidden origin", m)
			}
		})
	}
}

// TestOriginGuard_RejectionLeaksNothing verifies the 403 body does not disclose the
// allowlist or echo the attacker's input.
func TestOriginGuard_RejectionLeaksNothing(t *testing.T) {
	allow := NewOriginAllowlist("http://127.0.0.1:7070")
	h, _ := newGuarded(allow)
	rec := do(t, h, http.MethodPost, "http://evil.com")

	body := rec.Body.String()
	for _, secret := range []string{"127.0.0.1", "7070", "evil.com", "localhost", "allow"} {
		if strings.Contains(body, secret) {
			t.Errorf("403 body leaks %q: %s", secret, body)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// TestNoCORSHeadersOnAnyResponse is the SEC-1.3 hard invariant: the admin API must never
// emit Access-Control-* on ANY response, because doing so would re-enable the browser to
// deliver AND expose cross-origin requests.
//
// This walks the known response paths rather than a single route, so a future contributor
// adding CORS anywhere is caught by CI.
func TestNoCORSHeadersOnAnyResponse(t *testing.T) {
	allow := NewOriginAllowlist("http://127.0.0.1:7070")
	h, _ := newGuarded(allow)

	cases := []struct {
		name   string
		origin string
		method string
	}{
		{"allowed origin", "http://127.0.0.1:7070", http.MethodGet},
		{"forbidden origin", "http://evil.com", http.MethodPost},
		{"no origin", "", http.MethodPost},
		{"preflight OPTIONS", "http://evil.com", http.MethodOptions},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, h, tc.method, tc.origin)
			if hasAnyCORSHeader(rec.Header()) {
				t.Errorf("response carries a CORS header: %v", rec.Header())
			}
		})
	}
}

// TestChainOrdering verifies Chain applies middlewares outermost-first, which the documented
// security ordering depends on.
func TestChainOrdering(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})
	Chain(terminal, mw("outer"), mw("inner")).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	want := []string{"outer", "inner", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestChainNilMiddleware verifies a nil middleware is skipped rather than panicking, so
// optional layers can be wired unconditionally.
func TestChainNilMiddleware(t *testing.T) {
	called := false
	terminal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	Chain(terminal, nil, nil).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Error("terminal handler not reached past nil middlewares")
	}
}

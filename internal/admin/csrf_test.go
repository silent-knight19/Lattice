package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func mustToken(t *testing.T) *CSRFToken {
	t.Helper()
	tok, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken: %v", err)
	}
	return tok
}

// TestCSRFToken_Generation verifies the token is high-entropy and URL-safe.
func TestCSRFToken_Generation(t *testing.T) {
	tok := mustToken(t)
	v := tok.Value()
	if v == "" {
		t.Fatal("token value is empty")
	}
	// 32 bytes base64url unpadded => 43 characters.
	if len(v) != 43 {
		t.Errorf("token length = %d, want 43", len(v))
	}
	if strings.ContainsAny(v, "+/=") {
		t.Errorf("token %q is not URL-safe unpadded base64", v)
	}
	if tok.IssuedAt().IsZero() {
		t.Error("IssuedAt is zero")
	}
}

// TestCSRFToken_UniquePerBoot is the property that makes revocation unnecessary: every
// start yields a different token, so a captured old token is useless.
func TestCSRFToken_UniquePerBoot(t *testing.T) {
	const n = 256
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		v := mustToken(t).Value()
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate token generated: %q", v)
		}
		seen[v] = struct{}{}
	}
}

// TestCSRFToken_Valid covers acceptance and rejection of candidate values.
func TestCSRFToken_Valid(t *testing.T) {
	tok := mustToken(t)
	good := tok.Value()

	if !tok.Valid(good) {
		t.Fatal("correct token rejected")
	}
	for _, bad := range []string{
		"", " ", "wrong", good + "x", "x" + good, good[:len(good)-1],
		strings.ToUpper(good), strings.ToLower(good), good + " ", " " + good,
		strings.Repeat("A", 43), strings.Repeat("A", 1000),
	} {
		if tok.Valid(bad) {
			t.Errorf("invalid candidate %q accepted", bad)
		}
	}
	var nilTok *CSRFToken
	if nilTok.Valid(good) {
		t.Error("nil token accepted a candidate")
	}
}

// TestCSRFToken_StaleTokenFromPreviousBootRejected verifies subtask 3.3: a token issued by
// a previous daemon run does not work against the current one.
func TestCSRFToken_StaleTokenFromPreviousBootRejected(t *testing.T) {
	old := mustToken(t)
	oldValue := old.Value()
	newer := mustToken(t)

	if !old.Valid(oldValue) {
		t.Fatal("precondition: old token should validate against itself")
	}
	if newer.Valid(oldValue) {
		t.Error("SECURITY: token from a previous boot was accepted by the current boot")
	}
	if old.Value() == newer.Value() {
		t.Error("two boots produced the same token")
	}
}

// TestCSRFGuard_RequiresTokenAndConfirm is the core SEC-3 assertion table.
func TestCSRFGuard_RequiresTokenAndConfirm(t *testing.T) {
	tok := mustToken(t)
	good := tok.Value()

	tests := []struct {
		name    string
		method  string
		token   string
		confirm string
		wantRun bool
	}{
		// Safe methods need no token.
		{"GET without token", http.MethodGet, "", "", true},
		{"HEAD without token", http.MethodHead, "", "", true},
		{"OPTIONS without token", http.MethodOptions, "", "", true},

		// Unsafe methods require BOTH the token and explicit confirmation.
		{"POST token+confirm", http.MethodPost, good, "confirm", true},
		{"PUT token+confirm", http.MethodPut, good, "confirm", true},
		{"DELETE token+confirm", http.MethodDelete, good, "confirm", true},
		{"PATCH token+confirm", http.MethodPatch, good, "confirm", true},

		{"POST missing both", http.MethodPost, "", "", false},
		{"POST token without confirm", http.MethodPost, good, "", false},
		{"POST confirm without token", http.MethodPost, "", "confirm", false},
		{"POST wrong token", http.MethodPost, "wrong", "confirm", false},
		{"POST wrong confirm value", http.MethodPost, good, "yes", false},
		{"POST confirm wrong case", http.MethodPost, good, "CONFIRM", false},
		{"DELETE missing both", http.MethodDelete, "", "", false},

		// TRACE is not treated as safe.
		{"TRACE without token", http.MethodTrace, "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ran atomic.Int64
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran.Add(1)
				w.WriteHeader(http.StatusOK)
			})
			h := CSRFGuard(tok)(inner)

			req := httptest.NewRequest(tc.method, "/api/v1/keys", strings.NewReader("{}"))
			if tc.token != "" {
				req.Header.Set(CSRFHeader, tc.token)
			}
			if tc.confirm != "" {
				req.Header.Set(AdminConfirmHeader, tc.confirm)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if tc.wantRun {
				if ran.Load() != 1 {
					t.Errorf("handler ran %d times, want 1 (status %d)", ran.Load(), rec.Code)
				}
			} else {
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403", rec.Code)
				}
				if ran.Load() != 0 {
					t.Errorf("SECURITY: handler ran %d times on a forged request", ran.Load())
				}
			}
		})
	}
}

// TestCSRFGuard_RejectionLeaksNothing verifies a forged request's response reveals
// neither the expected token nor the supplied one.
func TestCSRFGuard_RejectionLeaksNothing(t *testing.T) {
	tok := mustToken(t)
	good := tok.Value()
	const supplied = "ATTACKER-SUPPLIED-TOKEN-VALUE"

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CSRFGuard(tok)(inner)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/keys", strings.NewReader("{}"))
	req.Header.Set(CSRFHeader, supplied)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, good) {
		t.Error("403 body leaks the expected token")
	}
	if strings.Contains(body, supplied) {
		t.Error("403 body echoes the supplied token")
	}
	if strings.Contains(body, csrfTokenBytes2str(tok.digest)) {
		t.Error("403 body leaks token digest material")
	}
	if hasAnyCORSHeader(rec.Header()) {
		t.Error("403 response carries a CORS header")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing no-store on rejection")
	}
}

// csrfTokenBytes2str renders the digest as hex for leak assertions.
func csrfTokenBytes2str(d [32]byte) string {
	const hexDigits = "0123456789abcdef"
	var b strings.Builder
	for _, c := range d {
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

// TestCSRFGuard_RejectionIsIndistinguishable verifies a wrong token and a missing token
// produce byte-identical responses, so the guard is not an oracle.
func TestCSRFGuard_RejectionIsIndistinguishable(t *testing.T) {
	tok := mustToken(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CSRFGuard(tok)(inner)

	render := func(setToken, confirm string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/keys", strings.NewReader("{}"))
		if setToken != "" {
			req.Header.Set(CSRFHeader, setToken)
		}
		if confirm != "" {
			req.Header.Set(AdminConfirmHeader, confirm)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	c1, b1 := render("", "")
	c2, b2 := render("wrong-token", "confirm")
	if c1 != c2 || b1 != b2 {
		t.Errorf("responses differ: (%d,%q) vs (%d,%q)", c1, b1, c2, b2)
	}

	// And the origin rejection must be indistinguishable from the CSRF rejection.
	//
	// The request MUST carry a disallowed Origin. An absent Origin is deliberately
	// permitted by SEC-1 (curl, the CLI, and health probes send none), so a request
	// without one would pass the guard and return 200 rather than being rejected.
	rec := httptest.NewRecorder()
	originReq := httptest.NewRequest(http.MethodPost, "/api/v1/keys", strings.NewReader("{}"))
	originReq.Header.Set("Origin", "http://evil.com")
	OriginGuard(NewOriginAllowlist("http://127.0.0.1:7070"))(inner).ServeHTTP(rec, originReq)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("precondition: origin rejection returned %d, want 403", rec.Code)
	}
	if rec.Code != c1 || rec.Body.String() != b1 {
		t.Errorf("origin and CSRF rejections are distinguishable; the API is an oracle:\n origin: (%d,%q)\n csrf:  (%d,%q)",
			rec.Code, rec.Body.String(), c1, b1)
	}
}

// TestSessionHandler covers the only endpoint that discloses the token.
func TestSessionHandler(t *testing.T) {
	tok := mustToken(t)
	h := SessionHandler(tok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), tok.Value()) {
		t.Error("session body does not carry the token")
	}
	if !strings.Contains(rec.Body.String(), `"boot_id"`) {
		t.Error("session body lacks boot_id")
	}

	// Non-GET must be refused.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/session", nil))
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /session status = %d, want 405", rec2.Code)
	}
	if allow := rec2.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

// TestSessionHandler_NilToken ensures the handler cannot panic on a nil token.
func TestSessionHandler_NilToken(t *testing.T) {
	rec := httptest.NewRecorder()
	SessionHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/session", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "csrf_token\":\"") && strings.Count(rec.Body.String(), "\"csrf_token\":\"\"") > 0 {
		t.Log("nil token serialized as empty string (acceptable)")
	}
}

// TestBootIDChangesPerBoot verifies the client-visible restart signal.
func TestBootIDChangesPerBoot(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 64; i++ {
		tok := mustToken(t)
		id := bootIDFrom(tok)
		if len(id) != 8 {
			t.Fatalf("boot id %q length = %d, want 8", id, len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate boot id %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestCSRFGuard_ConcurrentForgery hammers the guard to catch data races.
func TestCSRFGuard_ConcurrentForgery(t *testing.T) {
	tok := mustToken(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CSRFGuard(tok)(inner)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/keys", strings.NewReader("{}"))
				if i%2 == 0 {
					req.Header.Set(CSRFHeader, tok.Value())
					req.Header.Set(AdminConfirmHeader, "confirm")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if i%2 == 0 && rec.Code != http.StatusOK {
					t.Errorf("valid request failed with %d", rec.Code)
				}
				if i%2 == 1 && rec.Code != http.StatusForbidden {
					t.Errorf("forged request returned %d, want 403", rec.Code)
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestIsSafeMethod pins the safe-method set, including that TRACE is unsafe.
func TestIsSafeMethod(t *testing.T) {
	safe := map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodOptions: true,
	}
	for _, m := range []string{
		http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
		http.MethodConnect, http.MethodTrace,
	} {
		if got := isSafeMethod(m); got != safe[m] {
			t.Errorf("isSafeMethod(%q) = %v, want %v", m, got, safe[m])
		}
	}
}

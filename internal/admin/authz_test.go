package admin

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/silent-knight19/lattice/internal/transport"
)

func principal(role Role, authed bool) *Principal {
	return &Principal{Role: role, Authenticated: authed}
}

// TestPermits covers the permission matrix and every fail-closed path.
func TestPermits(t *testing.T) {
	tests := []struct {
		name     string
		p        *Principal
		required Permission
		want     bool
	}{
		{"admin read", principal(RoleAdmin, true), PermissionRead, true},
		{"admin write", principal(RoleAdmin, true), PermissionWrite, true},
		{"admin admin", principal(RoleAdmin, true), PermissionAdmin, true},

		{"writer read", principal(RoleWriter, true), PermissionRead, true},
		{"writer write", principal(RoleWriter, true), PermissionWrite, true},
		{"writer denied admin", principal(RoleWriter, true), PermissionAdmin, false},

		{"reader read", principal(RoleReader, true), PermissionRead, true},
		{"reader denied write", principal(RoleReader, true), PermissionWrite, false},
		{"reader denied admin", principal(RoleReader, true), PermissionAdmin, false},

		// Fail closed on ambiguous input.
		{"nil principal", nil, PermissionRead, false},
		{"unauthenticated", principal(RoleAdmin, false), PermissionRead, false},
		{"empty role", principal("", true), PermissionRead, false},
		{"unknown role", principal(Role("superuser"), true), PermissionRead, false},
		{"unknown role uppercase", principal(Role("ADMIN"), true), PermissionRead, false},
		{"zero requirement", principal(RoleAdmin, true), 0, false},

		// Combined bits require ALL of them.
		{"read|write with reader", principal(RoleReader, true), PermissionRead | PermissionWrite, false},
		{"read|write with writer", principal(RoleWriter, true), PermissionRead | PermissionWrite, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Permits(tc.p, tc.required); got != tc.want {
				t.Errorf("Permits(%+v, %d) = %v, want %v", tc.p, tc.required, got, tc.want)
			}
		})
	}
}

// TestRoutePlan_EveryRouteDeclaresPermission is the SEC-4.3 completeness test: a route
// added without a declared permission must fail here rather than becoming an open door.
func TestRoutePlan_EveryRouteDeclaresPermission(t *testing.T) {
	if len(routePlan) == 0 {
		t.Fatal("route plan is empty")
	}
	for path, spec := range routePlan {
		if spec.Permission == 0 {
			t.Errorf("route %q declares no permission", path)
		}
		if spec.Permission != PermissionRead &&
			spec.Permission != PermissionWrite &&
			spec.Permission != PermissionAdmin {
			t.Errorf("route %q declares a non-canonical permission %d", path, spec.Permission)
		}
		if spec.Method == "" {
			t.Errorf("route %q has no method", path)
		}
		if spec.description == "" {
			t.Errorf("route %q has no description", path)
		}
		// Every mutating route must be a POST and require at least Write.
		if spec.Mutating {
			if spec.Method != http.MethodPost {
				t.Errorf("mutating route %q uses %s, want POST", path, spec.Method)
			}
			if spec.Permission&PermissionWrite != PermissionWrite {
				t.Errorf("mutating route %q does not require PermissionWrite", path)
			}
		}
	}
}

// TestRoutePlan_DestructiveRoutesAreAdminOnly pins the blast-radius decisions.
func TestRoutePlan_DestructiveRoutesAreAdminOnly(t *testing.T) {
	adminOnly := []string{
		RouteLabCrash, // SIGKILL the node
		RouteLabClean, // deletes files
		RouteRaftCamp, // disrupts availability
		RouteRaftStep, // forces an election
		RouteConsole,  // arbitrary command execution
		RouteDiag,     // exfiltration boundary
		RouteRaftLog,  // committed user commands
		RouteWALDump,  // raw user key/value data
		RouteEvents,   // high-value drive-by target
		RouteLabStart, // load generator
		RouteLabStat,  // workload introspection
	}
	for _, path := range adminOnly {
		perm, ok := PlanFor(path)
		if !ok {
			t.Errorf("route %q missing from plan", path)
			continue
		}
		if perm != PermissionAdmin {
			t.Errorf("route %q has permission %d, want PermissionAdmin", path, perm)
		}
	}

	// Data mutation must be Write, not Read.
	for _, path := range []string{RouteKeyPut, RouteKeyDelete, RouteLSMDo, RouteLSMFlush} {
		perm, ok := PlanFor(path)
		if !ok {
			t.Errorf("route %q missing from plan", path)
			continue
		}
		if perm != PermissionWrite {
			t.Errorf("route %q has permission %d, want PermissionWrite", path, perm)
		}
	}
}

// TestRouter_DefaultDenyUnknownPaths verifies unregistered API paths yield JSON 404 and
// never reach the SPA fallback.
func TestRouter_DefaultDenyUnknownPaths(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	spaCalled := false
	spa := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spaCalled = true
		w.WriteHeader(http.StatusOK)
	})

	specs := []RouteSpec{{
		Method: http.MethodGet, Path: RouteHealth, Permission: PermissionRead,
		Handler: ok, description: "health",
	}}
	rt, err := NewRouter(specs, spa)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	for _, path := range []string{
		"/api/v1/nope", "/api/v1/", "/api/v1", "/api/v1/../etc/passwd",
		"/api/v1/health/extra", "/api/v2/health", "/api/v1/health/x",
	} {
		spaCalled = false
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if spaCalled {
			t.Errorf("GET %s reached the SPA fallback; API paths must never do so", path)
		}
	}

	// The registered route still works.
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("registered route = %d, want 200", rec.Code)
	}
}

// TestRouter_RejectsMalformedRoutes verifies construction fails closed.
func TestRouter_RejectsMalformedRoutes(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cases := []struct {
		name string
		spec RouteSpec
	}{
		{"no permission", RouteSpec{Method: http.MethodGet, Path: "/x", Handler: ok}},
		{"no handler", RouteSpec{Method: http.MethodGet, Path: "/x", Permission: PermissionRead}},
		{"no method", RouteSpec{Path: "/x", Permission: PermissionRead, Handler: ok}},
		{"no path", RouteSpec{Method: http.MethodGet, Permission: PermissionRead, Handler: ok}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRouter([]RouteSpec{tc.spec}, nil); err == nil {
				t.Error("NewRouter accepted a malformed route; it must refuse to start")
			}
		})
	}

	// Duplicate registration is refused.
	dup := RouteSpec{Method: http.MethodGet, Path: "/x", Permission: PermissionRead, Handler: ok}
	if _, err := NewRouter([]RouteSpec{dup, dup}, nil); err == nil {
		t.Error("NewRouter accepted a duplicate route")
	}
}

// TestRouter_PathNormalization ensures a trailing slash cannot bypass registration.
func TestRouter_PathNormalization(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	rt, err := NewRouter([]RouteSpec{{
		Method: http.MethodGet, Path: "/health", Permission: PermissionRead, Handler: ok,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/api/v1/health/", "/api/v1/health//"} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (normalization mismatch)", p, rec.Code)
		}
	}
}

// TestAuthzMiddleware verifies role enforcement at the middleware layer.
func TestAuthzMiddleware(t *testing.T) {
	var executed bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executed = true
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name      string
		role      Role
		authed    bool
		required  Permission
		wantAllow bool
	}{
		{"reader on read route", RoleReader, true, PermissionRead, true},
		{"reader on write route", RoleReader, true, PermissionWrite, false},
		{"writer on write route", RoleWriter, true, PermissionWrite, true},
		{"writer on admin route", RoleWriter, true, PermissionAdmin, false},
		{"admin on admin route", RoleAdmin, true, PermissionAdmin, true},
		{"unknown role", Role("root"), true, PermissionRead, false},
		{"unauthenticated admin", RoleAdmin, false, PermissionAdmin, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			executed = false
			h := Authz(tc.required, func(*http.Request) *Principal {
				return principal(tc.role, tc.authed)
			})(inner)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))

			if tc.wantAllow {
				if !executed {
					t.Errorf("authorized request was denied (status %d)", rec.Code)
				}
			} else {
				if executed {
					t.Error("SECURITY: unauthorized request reached the handler")
				}
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403", rec.Code)
				}
				if hasAnyCORSHeader(rec.Header()) {
					t.Error("403 carries a CORS header")
				}
			}
		})
	}
}

// TestAuthzMiddleware_NilResolver verifies a nil principal denies rather than panics.
func TestAuthzMiddleware_NilResolver(t *testing.T) {
	executed := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { executed = true })
	h := Authz(PermissionRead, func(*http.Request) *Principal { return nil })(inner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))
	if executed {
		t.Error("nil principal reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestRoleFromPolicy verifies certificate-fingerprint role resolution fails closed.
func TestRoleFromPolicy(t *testing.T) {
	fp := sha256.Sum256([]byte("client-cert-der"))
	canonical := hex.EncodeToString(fp[:])

	policy, err := ParseAuthzPolicyJSON([]byte(`{"` + canonical + `":"admin"}`))
	if err != nil {
		t.Fatalf("ParseAuthzPolicyJSON: %v", err)
	}
	role, ok := RoleFromPolicy(policy, canonical)
	if !ok || role != RoleAdmin {
		t.Errorf("RoleFromPolicy = (%q,%v), want (admin,true)", role, ok)
	}
	// Case-insensitive lookup, matching transport semantics.
	if r, ok := RoleFromPolicy(policy, "  "+upperHex(canonical)+" "); !ok || r != RoleAdmin {
		t.Errorf("case-insensitive lookup failed: (%q,%v)", r, ok)
	}
	// Unknown fingerprint.
	if _, ok := RoleFromPolicy(policy, "deadbeef"); ok {
		t.Error("unknown fingerprint resolved to a role")
	}
	// Empty policy fails closed.
	var nilPolicy *AuthzPolicy
	if _, ok := RoleFromPolicy(nilPolicy, canonical); ok {
		t.Error("nil policy resolved a role")
	}
	if _, ok := RoleFromPolicy(&AuthzPolicy{}, canonical); ok {
		t.Error("empty policy resolved a role")
	}
}

func upperHex(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'f' {
			out[i] -= 32
		}
	}
	return string(out)
}

// TestPrincipalFromTLS verifies the fingerprint plumbing matches transport's hashing.
func TestPrincipalFromTLS(t *testing.T) {
	der := []byte("pretend-x509-der")
	sum := sha256.Sum256(der)
	want := transport.CertificateFingerprintSHA256(&x509.Certificate{Raw: der})
	if want != hex.EncodeToString(sum[:]) {
		t.Fatal("fingerprint derivation disagrees with transport")
	}
	p := PrincipalFromTLS(want, RoleWriter, true)
	if p.Role != RoleWriter || !p.Authenticated || p.Fingerprint != want {
		t.Errorf("unexpected principal %+v", p)
	}
	// Unauthenticated must not be permitted.
	if Permits(p, PermissionRead) {
		t.Error("unauthenticated principal permitted")
	}
}

// TestPermissionFromHeaderRole documents the proxy-header fallback risk.
func TestPermissionFromHeaderRole(t *testing.T) {
	base := &Principal{Role: RoleReader, Authenticated: true}

	// A bare "admin" header is ignored: no advisory prefix.
	if p := PermissionFromHeaderRole(base, "admin"); p.Role != RoleReader {
		t.Error("bare role header was honoured; it must require the 'role ' prefix")
	}
	// Properly prefixed is honoured (documented fallback).
	if p := PermissionFromHeaderRole(base, "Role: admin"); p.Role != RoleAdmin {
		t.Errorf("prefixed header not honoured, got %q", p.Role)
	}
	// Garbage is ignored.
	if p := PermissionFromHeaderRole(base, "Role: nonsense"); p.Role != RoleReader {
		t.Error("invalid role header was honoured")
	}
	// nil base is safe.
	if p := PermissionFromHeaderRole(nil, "Role: admin"); p != nil {
		t.Error("nil principal should stay nil")
	}
	// Authenticated is not escalated by the header.
	p := PermissionFromHeaderRole(&Principal{Role: RoleReader}, "Role: admin")
	if p.Authenticated {
		t.Error("header fallback must not set Authenticated")
	}
	if Permits(p, PermissionRead) {
		t.Error("header-derived principal must remain unauthorized")
	}
}

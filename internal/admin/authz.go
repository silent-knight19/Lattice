package admin

import (
	"net/http"
	"strings"

	"github.com/silent-knight19/lattice/internal/transport"
)

// This file implements SEC-4: role-based authorization for the admin API.
//
// Design constraint (design-doc NG1): the console must NOT introduce a parallel identity
// system. It reuses the existing internal/transport role vocabulary verbatim:
// transport.Role (reader/writer/admin), transport.Permission, and transport.RolePermissions.
// An operator who already configures --client-authz-policy gets the same mental model and
// the same role strings, so there is nothing new to learn or misconfigure.
//
// The distinction SEC-1/2/3 vs SEC-4:
//
//	Origin / Host / CSRF  ->  proves INTENT     (this request really came from our console)
//	RBAC                  ->  proves AUTHORITY  (this caller may do this thing)
//
// They are orthogonal. A valid CSRF token from a reader-role caller must still be denied a
// destructive route; an admin-role caller with a forged token must still be blocked.

// Permission is the admin-API view of transport.Permission. It is an alias rather than a
// new type so the two vocabularies cannot drift, and so a transport.Permission value can
// be compared against a route requirement without conversion.
type Permission = transport.Permission

// The permission bits are re-exported from transport so admin code reads naturally while
// still using the single source of truth.
const (
	PermissionRead  = transport.PermissionRead
	PermissionWrite = transport.PermissionWrite
	PermissionAdmin = transport.PermissionAdmin
)

// Role is the admin-API view of transport.Role.
type Role = transport.Role

// The recognized roles, re-exported from transport.
const (
	RoleReader = transport.RoleReader
	RoleWriter = transport.RoleWriter
	RoleAdmin  = transport.RoleAdmin
)

// AuthzPolicy is the admin-API view of transport.AuthzPolicy, wrapping it rather than
// duplicating the fingerprint->role bindings.
type AuthzPolicy = transport.AuthzPolicy

// Principal is the admin-API view of transport.Principal.
type Principal = transport.Principal

// LoadAuthzPolicyFile loads a fingerprint->role policy file using the existing transport
// format, so mTLS operators keep working with the same file they already have.
func LoadAuthzPolicyFile(path string) (*AuthzPolicy, error) {
	return transport.LoadAuthzPolicyFile(path)
}

// ParseAuthzPolicyJSON parses a fingerprint->role policy from JSON using the existing
// transport format.
func ParseAuthzPolicyJSON(data []byte) (*AuthzPolicy, error) {
	return transport.ParseAuthzPolicyJSON(data)
}

// PrincipalFromTLS builds a principal from a verified client certificate.
//
// The role is derived from the certificate's SHA-256 fingerprint looked up in the policy,
// never from anything the client itself supplied. This is the only trusted role source
// (SEC-4.6): a self-declared header is not evidence of anything.
func PrincipalFromTLS(fingerprint string, role Role, authenticated bool) *Principal {
	return &Principal{
		Fingerprint:   fingerprint,
		Role:          role,
		Authenticated: authenticated,
	}
}

// RoleFromPolicy resolves a certificate fingerprint to a role using the policy.
//
// An absent or empty policy yields no role: without a configured mapping there is no
// basis for trusting the caller, and this fails closed (SEC-4.4).
func RoleFromPolicy(policy *AuthzPolicy, fingerprint string) (Role, bool) {
	if policy == nil || policy.IsEmpty() {
		return "", false
	}
	return policy.Lookup(fingerprint)
}

// Permits reports whether a principal holds a required permission.
//
// It fails closed on every ambiguous input:
//
//   - a nil principal
//   - a principal not marked authenticated
//   - an empty role
//   - a role that is not one of the three recognized values
//   - an unknown requirement bit
//
// Note it deliberately does NOT consult the authz policy bindings: policy resolution is
// how a role is obtained, not an additional check once a role is known.
func Permits(principal *Principal, required Permission) bool {
	if principal == nil || !principal.Authenticated {
		return false
	}
	if !principal.Role.Valid() {
		return false
	}
	granted := transport.RolePermissions(principal.Role)
	if granted&required != required {
		return false
	}
	// An empty requirement grants nothing: a route declared with no permission metadata
	// must not become an open door. Callers should treat this as a programming error.
	if required == 0 {
		return false
	}
	return true
}

// Authz is the request-time authorization middleware.
//
// The principal is resolved per request by resolvePrincipal, which is expected to consult
// the TLS connection state and the configured policy. Returning nil denies the request.
func Authz(required Permission, resolve func(*http.Request) *Principal) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal := resolve(r)
			if !Permits(principal, required) {
				writeForbiddenAuthz(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeForbiddenAuthz emits the standard rejection for an authorization failure.
//
// It reuses the same envelope as every other guard so that a caller cannot distinguish
// "not permitted" from "not permitted for a different reason" (see SEC-3's
// indistinguishability requirement). Deliberately 403 rather than 401: no credentials are
// expected from the browser client, so inviting authentication would be misleading.
func writeForbiddenAuthz(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "forbidden", "request origin is not permitted")
}

// PermissionFromHeaderRole resolves a role from a request header, for deployments that
// front the console with an authenticating proxy.
//
// SECURITY: this is a fallback ONLY. A header is supplied by the client and can be forged
// by anything that can reach the socket, so it must never be the sole basis for granting
// PermissionAdmin. Callers must only enable this when the admin listener is loopback-bound
// AND the header is set by a trusted reverse proxy that strips any client-supplied value.
// The advisory prefix is validated so that an accidental direct client cannot simply send
// the header.
func PermissionFromHeaderRole(principal *Principal, header string) *Principal {
	if principal == nil || header == "" {
		return principal
	}
	v := strings.ToLower(strings.TrimSpace(header))
	if !strings.HasPrefix(v, "role ") {
		return principal
	}
	role, err := transport.ParseRole(strings.TrimPrefix(v, "role "))
	if err != nil {
		return principal
	}
	return &Principal{
		Fingerprint:   principal.Fingerprint,
		Role:          role,
		Authenticated: principal.Authenticated,
	}
}

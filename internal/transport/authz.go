package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/security"
)

// Role defines an authorized client role in Lattice's role-based access control (RBAC) model.
type Role string

const (
	// RoleReader is permitted read-only queries and telemetry (GET, EXISTS, STATS).
	RoleReader Role = "reader"

	// RoleWriter is permitted both read and write operations (GET, EXISTS, STATS, PUT, DELETE, BATCH).
	RoleWriter Role = "writer"

	// RoleAdmin is permitted all currently supported client operations.
	RoleAdmin Role = "admin"
)

// Valid reports whether r is a recognized RBAC role.
func (r Role) Valid() bool {
	switch r {
	case RoleReader, RoleWriter, RoleAdmin:
		return true
	default:
		return false
	}
}

// ParseRole validates and canonicalizes a role string.
func ParseRole(s string) (Role, error) {
	clean := Role(strings.ToLower(strings.TrimSpace(s)))
	if !clean.Valid() {
		return "", fmt.Errorf("%w: unsupported role %q (must be reader, writer, or admin)", errors.ErrInvalidAuthzPolicy, s)
	}
	return clean, nil
}

// Permission defines a discrete privilege required to execute one or more operations.
type Permission uint32

const (
	// PermissionRead grants access to read queries (OpGet, OpExists, OpStats).
	PermissionRead Permission = 1 << 0

	// PermissionWrite grants access to mutation operations (OpPut, OpDelete, OpBatch).
	PermissionWrite Permission = 1 << 1

	// PermissionAdmin grants administrative operations.
	PermissionAdmin Permission = 1 << 2
)

// RequiredPermission returns the Permission required to execute the given operation code.
func RequiredPermission(op OpCode) (Permission, error) {
	switch op {
	case OpGet, OpExists, OpStats:
		return PermissionRead, nil
	case OpPut, OpDelete, OpBatch:
		return PermissionWrite, nil
	default:
		return 0, fmt.Errorf("%w: unsupported opcode 0x%02x", errors.ErrInvalidOpCode, byte(op))
	}
}

// RolePermissions returns the aggregate permissions granted to the specified role.
func RolePermissions(role Role) Permission {
	switch role {
	case RoleReader:
		return PermissionRead
	case RoleWriter:
		return PermissionRead | PermissionWrite
	case RoleAdmin:
		return PermissionRead | PermissionWrite | PermissionAdmin
	default:
		return 0
	}
}

// Principal represents the authenticated client identity bound to a connection.
type Principal struct {
	Fingerprint   string
	Role          Role
	Authenticated bool
}

// CertificateFingerprintSHA256 returns the canonical lowercase hexadecimal SHA-256 fingerprint
// of an X.509 certificate's DER bytes (cert.Raw).
func CertificateFingerprintSHA256(cert *x509.Certificate) string {
	if cert == nil || len(cert.Raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// ValidateFingerprint validates and canonicalizes a SHA-256 certificate fingerprint string.
// A valid fingerprint must be exactly 64 hexadecimal characters.
func ValidateFingerprint(fp string) (string, error) {
	trimmed := strings.TrimSpace(fp)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("%w: fingerprint cannot be empty", errors.ErrInvalidAuthzPolicy)
	}
	if len(trimmed) != 64 {
		return "", fmt.Errorf("%w: fingerprint %q must be exactly 64 hex characters (got %d)", errors.ErrInvalidAuthzPolicy, fp, len(trimmed))
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return "", fmt.Errorf("%w: malformed hex fingerprint %q: %v", errors.ErrInvalidAuthzPolicy, fp, err)
	}
	return strings.ToLower(trimmed), nil
}

// PolicyEntry represents an explicit binding of a certificate fingerprint to an RBAC role.
type PolicyEntry struct {
	Fingerprint string `json:"fingerprint"`
	Role        Role   `json:"role"`
}

// AuthzPolicy represents an immutable, server-owned authorization policy mapping
// authenticated client certificate fingerprints to authorized RBAC roles.
type AuthzPolicy struct {
	// bindings maps lowercase 64-character hex fingerprints to validated Roles.
	bindings map[string]Role
}

// NewAuthzPolicy constructs and validates an immutable AuthzPolicy from a map of fingerprint to role.
func NewAuthzPolicy(entries map[string]string) (*AuthzPolicy, error) {
	if entries == nil {
		return &AuthzPolicy{bindings: make(map[string]Role)}, nil
	}

	bindings := make(map[string]Role, len(entries))
	for rawFP, rawRole := range entries {
		canonicalFP, err := ValidateFingerprint(rawFP)
		if err != nil {
			return nil, err
		}

		role, err := ParseRole(rawRole)
		if err != nil {
			return nil, err
		}

		if existing, exists := bindings[canonicalFP]; exists {
			if existing != role {
				return nil, fmt.Errorf("%w: conflicting duplicate role for fingerprint %s: %q vs %q",
					errors.ErrInvalidAuthzPolicy, canonicalFP, existing, role)
			}
		}
		bindings[canonicalFP] = role
	}

	return &AuthzPolicy{bindings: bindings}, nil
}

// NewAuthzPolicyFromRoles constructs and validates an immutable AuthzPolicy from a map of fingerprint to typed Role.
func NewAuthzPolicyFromRoles(entries map[string]Role) (*AuthzPolicy, error) {
	if entries == nil {
		return &AuthzPolicy{bindings: make(map[string]Role)}, nil
	}

	bindings := make(map[string]Role, len(entries))
	for rawFP, role := range entries {
		canonicalFP, err := ValidateFingerprint(rawFP)
		if err != nil {
			return nil, err
		}

		if !role.Valid() {
			return nil, fmt.Errorf("%w: unsupported role %q for fingerprint %s", errors.ErrInvalidAuthzPolicy, role, canonicalFP)
		}

		if existing, exists := bindings[canonicalFP]; exists {
			if existing != role {
				return nil, fmt.Errorf("%w: conflicting duplicate role for fingerprint %s: %q vs %q",
					errors.ErrInvalidAuthzPolicy, canonicalFP, existing, role)
			}
		}
		bindings[canonicalFP] = role
	}

	return &AuthzPolicy{bindings: bindings}, nil
}

// NewAuthzPolicyFromEntries constructs an AuthzPolicy from an ordered list of PolicyEntry structures,
// rejecting conflicting duplicate bindings.
func NewAuthzPolicyFromEntries(entries []PolicyEntry) (*AuthzPolicy, error) {
	bindings := make(map[string]Role, len(entries))
	for _, entry := range entries {
		canonicalFP, err := ValidateFingerprint(entry.Fingerprint)
		if err != nil {
			return nil, err
		}

		if !entry.Role.Valid() {
			return nil, fmt.Errorf("%w: unsupported role %q for fingerprint %s", errors.ErrInvalidAuthzPolicy, entry.Role, canonicalFP)
		}

		if existing, exists := bindings[canonicalFP]; exists {
			if existing != entry.Role {
				return nil, fmt.Errorf("%w: conflicting duplicate role for fingerprint %s: %q vs %q",
					errors.ErrInvalidAuthzPolicy, canonicalFP, existing, entry.Role)
			}
		}
		bindings[canonicalFP] = entry.Role
	}

	return &AuthzPolicy{bindings: bindings}, nil
}

// Validate verifies that all internal bindings conform to required invariants.
func (p *AuthzPolicy) Validate() error {
	if p == nil {
		return nil
	}
	for fp, role := range p.bindings {
		if _, err := ValidateFingerprint(fp); err != nil {
			return err
		}
		if !role.Valid() {
			return fmt.Errorf("%w: unsupported role %q for fingerprint %s", errors.ErrInvalidAuthzPolicy, role, fp)
		}
	}
	return nil
}

// Lookup finds the configured role for a given certificate fingerprint.
// Returns (role, true) if found, or ("", false) if the fingerprint is not present.
func (p *AuthzPolicy) Lookup(fingerprint string) (Role, bool) {
	if p == nil || len(p.bindings) == 0 {
		return "", false
	}
	canonical := strings.ToLower(strings.TrimSpace(fingerprint))
	role, found := p.bindings[canonical]
	return role, found
}

// AuthorizeRole reports whether a given role has permission to execute the specified operation.
func (p *AuthzPolicy) AuthorizeRole(role Role, op OpCode) bool {
	if !role.Valid() {
		return false
	}
	reqPerm, err := RequiredPermission(op)
	if err != nil {
		return false
	}
	granted := RolePermissions(role)
	return (granted & reqPerm) == reqPerm
}

// Authorize reports whether the given principal is authorized to execute the operation.
func (p *AuthzPolicy) Authorize(principal *Principal, op OpCode) bool {
	if principal == nil || !principal.Authenticated || principal.Role == "" {
		return false
	}
	return p.AuthorizeRole(principal.Role, op)
}

// Len returns the number of certificate fingerprint bindings configured in the policy.
func (p *AuthzPolicy) Len() int {
	if p == nil {
		return 0
	}
	return len(p.bindings)
}

// IsEmpty reports whether the policy contains zero bindings.
func (p *AuthzPolicy) IsEmpty() bool {
	return p == nil || len(p.bindings) == 0
}

// Bindings returns a defensive copy of the policy mappings.
func (p *AuthzPolicy) Bindings() map[string]Role {
	if p == nil {
		return nil
	}
	cpy := make(map[string]Role, len(p.bindings))
	for k, v := range p.bindings {
		cpy[k] = v
	}
	return cpy
}

// ParseAuthzPolicyJSON parses and validates an AuthzPolicy from JSON data.
// Supported formats:
//   - Mapping object: {"<fingerprint>": "<role>", ...}
//   - Array of objects: [{"fingerprint": "...", "role": "..."}, ...]
func ParseAuthzPolicyJSON(data []byte) (*AuthzPolicy, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: empty policy JSON data", errors.ErrInvalidAuthzPolicy)
	}

	if trimmed[0] == '[' {
		var entries []PolicyEntry
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&entries); err != nil {
			return nil, fmt.Errorf("%w: malformed JSON array: %v", errors.ErrInvalidAuthzPolicy, err)
		}
		return NewAuthzPolicyFromEntries(entries)
	}

	// Detect duplicate keys in JSON object
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()

	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: invalid JSON: %v", errors.ErrInvalidAuthzPolicy, err)
	}
	delim, ok := t.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("%w: expected JSON object or array", errors.ErrInvalidAuthzPolicy)
	}

	bindings := make(map[string]Role)
	seenRaw := make(map[string]struct{})

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: failed to read JSON key: %v", errors.ErrInvalidAuthzPolicy, err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: expected string JSON key", errors.ErrInvalidAuthzPolicy)
		}

		var roleStr string
		if err := dec.Decode(&roleStr); err != nil {
			return nil, fmt.Errorf("%w: failed to decode role for key %s: %v", errors.ErrInvalidAuthzPolicy, key, err)
		}

		canonicalFP, err := ValidateFingerprint(key)
		if err != nil {
			return nil, err
		}

		role, err := ParseRole(roleStr)
		if err != nil {
			return nil, err
		}

		if _, duplicate := seenRaw[canonicalFP]; duplicate {
			if existing := bindings[canonicalFP]; existing != role {
				return nil, fmt.Errorf("%w: conflicting duplicate entry for fingerprint %s: %q vs %q",
					errors.ErrInvalidAuthzPolicy, canonicalFP, existing, role)
			}
		}
		seenRaw[canonicalFP] = struct{}{}
		bindings[canonicalFP] = role
	}

	return &AuthzPolicy{bindings: bindings}, nil
}

// LoadAuthzPolicyFile reads a JSON configuration file and constructs an AuthzPolicy.
func LoadAuthzPolicyFile(path string) (*AuthzPolicy, error) {
	cleanPath, err := security.CleanAndValidatePath(path)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid policy file path: %v", errors.ErrInvalidAuthzPolicy, err)
	}

	info, err := os.Stat(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot access policy file: %v", errors.ErrInvalidAuthzPolicy, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: path %s is a directory, not a policy file", errors.ErrInvalidAuthzPolicy, cleanPath)
	}
	if info.Size() > 1024*1024 {
		return nil, fmt.Errorf("%w: policy file exceeds maximum allowed limit (1 MiB)", errors.ErrInvalidAuthzPolicy)
	}

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read policy file: %v", errors.ErrInvalidAuthzPolicy, err)
	}

	return ParseAuthzPolicyJSON(data)
}

// ParseAuthzPolicyString parses a comma-separated key=value string (fingerprint=role,fingerprint=role).
func ParseAuthzPolicyString(s string) (*AuthzPolicy, error) {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) == 0 {
		return &AuthzPolicy{bindings: make(map[string]Role)}, nil
	}

	pairs := strings.Split(trimmed, ",")
	entries := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if len(pair) == 0 {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: invalid policy pair %q (expected fingerprint=role)", errors.ErrInvalidAuthzPolicy, pair)
		}
		fp := strings.TrimSpace(parts[0])
		role := strings.TrimSpace(parts[1])
		if existing, exists := entries[strings.ToLower(fp)]; exists && strings.ToLower(existing) != strings.ToLower(role) {
			return nil, fmt.Errorf("%w: conflicting duplicate role for fingerprint %s: %q vs %q",
				errors.ErrInvalidAuthzPolicy, fp, existing, role)
		}
		entries[fp] = role
	}

	return NewAuthzPolicy(entries)
}

type authzContextKey string

const principalContextKey authzContextKey = "lattice.authz.principal"

// WithPrincipal returns a new Context bearing the given Principal.
func WithPrincipal(ctx context.Context, principal *Principal) context.Context {
	return context.WithValue(ctx, principalContextKey, principal)
}

// PrincipalFromContext extracts the Principal from ctx, or nil if none is present.
func PrincipalFromContext(ctx context.Context) *Principal {
	if ctx == nil {
		return nil
	}
	p, _ := ctx.Value(principalContextKey).(*Principal)
	return p
}

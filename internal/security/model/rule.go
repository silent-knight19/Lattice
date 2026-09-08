package model

// AuditRule defines the interface that all security analysis rules must implement.
type AuditRule interface {
	// ID returns the stable, canonical identifier for the rule (e.g. "SECURITY-001").
	ID() string
	// Name returns a concise human-readable title for the rule.
	Name() string
	// Description returns an explanation of what the rule checks and why.
	Description() string
	// Category returns the broad security classification (e.g. "AST", "SECRET", "PERM", "CONFIG").
	Category() string
	// DefaultSeverity returns the baseline severity for findings emitted by this rule.
	DefaultSeverity() Severity
	// Run executes the audit check against the provided context and returns discovered findings.
	Run(ctx *AuditContext) ([]Finding, error)
}

// RuleMetadata provides metadata describing an audit rule.
type RuleMetadata struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Severity    Severity `json:"severity"`
}

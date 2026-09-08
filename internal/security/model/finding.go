package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// FindingStatus defines the operational lifecycle state of an audit finding.
type FindingStatus string

const (
	// StatusOpen: Newly discovered finding pending verification or remediation.
	StatusOpen FindingStatus = "OPEN"
	// StatusVerified: Manually or automatedly confirmed finding with reproduction.
	StatusVerified FindingStatus = "VERIFIED"
	// StatusFixed: Remediation implemented and regression test verified.
	StatusFixed FindingStatus = "FIXED"
	// StatusAccepted: Known limitation or accepted residual operational risk.
	StatusAccepted FindingStatus = "ACCEPTED"
	// StatusDismissed: Ruled out as false positive or non-applicable.
	StatusDismissed FindingStatus = "DISMISSED"
)

// Finding represents a single, structured security audit finding.
type Finding struct {
	ID                string         `json:"id"`
	RuleID            string         `json:"rule_id"`
	Title             string         `json:"title"`
	Severity          Severity       `json:"severity"`
	Classification    Classification `json:"classification"`
	Component         string         `json:"component"`
	File              string         `json:"file"`
	Line              int            `json:"line"`
	Description       string         `json:"description"`
	Preconditions     string         `json:"preconditions,omitempty"`
	AttackPath        string         `json:"attack_path,omitempty"`
	Impact            string         `json:"impact,omitempty"`
	Evidence          string         `json:"evidence"`
	Reproduction      string         `json:"reproduction,omitempty"`
	Recommendation    string         `json:"recommendation"`
	Status            FindingStatus  `json:"status"`
	Suppressed        bool           `json:"suppressed,omitempty"`
	SuppressionReason string         `json:"suppression_reason,omitempty"`
}

// GenerateFindingID generates a deterministic, stable identifier from invariant finding attributes.
// It explicitly excludes timestamps to ensure identical code and rules yield identical IDs.
func GenerateFindingID(ruleID, file string, line int, title string) string {
	normFile := strings.ReplaceAll(file, "\\", "/")
	data := fmt.Sprintf("%s|%s|%d|%s", ruleID, normFile, line, strings.TrimSpace(title))
	h := sha256.Sum256([]byte(data))
	return fmt.Sprintf("FIND-%s-%s", ruleID, hex.EncodeToString(h[:4]))
}

// MaskSecret masks sensitive data, ensuring raw credentials or tokens are never
// emitted in error messages, audit reports, or stdout.
func MaskSecret(secret string) string {
	s := strings.TrimSpace(secret)
	n := len(s)
	if n == 0 {
		return "[EMPTY]"
	}

	h := sha256.Sum256([]byte(s))
	hashPrefix := hex.EncodeToString(h[:3])

	if n <= 8 {
		return fmt.Sprintf("***[REDACTED len=%d sha256=%s]***", n, hashPrefix)
	}

	prefix := s[:3]
	suffix := s[n-2:]
	return fmt.Sprintf("%s***%s [REDACTED len=%d sha256=%s]", prefix, suffix, n, hashPrefix)
}

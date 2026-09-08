package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Severity represents the evaluated risk level of a security finding.
type Severity int

const (
	// SeverityUnknown is the uninitialized severity value.
	SeverityUnknown Severity = iota
	// SeverityInformational represents architectural notes, inventory entries, or design targets.
	SeverityInformational
	// SeverityLow represents minimal risk findings with limited or theoretical impact.
	SeverityLow
	// SeverityMedium represents security weaknesses with constrained exploitability or defense-in-depth gaps.
	SeverityMedium
	// SeverityHigh represents significant security defects threatening integrity, durability, or availability.
	SeverityHigh
	// SeverityCritical represents catastrophic security vulnerabilities allowing arbitrary execution, data destruction, or complete compromise.
	SeverityCritical
)

const (
	severityStrCritical      = "CRITICAL"
	severityStrHigh          = "HIGH"
	severityStrMedium        = "MEDIUM"
	severityStrLow           = "LOW"
	severityStrInformational = "INFORMATIONAL"
	severityStrUnknown       = "UNKNOWN"
)

// String returns the canonical uppercase string representation of the severity.
func (s Severity) String() string {
	switch s {
	case SeverityCritical:
		return severityStrCritical
	case SeverityHigh:
		return severityStrHigh
	case SeverityMedium:
		return severityStrMedium
	case SeverityLow:
		return severityStrLow
	case SeverityInformational:
		return severityStrInformational
	default:
		return severityStrUnknown
	}
}

// ParseSeverity parses a string into a Severity enum.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case severityStrCritical:
		return SeverityCritical, nil
	case severityStrHigh:
		return SeverityHigh, nil
	case severityStrMedium:
		return SeverityMedium, nil
	case severityStrLow:
		return SeverityLow, nil
	case severityStrInformational, "INFO":
		return SeverityInformational, nil
	case severityStrUnknown, "":
		return SeverityUnknown, nil
	default:
		return SeverityUnknown, fmt.Errorf("unknown severity: %q", s)
	}
}

// MarshalJSON serializes the severity to its JSON string representation.
func (s Severity) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON deserializes the severity from a JSON string representation.
func (s *Severity) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	val, err := ParseSeverity(str)
	if err != nil {
		return err
	}
	*s = val
	return nil
}

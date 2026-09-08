package model

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Classification represents the formal evidence classification of an audit result.
type Classification int

const (
	// ClassificationUnknown represents uninitialized classification.
	ClassificationUnknown Classification = iota
	// ConfirmedVulnerability: A concrete security weakness supported by source inspection, reproduction, or test.
	ConfirmedVulnerability
	// SecurityWeakness: A concrete weakness exists, but exploitability or impact is limited or context-dependent.
	SecurityWeakness
	// HardeningOpportunity: A design could be strengthened, but current behavior is not demonstrated to be vulnerable.
	HardeningOpportunity
	// DesignTarget: Intended security property documented by the architecture.
	DesignTarget
	// NotApplicable: The checked security condition does not apply to the current repository.
	NotApplicable
	// FalsePositiveDismissed: An apparent issue was investigated and ruled out using repository evidence.
	FalsePositiveDismissed
)

const (
	classStrConfirmedVulnerability = "CONFIRMED VULNERABILITY"
	classStrSecurityWeakness       = "SECURITY WEAKNESS"
	classStrHardeningOpportunity   = "HARDENING OPPORTUNITY"
	classStrDesignTarget           = "DESIGN TARGET"
	classStrNotApplicable          = "NOT APPLICABLE"
	classStrFalsePositive          = "FALSE POSITIVE / DISMISSED"
	classStrUnknown                = "UNKNOWN"
)

// String returns the canonical uppercase string representation of the classification.
func (c Classification) String() string {
	switch c {
	case ConfirmedVulnerability:
		return classStrConfirmedVulnerability
	case SecurityWeakness:
		return classStrSecurityWeakness
	case HardeningOpportunity:
		return classStrHardeningOpportunity
	case DesignTarget:
		return classStrDesignTarget
	case NotApplicable:
		return classStrNotApplicable
	case FalsePositiveDismissed:
		return classStrFalsePositive
	default:
		return classStrUnknown
	}
}

// ParseClassification parses a string into a Classification enum.
func ParseClassification(s string) (Classification, error) {
	norm := strings.ToUpper(strings.TrimSpace(s))
	switch norm {
	case classStrConfirmedVulnerability, "VULNERABILITY":
		return ConfirmedVulnerability, nil
	case classStrSecurityWeakness, "WEAKNESS":
		return SecurityWeakness, nil
	case classStrHardeningOpportunity, "HARDENING":
		return HardeningOpportunity, nil
	case classStrDesignTarget, "DESIGN":
		return DesignTarget, nil
	case classStrNotApplicable, "N/A", "NA":
		return NotApplicable, nil
	case classStrFalsePositive, "FALSE POSITIVE", "DISMISSED":
		return FalsePositiveDismissed, nil
	case classStrUnknown, "":
		return ClassificationUnknown, nil
	default:
		return ClassificationUnknown, fmt.Errorf("unknown classification: %q", s)
	}
}

// MarshalJSON serializes the classification to its JSON string representation.
func (c Classification) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.String())
}

// UnmarshalJSON deserializes the classification from a JSON string representation.
func (c *Classification) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	val, err := ParseClassification(str)
	if err != nil {
		return err
	}
	*c = val
	return nil
}

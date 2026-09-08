package model

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

var (
	// ErrSuppressionMissingRuleID indicates an audit suppression was declared without a RuleID.
	ErrSuppressionMissingRuleID = errors.New("suppression requires a non-empty rule ID")
	// ErrSuppressionMissingFilePath indicates an audit suppression was declared without a target file path.
	ErrSuppressionMissingFilePath = errors.New("suppression requires a non-empty file path")
	// ErrSuppressionMissingReason indicates an audit suppression lacked a mandatory justification.
	ErrSuppressionMissingReason = errors.New("suppression requires an explicit non-empty audit reason")
)

// Suppression represents an auditable override suppressing a finding.
type Suppression struct {
	RuleID   string `json:"rule_id"`
	FilePath string `json:"file_path"`
	Line     int    `json:"line,omitempty"` // 0 indicates whole-file suppression
	Reason   string `json:"reason"`
}

// Validate ensures all mandatory fields of a suppression are present.
func (s *Suppression) Validate() error {
	if strings.TrimSpace(s.RuleID) == "" {
		return ErrSuppressionMissingRuleID
	}
	if strings.TrimSpace(s.FilePath) == "" {
		return ErrSuppressionMissingFilePath
	}
	if strings.TrimSpace(s.Reason) == "" {
		return ErrSuppressionMissingReason
	}
	return nil
}

// SuppressionManager coordinates auditable finding suppressions.
type SuppressionManager struct {
	mu           sync.RWMutex
	suppressions []Suppression
}

// NewSuppressionManager constructs an empty suppression manager.
func NewSuppressionManager() *SuppressionManager {
	return &SuppressionManager{
		suppressions: make([]Suppression, 0),
	}
}

// Add registers a validated suppression. Returns error if invalid.
func (m *SuppressionManager) Add(s Suppression) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("invalid suppression: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suppressions = append(m.suppressions, s)
	return nil
}

// IsSuppressed checks whether a given finding is suppressed by any registered rule.
// Returns boolean and the auditable reason.
func (m *SuppressionManager) IsSuppressed(ruleID, file string, line int) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cleanFile := filepath.Clean(strings.ReplaceAll(file, "\\", "/"))

	for _, s := range m.suppressions {
		if s.RuleID != ruleID && s.RuleID != "*" {
			continue
		}
		suppFile := filepath.Clean(strings.ReplaceAll(s.FilePath, "\\", "/"))
		if !strings.HasSuffix(cleanFile, suppFile) && cleanFile != suppFile {
			continue
		}
		if s.Line != 0 && s.Line != line {
			continue
		}
		return true, s.Reason
	}
	return false, ""
}

// All returns a copy of all registered suppressions.
func (m *SuppressionManager) All() []Suppression {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Suppression, len(m.suppressions))
	copy(out, m.suppressions)
	return out
}

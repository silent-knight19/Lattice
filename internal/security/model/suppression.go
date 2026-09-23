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

// DefaultAuditableSuppressions returns the canonical repository suppressions for verified,
// low-level OS syscall wrappers, test fixtures, and synthetic benchmark generators.
func DefaultAuditableSuppressions() []Suppression {
	return []Suppression{
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/engine/unlink_darwin.go",
			Reason:   "Audited OS-specific atomic unlinkat syscall requiring unsafe.Pointer for raw uintptr syscall arguments",
		},
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/engine/unlink_linux.go",
			Reason:   "Audited OS-specific atomic unlinkat syscall requiring unsafe.Pointer for raw uintptr syscall arguments",
		},
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/metrics/disk_windows.go",
			Reason:   "Audited OS-specific GetDiskFreeSpaceExW Windows API requiring unsafe.Pointer for raw uintptr syscall arguments",
		},
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/version/current_ops_darwin.go",
			Reason:   "Audited OS-specific descriptor-pinned renameat/openat syscall requiring unsafe.Pointer for raw uintptr syscall arguments",
		},
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/version/current_ops_linux.go",
			Reason:   "Audited OS-specific descriptor-pinned renameat/openat syscall requiring unsafe.Pointer for raw uintptr syscall arguments",
		},
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/benchmark/histogram_test.go",
			Reason:   "Test-only memory layout and struct size verification",
		},
		{
			RuleID:   "SECURITY-001",
			FilePath: "internal/cache/sharded_test.go",
			Reason:   "Test-only pointer alignment verification",
		},
		{
			RuleID:   "SECURITY-002",
			FilePath: "cmd/lattice-cli/repl_test.go",
			Reason:   "Test fixture executing compiled CLI binary for interactive REPL validation",
		},
		{
			RuleID:   "SECURITY-002",
			FilePath: "cmd/lattice/chaos_sigkill_test.go",
			Reason:   "Test fixture executing compiled daemon subprocess for crash recovery validation (SIGKILL)",
		},
		{
			RuleID:   "SECURITY-002",
			FilePath: "cmd/lattice/daemon_test.go",
			Reason:   "Test fixture executing compiled daemon subprocess for lifecycle validation",
		},
		{
			RuleID:   "SECURITY-002",
			FilePath: "cmd/lattice/dump_wal_test.go",
			Reason:   "Test fixture executing compiled CLI dump-wal command",
		},
		{
			RuleID:   "SECURITY-002",
			FilePath: "cmd/lattice/inspect_test.go",
			Reason:   "Test fixture executing compiled CLI inspect command",
		},
		{
			RuleID:   "SECURITY-009",
			FilePath: "cmd/lattice-bench/runner.go",
			Reason:   "Synthetic benchmark load generator using math/rand for repeatable Zipfian distribution generation",
		},
		{
			RuleID:   "SECURITY-009",
			FilePath: "internal/benchmark/zipf.go",
			Reason:   "Synthetic benchmark workload generator using math/rand for Zipfian key selection",
		},
	}
}

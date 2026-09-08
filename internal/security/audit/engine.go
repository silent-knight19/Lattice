package audit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/silent-knight19/lattice/internal/security/model"
)

var (
	// ErrNilRule is returned when attempting to register a nil AuditRule.
	ErrNilRule = errors.New("audit rule cannot be nil")
	// ErrDuplicateRule is returned when registering a rule with an existing ID.
	ErrDuplicateRule = errors.New("duplicate audit rule ID")
)

// AuditError records an execution or file-access failure encountered by the audit engine.
// Audit errors are explicitly preserved in reports to distinguish tool failures from clean audits.
type AuditError struct {
	RuleID  string `json:"rule_id,omitempty"`
	File    string `json:"file,omitempty"`
	Message string `json:"message"`
}

func (e AuditError) Error() string {
	if e.File != "" && e.RuleID != "" {
		return fmt.Sprintf("[%s] %s: %s", e.RuleID, e.File, e.Message)
	}
	if e.File != "" {
		return fmt.Sprintf("%s: %s", e.File, e.Message)
	}
	return e.Message
}

// AuditReport summarizes the results of a security audit run.
type AuditReport struct {
	AuditVersion string          `json:"audit_version"`
	Commit       string          `json:"commit"`
	Timestamp    time.Time       `json:"timestamp"`
	RootDir      string          `json:"root_dir"`
	RulesRun     []string        `json:"rules_run"`
	Findings     []model.Finding `json:"findings"`
	Suppressed   []model.Finding `json:"suppressed"`
	Errors       []AuditError    `json:"errors"`
	Counts       map[string]int  `json:"counts"`
}

// Engine coordinates security rule registration, file collection, and audit execution.
type Engine struct {
	mu    sync.RWMutex
	rules map[string]model.AuditRule
}

// NewEngine creates an empty security audit engine.
func NewEngine() *Engine {
	return &Engine{
		rules: make(map[string]model.AuditRule),
	}
}

// RegisterRule registers a security audit rule.
func (e *Engine) RegisterRule(rule model.AuditRule) error {
	if rule == nil {
		return ErrNilRule
	}
	id := rule.ID()
	if strings.TrimSpace(id) == "" {
		return errors.New("audit rule ID cannot be empty")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.rules[id]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateRule, id)
	}
	e.rules[id] = rule
	return nil
}

// GetRule returns the rule registered with the given ID.
func (e *Engine) GetRule(id string) (model.AuditRule, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.rules[id]
	return r, ok
}

// Rules returns a slice of all registered rules sorted by ID.
func (e *Engine) Rules() []model.AuditRule {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]model.AuditRule, 0, len(e.rules))
	for _, r := range e.rules {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID() < out[j].ID()
	})
	return out
}

// CollectFiles walks the given root directory and returns all non-excluded files.
func (e *Engine) CollectFiles(rootDir string, exclusions []string) ([]string, []AuditError) {
	var files []string
	var auditErrors []AuditError

	exclMap := make(map[string]bool)
	for _, ex := range exclusions {
		exclMap[ex] = true
	}

	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			auditErrors = append(auditErrors, AuditError{
				File:    path,
				Message: fmt.Sprintf("access error: %v", err),
			})
			return nil // continue walking other directories
		}

		name := d.Name()
		if d.IsDir() {
			if exclMap[name] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip hidden or unwanted metadata files
		if strings.HasPrefix(name, ".") && name != ".golangci.yml" && name != ".editorconfig" {
			return nil
		}

		files = append(files, path)
		return nil
	})

	if err != nil {
		auditErrors = append(auditErrors, AuditError{
			File:    rootDir,
			Message: fmt.Sprintf("directory walk error: %v", err),
		})
	}

	sort.Strings(files)
	return files, auditErrors
}

// Run executes all registered rules against the provided AuditContext and produces a deterministic report.
func (e *Engine) Run(ctx *model.AuditContext) (*AuditReport, error) {
	if ctx == nil {
		return nil, errors.New("audit context cannot be nil")
	}

	rules := e.Rules()
	var allFindings []model.Finding
	var suppressedFindings []model.Finding
	var auditErrors []AuditError

	rulesRun := make([]string, 0, len(rules))

	for _, rule := range rules {
		rulesRun = append(rulesRun, rule.ID())

		findings, err := rule.Run(ctx)
		if err != nil {
			auditErrors = append(auditErrors, AuditError{
				RuleID:  rule.ID(),
				Message: fmt.Sprintf("rule execution error: %v", err),
			})
			continue
		}

		for _, f := range findings {
			// Check suppression
			if ctx.Suppression != nil {
				if suppressed, reason := ctx.Suppression.IsSuppressed(f.RuleID, f.File, f.Line); suppressed {
					f.Suppressed = true
					f.SuppressionReason = reason
					suppressedFindings = append(suppressedFindings, f)
					continue
				}
			}
			allFindings = append(allFindings, f)
		}
	}

	// Capture any file loading/parsing errors from context
	for file, err := range ctx.Errors() {
		auditErrors = append(auditErrors, AuditError{
			File:    ctx.RelativePath(file),
			Message: fmt.Sprintf("file parsing/loading error: %v", err),
		})
	}

	// Sort findings deterministically
	SortFindings(allFindings)
	SortFindings(suppressedFindings)

	// Sort errors deterministically
	sort.Slice(auditErrors, func(i, j int) bool {
		if auditErrors[i].RuleID != auditErrors[j].RuleID {
			return auditErrors[i].RuleID < auditErrors[j].RuleID
		}
		if auditErrors[i].File != auditErrors[j].File {
			return auditErrors[i].File < auditErrors[j].File
		}
		return auditErrors[i].Message < auditErrors[j].Message
	})

	counts := make(map[string]int)
	counts["total_findings"] = len(allFindings)
	counts["suppressed_findings"] = len(suppressedFindings)
	counts["critical"] = 0
	counts["high"] = 0
	counts["medium"] = 0
	counts["low"] = 0
	counts["informational"] = 0

	for _, f := range allFindings {
		switch f.Severity {
		case model.SeverityCritical:
			counts["critical"]++
		case model.SeverityHigh:
			counts["high"]++
		case model.SeverityMedium:
			counts["medium"]++
		case model.SeverityLow:
			counts["low"]++
		case model.SeverityInformational:
			counts["informational"]++
		}
	}

	report := &AuditReport{
		AuditVersion: "1.0.0-LATTICE-SEC",
		Commit:       getGitCommitOrFallback(ctx.RootDir),
		Timestamp:    time.Now().UTC(),
		RootDir:      ctx.RootDir,
		RulesRun:     rulesRun,
		Findings:     allFindings,
		Suppressed:   suppressedFindings,
		Errors:       auditErrors,
		Counts:       counts,
	}

	return report, nil
}

// SortFindings sorts findings in strict, deterministic order:
// Severity (descending) -> File (asc) -> Line (asc) -> RuleID (asc) -> FindingID (asc).
func SortFindings(findings []model.Finding) {
	sort.Slice(findings, func(i, j int) bool {
		// Severity descending (Critical first)
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		// File ascending
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		// Line ascending
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		// RuleID ascending
		if findings[i].RuleID != findings[j].RuleID {
			return findings[i].RuleID < findings[j].RuleID
		}
		// Finding ID ascending
		return findings[i].ID < findings[j].ID
	})
}

func getGitCommitOrFallback(rootDir string) string {
	headPath := filepath.Join(rootDir, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		return "UNKNOWN"
	}
	headStr := strings.TrimSpace(string(data))
	if strings.HasPrefix(headStr, "ref: ") {
		refPath := filepath.Join(rootDir, ".git", strings.TrimPrefix(headStr, "ref: "))
		refData, err := os.ReadFile(refPath)
		if err == nil {
			return strings.TrimSpace(string(refData))
		}
	}
	if len(headStr) >= 7 {
		return headStr
	}
	return "UNKNOWN"
}

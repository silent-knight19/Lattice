package audit

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/security/model"
)

type mockRule struct {
	id         string
	name       string
	category   string
	severity   model.Severity
	findingsFn func(ctx *model.AuditContext) ([]model.Finding, error)
}

func (m *mockRule) ID() string                      { return m.id }
func (m *mockRule) Name() string                    { return m.name }
func (m *mockRule) Description() string             { return "mock rule" }
func (m *mockRule) Category() string                { return m.category }
func (m *mockRule) DefaultSeverity() model.Severity { return m.severity }
func (m *mockRule) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	if m.findingsFn != nil {
		return m.findingsFn(ctx)
	}
	return nil, nil
}

func TestEngine_RuleRegistration(t *testing.T) {
	eng := NewEngine()

	// Nil rule
	if err := eng.RegisterRule(nil); !errors.Is(err, ErrNilRule) {
		t.Errorf("expected ErrNilRule, got %v", err)
	}

	// Empty ID rule
	emptyRule := &mockRule{id: ""}
	if err := eng.RegisterRule(emptyRule); err == nil {
		t.Errorf("expected error for empty rule ID, got nil")
	}

	// Valid rule
	r1 := &mockRule{id: "RULE-001", name: "Rule 1"}
	if err := eng.RegisterRule(r1); err != nil {
		t.Fatalf("unexpected error registering rule: %v", err)
	}

	// Duplicate rule
	if err := eng.RegisterRule(r1); !errors.Is(err, ErrDuplicateRule) {
		t.Errorf("expected ErrDuplicateRule, got %v", err)
	}

	// GetRule
	queried, ok := eng.GetRule("RULE-001")
	if !ok || queried.ID() != "RULE-001" {
		t.Errorf("expected to find RULE-001")
	}

	// Rules list sorted
	r2 := &mockRule{id: "RULE-002", name: "Rule 2"}
	_ = eng.RegisterRule(r2)
	rules := eng.Rules()
	if len(rules) != 2 || rules[0].ID() != "RULE-001" || rules[1].ID() != "RULE-002" {
		t.Errorf("expected sorted rules list, got: %v", rules)
	}
}

func TestEngine_ExecutionAndSortingDeterminism(t *testing.T) {
	eng := NewEngine()

	r1 := &mockRule{
		id: "RULE-001",
		findingsFn: func(ctx *model.AuditContext) ([]model.Finding, error) {
			return []model.Finding{
				{
					ID:             model.GenerateFindingID("RULE-001", "b.go", 10, "High issue"),
					RuleID:         "RULE-001",
					Title:          "High issue",
					Severity:       model.SeverityHigh,
					Classification: model.SecurityWeakness,
					File:           "b.go",
					Line:           10,
				},
				{
					ID:             model.GenerateFindingID("RULE-001", "a.go", 20, "Critical issue"),
					RuleID:         "RULE-001",
					Title:          "Critical issue",
					Severity:       model.SeverityCritical,
					Classification: model.ConfirmedVulnerability,
					File:           "a.go",
					Line:           20,
				},
			}, nil
		},
	}

	r2 := &mockRule{
		id: "RULE-002",
		findingsFn: func(ctx *model.AuditContext) ([]model.Finding, error) {
			return []model.Finding{
				{
					ID:             model.GenerateFindingID("RULE-002", "a.go", 5, "Low issue"),
					RuleID:         "RULE-002",
					Title:          "Low issue",
					Severity:       model.SeverityLow,
					Classification: model.HardeningOpportunity,
					File:           "a.go",
					Line:           5,
				},
			}, nil
		},
	}

	_ = eng.RegisterRule(r1)
	_ = eng.RegisterRule(r2)

	ctx := model.NewAuditContext(".", []string{"a.go", "b.go"})

	rep1, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error running engine: %v", err)
	}

	rep2, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error running engine 2nd time: %v", err)
	}

	if len(rep1.Findings) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(rep1.Findings))
	}

	// Verify order: Critical (a.go:20) -> High (b.go:10) -> Low (a.go:5)
	if rep1.Findings[0].Severity != model.SeverityCritical || rep1.Findings[0].File != "a.go" {
		t.Errorf("expected 1st finding to be Critical on a.go, got %v", rep1.Findings[0])
	}
	if rep1.Findings[1].Severity != model.SeverityHigh || rep1.Findings[1].File != "b.go" {
		t.Errorf("expected 2nd finding to be High on b.go, got %v", rep1.Findings[1])
	}
	if rep1.Findings[2].Severity != model.SeverityLow || rep1.Findings[2].File != "a.go" {
		t.Errorf("expected 3rd finding to be Low on a.go, got %v", rep1.Findings[2])
	}

	// Verify repeat-run determinism
	for i := range rep1.Findings {
		if rep1.Findings[i].ID != rep2.Findings[i].ID {
			t.Errorf("run determinism mismatch at index %d: %s != %s", i, rep1.Findings[i].ID, rep2.Findings[i].ID)
		}
	}
}

func TestEngine_SuppressionHandling(t *testing.T) {
	eng := NewEngine()

	_ = eng.RegisterRule(&mockRule{
		id: "SEC-001",
		findingsFn: func(ctx *model.AuditContext) ([]model.Finding, error) {
			return []model.Finding{
				{
					ID:       model.GenerateFindingID("SEC-001", "internal/wal/writer.go", 42, "Issue"),
					RuleID:   "SEC-001",
					Title:    "Issue",
					Severity: model.SeverityHigh,
					File:     "internal/wal/writer.go",
					Line:     42,
				},
			}, nil
		},
	})

	ctx := model.NewAuditContext(".", []string{"internal/wal/writer.go"})
	_ = ctx.Suppression.Add(model.Suppression{
		RuleID:   "SEC-001",
		FilePath: "internal/wal/writer.go",
		Line:     42,
		Reason:   "audited safe pattern",
	})

	report, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(report.Findings) != 0 {
		t.Errorf("expected active findings to be 0 due to suppression, got %d", len(report.Findings))
	}
	if len(report.Suppressed) != 1 {
		t.Fatalf("expected 1 suppressed finding, got %d", len(report.Suppressed))
	}
	if !report.Suppressed[0].Suppressed || report.Suppressed[0].SuppressionReason != "audited safe pattern" {
		t.Errorf("expected suppressed finding with reason, got: %+v", report.Suppressed[0])
	}
}

func TestEngine_ErrorPreservation(t *testing.T) {
	eng := NewEngine()

	// Rule that returns an execution error
	_ = eng.RegisterRule(&mockRule{
		id: "FAIL-RULE",
		findingsFn: func(ctx *model.AuditContext) ([]model.Finding, error) {
			return nil, errors.New("simulated rule failure")
		},
	})

	ctx := model.NewAuditContext(".", []string{})
	report, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("engine should not fail when a rule errors: %v", err)
	}

	if len(report.Errors) != 1 {
		t.Fatalf("expected 1 error preserved, got %d", len(report.Errors))
	}
	if report.Errors[0].RuleID != "FAIL-RULE" {
		t.Errorf("expected error from FAIL-RULE, got %s", report.Errors[0].RuleID)
	}
}

func TestEngine_CollectFilesWithExclusion(t *testing.T) {
	tempDir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(tempDir, ".git"), 0700)
	_ = os.MkdirAll(filepath.Join(tempDir, "internal", "wal"), 0700)

	_ = os.WriteFile(filepath.Join(tempDir, ".git", "config"), []byte("git"), 0600)
	_ = os.WriteFile(filepath.Join(tempDir, "internal", "wal", "writer.go"), []byte("package wal"), 0600)

	eng := NewEngine()
	files, errs := eng.CollectFiles(tempDir, []string{".git"})

	if len(errs) > 0 {
		t.Errorf("unexpected collection errors: %v", errs)
	}
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 file collected (.git excluded), got %d: %v", len(files), files)
	}
	if filepath.Base(files[0]) != "writer.go" {
		t.Errorf("expected writer.go, got %s", files[0])
	}
}

func TestEngine_EmptyRepository(t *testing.T) {
	eng := NewEngine()
	_ = eng.RegisterRule(&mockRule{id: "EMPTY-001"})

	ctx := model.NewAuditContext(t.TempDir(), []string{})
	report, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error for empty repo: %v", err)
	}
	if report.Counts["total_findings"] != 0 {
		t.Errorf("expected 0 findings for empty repo, got %d", report.Counts["total_findings"])
	}
	if len(report.Errors) != 0 {
		t.Errorf("expected 0 errors for empty repo, got %d", len(report.Errors))
	}
}

func TestEngine_ConcurrentAuditInvocation(t *testing.T) {
	eng := NewEngine()
	_ = eng.RegisterRule(&mockRule{
		id: "CONC-001",
		findingsFn: func(ctx *model.AuditContext) ([]model.Finding, error) {
			return []model.Finding{
				{
					ID:             model.GenerateFindingID("CONC-001", "file.go", 1, "Issue"),
					RuleID:         "CONC-001",
					Title:          "Issue",
					Severity:       model.SeverityMedium,
					Classification: model.SecurityWeakness,
					File:           "file.go",
					Line:           1,
				},
			}, nil
		},
	})

	ctx := model.NewAuditContext(".", []string{"file.go"})

	var wg sync.WaitGroup
	const workers = 10
	errChan := make(chan error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, err := eng.Run(ctx)
			if err != nil {
				errChan <- err
				return
			}
			if len(rep.Findings) != 1 {
				errChan <- errors.New("expected exactly 1 finding")
				return
			}
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("concurrent run error: %v", err)
	}
}

func TestEngine_MissingAndUnreadableFiles(t *testing.T) {
	tempDir := t.TempDir()
	missingFile := filepath.Join(tempDir, "does_not_exist.go")

	ctx := model.NewAuditContext(tempDir, []string{missingFile})

	// AST parsing on missing file should record error, not panic
	ast, err := ctx.GetAST(missingFile)
	if err == nil || ast != nil {
		t.Errorf("expected error for missing file, got ast=%v", ast)
	}

	// GetContent on missing file
	content, err := ctx.GetContent(missingFile)
	if err == nil || content != nil {
		t.Errorf("expected error reading missing file")
	}

	// Binary check on missing file
	isBin, err := ctx.IsBinary(missingFile)
	if err == nil || isBin {
		t.Errorf("expected error checking binary for missing file")
	}

	errs := ctx.Errors()
	if len(errs) == 0 {
		t.Errorf("expected recorded errors in context")
	}
}

func TestEngine_SymlinkHandling(t *testing.T) {
	tempDir := t.TempDir()
	realDir := filepath.Join(tempDir, "real")
	symDir := filepath.Join(tempDir, "symlink")

	if err := os.MkdirAll(realDir, 0700); err != nil {
		t.Fatalf("failed to create real dir: %v", err)
	}
	realFile := filepath.Join(realDir, "source.go")
	if err := os.WriteFile(realFile, []byte("package real"), 0600); err != nil {
		t.Fatalf("failed to write real file: %v", err)
	}

	// Create symlink to realDir
	if err := os.Symlink(realDir, symDir); err != nil {
		t.Skipf("symlinks not supported or permitted on this filesystem: %v", err)
	}

	eng := NewEngine()
	files, _ := eng.CollectFiles(tempDir, []string{})

	// Ensure engine does not crash and collects files deterministically
	if len(files) == 0 {
		t.Errorf("expected at least 1 file collected")
	}
}

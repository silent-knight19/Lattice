package audit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/security/audit"
	"github.com/silent-knight19/lattice/internal/security/model"
	"github.com/silent-knight19/lattice/internal/security/report"
	"github.com/silent-knight19/lattice/internal/security/rules"
)

func TestAudit_FullRepositoryScanAndReportGeneration(t *testing.T) {
	// Locate repository root
	repoRoot, err := filepath.Abs("../../../")
	if err != nil {
		t.Fatalf("failed to resolve repository root: %v", err)
	}

	eng := audit.NewEngine()
	if err := rules.RegisterAllDefaultRules(eng); err != nil {
		t.Fatalf("failed to register default security rules: %v", err)
	}

	// Exclude testdata and VCS directories from production report
	exclusions := append(model.DefaultExclusionDirs, "testdata")
	files, collectionErrs := eng.CollectFiles(repoRoot, exclusions)
	if len(collectionErrs) > 0 {
		t.Logf("collection encountered %d non-fatal warnings: %v", len(collectionErrs), collectionErrs)
	}
	if len(files) == 0 {
		t.Fatalf("no files collected from repository root %s", repoRoot)
	}

	ctx := model.NewAuditContext(repoRoot, files)
	ctx.Exclusions = exclusions

	auditRep, err := eng.Run(ctx)
	if err != nil {
		t.Fatalf("audit engine execution failed: %v", err)
	}

	// Verify all default rules were executed
	expectedRulesCount := len(rules.DefaultRules())
	if len(auditRep.RulesRun) != expectedRulesCount {
		t.Errorf("expected %d rules to run, got %d", expectedRulesCount, len(auditRep.RulesRun))
	}

	// Test generating Markdown report
	mdContent := report.GenerateMarkdown(auditRep)
	if len(mdContent) == 0 {
		t.Errorf("expected non-empty markdown report")
	}

	// Test generating JSON report
	jsonData, err := report.GenerateJSON(auditRep)
	if err != nil || len(jsonData) == 0 {
		t.Fatalf("failed to generate JSON report: %v", err)
	}

	// Write to temporary directory to verify file I/O without dirtying repository working tree
	testOutDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(testOutDir, "security-audit-report.md"), []byte(mdContent), 0644); err != nil {
		t.Fatalf("failed to write test markdown report: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testOutDir, "security-audit-report.json"), jsonData, 0644); err != nil {
		t.Fatalf("failed to write test JSON report: %v", err)
	}

	// If explicitly requested via environment variable, update tracked documentation reports
	if os.Getenv("LATTICE_UPDATE_SECURITY_REPORT") == "1" {
		secDocsDir := filepath.Join(repoRoot, "docs", "security")
		_ = os.MkdirAll(secDocsDir, 0755)
		_ = os.WriteFile(filepath.Join(secDocsDir, "security-audit-report.md"), []byte(mdContent), 0644)
		_ = os.WriteFile(filepath.Join(secDocsDir, "security-audit-report.json"), jsonData, 0644)
	}

	t.Logf("Audit complete: %d active findings, %d suppressed, %d errors across %d files.",
		len(auditRep.Findings), len(auditRep.Suppressed), len(auditRep.Errors), len(files))
}

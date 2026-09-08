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

	// Verify reports directory exists
	secDocsDir := filepath.Join(repoRoot, "docs", "security")
	if err := os.MkdirAll(secDocsDir, 0755); err != nil {
		t.Fatalf("failed to create docs/security: %v", err)
	}

	// Generate Markdown report
	mdContent := report.GenerateMarkdown(auditRep)
	mdPath := filepath.Join(secDocsDir, "security-audit-report.md")
	if err := os.WriteFile(mdPath, []byte(mdContent), 0644); err != nil {
		t.Fatalf("failed to write security-audit-report.md: %v", err)
	}

	// Generate JSON report
	jsonData, err := report.GenerateJSON(auditRep)
	if err != nil {
		t.Fatalf("failed to generate JSON report: %v", err)
	}
	jsonPath := filepath.Join(secDocsDir, "security-audit-report.json")
	if err := os.WriteFile(jsonPath, jsonData, 0644); err != nil {
		t.Fatalf("failed to write security-audit-report.json: %v", err)
	}

	t.Logf("Audit complete: %d active findings, %d suppressed, %d errors across %d files.",
		len(auditRep.Findings), len(auditRep.Suppressed), len(auditRep.Errors), len(files))
}

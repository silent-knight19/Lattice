package rules

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/security/model"
)

func getTestdataPath(t *testing.T) string {
	abs, err := filepath.Abs("../testdata")
	if err != nil {
		t.Fatalf("failed to get testdata path: %v", err)
	}
	return abs
}

func TestRule_SECURITY_001_Unsafe(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")
	safeFile := filepath.Join(testdata, "safe", "clean_storage.go")

	rule := NewSec001Unsafe()

	// Test dangerous file
	ctxDangerous := model.NewAuditContext(testdata, []string{dangerousFile})
	findings, err := rule.Run(ctxDangerous)
	if err != nil {
		t.Fatalf("rule execution error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 unsafe finding in dangerous fixture, got %d", len(findings))
	}
	if findings[0].RuleID != "SECURITY-001" || findings[0].Severity != model.SeverityHigh {
		t.Errorf("unexpected finding metadata: %+v", findings[0])
	}

	// Test safe file
	ctxSafe := model.NewAuditContext(testdata, []string{safeFile})
	findingsSafe, err := rule.Run(ctxSafe)
	if err != nil {
		t.Fatalf("rule execution error: %v", err)
	}
	if len(findingsSafe) != 0 {
		t.Errorf("expected 0 findings in safe fixture, got %d", len(findingsSafe))
	}
}

func TestRule_SECURITY_002_Exec(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")
	safeFile := filepath.Join(testdata, "safe", "clean_storage.go")

	rule := NewSec002Exec()

	// Dangerous
	ctxDangerous := model.NewAuditContext(testdata, []string{dangerousFile})
	findings, err := rule.Run(ctxDangerous)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 exec finding, got %d", len(findings))
	}
	if findings[0].Severity != model.SeverityCritical {
		t.Errorf("expected Critical severity for exec in prod code, got %v", findings[0].Severity)
	}

	// Safe
	ctxSafe := model.NewAuditContext(testdata, []string{safeFile})
	findingsSafe, err := rule.Run(ctxSafe)
	if err != nil || len(findingsSafe) != 0 {
		t.Errorf("expected 0 findings for safe fixture, got %d (err: %v)", len(findingsSafe), err)
	}
}

func TestRule_SECURITY_004_Perms(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")
	safeFile := filepath.Join(testdata, "safe", "clean_storage.go")

	rule := NewSec004Perms()

	// Dangerous (contains 0777)
	ctxDangerous := model.NewAuditContext(testdata, []string{dangerousFile})
	findings, err := rule.Run(ctxDangerous)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 perms finding for 0777, got %d", len(findings))
	}
	if !strings.Contains(findings[0].Evidence, "0777") {
		t.Errorf("expected evidence to show 0777, got %s", findings[0].Evidence)
	}

	// Safe (contains 0600)
	ctxSafe := model.NewAuditContext(testdata, []string{safeFile})
	findingsSafe, err := rule.Run(ctxSafe)
	if err != nil || len(findingsSafe) != 0 {
		t.Errorf("expected 0 findings for 0600 safe fixture, got %d", len(findingsSafe))
	}
}

func TestRule_SECURITY_008_Secrets(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")

	rule := NewSec008Secrets()

	// Explicitly pass the file without testdata exclusion filtering
	ctx := model.NewAuditContext(testdata, []string{dangerousFile})
	// Temporarily allow testdata path for this direct unit test
	ctx.Exclusions = []string{}

	findings, err := rule.Run(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should detect synthetic token
	foundToken := false
	for _, f := range findings {
		if strings.Contains(f.Title, "Hardcoded credential") {
			foundToken = true
			if strings.Contains(f.Evidence, "synthetic_token_abcdef1234567890") {
				t.Errorf("secret scanner must NEVER emit raw secret in evidence: %s", f.Evidence)
			}
			if !strings.Contains(f.Evidence, "REDACTED") {
				t.Errorf("evidence must indicate REDACTED: %s", f.Evidence)
			}
		}
	}
	if !foundToken {
		t.Errorf("expected to find hardcoded credential finding in dangerous fixture")
	}
}

func TestRule_SECURITY_009_Random(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")
	safeFile := filepath.Join(testdata, "safe", "clean_storage.go")

	rule := NewSec009Random()

	// Dangerous imports math/rand
	ctxDangerous := model.NewAuditContext(testdata, []string{dangerousFile})
	findings, err := rule.Run(ctxDangerous)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 random finding, got %d", len(findings))
	}

	// Safe uses crypto/rand
	ctxSafe := model.NewAuditContext(testdata, []string{safeFile})
	findingsSafe, err := rule.Run(ctxSafe)
	if err != nil || len(findingsSafe) != 0 {
		t.Errorf("expected 0 findings for safe fixture, got %d", len(findingsSafe))
	}
}

func TestRule_SECURITY_011_Panic(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")
	safeFile := filepath.Join(testdata, "safe", "clean_storage.go")

	rule := NewSec011Panic()

	// Dangerous has DecodePacketVulnerable with panic
	ctxDangerous := model.NewAuditContext(testdata, []string{dangerousFile})
	findings, err := rule.Run(ctxDangerous)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 panic finding, got %d", len(findings))
	}
	if !strings.Contains(findings[0].Title, "DecodePacketVulnerable") {
		t.Errorf("expected finding title to mention function name, got %s", findings[0].Title)
	}

	// Safe returns error
	ctxSafe := model.NewAuditContext(testdata, []string{safeFile})
	findingsSafe, err := rule.Run(ctxSafe)
	if err != nil || len(findingsSafe) != 0 {
		t.Errorf("expected 0 findings for safe fixture, got %d", len(findingsSafe))
	}
}

func TestRule_SECURITY_012_Alloc(t *testing.T) {
	testdata := getTestdataPath(t)
	dangerousFile := filepath.Join(testdata, "dangerous", "dangerous_fixtures.go")
	safeFile := filepath.Join(testdata, "safe", "clean_storage.go")

	rule := NewSec012Alloc()

	// Dangerous has ReadStreamUnbounded with make([]byte, sz) without bounds check
	ctxDangerous := model.NewAuditContext(testdata, []string{dangerousFile})
	findings, err := rule.Run(ctxDangerous)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 alloc finding, got %d", len(findings))
	}

	// Safe has bounds check: if lenVal > MaxSafeLen
	ctxSafe := model.NewAuditContext(testdata, []string{safeFile})
	findingsSafe, err := rule.Run(ctxSafe)
	if err != nil || len(findingsSafe) != 0 {
		t.Errorf("expected 0 findings for safe fixture with max check, got %d", len(findingsSafe))
	}
}

func TestRule_EdgecasesResilience(t *testing.T) {
	testdata := getTestdataPath(t)
	edgecasesDir := filepath.Join(testdata, "edgecases")

	files := []string{
		filepath.Join(edgecasesDir, "unicode_sample.go"),
		filepath.Join(edgecasesDir, "long_line.go"),
		filepath.Join(edgecasesDir, "empty.go"),
		filepath.Join(edgecasesDir, "binary.bin"),
	}

	ctx := model.NewAuditContext(testdata, files)

	// Run all default rules across edgecase files; verify zero crashes/panics
	rulesList := DefaultRules()
	for _, r := range rulesList {
		_, err := r.Run(ctx)
		if err != nil {
			t.Errorf("rule %s failed on edgecase input: %v", r.ID(), err)
		}
	}
}

func TestDepAuditRule_Inventory(t *testing.T) {
	root, err := filepath.Abs("../../../")
	if err != nil {
		t.Fatalf("failed to get root dir: %v", err)
	}

	ctx := model.NewAuditContext(root, []string{filepath.Join(root, "go.mod")})
	rule := NewDepAuditRule()

	findings, err := rule.Run(ctx)
	if err != nil {
		t.Fatalf("dep audit failed: %v", err)
	}

	if len(findings) != 1 {
		t.Fatalf("expected 1 dependency inventory finding, got %d", len(findings))
	}
	if findings[0].Classification != model.DesignTarget {
		t.Errorf("expected DesignTarget classification, got %v", findings[0].Classification)
	}
	if !strings.Contains(strings.ToLower(findings[0].Title), "dependency inventory complete") {
		t.Errorf("expected title to match inventory completion, got %s", findings[0].Title)
	}
}

package model

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeverity_JSON_And_String(t *testing.T) {
	cases := []struct {
		sev Severity
		str string
	}{
		{SeverityCritical, "CRITICAL"},
		{SeverityHigh, "HIGH"},
		{SeverityMedium, "MEDIUM"},
		{SeverityLow, "LOW"},
		{SeverityInformational, "INFORMATIONAL"},
		{SeverityUnknown, "UNKNOWN"},
	}

	for _, tc := range cases {
		if tc.sev.String() != tc.str {
			t.Errorf("expected %s, got %s", tc.str, tc.sev.String())
		}
		data, err := json.Marshal(tc.sev)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		var unmarshaled Severity
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if unmarshaled != tc.sev {
			t.Errorf("expected roundtrip %v, got %v", tc.sev, unmarshaled)
		}
	}

	// Parsing error
	if _, err := ParseSeverity("INVALID_SEVERITY"); err == nil {
		t.Errorf("expected error parsing invalid severity, got nil")
	}
}

func TestClassification_JSON_And_String(t *testing.T) {
	cases := []struct {
		class Classification
		str   string
	}{
		{ConfirmedVulnerability, "CONFIRMED VULNERABILITY"},
		{SecurityWeakness, "SECURITY WEAKNESS"},
		{HardeningOpportunity, "HARDENING OPPORTUNITY"},
		{DesignTarget, "DESIGN TARGET"},
		{NotApplicable, "NOT APPLICABLE"},
		{FalsePositiveDismissed, "FALSE POSITIVE / DISMISSED"},
		{ClassificationUnknown, "UNKNOWN"},
	}

	for _, tc := range cases {
		if tc.class.String() != tc.str {
			t.Errorf("expected %s, got %s", tc.str, tc.class.String())
		}
		data, err := json.Marshal(tc.class)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		var unmarshaled Classification
		if err := json.Unmarshal(data, &unmarshaled); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if unmarshaled != tc.class {
			t.Errorf("expected roundtrip %v, got %v", tc.class, unmarshaled)
		}
	}

	// Parsing aliases
	parsed, err := ParseClassification("VULNERABILITY")
	if err != nil || parsed != ConfirmedVulnerability {
		t.Errorf("expected ConfirmedVulnerability, got %v (%v)", parsed, err)
	}

	parsed, err = ParseClassification("N/A")
	if err != nil || parsed != NotApplicable {
		t.Errorf("expected NotApplicable, got %v (%v)", parsed, err)
	}

	if _, err := ParseClassification("UNKNOWN_CLASS"); err == nil {
		t.Errorf("expected error for unknown classification")
	}
}

func TestGenerateFindingID_Deterministic(t *testing.T) {
	id1 := GenerateFindingID("SECURITY-001", "internal/wal/writer.go", 42, "Unsafe write")
	id2 := GenerateFindingID("SECURITY-001", "internal/wal/writer.go", 42, "Unsafe write")
	id3 := GenerateFindingID("SECURITY-001", "internal/wal/writer.go", 43, "Unsafe write")
	id4 := GenerateFindingID("SECURITY-002", "internal/wal/writer.go", 42, "Unsafe write")

	if id1 != id2 {
		t.Errorf("expected identical finding IDs for identical inputs: %q != %q", id1, id2)
	}
	if id1 == id3 {
		t.Errorf("expected different IDs for different lines: %q == %q", id1, id3)
	}
	if id1 == id4 {
		t.Errorf("expected different IDs for different rules: %q == %q", id1, id4)
	}
	if !strings.HasPrefix(id1, "FIND-SECURITY-001-") {
		t.Errorf("unexpected ID format: %s", id1)
	}
}

func TestMaskSecret_NeverLeaksRaw(t *testing.T) {
	rawSecret := "super_secret_token_1234567890"
	masked := MaskSecret(rawSecret)

	if strings.Contains(masked, rawSecret) {
		t.Fatalf("masked output must NOT contain raw secret: %s", masked)
	}
	if !strings.Contains(masked, "REDACTED") {
		t.Errorf("masked output should indicate REDACTED: %s", masked)
	}
	if !strings.HasPrefix(masked, "sup***") {
		t.Errorf("masked output should show prefix: %s", masked)
	}

	// Short secret
	shortSecret := "abc"
	maskedShort := MaskSecret(shortSecret)
	if strings.Contains(maskedShort, "abc") {
		t.Errorf("short secret must be fully masked: %s", maskedShort)
	}

	// Empty secret
	emptyMasked := MaskSecret("")
	if emptyMasked != "[EMPTY]" {
		t.Errorf("expected [EMPTY], got %s", emptyMasked)
	}
}

func TestSuppressionManager(t *testing.T) {
	mgr := NewSuppressionManager()

	// Missing rule ID
	if err := mgr.Add(Suppression{FilePath: "foo.go", Reason: "valid"}); !errors.Is(err, ErrSuppressionMissingRuleID) {
		t.Errorf("expected ErrSuppressionMissingRuleID, got %v", err)
	}
	// Missing file path
	if err := mgr.Add(Suppression{RuleID: "SEC-001", Reason: "valid"}); !errors.Is(err, ErrSuppressionMissingFilePath) {
		t.Errorf("expected ErrSuppressionMissingFilePath, got %v", err)
	}
	// Missing reason
	if err := mgr.Add(Suppression{RuleID: "SEC-001", FilePath: "foo.go"}); !errors.Is(err, ErrSuppressionMissingReason) {
		t.Errorf("expected ErrSuppressionMissingReason, got %v", err)
	}

	// Valid suppression: whole file
	if err := mgr.Add(Suppression{RuleID: "SEC-001", FilePath: "internal/wal/foo.go", Reason: "accepted architectural pattern"}); err != nil {
		t.Fatalf("unexpected error adding suppression: %v", err)
	}

	// Valid suppression: specific line
	if err := mgr.Add(Suppression{RuleID: "SEC-002", FilePath: "internal/wal/bar.go", Line: 100, Reason: "test fixture"}); err != nil {
		t.Fatalf("unexpected error adding suppression: %v", err)
	}

	// Match whole file
	suppressed, reason := mgr.IsSuppressed("SEC-001", "internal/wal/foo.go", 42)
	if !suppressed || reason != "accepted architectural pattern" {
		t.Errorf("expected whole file suppressed: %v, reason: %s", suppressed, reason)
	}

	// Match specific line
	suppressed, reason = mgr.IsSuppressed("SEC-002", "internal/wal/bar.go", 100)
	if !suppressed || reason != "test fixture" {
		t.Errorf("expected line 100 suppressed: %v, reason: %s", suppressed, reason)
	}

	// Mismatched line
	suppressed, _ = mgr.IsSuppressed("SEC-002", "internal/wal/bar.go", 101)
	if suppressed {
		t.Errorf("expected line 101 NOT suppressed")
	}

	// Mismatched rule
	suppressed, _ = mgr.IsSuppressed("SEC-003", "internal/wal/foo.go", 42)
	if suppressed {
		t.Errorf("expected SEC-003 NOT suppressed")
	}
}

func TestAuditContext_FileOperations(t *testing.T) {
	tempDir := t.TempDir()
	goFilePath := filepath.Join(tempDir, "sample.go")
	binFilePath := filepath.Join(tempDir, "binary.bin")

	goContent := []byte("package main\n\nfunc main() {}\n")
	binContent := []byte{0x00, 0x01, 0x02, 0x03, 0x00}

	if err := os.WriteFile(goFilePath, goContent, 0600); err != nil {
		t.Fatalf("failed to write go file: %v", err)
	}
	if err := os.WriteFile(binFilePath, binContent, 0600); err != nil {
		t.Fatalf("failed to write bin file: %v", err)
	}

	ctx := NewAuditContext(tempDir, []string{goFilePath, binFilePath})

	// AST parsing
	astNode, err := ctx.GetAST(goFilePath)
	if err != nil || astNode == nil {
		t.Fatalf("failed to parse AST: %v", err)
	}
	if astNode.Name.Name != "main" {
		t.Errorf("expected package main, got %s", astNode.Name.Name)
	}

	// AST caching
	astNode2, err := ctx.GetAST(goFilePath)
	if err != nil || astNode != astNode2 {
		t.Errorf("expected cached AST pointer equality")
	}

	// Binary detection
	isBin, err := ctx.IsBinary(binFilePath)
	if err != nil || !isBin {
		t.Errorf("expected binFilePath to be binary: %v, %v", isBin, err)
	}

	isBin, err = ctx.IsBinary(goFilePath)
	if err != nil || isBin {
		t.Errorf("expected goFilePath to NOT be binary: %v, %v", isBin, err)
	}

	// Exclusion
	if !ctx.IsExcluded(".git/config") {
		t.Errorf("expected .git to be excluded")
	}
	if !ctx.IsExcluded(".itehaas/index") {
		t.Errorf("expected .itehaas to be excluded")
	}
	if ctx.IsExcluded("internal/wal/writer.go") {
		t.Errorf("expected internal/wal/writer.go NOT to be excluded")
	}

	// Relative path
	rel := ctx.RelativePath(goFilePath)
	if rel != "sample.go" {
		t.Errorf("expected sample.go, got %s", rel)
	}
}

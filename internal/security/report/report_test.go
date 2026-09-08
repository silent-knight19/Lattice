package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/security/audit"
	"github.com/silent-knight19/lattice/internal/security/model"
)

func TestReport_MarkdownAndJSON(t *testing.T) {
	finding := model.Finding{
		ID:             model.GenerateFindingID("SECURITY-008", "sample.go", 10, "Secret found"),
		RuleID:         "SECURITY-008",
		Title:          "Secret found",
		Severity:       model.SeverityHigh,
		Classification: model.ConfirmedVulnerability,
		Component:      "Secrets",
		File:           "sample.go",
		Line:           10,
		Description:    "Test description",
		Evidence:       model.MaskSecret("real_raw_secret_value_12345"),
		Recommendation: "Do not commit secrets",
		Status:         model.StatusOpen,
	}

	report := &audit.AuditReport{
		AuditVersion: "1.0.0-TEST",
		Commit:       "abc1234",
		Timestamp:    time.Now().UTC(),
		RootDir:      "/tmp/repo",
		RulesRun:     []string{"SECURITY-008"},
		Findings:     []model.Finding{finding},
		Suppressed:   []model.Finding{},
		Errors:       []audit.AuditError{},
		Counts: map[string]int{
			"total_findings": 1,
			"critical":       0,
			"high":           1,
			"medium":         0,
			"low":            0,
			"informational":  0,
		},
	}

	// 1. Markdown generation
	md := GenerateMarkdown(report)
	if !strings.Contains(md, "# Lattice: Security Audit Report") {
		t.Errorf("markdown header missing")
	}
	if !strings.Contains(md, "abc1234") {
		t.Errorf("commit missing in markdown")
	}
	if !strings.Contains(md, "REDACTED") {
		t.Errorf("masked evidence missing in markdown")
	}
	// Verify raw secret NEVER leaks in markdown
	if strings.Contains(md, "real_raw_secret_value_12345") {
		t.Fatalf("markdown report LEAKED raw secret!")
	}

	// 2. JSON generation
	jsonData, err := GenerateJSON(report)
	if err != nil {
		t.Fatalf("failed to generate JSON report: %v", err)
	}
	if strings.Contains(string(jsonData), "real_raw_secret_value_12345") {
		t.Fatalf("JSON report LEAKED raw secret!")
	}

	var roundtrip audit.AuditReport
	if err := json.Unmarshal(jsonData, &roundtrip); err != nil {
		t.Fatalf("failed to unmarshal generated JSON: %v", err)
	}
	if len(roundtrip.Findings) != 1 || roundtrip.Findings[0].ID != finding.ID {
		t.Errorf("roundtrip finding mismatch: %+v", roundtrip.Findings)
	}
}

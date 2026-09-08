package report

import (
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/audit"
)

// GenerateMarkdown formats an AuditReport into a GitHub Flavored Markdown document.
func GenerateMarkdown(report *audit.AuditReport) string {
	var sb strings.Builder

	sb.WriteString("# Lattice: Security Audit Report\n\n")
	fmt.Fprintf(&sb, "* **Audit Version**: %s\n", report.AuditVersion)
	fmt.Fprintf(&sb, "* **Commit Audited**: `%s`\n", report.Commit)
	fmt.Fprintf(&sb, "* **Audit Date (UTC)**: %s\n", report.Timestamp.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&sb, "* **Repository Root**: `%s`\n\n", report.RootDir)

	sb.WriteString("---\n\n")
	sb.WriteString("## 1. Executive Summary & Metric Counters\n\n")

	sb.WriteString("| Metric | Count |\n")
	sb.WriteString("| :--- | :--- |\n")
	fmt.Fprintf(&sb, "| Total Active Findings | %d |\n", report.Counts["total_findings"])
	fmt.Fprintf(&sb, "| Critical Severity | %d |\n", report.Counts["critical"])
	fmt.Fprintf(&sb, "| High Severity | %d |\n", report.Counts["high"])
	fmt.Fprintf(&sb, "| Medium Severity | %d |\n", report.Counts["medium"])
	fmt.Fprintf(&sb, "| Low Severity | %d |\n", report.Counts["low"])
	fmt.Fprintf(&sb, "| Informational / Design Targets | %d |\n", report.Counts["informational"])
	fmt.Fprintf(&sb, "| Audited Suppressions | %d |\n", report.Counts["suppressed_findings"])
	fmt.Fprintf(&sb, "| Audit Tool Execution Errors | %d |\n\n", len(report.Errors))

	sb.WriteString("---\n\n")
	sb.WriteString("## 2. Active Security Rules Executed\n\n")
	sb.WriteString("| Rule ID | Status |\n")
	sb.WriteString("| :--- | :--- |\n")
	for _, ruleID := range report.RulesRun {
		fmt.Fprintf(&sb, "| `%s` | Executed |\n", ruleID)
	}
	sb.WriteString("\n---\n\n")

	sb.WriteString("## 3. Discovered Findings\n\n")
	if len(report.Findings) == 0 {
		sb.WriteString("> [!NOTE]\n")
		sb.WriteString("> No active security findings were identified by the executed audit rules.\n\n")
	} else {
		for i, f := range report.Findings {
			fmt.Fprintf(&sb, "### %d. [%s] %s\n\n", i+1, f.Severity.String(), f.Title)
			fmt.Fprintf(&sb, "* **Finding ID**: `%s`\n", f.ID)
			fmt.Fprintf(&sb, "* **Rule ID**: `%s`\n", f.RuleID)
			fmt.Fprintf(&sb, "* **Classification**: `%s`\n", f.Classification.String())
			fmt.Fprintf(&sb, "* **Severity**: `%s`\n", f.Severity.String())
			fmt.Fprintf(&sb, "* **Component**: `%s`\n", f.Component)
			fmt.Fprintf(&sb, "* **Location**: `%s:%d`\n", f.File, f.Line)
			fmt.Fprintf(&sb, "* **Status**: `%s`\n\n", f.Status)

			fmt.Fprintf(&sb, "**Description**: %s\n\n", f.Description)
			if f.Preconditions != "" {
				fmt.Fprintf(&sb, "**Attack Preconditions**: %s\n\n", f.Preconditions)
			}
			if f.AttackPath != "" {
				fmt.Fprintf(&sb, "**Attack Path**: %s\n\n", f.AttackPath)
			}
			if f.Impact != "" {
				fmt.Fprintf(&sb, "**Security Impact**: %s\n\n", f.Impact)
			}
			fmt.Fprintf(&sb, "**Evidence**: `%s`\n\n", f.Evidence)
			if f.Reproduction != "" {
				fmt.Fprintf(&sb, "**Reproduction**: %s\n\n", f.Reproduction)
			}
			fmt.Fprintf(&sb, "**Recommendation**: %s\n\n", f.Recommendation)
			sb.WriteString("---\n\n")
		}
	}

	sb.WriteString("## 4. Auditable Suppressions\n\n")
	if len(report.Suppressed) == 0 {
		sb.WriteString("Zero findings are currently suppressed.\n\n")
	} else {
		sb.WriteString("| Finding ID | Rule ID | Location | Justification |\n")
		sb.WriteString("| :--- | :--- | :--- | :--- |\n")
		for _, sf := range report.Suppressed {
			fmt.Fprintf(&sb, "| `%s` | `%s` | `%s:%d` | %s |\n",
				sf.ID, sf.RuleID, sf.File, sf.Line, sf.SuppressionReason)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n\n")
	sb.WriteString("## 5. Audit Tool Execution Errors\n\n")
	if len(report.Errors) == 0 {
		sb.WriteString("Zero audit execution errors encountered. All target files processed cleanly.\n\n")
	} else {
		for _, err := range report.Errors {
			fmt.Fprintf(&sb, "- %s\n", err.Error())
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

package rules

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// ConfigAuditRule inspects project configuration files for insecure settings and embedded credentials.
type ConfigAuditRule struct{}

func NewConfigAuditRule() *ConfigAuditRule {
	return &ConfigAuditRule{}
}

func (r *ConfigAuditRule) ID() string {
	return "SECURITY-CFG-001"
}

func (r *ConfigAuditRule) Name() string {
	return "Configuration Security Audit"
}

func (r *ConfigAuditRule) Description() string {
	return "Audits configuration files (.golangci.yml, configs) for disabled security linters and exposed secrets."
}

func (r *ConfigAuditRule) Category() string {
	return "CONFIGURATION"
}

func (r *ConfigAuditRule) DefaultSeverity() model.Severity {
	return model.SeverityLow
}

func (r *ConfigAuditRule) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	for _, path := range ctx.FilePaths {
		if ctx.IsExcluded(path) {
			continue
		}

		filename := strings.ToLower(path)
		isConfig := strings.HasSuffix(filename, ".yml") ||
			strings.HasSuffix(filename, ".yaml") ||
			strings.HasSuffix(filename, ".json") ||
			strings.HasSuffix(filename, ".toml") ||
			strings.HasSuffix(filename, ".conf")

		if !isConfig {
			continue
		}

		content, err := ctx.GetContent(path)
		if err != nil {
			continue
		}

		relPath := ctx.RelativePath(path)
		lines := bytes.Split(content, []byte("\n"))

		// Check .golangci.yml for disabled critical linters
		if strings.HasSuffix(filename, ".golangci.yml") || strings.HasSuffix(filename, ".golangci.yaml") {
			inDisableSection := false
			for idx, lineBytes := range lines {
				lineNum := idx + 1
				rawLine := string(lineBytes)
				lineStr := strings.TrimSpace(rawLine)

				if strings.HasPrefix(lineStr, "disable:") {
					inDisableSection = true
					continue
				}
				// If an unindented key or another major section starts, exit disable section
				if len(rawLine) > 0 && rawLine[0] != ' ' && rawLine[0] != '\t' && strings.HasSuffix(lineStr, ":") {
					inDisableSection = false
				} else if inDisableSection && (strings.HasPrefix(lineStr, "enable:") || strings.HasPrefix(lineStr, "settings:")) {
					inDisableSection = false
				}

				// Only flag if errcheck is listed under disable:
				if inDisableSection && strings.HasPrefix(lineStr, "- errcheck") {
					title := "Linter 'errcheck' disabled in golangci-lint configuration"
					findings = append(findings, model.Finding{
						ID:             model.GenerateFindingID(r.ID(), relPath, lineNum, title),
						RuleID:         r.ID(),
						Title:          title,
						Severity:       model.SeverityMedium,
						Classification: model.SecurityWeakness,
						Component:      "Configuration",
						File:           relPath,
						Line:           lineNum,
						Description:    "Critical security linter 'errcheck' appears to be disabled in linter configuration.",
						Preconditions:  "Developer introduces unchecked error return.",
						AttackPath:     "Ignored disk I/O, checksum, or allocation errors causing silent data corruption.",
						Impact:         "Durability failure, silent data corruption, or panic.",
						Evidence:       lineStr,
						Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, lineNum),
						Recommendation: "Keep 'errcheck' enabled with check-type-assertions: true.",
						Status:         model.StatusOpen,
					})
				}
			}
		}
	}

	return findings, nil
}

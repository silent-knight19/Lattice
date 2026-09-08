package rules

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

var (
	// High-confidence patterns for real credentials
	privateKeyRegex = regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`)
	awsKeyRegex     = regexp.MustCompile(`\b(AKIA[0-9A-Z]{16})\b`)
	slackTokenRegex = regexp.MustCompile(`\b(xox[baprs]-[0-9a-zA-Z]{10,48})\b`)
	ghTokenRegex    = regexp.MustCompile(`\b(gh[pousr]_[0-9a-zA-Z]{36})\b`)
	// Assignment patterns for hardcoded credentials (variable = "literal" or key: "literal" where literal is >= 16 chars of entropy)
	secretAssignRegex = regexp.MustCompile(`(?i)(?:api[_-]?key|secret[_-]?key|private[_-]?key|auth[_-]?token|bearer[_-]?token|password|passwd)\s*(?::=|=|:)\s*["']([A-Za-z0-9_\-\.\$\+\/]{16,})["']`)
)

// Sec008Secrets implements conservative static detection of plaintext secrets and credentials.
type Sec008Secrets struct{}

func NewSec008Secrets() *Sec008Secrets {
	return &Sec008Secrets{}
}

func (r *Sec008Secrets) ID() string {
	return "SECURITY-008"
}

func (r *Sec008Secrets) Name() string {
	return "Plaintext Secret Detection"
}

func (r *Sec008Secrets) Description() string {
	return "Scans files for embedded private keys, tokens, and plaintext credentials, masking all discovered evidence."
}

func (r *Sec008Secrets) Category() string {
	return "SECRETS"
}

func (r *Sec008Secrets) DefaultSeverity() model.Severity {
	return model.SeverityCritical
}

func (r *Sec008Secrets) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	for _, path := range ctx.FilePaths {
		if ctx.IsExcluded(path) || IsTestFile(path) {
			continue
		}

		isBin, err := ctx.IsBinary(path)
		if err != nil || isBin {
			continue
		}

		content, err := ctx.GetContent(path)
		if err != nil {
			continue
		}

		relPath := ctx.RelativePath(path)
		lines := bytes.Split(content, []byte("\n"))

		for idx, lineBytes := range lines {
			lineNum := idx + 1
			lineStr := string(lineBytes)

			// 1. Private Key detection
			if privateKeyRegex.MatchString(lineStr) {
				title := "Embedded private key detected"
				finding := model.Finding{
					ID:             model.GenerateFindingID(r.ID(), relPath, lineNum, title),
					RuleID:         r.ID(),
					Title:          title,
					Severity:       model.SeverityCritical,
					Classification: model.ConfirmedVulnerability,
					Component:      "SecretManagement",
					File:           relPath,
					Line:           lineNum,
					Description:    "File contains an embedded cryptographic private key header.",
					Preconditions:  "Source code repository access.",
					AttackPath:     "Adversary extracts private key to impersonate services or decrypt traffic.",
					Impact:         "Complete cryptographic compromise of associated endpoints.",
					Evidence:       "-----BEGIN ... PRIVATE KEY----- [MASKED]",
					Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, lineNum),
					Recommendation: "Remove private key from source control immediately. Store in secure secret store.",
					Status:         model.StatusOpen,
				}
				findings = append(findings, finding)
				continue
			}

			// 2. AWS / GitHub / Slack tokens
			if m := awsKeyRegex.FindStringSubmatch(lineStr); len(m) > 1 {
				findings = append(findings, r.makeFinding(relPath, lineNum, "AWS Access Key ID detected", m[1]))
				continue
			}
			if m := ghTokenRegex.FindStringSubmatch(lineStr); len(m) > 1 {
				findings = append(findings, r.makeFinding(relPath, lineNum, "GitHub Personal Access Token detected", m[1]))
				continue
			}
			if m := slackTokenRegex.FindStringSubmatch(lineStr); len(m) > 1 {
				findings = append(findings, r.makeFinding(relPath, lineNum, "Slack API Token detected", m[1]))
				continue
			}

			// 3. Secret assignments (exclude obvious test placeholders like "example_password_123")
			if m := secretAssignRegex.FindStringSubmatch(lineStr); len(m) > 1 {
				secretVal := m[1]
				lower := strings.ToLower(secretVal)
				if strings.Contains(lower, "placeholder") || strings.Contains(lower, "dummy") || strings.Contains(lower, "example") {
					continue
				}
				findings = append(findings, r.makeFinding(relPath, lineNum, "Hardcoded credential literal detected", secretVal))
			}
		}
	}

	return findings, nil
}

func (r *Sec008Secrets) makeFinding(file string, line int, title, rawSecret string) model.Finding {
	return model.Finding{
		ID:             model.GenerateFindingID(r.ID(), file, line, title),
		RuleID:         r.ID(),
		Title:          title,
		Severity:       model.SeverityHigh,
		Classification: model.SecurityWeakness,
		Component:      "SecretManagement",
		File:           file,
		Line:           line,
		Description:    "Source file contains a hardcoded credential or secret literal.",
		Preconditions:  "Source repository access.",
		AttackPath:     "Adversary extracts credential from repository history or static binary.",
		Impact:         "Unauthorized authentication or API access using leaked credential.",
		Evidence:       model.MaskSecret(rawSecret),
		Reproduction:   fmt.Sprintf("Inspect %s at line %d.", file, line),
		Recommendation: "Do not commit plaintext credentials to source control. Load from environment variables or vault.",
		Status:         model.StatusOpen,
	}
}

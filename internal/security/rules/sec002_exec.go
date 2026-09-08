package rules

import (
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec002Exec checks for subprocess execution via "os/exec".
type Sec002Exec struct{}

func NewSec002Exec() *Sec002Exec {
	return &Sec002Exec{}
}

func (r *Sec002Exec) ID() string {
	return "SECURITY-002"
}

func (r *Sec002Exec) Name() string {
	return "Command and Subprocess Execution"
}

func (r *Sec002Exec) Description() string {
	return "Detects subprocess execution via 'os/exec', which introduces command injection and privilege escalation vectors."
}

func (r *Sec002Exec) Category() string {
	return "EXECUTION"
}

func (r *Sec002Exec) DefaultSeverity() model.Severity {
	return model.SeverityCritical
}

func (r *Sec002Exec) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	for _, path := range ctx.FilePaths {
		if !strings.HasSuffix(path, ".go") || ctx.IsExcluded(path) {
			continue
		}

		astFile, err := ctx.GetAST(path)
		if err != nil {
			continue
		}

		spec := FindImportSpec(astFile, "os/exec")
		if spec != nil {
			line := NodeLine(ctx.Fset, spec)
			relPath := ctx.RelativePath(path)
			title := "Import of 'os/exec' subprocess package detected"

			sev := model.SeverityCritical
			if IsTestFile(path) {
				sev = model.SeverityMedium
			}

			finding := model.Finding{
				ID:             model.GenerateFindingID(r.ID(), relPath, line, title),
				RuleID:         r.ID(),
				Title:          title,
				Severity:       sev,
				Classification: model.SecurityWeakness,
				Component:      "SubprocessExecution",
				File:           relPath,
				Line:           line,
				Description:    fmt.Sprintf("File %s imports 'os/exec'. Database engines must not invoke external shell commands.", relPath),
				Preconditions:  "Attacker controls command arguments or environment passed to exec.",
				AttackPath:     "Command injection, environment variable poisoning, or shell escaping leading to arbitrary command execution.",
				Impact:         "Host system compromise, arbitrary code execution, lateral movement.",
				Evidence:       "import \"os/exec\"",
				Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
				Recommendation: "Eliminate external command execution. Use pure Go library implementations.",
				Status:         model.StatusOpen,
			}
			findings = append(findings, finding)
		}
	}

	return findings, nil
}

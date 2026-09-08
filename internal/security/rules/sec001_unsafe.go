package rules

import (
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec001Unsafe checks for usage of the "unsafe" package.
type Sec001Unsafe struct{}

func NewSec001Unsafe() *Sec001Unsafe {
	return &Sec001Unsafe{}
}

func (r *Sec001Unsafe) ID() string {
	return "SECURITY-001"
}

func (r *Sec001Unsafe) Name() string {
	return "Unsafe Package Usage"
}

func (r *Sec001Unsafe) Description() string {
	return "Detects import and usage of the Go 'unsafe' package, which bypasses type safety and memory boundaries."
}

func (r *Sec001Unsafe) Category() string {
	return "MEMORY_SAFETY"
}

func (r *Sec001Unsafe) DefaultSeverity() model.Severity {
	return model.SeverityHigh
}

func (r *Sec001Unsafe) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	for _, path := range ctx.FilePaths {
		if !strings.HasSuffix(path, ".go") || ctx.IsExcluded(path) {
			continue
		}

		astFile, err := ctx.GetAST(path)
		if err != nil {
			continue // Parse errors are tracked in context
		}

		spec := FindImportSpec(astFile, "unsafe")
		if spec != nil {
			line := NodeLine(ctx.Fset, spec)
			relPath := ctx.RelativePath(path)
			title := "Import of 'unsafe' package detected"

			sev := model.SeverityHigh
			if IsTestFile(path) {
				sev = model.SeverityLow
			}

			finding := model.Finding{
				ID:             model.GenerateFindingID(r.ID(), relPath, line, title),
				RuleID:         r.ID(),
				Title:          title,
				Severity:       sev,
				Classification: model.SecurityWeakness,
				Component:      "MemorySafety",
				File:           relPath,
				Line:           line,
				Description:    fmt.Sprintf("File %s imports the 'unsafe' package. Unsafe pointers bypass Go memory and boundary protections.", relPath),
				Preconditions:  "Attacker triggers code path utilizing unsafe memory pointers.",
				AttackPath:     "Type confusion, out-of-bounds pointer arithmetic, or garbage collection memory corruption.",
				Impact:         "Memory safety violation, potential arbitrary read/write, or process crash.",
				Evidence:       "import \"unsafe\"",
				Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
				Recommendation: "Avoid 'unsafe' in core storage paths. Use standard slices and encoding primitives.",
				Status:         model.StatusOpen,
			}
			findings = append(findings, finding)
		}
	}

	return findings, nil
}

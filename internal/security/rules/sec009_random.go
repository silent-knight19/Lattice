package rules

import (
	"fmt"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec009Random checks for usage of math/rand in production code where crypto/rand is required.
type Sec009Random struct{}

func NewSec009Random() *Sec009Random {
	return &Sec009Random{}
}

func (r *Sec009Random) ID() string {
	return "SECURITY-009"
}

func (r *Sec009Random) Name() string {
	return "Cryptographically Weak Randomness"
}

func (r *Sec009Random) Description() string {
	return "Detects import of 'math/rand' instead of 'crypto/rand' in non-test production code."
}

func (r *Sec009Random) Category() string {
	return "CRYPTOGRAPHY"
}

func (r *Sec009Random) DefaultSeverity() model.Severity {
	return model.SeverityMedium
}

func (r *Sec009Random) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	for _, path := range ctx.FilePaths {
		if !strings.HasSuffix(path, ".go") || ctx.IsExcluded(path) {
			continue
		}

		// math/rand is acceptable in tests for benchmark generation
		if IsTestFile(path) {
			continue
		}

		astFile, err := ctx.GetAST(path)
		if err != nil {
			continue
		}

		spec := FindImportSpec(astFile, "math/rand")
		if spec != nil {
			line := NodeLine(ctx.Fset, spec)
			relPath := ctx.RelativePath(path)
			title := "Import of pseudo-random generator 'math/rand' in production code"

			finding := model.Finding{
				ID:             model.GenerateFindingID(r.ID(), relPath, line, title),
				RuleID:         r.ID(),
				Title:          title,
				Severity:       model.SeverityMedium,
				Classification: model.HardeningOpportunity,
				Component:      "Cryptography",
				File:           relPath,
				Line:           line,
				Description:    fmt.Sprintf("File %s imports 'math/rand'. For cryptographic tokens, session keys, and election nonces, 'crypto/rand' must be used.", relPath),
				Preconditions:  "Attacker observes pseudo-random sequence to predict future values.",
				AttackPath:     "Predictable state transitions, nonce reuse, or session hijacking.",
				Impact:         "Loss of cryptographic unpredictability.",
				Evidence:       "import \"math/rand\"",
				Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
				Recommendation: "Use 'crypto/rand' for security-sensitive entropy, or document algorithmic pseudo-randomness (e.g. SkipList level generation).",
				Status:         model.StatusOpen,
			}
			findings = append(findings, finding)
		}
	}

	return findings, nil
}

package rules

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec010Logging checks for direct, unredacted logging of sensitive variables (e.g., password, secret).
type Sec010Logging struct{}

func NewSec010Logging() *Sec010Logging {
	return &Sec010Logging{}
}

func (r *Sec010Logging) ID() string {
	return "SECURITY-010"
}

func (r *Sec010Logging) Name() string {
	return "Sensitive Data Logging"
}

func (r *Sec010Logging) Description() string {
	return "Detects potential logging of sensitive variables (password, secret, private_key) without redaction."
}

func (r *Sec010Logging) Category() string {
	return "INFORMATION_DISCLOSURE"
}

func (r *Sec010Logging) DefaultSeverity() model.Severity {
	return model.SeverityMedium
}

func (r *Sec010Logging) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	sensitiveStems := []string{"password", "passwd", "secret", "privatekey", "bearer_token", "apikey"}

	for _, path := range ctx.FilePaths {
		if !strings.HasSuffix(path, ".go") || ctx.IsExcluded(path) {
			continue
		}

		// Skip logger package itself (which defines redaction mechanisms) and test files
		if strings.Contains(path, "internal/logger") || IsTestFile(path) {
			continue
		}

		astFile, err := ctx.GetAST(path)
		if err != nil {
			continue
		}

		relPath := ctx.RelativePath(path)

		ast.Inspect(astFile, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			pkgName, funcName := GetCallPackageAndFunc(call)
			isLoggingCall := (pkgName == "log" || pkgName == "slog" || pkgName == "fmt") &&
				(funcName == "Print" || funcName == "Println" || funcName == "Printf" || funcName == "Info" || funcName == "Warn" || funcName == "Error")

			if !isLoggingCall {
				return true
			}

			for _, arg := range call.Args {
				if ident, ok := arg.(*ast.Ident); ok {
					identLower := strings.ToLower(strings.ReplaceAll(ident.Name, "_", ""))
					for _, stem := range sensitiveStems {
						if strings.Contains(identLower, stem) {
							line := NodeLine(ctx.Fset, arg)
							title := fmt.Sprintf("Sensitive variable %s passed to %s.%s", ident.Name, pkgName, funcName)

							finding := model.Finding{
								ID:             model.GenerateFindingID(r.ID(), relPath, line, title),
								RuleID:         r.ID(),
								Title:          title,
								Severity:       model.SeverityMedium,
								Classification: model.SecurityWeakness,
								Component:      "Logging",
								File:           relPath,
								Line:           line,
								Description:    fmt.Sprintf("Logging call %s.%s appears to log sensitive variable %s directly.", pkgName, funcName, ident.Name),
								Preconditions:  "Operator or attacker has access to application standard output or log streams.",
								AttackPath:     "Information disclosure through persistent log files or monitoring collectors.",
								Impact:         "Credential leakage and confidentiality compromise.",
								Evidence:       fmt.Sprintf("%s.%s(..., %s)", pkgName, funcName, ident.Name),
								Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
								Recommendation: "Use Lattice's internal/logger with Redactable or masked attributes.",
								Status:         model.StatusOpen,
							}
							findings = append(findings, finding)
						}
					}
				}
			}

			return true
		})
	}

	return findings, nil
}

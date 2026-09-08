package rules

import (
	"fmt"
	"go/ast"
	"strconv"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec004Perms checks for overly permissive filesystem mode literals (e.g. 0777, 0666).
type Sec004Perms struct{}

func NewSec004Perms() *Sec004Perms {
	return &Sec004Perms{}
}

func (r *Sec004Perms) ID() string {
	return "SECURITY-004"
}

func (r *Sec004Perms) Name() string {
	return "Risky Filesystem Permissions"
}

func (r *Sec004Perms) Description() string {
	return "Detects creation or modification of files/directories with world-writable or overly permissive modes (0777, 0666)."
}

func (r *Sec004Perms) Category() string {
	return "PRIVILEGE"
}

func (r *Sec004Perms) DefaultSeverity() model.Severity {
	return model.SeverityHigh
}

func (r *Sec004Perms) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	for _, path := range ctx.FilePaths {
		if !strings.HasSuffix(path, ".go") || ctx.IsExcluded(path) || IsTestFile(path) {
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
			if pkgName != "os" {
				return true
			}

			// Functions taking permission modes: OpenFile, Mkdir, MkdirAll, Chmod, WriteFile
			var permArg ast.Expr
			switch funcName {
			case "OpenFile":
				if len(call.Args) >= 3 {
					permArg = call.Args[2]
				}
			case "Mkdir", "MkdirAll", "Chmod":
				if len(call.Args) >= 2 {
					permArg = call.Args[1]
				}
			case "WriteFile":
				if len(call.Args) >= 3 {
					permArg = call.Args[2]
				}
			}

			if permArg == nil {
				return true
			}

			if lit, ok := permArg.(*ast.BasicLit); ok && strings.HasPrefix(lit.Value, "0") {
				val, err := strconv.ParseUint(lit.Value, 8, 32)
				if err == nil {
					// 0777 or 0666 or world-writable (other write bit set: val & 0002 != 0)
					if val == 0777 || val == 0666 || (val&0002 != 0) {
						line := NodeLine(ctx.Fset, lit)
						title := fmt.Sprintf("Overly permissive filesystem mode %s in %s.%s", lit.Value, pkgName, funcName)

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
							Component:      "FilesystemSecurity",
							File:           relPath,
							Line:           line,
							Description:    fmt.Sprintf("Call to %s.%s specifies world-writable permission %s. Files containing database logs or keys must use 0700 or 0600.", pkgName, funcName, lit.Value),
							Preconditions:  "Multi-tenant or unprivileged local user has access to host filesystem.",
							AttackPath:     "Local unprivileged user reads customer data or tampers with database files.",
							Impact:         "Confidentiality and integrity compromise of database persistent storage.",
							Evidence:       fmt.Sprintf("%s.%s(..., %s)", pkgName, funcName, lit.Value),
							Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
							Recommendation: "Enforce restrictive permissions: 0700 for directories, 0600 for log/data files.",
							Status:         model.StatusOpen,
						}
						findings = append(findings, finding)
					}
				}
			}

			return true
		})
	}

	return findings, nil
}

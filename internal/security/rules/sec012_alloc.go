package rules

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec012Alloc checks for unbounded slice allocations based on external integer lengths.
type Sec012Alloc struct{}

func NewSec012Alloc() *Sec012Alloc {
	return &Sec012Alloc{}
}

func (r *Sec012Alloc) ID() string {
	return "SECURITY-012"
}

func (r *Sec012Alloc) Name() string {
	return "Unbounded Allocation Based on External Length"
}

func (r *Sec012Alloc) Description() string {
	return "Detects make([]byte, length) calls where length is taken from stream input without defensive bounds checking."
}

func (r *Sec012Alloc) Category() string {
	return "RESOURCE_EXHAUSTION"
}

func (r *Sec012Alloc) DefaultSeverity() model.Severity {
	return model.SeverityHigh
}

func (r *Sec012Alloc) Run(ctx *model.AuditContext) ([]model.Finding, error) {
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
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}

			// Look for functions taking io.Reader or []byte
			takesStream := false
			if fn.Type.Params != nil {
				for _, p := range fn.Type.Params.List {
					typeStr := fmt.Sprintf("%v", p.Type)
					if strings.Contains(typeStr, "Reader") || strings.Contains(typeStr, "[]byte") {
						takesStream = true
						break
					}
				}
			}

			if !takesStream {
				return true
			}

			// Check for make([]byte, length) with bounds check in the function body
			hasMaxCheck := false
			ast.Inspect(fn.Body, func(b ast.Node) bool {
				if ifStmt, isIf := b.(*ast.IfStmt); isIf {
					ast.Inspect(ifStmt.Cond, func(condNode ast.Node) bool {
						if ident, ok := condNode.(*ast.Ident); ok {
							nameLower := strings.ToLower(ident.Name)
							if strings.Contains(nameLower, "max") || strings.Contains(nameLower, "limit") || strings.Contains(nameLower, "bound") {
								hasMaxCheck = true
								return false
							}
						}
						return true
					})
				}
				return true
			})

			if hasMaxCheck {
				// Function properly includes bounds checks
				return true
			}

			// If no bounds check exists, look for make([]byte, non-constant)
			ast.Inspect(fn.Body, func(b ast.Node) bool {
				call, isCall := b.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == "make" {
					if len(call.Args) >= 2 {
						// Check if 1st arg is []byte
						if sliceType, isSlice := call.Args[0].(*ast.ArrayType); isSlice {
							if eltIdent, isElt := sliceType.Elt.(*ast.Ident); isElt && eltIdent.Name == "byte" {
								// Check if 2nd arg is variable (not constant basic lit)
								if _, isLit := call.Args[1].(*ast.BasicLit); !isLit {
									line := NodeLine(ctx.Fset, call)
									title := fmt.Sprintf("Unbounded slice allocation in %s", fn.Name.Name)

									finding := model.Finding{
										ID:             model.GenerateFindingID(r.ID(), relPath, line, title),
										RuleID:         r.ID(),
										Title:          title,
										Severity:       model.SeverityHigh,
										Classification: model.SecurityWeakness,
										Component:      "ResourceManagement",
										File:           relPath,
										Line:           line,
										Description:    fmt.Sprintf("Function %s allocates a slice with variable capacity without apparent maximum bounds validation.", fn.Name.Name),
										Preconditions:  "Attacker provides crafted length field in input stream.",
										AttackPath:     "Attacker supplies MaxUint32 length, triggering massive allocation and out-of-memory crash (OOM DoS).",
										Impact:         "Process termination via Linux OOM killer, denial of service.",
										Evidence:       fmt.Sprintf("make([]byte, %v)", call.Args[1]),
										Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
										Recommendation: "Validate integer lengths against explicit ceilings (e.g. MaxKeyLen, MaxValueLen) before allocation.",
										Status:         model.StatusOpen,
									}
									findings = append(findings, finding)
								}
							}
						}
					}
				}
				return true
			})

			return true
		})
	}

	return findings, nil
}

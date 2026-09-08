package rules

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/silent-knight19/lattice/internal/security/model"
)

// Sec011Panic checks for panic calls inside input parsing/decoding functions.
type Sec011Panic struct{}

func NewSec011Panic() *Sec011Panic {
	return &Sec011Panic{}
}

func (r *Sec011Panic) ID() string {
	return "SECURITY-011"
}

func (r *Sec011Panic) Name() string {
	return "Panic on Parsing/Decoding Path"
}

func (r *Sec011Panic) Description() string {
	return "Detects explicit panic() calls inside parser or deserializer functions handling external input."
}

func (r *Sec011Panic) Category() string {
	return "AVAILABILITY"
}

func (r *Sec011Panic) DefaultSeverity() model.Severity {
	return model.SeverityHigh
}

func (r *Sec011Panic) Run(ctx *model.AuditContext) ([]model.Finding, error) {
	var findings []model.Finding

	parsingPrefixes := []string{"Decode", "Parse", "Unmarshal", "ReadRecord", "GetVarint"}

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

			fnName := fn.Name.Name
			isParsingFunc := false
			for _, prefix := range parsingPrefixes {
				if strings.HasPrefix(fnName, prefix) {
					isParsingFunc = true
					break
				}
			}

			if !isParsingFunc {
				return true
			}

			// Inspect inside function body for panic calls
			ast.Inspect(fn.Body, func(bodyNode ast.Node) bool {
				call, isCall := bodyNode.(*ast.CallExpr)
				if !isCall {
					return true
				}
				if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == "panic" {
					line := NodeLine(ctx.Fset, call)
					title := fmt.Sprintf("panic() in parsing function %s", fnName)

					finding := model.Finding{
						ID:             model.GenerateFindingID(r.ID(), relPath, line, title),
						RuleID:         r.ID(),
						Title:          title,
						Severity:       model.SeverityHigh,
						Classification: model.SecurityWeakness,
						Component:      "InputParsing",
						File:           relPath,
						Line:           line,
						Description:    fmt.Sprintf("Function %s invokes panic() on a data decoding path. Parsers handling external or disk input must return structured errors.", fnName),
						Preconditions:  "Attacker delivers malformed record or network payload.",
						AttackPath:     "Malformed input triggers panic(), causing immediate process termination (Denial of Service).",
						Impact:         "Process crash, database unavailability.",
						Evidence:       fmt.Sprintf("func %s(...) { ... panic(...) }", fnName),
						Reproduction:   fmt.Sprintf("Inspect %s at line %d.", relPath, line),
						Recommendation: "Return structured domain error (*errors.CorruptRecordError, etc.) instead of panicking.",
						Status:         model.StatusOpen,
					}
					findings = append(findings, finding)
				}
				return true
			})

			return true
		})
	}

	return findings, nil
}

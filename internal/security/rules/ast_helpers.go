package rules

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// HasImport checks whether the given AST file imports the specified package path.
func HasImport(file *ast.File, importPath string) bool {
	for _, imp := range file.Imports {
		if imp.Path != nil {
			path, err := strconv.Unquote(imp.Path.Value)
			if err == nil && path == importPath {
				return true
			}
		}
	}
	return false
}

// FindImportSpec returns the ImportSpec for a given package if it exists.
func FindImportSpec(file *ast.File, importPath string) *ast.ImportSpec {
	for _, imp := range file.Imports {
		if imp.Path != nil {
			path, err := strconv.Unquote(imp.Path.Value)
			if err == nil && path == importPath {
				return imp
			}
		}
	}
	return nil
}

// IsTestFile reports whether a file path is a Go test file.
func IsTestFile(path string) bool {
	return strings.HasSuffix(path, "_test.go")
}

// GetCallPackageAndFunc extracts the package and function name from a CallExpr if it is a selector call (pkg.Func).
func GetCallPackageAndFunc(call *ast.CallExpr) (pkgName, funcName string) {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if ident, ok := sel.X.(*ast.Ident); ok {
			return ident.Name, sel.Sel.Name
		}
	}
	return "", ""
}

// NodeLine returns the 1-based line number for an AST node in the FileSet.
func NodeLine(fset *token.FileSet, node ast.Node) int {
	if node == nil || fset == nil {
		return 1
	}
	pos := fset.Position(node.Pos())
	if pos.Line <= 0 {
		return 1
	}
	return pos.Line
}

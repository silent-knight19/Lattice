package model

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultExclusionDirs defines repository directories excluded from static AST analysis
// and secret scanning to prevent false positives and infinite loops.
var DefaultExclusionDirs = []string{
	".git",
	".itehaas",
	"bin",
	"dist",
	"vendor",
}

// AuditContext encapsulates the repository environment, files under analysis,
// single-pass AST caches, and exclusion policies for security rules.
type AuditContext struct {
	RootDir     string
	FilePaths   []string
	Exclusions  []string
	Suppression *SuppressionManager

	mu           sync.RWMutex
	Fset         *token.FileSet
	parsedFiles  map[string]*ast.File
	fileContents map[string][]byte
	errors       map[string]error
}

// NewAuditContext initializes an AuditContext with empty caches.
func NewAuditContext(rootDir string, filePaths []string) *AuditContext {
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		absRoot = rootDir
	}

	return &AuditContext{
		RootDir:      absRoot,
		FilePaths:    filePaths,
		Exclusions:   DefaultExclusionDirs,
		Suppression:  NewSuppressionManager(),
		Fset:         token.NewFileSet(),
		parsedFiles:  make(map[string]*ast.File),
		fileContents: make(map[string][]byte),
		errors:       make(map[string]error),
	}
}

// IsExcluded reports whether a given file path should be skipped according to exclusion policies.
func (c *AuditContext) IsExcluded(path string) bool {
	norm := filepath.ToSlash(path)
	parts := strings.Split(norm, "/")

	for _, part := range parts {
		for _, excl := range c.Exclusions {
			if part == excl {
				return true
			}
		}
	}
	return false
}

// RelativePath returns the file path relative to the context's RootDir.
func (c *AuditContext) RelativePath(path string) string {
	rel, err := filepath.Rel(c.RootDir, path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.ToSlash(rel)
}

// GetContent reads and caches the raw byte content of a file.
func (c *AuditContext) GetContent(path string) ([]byte, error) {
	c.mu.RLock()
	if content, exists := c.fileContents[path]; exists {
		c.mu.RUnlock()
		return content, nil
	}
	if err, exists := c.errors[path]; exists {
		c.mu.RUnlock()
		return nil, err
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double check
	if content, exists := c.fileContents[path]; exists {
		return content, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		c.errors[path] = err
		return nil, err
	}

	c.fileContents[path] = content
	return content, nil
}

// GetAST parses and caches the AST for a Go source file.
// If the file is not valid Go source or is unreadable, returns the parsing error safely without panicking.
func (c *AuditContext) GetAST(path string) (*ast.File, error) {
	c.mu.RLock()
	if node, exists := c.parsedFiles[path]; exists {
		c.mu.RUnlock()
		return node, nil
	}
	if err, exists := c.errors[path]; exists {
		c.mu.RUnlock()
		return nil, err
	}
	c.mu.RUnlock()

	content, err := c.GetContent(path)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double check
	if node, exists := c.parsedFiles[path]; exists {
		return node, nil
	}

	node, err := parser.ParseFile(c.Fset, path, content, parser.ParseComments)
	if err != nil {
		c.errors[path] = err
		return nil, err
	}

	c.parsedFiles[path] = node
	return node, nil
}

// IsBinary detects whether content appears to be binary by checking for null bytes.
func (c *AuditContext) IsBinary(path string) (bool, error) {
	content, err := c.GetContent(path)
	if err != nil {
		return false, err
	}
	limit := len(content)
	if limit > 1024 {
		limit = 1024
	}
	return bytes.IndexByte(content[:limit], 0x00) != -1, nil
}

// Errors returns a map of all file loading/parsing errors encountered during analysis.
func (c *AuditContext) Errors() map[string]error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]error, len(c.errors))
	for k, v := range c.errors {
		out[k] = v
	}
	return out
}

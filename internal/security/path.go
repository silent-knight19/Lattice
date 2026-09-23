package security

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/silent-knight19/lattice/internal/errors"
)

var (
	// dbFileNameRegex enforces strict alphanumeric, underscore, hyphen, and period characters
	// for database filenames (wal_*.log, *.sst, MANIFEST-*, CURRENT, raft_state, etc.).
	// Disallows directory separators, traversal tokens, spaces, and shell metacharacters.
	dbFileNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
)

// ValidateDatabaseFileName verifies that name is a pure filename (not a path),
// contains no directory separators, null bytes, or traversal sequences,
// and strictly conforms to the allowed character set [a-zA-Z0-9_.-].
func ValidateDatabaseFileName(name string) error {
	if name == "" {
		return &errors.InvalidPathError{
			Path:   name,
			Reason: "filename cannot be empty",
		}
	}
	if len(name) > 255 {
		return &errors.InvalidPathError{
			Path:   name,
			Reason: "filename exceeds maximum length of 255 bytes",
		}
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return &errors.InvalidPathError{
			Path:   name,
			Reason: "filename contains directory separators or null bytes",
		}
	}
	if name == "." || name == ".." {
		return &errors.InvalidPathError{
			Path:   name,
			Reason: "filename cannot be relative traversal token",
		}
	}
	if !dbFileNameRegex.MatchString(name) {
		return &errors.InvalidPathError{
			Path:   name,
			Reason: fmt.Sprintf("filename %q contains unpermitted characters (allowed: [a-zA-Z0-9_.-])", name),
		}
	}
	return nil
}

// CleanAndValidatePath verifies that path is non-empty, contains no null bytes,
// and returns the lexically cleaned path.
func CleanAndValidatePath(path string) (string, error) {
	if path == "" {
		return "", &errors.InvalidPathError{
			Path:   path,
			Reason: "path cannot be empty",
		}
	}
	if strings.ContainsRune(path, 0) {
		return "", &errors.InvalidPathError{
			Path:   path,
			Reason: "path contains null byte",
		}
	}
	return filepath.Clean(path), nil
}

// ValidateContainment verifies that targetPath is strictly contained within rootDir (or equals rootDir).
// It defends against lexical directory traversal (..), sibling-directory prefix confusion (/root vs /root-evil),
// absolute-path escapes, volume/UNC mismatches, and symlink escapes where ancestor directories exist.
func ValidateContainment(rootDir, targetPath string) error {
	cleanRoot, err := CleanAndValidatePath(rootDir)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   rootDir,
			Root:   rootDir,
			Reason: fmt.Sprintf("invalid root directory: %v", err),
		}
	}

	cleanTarget, err := CleanAndValidatePath(targetPath)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   cleanRoot,
			Reason: fmt.Sprintf("invalid target path: %v", err),
		}
	}

	absRoot, err := filepath.Abs(cleanRoot)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   cleanTarget,
			Root:   cleanRoot,
			Reason: fmt.Sprintf("failed to resolve absolute root: %v", err),
		}
	}

	absTarget, err := filepath.Abs(cleanTarget)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   cleanTarget,
			Root:   cleanRoot,
			Reason: fmt.Sprintf("failed to resolve absolute target: %v", err),
		}
	}

	// Windows Volume/Drive check
	if filepath.VolumeName(absRoot) != filepath.VolumeName(absTarget) {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: fmt.Sprintf("target volume %q does not match root volume %q", filepath.VolumeName(absTarget), filepath.VolumeName(absRoot)),
		}
	}

	// Lexical Containment Check
	// If absRoot is the filesystem root ("/" or "C:\"), all paths on that volume are within it.
	rootIsVolumeRoot := absRoot == string(filepath.Separator) || (filepath.VolumeName(absRoot) != "" && absRoot == filepath.VolumeName(absRoot)+string(filepath.Separator))

	if !rootIsVolumeRoot {
		// Must either equal absRoot or start with absRoot + separator (defends against sibling confusion)
		sep := string(filepath.Separator)
		if absTarget != absRoot && !strings.HasPrefix(absTarget, absRoot+sep) {
			return &errors.InvalidPathError{
				Path:   targetPath,
				Root:   rootDir,
				Reason: "path escapes security root (prefix boundary mismatch)",
			}
		}
	}

	// Relative path check via filepath.Rel
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: fmt.Sprintf("failed to compute relative path: %v", err),
		}
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: "path escapes security root via relative traversal",
		}
	}

	// Canonical symlink containment evaluation:
	// Canonicalize both root and target through their deepest existing ancestors.
	// This guarantees fail-closed containment even when rootDir or targetPath (or subdirectories)
	// do not yet exist on disk, closing symlink escape windows across uncreated parent hierarchies.
	canonicalRoot, err := evalDeepestExistingAncestor(absRoot)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: fmt.Sprintf("failed to canonicalize security root %q: %v", absRoot, err),
		}
	}

	canonicalTarget, err := evalDeepestExistingAncestor(absTarget)
	if err != nil {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: fmt.Sprintf("failed to canonicalize target path %q: %v", absTarget, err),
		}
	}

	// Canonical Volume check
	if filepath.VolumeName(canonicalRoot) != filepath.VolumeName(canonicalTarget) {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: fmt.Sprintf("canonical target volume %q does not match root volume %q", filepath.VolumeName(canonicalTarget), filepath.VolumeName(canonicalRoot)),
		}
	}

	// Canonical Lexical Containment Check
	canonicalRootIsVolRoot := canonicalRoot == string(filepath.Separator) || (filepath.VolumeName(canonicalRoot) != "" && canonicalRoot == filepath.VolumeName(canonicalRoot)+string(filepath.Separator))

	if !canonicalRootIsVolRoot {
		sep := string(filepath.Separator)
		if canonicalTarget != canonicalRoot && !strings.HasPrefix(canonicalTarget, canonicalRoot+sep) {
			return &errors.InvalidPathError{
				Path:   targetPath,
				Root:   rootDir,
				Reason: fmt.Sprintf("canonical target path %q escapes canonical root %q via symbolic link", canonicalTarget, canonicalRoot),
			}
		}
	}

	// Canonical Relative path check
	canonicalRel, err := filepath.Rel(canonicalRoot, canonicalTarget)
	if err != nil || canonicalRel == ".." || strings.HasPrefix(canonicalRel, ".."+string(filepath.Separator)) {
		return &errors.InvalidPathError{
			Path:   targetPath,
			Root:   rootDir,
			Reason: fmt.Sprintf("canonical target path %q escapes canonical root %q via symbolic link", canonicalTarget, canonicalRoot),
		}
	}

	return nil
}

// ResolvePath resolves userPath relative to rootDir and verifies strict containment.
// If userPath is relative, it is joined to rootDir; if userPath is absolute, it must reside within rootDir.
// Returns the cleaned target path or an error wrapping ErrInvalidPath.
func ResolvePath(rootDir, userPath string) (string, error) {
	if rootDir == "" {
		return "", &errors.InvalidPathError{Path: userPath, Root: rootDir, Reason: "root directory cannot be empty"}
	}
	if userPath == "" {
		return "", &errors.InvalidPathError{Path: userPath, Root: rootDir, Reason: "target path cannot be empty"}
	}
	if strings.ContainsRune(userPath, 0) || strings.ContainsRune(rootDir, 0) {
		return "", &errors.InvalidPathError{Path: userPath, Root: rootDir, Reason: "path contains null byte"}
	}

	cleanUser := filepath.Clean(userPath)
	var candidate string
	if filepath.IsAbs(cleanUser) {
		candidate = cleanUser
	} else {
		candidate = filepath.Join(rootDir, cleanUser)
	}

	if err := ValidateContainment(rootDir, candidate); err != nil {
		return "", err
	}
	return filepath.Clean(candidate), nil
}

// SanitizePath is an alias for ResolvePath.
func SanitizePath(rootDir, userPath string) (string, error) {
	return ResolvePath(rootDir, userPath)
}

// evalDeepestExistingAncestor evaluates symlinks on the deepest existing ancestor of absPath
// and reassembles the canonical path with any non-existent trailing components.
// This guarantees that canonical containment can be proven even when rootDir or targetPath
// (or their intermediate subdirectories) do not yet exist on disk.
func evalDeepestExistingAncestor(absPath string) (string, error) {
	eval, err := filepath.EvalSymlinks(absPath)
	if err == nil {
		return eval, nil
	}

	var missing []string
	cur := absPath
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached filesystem root or volume root
			eval, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			canonical := eval
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		}
		missing = append(missing, filepath.Base(cur))
		eval, err := filepath.EvalSymlinks(parent)
		if err == nil {
			canonical := eval
			for i := len(missing) - 1; i >= 0; i-- {
				canonical = filepath.Join(canonical, missing[i])
			}
			return canonical, nil
		}
		cur = parent
	}
}

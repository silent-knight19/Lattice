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

	// Symlink containment check for existing paths or existing parent directories
	// If root exists, resolve its canonical path
	evalRoot, err := filepath.EvalSymlinks(absRoot)
	if err == nil {
		// Walk up target path to find the deepest existing ancestor
		cur := absTarget
		for {
			evalTarget, err := filepath.EvalSymlinks(cur)
			if err == nil {
				// We found an existing ancestor. Verify it is contained in evalRoot.
				evalRootIsVolRoot := evalRoot == string(filepath.Separator) || (filepath.VolumeName(evalRoot) != "" && evalRoot == filepath.VolumeName(evalRoot)+string(filepath.Separator))
				if !evalRootIsVolRoot {
					sep := string(filepath.Separator)
					if evalTarget != evalRoot && !strings.HasPrefix(evalTarget, evalRoot+sep) {
						return &errors.InvalidPathError{
							Path:   targetPath,
							Root:   rootDir,
							Reason: fmt.Sprintf("canonical target path %q escapes canonical root %q via symbolic link", evalTarget, evalRoot),
						}
					}
				}
				evalRel, relErr := filepath.Rel(evalRoot, evalTarget)
				if relErr != nil || evalRel == ".." || strings.HasPrefix(evalRel, ".."+string(filepath.Separator)) {
					return &errors.InvalidPathError{
						Path:   targetPath,
						Root:   rootDir,
						Reason: fmt.Sprintf("canonical target path %q escapes canonical root %q via symbolic link", evalTarget, evalRoot),
					}
				}
				break
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				break
			}
			cur = parent
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

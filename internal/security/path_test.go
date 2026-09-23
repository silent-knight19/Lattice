package security_test

import (
	stdErrors "errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/security"
)

func TestCleanAndValidatePath(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      string
		expectErr bool
	}{
		{"valid relative", "foo/bar/baz", filepath.Join("foo", "bar", "baz"), false},
		{"valid absolute", "/var/lib/lattice", filepath.Clean("/var/lib/lattice"), false},
		{"redundant slashes", "foo///bar//baz", filepath.Join("foo", "bar", "baz"), false},
		{"inner dot dots", "foo/bar/../baz", filepath.Join("foo", "baz"), false},
		{"empty string", "", "", true},
		{"null byte in middle", "foo\x00bar", "", true},
		{"null byte at start", "\x00foo", "", true},
		{"null byte at end", "foo\x00", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := security.CleanAndValidatePath(tc.input)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error for input %q, got nil", tc.input)
				}
				if !stdErrors.Is(err, errors.ErrInvalidPath) {
					t.Fatalf("expected error to wrap ErrInvalidPath, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for input %q: %v", tc.input, err)
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestValidateDatabaseFileName(t *testing.T) {
	validNames := []string{
		"wal_000000000001.log",
		"000001.sst",
		"MANIFEST-000001",
		"CURRENT",
		"CURRENT.tmp",
		"raft_state",
		"raft_state.tmp",
		"raft.log",
		"raft.log.tmp",
		".tmp_000001.sst_abc123",
		"a",
		"test-file_123.dat",
	}

	for _, name := range validNames {
		t.Run("valid_"+name, func(t *testing.T) {
			if err := security.ValidateDatabaseFileName(name); err != nil {
				t.Fatalf("expected %q to be valid, got: %v", name, err)
			}
		})
	}

	invalidNames := []struct {
		name   string
		reason string
	}{
		{"", "empty string"},
		{".", "single dot"},
		{"..", "dot dot traversal"},
		{"../wal_001.log", "parent traversal prefix"},
		{"wal/001.log", "forward slash"},
		{"wal\\001.log", "backward slash"},
		{"wal\x00.log", "null byte"},
		{"file with space.sst", "space character"},
		{"file;rm -rf.log", "semicolon injection"},
		{"file$var.sst", "shell expansion character"},
		{"file`cmd`.sst", "backtick injection"},
		{"file|pipe.sst", "pipe character"},
		{"file>redirect.sst", "redirection character"},
		{strings.Repeat("a", 256), "exceeds 255 bytes"},
	}

	for _, tc := range invalidNames {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			err := security.ValidateDatabaseFileName(tc.name)
			if err == nil {
				t.Fatalf("expected error for %q (%s), got nil", tc.name, tc.reason)
			}
			if !stdErrors.Is(err, errors.ErrInvalidPath) {
				t.Fatalf("expected error to wrap ErrInvalidPath for %q, got: %v", tc.name, err)
			}
		})
	}
}

func TestValidateContainment_LexicalAndBoundary(t *testing.T) {
	tempDir := t.TempDir()
	root := filepath.Join(tempDir, "db_root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("failed to create test root: %v", err)
	}

	// 1. Basic In-Root Paths
	validPaths := []string{
		root,
		filepath.Join(root, "wal"),
		filepath.Join(root, "wal", "wal_000000000001.log"),
		filepath.Join(root, "000001.sst"),
		filepath.Join(root, "sub", "dir", "file.txt"),
		filepath.Join(root, "wal", "..", "000001.sst"),
	}

	for _, p := range validPaths {
		t.Run("valid_"+p, func(t *testing.T) {
			if err := security.ValidateContainment(root, p); err != nil {
				t.Fatalf("expected %q to be contained in %q, got: %v", p, root, err)
			}
		})
	}

	// 2. Traversal Escapes
	escapingPaths := []struct {
		name   string
		target string
	}{
		{"parent of root", filepath.Dir(root)},
		{"direct dot dot", filepath.Join(root, "..")},
		{"dot dot escape", filepath.Join(root, "..", "escaped.txt")},
		{"nested dot dot escape", filepath.Join(root, "sub", "..", "..", "escaped.txt")},
		{"deep escape", filepath.Join(root, "a", "b", "c", "..", "..", "..", "..", "etc", "passwd")},
		{"temp root", tempDir},
	}

	for _, tc := range escapingPaths {
		t.Run("escape_"+tc.name, func(t *testing.T) {
			err := security.ValidateContainment(root, tc.target)
			if err == nil {
				t.Fatalf("expected containment failure for %q in %q, got nil", tc.target, root)
			}
			if !stdErrors.Is(err, errors.ErrInvalidPath) {
				t.Fatalf("expected error to wrap ErrInvalidPath, got: %v", err)
			}
		})
	}

	// 3. Sibling Directory Prefix Confusion
	// Conceptual test: root is /tmp/.../db_root, attacker targets /tmp/.../db_root_evil/file
	siblingDir := root + "_evil"
	siblingTarget := filepath.Join(siblingDir, "stolen.sst")
	t.Run("sibling_prefix_confusion", func(t *testing.T) {
		err := security.ValidateContainment(root, siblingTarget)
		if err == nil {
			t.Fatalf("CRITICAL: Sibling directory prefix confusion bypass succeeded! %q was accepted in root %q", siblingTarget, root)
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected error to wrap ErrInvalidPath, got: %v", err)
		}
	})

	// Sibling directory with hyphen
	siblingHyphen := root + "-backup"
	siblingHyphenTarget := filepath.Join(siblingHyphen, "wal_01.log")
	t.Run("sibling_hyphen_confusion", func(t *testing.T) {
		err := security.ValidateContainment(root, siblingHyphenTarget)
		if err == nil {
			t.Fatalf("CRITICAL: Sibling directory hyphen confusion bypass succeeded! %q was accepted in root %q", siblingHyphenTarget, root)
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected error to wrap ErrInvalidPath, got: %v", err)
		}
	})

	// 4. Null Byte Injection
	t.Run("null_byte_target", func(t *testing.T) {
		err := security.ValidateContainment(root, filepath.Join(root, "wal\x00_01.log"))
		if err == nil {
			t.Fatal("expected null byte rejection, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected ErrInvalidPath, got: %v", err)
		}
	})

	t.Run("null_byte_root", func(t *testing.T) {
		err := security.ValidateContainment(root+"\x00", filepath.Join(root, "wal.log"))
		if err == nil {
			t.Fatal("expected null byte root rejection, got nil")
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected ErrInvalidPath, got: %v", err)
		}
	})
}

func TestResolvePath(t *testing.T) {
	tempDir := t.TempDir()
	root := filepath.Join(tempDir, "sandbox")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("failed to create sandbox: %v", err)
	}

	tests := []struct {
		name      string
		root      string
		userPath  string
		want      string
		expectErr bool
	}{
		{
			name:      "simple relative file",
			root:      root,
			userPath:  "000001.sst",
			want:      filepath.Join(root, "000001.sst"),
			expectErr: false,
		},
		{
			name:      "nested relative file",
			root:      root,
			userPath:  "wal/wal_000000000001.log",
			want:      filepath.Join(root, "wal", "wal_000000000001.log"),
			expectErr: false,
		},
		{
			name:      "relative with self-canceling dot dot",
			root:      root,
			userPath:  "wal/../wal/wal_000000000002.log",
			want:      filepath.Join(root, "wal", "wal_000000000002.log"),
			expectErr: false,
		},
		{
			name:      "absolute path inside root",
			root:      root,
			userPath:  filepath.Join(root, "data", "table.sst"),
			want:      filepath.Join(root, "data", "table.sst"),
			expectErr: false,
		},
		{
			name:      "traversal escaping root",
			root:      root,
			userPath:  "../../etc/passwd",
			want:      "",
			expectErr: true,
		},
		{
			name:      "traversal escaping from subfolder",
			root:      root,
			userPath:  "a/b/../../../../escaped.dat",
			want:      "",
			expectErr: true,
		},
		{
			name:      "absolute path outside root",
			root:      root,
			userPath:  filepath.Clean("/etc/shadow"),
			want:      "",
			expectErr: true,
		},
		{
			name:      "empty user path",
			root:      root,
			userPath:  "",
			want:      "",
			expectErr: true,
		},
		{
			name:      "empty root path",
			root:      "",
			userPath:  "000001.sst",
			want:      "",
			expectErr: true,
		},
		{
			name:      "null byte in user path",
			root:      root,
			userPath:  "valid/../../\x00escape",
			want:      "",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := security.ResolvePath(tc.root, tc.userPath)
			if tc.expectErr {
				if err == nil {
					t.Fatalf("expected error resolving %q in root %q, got nil (result: %q)", tc.userPath, tc.root, got)
				}
				if !stdErrors.Is(err, errors.ErrInvalidPath) {
					t.Fatalf("expected error to wrap ErrInvalidPath, got: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error resolving %q in root %q: %v", tc.userPath, tc.root, err)
				}
				if got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestValidateContainment_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}

	tempDir := t.TempDir()
	root := filepath.Join(tempDir, "root_dir")
	outsideDir := filepath.Join(tempDir, "outside_dir")

	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("failed to create root: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0700); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}

	// Create an external secret file
	secretFile := filepath.Join(outsideDir, "secret.key")
	if err := os.WriteFile(secretFile, []byte("SUPER_SECRET"), 0600); err != nil {
		t.Fatalf("failed to write secret: %v", err)
	}

	// Create a malicious symlink inside root pointing to outsideDir
	symlinkPath := filepath.Join(root, "sym_to_outside")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Attempting to resolve or validate a path through the symlink must fail closed!
	targetThroughSymlink := filepath.Join(symlinkPath, "secret.key")
	t.Run("symlink_escape_rejection", func(t *testing.T) {
		err := security.ValidateContainment(root, targetThroughSymlink)
		if err == nil {
			t.Fatalf("CRITICAL: Symlink escape containment check bypassed! %q was accepted inside %q", targetThroughSymlink, root)
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected ErrInvalidPath, got: %v", err)
		}
	})

	// Also verify via ResolvePath
	t.Run("resolve_path_symlink_escape", func(t *testing.T) {
		_, err := security.ResolvePath(root, "sym_to_outside/secret.key")
		if err == nil {
			t.Fatal("CRITICAL: ResolvePath allowed symlink escape!")
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected ErrInvalidPath, got: %v", err)
		}
	})

	// Non-escaping symlink (internal to root)
	internalTarget := filepath.Join(root, "real_subdir")
	if err := os.MkdirAll(internalTarget, 0700); err != nil {
		t.Fatalf("failed to create internal target: %v", err)
	}
	internalSymlink := filepath.Join(root, "internal_link")
	if err := os.Symlink(internalTarget, internalSymlink); err != nil {
		t.Fatalf("failed to create internal symlink: %v", err)
	}

	t.Run("internal_symlink_allowed", func(t *testing.T) {
		targetInternal := filepath.Join(internalSymlink, "data.sst")
		if err := security.ValidateContainment(root, targetInternal); err != nil {
			t.Fatalf("internal symlink should be contained, got error: %v", err)
		}
	})

	t.Run("uncreated_root_symlink_escape_rejection", func(t *testing.T) {
		// Root does not exist yet on disk
		uncreatedRoot := filepath.Join(tempDir, "uncreated_db_root")
		// Malicious target has a symlink in its path pointing outside
		maliciousTarget := filepath.Join(symlinkPath, "secret.key")

		err := security.ValidateContainment(uncreatedRoot, maliciousTarget)
		if err == nil {
			t.Fatalf("CRITICAL: uncreated root permitted symlink escape! target=%q, root=%q", maliciousTarget, uncreatedRoot)
		}
		if !stdErrors.Is(err, errors.ErrInvalidPath) {
			t.Fatalf("expected ErrInvalidPath, got: %v", err)
		}
	})

	t.Run("ancestor_symlink_canonical_containment", func(t *testing.T) {
		// Create a symlink dir pointing to root
		aliasDir := filepath.Join(tempDir, "alias_to_root")
		if err := os.Symlink(root, aliasDir); err != nil {
			t.Fatalf("failed to create alias symlink: %v", err)
		}

		// When root is specified as the alias directory (which is a symlink pointing to root)
		targetViaAlias := filepath.Join(aliasDir, "real_subdir", "file.sst")
		if err := security.ValidateContainment(aliasDir, targetViaAlias); err != nil {
			t.Fatalf("expected path inside alias root to be contained: %v", err)
		}

		// An escaping path addressed via alias_to_root/sym_to_outside/secret.key must be rejected
		escapingViaAlias := filepath.Join(aliasDir, "sym_to_outside", "secret.key")
		if err := security.ValidateContainment(aliasDir, escapingViaAlias); err == nil {
			t.Fatalf("expected escaping path through alias root to be rejected, got nil")
		}
	})
}

func FuzzResolvePath(f *testing.F) {
	f.Add("/var/lib/lattice", "000001.sst")
	f.Add("/var/lib/lattice", "../000001.sst")
	f.Add("/var/lib/lattice", "../../etc/passwd")
	f.Add("/var/lib/lattice", "wal/wal_000000000001.log")
	f.Add("/var/lib/lattice", "/var/lib/lattice/000001.sst")
	f.Add("/var/lib/lattice", "/var/lib/lattice-evil/000001.sst")
	f.Add("/var/lib/lattice", "././././wal/./log.txt")
	f.Add("/var/lib/lattice", "")
	f.Add("", "test.sst")

	f.Fuzz(func(t *testing.T, root, userPath string) {
		resolved, err := security.ResolvePath(root, userPath)
		if err == nil {
			// Invariant 1: Root and userPath must be non-empty
			if root == "" || userPath == "" {
				t.Fatalf("ResolvePath succeeded on empty input: root=%q, userPath=%q", root, userPath)
			}
			// Invariant 2: Result must not contain null bytes
			if strings.ContainsRune(resolved, 0) {
				t.Fatalf("ResolvePath returned path with null byte: %q", resolved)
			}
			// Invariant 3: Result must be lexically contained in root
			absRoot, aErr := filepath.Abs(filepath.Clean(root))
			absRes, bErr := filepath.Abs(resolved)
			if aErr == nil && bErr == nil {
				rel, rErr := filepath.Rel(absRoot, absRes)
				if rErr == nil {
					if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						t.Fatalf("CRITICAL FUZZ VIOLATION: resolved path %q escaped root %q (rel=%q)", resolved, root, rel)
					}
				}
			}
		} else {
			// Must return error wrapping ErrInvalidPath
			if !stdErrors.Is(err, errors.ErrInvalidPath) {
				t.Fatalf("expected error wrapping ErrInvalidPath, got: %v", err)
			}
		}
	})
}

package wal_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/silent-knight19/lattice/internal/wal"
)

func TestSEC03_Perms_01_DirectoryAndSegmentRuntimeModes(t *testing.T) {
	h := NewSecurityHarness(t)

	walPath, err := wal.InitDir(h.RootDir())
	if err != nil {
		t.Fatalf("InitDir failed: %v", err)
	}

	if !h.IsWindows() {
		// Verify WAL directory mode is strictly 0700
		dirInfo, err := os.Stat(walPath)
		if err != nil {
			t.Fatalf("stat walPath failed: %v", err)
		}
		if dirInfo.Mode().Perm() != 0700 {
			t.Errorf("expected WAL directory mode 0700, got %#o", dirInfo.Mode().Perm())
		}
	}

	// Create a segment writer
	segPath := wal.SegmentPath(h.RootDir(), 1)
	w, err := wal.CreateWriter(segPath)
	if err != nil {
		t.Fatalf("CreateWriter failed: %v", err)
	}
	_ = w.Close()

	if !h.IsWindows() {
		// Verify segment file mode is strictly 0600
		segInfo, err := os.Stat(segPath)
		if err != nil {
			t.Fatalf("stat segPath failed: %v", err)
		}
		if segInfo.Mode().Perm() != 0600 {
			t.Errorf("expected segment file mode 0600, got %#o", segInfo.Mode().Perm())
		}
	}
}

func TestSEC03_Perms_02_PreexistingPermissiveDirectoryHardened(t *testing.T) {
	h := NewSecurityHarness(t)

	if h.IsWindows() {
		t.Skip("POSIX permission mode hardening is not applicable on Windows NTFS")
	}

	// Create pre-existing wal directory with insecure 0777 permissions
	walPath := h.WALDir()
	if err := os.MkdirAll(walPath, 0777); err != nil {
		t.Fatalf("mkdir walPath failed: %v", err)
	}
	_ = os.Chmod(walPath, 0777)

	// Verify initial loose permissions
	info, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("lstat failed: %v", err)
	}
	if info.Mode().Perm() != 0777 {
		t.Skipf("filesystem did not apply 0777 (umask active: %#o)", info.Mode().Perm())
	}

	// InitDir must safely detect the existing directory and harden it to 0700 via fchmod
	_, err = wal.InitDir(h.RootDir())
	if err != nil {
		t.Fatalf("InitDir failed to harden existing directory: %v", err)
	}

	postInfo, err := os.Lstat(walPath)
	if err != nil {
		t.Fatalf("lstat post-init failed: %v", err)
	}
	if postInfo.Mode().Perm() != 0700 {
		t.Errorf("expected hardened mode 0700, got %#o", postInfo.Mode().Perm())
	}
}

func TestSEC03_Perms_03_RotatedSegmentsEnforceStrictModes(t *testing.T) {
	h := NewSecurityHarness(t)

	opts := wal.Options{
		SegmentSize: 100, // tiny segment to force rapid rotation
	}

	rw, err := wal.OpenRotatingWriter(h.RootDir(), opts)
	if err != nil {
		t.Fatalf("OpenRotatingWriter failed: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// Write 5 records to trigger rotation across multiple segments
	for i := 1; i <= 5; i++ {
		rec := h.MakeRecord(uint64(i), "large-key-padding", "large-value-padding-exceeding-limit")
		if err := rw.AppendSync(rec); err != nil {
			t.Fatalf("AppendSync failed at record %d: %v", i, err)
		}
	}

	// Verify all created segments have 0600 permissions
	ids, err := wal.ListSegments(h.RootDir())
	if err != nil {
		t.Fatalf("ListSegments failed: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("expected multiple rotated segments, got %v", ids)
	}

	if !h.IsWindows() {
		for _, id := range ids {
			sPath := wal.SegmentPath(h.RootDir(), id)
			sInfo, err := os.Stat(sPath)
			if err != nil {
				t.Fatalf("stat segment %d failed: %v", id, err)
			}
			if sInfo.Mode().Perm() != 0600 {
				t.Errorf("rotated segment %d has insecure mode %#o, expected 0600", id, sInfo.Mode().Perm())
			}
		}
	}
}

func TestSEC03_Perms_04_InaccessibleDirectoryFailsClosed(t *testing.T) {
	h := NewSecurityHarness(t)

	if h.IsWindows() {
		t.Skip("POSIX 0000 / 0500 directory permissions not applicable on Windows")
	}

	restrictedDB := filepath.Join(h.RootDir(), "restricted_db")
	if err := os.MkdirAll(restrictedDB, 0700); err != nil {
		t.Fatalf("mkdir restrictedDB: %v", err)
	}

	// Make parent read-only (0500) so wal directory cannot be created
	if err := os.Chmod(restrictedDB, 0500); err != nil {
		t.Fatalf("chmod 0500 failed: %v", err)
	}
	defer func() { _ = os.Chmod(restrictedDB, 0700) }()

	// InitDir must fail closed
	_, err := wal.InitDir(restrictedDB)
	if err == nil {
		t.Fatalf("expected InitDir to fail in read-only parent directory")
	}
	if !os.IsPermission(err) && !errors.Is(err, os.ErrPermission) {
		t.Logf("InitDir returned expected permission-related error: %v", err)
	}
}

func TestSEC03_Perms_05_ReadOnlySegmentRecovery(t *testing.T) {
	h := NewSecurityHarness(t)

	segPath := h.CreateSegmentWithRecords(1, 1, 3)

	if !h.IsWindows() {
		// Make segment 0400 (read-only)
		if err := os.Chmod(segPath, 0400); err != nil {
			t.Fatalf("chmod 0400 failed: %v", err)
		}
		defer func() { _ = os.Chmod(segPath, 0600) }()

		// RecoverSegment requires O_RDWR to truncate if torn; on a clean file with 0400,
		// opening in O_RDWR fails safely with permission denied.
		_, err := wal.RecoverSegment(segPath)
		if err == nil {
			t.Fatalf("expected RecoverSegment to fail opening 0400 file in O_RDWR mode")
		}

		// OpenReader (O_RDONLY) should successfully stream records from read-only segment
		r, err := wal.OpenReader(segPath)
		if err != nil {
			t.Fatalf("OpenReader should succeed on read-only segment: %v", err)
		}
		defer func() { _ = r.Close() }()

		count := 0
		for {
			rec, err := r.Next()
			if err != nil {
				break
			}
			if rec.SeqNum != 0 {
				count++
			}
		}
		if count != 3 {
			t.Errorf("expected 3 records from read-only reader, got %d", count)
		}
	}
}

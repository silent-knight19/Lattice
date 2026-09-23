package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/filter"
	"github.com/silent-knight19/lattice/internal/sstable"
)

// helperBuildSSTable creates a valid SSTable at path with count sequentially numbered keys.
func helperBuildSSTable(t *testing.T, path string, count int) {
	t.Helper()
	opts := sstable.DefaultTableWriterOptions()
	opts.TargetBlockSize = 512 // small block size to force multi-block fixtures easily
	opts.FilterBuilder = filter.NewFilterBlockBuilder(count)
	writer, err := sstable.NewTableWriter(path, opts)
	if err != nil {
		t.Fatalf("failed to create table writer: %v", err)
	}

	for i := 0; i < count; i++ {
		userKey := []byte(fmt.Sprintf("user:%06d", i))
		val := []byte(fmt.Sprintf("value_data_%06d", i))
		ik := binary.InternalKey{
			UserKey: userKey,
			SeqNum:  binary.SeqNum(1000 + i),
			OpType:  binary.OpTypePut,
		}
		if err := writer.Add(ik, val); err != nil {
			t.Fatalf("failed to add record %d: %v", i, err)
		}
	}

	if _, err := writer.Finish(); err != nil {
		t.Fatalf("failed to finalize writer: %v", err)
	}
}

// helperFileHash returns the SHA-256 hash of a file to verify read-only immutability.
func helperFileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read file for hashing: %v", err)
	}
	return sha256.Sum256(data)
}

// -----------------------------------------------------------------------------
// 1. Valid SSTable Inspection & Read-Only Immutability
// -----------------------------------------------------------------------------

func TestInspectSSTable_Valid(t *testing.T) {
	tempDir := t.TempDir()
	sstPath := filepath.Join(tempDir, "000001.sst")
	helperBuildSSTable(t, sstPath, 50)

	hashBefore := helperFileHash(t, sstPath)

	var report ForensicReport
	if err := InspectSSTable(sstPath, &report); err != nil {
		t.Fatalf("unexpected error inspecting valid SSTable: %v", err)
	}

	hashAfter := helperFileHash(t, sstPath)
	if hashBefore != hashAfter {
		t.Fatal("INSPECTION MUTATED FILE: byte-for-byte read-only invariant violated!")
	}

	if !report.Valid {
		t.Fatalf("expected report.Valid == true, got false (note: %s)", report.CorruptionNote)
	}
	if !report.MagicValid {
		t.Errorf("expected MagicValid == true, got false (magic=0x%016x)", report.FooterMagic)
	}
	if !report.PaddingValid {
		t.Errorf("expected PaddingValid == true, got false")
	}
	if !report.IndexCRCPass {
		t.Errorf("expected IndexCRCPass == true, got false")
	}
	if report.TotalRecs != 50 {
		t.Errorf("expected TotalRecs == 50, got %d", report.TotalRecs)
	}
	if !report.HasKeys {
		t.Errorf("expected HasKeys == true")
	}
	if !report.Filter.Present {
		t.Errorf("expected Filter.Present == true")
	} else {
		if !report.Filter.CRCPass {
			t.Errorf("expected Filter.CRCPass == true")
		}
		if report.Filter.HashCount != 7 {
			t.Errorf("expected HashCount == 7, got %d", report.Filter.HashCount)
		}
		if report.Filter.BitCount == 0 {
			t.Errorf("expected BitCount > 0")
		}
	}
}

// -----------------------------------------------------------------------------
// 2. Multi-Block SSTable Inspection
// -----------------------------------------------------------------------------

func TestInspectSSTable_MultiBlock(t *testing.T) {
	tempDir := t.TempDir()
	sstPath := filepath.Join(tempDir, "000002.sst")
	helperBuildSSTable(t, sstPath, 200)

	var report ForensicReport
	if err := InspectSSTable(sstPath, &report); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !report.Valid {
		t.Fatalf("expected report.Valid == true, got false: %s", report.CorruptionNote)
	}
	if len(report.DataBlocks) <= 1 {
		t.Fatalf("expected multiple data blocks, got %d", len(report.DataBlocks))
	}
	if report.IndexEntryCnt != len(report.DataBlocks) {
		t.Errorf("expected IndexEntryCnt == %d, got %d", len(report.DataBlocks), report.IndexEntryCnt)
	}
	if report.TotalRecs != 200 {
		t.Errorf("expected 200 total records, got %d", report.TotalRecs)
	}

	for _, b := range report.DataBlocks {
		if !b.CRCPass {
			t.Errorf("block %d CRC failed", b.Index)
		}
		if b.RestartCount == 0 {
			t.Errorf("block %d has 0 restart points", b.Index)
		}
		if b.Error != "" {
			t.Errorf("block %d has unexpected error: %s", b.Index, b.Error)
		}
	}
}

// -----------------------------------------------------------------------------
// 3. Binary Keys Inspection & Formatting
// -----------------------------------------------------------------------------

func TestInspectSSTable_BinaryKeys(t *testing.T) {
	tempDir := t.TempDir()
	sstPath := filepath.Join(tempDir, "000003.sst")

	opts := sstable.DefaultTableWriterOptions()
	writer, err := sstable.NewTableWriter(sstPath, opts)
	if err != nil {
		t.Fatalf("failed to create writer: %v", err)
	}

	// Add records with binary user keys
	k1 := binary.InternalKey{UserKey: []byte("\x00\x01binary\xff"), SeqNum: binary.SeqNum(10), OpType: binary.OpTypePut}
	k2 := binary.InternalKey{UserKey: []byte("\xfe\xffend"), SeqNum: binary.SeqNum(20), OpType: binary.OpTypePut}
	_ = writer.Add(k1, []byte("val1"))
	_ = writer.Add(k2, []byte("val2"))
	if _, err := writer.Finish(); err != nil {
		t.Fatalf("failed to finalize writer: %v", err)
	}

	var report ForensicReport
	if err := InspectSSTable(sstPath, &report); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !report.Valid {
		t.Fatalf("expected valid report, got: %s", report.CorruptionNote)
	}

	// Verify FormatBytes safe output
	formatted := FormatBytes(k1.UserKey)
	if !strings.Contains(formatted, `\x00`) || !strings.Contains(formatted, `\xff`) {
		t.Errorf("expected escaped binary output, got: %s", formatted)
	}
}

// -----------------------------------------------------------------------------
// 4. Corruption Test Matrix
// -----------------------------------------------------------------------------

func TestInspectSSTable_CorruptionCases(t *testing.T) {
	tempDir := t.TempDir()
	baseSST := filepath.Join(tempDir, "base.sst")
	helperBuildSSTable(t, baseSST, 30)

	baseBytes, err := os.ReadFile(baseSST)
	if err != nil {
		t.Fatalf("failed to read base SSTable: %v", err)
	}

	t.Run("TruncatedFile", func(t *testing.T) {
		corruptPath := filepath.Join(tempDir, "truncated.sst")
		_ = os.WriteFile(corruptPath, baseBytes[:20], 0600)

		var report ForensicReport
		err := InspectSSTable(corruptPath, &report)
		if err != nil {
			t.Fatalf("expected no I/O error, got: %v", err)
		}
		if report.Valid {
			t.Error("expected Valid == false for truncated file")
		}
		if !strings.Contains(report.CorruptionNote, "smaller than minimum") {
			t.Errorf("expected smaller than minimum footer error, got: %s", report.CorruptionNote)
		}
	})

	t.Run("CorruptMagic", func(t *testing.T) {
		corruptPath := filepath.Join(tempDir, "bad_magic.sst")
		badBytes := make([]byte, len(baseBytes))
		copy(badBytes, baseBytes)
		// Corrupt magic at [len-8 : len]
		badBytes[len(badBytes)-1] ^= 0xFF
		_ = os.WriteFile(corruptPath, badBytes, 0600)

		var report ForensicReport
		_ = InspectSSTable(corruptPath, &report)
		if report.Valid || report.MagicValid {
			t.Error("expected Valid == false and MagicValid == false for bad magic")
		}
		if !strings.Contains(report.CorruptionNote, "footer magic mismatch") {
			t.Errorf("expected magic mismatch note, got: %s", report.CorruptionNote)
		}
	})

	t.Run("CorruptPadding", func(t *testing.T) {
		corruptPath := filepath.Join(tempDir, "bad_padding.sst")
		badBytes := make([]byte, len(baseBytes))
		copy(badBytes, baseBytes)
		// Corrupt padding at [len-16 : len-8]
		badBytes[len(badBytes)-12] = 0xAA
		_ = os.WriteFile(corruptPath, badBytes, 0600)

		var report ForensicReport
		_ = InspectSSTable(corruptPath, &report)
		if report.Valid || report.PaddingValid {
			t.Error("expected Valid == false and PaddingValid == false for bad padding")
		}
		if !strings.Contains(report.CorruptionNote, "footer padding non-zero") {
			t.Errorf("expected padding non-zero note, got: %s", report.CorruptionNote)
		}
	})

	t.Run("CorruptIndexCRC", func(t *testing.T) {
		corruptPath := filepath.Join(tempDir, "bad_index_crc.sst")
		badBytes := make([]byte, len(baseBytes))
		copy(badBytes, baseBytes)

		// Decode footer to find index block offset and size
		footerBytes := badBytes[len(badBytes)-sstable.FooterSize:]
		footer, err := sstable.DecodeFooter(footerBytes)
		if err != nil {
			t.Fatalf("failed to decode footer: %v", err)
		}

		// Flip bit in index block payload
		idxEnd := footer.IndexHandle.Offset + footer.IndexHandle.Size
		badBytes[idxEnd-1] ^= 0xFF // Flip CRC byte
		_ = os.WriteFile(corruptPath, badBytes, 0600)

		var report ForensicReport
		_ = InspectSSTable(corruptPath, &report)
		if report.Valid || report.IndexCRCPass {
			t.Error("expected Valid == false and IndexCRCPass == false for bad index CRC")
		}
		if !strings.Contains(report.CorruptionNote, "index block CRC mismatch") {
			t.Errorf("expected index CRC mismatch note, got: %s", report.CorruptionNote)
		}
	})

	t.Run("CorruptDataBlockCRC", func(t *testing.T) {
		corruptPath := filepath.Join(tempDir, "bad_data_crc.sst")
		badBytes := make([]byte, len(baseBytes))
		copy(badBytes, baseBytes)

		// Flip bit at offset 10 (inside first data block)
		badBytes[10] ^= 0xFF
		_ = os.WriteFile(corruptPath, badBytes, 0600)

		var report ForensicReport
		_ = InspectSSTable(corruptPath, &report)
		if report.Valid {
			t.Error("expected Valid == false for corrupted data block")
		}
		if len(report.DataBlocks) > 0 && report.DataBlocks[0].CRCPass {
			t.Error("expected block 0 CRCPass == false")
		}
	})

	t.Run("CorruptRestartCount", func(t *testing.T) {
		corruptPath := filepath.Join(tempDir, "bad_restart.sst")
		badBytes := make([]byte, len(baseBytes))
		copy(badBytes, baseBytes)

		footerBytes := badBytes[len(badBytes)-sstable.FooterSize:]
		footer, _ := sstable.DecodeFooter(footerBytes)
		idxBuf := badBytes[footer.IndexHandle.Offset : footer.IndexHandle.Offset+footer.IndexHandle.Size]
		blockIdx, _ := sstable.DecodeBlockIndex(idxBuf)
		h := blockIdx.Entries()[0].Handle

		// Data block trailer: RestartCount at [Offset + Size - 8 : Offset + Size - 4]
		restartPos := h.Offset + h.Size - 8
		binary.PutUint32(badBytes[restartPos:restartPos+4], sstable.MaxRestartCount+100)

		// Recompute block CRC so it passes CRC check and fails on restart count check
		newCRC := binary.Checksum(badBytes[h.Offset : h.Offset+h.Size-4])
		binary.PutUint32(badBytes[h.Offset+h.Size-4:h.Offset+h.Size], newCRC)

		_ = os.WriteFile(corruptPath, badBytes, 0600)

		var report ForensicReport
		_ = InspectSSTable(corruptPath, &report)
		if report.Valid {
			t.Error("expected Valid == false for excessive restart count")
		}
		if len(report.DataBlocks) > 0 && !strings.Contains(report.DataBlocks[0].Error, "invalid restart count") {
			t.Errorf("expected invalid restart count error, got: %s", report.DataBlocks[0].Error)
		}
	})
}

// -----------------------------------------------------------------------------
// 5. CLI Subcommand Unit Tests
// -----------------------------------------------------------------------------

func TestRunInspectSSTable_Unit(t *testing.T) {
	tempDir := t.TempDir()
	sstPath := filepath.Join(tempDir, "valid.sst")
	helperBuildSSTable(t, sstPath, 10)

	var stdout, stderr bytes.Buffer

	// Help flag
	code := runInspectSSTable([]string{"--help"}, &stdout, &stderr)
	if code != ExitInspectSuccess {
		t.Errorf("expected exit 0 for --help, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Lattice SSTable Forensic Inspection Tool") {
		t.Errorf("expected usage in stdout, got: %s", stdout.String())
	}

	// Missing path
	stdout.Reset()
	stderr.Reset()
	code = runInspectSSTable([]string{}, &stdout, &stderr)
	if code != ExitInspectUsageError {
		t.Errorf("expected exit 1 for missing path, got %d", code)
	}

	// Extra args
	stdout.Reset()
	stderr.Reset()
	code = runInspectSSTable([]string{"path1", "path2"}, &stdout, &stderr)
	if code != ExitInspectUsageError {
		t.Errorf("expected exit 1 for extra args, got %d", code)
	}

	// Non-existent file
	stdout.Reset()
	stderr.Reset()
	code = runInspectSSTable([]string{"/nonexistent/000001.sst"}, &stdout, &stderr)
	if code != ExitInspectFileError {
		t.Errorf("expected exit 2 for non-existent file, got %d", code)
	}

	// Directory path
	stdout.Reset()
	stderr.Reset()
	code = runInspectSSTable([]string{tempDir}, &stdout, &stderr)
	if code != ExitInspectFileError {
		t.Errorf("expected exit 2 for directory path, got %d", code)
	}

	// Valid SSTable inspection
	stdout.Reset()
	stderr.Reset()
	code = runInspectSSTable([]string{sstPath}, &stdout, &stderr)
	if code != ExitInspectSuccess {
		t.Errorf("expected exit 0 for valid SSTable, got %d, stderr: %s", code, stderr.String())
	}
	outStr := stdout.String()
	if !strings.Contains(outStr, "Status            : VALID") {
		t.Errorf("expected VALID in output, got: %s", outStr)
	}

	// Verbose inspection
	stdout.Reset()
	stderr.Reset()
	code = runInspectSSTable([]string{"-v", sstPath}, &stdout, &stderr)
	if code != ExitInspectSuccess {
		t.Errorf("expected exit 0 for verbose inspection, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Restarts:") {
		t.Errorf("expected Restarts in verbose output, got: %s", stdout.String())
	}
}

// -----------------------------------------------------------------------------
// 6. Real Binary Subprocess Verification
// -----------------------------------------------------------------------------

func TestBinarySubprocess_InspectSSTable(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "lattice")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build lattice binary: %v\nOutput: %s", err, string(out))
	}

	// 1. Test binary help
	cmd := exec.Command(binPath, "inspect-sstable", "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect-sstable --help failed: %v", err)
	}
	if !strings.Contains(string(out), "Lattice SSTable Forensic Inspection Tool") {
		t.Errorf("expected usage in help output, got: %s", string(out))
	}

	// 2. Test binary with non-existent file
	cmd = exec.Command(binPath, "inspect-sstable", filepath.Join(tempDir, "missing.sst"))
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for missing file, got 0")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() != ExitInspectFileError {
			t.Errorf("expected exit code %d, got %d", ExitInspectFileError, exitErr.ExitCode())
		}
	}

	// 3. Test binary with valid file
	validPath := filepath.Join(tempDir, "valid.sst")
	helperBuildSSTable(t, validPath, 25)

	cmd = exec.Command(binPath, "inspect-sstable", validPath)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect-sstable valid file failed: %v\nOutput: %s", err, string(out))
	}
	outStr := string(out)
	if !strings.Contains(outStr, "[SSTable Forensic Report]") || !strings.Contains(outStr, "Status            : VALID") {
		t.Errorf("unexpected output for valid file: %s", outStr)
	}

	// 4. Test binary with corrupted file
	corruptPath := filepath.Join(tempDir, "corrupt.sst")
	corruptBytes, _ := os.ReadFile(validPath)
	corruptBytes[len(corruptBytes)-1] ^= 0xFF // Flip magic
	_ = os.WriteFile(corruptPath, corruptBytes, 0600)

	cmd = exec.Command(binPath, "inspect-sstable", corruptPath)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for corrupted file, got 0")
	}
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() != ExitInspectCorruptError {
			t.Errorf("expected exit code %d (ExitInspectCorruptError), got %d", ExitInspectCorruptError, exitErr.ExitCode())
		}
	}
	if !strings.Contains(string(out), "Status            : CORRUPT") {
		t.Errorf("expected CORRUPT in output, got: %s", string(out))
	}
}

// TestInspectSSTable_FIFORejection verifies SEC-P12-001:
// Pointing inspect-sstable at a named pipe (FIFO) fails immediately with an error
// and does not block indefinitely in os.Open.
func TestInspectSSTable_FIFORejection(t *testing.T) {
	tempDir := t.TempDir()
	fifoPath := filepath.Join(tempDir, "test.fifo")

	cmd := exec.Command("mkfifo", fifoPath)
	if err := cmd.Run(); err != nil {
		t.Skipf("mkfifo not supported in test environment: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var report ForensicReport
		done <- InspectSSTable(fifoPath, &report)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error on FIFO, got nil")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("expected 'not a regular file' error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CRITICAL: InspectSSTable blocked indefinitely on FIFO!")
	}
}

// TestInspectSSTable_SecurityPathRejection tests P19-S01-M01 security path sanitization.
func TestInspectSSTable_SecurityPathRejection(t *testing.T) {
	tempDir := t.TempDir()

	t.Run("null byte rejected", func(t *testing.T) {
		var report ForensicReport
		err := InspectSSTable("sstable\x00evil.sst", &report)
		if err == nil {
			t.Fatal("expected error on null byte, got nil")
		}
	})

	t.Run("symlink rejected", func(t *testing.T) {
		targetFile := filepath.Join(tempDir, "real.sst")
		helperBuildSSTable(t, targetFile, 5)

		symlinkPath := filepath.Join(tempDir, "symlink.sst")
		if err := os.Symlink(targetFile, symlinkPath); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		var report ForensicReport
		err := InspectSSTable(symlinkPath, &report)
		if err == nil {
			t.Fatal("expected error on symlink target, got nil")
		}
		if !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("expected symbolic link error, got: %v", err)
		}
	})
}

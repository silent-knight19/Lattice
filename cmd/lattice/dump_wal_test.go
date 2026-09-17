package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/wal"
)

// helperBuildRecord serializes a valid WAL record.
func helperBuildRecord(t *testing.T, recType wal.RecordType, seq uint64, ts uint64, key, val []byte) []byte {
	t.Helper()
	rec := wal.Record{
		Type:      recType,
		SeqNum:    binary.SeqNum(seq),
		Timestamp: ts,
		Key:       key,
		Value:     val,
	}
	encoded, err := wal.EncodeRecord(rec)
	if err != nil {
		t.Fatalf("helperBuildRecord failed: %v", err)
	}
	return encoded
}

// -----------------------------------------------------------------------------
// 1. Valid WAL Inspection
// -----------------------------------------------------------------------------

func TestDumpWAL_Valid_SinglePut(t *testing.T) {
	tempDir := t.TempDir()
	walPath := filepath.Join(tempDir, "wal_000000000001.log")

	recBytes := helperBuildRecord(t, wal.RecordTypePut, 101, 1700000000000, []byte("user:123"), []byte("Alice Smith"))
	if err := os.WriteFile(walPath, recBytes, 0600); err != nil {
		t.Fatalf("failed to write WAL file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := DumpWAL(walPath, false, &stdout, &stderr)

	if exitCode != ExitDumpSuccess {
		t.Fatalf("expected ExitDumpSuccess (0), got %d; stderr: %s", exitCode, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Integrity:      VALID") {
		t.Errorf("expected report to contain Integrity: VALID, got:\n%s", out)
	}
	if !strings.Contains(out, "Record #0:") {
		t.Errorf("expected report to contain Record #0, got:\n%s", out)
	}
	if !strings.Contains(out, "Type:         PUT") {
		t.Errorf("expected report to contain Type: PUT, got:\n%s", out)
	}
	if !strings.Contains(out, "Sequence:     101") {
		t.Errorf("expected report to contain Sequence: 101, got:\n%s", out)
	}
	if !strings.Contains(out, "Records:        1") {
		t.Errorf("expected report to show 1 record, got:\n%s", out)
	}
	if !strings.Contains(out, "CRC:          ") || !strings.Contains(out, "[PASS]") {
		t.Errorf("expected report to show CRC [PASS], got:\n%s", out)
	}
}

func TestDumpWAL_Valid_MultiRecords(t *testing.T) {
	tempDir := t.TempDir()
	walPath := filepath.Join(tempDir, "wal_000000000002.log")

	var allBytes []byte
	// 1. BATCH_START
	allBytes = append(allBytes, helperBuildRecord(t, wal.RecordTypeBatchStart, 1, 1000, nil, nil)...)
	// 2. PUT normal
	allBytes = append(allBytes, helperBuildRecord(t, wal.RecordTypePut, 2, 1001, []byte("alpha"), []byte("value_alpha"))...)
	// 3. PUT zero-length value
	allBytes = append(allBytes, helperBuildRecord(t, wal.RecordTypePut, 3, 1002, []byte("empty_val"), []byte(""))...)
	// 4. DELETE tombstone
	allBytes = append(allBytes, helperBuildRecord(t, wal.RecordTypeDelete, 4, 1003, []byte("alpha"), nil)...)
	// 5. PUT with binary keys and binary values
	allBytes = append(allBytes, helperBuildRecord(t, wal.RecordTypePut, 5, 1004, []byte("\x00\x01\xff\xfe"), []byte("\x1b[31mRed\x00Binary\xff"))...)
	// 6. BATCH_COMMIT
	allBytes = append(allBytes, helperBuildRecord(t, wal.RecordTypeBatchCommit, 6, 1005, nil, nil)...)

	if err := os.WriteFile(walPath, allBytes, 0600); err != nil {
		t.Fatalf("failed to write WAL file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := DumpWAL(walPath, true, &stdout, &stderr)

	if exitCode != ExitDumpSuccess {
		t.Fatalf("expected ExitDumpSuccess (0), got %d; stderr: %s", exitCode, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Records:        6") {
		t.Errorf("expected report to show 6 records, got:\n%s", out)
	}
	if !strings.Contains(out, "First Sequence: 1") || !strings.Contains(out, "Last Sequence:  6") {
		t.Errorf("expected First Sequence: 1 and Last Sequence: 6, got:\n%s", out)
	}
	// Verify binary key is escaped and does not inject raw ANSI escape
	if strings.Contains(out, "\x1b[31m") {
		t.Errorf("raw escape sequences should not appear unescaped in stdout!")
	}
	if !strings.Contains(out, `\x1b`) {
		t.Errorf("expected escaped \\x1b for binary value, got:\n%s", out)
	}
	if !strings.Contains(out, `\x00\x01\xff\xfe`) {
		t.Errorf("expected escaped binary key, got:\n%s", out)
	}
}

func TestDumpWAL_Valid_EmptyWAL(t *testing.T) {
	tempDir := t.TempDir()
	walPath := filepath.Join(tempDir, "empty.log")
	if err := os.WriteFile(walPath, nil, 0600); err != nil {
		t.Fatalf("failed to create empty file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := DumpWAL(walPath, false, &stdout, &stderr)

	if exitCode != ExitDumpSuccess {
		t.Fatalf("expected ExitDumpSuccess (0) on empty WAL, got %d", exitCode)
	}

	out := stdout.String()
	if !strings.Contains(out, "Records:        0") {
		t.Errorf("expected 0 records, got:\n%s", out)
	}
	if !strings.Contains(out, "Integrity:      VALID") {
		t.Errorf("expected Integrity: VALID, got:\n%s", out)
	}
	if !strings.Contains(out, "First Sequence: N/A") {
		t.Errorf("expected First Sequence: N/A, got:\n%s", out)
	}
}

func TestDumpWAL_VerboseMode(t *testing.T) {
	tempDir := t.TempDir()
	walPath := filepath.Join(tempDir, "verbose.log")

	longVal := make([]byte, 128)
	for i := range longVal {
		longVal[i] = 'A' + byte(i%26)
	}

	recBytes := helperBuildRecord(t, wal.RecordTypePut, 42, 9999, []byte("my_key"), longVal)
	if err := os.WriteFile(walPath, recBytes, 0600); err != nil {
		t.Fatalf("failed to write WAL: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := DumpWAL(walPath, true, &stdout, &stderr)
	if exitCode != ExitDumpSuccess {
		t.Fatalf("expected success, got %d", exitCode)
	}

	out := stdout.String()
	if !strings.Contains(out, "Key:          \"my_key\"") {
		t.Errorf("expected verbose key display, got:\n%s", out)
	}
	// Verify bounded value preview of 64 bytes + total length
	if !strings.Contains(out, "... (128 bytes total)") {
		t.Errorf("expected bounded value preview with total count, got:\n%s", out)
	}
}

// -----------------------------------------------------------------------------
// 2. Read-Only Immutability
// -----------------------------------------------------------------------------

func TestDumpWAL_ReadOnlyImmutability(t *testing.T) {
	tempDir := t.TempDir()
	walPath := filepath.Join(tempDir, "immutable.log")

	recBytes := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("k"), []byte("v"))
	if err := os.WriteFile(walPath, recBytes, 0600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	hashBefore := helperFileHash(t, walPath)
	infoBefore, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat before failed: %v", err)
	}

	// Perform multiple forensic dump operations
	for i := 0; i < 5; i++ {
		var stdout, stderr bytes.Buffer
		code := DumpWAL(walPath, true, &stdout, &stderr)
		if code != ExitDumpSuccess {
			t.Fatalf("iteration %d failed with code %d", i, code)
		}
	}

	hashAfter := helperFileHash(t, walPath)
	infoAfter, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat after failed: %v", err)
	}

	if hashBefore != hashAfter {
		t.Fatalf("CRITICAL: DumpWAL modified the WAL file! Hash before: %x, hash after: %x", hashBefore, hashAfter)
	}
	if infoBefore.Size() != infoAfter.Size() {
		t.Fatalf("CRITICAL: DumpWAL modified file size! Before: %d, after: %d", infoBefore.Size(), infoAfter.Size())
	}
	if !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatalf("CRITICAL: DumpWAL changed file modification time!")
	}
}

// -----------------------------------------------------------------------------
// 3. Corruption & Truncation Matrix
// -----------------------------------------------------------------------------

func TestDumpWAL_CorruptionMatrix(t *testing.T) {
	tests := []struct {
		name              string
		buildData         func() []byte
		expectedIntegrity string
		expectedErrorFrag string
	}{
		{
			name: "one_byte_file",
			buildData: func() []byte {
				return []byte{0x01}
			},
			expectedIntegrity: "TRUNCATED",
			expectedErrorFrag: "wal record header truncated",
		},
		{
			name: "truncated_header_10_bytes",
			buildData: func() []byte {
				return make([]byte, 10)
			},
			expectedIntegrity: "TRUNCATED",
			expectedErrorFrag: "wal record header truncated",
		},
		{
			name: "invalid_record_type",
			buildData: func() []byte {
				rec := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("key"), []byte("val"))
				// RecordType is at byte offset 4
				rec[4] = 0x99 // invalid record type
				return rec
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "unrecognized record type 0x99",
		},
		{
			name: "corrupt_crc_header",
			buildData: func() []byte {
				rec := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("key"), []byte("val"))
				// Flip bit in timestamp (byte 13)
				rec[13] ^= 0x01
				return rec
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "expected 0x",
		},
		{
			name: "corrupt_crc_payload",
			buildData: func() []byte {
				rec := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("key"), []byte("val"))
				// Flip bit in payload
				rec[len(rec)-1] ^= 0x01
				return rec
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "expected 0x",
		},
		{
			name: "zero_key_len_put",
			buildData: func() []byte {
				// Manually construct PUT header with KeyLength=0
				var buf [wal.HeaderSize + 2 + 4]byte
				buf[4] = byte(wal.RecordTypePut)
				binary.PutUint16(buf[21:23], 0) // keyLen = 0
				binary.PutUint32(buf[23:27], 0) // valLen = 0
				crc := binary.Checksum(buf[4:])
				binary.PutUint32(buf[0:4], crc)
				return buf[:]
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "put or delete record has zero-length key",
		},
		{
			name: "batch_marker_with_key",
			buildData: func() []byte {
				// Manually construct BATCH_START with keyLen > 0
				var buf [wal.HeaderSize + 2 + 4 + 4]byte
				buf[4] = byte(wal.RecordTypeBatchStart)
				binary.PutUint16(buf[21:23], 4) // keyLen = 4
				copy(buf[23:27], "abcd")
				binary.PutUint32(buf[27:31], 0) // valLen = 0
				crc := binary.Checksum(buf[4:])
				binary.PutUint32(buf[0:4], crc)
				return buf[:]
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "batch marker cannot have a key payload",
		},
		{
			name: "batch_marker_with_value",
			buildData: func() []byte {
				// Manually construct BATCH_START with valLen > 0
				var buf [wal.HeaderSize + 2 + 4 + 4]byte
				buf[4] = byte(wal.RecordTypeBatchStart)
				binary.PutUint16(buf[21:23], 0) // keyLen = 0
				binary.PutUint32(buf[23:27], 4) // valLen = 4
				copy(buf[27:31], "1234")
				crc := binary.Checksum(buf[4:])
				binary.PutUint32(buf[0:4], crc)
				return buf[:]
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "batch marker cannot have a value payload",
		},
		{
			name: "truncated_key_payload",
			buildData: func() []byte {
				// Declare keyLen = 10, provide only 5 bytes
				var buf [wal.HeaderSize + 2 + 5]byte
				buf[4] = byte(wal.RecordTypePut)
				binary.PutUint16(buf[21:23], 10)
				copy(buf[23:], "12345")
				return buf[:]
			},
			expectedIntegrity: "TRUNCATED",
			expectedErrorFrag: "unexpected EOF",
		},
		{
			name: "truncated_value_payload",
			buildData: func() []byte {
				// Declare valLen = 20, provide only 5 bytes
				var buf [wal.HeaderSize + 2 + 3 + 4 + 5]byte
				buf[4] = byte(wal.RecordTypePut)
				binary.PutUint16(buf[21:23], 3)
				copy(buf[23:26], "key")
				binary.PutUint32(buf[26:30], 20)
				copy(buf[30:], "12345")
				return buf[:]
			},
			expectedIntegrity: "TRUNCATED",
			expectedErrorFrag: "unexpected EOF",
		},
		{
			name: "corruption_after_valid_records",
			buildData: func() []byte {
				valid1 := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("key1"), []byte("val1"))
				valid2 := helperBuildRecord(t, wal.RecordTypePut, 2, 101, []byte("key2"), []byte("val2"))
				corrupt := helperBuildRecord(t, wal.RecordTypePut, 3, 102, []byte("key3"), []byte("val3"))
				corrupt[13] ^= 0xff // corrupt CRC of record #3

				var combined []byte
				combined = append(combined, valid1...)
				combined = append(combined, valid2...)
				combined = append(combined, corrupt...)
				return combined
			},
			expectedIntegrity: "CORRUPT",
			expectedErrorFrag: "expected 0x",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			walPath := filepath.Join(tempDir, "corrupt.log")
			if err := os.WriteFile(walPath, tc.buildData(), 0600); err != nil {
				t.Fatalf("failed to write fixture: %v", err)
			}

			var stdout, stderr bytes.Buffer
			exitCode := DumpWAL(walPath, false, &stdout, &stderr)

			if exitCode != ExitDumpCorruptError {
				t.Fatalf("expected ExitDumpCorruptError (3), got %d; stdout:\n%s", exitCode, stdout.String())
			}

			out := stdout.String()
			if !strings.Contains(out, fmt.Sprintf("Integrity:      %s", tc.expectedIntegrity)) {
				t.Errorf("expected Integrity: %s, got:\n%s", tc.expectedIntegrity, out)
			}
			if !strings.Contains(out, tc.expectedErrorFrag) {
				t.Errorf("expected error fragment %q, got:\n%s", tc.expectedErrorFrag, out)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 4. Torn Tail Matrix
// -----------------------------------------------------------------------------

func TestDumpWAL_TornTailMatrix(t *testing.T) {
	// Construct a WAL with 1 valid record followed by a second record torn at different byte offsets
	validRec := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("good_key"), []byte("good_value"))
	secondRec := helperBuildRecord(t, wal.RecordTypePut, 2, 200, []byte("torn_key_long"), []byte("torn_value_payload_long"))

	truncations := []int{
		1,                                   // truncated by 1 byte
		5,                                   // truncated by 5 bytes
		10,                                  // truncated mid-value
		len(secondRec) - wal.HeaderSize - 2, // truncated mid-key
		len(secondRec) - 10,                 // truncated mid-header
		len(secondRec) - 1,                  // truncated 1 byte into header
	}

	for _, cutBytes := range truncations {
		t.Run(fmt.Sprintf("truncated_by_%d", cutBytes), func(t *testing.T) {
			tempDir := t.TempDir()
			walPath := filepath.Join(tempDir, "torn.log")

			tornPart := secondRec[:len(secondRec)-cutBytes]
			data := append(append([]byte{}, validRec...), tornPart...)

			if err := os.WriteFile(walPath, data, 0600); err != nil {
				t.Fatalf("failed to write torn file: %v", err)
			}

			var stdout, stderr bytes.Buffer
			code := DumpWAL(walPath, false, &stdout, &stderr)

			if code != ExitDumpCorruptError {
				t.Fatalf("expected ExitDumpCorruptError (3) on torn tail, got %d", code)
			}

			out := stdout.String()
			// The first record must be successfully reported
			if !strings.Contains(out, "Record #0:") {
				t.Errorf("expected Record #0 to be reported before torn tail, got:\n%s", out)
			}
			if !strings.Contains(out, "Records:        1") {
				t.Errorf("expected valid record count 1, got:\n%s", out)
			}
			if !strings.Contains(out, "Integrity:      TRUNCATED") {
				t.Errorf("expected Integrity: TRUNCATED, got:\n%s", out)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 5. Determinism Test
// -----------------------------------------------------------------------------

func TestDumpWAL_Determinism(t *testing.T) {
	tempDir := t.TempDir()
	walPath := filepath.Join(tempDir, "deterministic.log")

	var data []byte
	data = append(data, helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("key_a"), []byte("val_a"))...)
	data = append(data, helperBuildRecord(t, wal.RecordTypePut, 2, 200, []byte("key_b"), []byte("val_b"))...)
	if err := os.WriteFile(walPath, data, 0600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	var firstStdout, firstStderr bytes.Buffer
	code1 := DumpWAL(walPath, true, &firstStdout, &firstStderr)

	var secondStdout, secondStderr bytes.Buffer
	code2 := DumpWAL(walPath, true, &secondStdout, &secondStderr)

	if code1 != code2 {
		t.Fatalf("exit codes differ across runs: %d vs %d", code1, code2)
	}
	if firstStdout.String() != secondStdout.String() {
		t.Fatalf("stdout differs across runs!\nRun 1:\n%s\nRun 2:\n%s", firstStdout.String(), secondStdout.String())
	}
	if firstStderr.String() != secondStderr.String() {
		t.Fatalf("stderr differs across runs!")
	}
}

// -----------------------------------------------------------------------------
// 6. CLI Usage & Filesystem Error Handling
// -----------------------------------------------------------------------------

func TestDumpWAL_CLIUsageAndFileErrors(t *testing.T) {
	tempDir := t.TempDir()

	// 1. Missing arguments
	var stdout, stderr bytes.Buffer
	code := runDumpWAL([]string{}, &stdout, &stderr)
	if code != ExitDumpUsageError {
		t.Errorf("expected ExitDumpUsageError (1) on empty args, got %d", code)
	}

	// 2. Too many arguments
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{"file1.log", "file2.log"}, &stdout, &stderr)
	if code != ExitDumpUsageError {
		t.Errorf("expected ExitDumpUsageError (1) on multiple args, got %d", code)
	}

	// 3. Unknown flag
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{"--nonexistent-flag", "file.log"}, &stdout, &stderr)
	if code != ExitDumpUsageError {
		t.Errorf("expected ExitDumpUsageError (1) on unknown flag, got %d", code)
	}

	// 4. Help flag
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{"--help"}, &stdout, &stderr)
	if code != ExitDumpSuccess {
		t.Errorf("expected ExitDumpSuccess (0) on --help, got %d", code)
	}
	if !strings.Contains(stdout.String(), "Usage of lattice dump-wal:") {
		t.Errorf("expected help output, got:\n%s", stdout.String())
	}

	// 5. Version flag
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{"-V"}, &stdout, &stderr)
	if code != ExitDumpSuccess {
		t.Errorf("expected ExitDumpSuccess (0) on -V, got %d", code)
	}
	if !strings.Contains(stdout.String(), "lattice dump-wal version") {
		t.Errorf("expected version output, got:\n%s", stdout.String())
	}

	// 6. Non-existent file
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{filepath.Join(tempDir, "missing.log")}, &stdout, &stderr)
	if code != ExitDumpFileError {
		t.Errorf("expected ExitDumpFileError (2) on missing file, got %d", code)
	}

	// 7. Directory target
	dirPath := filepath.Join(tempDir, "some_dir")
	if err := os.Mkdir(dirPath, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{dirPath}, &stdout, &stderr)
	if code != ExitDumpFileError {
		t.Errorf("expected ExitDumpFileError (2) on directory target, got %d", code)
	}

	// 8. Symlink target
	targetPath := filepath.Join(tempDir, "target.log")
	if err := os.WriteFile(targetPath, []byte("data"), 0600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	symlinkPath := filepath.Join(tempDir, "symlink.log")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = runDumpWAL([]string{symlinkPath}, &stdout, &stderr)
	if code != ExitDumpFileError {
		t.Errorf("expected ExitDumpFileError (2) on symlink target, got %d", code)
	}
}

// -----------------------------------------------------------------------------
// 7. Binary Subprocess Integration Test
// -----------------------------------------------------------------------------

func TestBinarySubprocess_DumpWAL(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "lattice_test_bin")

	// Compile the real executable
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build lattice binary: %v\nOutput:\n%s", err, string(out))
	}

	walValid := filepath.Join(tempDir, "valid.log")
	rec := helperBuildRecord(t, wal.RecordTypePut, 1, 100, []byte("key"), []byte("val"))
	if err := os.WriteFile(walValid, rec, 0600); err != nil {
		t.Fatalf("write valid WAL failed: %v", err)
	}

	walCorrupt := filepath.Join(tempDir, "corrupt.log")
	corruptRec := make([]byte, len(rec))
	copy(corruptRec, rec)
	corruptRec[len(corruptRec)-1] ^= 0xff
	if err := os.WriteFile(walCorrupt, corruptRec, 0600); err != nil {
		t.Fatalf("write corrupt WAL failed: %v", err)
	}

	// 1. Subprocess: dump-wal on valid file
	cmd := exec.Command(binPath, "dump-wal", walValid)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Fatalf("subprocess dump-wal valid failed: %v\nStderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Integrity:      VALID") {
		t.Errorf("expected Integrity: VALID in subprocess output, got:\n%s", stdout.String())
	}

	// 2. Subprocess: dump-wal on corrupt file
	cmd = exec.Command(binPath, "dump-wal", walCorrupt)
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err == nil {
		t.Fatalf("expected subprocess dump-wal corrupt to fail with exit code 3")
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() != ExitDumpCorruptError {
			t.Errorf("expected exit code 3, got %d", exitErr.ExitCode())
		}
	}
	if !strings.Contains(stdout.String(), "Integrity:      CORRUPT") {
		t.Errorf("expected Integrity: CORRUPT in subprocess output, got:\n%s", stdout.String())
	}

	// 3. Subprocess: --help flag
	cmd = exec.Command(binPath, "dump-wal", "--help")
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess dump-wal --help failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "Usage of lattice dump-wal:") {
		t.Errorf("expected help output, got:\n%s", stdout.String())
	}

	// 4. Subprocess: main lattice help lists dump-wal
	cmd = exec.Command(binPath, "--help")
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()
	if !strings.Contains(stdout.String(), "dump-wal") {
		t.Errorf("expected lattice --help to list dump-wal command, got:\n%s", stdout.String())
	}
}

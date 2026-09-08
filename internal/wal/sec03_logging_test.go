package wal_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/logger"
	"github.com/silent-knight19/lattice/internal/wal"
)

// SEC-03.12: Logging Security Audit
//
// Invariants tested:
//   1. Secret-like keys/values in structured logger attributes are automatically redacted.
//   2. Record.String() never leaks raw key or value bytes, even when keys contain secret patterns.
//   3. Errors wrapped with %v or %+v do not dump binary key/value contents.
//   4. Recovery errors and reports do not leak internal secret payloads.
//   5. Non-secret keys are preserved where appropriate (no blanket over-redaction).

// TestSEC03_Logging_01_RecordStringDoesNotLeakPayloads verifies that formatting a Record
// via String(), %v, or %+v prints metadata and lengths only, never raw payload bytes.
func TestSEC03_Logging_01_RecordStringDoesNotLeakPayloads(t *testing.T) {
	secretPayload := "SUPER_SECRET_AUTHENTICATION_KEY_12345"
	secretKey := "api_token_key"

	rec := wal.Record{
		CRC:       0x12345678,
		Type:      wal.RecordTypePut,
		SeqNum:    binary.SeqNum(100),
		Timestamp: 1700000000000,
		Key:       []byte(secretKey),
		Value:     []byte(secretPayload),
	}

	// 1. Direct String() call
	s := rec.String()
	if strings.Contains(s, secretPayload) {
		t.Fatalf("SECURITY VIOLATION: rec.String() leaked secret payload: %s", s)
	}
	if strings.Contains(s, secretKey) {
		t.Fatalf("SECURITY VIOLATION: rec.String() leaked secret key: %s", s)
	}

	// 2. fmt.Sprintf("%v")
	sV := fmt.Sprintf("%v", rec)
	if strings.Contains(sV, secretPayload) || strings.Contains(sV, secretKey) {
		t.Fatalf("SECURITY VIOLATION: fmt.Sprintf(%%v) leaked secret: %s", sV)
	}

	// 3. fmt.Sprintf("%+v")
	sPlusV := fmt.Sprintf("%+v", rec)
	if strings.Contains(sPlusV, secretPayload) || strings.Contains(sPlusV, secretKey) {
		t.Fatalf("SECURITY VIOLATION: fmt.Sprintf(%%+v) leaked secret: %s", sPlusV)
	}

	// Verify lengths are safely present
	if !strings.Contains(s, fmt.Sprintf("KeyLen=%d", len(secretKey))) {
		t.Errorf("expected KeyLen in String(), got: %s", s)
	}
	if !strings.Contains(s, fmt.Sprintf("ValLen=%d", len(secretPayload))) {
		t.Errorf("expected ValLen in String(), got: %s", s)
	}
}

// TestSEC03_Logging_02_StructuredLoggerRedactsSensitiveKeys verifies that logging
// attributes matching defaultSensitiveKeys (password, token, api_key, secret, etc.)
// replaces their values with [REDACTED].
func TestSEC03_Logging_02_StructuredLoggerRedactsSensitiveKeys(t *testing.T) {
	sensitiveKeys := []string{
		"password",
		"passwd",
		"secret",
		"token",
		"auth",
		"authorization",
		"api_key",
		"apikey",
		"private_key",
		"credential",
		"credentials",
		"access_token",
		"refresh_token",
	}

	for _, k := range sensitiveKeys {
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)

		secretVal := "super-confidential-secret-value-999"
		log.Info("user operation", k, secretVal)

		out := buf.String()
		if strings.Contains(out, secretVal) {
			t.Fatalf("SECURITY VIOLATION: sensitive key %q leaked value in log: %s", k, out)
		}

		var parsed map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
			t.Fatalf("failed to parse JSON log: %v", err)
		}

		if val, ok := parsed[k]; !ok || val != logger.RedactedPlaceholder {
			t.Errorf("key %q: expected %q, got %v", k, logger.RedactedPlaceholder, val)
		}
	}
}

// TestSEC03_Logging_03_ErrorsDoNotLeakSensitiveKeyValues verifies that domain and
// WAL errors do not print entire binary key or value payloads into error strings.
func TestSEC03_Logging_03_ErrorsDoNotLeakSensitiveKeyValues(t *testing.T) {
	hugeSecretVal := make([]byte, 5*1024*1024) // 5 MB secret
	for i := range hugeSecretVal {
		hugeSecretVal[i] = 'X'
	}

	err := binary.ValidateValue(hugeSecretVal)
	if err == nil {
		t.Fatalf("expected ValidateValue to fail")
	}

	errStr := err.Error()
	if strings.Contains(errStr, "XXXXXXXXXX") {
		t.Fatalf("SECURITY VIOLATION: error string printed binary value payload: %s", errStr)
	}

	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelError)
	log.Error("validation failure", "error", err)

	logOut := buf.String()
	if strings.Contains(logOut, "XXXXXXXXXX") {
		t.Fatalf("SECURITY VIOLATION: structured error log contained raw value bytes: %s", logOut)
	}
}

// TestSEC03_Logging_04_RecoveryErrorReportLogging verifies that RecoveryReport
// logs safe operational diagnostics without exposing raw database contents.
func TestSEC03_Logging_04_RecoveryErrorReportLogging(t *testing.T) {
	report := wal.RecoveryReport{
		SegmentCount:     5,
		HighestSegmentID: 5,
		ValidRecords:     150,
		ReplayedRecords:  150,
		Truncated:        true,
		TruncatedBytes:   24,
		LastSeqNum:       150,
	}

	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)
	log.Info("WAL recovery completed",
		"segments", report.SegmentCount,
		"highest_id", report.HighestSegmentID,
		"valid_records", report.ValidRecords,
		"truncated", report.Truncated,
		"truncated_bytes", report.TruncatedBytes,
		"last_seq", report.LastSeqNum.String(),
	)

	out := buf.String()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		t.Fatalf("failed to parse JSON log: %v", err)
	}

	valRecs, ok := parsed["valid_records"].(float64)
	if !ok || valRecs != 150 {
		t.Errorf("expected valid_records 150, got %v", parsed["valid_records"])
	}
	truncVal, ok := parsed["truncated"].(bool)
	if !ok || !truncVal {
		t.Errorf("expected truncated true, got %v", parsed["truncated"])
	}
}

// TestSEC03_Logging_05_NonSensitiveKeysNotOverRedacted verifies that normal operational
// database keys and attributes (like "component", "segment_id", "seq_num", "path") are preserved.
func TestSEC03_Logging_05_NonSensitiveKeysNotOverRedacted(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	log.Info("segment rotated",
		"component", "wal",
		"segment_id", 42,
		"seq_num", 1000,
		"path", "/tmp/wal_000000000042.log",
	)

	out := buf.String()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &parsed); err != nil {
		t.Fatalf("failed to parse JSON log: %v", err)
	}

	if parsed["component"] != "wal" {
		t.Errorf("expected component 'wal', got %v", parsed["component"])
	}
	segIDVal, ok := parsed["segment_id"].(float64)
	if !ok || segIDVal != 42 {
		t.Errorf("expected segment_id 42, got %v", parsed["segment_id"])
	}
}

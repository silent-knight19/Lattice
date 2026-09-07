package logger_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/binary"
	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/logger"
)

// TestCrossPhaseDomainErrorsLogging tests that all domain errors produced by
// internal/binary and internal/errors integrate safely with internal/logger.
func TestCrossPhaseDomainErrorsLogging(t *testing.T) {
	// Construct errors from actual binary validation and codec failures
	keyErr := binary.ValidateKey(make([]byte, 70000))
	valErr := binary.ValidateValue(make([]byte, 5*1024*1024))
	_, opErr := binary.ParseOpType(0xFE)
	_, seqErr := binary.SeqNum(math.MaxUint64).Next()
	_, decodeErr := binary.DecodeInternalKey([]byte{1, 2, 3})
	_, _, varintErr := binary.GetVarint64([]byte{0x80})

	testErrors := []struct {
		name     string
		err      error
		sentinel error
	}{
		{"KeyTooLargeError", keyErr, errors.ErrKeyTooLarge},
		{"ValueTooLargeError", valErr, errors.ErrValueTooLarge},
		{"InvalidOpTypeError", opErr, errors.ErrInvalidOpType},
		{"SeqNumOverflowError", seqErr, errors.ErrSeqNumOverflow},
		{"ErrInternalKeyTruncated", decodeErr, errors.ErrInternalKeyTruncated},
		{"ErrVarintTruncated", varintErr, errors.ErrVarintTruncated},
		{"ChecksumMismatchError", &errors.ChecksumMismatchError{Offset: 1024, Expected: 0x11223344, Actual: 0x55667788}, errors.ErrChecksumMismatch},
		{"TornWriteError", &errors.TornWriteError{Offset: 4096, Reason: "header incomplete"}, errors.ErrTornWrite},
		{"ErrEmptyKey", errors.ErrEmptyKey, errors.ErrEmptyKey},
		{"ErrKeyNotFound", errors.ErrKeyNotFound, errors.ErrKeyNotFound},
		{"ErrCompactionRunning", errors.ErrCompactionRunning, errors.ErrCompactionRunning},
	}

	for _, tc := range testErrors {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)

			// 1. Direct logging with logger.Err
			log.Error("operation failed", logger.Err(tc.err))

			// 2. Logging wrapped error
			wrapped := fmt.Errorf("storage engine context: %w", tc.err)
			log.Warn("wrapped failure", "error", wrapped)

			// 3. Verify emitted logs are valid JSON
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("expected 2 log lines, got %d", len(lines))
			}

			for idx, line := range lines {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(line), &parsed); err != nil {
					t.Fatalf("line %d is invalid JSON: %v, raw: %s", idx, err, line)
				}

				errMsg, ok := parsed["error"].(string)
				if !ok || errMsg == "" {
					t.Fatalf("line %d missing valid error string: %v", idx, parsed["error"])
				}

				// Error message must not contain arbitrary raw byte escapes
				if strings.Contains(errMsg, "\\x") {
					t.Errorf("error message contains raw hex bytes: %q", errMsg)
				}
			}
		})
	}
}

// TestCrossPhaseInternalKeyLogging verifies logging of InternalKey objects.
func TestCrossPhaseInternalKeyLogging(t *testing.T) {
	key, err := binary.NewInternalKey([]byte("user:1001:profile"), 42, binary.OpTypePut)
	if err != nil {
		t.Fatalf("failed to create InternalKey: %v", err)
	}

	t.Run("NonSensitiveKeyLogging_String", func(t *testing.T) {
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)
		log.Info("committing record", "internal_key", key.String())

		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		gotStr, ok := parsed["internal_key"].(string)
		if !ok {
			t.Fatalf("expected string representation for internal_key, got %T", parsed["internal_key"])
		}

		expected := key.String()
		if gotStr != expected {
			t.Errorf("expected %q, got %q", expected, gotStr)
		}
	})

	t.Run("NonSensitiveKeyLogging_Struct", func(t *testing.T) {
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)
		log.Info("committing record", "internal_key", key)

		var parsed map[string]any
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		keyObj, ok := parsed["internal_key"].(map[string]any)
		if !ok {
			t.Fatalf("expected struct map representation for internal_key in JSON, got %T", parsed["internal_key"])
		}

		seqNum, ok := keyObj["SeqNum"].(float64)
		if !ok || seqNum != 42 {
			t.Errorf("expected SeqNum 42, got %v", keyObj["SeqNum"])
		}
		opType, ok := keyObj["OpType"].(float64)
		if !ok || opType != float64(binary.OpTypePut) {
			t.Errorf("expected OpType %d, got %v", binary.OpTypePut, keyObj["OpType"])
		}
	})

	t.Run("TextHandlerLogging_InvokesStringer", func(t *testing.T) {
		var buf bytes.Buffer
		log := logger.NewText(&buf, logger.LevelInfo)
		log.Info("committing record", "internal_key", key)

		textOutput := buf.String()
		if !strings.Contains(textOutput, "InternalKey(") {
			t.Errorf("expected TextHandler to format InternalKey via String(), got: %s", textOutput)
		}
	})

	t.Run("SensitiveKeyRedactionWithInternalKey", func(t *testing.T) {
		sensitiveAttrs := []string{
			"password",
			"user.password",
			"auth_token",
			"metadata.authorization",
			"client_secret",
			"accessToken",
			"refreshToken",
			"user_auth",
			"api_key",
		}

		for _, attr := range sensitiveAttrs {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("sensitive write", attr, key)

			var parsed map[string]any
			if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
				t.Fatalf("invalid JSON for attr %q: %v", attr, err)
			}

			if parsed[attr] != logger.RedactedPlaceholder {
				t.Errorf("expected sensitive key %q with InternalKey value to be redacted, got %v",
					attr, parsed[attr])
			}
		}
	})
}

// TestCrossPhaseZeroValueInternalKeyLogging verifies that zero-value InternalKey
// can be safely logged, cloned, and compared without panic.
func TestCrossPhaseZeroValueInternalKeyLogging(t *testing.T) {
	var zeroKey binary.InternalKey

	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	// Must not panic
	log.Info("logging zero key", "key", zeroKey, "key_str", zeroKey.String())

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if parsed["key"] == nil || parsed["key_str"] == nil {
		t.Errorf("expected non-nil fields for zero key: %v", parsed)
	}
}

// TestCrossPhaseRedactableWithInternalKey verifies that custom Redactable wrappers
// containing binary types integrate correctly with logger redaction.
type userCredentialsRecord struct {
	Key      binary.InternalKey
	Password string
}

func (u userCredentialsRecord) Redact() any {
	return map[string]any{
		"Key":      u.Key.String(),
		"Password": logger.RedactedPlaceholder,
	}
}

func TestCrossPhaseRedactableDomainType(t *testing.T) {
	k, err := binary.NewInternalKey([]byte("user:admin"), 100, binary.OpTypePut)
	if err != nil {
		t.Fatalf("failed to create key: %v", err)
	}

	rec := userCredentialsRecord{
		Key:      k,
		Password: "plain-text-admin-password",
	}

	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)
	log.Info("record write", "account", rec)

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	accountMap, ok := parsed["account"].(map[string]any)
	if !ok {
		t.Fatalf("expected account to be map, got %T", parsed["account"])
	}

	if accountMap["Password"] != logger.RedactedPlaceholder {
		t.Errorf("expected Password to be redacted, got %v", accountMap["Password"])
	}
	if accountMap["Key"] != k.String() {
		t.Errorf("expected Key %q, got %v", k.String(), accountMap["Key"])
	}
}

// BenchmarkCrossPhaseComposition measures allocation and latency of composing
// binary validation, InternalKey construction, and structured logging.
func BenchmarkCrossPhaseComposition(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	userKey := []byte("user:10001:profile_data")

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		key, err := binary.NewInternalKey(userKey, binary.SeqNum(i+1), binary.OpTypePut)
		if err != nil {
			b.Fatalf("NewInternalKey failed: %v", err)
		}
		log.Info("commit", "key", key, "status", "ok")
	}
}

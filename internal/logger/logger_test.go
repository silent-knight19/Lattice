package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	domainErrors "github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/logger"
)

// syncBuffer is a concurrency-safe bytes.Buffer wrapper for testing concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := strings.TrimSpace(s.b.String())
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func TestLogLevelFiltering(t *testing.T) {
	tests := []struct {
		configuredLevel logger.Level
		callLevel       logger.Level
		shouldLog       bool
	}{
		{logger.LevelDebug, logger.LevelDebug, true},
		{logger.LevelDebug, logger.LevelInfo, true},
		{logger.LevelInfo, logger.LevelDebug, false},
		{logger.LevelInfo, logger.LevelInfo, true},
		{logger.LevelInfo, logger.LevelWarn, true},
		{logger.LevelInfo, logger.LevelError, true},
		{logger.LevelWarn, logger.LevelInfo, false},
		{logger.LevelWarn, logger.LevelWarn, true},
		{logger.LevelWarn, logger.LevelError, true},
		{logger.LevelError, logger.LevelWarn, false},
		{logger.LevelError, logger.LevelError, true},
	}

	for _, tc := range tests {
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, tc.configuredLevel)

		switch tc.callLevel {
		case logger.LevelDebug:
			log.Debug("test message")
		case logger.LevelInfo:
			log.Info("test message")
		case logger.LevelWarn:
			log.Warn("test message")
		case logger.LevelError:
			log.Error("test message")
		}

		logged := buf.Len() > 0
		if logged != tc.shouldLog {
			t.Errorf("configured level %s, called %s: expected logged=%v, got logged=%v",
				tc.configuredLevel, tc.callLevel, tc.shouldLog, logged)
		}
	}
}

func TestJSONFormatting(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelDebug)

	log.Info("node joined cluster", "node_id", "node-1", "port", 9000)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected non-empty log output")
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(line), &data); err != nil {
		t.Fatalf("failed to parse log output as JSON: %v, raw output: %s", err, line)
	}

	if data["msg"] != "node joined cluster" {
		t.Errorf("expected msg 'node joined cluster', got %v", data["msg"])
	}
	if data["level"] != "INFO" {
		t.Errorf("expected level 'INFO', got %v", data["level"])
	}
	if data["node_id"] != "node-1" {
		t.Errorf("expected node_id 'node-1', got %v", data["node_id"])
	}
	if port, ok := data["port"].(float64); !ok || int(port) != 9000 {
		t.Errorf("expected port 9000, got %v", data["port"])
	}
	if _, ok := data["time"]; !ok {
		t.Error("expected time field in JSON log record")
	}
}

func TestTextFormatting(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewText(&buf, logger.LevelInfo)

	log.Info("starting engine", "storage_path", "/var/data")

	output := buf.String()
	if !strings.Contains(output, "level=INFO") {
		t.Errorf("expected output to contain level=INFO, got: %s", output)
	}
	if !strings.Contains(output, `msg="starting engine"`) && !strings.Contains(output, "msg=starting engine") {
		t.Errorf("expected output to contain msg for starting engine, got: %s", output)
	}
	if !strings.Contains(output, "storage_path=/var/data") {
		t.Errorf("expected output to contain storage_path=/var/data, got: %s", output)
	}
}

func TestWithComponent(t *testing.T) {
	var buf bytes.Buffer
	baseLog := logger.NewJSON(&buf, logger.LevelInfo)

	walLog := baseLog.WithComponent("wal")
	walLog.Info("flush initiated", "segment_id", 42)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if data["component"] != "wal" {
		t.Errorf("expected component 'wal', got %v", data["component"])
	}
	if data["msg"] != "flush initiated" {
		t.Errorf("expected msg 'flush initiated', got %v", data["msg"])
	}

	// Chaining With on component logger
	buf.Reset()
	scopedLog := walLog.With("subsystem_state", "active")
	scopedLog.Info("state update")

	var chainedData map[string]any
	if err := json.Unmarshal(buf.Bytes(), &chainedData); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if chainedData["component"] != "wal" {
		t.Errorf("expected component 'wal' preserved, got %v", chainedData["component"])
	}
	if chainedData["subsystem_state"] != "active" {
		t.Errorf("expected subsystem_state 'active', got %v", chainedData["subsystem_state"])
	}
}

func TestErrIntegration(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelError)

	stdErr := errors.New("underlying io error")
	log.Error("failed to write record", logger.Err(stdErr))

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if data["error"] != "underlying io error" {
		t.Errorf("expected error string 'underlying io error', got %v", data["error"])
	}

	// Test with domain error
	buf.Reset()
	domErr := &domainErrors.ChecksumMismatchError{
		Offset:   1024,
		Expected: 0x12345678,
		Actual:   0x87654321,
	}
	log.Error("integrity check failed", logger.Err(domErr))

	var domData map[string]any
	if err := json.Unmarshal(buf.Bytes(), &domData); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	errStr, ok := domData["error"].(string)
	if !ok {
		t.Fatalf("expected error attribute to be string, got %T", domData["error"])
	}
	if !strings.Contains(errStr, "checksum mismatch at offset 1024") {
		t.Errorf("expected domain error message in log, got %v", domData["error"])
	}

	// Test with nil error
	buf.Reset()
	log.Error("clean operation", logger.Err(nil))
	var nilData map[string]any
	if err := json.Unmarshal(buf.Bytes(), &nilData); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}
	if _, exists := nilData["error"]; exists {
		t.Errorf("expected error attribute to be absent when err is nil, got: %v", nilData["error"])
	}
}

func TestSensitiveFieldRedaction(t *testing.T) {
	sensitiveCases := []struct {
		key   string
		value string
	}{
		{"password", "supersecret123"},
		{"PassWord", "casedpassword"},
		{"secret", "sensitive-secret-token"},
		{"token", "jwt-token-value"},
		{"auth", "bearer-credential"},
		{"authorization", "Basic dXNlcjpwYXNz"},
		{"api_key", "sk_live_12345"},
		{"apikey", "sk_live_67890"},
		{"private_key", "-----BEGIN PRIVATE KEY-----"},
		{"credential", "admin_cred"},
		{"credentials", "multiple_creds"},
		{"access_token", "oauth_access_xyz"},
		{"refresh_token", "oauth_refresh_abc"},
	}

	for _, tc := range sensitiveCases {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)

			log.Info("authentication attempt", tc.key, tc.value, "user_id", "admin")

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			// Value for sensitive key must be redacted
			val, exists := data[tc.key]
			if !exists {
				t.Fatalf("expected attribute %q to be present", tc.key)
			}
			if val != logger.RedactedPlaceholder {
				t.Errorf("expected redacted placeholder for key %s, got %q", tc.key, val)
			}

			// Non-sensitive key must remain intact
			if data["user_id"] != "admin" {
				t.Errorf("expected user_id to remain intact as 'admin', got %v", data["user_id"])
			}
		})
	}
}

func TestCustomRedactedKeys(t *testing.T) {
	var buf bytes.Buffer
	log := logger.New(logger.Config{
		Level:        logger.LevelInfo,
		Format:       logger.FormatJSON,
		Output:       &buf,
		RedactedKeys: []string{"custom_cookie", "session_id"},
	})

	log.Info("session started", "custom_cookie", "secret-cookie-val", "session_id", "sess-999", "tenant", "prod")

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if data["custom_cookie"] != logger.RedactedPlaceholder {
		t.Errorf("expected custom_cookie to be redacted, got %v", data["custom_cookie"])
	}
	if data["session_id"] != logger.RedactedPlaceholder {
		t.Errorf("expected session_id to be redacted, got %v", data["session_id"])
	}
	if data["tenant"] != "prod" {
		t.Errorf("expected tenant to remain untouched, got %v", data["tenant"])
	}
}

type testRedactableObject struct {
	Name   string
	Secret string
}

func (r testRedactableObject) Redact() any {
	return map[string]string{
		"Name":   r.Name,
		"Secret": "[SAFE_REDACTED]",
	}
}

func TestRedactableInterface(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	obj := testRedactableObject{
		Name:   "cluster-agent",
		Secret: "raw-super-secret",
	}

	log.Info("registering service", "service_info", obj)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	serviceInfo, ok := data["service_info"].(map[string]any)
	if !ok {
		t.Fatalf("expected service_info to be an object, got %T: %v", data["service_info"], data["service_info"])
	}

	if serviceInfo["Secret"] != "[SAFE_REDACTED]" {
		t.Errorf("expected custom Redact() method to be called, got Secret=%v", serviceInfo["Secret"])
	}
	if serviceInfo["Name"] != "cluster-agent" {
		t.Errorf("expected Name to be 'cluster-agent', got %v", serviceInfo["Name"])
	}
}

func TestNewNop(t *testing.T) {
	nop := logger.NewNop()

	// Should not panic on any log calls
	nop.Debug("debug msg", "key", "val")
	nop.Info("info msg", "key", "val")
	nop.Warn("warn msg", "key", "val")
	nop.Error("error msg", "key", "val")

	ctx := context.Background()
	nop.DebugContext(ctx, "debug ctx")
	nop.InfoContext(ctx, "info ctx")
	nop.WarnContext(ctx, "warn ctx")
	nop.ErrorContext(ctx, "error ctx")

	// Chaining should still return a functional nop logger
	scoped := nop.WithComponent("wal").With("key", "val")
	scoped.Info("scoped info")
}

func TestContextLogging(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelDebug)
	ctx := context.Background()

	log.DebugContext(ctx, "debug event", "step", 1)
	log.InfoContext(ctx, "info event", "step", 2)
	log.WarnContext(ctx, "warn event", "step", 3)
	log.ErrorContext(ctx, "error event", "step", 4)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 log lines, got %d", len(lines))
	}
}

func TestConcurrentLogging(t *testing.T) {
	sBuf := &syncBuffer{}
	log := logger.NewJSON(sBuf, logger.LevelInfo)

	const goroutines = 100
	const messagesPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			workerLog := log.WithComponent("worker").With("worker_id", workerID)
			for j := 0; j < messagesPerGoroutine; j++ {
				workerLog.Info("worker heartbeat", "seq", j, "password", "leak-attempt")
			}
		}()
	}

	wg.Wait()

	lines := sBuf.Lines()
	expectedTotal := goroutines * messagesPerGoroutine
	if len(lines) != expectedTotal {
		t.Fatalf("expected %d log lines, got %d", expectedTotal, len(lines))
	}

	// Verify that each emitted line is valid JSON and passwords were redacted concurrently
	for idx, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d is corrupted/invalid JSON: %v, raw: %s", idx, err, line)
		}
		if entry["password"] != logger.RedactedPlaceholder {
			t.Fatalf("line %d had unredacted password: %v", idx, entry["password"])
		}
	}
}

func TestLevelAndFormatStrings(t *testing.T) {
	levels := []struct {
		l        logger.Level
		expected string
	}{
		{logger.LevelDebug, "DEBUG"},
		{logger.LevelInfo, "INFO"},
		{logger.LevelWarn, "WARN"},
		{logger.LevelError, "ERROR"},
		{logger.Level(999), "UNKNOWN"},
	}

	for _, tc := range levels {
		if tc.l.String() != tc.expected {
			t.Errorf("level %d: expected string %q, got %q", tc.l, tc.expected, tc.l.String())
		}
	}

	formats := []struct {
		f        logger.Format
		expected string
	}{
		{logger.FormatJSON, "json"},
		{logger.FormatText, "text"},
		{logger.Format(999), "unknown"},
	}

	for _, tc := range formats {
		if tc.f.String() != tc.expected {
			t.Errorf("format %d: expected string %q, got %q", tc.f, tc.expected, tc.f.String())
		}
	}
}

func TestNilOutputDefaultsToStdout(t *testing.T) {
	// Must not panic when output is nil
	log := logger.New(logger.Config{
		Level:  logger.LevelInfo,
		Format: logger.FormatJSON,
		Output: nil,
	})

	if log == nil {
		t.Fatal("expected non-nil logger instance")
	}
}

func TestEmptyWriterDiscard(t *testing.T) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	log.Info("writing to discard", "status", "ok")
}

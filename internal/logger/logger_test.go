package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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

func TestCompoundSensitiveKeyRedaction(t *testing.T) {
	compoundKeys := []struct {
		key   string
		value string
	}{
		{"db_password", "super-secret-db-pass"},
		{"client_secret", "oauth-client-secret-999"},
		{"auth_token", "bearer-token-val"},
		{"session_token", "session-token-xyz"},
		{"api-key", "api-key-with-hyphen"},
		{"private-key", "private-key-with-hyphen"},
		{"jwt_token", "jwt-token-string"},
	}

	for _, tc := range compoundKeys {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("compound key test", tc.key, tc.value, "token_count", 42)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			if data[tc.key] != logger.RedactedPlaceholder {
				t.Errorf("expected key %q to be redacted, got %v", tc.key, data[tc.key])
			}
			// Verify token_count was NOT mistakenly redacted
			if tc.key != "token_count" {
				if count, ok := data["token_count"].(float64); !ok || int(count) != 42 {
					t.Errorf("expected token_count to remain intact as 42, got %v", data["token_count"])
				}
			}
		})
	}
}

func TestSensitiveRedactionBypassReproducer(t *testing.T) {
	bypassKeys := []struct {
		key   string
		value string
	}{
		{"metadata.authorization", "Bearer top-secret-token"},
		{"user.token", "user-session-token"},
		{"user:token", "user-colon-token"},
		{"user/token", "user-slash-token"},
		{"accessToken", "oauth-access-12345"},
		{"refreshToken", "oauth-refresh-67890"},
		{"authToken", "auth-token-val"},
		{"bearerToken", "bearer-token-val"},
		{"http_authorization", "Basic dXNlcjpwYXNz"},
		{"user_auth", "user-auth-secret"},
		{"user.auth", "user-dot-auth-secret"},
	}

	for _, tc := range bypassKeys {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("bypass test", tc.key, tc.value, "token_count", 42)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			if data[tc.key] != logger.RedactedPlaceholder {
				t.Errorf("REDACTION BYPASS DETECTED for key %q: got %q, want %q",
					tc.key, data[tc.key], logger.RedactedPlaceholder)
			}
		})
	}
}

func TestAdversarialObfuscationAndKeyRedaction(t *testing.T) {
	testCases := []struct {
		name  string
		key   string
		value any
	}{
		{"nested dotted password", "user.password", "secret123"},
		{"bracketed password", "user[password]", "secret123"},
		{"colons and slashes", "config://auth/token", "secret123"},
		{"leading trailing punctuation", ":::user:::password:::", "secret123"},
		{"whitespace and dashes", "  --PASSWORD--  ", "secret123"},
		{"PascalCase APIKey", "APIKey", "secret123"},
		{"PascalCase PrivateKey", "PrivateKey", "secret123"},
		{"PascalCase ClientSecret", "ClientSecret", "secret123"},
		{"PascalCase SessionToken", "SessionToken", "secret123"},
		{"deeply nested compound", "request.oauth_access_token", "secret123"},
		{"nested api key value", "nested.api_key.value", "secret123"},
		{"byte slice secret value", "user_secret", []byte("raw-bytes-secret")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("adversarial test", tc.key, tc.value)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			if data[tc.key] != logger.RedactedPlaceholder {
				t.Errorf("expected key %q to be redacted, got %v", tc.key, data[tc.key])
			}
		})
	}
}

func TestFalsePositiveResistance(t *testing.T) {
	benignCases := []struct {
		key   string
		value any
	}{
		{"token_count", 42},
		{"tokens_per_sec", 1000},
		{"author", "Shakespeare"},
		{"authority", "root-ca"},
		{"authenticate_user_flag", true},
	}

	for _, tc := range benignCases {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("benign test", tc.key, tc.value)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			if data[tc.key] == logger.RedactedPlaceholder {
				t.Errorf("false positive: benign key %q was incorrectly redacted!", tc.key)
			}
		})
	}
}

type typedNilRedactable struct {
	Secret string
}

func (n *typedNilRedactable) Redact() any {
	return map[string]string{"Secret": n.Secret}
}

func TestNilRedactablePointer(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	var nilObj *typedNilRedactable = nil

	// Non-sensitive key: must not panic when logging typed nil Redactable, serializes as nil/null
	log.Info("testing nil redactable", "nil_redactable", nilObj)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	if data["nil_redactable"] != nil {
		t.Errorf("expected nil_redactable to be nil/null, got %v", data["nil_redactable"])
	}
}

func TestTypedNilRedactablePrecedence(t *testing.T) {
	var nilObj *typedNilRedactable = nil

	// Case A: Sensitive key + typed-nil Redactable
	// Must immediately redact with [REDACTED] and not panic
	{
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)
		log.Info("sensitive typed-nil test", "password", nilObj)

		var data map[string]any
		if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}
		if data["password"] != logger.RedactedPlaceholder {
			t.Errorf("expected sensitive key with typed-nil to be %q, got %v", logger.RedactedPlaceholder, data["password"])
		}
	}

	// Case B: Non-sensitive key + typed-nil Redactable
	// Evaluated safely without panic, serialized as nil/null
	{
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)
		log.Info("non-sensitive typed-nil test", "storage_object", nilObj)

		var data map[string]any
		if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}
		if data["storage_object"] != nil {
			t.Errorf("expected non-sensitive key with typed-nil to be nil/null, got %v", data["storage_object"])
		}
	}
}

type hostileRedactable struct {
	invoked *bool
	secret  string
}

func (h hostileRedactable) Redact() any {
	if h.invoked != nil {
		*h.invoked = true
	}
	return h.secret
}

func TestSensitiveKeyPrecedenceOverHostileRedactable(t *testing.T) {
	sensitiveKeys := []string{
		"password",
		"user.password",
		"authorization",
		"metadata.authorization",
		"accessToken",
		"refreshToken",
		"user_auth",
		"http_authorization",
		"auth_token",
	}

	for _, key := range sensitiveKeys {
		t.Run(key, func(t *testing.T) {
			invoked := false
			obj := hostileRedactable{
				invoked: &invoked,
				secret:  "EXPOSED-LEAKED-SECRET-42",
			}

			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("sensitive operation", key, obj)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			// 1. Output must be [REDACTED]
			if data[key] != logger.RedactedPlaceholder {
				t.Errorf("expected key %q to be redacted with placeholder, got %v", key, data[key])
			}

			// 2. Secret must NOT appear anywhere in the output
			if strings.Contains(buf.String(), "EXPOSED-LEAKED-SECRET-42") {
				t.Errorf("secret leaked in log output for key %q: %s", key, buf.String())
			}

			// 3. Redact() must NOT be invoked when the attribute key is sensitive
			if invoked {
				t.Errorf("SECURITY DEFECT: Redact() was invoked for sensitive key %q!", key)
			}
		})
	}
}

type trackingSafeRedactable struct {
	invoked *bool
	label   string
	secret  string
}

func (s trackingSafeRedactable) Redact() any {
	if s.invoked != nil {
		*s.invoked = true
	}
	return map[string]string{
		"label":  s.label,
		"secret": "[SAFE_SCRUBBED]",
	}
}

func TestNonSensitiveKeyRedactableExecution(t *testing.T) {
	nonSensitiveKeys := []string{
		"internal_key",
		"storage_object",
		"component_state",
	}

	for _, key := range nonSensitiveKeys {
		t.Run(key, func(t *testing.T) {
			invoked := false
			obj := trackingSafeRedactable{
				invoked: &invoked,
				label:   "state-valid",
				secret:  "raw-in-memory-state",
			}

			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("safe operation", key, obj)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			// 1. Redact() must be invoked
			if !invoked {
				t.Errorf("expected Redact() to be invoked for non-sensitive key %q", key)
			}

			// 2. Output must contain safe scrubbed representation
			valObj, ok := data[key].(map[string]any)
			if !ok {
				t.Fatalf("expected %q to be an object, got %T: %v", key, data[key], data[key])
			}
			if valObj["secret"] != "[SAFE_SCRUBBED]" {
				t.Errorf("expected safe scrubbed secret, got %v", valObj["secret"])
			}
			if valObj["label"] != "state-valid" {
				t.Errorf("expected label state-valid, got %v", valObj["label"])
			}

			// 3. Raw secret must NOT appear
			if strings.Contains(buf.String(), "raw-in-memory-state") {
				t.Errorf("raw secret leaked in log output for key %q", key)
			}
		})
	}
}

type panickingRedactable struct {
	panicMsg string
}

func (p panickingRedactable) Redact() any {
	if p.panicMsg != "" {
		panic(p.panicMsg)
	}
	panic("exploit attempt in custom Redact()")
}

func TestPanickingRedactablePrecedence(t *testing.T) {
	// Case A: Sensitive key + panicking Redactable
	// Redact() MUST NOT be invoked, so panic MUST NEVER OCCUR.
	sensitiveKeys := []string{
		"password",
		"auth_token",
		"metadata.authorization",
	}

	for _, key := range sensitiveKeys {
		t.Run("sensitive_"+key, func(t *testing.T) {
			obj := panickingRedactable{
				panicMsg: "CRITICAL FAILURE: Redact() was illegally called on sensitive key!",
			}

			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)

			// If Redact() was invoked, this would panic and fail the test.
			log.Info("sensitive operation with panicking object", key, obj)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			if data[key] != logger.RedactedPlaceholder {
				t.Errorf("expected key %q to be redacted with placeholder, got %v", key, data[key])
			}
		})
	}

	// Case B: Non-sensitive key + panicking Redactable
	// Redact() is invoked, panics, safeRedact catches it, returns [REDACTED], no process crash.
	t.Run("non_sensitive_recovery", func(t *testing.T) {
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)

		log.Info("testing panicking redactable recovery", "storage_object", panickingRedactable{})

		var data map[string]any
		if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
			t.Fatalf("failed to parse JSON: %v", err)
		}

		if data["storage_object"] != logger.RedactedPlaceholder {
			t.Errorf("expected recovered panicking redactable to be masked with placeholder, got %v", data["storage_object"])
		}
	})
}

func TestFalsePositiveKeysWithRedactable(t *testing.T) {
	benignKeys := []string{
		"author",
		"authority",
		"authenticate_user_flag",
		"token_count",
		"tokens_per_sec",
	}

	for _, key := range benignKeys {
		t.Run(key, func(t *testing.T) {
			invoked := false
			obj := trackingSafeRedactable{
				invoked: &invoked,
				label:   "metric-valid",
				secret:  "safe-metric-detail",
			}

			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("benign test with redactable", key, obj)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			// Redact() should be called
			if !invoked {
				t.Errorf("expected Redact() to be invoked for benign key %q", key)
			}

			// Output should be safe representation, NOT [REDACTED]
			if data[key] == logger.RedactedPlaceholder {
				t.Errorf("false positive: benign key %q was incorrectly masked as [REDACTED]", key)
			}

			valObj, ok := data[key].(map[string]any)
			if !ok {
				t.Fatalf("expected %q to be an object, got %T: %v", key, data[key], data[key])
			}
			if valObj["label"] != "metric-valid" {
				t.Errorf("expected label metric-valid, got %v", valObj["label"])
			}
		})
	}
}

type concurrentHostileRedactable struct {
	invoked *atomic.Int64
	secret  string
}

func (c concurrentHostileRedactable) Redact() any {
	if c.invoked != nil {
		c.invoked.Add(1)
	}
	return c.secret
}

type concurrentSafeRedactable struct {
	invoked *atomic.Int64
	val     string
}

func (c concurrentSafeRedactable) Redact() any {
	if c.invoked != nil {
		c.invoked.Add(1)
	}
	return map[string]string{"safe": c.val}
}

func TestConcurrentRedactionPrecedence(t *testing.T) {
	const goroutines = 50
	const iterations = 100

	var sensitiveInvocations atomic.Int64
	var nonSensitiveInvocations atomic.Int64

	var buf syncBuffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Sensitive key: Redact() MUST NOT be called
				hostile := concurrentHostileRedactable{
					invoked: &sensitiveInvocations,
					secret:  "CONCURRENT-HOSTILE-SECRET-DATA",
				}
				log.Info("concurrent sensitive", "password", hostile, "worker", workerID)

				// Non-sensitive key: Redact() MUST be called
				safe := concurrentSafeRedactable{
					invoked: &nonSensitiveInvocations,
					val:     "safe-state",
				}
				log.Info("concurrent non-sensitive", "storage_object", safe, "worker", workerID)
			}
		}(g)
	}

	wg.Wait()

	// 1. Sensitive key Redact() must NEVER have been invoked
	if got := sensitiveInvocations.Load(); got != 0 {
		t.Fatalf("SECURITY VIOLATION: sensitive key Redact() was invoked %d times concurrently!", got)
	}

	// 2. Non-sensitive key Redact() must have been invoked exactly once per non-sensitive log call
	expectedNonSensitive := int64(goroutines * iterations)
	if got := nonSensitiveInvocations.Load(); got != expectedNonSensitive {
		t.Fatalf("expected non-sensitive Redact() to be invoked %d times, got %d", expectedNonSensitive, got)
	}

	// 3. Sensitive secret must NOT appear anywhere in the output
	rawOutput := buf.String()
	if strings.Contains(rawOutput, "CONCURRENT-HOSTILE-SECRET-DATA") {
		t.Fatal("CONCURRENT-HOSTILE-SECRET-DATA leaked in concurrent log buffer!")
	}

	// 4. Safe representation must be present
	if !strings.Contains(rawOutput, "safe-state") {
		t.Fatal("expected safe-state to be present in concurrent log buffer")
	}

	// 5. Verify all lines are valid JSON
	lines := buf.Lines()
	for idx, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d is not valid JSON: %v, raw: %s", idx, err, line)
		}
		if pwd, ok := entry["password"]; ok {
			if pwd != logger.RedactedPlaceholder {
				t.Fatalf("line %d: password expected %q, got %v", idx, logger.RedactedPlaceholder, pwd)
			}
		}
	}
}

type errWriter struct {
	err error
}

func (w *errWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

func TestFailingWriterDoesNotPanic(t *testing.T) {
	w := &errWriter{err: errors.New("simulated disk I/O error")}
	log := logger.NewJSON(w, logger.LevelInfo)

	// Writing to failing writer must not panic
	log.Info("message to failing writer", "key", "val")
}

func TestMalformedArguments(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	// Odd number of arguments: must not panic
	log.Info("odd arguments", "key_only")

	// Non-string keys: must not panic
	log.Info("non-string key", 12345, "val")

	if buf.Len() == 0 {
		t.Errorf("expected log output to be produced despite malformed arguments")
	}
}

func TestHugeAttributePayload(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	hugePayload := strings.Repeat("X", 1024*1024) // 1 MB
	log.Info("huge payload", "data", hugePayload)

	if buf.Len() < 1024*1024 {
		t.Errorf("expected output buffer to contain 1MB payload")
	}
}

func BenchmarkNewNop(b *testing.B) {
	log := logger.NewNop()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info("nop heartbeat", "seq", i, "key", "val")
	}
}

func BenchmarkJSON_NoRedaction(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info("standard message", "component", "storage", "seq", i)
	}
}

func BenchmarkJSON_WithRedaction(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info("auth event", "user.password", "secret123", "token", "tok123")
	}
}

func BenchmarkJSON_Parallel(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			log.Info("concurrent log", "worker", i, "token", "secret-token")
		}
	})
}

func BenchmarkJSON_SensitiveKey_RedactableValue(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	obj := testRedactableObject{Name: "bench", Secret: "secret"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info("bench record", "password", obj)
	}
}

func BenchmarkJSON_NonSensitiveKey_RedactableValue(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	obj := testRedactableObject{Name: "bench", Secret: "secret"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info("bench record", "storage_object", obj)
	}
}

func BenchmarkJSON_SensitiveKey_OrdinaryValue(b *testing.B) {
	log := logger.NewJSON(io.Discard, logger.LevelInfo)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info("bench record", "password", "raw-secret-string")
	}
}

func FuzzLoggerKeyRedaction(f *testing.F) {
	seeds := []struct {
		key string
		val string
	}{
		{"password", "p1"},
		{"user.password", "p2"},
		{"metadata.authorization", "Bearer tok"},
		{"accessToken", "tok3"},
		{"token_count", "100"},
		{"author", "test"},
		{"nested:auth:token", "tok4"},
		{"", ""},
		{"---", "---"},
		{"__proto__", "exploit"},
		{"very_long_key_" + strings.Repeat("A", 1000), "val"},
	}

	for _, s := range seeds {
		f.Add(s.key, s.val)
	}

	f.Fuzz(func(t *testing.T, key string, val string) {
		var buf bytes.Buffer
		log := logger.NewJSON(&buf, logger.LevelInfo)
		// Must not panic or crash
		log.Info("fuzz record", key, val)

		// If output produced, must be valid JSON
		if buf.Len() > 0 {
			var parsed map[string]any
			if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
				t.Fatalf("fuzz generated invalid JSON: %v, raw: %s", err, buf.String())
			}
		}
	})
}

package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/logger"
)

type customStringerWithToken struct {
	token string
}

func (c customStringerWithToken) String() string {
	return fmt.Sprintf("custom_obj(user=admin, token=%s)", c.token)
}

type customStringerWithBearer struct {
	jwt string
}

func (c customStringerWithBearer) String() string {
	return fmt.Sprintf("Authorization: Bearer %s", c.jwt)
}

type userCredentialsStruct struct {
	Username string `json:"username"`
	Password string `json:"password"`
	APIKey   string `json:"api_key"`
}

func TestVULN004_NestedMapRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	// Nested map matching the exact proof-of-concept from VULN-004
	nestedPayload := map[string]any{
		"request": map[string]string{
			"user_token": "s3cr3t_t0k3n",
			"username":   "alice",
		},
	}

	log.Info("operation", "payload", nestedPayload)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	payload, ok := data["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload to be map, got %T: %v", data["payload"], data["payload"])
	}

	req, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("expected request to be map, got %T: %v", payload["request"], payload["request"])
	}

	if req["user_token"] != logger.RedactedPlaceholder {
		t.Errorf("expected user_token to be redacted, got: %v", req["user_token"])
	}
	if req["username"] != "alice" {
		t.Errorf("expected username to remain alice, got: %v", req["username"])
	}

	if strings.Contains(buf.String(), "s3cr3t_t0k3n") {
		t.Errorf("raw secret s3cr3t_t0k3n leaked in log output: %s", buf.String())
	}
}

func TestVULN004_NestedSliceRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	nestedSlice := []any{
		map[string]any{
			"id":       1,
			"password": "pass_one_secret",
		},
		map[string]any{
			"id":     2,
			"secret": "secret_two_val",
		},
	}

	log.Info("batch_operation", "items", nestedSlice)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	items, ok := data["items"].([]any)
	if !ok {
		t.Fatalf("expected items to be slice, got %T: %v", data["items"], data["items"])
	}

	item0 := items[0].(map[string]any)
	if item0["password"] != logger.RedactedPlaceholder {
		t.Errorf("expected item 0 password to be redacted, got: %v", item0["password"])
	}

	item1 := items[1].(map[string]any)
	if item1["secret"] != logger.RedactedPlaceholder {
		t.Errorf("expected item 1 secret to be redacted, got: %v", item1["secret"])
	}

	if strings.Contains(buf.String(), "pass_one_secret") || strings.Contains(buf.String(), "secret_two_val") {
		t.Errorf("raw secrets leaked in slice log output: %s", buf.String())
	}
}

func TestVULN004_CustomStringerRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	cs := customStringerWithToken{token: "raw_token_xyz_999"}
	log.Info("stringer_event", "context", cs)

	if strings.Contains(buf.String(), "raw_token_xyz_999") {
		t.Errorf("raw secret token leaked from custom Stringer: %s", buf.String())
	}
	if !strings.Contains(buf.String(), logger.RedactedPlaceholder) {
		t.Errorf("expected %s in output, got: %s", logger.RedactedPlaceholder, buf.String())
	}

	// Test Bearer token Stringer
	buf.Reset()
	bearerObj := customStringerWithBearer{jwt: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"}
	log.Info("bearer_event", "auth", bearerObj)

	if strings.Contains(buf.String(), "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9") {
		t.Errorf("raw bearer token leaked from custom Stringer: %s", buf.String())
	}
}

func TestVULN004_StructFieldRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	creds := userCredentialsStruct{
		Username: "admin_user",
		Password: "super_secret_password_123",
		APIKey:   "sk_test_51MzZ",
	}

	log.Info("credentials_event", "account_info", creds)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v", err)
	}

	credMap, ok := data["account_info"].(map[string]any)
	if !ok {
		t.Fatalf("expected account_info to be map, got %T: %v (raw JSON: %s)", data["account_info"], data["account_info"], buf.String())
	}

	if credMap["password"] != logger.RedactedPlaceholder {
		t.Errorf("expected password field to be redacted, got: %v", credMap["password"])
	}
	if credMap["api_key"] != logger.RedactedPlaceholder {
		t.Errorf("expected api_key field to be redacted, got: %v", credMap["api_key"])
	}
	if credMap["username"] != "admin_user" {
		t.Errorf("expected username to remain intact, got: %v", credMap["username"])
	}

	if strings.Contains(buf.String(), "super_secret_password_123") || strings.Contains(buf.String(), "sk_test_51MzZ") {
		t.Errorf("raw credentials leaked from struct: %s", buf.String())
	}
}

// TestSEC002_RecursiveSlogGroupRedaction tests multi-level nested slog.Group attributes.
func TestSEC002_RecursiveSlogGroupRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := logger.NewJSON(&buf, logger.LevelInfo)

	// Nesting: outer group -> middle group -> inner group with both sensitive and benign keys
	log.Info("nested_event",
		slog.Group("cluster",
			slog.String("region", "us-east-1"),
			slog.Group("auth_context",
				slog.String("user", "admin"),
				slog.String("pwd", "pass_nested_123"),
				slog.Group("crypto",
					slog.String("algorithm", "AES-256-GCM"),
					slog.String("master_key", "mk_secret_val"),
					slog.String("seed", "twelve word seed phrase goes here"),
				),
			),
		),
	)

	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("failed to parse JSON: %v, raw: %s", err, buf.String())
	}

	cluster, ok := data["cluster"].(map[string]any)
	if !ok {
		t.Fatalf("expected cluster group to be map, got %T: %v", data["cluster"], data["cluster"])
	}
	if cluster["region"] != "us-east-1" {
		t.Errorf("expected region to be us-east-1, got: %v", cluster["region"])
	}

	authCtx, ok := cluster["auth_context"].(map[string]any)
	if !ok {
		t.Fatalf("expected auth_context group to be map, got %T: %v", cluster["auth_context"], cluster["auth_context"])
	}
	if authCtx["user"] != "admin" {
		t.Errorf("expected user to be admin, got: %v", authCtx["user"])
	}
	if authCtx["pwd"] != logger.RedactedPlaceholder {
		t.Errorf("expected pwd in group to be %s, got: %v", logger.RedactedPlaceholder, authCtx["pwd"])
	}

	cryptoGrp, ok := authCtx["crypto"].(map[string]any)
	if !ok {
		t.Fatalf("expected crypto group to be map, got %T: %v", authCtx["crypto"], authCtx["crypto"])
	}
	if cryptoGrp["algorithm"] != "AES-256-GCM" {
		t.Errorf("expected algorithm to remain intact, got: %v", cryptoGrp["algorithm"])
	}
	if cryptoGrp["master_key"] != logger.RedactedPlaceholder {
		t.Errorf("expected master_key in nested group to be %s, got: %v", logger.RedactedPlaceholder, cryptoGrp["master_key"])
	}
	if cryptoGrp["seed"] != logger.RedactedPlaceholder {
		t.Errorf("expected seed in nested group to be %s, got: %v", logger.RedactedPlaceholder, cryptoGrp["seed"])
	}

	rawOut := buf.String()
	if strings.Contains(rawOut, "pass_nested_123") || strings.Contains(rawOut, "mk_secret_val") || strings.Contains(rawOut, "twelve word seed phrase") {
		t.Errorf("nested secrets leaked into raw log output: %s", rawOut)
	}
}

// TestSEC002_ExpandedSensitiveKeywords tests each of the newly added stems.
func TestSEC002_ExpandedSensitiveKeywords(t *testing.T) {
	testCases := []struct {
		key      string
		rawVal   string
		expected string
	}{
		{"pwd", "pass123", logger.RedactedPlaceholder},
		{"user_pwd", "pass456", logger.RedactedPlaceholder},
		{"admin_pwd", "pass789", logger.RedactedPlaceholder},
		{"passphrase", "secret_passphrase", logger.RedactedPlaceholder},
		{"cvc", "999", logger.RedactedPlaceholder},
		{"card_cvc", "888", logger.RedactedPlaceholder},
		{"mnemonic", "apple banana cherry dog elephant fox grape hat", logger.RedactedPlaceholder},
		{"seed", "secret_crypto_seed", logger.RedactedPlaceholder},
		{"secret_seed", "seed123", logger.RedactedPlaceholder},
		{"secret_key", "sk_live_12345", logger.RedactedPlaceholder},
		{"master_key", "mk_live_99999", logger.RedactedPlaceholder},
		{"signing_key", "sign_priv_001", logger.RedactedPlaceholder},
		{"encryption_key", "enc_aes_key", logger.RedactedPlaceholder},
		{"access_key", "AKIAIOSFODNN7EXAMPLE", logger.RedactedPlaceholder},
		{"set_cookie", "session=abc; Path=/", logger.RedactedPlaceholder},
		{"session_id", "sess_987654321", logger.RedactedPlaceholder},
	}

	for _, tc := range testCases {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("keyword_test", tc.key, tc.rawVal)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v, raw: %s", err, buf.String())
			}

			if data[tc.key] != tc.expected {
				t.Errorf("key %q: expected %q, got %q", tc.key, tc.expected, data[tc.key])
			}
			if strings.Contains(buf.String(), tc.rawVal) {
				t.Errorf("raw secret value leaked in output for key %q: %s", tc.key, buf.String())
			}
		})
	}
}

// TestSEC002_MessageStringScrubbing verifies secrets formatted into msg strings are scrubbed.
func TestSEC002_MessageStringScrubbing(t *testing.T) {
	testCases := []struct {
		name      string
		logFn     func(log logger.Logger, msg string)
		msg       string
		mustScrub string
	}{
		{
			name: "Info with password in msg",
			logFn: func(log logger.Logger, msg string) {
				log.Info(msg)
			},
			msg:       "auth failed for user admin: password=supersecret123 was rejected",
			mustScrub: "supersecret123",
		},
		{
			name: "Warn with pwd in msg",
			logFn: func(log logger.Logger, msg string) {
				log.Warn(msg)
			},
			msg:       "connection retry with pwd=retrypass999 failed",
			mustScrub: "retrypass999",
		},
		{
			name: "Error with Bearer token in msg",
			logFn: func(log logger.Logger, msg string) {
				log.Error(msg)
			},
			msg:       "unauthorized request with header Bearer eyJhbGciOiJIUzI1NiJ9",
			mustScrub: "eyJhbGciOiJIUzI1NiJ9",
		},
		{
			name: "DebugContext with seed in msg",
			logFn: func(log logger.Logger, msg string) {
				log.DebugContext(context.Background(), msg)
			},
			msg:       "wallet initialized with seed=mysecretseedphrase successfully",
			mustScrub: "mysecretseedphrase",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelDebug)
			tc.logFn(log, tc.msg)

			raw := buf.String()
			if strings.Contains(raw, tc.mustScrub) {
				t.Errorf("raw secret %q leaked in log msg: %s", tc.mustScrub, raw)
			}
			if !strings.Contains(raw, logger.RedactedPlaceholder) {
				t.Errorf("expected placeholder %s in log msg, got: %s", logger.RedactedPlaceholder, raw)
			}
		})
	}
}

// TestSEC002_StorageEngineBenignKeysPreserved ensures storage engine keys are never falsely redacted.
func TestSEC002_StorageEngineBenignKeysPreserved(t *testing.T) {
	benignKeys := []struct {
		key   string
		value any
	}{
		{"user_key", "table_prefix_001"},
		{"smallest_key", "aaa"},
		{"largest_key", "zzz"},
		{"key_count", 4096},
		{"key_len", 32},
		{"has_prev_key", true},
		{"primary_key_index", 1},
	}

	for _, tc := range benignKeys {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.NewJSON(&buf, logger.LevelInfo)
			log.Info("engine_metric", tc.key, tc.value)

			var data map[string]any
			if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
				t.Fatalf("failed to parse JSON: %v, raw: %s", err, buf.String())
			}

			if data[tc.key] == logger.RedactedPlaceholder {
				t.Errorf("false positive: benign storage key %q was incorrectly redacted!", tc.key)
			}
		})
	}
}

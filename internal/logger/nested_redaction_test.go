package logger_test

import (
	"bytes"
	"encoding/json"
	"fmt"
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

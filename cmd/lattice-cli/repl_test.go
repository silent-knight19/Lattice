package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/transport"
)

// mockClient implements LatticeClient for unit testing command execution and REPL handling.
type mockClient struct {
	executeFn func(req *transport.Request) (*transport.Response, error)
	closed    bool
}

func (m *mockClient) Execute(req *transport.Request) (*transport.Response, error) {
	if m.executeFn != nil {
		return m.executeFn(req)
	}
	return &transport.Response{
		OpCode: req.OpCode,
		Status: transport.StatusOk,
		SeqID:  req.SeqID,
	}, nil
}

func (m *mockClient) Close() error {
	m.closed = true
	return nil
}

// -----------------------------------------------------------------------------
// 1. Config & Flag Parsing Tests
// -----------------------------------------------------------------------------

func TestParseCLIFlags_Defaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, isHelp, err := ParseCLIFlags([]string{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isHelp {
		t.Fatalf("expected isHelp=false for empty args")
	}
	if cfg.Address != "127.0.0.1:9099" {
		t.Errorf("expected default address 127.0.0.1:9099, got %q", cfg.Address)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("expected default timeout 5s, got %v", cfg.Timeout)
	}
	if cfg.NoColor {
		t.Errorf("expected default NoColor=false")
	}
}

func TestParseCLIFlags_Overrides(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{
		"--address", "10.0.0.1:8080",
		"--timeout", "2s",
		"--no-color",
	}
	cfg, isHelp, err := ParseCLIFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isHelp {
		t.Fatalf("expected isHelp=false")
	}
	if cfg.Address != "10.0.0.1:8080" {
		t.Errorf("expected address 10.0.0.1:8080, got %q", cfg.Address)
	}
	if cfg.Timeout != 2*time.Second {
		t.Errorf("expected timeout 2s, got %v", cfg.Timeout)
	}
	if !cfg.NoColor {
		t.Errorf("expected NoColor=true")
	}
}

func TestParseCLIFlags_PortOverride(t *testing.T) {
	var stdout, stderr bytes.Buffer
	args := []string{"--address", "127.0.0.1", "--port", "9999"}
	cfg, _, err := ParseCLIFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Address != "127.0.0.1:9999" {
		t.Errorf("expected address 127.0.0.1:9999, got %q", cfg.Address)
	}
}

func TestParseCLIFlags_InvalidPort(t *testing.T) {
	var stdout, stderr bytes.Buffer
	invalidPorts := []string{"-1", "0", "65536", "999999"}
	for _, port := range invalidPorts {
		_, _, err := ParseCLIFlags([]string{"--port", port}, &stdout, &stderr)
		if err == nil {
			t.Errorf("expected error for invalid port %s, got nil", port)
		}
	}
}

func TestParseCLIFlags_InvalidTimeout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, _, err := ParseCLIFlags([]string{"--timeout", "0s"}, &stdout, &stderr)
	if err == nil {
		t.Error("expected error for zero timeout, got nil")
	}
	_, _, err = ParseCLIFlags([]string{"--timeout", "-1s"}, &stdout, &stderr)
	if err == nil {
		t.Error("expected error for negative timeout, got nil")
	}
}

func TestParseCLIFlags_HelpAndVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, isHelp, err := ParseCLIFlags([]string{"--help"}, &stdout, &stderr)
	if err != nil || !isHelp {
		t.Fatalf("expected isHelp=true, got err=%v, isHelp=%v", err, isHelp)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Errorf("expected usage in stdout, got: %s", stdout.String())
	}

	stdout.Reset()
	_, isVer, err := ParseCLIFlags([]string{"--version"}, &stdout, &stderr)
	if err != nil || !isVer {
		t.Fatalf("expected isVer=true, got err=%v, isVer=%v", err, isVer)
	}
	if !strings.Contains(stdout.String(), Version) {
		t.Errorf("expected version %s in stdout, got: %s", Version, stdout.String())
	}
}

// -----------------------------------------------------------------------------
// 2. Tokenize & Parser Tests
// -----------------------------------------------------------------------------

func TestTokenize_Basics(t *testing.T) {
	tokens, err := Tokenize("PUT key value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	if string(tokens[0]) != "PUT" || string(tokens[1]) != "key" || string(tokens[2]) != "value" {
		t.Errorf("unexpected tokens: %v", tokens)
	}
}

func TestTokenize_SingleQuotes(t *testing.T) {
	tokens, err := Tokenize("PUT user:1001 '{\"name\": \"Alice\", \"role\": \"admin\"}'")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	expectedVal := `{"name": "Alice", "role": "admin"}`
	if string(tokens[2]) != expectedVal {
		t.Errorf("expected value %q, got %q", expectedVal, string(tokens[2]))
	}

	// Empty single quotes
	tokens, err = Tokenize("PUT empty ''")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	if len(tokens[2]) != 0 {
		t.Errorf("expected empty token for '', got %q", string(tokens[2]))
	}

	// Unclosed single quote
	_, err = Tokenize("PUT key 'unclosed")
	if err == nil {
		t.Error("expected error for unclosed single quote, got nil")
	}
}

func TestTokenize_DoubleQuotesAndEscapes(t *testing.T) {
	tokens, err := Tokenize(`PUT bin "hello\nworld\t\"quoted\"\\back"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	expected := "hello\nworld\t\"quoted\"\\back"
	if string(tokens[2]) != expected {
		t.Errorf("expected %q, got %q", expected, string(tokens[2]))
	}

	// Hex escapes \x00, \xff
	tokens, err = Tokenize(`PUT hex "\x00\x01\xff"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	expectedHex := []byte{0x00, 0x01, 0xff}
	if !bytes.Equal(tokens[2], expectedHex) {
		t.Errorf("expected hex bytes %v, got %v", expectedHex, tokens[2])
	}

	// Empty double quotes
	tokens, err = Tokenize(`PUT empty ""`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	if len(tokens[2]) != 0 {
		t.Errorf("expected empty token for \"\", got %q", string(tokens[2]))
	}

	// Invalid hex escapes
	_, err = Tokenize(`PUT key "\xG1"`)
	if err == nil {
		t.Error("expected error for invalid hex escape, got nil")
	}
	_, err = Tokenize(`PUT key "\x0"`)
	if err == nil {
		t.Error("expected error for truncated hex escape, got nil")
	}

	// Unclosed double quote
	_, err = Tokenize(`PUT key "unclosed`)
	if err == nil {
		t.Error("expected error for unclosed double quote, got nil")
	}
}

func TestParseCommand_Validation(t *testing.T) {
	// Empty line
	cmd, err := ParseCommand("   ")
	if err != nil || cmd != nil {
		t.Fatalf("expected nil cmd and nil err for blank line, got cmd=%v, err=%v", cmd, err)
	}

	// PUT validation
	_, err = ParseCommand("PUT")
	if err == nil || !strings.Contains(err.Error(), "requires key and value") {
		t.Errorf("expected missing args error for PUT, got: %v", err)
	}
	_, err = ParseCommand("PUT key")
	if err == nil || !strings.Contains(err.Error(), "requires key and value") {
		t.Errorf("expected missing value error for PUT, got: %v", err)
	}
	_, err = ParseCommand("PUT key val extra")
	if err == nil || !strings.Contains(err.Error(), "takes exactly 2 arguments") {
		t.Errorf("expected extra args error for PUT, got: %v", err)
	}
	_, err = ParseCommand(`PUT "" val`)
	if err == nil || !strings.Contains(err.Error(), "key cannot be empty") {
		t.Errorf("expected empty key error for PUT, got: %v", err)
	}

	// Case insensitivity
	cmd, err = ParseCommand("put mykey myval")
	if err != nil || cmd.Kind != CmdPut || string(cmd.Key) != "mykey" || string(cmd.Value) != "myval" {
		t.Errorf("expected valid lowercase put command, got cmd=%v, err=%v", cmd, err)
	}

	// Empty value in PUT is valid
	cmd, err = ParseCommand(`PUT mykey ""`)
	if err != nil || cmd.Kind != CmdPut || len(cmd.Value) != 0 {
		t.Errorf("expected valid PUT with empty value, got cmd=%v, err=%v", cmd, err)
	}

	// GET validation
	_, err = ParseCommand("GET")
	if err == nil || !strings.Contains(err.Error(), "requires a key") {
		t.Errorf("expected missing key error for GET, got: %v", err)
	}
	_, err = ParseCommand("GET k extra")
	if err == nil || !strings.Contains(err.Error(), "takes exactly 1 argument") {
		t.Errorf("expected extra args error for GET, got: %v", err)
	}
	_, err = ParseCommand(`GET ""`)
	if err == nil || !strings.Contains(err.Error(), "key cannot be empty") {
		t.Errorf("expected empty key error for GET, got: %v", err)
	}
	cmd, err = ParseCommand("get k")
	if err != nil || cmd.Kind != CmdGet || string(cmd.Key) != "k" {
		t.Errorf("expected valid GET, got cmd=%v, err=%v", cmd, err)
	}

	// DELETE validation
	_, err = ParseCommand("DELETE")
	if err == nil || !strings.Contains(err.Error(), "requires a key") {
		t.Errorf("expected missing key error for DELETE, got: %v", err)
	}
	_, err = ParseCommand("DELETE k extra")
	if err == nil || !strings.Contains(err.Error(), "takes exactly 1 argument") {
		t.Errorf("expected extra args error for DELETE, got: %v", err)
	}
	cmd, err = ParseCommand("delete k")
	if err != nil || cmd.Kind != CmdDelete || string(cmd.Key) != "k" {
		t.Errorf("expected valid DELETE, got cmd=%v, err=%v", cmd, err)
	}

	// EXISTS validation
	_, err = ParseCommand("EXISTS")
	if err == nil || !strings.Contains(err.Error(), "requires a key") {
		t.Errorf("expected missing key error for EXISTS, got: %v", err)
	}
	_, err = ParseCommand("EXISTS k extra")
	if err == nil || !strings.Contains(err.Error(), "takes exactly 1 argument") {
		t.Errorf("expected extra args error for EXISTS, got: %v", err)
	}
	cmd, err = ParseCommand("exists k")
	if err != nil || cmd.Kind != CmdExists || string(cmd.Key) != "k" {
		t.Errorf("expected valid EXISTS, got cmd=%v, err=%v", cmd, err)
	}

	// BATCH validation
	_, err = ParseCommand("BATCH")
	if err == nil || !strings.Contains(err.Error(), "requires at least one operation") {
		t.Errorf("expected error for empty BATCH, got: %v", err)
	}
	_, err = ParseCommand("BATCH PUT")
	if err == nil || !strings.Contains(err.Error(), "requires key and value") {
		t.Errorf("expected error for incomplete BATCH PUT, got: %v", err)
	}
	_, err = ParseCommand("BATCH PUT k")
	if err == nil || !strings.Contains(err.Error(), "requires key and value") {
		t.Errorf("expected error for incomplete BATCH PUT, got: %v", err)
	}
	_, err = ParseCommand("BATCH DELETE")
	if err == nil || !strings.Contains(err.Error(), "requires key") {
		t.Errorf("expected error for incomplete BATCH DELETE, got: %v", err)
	}
	_, err = ParseCommand("BATCH FOOBAR k")
	if err == nil || !strings.Contains(err.Error(), "unexpected operation") {
		t.Errorf("expected error for unknown op in BATCH, got: %v", err)
	}
	cmd, err = ParseCommand("batch put k1 v1 delete k2 put k3 v3")
	if err != nil || cmd.Kind != CmdBatch || len(cmd.Batch) != 3 {
		t.Fatalf("expected valid BATCH with 3 ops, got cmd=%v, err=%v", cmd, err)
	}
	if cmd.Batch[0].Type != transport.BatchOpPut || string(cmd.Batch[0].Key) != "k1" || string(cmd.Batch[0].Value) != "v1" {
		t.Errorf("unexpected batch op 0: %v", cmd.Batch[0])
	}
	if cmd.Batch[1].Type != transport.BatchOpDelete || string(cmd.Batch[1].Key) != "k2" {
		t.Errorf("unexpected batch op 1: %v", cmd.Batch[1])
	}
	if cmd.Batch[2].Type != transport.BatchOpPut || string(cmd.Batch[2].Key) != "k3" || string(cmd.Batch[2].Value) != "v3" {
		t.Errorf("unexpected batch op 2: %v", cmd.Batch[2])
	}

	// STATS validation
	_, err = ParseCommand("STATS extra")
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("expected error for STATS with args, got: %v", err)
	}
	cmd, err = ParseCommand("stats")
	if err != nil || cmd.Kind != CmdStats {
		t.Errorf("expected valid STATS, got cmd=%v, err=%v", cmd, err)
	}

	// HELP validation
	cmd, err = ParseCommand("help")
	if err != nil || cmd.Kind != CmdHelp {
		t.Errorf("expected valid HELP, got cmd=%v, err=%v", cmd, err)
	}
	cmd, err = ParseCommand("help put")
	if err != nil || cmd.Kind != CmdHelp || len(cmd.Args) != 1 || cmd.Args[0] != "put" {
		t.Errorf("expected valid HELP put, got cmd=%v, err=%v", cmd, err)
	}

	// EXIT and QUIT
	cmd, err = ParseCommand("exit")
	if err != nil || cmd.Kind != CmdExit {
		t.Errorf("expected valid EXIT, got cmd=%v, err=%v", cmd, err)
	}
	cmd, err = ParseCommand("quit")
	if err != nil || cmd.Kind != CmdQuit {
		t.Errorf("expected valid QUIT, got cmd=%v, err=%v", cmd, err)
	}
	_, err = ParseCommand("exit extra")
	if err == nil || !strings.Contains(err.Error(), "takes no arguments") {
		t.Errorf("expected error for exit with args, got: %v", err)
	}

	// Unknown command
	_, err = ParseCommand("UNKNOWN_CMD")
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("expected unknown command error, got: %v", err)
	}
}

func TestParseCommand_MaxLineBounds(t *testing.T) {
	hugeLine := "PUT key " + strings.Repeat("A", MaxInputLineSize+10)
	_, err := ParseCommand(hugeLine)
	if err == nil {
		t.Fatal("expected error for input exceeding MaxInputLineSize, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 3. Value Formatter Tests
// -----------------------------------------------------------------------------

func TestFormatValue(t *testing.T) {
	// Empty slice returns ""
	if got := FormatValue([]byte{}); got != `""` {
		t.Errorf("expected \"\" for empty slice, got %q", got)
	}

	// Plain printable string
	if got := FormatValue([]byte("hello world")); got != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", got)
	}

	// String with newlines and tabs
	if got := FormatValue([]byte("hello\nworld\t1")); got != "hello\nworld\t1" {
		t.Errorf("expected %q, got %q", "hello\nworld\t1", got)
	}

	// String with CRLF newlines and tabs
	if got := FormatValue([]byte("hello\r\nworld\t1")); got != "hello\r\nworld\t1" {
		t.Errorf("expected %q, got %q", "hello\r\nworld\t1", got)
	}

	// Bare carriage return without newline is escaped to prevent terminal line overwriting
	if got := FormatValue([]byte("hello\rworld")); got != `"hello\rworld"` {
		t.Errorf("expected escaped bare CR %q, got %q", `"hello\rworld"`, got)
	}

	// Binary data with non-printable bytes
	bin := []byte{0x00, 0x01, 'A', 0xff}
	got := FormatValue(bin)
	expectedFormatted := `"\x00\x01A\xff"`
	if got != expectedFormatted {
		t.Errorf("expected %q for binary data, got %q", expectedFormatted, got)
	}
}

// -----------------------------------------------------------------------------
// 4. Mock Client Command Execution Tests
// -----------------------------------------------------------------------------

func TestExecuteCommand_MockCases(t *testing.T) {
	style := NewStyle(false)

	// 1. PUT Success
	client := &mockClient{
		executeFn: func(req *transport.Request) (*transport.Response, error) {
			if req.OpCode != transport.OpPut {
				t.Fatalf("expected OpPut, got %s", req.OpCode)
			}
			return &transport.Response{
				OpCode: transport.OpPut,
				Status: transport.StatusOk,
				SeqID:  req.SeqID,
			}, nil
		},
	}
	var out bytes.Buffer
	cmd, _ := ParseCommand("PUT k v")
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out.String()) != "OK" {
		t.Errorf("expected OK, got %q", out.String())
	}

	// 2. GET Success (Printable)
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode: transport.OpGet,
			Status: transport.StatusOk,
			SeqID:  req.SeqID,
			Value:  []byte("alice"),
		}, nil
	}
	cmd, _ = ParseCommand("GET k")
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out.String()) != "alice" {
		t.Errorf("expected alice, got %q", out.String())
	}

	// 3. GET KeyNotFound
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode: transport.OpGet,
			Status: transport.StatusKeyNotFound,
			SeqID:  req.SeqID,
		}, nil
	}
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out.String()) != "NOT FOUND" {
		t.Errorf("expected NOT FOUND, got %q", out.String())
	}

	// 4. GET Empty Value Distinct from Not Found
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode: transport.OpGet,
			Status: transport.StatusOk,
			SeqID:  req.SeqID,
			Value:  []byte{},
		}, nil
	}
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out.String()) != `""` {
		t.Errorf("expected \"\" for empty value, got %q", out.String())
	}

	// 5. DELETE Success
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode: transport.OpDelete,
			Status: transport.StatusOk,
			SeqID:  req.SeqID,
		}, nil
	}
	cmd, _ = ParseCommand("DELETE k")
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out.String()) != "OK" {
		t.Errorf("expected OK, got %q", out.String())
	}

	// 6. EXISTS Server Unsupported Response
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode:  transport.OpExists,
			Status:  transport.StatusInvalidRequest,
			SeqID:   req.SeqID,
			Message: "unsupported operation: EXISTS is not implemented by storage engine",
		}, nil
	}
	cmd, _ = ParseCommand("EXISTS k")
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedExistsErr := "(error) unsupported operation: EXISTS is not implemented by storage engine"
	if strings.TrimSpace(out.String()) != expectedExistsErr {
		t.Errorf("expected %q, got %q", expectedExistsErr, out.String())
	}

	// 7. STATS Server Unsupported Response
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return &transport.Response{
			OpCode:  transport.OpStats,
			Status:  transport.StatusInvalidRequest,
			SeqID:   req.SeqID,
			Message: "unsupported operation: STATS is not implemented by storage engine",
		}, nil
	}
	cmd, _ = ParseCommand("STATS")
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expectedStatsErr := "(error) unsupported operation: STATS is not implemented by storage engine"
	if strings.TrimSpace(out.String()) != expectedStatsErr {
		t.Errorf("expected %q, got %q", expectedStatsErr, out.String())
	}

	// 8. BATCH Success
	out.Reset()
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		if req.OpCode != transport.OpBatch {
			t.Fatalf("expected OpBatch, got %s", req.OpCode)
		}
		if len(req.Batch) != 2 {
			t.Fatalf("expected 2 batch ops, got %d", len(req.Batch))
		}
		return &transport.Response{
			OpCode: transport.OpBatch,
			Status: transport.StatusOk,
			SeqID:  req.SeqID,
		}, nil
	}
	cmd, _ = ParseCommand("BATCH PUT k1 v1 DELETE k2")
	if err := executeCommand(client, cmd, &out, style); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(out.String()) != "OK" {
		t.Errorf("expected OK, got %q", out.String())
	}

	// 9. Network Error during Execution
	client.executeFn = func(req *transport.Request) (*transport.Response, error) {
		return nil, errors.New("connection reset by peer")
	}
	cmd, _ = ParseCommand("GET k")
	if err := executeCommand(client, cmd, &out, style); err == nil {
		t.Error("expected network error, got nil")
	}
}

// -----------------------------------------------------------------------------
// 5. REPL Loop Tests
// -----------------------------------------------------------------------------

func TestRunREPL_ScriptedSession(t *testing.T) {
	storage := make(map[string][]byte)

	client := &mockClient{
		executeFn: func(req *transport.Request) (*transport.Response, error) {
			switch req.OpCode {
			case transport.OpPut:
				storage[string(req.Key)] = req.Value
				return &transport.Response{
					OpCode: transport.OpPut,
					Status: transport.StatusOk,
					SeqID:  req.SeqID,
				}, nil
			case transport.OpGet:
				val, found := storage[string(req.Key)]
				if !found {
					return &transport.Response{
						OpCode: transport.OpGet,
						Status: transport.StatusKeyNotFound,
						SeqID:  req.SeqID,
					}, nil
				}
				return &transport.Response{
					OpCode: transport.OpGet,
					Status: transport.StatusOk,
					SeqID:  req.SeqID,
					Value:  val,
				}, nil
			case transport.OpDelete:
				delete(storage, string(req.Key))
				return &transport.Response{
					OpCode: transport.OpDelete,
					Status: transport.StatusOk,
					SeqID:  req.SeqID,
				}, nil
			default:
				return nil, fmt.Errorf("unexpected opcode %s", req.OpCode)
			}
		},
	}

	input := strings.Join([]string{
		"PUT user:1 Alice",
		"GET user:1",
		"PUT user:1 Bob",
		"GET user:1",
		"DELETE user:1",
		"GET user:1",
		"EXIT",
	}, "\n") + "\n"

	var out, errOut bytes.Buffer
	style := NewStyle(false)

	exitCode := runREPL(context.Background(), client, strings.NewReader(input), &out, &errOut, false, style)
	if exitCode != ExitSuccess {
		t.Fatalf("expected ExitSuccess, got %d, stderr: %s", exitCode, errOut.String())
	}

	expectedLines := []string{
		"OK",
		"Alice",
		"OK",
		"Bob",
		"OK",
		"NOT FOUND",
	}

	outLines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(outLines) != len(expectedLines) {
		t.Fatalf("expected %d output lines, got %d: %v", len(expectedLines), len(outLines), outLines)
	}

	for i, expected := range expectedLines {
		if outLines[i] != expected {
			t.Errorf("line %d: expected %q, got %q", i, expected, outLines[i])
		}
	}
}

func TestRunREPL_EOF(t *testing.T) {
	client := &mockClient{}
	var out, errOut bytes.Buffer
	style := NewStyle(false)

	input := "PUT a b\n"
	exitCode := runREPL(context.Background(), client, strings.NewReader(input), &out, &errOut, false, style)
	if exitCode != ExitSuccess {
		t.Fatalf("expected ExitSuccess on EOF, got %d", exitCode)
	}
	if strings.TrimSpace(out.String()) != "OK" {
		t.Errorf("expected OK in output, got %q", out.String())
	}
}

func TestRunREPL_HelpAndSyntaxErrors(t *testing.T) {
	client := &mockClient{}
	var out, errOut bytes.Buffer
	style := NewStyle(false)

	input := strings.Join([]string{
		"HELP",
		"INVALID_COMMAND_NAME",
		"PUT", // missing args
		"QUIT",
	}, "\n") + "\n"

	exitCode := runREPL(context.Background(), client, strings.NewReader(input), &out, &errOut, false, style)
	if exitCode != ExitSuccess {
		t.Fatalf("expected ExitSuccess, got %d", exitCode)
	}

	if !strings.Contains(out.String(), "Lattice Interactive REPL Commands:") {
		t.Errorf("expected help screen in out, got: %s", out.String())
	}

	if !strings.Contains(errOut.String(), "unknown command") {
		t.Errorf("expected unknown command in errOut, got: %s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "requires key and value") {
		t.Errorf("expected syntax error in errOut, got: %s", errOut.String())
	}
}

func TestRunREPL_NetworkFailure(t *testing.T) {
	client := &mockClient{
		executeFn: func(req *transport.Request) (*transport.Response, error) {
			return nil, errors.New("broken pipe")
		},
	}
	var out, errOut bytes.Buffer
	style := NewStyle(false)

	input := "PUT a b\n"
	exitCode := runREPL(context.Background(), client, strings.NewReader(input), &out, &errOut, false, style)
	if exitCode != ExitNetworkError {
		t.Fatalf("expected ExitNetworkError on network failure, got %d", exitCode)
	}
	if !strings.Contains(errOut.String(), "broken pipe") {
		t.Errorf("expected broken pipe in errOut, got: %s", errOut.String())
	}
}

// -----------------------------------------------------------------------------
// 6. TCP Client Correlation Tests
// -----------------------------------------------------------------------------

func TestTCPClient_SeqIDMismatch(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	client := &TCPClient{
		conn:    clientConn,
		timeout: 1 * time.Second,
	}

	// Server goroutine sends mismatched SeqID
	go func() {
		req, err := transport.ReadRequest(serverConn)
		if err != nil {
			return
		}
		// Send response with intentionally wrong SeqID
		resp := &transport.Response{
			OpCode: req.OpCode,
			Status: transport.StatusOk,
			SeqID:  req.SeqID + 999, // Mismatch!
		}
		_ = transport.WriteResponse(serverConn, resp)
	}()

	req := &transport.Request{
		OpCode: transport.OpGet,
		Key:    []byte("foo"),
	}

	_, err := client.Execute(req)
	if err == nil {
		t.Fatal("expected error on sequence ID mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "sequence ID mismatch") {
		t.Errorf("expected sequence ID mismatch error, got: %v", err)
	}
}

func TestTCPClient_OpCodeMismatch(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	client := &TCPClient{
		conn:    clientConn,
		timeout: 1 * time.Second,
	}

	// Server goroutine sends wrong OpCode
	go func() {
		req, err := transport.ReadRequest(serverConn)
		if err != nil {
			return
		}
		resp := &transport.Response{
			OpCode: transport.OpPut, // Wrong OpCode for OpGet request!
			Status: transport.StatusOk,
			SeqID:  req.SeqID,
		}
		_ = transport.WriteResponse(serverConn, resp)
	}()

	req := &transport.Request{
		OpCode: transport.OpGet,
		Key:    []byte("foo"),
	}

	_, err := client.Execute(req)
	if err == nil {
		t.Fatal("expected error on opcode mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "opcode mismatch") {
		t.Errorf("expected opcode mismatch error, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 7. Real TCP Integration Test with Live Storage Engine
// -----------------------------------------------------------------------------

func TestRealTCPIntegration(t *testing.T) {
	dir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}
	defer eng.Close()

	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0" // Random available port
	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		t.Fatalf("failed to create transport server: %v", err)
	}

	ln, err := net.Listen("tcp", srvCfg.Address)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", srvCfg.Address, err)
	}
	serverAddr := ln.Addr().String()

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Serve(ln)
	}()
	defer func() {
		_ = srv.Shutdown(context.Background())
	}()

	// Connect TCPClient to live server
	client, err := Dial(serverAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to live server at %s: %v", serverAddr, err)
	}
	defer client.Close()

	commands := []string{
		// 1. Basic PUT / GET
		"PUT user:1001 '{\"name\": \"Alice\", \"role\": \"admin\"}'",
		"GET user:1001",

		// 2. Empty value PUT / GET
		"PUT empty:key \"\"",
		"GET empty:key",

		// 3. Binary value with escapes
		"PUT bin:key \"\\x00\\x01\\x02\\xff\"",
		"GET bin:key",

		// 4. DELETE / GET (NOT FOUND)
		"DELETE user:1001",
		"GET user:1001",

		// 5. BATCH atomic mutations
		"BATCH PUT b1 v1 PUT b2 v2 DELETE empty:key",
		"GET b1",
		"GET b2",
		"GET empty:key",

		// 6. EXISTS (Unsupported operation from server)
		"EXISTS anykey",

		// 7. STATS (Unsupported operation from server)
		"STATS",

		// 8. Clean Exit
		"EXIT",
	}

	input := strings.Join(commands, "\n") + "\n"
	var out, errOut bytes.Buffer
	style := NewStyle(false)

	exitCode := runREPL(context.Background(), client, strings.NewReader(input), &out, &errOut, false, style)
	if exitCode != ExitSuccess {
		t.Fatalf("expected ExitSuccess, got %d, stderr: %s", exitCode, errOut.String())
	}

	outLines := strings.Split(strings.TrimSpace(out.String()), "\n")
	expectedOutputs := []string{
		"OK",
		`{"name": "Alice", "role": "admin"}`,
		"OK",
		`""`,
		"OK",
		`"\x00\x01\x02\xff"`,
		"OK",
		"NOT FOUND",
		"OK",
		"v1",
		"v2",
		"NOT FOUND",
		"(error) unsupported operation: EXISTS is not implemented by storage engine",
		"(error) unsupported operation: STATS is not implemented by storage engine",
	}

	if len(outLines) != len(expectedOutputs) {
		t.Fatalf("expected %d outputs, got %d:\n%v", len(expectedOutputs), len(outLines), outLines)
	}

	for i, expected := range expectedOutputs {
		if outLines[i] != expected {
			t.Errorf("output %d: expected %q, got %q", i, expected, outLines[i])
		}
	}
}

// -----------------------------------------------------------------------------
// 8. Real Binary Subprocess Tests
// -----------------------------------------------------------------------------

func TestBinarySubprocess_ServerUnavailable(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "lattice-cli")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build lattice-cli binary: %v\nOutput: %s", err, string(out))
	}

	// Pick an address that is definitely not listening
	cmd := exec.Command(binPath, "--address", "127.0.0.1:1", "--timeout", "200ms")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit code when server is unavailable, got nil")
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() != ExitNetworkError {
			t.Errorf("expected exit code %d (ExitNetworkError), got %d", ExitNetworkError, exitErr.ExitCode())
		}
	} else {
		t.Fatalf("unexpected error type: %v", err)
	}

	if !strings.Contains(stderr.String(), "connection to 127.0.0.1:1 failed") {
		t.Errorf("expected connection failure message in stderr, got: %s", stderr.String())
	}
}

func TestBinarySubprocess_HelpAndVersion(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "lattice-cli")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build lattice-cli binary: %v\nOutput: %s", err, string(out))
	}

	// Test --help
	cmd := exec.Command(binPath, "--help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unexpected error running --help: %v", err)
	}
	if !strings.Contains(string(out), "Lattice Interactive REPL Client") {
		t.Errorf("expected usage output, got: %s", string(out))
	}

	// Test --version
	cmd = exec.Command(binPath, "--version")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unexpected error running --version: %v", err)
	}
	if !strings.Contains(string(out), Version) {
		t.Errorf("expected version output %s, got: %s", Version, string(out))
	}
}

func TestBinarySubprocess_PipedScriptExecution(t *testing.T) {
	// Start live server
	dbDir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: dbDir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}
	defer eng.Close()

	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0"
	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ln, err := net.Listen("tcp", srvCfg.Address)
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer ln.Close()
	serverAddr := ln.Addr().String()

	go func() {
		_ = srv.Serve(ln)
	}()
	defer func() {
		_ = srv.Shutdown(context.Background())
	}()

	// Ensure server is accepting connections before launching subprocess
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", serverAddr, 50*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server failed to start accepting connections: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Build binary
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "lattice-cli")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build binary: %v\nOutput: %s", err, string(out))
	}

	// Run piped commands through actual binary
	cliCmd := exec.Command(binPath, "--address", serverAddr)
	input := "PUT greeting 'hello world'\nGET greeting\nDELETE greeting\nGET greeting\nQUIT\n"
	cliCmd.Stdin = strings.NewReader(input)

	var stdout, stderr bytes.Buffer
	cliCmd.Stdout = &stdout
	cliCmd.Stderr = &stderr

	if err := cliCmd.Run(); err != nil {
		t.Fatalf("failed running binary: %v, stderr: %s", err, stderr.String())
	}

	expected := "OK\nhello world\nOK\nNOT FOUND"
	if strings.TrimSpace(stdout.String()) != expected {
		t.Errorf("expected:\n%s\ngot:\n%s", expected, stdout.String())
	}
}

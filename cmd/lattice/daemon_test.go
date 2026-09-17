package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	stdErrors "errors"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// TestFlagParsing_Defaults verifies that running with no flags yields production defaults.
func TestFlagParsing_Defaults(t *testing.T) {
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	cfg, isHelp, err := ParseFlags([]string{}, stdout, stderr)
	if err != nil {
		t.Fatalf("unexpected error parsing defaults: %v", err)
	}
	if isHelp {
		t.Fatalf("expected isHelp=false for defaults")
	}
	if cfg.DataDir != filepath.Clean(DefaultDataDir) {
		t.Errorf("expected DataDir=%s, got %s", filepath.Clean(DefaultDataDir), cfg.DataDir)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("expected Port=%d, got %d", DefaultPort, cfg.Port)
	}
	expectedAddr := net.JoinHostPort(DefaultHost, strconv.Itoa(DefaultPort))
	if cfg.Address != expectedAddr {
		t.Errorf("expected Address=%s, got %s", expectedAddr, cfg.Address)
	}
	if cfg.InsecureTransport {
		t.Errorf("expected InsecureTransport=false by default")
	}
}

// TestFlagParsing_ExplicitFlags verifies that explicit flags override defaults.
func TestFlagParsing_ExplicitFlags(t *testing.T) {
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{
		"--data-dir", "/tmp/custom-data",
		"--port", "9876",
		"--insecure-transport",
	}
	cfg, isHelp, err := ParseFlags(args, stdout, stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isHelp {
		t.Fatalf("unexpected isHelp=true")
	}
	if cfg.DataDir != "/tmp/custom-data" {
		t.Errorf("expected DataDir=/tmp/custom-data, got %s", cfg.DataDir)
	}
	if cfg.Port != 9876 {
		t.Errorf("expected Port=9876, got %d", cfg.Port)
	}
	if cfg.Address != "127.0.0.1:9876" {
		t.Errorf("expected Address=127.0.0.1:9876, got %s", cfg.Address)
	}
	if !cfg.InsecureTransport {
		t.Errorf("expected InsecureTransport=true")
	}
}

// TestFlagParsing_AddressFlag verifies address parsing and port derivation.
func TestFlagParsing_AddressFlag(t *testing.T) {
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{"--address", "127.0.0.1:7788"}
	cfg, isHelp, err := ParseFlags(args, stdout, stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isHelp {
		t.Fatalf("unexpected isHelp=true")
	}
	if cfg.Port != 7788 {
		t.Errorf("expected Port=7788 derived from address, got %d", cfg.Port)
	}
	if cfg.Address != "127.0.0.1:7788" {
		t.Errorf("expected Address=127.0.0.1:7788, got %s", cfg.Address)
	}
}

// TestFlagParsing_PortBoundaries tests validation of boundary and invalid port values.
func TestFlagParsing_PortBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		port    string
		wantErr bool
	}{
		{"Negative port", "-1", true},
		{"Port zero (ephemeral)", "0", false},
		{"Port 1", "1", false},
		{"Port 65535", "65535", false},
		{"Port 65536 (too high)", "65536", true},
		{"Non-numeric port", "abc", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
			args := []string{"--port", tc.port}
			_, _, err := ParseFlags(args, stdout, stderr)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseFlags() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestFlagParsing_HelpAndVersion verifies help and version flags do not trigger errors.
func TestFlagParsing_HelpAndVersion(t *testing.T) {
	for _, flagName := range []string{"--help", "-h", "--version", "-v"} {
		stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
		cfg, isHelp, err := ParseFlags([]string{flagName}, stdout, stderr)
		if err != nil {
			t.Fatalf("flag %s returned error: %v", flagName, err)
		}
		if !isHelp {
			t.Fatalf("flag %s expected isHelp=true", flagName)
		}
		if cfg != nil {
			t.Fatalf("flag %s expected nil cfg", flagName)
		}
		if stdout.Len() == 0 {
			t.Fatalf("flag %s expected non-empty stdout output", flagName)
		}
	}
}

// TestFlagParsing_LoopbackSecurityEnforcement verifies SEC-P11-001 loopback constraint.
func TestFlagParsing_LoopbackSecurityEnforcement(t *testing.T) {
	// Attempt non-loopback binding without --insecure-transport -> must fail with ErrInsecureTransport
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{"--address", "0.0.0.0:9099"}
	_, _, err := ParseFlags(args, stdout, stderr)
	if err == nil {
		t.Fatalf("expected error when binding 0.0.0.0 without --insecure-transport")
	}
	if !stdErrors.Is(err, errors.ErrInsecureTransport) {
		t.Fatalf("expected ErrInsecureTransport, got: %v", err)
	}

	// Attempt non-loopback binding WITH --insecure-transport -> must succeed
	stdout.Reset()
	stderr.Reset()
	args = []string{"--address", "0.0.0.0:9099", "--insecure-transport"}
	cfg, _, err := ParseFlags(args, stdout, stderr)
	if err != nil {
		t.Fatalf("unexpected error with --insecure-transport: %v", err)
	}
	if cfg.Address != "0.0.0.0:9099" || !cfg.InsecureTransport {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

// TestFlagParsing_UnexpectedPositionalArguments verifies SEC-P12-002:
// Unrecognized subcommands and unexpected positional arguments are rejected with an error.
func TestFlagParsing_UnexpectedPositionalArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unknown_subcommand", []string{"foobar"}},
		{"misspelled_subcommand", []string{"dump_wal"}},
		{"extra_trailing_arg", []string{"--port", "9099", "trailing_value"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
			_, _, err := ParseFlags(tc.args, stdout, stderr)
			if err == nil {
				t.Fatalf("expected error for args %v, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), "unexpected argument") {
				t.Errorf("expected 'unexpected argument' error, got: %v", err)
			}
		})
	}
}

// TestConfigFile_Precedence verifies Defaults -> Config File -> CLI Flags.
func TestConfigFile_Precedence(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "lattice.json")
	configJSON := `{
		"data_dir": "/from/config/json",
		"port": 9111,
		"insecure_transport": false
	}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	// Case 1: Config file only (overrides defaults)
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	cfg, _, err := ParseFlags([]string{"--config", configPath}, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to parse with config file: %v", err)
	}
	if cfg.DataDir != "/from/config/json" {
		t.Errorf("expected DataDir from config file, got %s", cfg.DataDir)
	}
	if cfg.Port != 9111 {
		t.Errorf("expected Port from config file, got %d", cfg.Port)
	}

	// Case 2: CLI flag overrides config file value
	stdout.Reset()
	stderr.Reset()
	cfg, _, err = ParseFlags([]string{"--config", configPath, "--port", "9222"}, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to parse with override: %v", err)
	}
	if cfg.Port != 9222 {
		t.Errorf("expected CLI flag override Port=9222, got %d", cfg.Port)
	}
	if cfg.DataDir != "/from/config/json" {
		t.Errorf("expected DataDir retained from config file, got %s", cfg.DataDir)
	}
}

// TestConfigFile_KeyValueFormat verifies loading simple key-value / YAML configuration files.
func TestConfigFile_KeyValueFormat(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "lattice.conf")
	configKV := `# Lattice configuration test
data_dir: /from/config/kv
port = 9333
insecure_transport: false
`
	if err := os.WriteFile(configPath, []byte(configKV), 0600); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	cfg, _, err := ParseFlags([]string{"--config", configPath}, stdout, stderr)
	if err != nil {
		t.Fatalf("failed to parse kv config file: %v", err)
	}
	if cfg.DataDir != "/from/config/kv" {
		t.Errorf("expected DataDir=/from/config/kv, got %s", cfg.DataDir)
	}
	if cfg.Port != 9333 {
		t.Errorf("expected Port=9333, got %d", cfg.Port)
	}
}

// TestConfigFile_Errors verifies error reporting on missing, directory, oversized, or malformed config files.
func TestConfigFile_Errors(t *testing.T) {
	tempDir := t.TempDir()

	// Nonexistent file
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	_, _, err := ParseFlags([]string{"--config", filepath.Join(tempDir, "nonexistent.json")}, stdout, stderr)
	if err == nil {
		t.Errorf("expected error on nonexistent config file")
	}

	// Directory as file
	stdout.Reset()
	stderr.Reset()
	_, _, err = ParseFlags([]string{"--config", tempDir}, stdout, stderr)
	if err == nil {
		t.Errorf("expected error when directory passed as config file")
	}

	// Oversized file (> 1 MiB)
	oversizedPath := filepath.Join(tempDir, "huge.json")
	hugeData := make([]byte, MaxConfigFileSize+10)
	if err := os.WriteFile(oversizedPath, hugeData, 0600); err != nil {
		t.Fatalf("failed to create huge file: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	_, _, err = ParseFlags([]string{"--config", oversizedPath}, stdout, stderr)
	if err == nil {
		t.Errorf("expected error on oversized config file")
	}
}

// TestDaemon_Lifecycle_RealTCPIntegration tests full boot, real TCP PUT/GET/DELETE, and clean shutdown.
func TestDaemon_Lifecycle_RealTCPIntegration(t *testing.T) {
	tempDir := t.TempDir()

	// Choose an ephemeral port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get ephemeral port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(port),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCode := runWithContext(ctx, args, stdout, stderr, readyCh)
		exitCh <- exitCode
	}()

	select {
	case <-readyCh:
		// Server started successfully
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon timed out waiting for readiness")
	}

	// Connect real TCP client and perform operations
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}
	defer conn.Close()

	// 1. PUT key1 -> val1
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("test_key"),
		Value:  []byte("test_val"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("failed to write PUT: %v", err)
	}
	resp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read PUT response: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("PUT returned status %s: %s", resp.Status, resp.Message)
	}

	// 2. GET key1 -> verify val1
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  2,
		Key:    []byte("test_key"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write GET: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read GET response: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("GET returned status %s: %s", resp.Status, resp.Message)
	}
	if string(resp.Value) != "test_val" {
		t.Fatalf("GET returned value %q, want %q", resp.Value, "test_val")
	}

	// 3. DELETE key1
	delReq := &transport.Request{
		OpCode: transport.OpDelete,
		SeqID:  3,
		Key:    []byte("test_key"),
	}
	if err := transport.WriteRequest(conn, delReq); err != nil {
		t.Fatalf("failed to write DELETE: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read DELETE response: %v", err)
	}
	if resp.Status != transport.StatusOk {
		t.Fatalf("DELETE returned status %s: %s", resp.Status, resp.Message)
	}

	// 4. GET key1 -> should return KeyNotFound
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write second GET: %v", err)
	}
	resp, err = transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read second GET response: %v", err)
	}
	if resp.Status != transport.StatusKeyNotFound {
		t.Fatalf("expected StatusKeyNotFound, got %s: %s", resp.Status, resp.Message)
	}

	// 5. Trigger graceful shutdown via context cancellation
	cancel()

	select {
	case code := <-exitCh:
		if code != ExitSuccess {
			t.Fatalf("expected ExitSuccess (0), got %d: stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon shutdown timed out")
	}
}

// TestDaemon_PersistenceAcrossRestart verifies that data written before graceful shutdown
// survives across a daemon restart.
func TestDaemon_PersistenceAcrossRestart(t *testing.T) {
	tempDir := t.TempDir()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get ephemeral port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// Session 1: Start, Write durable data, Shutdown
	{
		ctx, cancel := context.WithCancel(context.Background())
		readyCh := make(chan struct{})
		stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

		args := []string{"--data-dir", tempDir, "--port", strconv.Itoa(port)}
		exitCh := make(chan int, 1)
		go func() {
			exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
		}()

		select {
		case <-readyCh:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("session 1 timed out waiting for readiness")
		}

		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
		if err != nil {
			cancel()
			t.Fatalf("failed to connect: %v", err)
		}

		putReq := &transport.Request{
			OpCode: transport.OpPut,
			SeqID:  1,
			Key:    []byte("durable_key"),
			Value:  []byte("durable_payload_12345"),
		}
		if err := transport.WriteRequest(conn, putReq); err != nil {
			conn.Close()
			cancel()
			t.Fatalf("failed to write PUT: %v", err)
		}
		resp, err := transport.ReadResponse(conn)
		conn.Close()
		if err != nil || resp.Status != transport.StatusOk {
			cancel()
			t.Fatalf("PUT failed: resp=%+v err=%v", resp, err)
		}

		// Trigger shutdown
		cancel()
		select {
		case code := <-exitCh:
			if code != ExitSuccess {
				t.Fatalf("session 1 exit code %d, stderr=%s", code, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("session 1 shutdown timed out")
		}
	}

	// Session 2: Start new daemon on same dataDir, Read durable data
	{
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		readyCh := make(chan struct{})
		stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

		args := []string{"--data-dir", tempDir, "--port", strconv.Itoa(port)}
		exitCh := make(chan int, 1)
		go func() {
			exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
		}()

		select {
		case <-readyCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("session 2 timed out waiting for readiness")
		}

		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 2*time.Second)
		if err != nil {
			t.Fatalf("failed to connect session 2: %v", err)
		}
		defer conn.Close()

		getReq := &transport.Request{
			OpCode: transport.OpGet,
			SeqID:  2,
			Key:    []byte("durable_key"),
		}
		if err := transport.WriteRequest(conn, getReq); err != nil {
			t.Fatalf("session 2 failed to write GET: %v", err)
		}
		resp, err := transport.ReadResponse(conn)
		if err != nil || resp.Status != transport.StatusOk {
			t.Fatalf("session 2 GET failed: resp=%+v err=%v", resp, err)
		}
		if string(resp.Value) != "durable_payload_12345" {
			t.Fatalf("session 2 returned value %q, want %q", resp.Value, "durable_payload_12345")
		}

		cancel()
		select {
		case code := <-exitCh:
			if code != ExitSuccess {
				t.Fatalf("session 2 exit code %d, stderr=%s", code, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("session 2 shutdown timed out")
		}
	}
}

// TestDaemon_PortCollision verifies that if a port is already in use, the second
// daemon fails cleanly with ExitStartupError and closes its Engine without resource leaks.
func TestDaemon_PortCollision(t *testing.T) {
	tempDir1 := t.TempDir()
	tempDir2 := t.TempDir()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get ephemeral port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// Start Daemon A on port
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	readyChA := make(chan struct{})
	stdoutA, stderrA := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	argsA := []string{"--data-dir", tempDir1, "--port", strconv.Itoa(port)}
	exitChA := make(chan int, 1)
	go func() {
		exitChA <- runWithContext(ctxA, argsA, stdoutA, stderrA, readyChA)
	}()

	select {
	case <-readyChA:
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon A timed out waiting for readiness")
	}

	// Attempt to start Daemon B on the same port -> must fail with ExitStartupError
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	stdoutB, stderrB := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	argsB := []string{"--data-dir", tempDir2, "--port", strconv.Itoa(port)}
	codeB := runWithContext(ctxB, argsB, stdoutB, stderrB, nil)
	if codeB != ExitStartupError {
		t.Fatalf("daemon B expected ExitStartupError (%d), got %d (stderr: %s)", ExitStartupError, codeB, stderrB.String())
	}

	// Clean shutdown of Daemon A
	cancelA()
	select {
	case codeA := <-exitChA:
		if codeA != ExitSuccess {
			t.Fatalf("daemon A exit code %d", codeA)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("daemon A shutdown timed out")
	}
}

// TestDaemon_SubprocessBinary verifies the actual compiled binary execution,
// flag handling, and signal shutdown.
func TestDaemon_SubprocessBinary(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "lattice")

	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build lattice binary: %v, output: %s", err, string(out))
	}

	// Test 1: ./lattice --help exits with 0
	{
		cmd := exec.Command(binPath, "--help")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("--help failed: %v, output: %s", err, string(out))
		}
		if !bytes.Contains(out, []byte("Usage of lattice:")) {
			t.Fatalf("expected usage message, got: %s", string(out))
		}
	}

	// Test 2: ./lattice --version exits with 0
	{
		cmd := exec.Command(binPath, "--version")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("--version failed: %v, output: %s", err, string(out))
		}
		if !bytes.Contains(out, []byte("lattice server daemon version")) {
			t.Fatalf("expected version message, got: %s", string(out))
		}
	}

	// Test 3: ./lattice with invalid flag exits with non-zero
	{
		cmd := exec.Command(binPath, "--unknown-flag-xyz")
		err := cmd.Run()
		if err == nil {
			t.Fatalf("expected error on unknown flag, got nil")
		}
	}

	// Test 4: Real subprocess running and receiving SIGTERM
	{
		dataDir := t.TempDir()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to get port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		cmd := exec.Command(binPath, "--data-dir", dataDir, "--port", strconv.Itoa(port))
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("failed to get stdout pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("failed to start subprocess: %v", err)
		}

		// Wait for server to announce listening
		buf := make([]byte, 512)
		n, err := stdoutPipe.Read(buf)
		if err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("failed to read server startup: %v", err)
		}
		if !bytes.Contains(buf[:n], []byte("server listening on")) {
			_ = cmd.Process.Kill()
			t.Fatalf("expected startup announcement, got: %s", string(buf[:n]))
		}

		// Send SIGTERM
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("failed to send SIGTERM: %v", err)
		}

		// Wait for clean exit
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("subprocess did not exit cleanly on SIGTERM: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("subprocess timed out waiting for SIGTERM exit")
		}
	}

	// Test 5: Real subprocess running and receiving SIGINT
	{
		dataDir := t.TempDir()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to get port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		cmd := exec.Command(binPath, "--data-dir", dataDir, "--port", strconv.Itoa(port))
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("failed to get stdout pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("failed to start subprocess: %v", err)
		}

		// Wait for server to announce listening
		buf := make([]byte, 512)
		n, err := stdoutPipe.Read(buf)
		if err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("failed to read server startup: %v", err)
		}
		if !bytes.Contains(buf[:n], []byte("server listening on")) {
			_ = cmd.Process.Kill()
			t.Fatalf("expected startup announcement, got: %s", string(buf[:n]))
		}

		// Send SIGINT
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			_ = cmd.Process.Kill()
			t.Fatalf("failed to send SIGINT: %v", err)
		}

		// Wait for clean exit
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("subprocess did not exit cleanly on SIGINT: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("subprocess timed out waiting for SIGINT exit")
		}
	}

	// Test 6: Repeated signals during shutdown do not cause panic or double close
	{
		dataDir := t.TempDir()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to get port: %v", err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()

		cmd := exec.Command(binPath, "--data-dir", dataDir, "--port", strconv.Itoa(port))
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatalf("failed to get stdout pipe: %v", err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatalf("failed to start subprocess: %v", err)
		}

		buf := make([]byte, 512)
		n, err := stdoutPipe.Read(buf)
		if err != nil || !bytes.Contains(buf[:n], []byte("server listening on")) {
			_ = cmd.Process.Kill()
			t.Fatalf("failed to read server startup: %v", err)
		}

		// Send repeated signals rapidly
		_ = cmd.Process.Signal(syscall.SIGINT)
		_ = cmd.Process.Signal(syscall.SIGINT)
		_ = cmd.Process.Signal(syscall.SIGTERM)

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("subprocess failed under repeated signals: %v", err)
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Fatalf("subprocess hung on repeated signals")
		}
	}
}

// TestDaemon_StartupFailure_InvalidDataDir verifies that if the data directory
// cannot be initialized (e.g. a regular file is passed as the directory path),
// the daemon exits cleanly with ExitStartupError without panic or leak.
func TestDaemon_StartupFailure_InvalidDataDir(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "existing_file")
	if err := os.WriteFile(filePath, []byte("data"), 0600); err != nil {
		t.Fatalf("failed to create dummy file: %v", err)
	}

	// Attempt to use the file as the data directory path
	invalidDataDir := filepath.Join(filePath, "child_dir")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{"--data-dir", invalidDataDir, "--port", "0"}

	code := runWithContext(ctx, args, stdout, stderr, nil)
	if code != ExitStartupError {
		t.Fatalf("expected ExitStartupError (%d), got %d (stderr: %s)", ExitStartupError, code, stderr.String())
	}
}

// TestDaemon_EarlyContextCancel verifies that passing an already cancelled context
// aborts before opening resources and returns ExitSuccess.
func TestDaemon_EarlyContextCancel(t *testing.T) {
	tempDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel before starting

	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{"--data-dir", tempDir, "--port", "0"}

	code := runWithContext(ctx, args, stdout, stderr, nil)
	if code != ExitSuccess {
		t.Fatalf("expected ExitSuccess (%d), got %d (stderr: %s)", ExitSuccess, code, stderr.String())
	}
}

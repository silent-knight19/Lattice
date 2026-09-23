package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/errors"
	"github.com/silent-knight19/lattice/internal/transport"
)

// getFreePort helper reserves and immediately yields a free loopback TCP port.
func getFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestDaemon_MetricsServer_LifecycleAndLiveOperations verifies that a Lattice daemon
// started with --metrics-address exposes the Prometheus metrics endpoint, tracks active
// connections, updates write and read histograms upon client operations, records WAL bytes,
// and shuts down cleanly within context deadline.
func TestDaemon_MetricsServer_LifecycleAndLiveOperations(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "lattice_metrics_lifecycle_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely with code %d: stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for daemon readiness")
	}

	metricsURL := fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort)
	httpClient := &http.Client{Timeout: 3 * time.Second}
	defer httpClient.CloseIdleConnections()

	// 1. Initial scrape: endpoint returns HTTP 200 with standard Prometheus Content-Type
	resp, err := httpClient.Get(metricsURL)
	if err != nil {
		t.Fatalf("failed to scrape /metrics: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from /metrics, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	expectedCT := "text/plain; version=0.0.4; charset=utf-8"
	if ct != expectedCT {
		t.Errorf("expected Content-Type %q, got %q", expectedCT, ct)
	}
	initialBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("failed to read initial metrics body: %v", err)
	}
	initialStr := string(initialBody)

	// Invariant: no active data connections initially
	if !strings.Contains(initialStr, "lattice_connections_active 0") {
		t.Errorf("expected lattice_connections_active 0 in initial metrics:\n%s", initialStr)
	}

	// Helper to extract numeric value for a metric line prefix
	parseMetric := func(metricsText, metricPrefix string) int64 {
		for _, line := range strings.Split(metricsText, "\n") {
			if strings.HasPrefix(line, metricPrefix) {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					val, _ := strconv.ParseInt(fields[1], 10, 64)
					return val
				}
			}
		}
		return -1
	}

	beforePutCount := parseMetric(initialStr, `lattice_engine_write_latency_seconds_count{op="put"}`)
	beforeReadCount := parseMetric(initialStr, `lattice_engine_read_latency_seconds_count`)
	beforeReqPutCount := parseMetric(initialStr, `lattice_request_duration_seconds_count{op="put",status="ok"}`)
	beforeReqGetCount := parseMetric(initialStr, `lattice_request_duration_seconds_count{op="get",status="ok"}`)
	beforeWALBytes := parseMetric(initialStr, `lattice_wal_bytes_written_total`)

	// 2. Open TCP client connection and verify connection gauge increases to 1
	clientAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(srvPort))
	conn, err := net.DialTimeout("tcp", clientAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to daemon: %v", err)
	}

	var connBody []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = httpClient.Get(metricsURL)
		if err != nil {
			t.Fatalf("failed to scrape /metrics after connecting: %v", err)
		}
		connBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(connBody), "lattice_connections_active 1") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(string(connBody), "lattice_connections_active 1") {
		t.Errorf("expected lattice_connections_active 1 after client connection:\n%s", string(connBody))
	}

	// 3. Perform real PUT request
	putReq := &transport.Request{
		OpCode: transport.OpPut,
		SeqID:  1,
		Key:    []byte("metrics_key_1"),
		Value:  []byte("metrics_val_1"),
	}
	if err := transport.WriteRequest(conn, putReq); err != nil {
		t.Fatalf("failed to write PUT request: %v", err)
	}
	putResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read PUT response: %v", err)
	}
	if putResp.Status != transport.StatusOk {
		t.Fatalf("PUT request failed: status=%s message=%s", putResp.Status, putResp.Message)
	}

	// 4. Perform real GET request
	getReq := &transport.Request{
		OpCode: transport.OpGet,
		SeqID:  2,
		Key:    []byte("metrics_key_1"),
	}
	if err := transport.WriteRequest(conn, getReq); err != nil {
		t.Fatalf("failed to write GET request: %v", err)
	}
	getResp, err := transport.ReadResponse(conn)
	if err != nil {
		t.Fatalf("failed to read GET response: %v", err)
	}
	if getResp.Status != transport.StatusOk || string(getResp.Value) != "metrics_val_1" {
		t.Fatalf("GET request failed: status=%s value=%q", getResp.Status, string(getResp.Value))
	}

	// Close client connection
	conn.Close()

	// 5. Scrape metrics after operations and verify write, read, and WAL metrics changed
	resp, err = httpClient.Get(metricsURL)
	if err != nil {
		t.Fatalf("failed to scrape /metrics after operations: %v", err)
	}
	afterBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	afterStr := string(afterBody)

	afterPutCount := parseMetric(afterStr, `lattice_engine_write_latency_seconds_count{op="put"}`)
	afterReadCount := parseMetric(afterStr, `lattice_engine_read_latency_seconds_count`)
	afterReqPutCount := parseMetric(afterStr, `lattice_request_duration_seconds_count{op="put",status="ok"}`)
	afterReqGetCount := parseMetric(afterStr, `lattice_request_duration_seconds_count{op="get",status="ok"}`)
	afterWALBytes := parseMetric(afterStr, `lattice_wal_bytes_written_total`)

	// Write histogram count for "put" must increase
	if afterPutCount <= beforePutCount {
		t.Errorf("expected engine write latency count for 'put' to increase: before=%d after=%d", beforePutCount, afterPutCount)
	}
	// Read histogram count must increase
	if afterReadCount <= beforeReadCount {
		t.Errorf("expected engine read latency count to increase: before=%d after=%d", beforeReadCount, afterReadCount)
	}
	// Request duration count for wire protocol op="put",status="ok" must increase
	if afterReqPutCount <= beforeReqPutCount {
		t.Errorf("expected request duration count for put/ok to increase: before=%d after=%d", beforeReqPutCount, afterReqPutCount)
	}
	// Request duration count for wire protocol op="get",status="ok" must increase
	if afterReqGetCount <= beforeReqGetCount {
		t.Errorf("expected request duration count for get/ok to increase: before=%d after=%d", beforeReqGetCount, afterReqGetCount)
	}
	// WAL bytes written must increase
	if afterWALBytes <= beforeWALBytes {
		t.Errorf("expected WAL bytes to increase: before=%d after=%d", beforeWALBytes, afterWALBytes)
	}
	// Active connections should return to 0
	if !strings.Contains(afterStr, "lattice_connections_active 0") {
		t.Errorf("expected lattice_connections_active 0 after client disconnect:\n%s", afterStr)
	}

	// 6. Graceful shutdown
	cancel()
	select {
	case code := <-exitCh:
		if code != ExitSuccess {
			t.Errorf("expected exit code %d, got %d", ExitSuccess, code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for daemon graceful shutdown")
	}

	// Verify metrics server is no longer listening
	_, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)), 200*time.Millisecond)
	if dialErr == nil {
		t.Errorf("expected metrics listener to be closed after shutdown")
	}
}

// TestDaemon_MetricsServer_SecurityRejections verifies configuration validation
// rules for the metrics endpoint, including non-loopback binding rejection without
// --insecure-transport and port collision detection.
func TestDaemon_MetricsServer_SecurityRejections(t *testing.T) {
	tempDir := t.TempDir()

	testCases := []struct {
		name        string
		args        []string
		expectedErr string
	}{
		{
			name: "Non-loopback metrics without insecure transport rejected",
			args: []string{
				"--data-dir", tempDir,
				"--metrics-address", "0.0.0.0:9100",
			},
			expectedErr: errors.ErrInsecureTransport.Error(),
		},
		{
			name: "Public IP metrics without insecure transport rejected",
			args: []string{
				"--data-dir", tempDir,
				"--metrics-address", "192.168.1.100:9100",
			},
			expectedErr: errors.ErrInsecureTransport.Error(),
		},
		{
			name: "Metrics port conflicts with server address",
			args: []string{
				"--data-dir", tempDir,
				"--port", "9099",
				"--metrics-address", "127.0.0.1:9099",
			},
			expectedErr: "conflicts with server --address port 9099",
		},
		{
			name: "Metrics port conflicts with pprof address",
			args: []string{
				"--data-dir", tempDir,
				"--pprof-address", "127.0.0.1:6060",
				"--metrics-address", "127.0.0.1:6060",
			},
			expectedErr: "conflicts with pprof address port 6060",
		},
		{
			name: "Invalid port in metrics address",
			args: []string{
				"--data-dir", tempDir,
				"--metrics-address", "127.0.0.1:99999",
			},
			expectedErr: "invalid port in --metrics-address",
		},
		{
			name: "Invalid format in metrics address",
			args: []string{
				"--data-dir", tempDir,
				"--metrics-address", "not-a-valid-address:abc:def",
			},
			expectedErr: "invalid --metrics-address",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
			_, _, err := ParseFlags(tc.args, stdout, stderr)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.expectedErr)
			}
			if !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("expected error containing %q, got: %v", tc.expectedErr, err)
			}
		})
	}
}

// TestDaemon_MetricsServer_InsecureTransportPermitsNonLoopback verifies that explicit
// opt-in with --insecure-transport allows binding to non-loopback addresses.
func TestDaemon_MetricsServer_InsecureTransportPermitsNonLoopback(t *testing.T) {
	tempDir := t.TempDir()
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{
		"--data-dir", tempDir,
		"--insecure-transport",
		"--address", "0.0.0.0:9099",
		"--metrics-address", "0.0.0.0:9100",
	}
	cfg, _, err := ParseFlags(args, stdout, stderr)
	if err != nil {
		t.Fatalf("unexpected validation error with --insecure-transport: %v", err)
	}
	if cfg.MetricsAddress != "0.0.0.0:9100" {
		t.Errorf("expected MetricsAddress=0.0.0.0:9100, got %s", cfg.MetricsAddress)
	}
}

// TestDaemon_MetricsServer_HttpSemantics verifies standard HTTP method and path semantics
// on the metrics server: GET /metrics -> 200, POST /metrics -> 405, GET /unknown -> 404.
func TestDaemon_MetricsServer_HttpSemantics(t *testing.T) {
	tempDir := t.TempDir()
	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely: code=%d stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for daemon readiness")
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", metricsPort)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	// 1. POST /metrics -> 405 Method Not Allowed
	postResp, err := client.Post(baseURL+"/metrics", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /metrics failed: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected HTTP 405 on POST /metrics, got %d", postResp.StatusCode)
	}
	if allow := postResp.Header.Get("Allow"); allow != http.MethodGet {
		t.Errorf("expected Allow: GET header, got %q", allow)
	}

	// 2. GET /nonexistent -> 404 Not Found
	notFoundResp, err := client.Get(baseURL + "/nonexistent")
	if err != nil {
		t.Fatalf("GET /nonexistent failed: %v", err)
	}
	notFoundResp.Body.Close()
	if notFoundResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected HTTP 404 on GET /nonexistent, got %d", notFoundResp.StatusCode)
	}

	cancel()
	<-exitCh
}

// TestDaemon_MetricsServer_PortCollisionAtStartup verifies that if another process
// is already occupying the requested metrics port, the daemon exits cleanly with ExitStartupError.
func TestDaemon_MetricsServer_PortCollisionAtStartup(t *testing.T) {
	tempDir := t.TempDir()

	// Occupy a port
	occupiedListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind listener: %v", err)
	}
	defer occupiedListener.Close()
	occupiedPort := occupiedListener.Addr().(*net.TCPAddr).Port

	srvPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(occupiedPort)),
	}

	exitCode := runWithContext(ctx, args, stdout, stderr, nil)
	if exitCode != ExitStartupError {
		t.Fatalf("expected ExitStartupError (%d), got %d (stderr: %s)", ExitStartupError, exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "failed to start metrics listener") {
		t.Errorf("expected error message in stderr, got: %s", stderr.String())
	}
}

// TestDaemon_MetricsServer_ConcurrentScrapingUnderLoad stresses the metrics endpoint
// with concurrent HTTP scrapers while the database actively processes concurrent client writes and reads.
func TestDaemon_MetricsServer_ConcurrentScrapingUnderLoad(t *testing.T) {
	tempDir := t.TempDir()
	srvPort := getFreePort(t)
	metricsPort := getFreePort(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct{})
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)

	args := []string{
		"--data-dir", tempDir,
		"--port", strconv.Itoa(srvPort),
		"--metrics-address", net.JoinHostPort("127.0.0.1", strconv.Itoa(metricsPort)),
	}

	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runWithContext(ctx, args, stdout, stderr, readyCh)
	}()

	select {
	case <-readyCh:
	case code := <-exitCh:
		t.Fatalf("daemon exited prematurely: code=%d stderr=%s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for daemon readiness")
	}

	metricsURL := fmt.Sprintf("http://127.0.0.1:%d/metrics", metricsPort)
	clientAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(srvPort))

	var wg sync.WaitGroup

	// Worker 1-4: Concurrent client storage operations
	for w := 0; w < 4; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", clientAddr, 2*time.Second)
			if err != nil {
				t.Errorf("worker %d failed to connect: %v", workerID, err)
				return
			}
			defer conn.Close()

			for i := 0; i < 25; i++ {
				key := []byte(fmt.Sprintf("stress_k_%d_%d", workerID, i))
				val := []byte(fmt.Sprintf("stress_v_%d_%d", workerID, i))

				putReq := &transport.Request{
					OpCode: transport.OpPut,
					SeqID:  uint64(i*10 + workerID),
					Key:    key,
					Value:  val,
				}
				if err := transport.WriteRequest(conn, putReq); err != nil {
					return
				}
				if _, err := transport.ReadResponse(conn); err != nil {
					return
				}

				getReq := &transport.Request{
					OpCode: transport.OpGet,
					SeqID:  uint64(i*10 + workerID + 1),
					Key:    key,
				}
				if err := transport.WriteRequest(conn, getReq); err != nil {
					return
				}
				if _, err := transport.ReadResponse(conn); err != nil {
					return
				}
			}
		}()
	}

	// Worker 5-10: Concurrent HTTP scrapers
	for s := 0; s < 6; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &http.Client{Timeout: 3 * time.Second}
			defer c.CloseIdleConnections()

			for i := 0; i < 15; i++ {
				resp, err := c.Get(metricsURL)
				if err != nil {
					t.Errorf("concurrent scrape failed: %v", err)
					return
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Errorf("failed to read scrape body: %v", err)
					return
				}
				if resp.StatusCode != http.StatusOK {
					t.Errorf("expected 200 OK from scrape, got %d", resp.StatusCode)
				}
				if !strings.Contains(string(body), "lattice_engine_write_latency_seconds") {
					t.Errorf("metrics output missing expected metric header")
				}
			}
		}()
	}

	wg.Wait()

	cancel()
	<-exitCh
}

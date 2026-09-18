package main

import (
	"bytes"
	"context"
	"math/rand"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/benchmark"
	"github.com/silent-knight19/lattice/internal/engine"
	"github.com/silent-knight19/lattice/internal/transport"
)

// TestBenchmarkE2E_RealServer runs an end-to-end benchmark against a live transport.Server
// backed by a real LSM storage engine.
func TestBenchmarkE2E_RealServer(t *testing.T) {
	tempDir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: tempDir})
	if err := eng.Open(); err != nil {
		t.Fatalf("failed to open engine: %v", err)
	}
	defer func() {
		_ = eng.Close()
	}()

	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0" // Dynamic free port
	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		t.Fatalf("failed to construct transport server: %v", err)
	}

	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer func() {
		_ = srv.Close()
	}()

	boundAddr := srv.Addr().String()

	// Configure benchmark: 4 workers, 1s duration, mixed workload, 500 keys
	cfg := DefaultConfig()
	cfg.Address = boundAddr
	cfg.Concurrency = 4
	cfg.Duration = 1 * time.Second
	cfg.Keyspace = 500
	cfg.PopulateKeys = 500
	cfg.Populate = true
	cfg.ValSize = 128
	cfg.ReadRatio = 0.75
	cfg.Workload = WorkloadMixed

	var stdout, stderr bytes.Buffer
	res, err := Run(context.Background(), &cfg, &stdout, &stderr)
	if err != nil {
		t.Fatalf("benchmark run failed: %v\nstderr: %s", err, stderr.String())
	}

	// 1. Verify Operations Executed
	if res.TotalOps == 0 {
		t.Fatalf("expected non-zero operations, got 0")
	}
	if res.GetCount == 0 {
		t.Errorf("expected non-zero GET operations, got 0")
	}
	if res.PutCount == 0 {
		t.Errorf("expected non-zero PUT operations, got 0")
	}
	if res.GetErrors != 0 {
		t.Errorf("expected 0 GET errors with full pre-population, got %d", res.GetErrors)
	}

	// 2. Verify Counters Match Histograms
	if res.GetHist.Count() != res.GetCount {
		t.Errorf("GetHist.Count() = %d, want %d", res.GetHist.Count(), res.GetCount)
	}
	if res.PutHist.Count() != res.PutCount {
		t.Errorf("PutHist.Count() = %d, want %d", res.PutHist.Count(), res.PutCount)
	}
	if res.TotalOps != res.GetCount+res.PutCount {
		t.Errorf("TotalOps %d != GetCount %d + PutCount %d", res.TotalOps, res.GetCount, res.PutCount)
	}

	// 3. Verify Latency Percentile Monotonicity
	if res.GetHist.Min() > res.GetHist.P50() {
		t.Errorf("Get Min (%v) > P50 (%v)", res.GetHist.Min(), res.GetHist.P50())
	}
	if res.GetHist.P50() > res.GetHist.P90() {
		t.Errorf("Get P50 (%v) > P90 (%v)", res.GetHist.P50(), res.GetHist.P90())
	}
	if res.GetHist.P90() > res.GetHist.P99() {
		t.Errorf("Get P90 (%v) > P99 (%v)", res.GetHist.P90(), res.GetHist.P99())
	}
	if res.GetHist.P99() > res.GetHist.Max() {
		t.Errorf("Get P99 (%v) > Max (%v)", res.GetHist.P99(), res.GetHist.Max())
	}

	if res.PutHist.Min() > res.PutHist.P50() {
		t.Errorf("Put Min (%v) > P50 (%v)", res.PutHist.Min(), res.PutHist.P50())
	}
	if res.PutHist.P50() > res.PutHist.P90() {
		t.Errorf("Put P50 (%v) > P90 (%v)", res.PutHist.P50(), res.PutHist.P90())
	}
	if res.PutHist.P90() > res.PutHist.P99() {
		t.Errorf("Put P90 (%v) > P99 (%v)", res.PutHist.P90(), res.PutHist.P99())
	}
	if res.PutHist.P99() > res.PutHist.Max() {
		t.Errorf("Put P99 (%v) > Max (%v)", res.PutHist.P99(), res.PutHist.Max())
	}

	// 4. Verify Throughput Calculation
	if res.Throughput <= 0 {
		t.Errorf("expected positive throughput, got %f", res.Throughput)
	}

	// 5. Verify Report Formatting
	report := FormatReport(res)
	if !stringsContains(report, "LATTICE BENCHMARK REPORT") {
		t.Errorf("expected valid formatted report header")
	}
	if !stringsContains(report, "Operation Latencies:") {
		t.Errorf("expected latency section in report")
	}
}

// TestRunCLI_ExitCodes tests the CLI run entrypoint exit codes.
func TestRunCLI_ExitCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer

	// Help should return ExitSuccess (0)
	code := run([]string{"--help"}, &stdout, &stderr)
	if code != ExitSuccess {
		t.Errorf("expected ExitSuccess (0) for --help, got %d", code)
	}

	// Invalid flag should return ExitConfigError (1)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--invalid-flag"}, &stdout, &stderr)
	if code != ExitConfigError {
		t.Errorf("expected ExitConfigError (1) for invalid flag, got %d", code)
	}

	// Unreachable address should return ExitRuntimeError (2)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--address=127.0.0.1:59998", "--timeout=100ms", "--populate=false"}, &stdout, &stderr)
	if code != ExitRuntimeError {
		t.Errorf("expected ExitRuntimeError (2) for unreachable address, got %d", code)
	}
}

// Microbenchmarks measuring worker hot path allocations and CPU overhead

func BenchmarkWorker_KeyGen_Zipf(b *testing.B) {
	gen, err := benchmark.NewDefaultZipfGenerator(100_000, 42)
	if err != nil {
		b.Fatalf("zipf init error: %v", err)
	}
	var buf [64]byte

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = gen.NextKeyBuf(buf[:0])
	}
}

func BenchmarkWorker_KeyGen_Uniform(b *testing.B) {
	gen, err := benchmark.NewDefaultZipfGenerator(100_000, 42)
	if err != nil {
		b.Fatalf("zipf init error: %v", err)
	}
	rng := rand.New(rand.NewSource(42))
	var buf [64]byte
	keyspace := uint64(100_000)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rank := rng.Uint64() % keyspace
		_ = gen.FormatKey(buf[:0], rank)
	}
}

func BenchmarkWorker_OpSelection(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	ratio := 0.8

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = rng.Float64() < ratio
	}
}

func BenchmarkReport_Format(b *testing.B) {
	cfg := DefaultConfig()
	getHist := benchmark.NewLatencyHistogram()
	putHist := benchmark.NewLatencyHistogram()
	for i := 1; i <= 1000; i++ {
		getHist.Record(time.Duration(i) * time.Microsecond)
		putHist.Record(time.Duration(i*2) * time.Microsecond)
	}

	res := &Result{
		Config:     cfg,
		Elapsed:    60 * time.Second,
		GetHist:    getHist,
		PutHist:    putHist,
		GetCount:   1000,
		PutCount:   1000,
		TotalOps:   2000,
		Throughput: 33.33,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = FormatReport(res)
	}
}

func stringsContains(s, substr string) bool {
	return len(s) >= len(substr) && bytes.Contains([]byte(s), []byte(substr))
}

// TestRunner_ReconnectFailure_NoPanic tests that when a server drops an active connection
// and rejects subsequent reconnects, the worker loop does NOT panic (no nil pointer dereference)
// and shuts down gracefully upon context cancellation (remediating SEC-P13-M03-001).
func TestRunner_ReconnectFailure_NoPanic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var reqCount atomic.Int32
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read one request, send OK response, then close connection and listener
		req, err := transport.ReadRequest(conn)
		if err == nil {
			reqCount.Add(1)
			_ = transport.WriteResponse(conn, &transport.Response{
				OpCode: req.OpCode,
				Status: transport.StatusOk,
				SeqID:  req.SeqID,
			})
		}
		_ = conn.Close()
		_ = ln.Close() // Reject all reconnect attempts
	}()

	cfg := DefaultConfig()
	cfg.Address = ln.Addr().String()
	cfg.Concurrency = 1
	cfg.Duration = 300 * time.Millisecond
	cfg.Timeout = 100 * time.Millisecond
	cfg.Populate = false
	cfg.Workload = WorkloadWrite

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANIC detected on reconnect failure: %v", r)
		}
	}()

	var stdout, stderr bytes.Buffer
	res, err := Run(context.Background(), &cfg, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected Run error: %v", err)
	}
	if res.NetErrors == 0 {
		t.Errorf("expected non-zero NetErrors after server dropped connection")
	}
}

// TestRunner_DurationOverrun_Bounded verifies that stalled network I/O does not overrun
// the benchmark duration by the 5-second per-operation timeout (remediating SEC-P13-M03-003).
func TestRunner_DurationOverrun_Bounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// Server accepts connection and reads request, but deliberately stalls (sends no response)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = transport.ReadRequest(conn)
		// Stalled: do not respond, block until caller closes connection
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
	}()

	cfg := DefaultConfig()
	cfg.Address = ln.Addr().String()
	cfg.Concurrency = 1
	cfg.Duration = 200 * time.Millisecond
	cfg.Timeout = 5 * time.Second // 5s timeout >> 200ms duration
	cfg.Populate = false
	cfg.Workload = WorkloadWrite

	start := time.Now()
	var stdout, stderr bytes.Buffer
	res, err := Run(context.Background(), &cfg, &stdout, &stderr)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected Run error: %v", err)
	}

	// Must finish shortly after 200ms (e.g. < 1.5s) and NOT wait for 5s timeout
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("benchmark duration overrun: took %v (expected < 1.5s, timeout was 5s)", elapsed)
	}
	if res.NetErrors == 0 {
		t.Errorf("expected net error from stalled request")
	}
}

// TestRunner_WorkerInitLeak_DefensiveCleanup proves that worker initialization errors
// (e.g. invalid keyspace passed directly to Run) defensively close the dialed TCP connection (remediating SEC-P13-M03-004).
func TestRunner_WorkerInitLeak_DefensiveCleanup(t *testing.T) {
	tempDir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: tempDir})
	if err := eng.Open(); err != nil {
		t.Fatalf("engine open: %v", err)
	}
	defer eng.Close()

	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0"
	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Address = srv.Addr().String()
	cfg.Concurrency = 1
	cfg.Populate = false
	// Deliberately invalid keyspace to cause NewZipfGenerator to fail after Dial succeeds
	cfg.Keyspace = 0

	_, err = Run(context.Background(), &cfg, nil, nil)
	if err == nil {
		t.Fatalf("expected error from invalid keyspace")
	}

	// Verify server detects closed connection and drops active connection count to 0
	deadline := time.Now().Add(1000 * time.Millisecond)
	for srv.ActiveConnections() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if active := srv.ActiveConnections(); active != 0 {
		t.Fatalf("connection leak detected: server still reports %d active connections", active)
	}
}

// TestPrepopulation_FullKeyspaceCoverage proves that full keyspace pre-population prevents
// StatusKeyNotFound errors during subsequent read operations (remediating SEC-P13-M03-002).
func TestPrepopulation_FullKeyspaceCoverage(t *testing.T) {
	tempDir := t.TempDir()
	eng := engine.NewEngineWithOptions(engine.EngineOptions{DBPath: tempDir})
	if err := eng.Open(); err != nil {
		t.Fatalf("engine open: %v", err)
	}
	defer eng.Close()

	srvCfg := transport.DefaultServerConfig()
	srvCfg.Address = "127.0.0.1:0"
	srv, err := transport.NewServer(srvCfg, eng)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Address = srv.Addr().String()
	cfg.Concurrency = 2
	cfg.Duration = 500 * time.Millisecond
	cfg.Keyspace = 200
	cfg.PopulateKeys = 200
	cfg.Populate = true
	cfg.Workload = WorkloadRead // Pure read workload

	var stdout, stderr bytes.Buffer
	res, err := Run(context.Background(), &cfg, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.GetCount == 0 {
		t.Fatalf("expected positive GET count, got 0")
	}
	if res.GetErrors != 0 {
		t.Fatalf("expected 0 GET errors with 100%% populated keyspace, got %d", res.GetErrors)
	}
	if res.PopulatedCount != 200 {
		t.Errorf("expected 200 populated keys, got %d", res.PopulatedCount)
	}
}

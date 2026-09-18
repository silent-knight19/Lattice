package main

import (
	"bytes"
	"context"
	"math/rand"
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
	cfg.PopulateKeys = 100
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

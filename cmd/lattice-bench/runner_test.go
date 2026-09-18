package main

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

func TestFormatUint(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0"},
		{9, "9"},
		{99, "99"},
		{999, "999"},
		{1000, "1,000"},
		{10000, "10,000"},
		{100000, "100,000"},
		{1000000, "1,000,000"},
		{7452198, "7,452,198"},
		{18446744073709551615, "18,446,744,073,709,551,615"},
	}

	for _, tt := range tests {
		got := formatUint(tt.input)
		if got != tt.want {
			t.Errorf("formatUint(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{0, "0.00ms"},
		{500 * time.Nanosecond, "<0.01ms"},
		{10 * time.Microsecond, "0.01ms"},
		{210 * time.Microsecond, "0.21ms"},
		{1500 * time.Microsecond, "1.50ms"},
		{10 * time.Millisecond, "10.00ms"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatReport_ZeroSamples(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workload = WorkloadRead
	res := &Result{
		Config:     cfg,
		Elapsed:    10 * time.Second,
		GetHist:    benchmark.NewLatencyHistogram(),
		PutHist:    benchmark.NewLatencyHistogram(),
		GetCount:   0,
		PutCount:   0,
		TotalOps:   0,
		Throughput: 0,
	}

	report := FormatReport(res)
	if !strings.Contains(report, "LATTICE BENCHMARK REPORT") {
		t.Errorf("expected header in report")
	}
	if !strings.Contains(report, "Read-Only (100% GET)") {
		t.Errorf("expected read-only workload line")
	}
	if !strings.Contains(report, "GET") || !strings.Contains(report, "N/A") {
		t.Errorf("expected N/A for 0 GET samples")
	}
	if !strings.Contains(report, "PUT") || !strings.Contains(report, "N/A") {
		t.Errorf("expected N/A for 0 PUT samples")
	}
}

func TestFormatReport_WithSamples(t *testing.T) {
	cfg := DefaultConfig()
	getHist := benchmark.NewLatencyHistogram()
	getHist.Record(210 * time.Microsecond)
	getHist.Record(480 * time.Microsecond)
	getHist.Record(1120 * time.Microsecond)

	putHist := benchmark.NewLatencyHistogram()
	putHist.Record(840 * time.Microsecond)
	putHist.Record(1420 * time.Microsecond)

	res := &Result{
		Config:     cfg,
		Elapsed:    5 * time.Second,
		GetHist:    getHist,
		PutHist:    putHist,
		GetCount:   3,
		PutCount:   2,
		TotalOps:   5,
		Throughput: 1.0,
	}

	report := FormatReport(res)
	if !strings.Contains(report, "Total Operations      : 5 ops") {
		t.Errorf("expected 5 ops in report, got:\n%s", report)
	}
	if !strings.Contains(report, "GET                   3") {
		t.Errorf("expected 3 GETs in table, got:\n%s", report)
	}
	if !strings.Contains(report, "PUT                   2") {
		t.Errorf("expected 2 PUTs in table, got:\n%s", report)
	}
	if !strings.Contains(report, "Achieved Read Ratio : 60.00%") {
		t.Errorf("expected 60.00%% read ratio in report, got:\n%s", report)
	}
}

func TestWorkloadRatio_BernoulliConvergence(t *testing.T) {
	// Statistical check: 100,000 trials with readRatio=0.8 should be within 0.79..0.81
	rng := rand.New(rand.NewSource(12345))
	ratio := 0.8
	trials := 100_000
	var reads int

	for i := 0; i < trials; i++ {
		if rng.Float64() < ratio {
			reads++
		}
	}

	achieved := float64(reads) / float64(trials)
	if achieved < 0.79 || achieved > 0.81 {
		t.Errorf("expected achieved ratio ~0.80 (+-0.01), got %v", achieved)
	}
}

func TestDeterministicSeed_Reproducibility(t *testing.T) {
	seed := int64(98765)
	keyspace := uint64(1000)

	gen1, err1 := benchmark.NewZipfGenerator(keyspace, benchmark.DefaultZipfTheta, seed)
	if err1 != nil {
		t.Fatalf("zipf init error: %v", err1)
	}
	gen2, err2 := benchmark.NewZipfGenerator(keyspace, benchmark.DefaultZipfTheta, seed)
	if err2 != nil {
		t.Fatalf("zipf init error: %v", err2)
	}

	var buf1 [64]byte
	var buf2 [64]byte

	for i := 0; i < 1000; i++ {
		k1 := string(gen1.NextKeyBuf(buf1[:0]))
		k2 := string(gen2.NextKeyBuf(buf2[:0]))
		if k1 != k2 {
			t.Fatalf("iteration %d: key mismatch %q != %q", i, k1, k2)
		}
	}
}

func TestRun_NilConfig(t *testing.T) {
	_, err := Run(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatalf("expected error on nil config")
	}
}

func TestRun_InvalidAddressFailsFast(t *testing.T) {
	cfg := DefaultConfig()
	// Deliberately unreachable endpoint
	cfg.Address = "127.0.0.1:59999"
	cfg.Timeout = 100 * time.Millisecond
	cfg.Populate = false

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := Run(ctx, &cfg, nil, nil)
	if err == nil {
		t.Fatalf("expected error connecting to unreachable port")
	}
}

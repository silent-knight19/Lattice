package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseFlags_Defaults(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, isHelpOrVersion, err := ParseFlags([]string{}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error on default flags: %v", err)
	}
	if isHelpOrVersion {
		t.Fatalf("expected isHelpOrVersion to be false")
	}

	if cfg.Address != "127.0.0.1:9099" {
		t.Errorf("expected default address 127.0.0.1:9099, got %q", cfg.Address)
	}
	if cfg.Workload != WorkloadMixed {
		t.Errorf("expected default workload mixed, got %v", cfg.Workload)
	}
	if cfg.ReadRatio != 0.8 {
		t.Errorf("expected default read ratio 0.8, got %v", cfg.ReadRatio)
	}
	if cfg.Concurrency != 64 {
		t.Errorf("expected default concurrency 64, got %d", cfg.Concurrency)
	}
	if cfg.Duration != 60*time.Second {
		t.Errorf("expected default duration 60s, got %v", cfg.Duration)
	}
	if cfg.KeyDistribution != DistributionZipfian {
		t.Errorf("expected default distribution zipfian, got %v", cfg.KeyDistribution)
	}
	if cfg.Keyspace != 10_000 {
		t.Errorf("expected default keyspace 10000, got %d", cfg.Keyspace)
	}
	if cfg.ValSize != 256 {
		t.Errorf("expected default val-size 256, got %d", cfg.ValSize)
	}
	if cfg.Seed != 42 {
		t.Errorf("expected default seed 42, got %d", cfg.Seed)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("expected default timeout 5s, got %v", cfg.Timeout)
	}
	if !cfg.Populate {
		t.Errorf("expected default populate true, got false")
	}
	if cfg.PopulateKeys != DefaultPopulateCap {
		t.Errorf("expected default populate-keys %d, got %d", DefaultPopulateCap, cfg.PopulateKeys)
	}
}

func TestParseFlags_ValidCustomFlags(t *testing.T) {
	args := []string{
		"--address=127.0.0.1:9100",
		"--workload=read",
		"--concurrency=16",
		"--duration=10s",
		"--key-distribution=uniform",
		"--keys=50000",
		"--val-size=512",
		"--seed=12345",
		"--timeout=2s",
		"--populate=false",
	}

	var stdout, stderr bytes.Buffer
	cfg, isHelpOrVersion, err := ParseFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error parsing custom flags: %v", err)
	}
	if isHelpOrVersion {
		t.Fatalf("expected isHelpOrVersion to be false")
	}

	if cfg.Address != "127.0.0.1:9100" {
		t.Errorf("got address %q, want 127.0.0.1:9100", cfg.Address)
	}
	if cfg.Workload != WorkloadRead || cfg.ReadRatio != 1.0 {
		t.Errorf("got workload %v (ratio %v), want read (1.0)", cfg.Workload, cfg.ReadRatio)
	}
	if cfg.Concurrency != 16 {
		t.Errorf("got concurrency %d, want 16", cfg.Concurrency)
	}
	if cfg.Duration != 10*time.Second {
		t.Errorf("got duration %v, want 10s", cfg.Duration)
	}
	if cfg.KeyDistribution != DistributionUniform {
		t.Errorf("got distribution %v, want uniform", cfg.KeyDistribution)
	}
	if cfg.Keyspace != 50000 {
		t.Errorf("got keyspace %d, want 50000", cfg.Keyspace)
	}
	if cfg.ValSize != 512 {
		t.Errorf("got val-size %d, want 512", cfg.ValSize)
	}
	if cfg.Seed != 12345 {
		t.Errorf("got seed %d, want 12345", cfg.Seed)
	}
	if cfg.Timeout != 2*time.Second {
		t.Errorf("got timeout %v, want 2s", cfg.Timeout)
	}
	if cfg.Populate {
		t.Errorf("got populate true, want false")
	}
}

func TestParseFlags_WorkloadWrite(t *testing.T) {
	args := []string{"--workload=write"}
	var stdout, stderr bytes.Buffer
	cfg, _, err := ParseFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Workload != WorkloadWrite || cfg.ReadRatio != 0.0 {
		t.Errorf("got workload %v with ratio %v, want write with ratio 0.0", cfg.Workload, cfg.ReadRatio)
	}
}

func TestParseFlags_KeyspaceAlias(t *testing.T) {
	args := []string{"--keyspace=25000", "--populate-keys=25000"}
	var stdout, stderr bytes.Buffer
	cfg, _, err := ParseFlags(args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Keyspace != 25000 {
		t.Errorf("got keyspace %d, want 25000", cfg.Keyspace)
	}
}

func TestParseFlags_HelpAndVersion(t *testing.T) {
	// Help
	var stdout, stderr bytes.Buffer
	_, isHelp, err := ParseFlags([]string{"--help"}, &stdout, &stderr)
	if err != nil || !isHelp {
		t.Errorf("expected isHelp=true, got err=%v, isHelp=%v", err, isHelp)
	}
	if !strings.Contains(stdout.String(), "Usage: lattice-bench") {
		t.Errorf("expected usage instructions in stdout")
	}

	// Version
	stdout.Reset()
	stderr.Reset()
	_, isVersion, err := ParseFlags([]string{"--version"}, &stdout, &stderr)
	if err != nil || !isVersion {
		t.Errorf("expected isVersion=true, got err=%v, isVersion=%v", err, isVersion)
	}
	if !strings.Contains(stdout.String(), Version) {
		t.Errorf("expected version string in stdout")
	}
}

func TestParseFlags_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "empty address",
			args:    []string{"--address="},
			wantErr: "address cannot be empty",
		},
		{
			name:    "invalid address missing port",
			args:    []string{"--address=127.0.0.1"},
			wantErr: "invalid address format",
		},
		{
			name:    "invalid port 0",
			args:    []string{"--address=127.0.0.1:0"},
			wantErr: "invalid port",
		},
		{
			name:    "invalid port 70000",
			args:    []string{"--address=127.0.0.1:70000"},
			wantErr: "invalid port",
		},
		{
			name:    "unknown workload",
			args:    []string{"--workload=scan"},
			wantErr: "unknown workload",
		},
		{
			name:    "read ratio negative",
			args:    []string{"--read-ratio=-0.1"},
			wantErr: "invalid read-ratio",
		},
		{
			name:    "read ratio greater than 1",
			args:    []string{"--read-ratio=1.1"},
			wantErr: "invalid read-ratio",
		},
		{
			name:    "concurrency zero",
			args:    []string{"--concurrency=0"},
			wantErr: "invalid concurrency",
		},
		{
			name:    "concurrency negative",
			args:    []string{"--concurrency=-5"},
			wantErr: "invalid concurrency",
		},
		{
			name:    "concurrency exceeds max 1024",
			args:    []string{"--concurrency=1025"},
			wantErr: "invalid concurrency",
		},
		{
			name:    "duration zero",
			args:    []string{"--duration=0s"},
			wantErr: "invalid duration",
		},
		{
			name:    "duration below min 100ms",
			args:    []string{"--duration=50ms"},
			wantErr: "invalid duration",
		},
		{
			name:    "duration above max 24h",
			args:    []string{"--duration=25h"},
			wantErr: "invalid duration",
		},
		{
			name:    "unknown distribution",
			args:    []string{"--key-distribution=poisson"},
			wantErr: "unknown key-distribution",
		},
		{
			name:    "keyspace zero",
			args:    []string{"--keys=0"},
			wantErr: "invalid keyspace",
		},
		{
			name:    "keyspace exceeds max 1B",
			args:    []string{"--keys=1000000001"},
			wantErr: "invalid keyspace",
		},
		{
			name:    "val size negative",
			args:    []string{"--val-size=-1"},
			wantErr: "invalid val-size",
		},
		{
			name:    "val size exceeds max 4MB",
			args:    []string{"--val-size=4194305"},
			wantErr: "invalid val-size",
		},
		{
			name:    "timeout below min 100ms",
			args:    []string{"--timeout=50ms"},
			wantErr: "invalid timeout",
		},
		{
			name:    "timeout above max 60s",
			args:    []string{"--timeout=61s"},
			wantErr: "invalid timeout",
		},
		{
			name:    "populate keys exceeds keyspace",
			args:    []string{"--keys=500", "--populate-keys=1000"},
			wantErr: "populate-keys (1000) cannot exceed total keyspace (500)",
		},
		{
			name:    "keyspace exceeds default cap without explicit populate keys for mixed workload",
			args:    []string{"--keys=50000"},
			wantErr: "exceeds default populate cap",
		},
		{
			name:    "keyspace exceeds default cap without explicit populate keys for read workload",
			args:    []string{"--workload=read", "--keys=50000"},
			wantErr: "exceeds default populate cap",
		},
		{
			name:    "partial prepopulation rejected for mixed workload",
			args:    []string{"--keys=50000", "--populate-keys=10000"},
			wantErr: "partial pre-population",
		},
		{
			name:    "partial prepopulation rejected for read workload",
			args:    []string{"--workload=read", "--keys=50000", "--populate-keys=10000"},
			wantErr: "partial pre-population",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			_, _, err := ParseFlags(tt.args, &stdout, &stderr)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestParseFlags_SmallKeyspacePopulateHeuristic(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg, _, err := ParseFlags([]string{"--keys=500"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PopulateKeys != 500 {
		t.Errorf("expected populate-keys to match small keyspace 500, got %d", cfg.PopulateKeys)
	}
}

func TestParseFlags_PrepopulationSemantics(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantPopulate bool
		wantKeys     uint64
		wantPopKeys  uint64
	}{
		{
			name:         "default configuration populates 100% of keyspace",
			args:         []string{},
			wantPopulate: true,
			wantKeys:     10_000,
			wantPopKeys:  10_000,
		},
		{
			name:         "keyspace 1",
			args:         []string{"--keys=1"},
			wantPopulate: true,
			wantKeys:     1,
			wantPopKeys:  1,
		},
		{
			name:         "keyspace 100",
			args:         []string{"--keys=100"},
			wantPopulate: true,
			wantKeys:     100,
			wantPopKeys:  100,
		},
		{
			name:         "keyspace 10000 explicitly equal",
			args:         []string{"--keys=10000", "--populate-keys=10000"},
			wantPopulate: true,
			wantKeys:     10_000,
			wantPopKeys:  10_000,
		},
		{
			name:         "large keyspace with explicit full population",
			args:         []string{"--keys=50000", "--populate-keys=50000"},
			wantPopulate: true,
			wantKeys:     50_000,
			wantPopKeys:  50_000,
		},
		{
			name:         "large keyspace with populate disabled",
			args:         []string{"--keys=50000", "--populate=false"},
			wantPopulate: false,
			wantKeys:     50_000,
			wantPopKeys:  0,
		},
		{
			name:         "large keyspace with write workload",
			args:         []string{"--workload=write", "--keys=50000"},
			wantPopulate: true,
			wantKeys:     50_000,
			wantPopKeys:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cfg, _, err := ParseFlags(tt.args, &stdout, &stderr)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Populate != tt.wantPopulate {
				t.Errorf("got Populate %v, want %v", cfg.Populate, tt.wantPopulate)
			}
			if cfg.Keyspace != tt.wantKeys {
				t.Errorf("got Keyspace %d, want %d", cfg.Keyspace, tt.wantKeys)
			}
			if cfg.PopulateKeys != tt.wantPopKeys {
				t.Errorf("got PopulateKeys %d, want %d", cfg.PopulateKeys, tt.wantPopKeys)
			}
		})
	}
}

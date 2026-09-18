package main

import (
	"bytes"
	"strings"
	"testing"
)

func FuzzParseFlags(f *testing.F) {
	// Seed initial interesting flag permutations
	f.Add("--address=127.0.0.1:9099 --workload=mixed --read-ratio=0.8 --concurrency=64 --duration=10s")
	f.Add("--address=localhost:8080 --workload=read --concurrency=1 --keys=1000")
	f.Add("--address=10.0.0.1:9999 --workload=write --val-size=1024 --seed=999")
	f.Add("--workload=invalid --concurrency=-1 --duration=0s")
	f.Add("--keys=0 --val-size=99999999 --read-ratio=2.5")
	f.Add("--help -v --timeout=1s")

	f.Fuzz(func(t *testing.T, input string) {
		// Tokenize input string by spaces
		args := strings.Fields(input)
		var stdout, stderr bytes.Buffer

		// ParseFlags must never panic under arbitrary untrusted input
		cfg, isHelp, err := ParseFlags(args, &stdout, &stderr)
		if err == nil && !isHelp {
			// If parsing succeeded, invariants must hold
			if cfg.Concurrency < MinConcurrency || cfg.Concurrency > MaxConcurrency {
				t.Fatalf("fuzz invariant violated: invalid concurrency %d accepted", cfg.Concurrency)
			}
			if cfg.Duration < MinDuration || cfg.Duration > MaxDuration {
				t.Fatalf("fuzz invariant violated: invalid duration %v accepted", cfg.Duration)
			}
			if cfg.ReadRatio < 0.0 || cfg.ReadRatio > 1.0 {
				t.Fatalf("fuzz invariant violated: invalid read ratio %v accepted", cfg.ReadRatio)
			}
			if cfg.ValSize < MinValSize || cfg.ValSize > MaxValSize {
				t.Fatalf("fuzz invariant violated: invalid val-size %d accepted", cfg.ValSize)
			}
			if cfg.Populate && cfg.Workload != WorkloadWrite {
				if cfg.PopulateKeys != cfg.Keyspace {
					t.Fatalf("fuzz invariant violated: partial pre-population %d < %d accepted", cfg.PopulateKeys, cfg.Keyspace)
				}
			}
		}
	})
}

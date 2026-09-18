package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/silent-knight19/lattice/internal/benchmark"
)

// Result encapsulates the aggregated outcome of a benchmark execution.
type Result struct {
	Config          Config
	Elapsed         time.Duration
	GetHist         *benchmark.LatencyHistogram
	PutHist         *benchmark.LatencyHistogram
	GetCount        uint64
	PutCount        uint64
	GetErrors       uint64
	PutErrors       uint64
	NetErrors       uint64
	TotalOps        uint64
	Throughput      float64
	PopulatedCount  uint64
	PopulateElapsed time.Duration
}

// FormatReport generates the human-readable terminal benchmark report.
func FormatReport(res *Result) string {
	if res == nil {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("====================== LATTICE BENCHMARK REPORT ======================\n")

	// Configuration Section
	fmt.Fprintf(&sb, "Target Node           : %s\n", res.Config.Address)
	switch res.Config.Workload {
	case WorkloadRead:
		sb.WriteString("Workload Profile      : Read-Only (100% GET)\n")
	case WorkloadWrite:
		sb.WriteString("Workload Profile      : Write-Only (100% PUT)\n")
	case WorkloadMixed:
		fmt.Fprintf(&sb, "Workload Profile      : Mixed (%.1f%% Read / %.1f%% Write)\n",
			res.Config.ReadRatio*100.0, (1.0-res.Config.ReadRatio)*100.0)
	}

	if res.Config.KeyDistribution == DistributionZipfian {
		fmt.Fprintf(&sb, "Key Distribution      : Zipfian (theta = %.2f)\n", benchmark.DefaultZipfTheta)
	} else {
		sb.WriteString("Key Distribution      : Uniform\n")
	}

	fmt.Fprintf(&sb, "Keyspace Domain       : %s keys\n", formatUint(res.Config.Keyspace))
	fmt.Fprintf(&sb, "Value Payload Size    : %d bytes\n", res.Config.ValSize)
	fmt.Fprintf(&sb, "Random Seed           : %d\n", res.Config.Seed)
	fmt.Fprintf(&sb, "Active Connections    : %d Concurrently Active Goroutines\n", res.Config.Concurrency)

	// Pre-population if executed
	if res.PopulatedCount > 0 {
		fmt.Fprintf(&sb, "Pre-Populated Keys    : %s keys (in %v)\n",
			formatUint(res.PopulatedCount), res.PopulateElapsed.Round(time.Millisecond))
	}

	// Execution Section
	fmt.Fprintf(&sb, "Total Duration        : %v\n", res.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(&sb, "Total Operations      : %s ops\n", formatUint(res.TotalOps))
	fmt.Fprintf(&sb, "Achieved Throughput   : %.2f ops/sec\n", res.Throughput)

	// Latency Table
	sb.WriteString("\nOperation Latencies:\n")
	sb.WriteString("  Operation        Count       Min      Mean       P50       P90       P99     P99.9       Max\n")
	sb.WriteString("  ------------------------------------------------------------------------------------------\n")
	sb.WriteString(formatLatencyRow("GET", res.GetHist))
	sb.WriteString(formatLatencyRow("PUT", res.PutHist))

	// Execution Summary
	sb.WriteString("\nExecution Summary:\n")
	fmt.Fprintf(&sb, "  Successful Ops      : %s (GET: %s, PUT: %s)\n",
		formatUint(res.TotalOps), formatUint(res.GetCount), formatUint(res.PutCount))

	totalErrors := res.GetErrors + res.PutErrors + res.NetErrors
	if totalErrors > 0 {
		fmt.Fprintf(&sb, "  Failed Ops          : %s (GET Errors: %s, PUT Errors: %s, Network Drops: %s)\n",
			formatUint(totalErrors), formatUint(res.GetErrors), formatUint(res.PutErrors), formatUint(res.NetErrors))
	} else {
		sb.WriteString("  Failed Ops          : 0 (GET Errors: 0, PUT Errors: 0, Network Drops: 0)\n")
	}

	if res.TotalOps > 0 {
		achievedReadRatio := float64(res.GetCount) / float64(res.TotalOps) * 100.0
		fmt.Fprintf(&sb, "  Achieved Read Ratio : %.2f%%\n", achievedReadRatio)
	}

	sb.WriteString("======================================================================\n")
	return sb.String()
}

// formatLatencyRow formats a single operation row in the latency percentile table.
func formatLatencyRow(opName string, h *benchmark.LatencyHistogram) string {
	if h == nil || h.Count() == 0 {
		return fmt.Sprintf("  %-10s %12s       N/A       N/A       N/A       N/A       N/A       N/A       N/A\n",
			opName, "0")
	}

	return fmt.Sprintf("  %-10s %12s %9s %9s %9s %9s %9s %9s %9s\n",
		opName,
		formatUint(h.Count()),
		formatDuration(h.Min()),
		formatDuration(h.Mean()),
		formatDuration(h.P50()),
		formatDuration(h.P90()),
		formatDuration(h.P99()),
		formatDuration(h.P999()),
		formatDuration(h.Max()),
	)
}

// formatDuration formats a duration into human-readable milliseconds (e.g. "0.21ms", "14.50ms").
func formatDuration(d time.Duration) string {
	ms := float64(d.Nanoseconds()) / 1e6
	if ms < 0.001 && d > 0 {
		return "<0.01ms"
	}
	return fmt.Sprintf("%.2fms", ms)
}

// formatUint formats a uint64 with comma separators (e.g. 7452198 -> "7,452,198").
func formatUint(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var res []byte
	rem := len(s) % 3
	if rem > 0 {
		res = append(res, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(res) > 0 {
			res = append(res, ',')
		}
		res = append(res, s[i:i+3]...)
	}
	return string(res)
}

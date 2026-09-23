package metrics

import (
	"bytes"
	"math"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCounter_Basics(t *testing.T) {
	c := NewCounter()
	if c.Value() != 0 {
		t.Fatalf("expected initial 0, got %d", c.Value())
	}
	c.Inc()
	if c.Value() != 1 {
		t.Fatalf("expected 1, got %d", c.Value())
	}
	c.Add(10)
	if c.Value() != 11 {
		t.Fatalf("expected 11, got %d", c.Value())
	}
}

func TestCounter_Concurrent(t *testing.T) {
	c := NewCounter()
	const workers = 20
	const iters = 1000

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if c.Value() != workers*iters {
		t.Fatalf("expected %d, got %d", workers*iters, c.Value())
	}
}

func TestGauge_Basics(t *testing.T) {
	g := NewGauge()
	if g.Value() != 0 {
		t.Fatalf("expected 0, got %d", g.Value())
	}
	g.Set(42)
	if g.Value() != 42 {
		t.Fatalf("expected 42, got %d", g.Value())
	}
	g.Add(8)
	if g.Value() != 50 {
		t.Fatalf("expected 50, got %d", g.Value())
	}
	g.Sub(20)
	if g.Value() != 30 {
		t.Fatalf("expected 30, got %d", g.Value())
	}
}

func TestHistogram_BucketClassification(t *testing.T) {
	buckets := []float64{0.001, 0.005, 0.010} // 1ms, 5ms, 10ms
	h := NewHistogram(buckets)

	h.Observe(0.0005) // -> <= 1ms
	h.Observe(0.0010) // -> <= 1ms
	h.Observe(0.0030) // -> <= 5ms
	h.Observe(0.0070) // -> <= 10ms
	h.Observe(0.0200) // -> > 10ms (+Inf)

	snap := h.Snapshot()
	if snap.Count != 5 {
		t.Fatalf("expected count 5, got %d", snap.Count)
	}

	// Cumulative checks:
	// <= 1ms: 2
	if snap.Cumulative[0] != 2 {
		t.Errorf("expected cumulative[0] (<=1ms) = 2, got %d", snap.Cumulative[0])
	}
	// <= 5ms: 2 + 1 = 3
	if snap.Cumulative[1] != 3 {
		t.Errorf("expected cumulative[1] (<=5ms) = 3, got %d", snap.Cumulative[1])
	}
	// <= 10ms: 3 + 1 = 4
	if snap.Cumulative[2] != 4 {
		t.Errorf("expected cumulative[2] (<=10ms) = 4, got %d", snap.Cumulative[2])
	}
	// +Inf: 5
	if snap.Cumulative[3] != 5 {
		t.Errorf("expected cumulative[3] (+Inf) = 5, got %d", snap.Cumulative[3])
	}

	expectedSum := 0.0005 + 0.0010 + 0.0030 + 0.0070 + 0.0200
	if math.Abs(snap.SumSeconds-expectedSum) > 1e-6 {
		t.Errorf("expected sum %f, got %f", expectedSum, snap.SumSeconds)
	}
}

func TestHistogram_ZeroAllocOnHotPath(t *testing.T) {
	h := NewHistogram(DefaultLatencyBuckets)

	// Warmup
	h.ObserveDuration(100 * time.Microsecond)

	allocs := testing.AllocsPerRun(1000, func() {
		h.ObserveDuration(250 * time.Microsecond)
	})

	if allocs > 0 {
		t.Fatalf("expected 0 allocs on histogram ObserveDuration hot path, got %f", allocs)
	}
}

func TestHistogram_ConcurrentStress(t *testing.T) {
	h := NewHistogram(DefaultLatencyBuckets)
	const workers = 16
	const iters = 2000

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)))
			for j := 0; j < iters; j++ {
				dur := time.Duration(rng.Intn(5000)) * time.Microsecond
				h.ObserveDuration(dur)
			}
		}(i)
	}
	wg.Wait()

	snap := h.Snapshot()
	if snap.Count != workers*iters {
		t.Fatalf("expected total count %d, got %d", workers*iters, snap.Count)
	}
	if snap.Cumulative[len(snap.Cumulative)-1] != workers*iters {
		t.Fatalf("expected +Inf bucket %d, got %d", workers*iters, snap.Cumulative[len(snap.Cumulative)-1])
	}
	// Monotonicity check across cumulative buckets
	for i := 1; i < len(snap.Cumulative); i++ {
		if snap.Cumulative[i] < snap.Cumulative[i-1] {
			t.Fatalf("cumulative bucket %d (%d) < bucket %d (%d)",
				i, snap.Cumulative[i], i-1, snap.Cumulative[i-1])
		}
	}
}

func TestHistogramVec_CardinalityDefense(t *testing.T) {
	hv := NewHistogramVec(
		DefaultLatencyBuckets,
		[]string{"op"},
		map[string][]string{
			"op": {"put", "delete"},
		},
	)

	// Valid operations should work
	hv.WithLabelValues("put").ObserveDuration(time.Millisecond)
	hv.WithLabelValues("delete").ObserveDuration(2 * time.Millisecond)

	snapPut := hv.WithLabelValues("put").Snapshot()
	if snapPut.Count != 1 {
		t.Fatalf("expected put count 1, got %d", snapPut.Count)
	}

	snapDelete := hv.WithLabelValues("delete").Snapshot()
	if snapDelete.Count != 1 {
		t.Fatalf("expected delete count 1, got %d", snapDelete.Count)
	}

	// Adversarial input: attacker supplies malicious/unbounded keys or extra labels
	for _, mal := range []string{"drop_table", "../../../secret", "1; DROP TABLE", "random_uuid_1234"} {
		// Must not panic, must not create new series in entries
		h := hv.WithLabelValues(mal)
		h.ObserveDuration(time.Millisecond)
	}

	// Verify entries map cardinality has not expanded
	if len(hv.entries) != 2 {
		t.Fatalf("expected exactly 2 entries in HistogramVec, got %d (cardinality leak!)", len(hv.entries))
	}
}

func TestRegistry_PrometheusExpositionFormatting(t *testing.T) {
	reg := NewRegistry()

	c := NewCounter()
	c.Add(12345)
	reg.RegisterCounter("lattice_wal_bytes_written_total", "Total WAL bytes", c)

	g := NewGauge()
	g.Set(42)
	reg.RegisterGauge("lattice_connections_active", "Active client connections", g)

	reg.RegisterGaugeFunc("lattice_memtable_active_bytes", "Active memtable bytes", nil, func() int64 {
		return 67108864
	})

	h := NewHistogram([]float64{0.001, 0.005})
	h.Observe(0.0005)
	h.Observe(0.003)
	reg.RegisterHistogram("lattice_engine_read_latency_seconds", "Read latency", h)

	hv := NewHistogramVec(
		[]float64{0.001, 0.010},
		[]string{"op"},
		map[string][]string{"op": {"put"}},
	)
	hv.WithLabelValues("put").Observe(0.002)
	reg.RegisterHistogramVec("lattice_engine_write_latency_seconds", "Write latency", hv)

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("failed to write Prometheus output: %v", err)
	}

	output := buf.String()

	// Verify standard comments
	expectedSubstrings := []string{
		"# HELP lattice_wal_bytes_written_total Total WAL bytes",
		"# TYPE lattice_wal_bytes_written_total counter",
		"lattice_wal_bytes_written_total 12345",
		"# HELP lattice_connections_active Active client connections",
		"# TYPE lattice_connections_active gauge",
		"lattice_connections_active 42",
		"# HELP lattice_memtable_active_bytes Active memtable bytes",
		"# TYPE lattice_memtable_active_bytes gauge",
		"lattice_memtable_active_bytes 67108864",
		"# HELP lattice_engine_read_latency_seconds Read latency",
		"# TYPE lattice_engine_read_latency_seconds histogram",
		`lattice_engine_read_latency_seconds_bucket{le="0.001"} 1`,
		`lattice_engine_read_latency_seconds_bucket{le="0.005"} 2`,
		`lattice_engine_read_latency_seconds_bucket{le="+Inf"} 2`,
		"lattice_engine_read_latency_seconds_count 2",
		"# HELP lattice_engine_write_latency_seconds Write latency",
		"# TYPE lattice_engine_write_latency_seconds histogram",
		`lattice_engine_write_latency_seconds_bucket{op="put",le="0.001"} 0`,
		`lattice_engine_write_latency_seconds_bucket{op="put",le="0.01"} 1`,
		`lattice_engine_write_latency_seconds_bucket{op="put",le="+Inf"} 1`,
		`lattice_engine_write_latency_seconds_count{op="put"} 1`,
	}

	for _, sub := range expectedSubstrings {
		if !strings.Contains(output, sub) {
			t.Errorf("missing expected substring %q in Prometheus output:\n%s", sub, output)
		}
	}
}

func TestRegistry_LabelAndHelpEscaping(t *testing.T) {
	// Test escaping of special characters in labels and help strings
	rawLabel := `test"value\with\backslashes
and newline`
	escapedLabel := EscapeLabelValue(rawLabel)
	expectedLabel := `test\"value\\with\\backslashes\nand newline`
	if escapedLabel != expectedLabel {
		t.Fatalf("expected escaped label %q, got %q", expectedLabel, escapedLabel)
	}

	rawHelp := `Help with \ and
newline`
	escapedHelp := EscapeHelpText(rawHelp)
	expectedHelp := `Help with \\ and\nnewline`
	if escapedHelp != expectedHelp {
		t.Fatalf("expected escaped help %q, got %q", expectedHelp, escapedHelp)
	}
}

func TestRegistry_AdversarialOutputInjection(t *testing.T) {
	// Adversarial test strings designed to attempt Prometheus line injection
	maliciousValues := []string{
		`"`,
		`\`,
		"\n",
		`,`,
		`=`,
		`{}`,
		`#`,
		"val\"\nmalicious_metric 12345\n#",
		"x\"}\ninjected_series{danger=\"true\"} 1\n#",
	}

	for _, mal := range maliciousValues {
		escaped := EscapeLabelValue(mal)
		if strings.Contains(escaped, "\n") {
			t.Errorf("escaped value must never contain unescaped newline: %q -> %q", mal, escaped)
		}
		// When rendered in formatLabels, verify it doesn't break out of quotes
		labels := []Label{{Name: "test_label", Value: mal}}
		formatted := formatLabels(labels, "", "")
		// Ensure every line inside formatted remains on a single line (no raw newlines)
		if strings.Contains(formatted, "\n") {
			t.Errorf("formatted labels contain unescaped newline for input %q: %q", mal, formatted)
		}
	}
}

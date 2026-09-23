package metrics

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func FuzzHistogram_Observe(f *testing.F) {
	f.Add(0.0)
	f.Add(0.0001)
	f.Add(1.5)
	f.Add(100.0)
	f.Add(-10.0)
	f.Add(math.MaxFloat64)

	h := NewHistogram(DefaultLatencyBuckets)

	f.Fuzz(func(t *testing.T, val float64) {
		h.Observe(val)
		snap := h.Snapshot()
		if snap.Count == 0 {
			t.Fatalf("expected count >= 1 after observation")
		}
		if snap.Cumulative[len(snap.Cumulative)-1] != snap.Count {
			t.Fatalf("expected +Inf bucket to equal total count")
		}
	})
}

func FuzzRegistry_LabelEscaping(f *testing.F) {
	f.Add("normal_value")
	f.Add("with\"quote")
	f.Add("with\\backslash")
	f.Add("with\nnewline")
	f.Add("\"\\\n\r\t")

	f.Fuzz(func(t *testing.T, input string) {
		escaped := EscapeLabelValue(input)
		// Escaped string must not have unescaped quotes or raw newlines
		// A quote is escaped if preceded by an odd number of backslashes
		for i := 0; i < len(escaped); i++ {
			if escaped[i] == '\n' {
				t.Fatalf("raw newline found in escaped output: %q", escaped)
			}
			if escaped[i] == '"' {
				// Count preceding backslashes
				slashes := 0
				for j := i - 1; j >= 0 && escaped[j] == '\\'; j-- {
					slashes++
				}
				if slashes%2 == 0 {
					t.Fatalf("unescaped quote found in escaped output: %q", escaped)
				}
			}
		}
	})
}

func FuzzRegistry_WritePrometheus(f *testing.F) {
	f.Add("counter_help", "gauge_help")
	f.Add("help with \"quotes\"", "help with \\backslash")
	f.Add("multi\nline\nhelp", "another\nhelp")

	f.Fuzz(func(t *testing.T, counterHelp, gaugeHelp string) {
		reg := NewRegistry()
		c := NewCounter()
		c.Add(10)
		reg.RegisterCounter("fuzz_counter", counterHelp, c)

		g := NewGauge()
		g.Set(-5)
		reg.RegisterGauge("fuzz_gauge", gaugeHelp, g)

		var buf bytes.Buffer
		if err := reg.WritePrometheus(&buf); err != nil {
			t.Fatalf("unexpected error writing prometheus format: %v", err)
		}

		out := buf.String()
		lines := strings.Split(out, "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "#") {
				// Comment lines must not contain raw line breaks
				if strings.Count(line, "\r") > 0 {
					t.Fatalf("carriage return in comment: %q", line)
				}
			}
		}
	})
}

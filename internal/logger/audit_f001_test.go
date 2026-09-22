package logger_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/silent-knight19/lattice/internal/logger"
)

// auditNode is a cyclic-capable structure used to prove that logging
// reference cycles cannot exhaust the goroutine stack (AUDIT-F-001).
type auditNode struct {
	Name string
	Next *auditNode
}

// panicStringer panics in String to prove logging never propagates
// caller-controlled panics out of a log call (AUDIT-F-001).
type panicStringer struct{}

func (panicStringer) String() string { panic("hostile String()") }

// TestAudit_F001_CyclicStructCannotCrashLogger is the regression test for
// AUDIT-F-001 (CRITICAL): pre-fix, scrubValue recursed without bound on
// cyclic structures and died with a fatal, unrecoverable stack overflow that
// killed the entire test binary. Post-fix the call must return promptly with
// the cycle truncated.
func TestAudit_F001_CyclicStructCannotCrashLogger(t *testing.T) {
	a := &auditNode{Name: "a"}
	b := &auditNode{Name: "b"}
	a.Next = b
	b.Next = a // reference cycle

	var buf bytes.Buffer
	lg := logger.New(logger.Config{Output: &buf, Format: logger.FormatJSON})
	lg.Info("cycle test", "node", a)

	out := buf.String()
	if !strings.Contains(out, "cycle test") {
		t.Fatalf("expected log record to be emitted, got %q", out)
	}
	if !strings.Contains(out, logger.RedactedPlaceholder) {
		t.Fatalf("expected cycle truncation marker %q in output, got %q", logger.RedactedPlaceholder, out)
	}
}

// TestAudit_F001_CyclicMapCannotCrashLogger covers cycles formed through
// maps and interface indirection rather than struct pointers.
func TestAudit_F001_CyclicMapCannotCrashLogger(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	m["password"] = "supersecret"

	var buf bytes.Buffer
	lg := logger.New(logger.Config{Output: &buf, Format: logger.FormatJSON})
	lg.Info("cycle map test", "m", m)

	out := buf.String()
	if !strings.Contains(out, "cycle map test") {
		t.Fatalf("expected log record to be emitted, got %q", out)
	}
	if strings.Contains(out, "supersecret") {
		t.Fatalf("sensitive value leaked through cyclic map: %q", out)
	}
}

// TestAudit_F001_DeepNestingIsBounded proves adversarially deep (but acyclic)
// nesting terminates instead of exhausting the stack.
func TestAudit_F001_DeepNestingIsBounded(t *testing.T) {
	var v any = "leaf"
	for i := 0; i < 100000; i++ {
		v = []any{v}
	}

	var buf bytes.Buffer
	lg := logger.New(logger.Config{Output: &buf, Format: logger.FormatJSON})
	lg.Info("deep test", "v", v)

	if !strings.Contains(buf.String(), "deep test") {
		t.Fatalf("expected log record to be emitted")
	}
}

// TestAudit_F001_PanickingStringerCannotCrashLogger proves a hostile
// fmt.Stringer cannot propagate a panic out of the logging call.
func TestAudit_F001_PanickingStringerCannotCrashLogger(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(logger.Config{Output: &buf, Format: logger.FormatJSON})

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("log call propagated caller panic: %v", rec)
		}
	}()
	lg.Info("stringer test", "v", panicStringer{})

	out := buf.String()
	if !strings.Contains(out, "stringer test") {
		t.Fatalf("expected log record to be emitted, got %q", out)
	}
	if !strings.Contains(out, logger.RedactedPlaceholder) {
		t.Fatalf("expected panicking value to be redacted, got %q", out)
	}
}

package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"unicode"
)

// Level defines the logging severity level for Lattice.
type Level int

const (
	// LevelDebug logs high-volume diagnostic and debugging events.
	LevelDebug Level = iota
	// LevelInfo logs normal operational and lifecycle events.
	LevelInfo
	// LevelWarn logs non-critical anomalies, retries, and degradations.
	LevelWarn
	// LevelError logs actionable failures, corruptions, and aborts.
	LevelError
)

// String returns the uppercase string representation of the Level.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// toSlog maps the Lattice Level to slog.Level.
func (l Level) toSlog() slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelInfo:
		return slog.LevelInfo
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Format defines the serialization format of the logger output.
type Format int

const (
	// FormatJSON formats logs as structured JSON lines (production default).
	FormatJSON Format = iota
	// FormatText formats logs as human-readable key-value text (development / CLI).
	FormatText
)

// String returns the string representation of the Format.
func (f Format) String() string {
	switch f {
	case FormatJSON:
		return "json"
	case FormatText:
		return "text"
	default:
		return "unknown"
	}
}

// Redactable allows custom domain types to provide safe representations
// for logging, stripping credentials, internal keys, or raw payloads.
//
// Security Precedence: Sensitive attribute keys take strict precedence over Redactable.
// If an attribute key is classified as sensitive, the logger replaces the entire value
// with RedactedPlaceholder immediately without invoking Redact(). Redact() is only evaluated
// for non-sensitive attribute keys.
type Redactable interface {
	Redact() any
}

// RedactedPlaceholder is the string value substituted for sensitive fields.
const RedactedPlaceholder = "[REDACTED]"

// defaultSensitiveKeys is the baseline set of attribute keys that are automatically redacted.
var defaultSensitiveKeys = map[string]struct{}{
	"password":      {},
	"passwd":        {},
	"secret":        {},
	"token":         {},
	"auth":          {},
	"authorization": {},
	"api_key":       {},
	"apikey":        {},
	"private_key":   {},
	"credential":    {},
	"credentials":   {},
	"access_token":  {},
	"refresh_token": {},
}

// Config specifies configuration options for initializing a Logger.
type Config struct {
	// Level specifies the minimum severity level to log. Defaults to LevelInfo.
	Level Level
	// Format specifies the output serialization format (FormatJSON or FormatText). Defaults to FormatJSON.
	Format Format
	// Output is the destination writer for log records. Defaults to os.Stdout if nil.
	Output io.Writer
	// AddSource adds the source code file and line number to log records.
	AddSource bool
	// RedactedKeys provides additional attribute keys to automatically redact.
	RedactedKeys []string
}

// Logger defines the foundational structured logging contract for Lattice subsystems.
// All implementations must be concurrency-safe for simultaneous use across goroutines.
type Logger interface {
	// Debug logs at LevelDebug.
	Debug(msg string, args ...any)
	// Info logs at LevelInfo.
	Info(msg string, args ...any)
	// Warn logs at LevelWarn.
	Warn(msg string, args ...any)
	// Error logs at LevelError.
	Error(msg string, args ...any)

	// DebugContext logs at LevelDebug with the given context.
	DebugContext(ctx context.Context, msg string, args ...any)
	// InfoContext logs at LevelInfo with the given context.
	InfoContext(ctx context.Context, msg string, args ...any)
	// WarnContext logs at LevelWarn with the given context.
	WarnContext(ctx context.Context, msg string, args ...any)
	// ErrorContext logs at LevelError with the given context.
	ErrorContext(ctx context.Context, msg string, args ...any)

	// With returns a new Logger that includes the provided key-value attributes.
	With(args ...any) Logger

	// WithComponent returns a new Logger scoped to a specific subsystem (e.g. "wal", "memtable").
	WithComponent(name string) Logger
}

// defaultLogger implements Logger wrapping standard library log/slog.Logger.
type defaultLogger struct {
	inner *slog.Logger
}

func (l *defaultLogger) Debug(msg string, args ...any) {
	l.inner.Debug(msg, args...)
}

func (l *defaultLogger) Info(msg string, args ...any) {
	l.inner.Info(msg, args...)
}

func (l *defaultLogger) Warn(msg string, args ...any) {
	l.inner.Warn(msg, args...)
}

func (l *defaultLogger) Error(msg string, args ...any) {
	l.inner.Error(msg, args...)
}

func (l *defaultLogger) DebugContext(ctx context.Context, msg string, args ...any) {
	l.inner.DebugContext(ctx, msg, args...)
}

func (l *defaultLogger) InfoContext(ctx context.Context, msg string, args ...any) {
	l.inner.InfoContext(ctx, msg, args...)
}

func (l *defaultLogger) WarnContext(ctx context.Context, msg string, args ...any) {
	l.inner.WarnContext(ctx, msg, args...)
}

func (l *defaultLogger) ErrorContext(ctx context.Context, msg string, args ...any) {
	l.inner.ErrorContext(ctx, msg, args...)
}

func (l *defaultLogger) With(args ...any) Logger {
	return &defaultLogger{inner: l.inner.With(args...)}
}

func (l *defaultLogger) WithComponent(name string) Logger {
	return &defaultLogger{inner: l.inner.With(slog.String("component", name))}
}

// safeRedact extracts the safe representation of a Redactable instance.
// It defends against typed nil pointers and recovers from panics in custom Redact() implementations.
func safeRedact(r Redactable) (res any, ok bool) {
	if r == nil {
		return nil, true
	}
	val := reflect.ValueOf(r)
	switch val.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		if val.IsNil() {
			return nil, true
		}
	}

	defer func() {
		if rec := recover(); rec != nil {
			res = RedactedPlaceholder
			ok = true
		}
	}()

	return r.Redact(), true
}

// normalizeKey converts an attribute key into canonical snake_case.
// It trims whitespace, treats camelCase/PascalCase boundaries and common delimiters
// (-, ., :, /, \, [, ], (, ), {, }, spaces, etc.) as word boundaries, lowercases characters,
// and collapses repeated underscores.
func normalizeKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	n := len(runes)

	for i := 0; i < n; i++ {
		r := runes[i]
		switch r {
		case '-', '.', ':', '/', '\\', '[', ']', '(', ')', '{', '}', ' ', '\t', '_', '@', '#', '$', '%', '^', '&', '*', '=', '+', '|', '~', '`', '<', '>', '?', '!':
			b.WriteByte('_')
			continue
		}

		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
				b.WriteByte('_')
			} else if i > 0 && i+1 < n && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1]) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}

		b.WriteRune(unicode.ToLower(r))
	}

	raw := b.String()
	for strings.Contains(raw, "__") {
		raw = strings.ReplaceAll(raw, "__", "_")
	}
	return strings.Trim(raw, "_")
}

// isAuth checks if the canonical key contains "auth" as a discrete word segment.
// This matches "auth", "user_auth", "auth_token", etc., while avoiding false positives like
// "author", "authority", or "authenticate".
func isAuth(canonical string) bool {
	return canonical == "auth" ||
		strings.HasPrefix(canonical, "auth_") ||
		strings.HasSuffix(canonical, "_auth") ||
		strings.Contains(canonical, "_auth_")
}

// isSensitiveKey checks whether an attribute key matches exact or compound sensitive patterns.
func isSensitiveKey(key string, exactMap map[string]struct{}) bool {
	canonical := normalizeKey(key)
	if canonical == "" {
		return false
	}
	if _, exists := exactMap[canonical]; exists {
		return true
	}

	stripped := strings.ReplaceAll(canonical, "_", "")
	if _, exists := exactMap[stripped]; exists {
		return true
	}

	if strings.Contains(canonical, "password") ||
		strings.Contains(canonical, "passwd") ||
		strings.Contains(canonical, "pass_word") ||
		strings.Contains(stripped, "password") ||
		strings.Contains(stripped, "passwd") ||
		strings.Contains(canonical, "secret") ||
		strings.Contains(canonical, "credential") ||
		strings.Contains(canonical, "private_key") ||
		strings.Contains(canonical, "privatekey") ||
		strings.Contains(canonical, "api_key") ||
		strings.Contains(canonical, "apikey") ||
		strings.Contains(canonical, "authorization") ||
		strings.HasSuffix(canonical, "_token") ||
		canonical == "token" ||
		isAuth(canonical) {
		return true
	}

	return false
}

// makeReplaceAttr builds an attribute replacement function that masks sensitive keys
// and supports custom Redactable types.
//
// Security Precedence:
//  1. Empty attributes are preserved/dropped by slog.
//  2. Sensitive attribute keys ALWAYS take precedence over custom Redactable values.
//     If an attribute key is classified as sensitive, the entire value is immediately replaced
//     with RedactedPlaceholder without invoking Redact().
//  3. For non-sensitive keys, if the value implements Redactable, safeRedact is evaluated.
//  4. All other attributes remain unchanged.
func makeReplaceAttr(customRedacted []string) func(groups []string, a slog.Attr) slog.Attr {
	redactedMap := make(map[string]struct{}, len(defaultSensitiveKeys)+len(customRedacted))
	for k := range defaultSensitiveKeys {
		redactedMap[k] = struct{}{}
	}
	for _, k := range customRedacted {
		normalized := normalizeKey(k)
		if normalized != "" {
			redactedMap[normalized] = struct{}{}
		}
	}

	return func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == "" {
			return a
		}

		// Precedence 1: Sensitive attribute key always wins.
		// Immediately mask with RedactedPlaceholder without evaluating custom Redact() logic.
		if isSensitiveKey(a.Key, redactedMap) {
			return slog.String(a.Key, RedactedPlaceholder)
		}

		// Precedence 2: For non-sensitive keys, evaluate custom Redactable implementations.
		if r, ok := a.Value.Any().(Redactable); ok {
			if safeVal, safe := safeRedact(r); safe {
				return slog.Any(a.Key, safeVal)
			}
		}

		return a
	}
}

// New creates a new Logger instance configured according to the provided Config.
func New(cfg Config) Logger {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}

	opts := &slog.HandlerOptions{
		Level:       cfg.Level.toSlog(),
		AddSource:   cfg.AddSource,
		ReplaceAttr: makeReplaceAttr(cfg.RedactedKeys),
	}

	var handler slog.Handler
	switch cfg.Format {
	case FormatText:
		handler = slog.NewTextHandler(out, opts)
	case FormatJSON:
		fallthrough
	default:
		handler = slog.NewJSONHandler(out, opts)
	}

	return &defaultLogger{inner: slog.New(handler)}
}

// NewJSON creates a new Logger that outputs structured JSON to w at the given level.
func NewJSON(w io.Writer, level Level) Logger {
	return New(Config{
		Level:  level,
		Format: FormatJSON,
		Output: w,
	})
}

// NewText creates a new Logger that outputs human-readable key-value text to w at the given level.
func NewText(w io.Writer, level Level) Logger {
	return New(Config{
		Level:  level,
		Format: FormatText,
		Output: w,
	})
}

// nopHandler is an slog.Handler that drops all logs with zero allocations.
type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (n nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return n }
func (n nopHandler) WithGroup(string) slog.Handler           { return n }

// NewNop returns a Logger that discards all log records without formatting or allocation overhead.
// It is intended for testing and benchmarks where logging is suppressed.
func NewNop() Logger {
	return &defaultLogger{inner: slog.New(nopHandler{})}
}

// Err returns a slog.Attr for logging errors using the standard "error" key.
// If err is nil, it returns an empty slog.Attr which slog safely drops.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.Any("error", err)
}

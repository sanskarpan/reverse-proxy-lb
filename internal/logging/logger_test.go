package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// buildLogger creates a Logger that writes to buf instead of os.Stdout.
func buildLogger(level, format string, buf *bytes.Buffer) *Logger {
	lv := &slog.LevelVar{}
	lv.Set(parseSlogLevel(level))
	if format == "" {
		format = "json"
	}
	return &Logger{
		inner:  newInner(format, lv, buf),
		lv:     lv,
		format: format,
		output: buf,
	}
}

// ---------------------------------------------------------------------------
// parseSlogLevel
// ---------------------------------------------------------------------------

func TestParseSlogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"", slog.LevelInfo},
	}
	for _, tt := range tests {
		got := parseSlogLevel(tt.input)
		if got != tt.want {
			t.Errorf("parseSlogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// New / constructor
// ---------------------------------------------------------------------------

func TestNew(t *testing.T) {
	l := New("info", "json")
	if l == nil {
		t.Fatal("New returned nil")
	}
}

func TestNewDefaultFormat(t *testing.T) {
	// empty format should default to "json"
	l := New("info", "")
	if l.format != "json" {
		t.Errorf("default format = %q, want %q", l.format, "json")
	}
}

// ---------------------------------------------------------------------------
// SetLevel
// ---------------------------------------------------------------------------

func TestSetLevel(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("info", "json", &buf)

	// Should not panic.
	l.SetLevel("debug")
	l.SetLevel("warn")
	l.SetLevel("error")
	l.SetLevel("info")
}

// ---------------------------------------------------------------------------
// SetFormat
// ---------------------------------------------------------------------------

func TestSetFormatNoOp(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("info", "json", &buf)

	// Same format: no-op, pointer must remain the same.
	inner1 := l.inner
	l.SetFormat("json")
	if l.inner != inner1 {
		t.Error("SetFormat with same format should be a no-op")
	}
}

func TestSetFormatEmpty(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("info", "json", &buf)

	l.SetFormat("") // should be ignored
	if l.format != "json" {
		t.Errorf("SetFormat('') should not change format, got %q", l.format)
	}
}

func TestSetFormatText(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("info", "json", &buf)

	l.SetFormat("text")
	if l.format != "text" {
		t.Errorf("format = %q, want %q", l.format, "text")
	}
}

// ---------------------------------------------------------------------------
// Log methods – JSON output
// ---------------------------------------------------------------------------

func TestLoggerInfoJSON(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("debug", "json", &buf)

	l.Info("hello world", map[string]interface{}{"key": "value"})

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("expected log output, got empty")
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("failed to parse JSON log line %q: %v", line, err)
	}
	if m["msg"] != "hello world" {
		t.Errorf("msg = %v, want %q", m["msg"], "hello world")
	}
	if m["key"] != "value" {
		t.Errorf("key = %v, want %q", m["key"], "value")
	}
}

func TestLoggerDebugSuppressedAtInfo(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("info", "json", &buf)

	l.Debug("should not appear", nil)
	if buf.Len() > 0 {
		t.Errorf("Debug should be suppressed at info level, got: %q", buf.String())
	}
}

func TestLoggerAllLevels(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("debug", "json", &buf)

	l.Debug("debug msg", nil)
	l.Info("info msg", nil)
	l.Warn("warn msg", nil)
	l.Error("error msg", nil)

	out := buf.String()
	for _, want := range []string{"debug msg", "info msg", "warn msg", "error msg"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q; full output: %s", want, out)
		}
	}
}

func TestLoggerTextFormat(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("info", "text", &buf)

	l.Info("text-format message", map[string]interface{}{"k": 42})

	if !strings.Contains(buf.String(), "text-format message") {
		t.Errorf("text format output missing message; got: %q", buf.String())
	}
}

func TestLoggerWithFields(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("debug", "json", &buf)

	l.Info("msg with fields", map[string]interface{}{"user": "alice", "count": 3})

	out := buf.String()
	if !strings.Contains(out, "alice") {
		t.Errorf("expected field value 'alice' in output, got: %s", out)
	}
}

// ---------------------------------------------------------------------------
// fieldsToAttrs (internal helper)
// ---------------------------------------------------------------------------

func TestFieldsToAttrs(t *testing.T) {
	if got := fieldsToAttrs(nil); got != nil {
		t.Errorf("fieldsToAttrs(nil) = %v, want nil", got)
	}
	if got := fieldsToAttrs(map[string]interface{}{}); got != nil {
		t.Errorf("fieldsToAttrs({}) = %v, want nil", got)
	}

	attrs := fieldsToAttrs(map[string]interface{}{"a": 1, "b": "two"})
	if len(attrs) != 4 {
		t.Errorf("fieldsToAttrs: len = %d, want 4", len(attrs))
	}
}

// ---------------------------------------------------------------------------
// Package-level default logger functions
// ---------------------------------------------------------------------------

func TestDefaultLoggerFuncs(t *testing.T) {
	// These write to os.Stdout; we just ensure they don't panic.
	SetLevel("debug")
	Debug("debug pkg-level", nil)
	Info("info pkg-level", nil)
	Warn("warn pkg-level", nil)
	Error("error pkg-level", nil)
}

func TestConfigure(t *testing.T) {
	// Configure should not panic and should apply level+format changes.
	Configure("info", "json")
	Configure("debug", "text")
	Configure("warn", "json")
	// Restore default.
	Configure("info", "json")
}

// ---------------------------------------------------------------------------
// Hot-reload: SetLevel after construction
// ---------------------------------------------------------------------------

func TestSetLevelHotReload(t *testing.T) {
	var buf bytes.Buffer
	l := buildLogger("warn", "json", &buf)

	// Debug not emitted at warn level.
	l.Debug("should be hidden", nil)
	if buf.Len() > 0 {
		t.Errorf("debug should not emit at warn level: %s", buf.String())
	}

	// Hot-reload to debug.
	l.SetLevel("debug")
	l.Debug("now visible", nil)
	if !strings.Contains(buf.String(), "now visible") {
		t.Errorf("debug should emit after SetLevel(debug): %s", buf.String())
	}
}

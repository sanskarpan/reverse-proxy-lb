package logging

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

// Level mirrors the old string-typed constants so external packages that
// imported them continue to compile unchanged.
type Level = string

const (
	DebugLevel Level = "debug"
	InfoLevel  Level = "info"
	WarnLevel  Level = "warn"
	ErrorLevel Level = "error"
)

// Logger is a thin wrapper around *slog.Logger.  It uses a *slog.LevelVar for
// thread-safe level hot-reload (replaces the old sync.Mutex + custom level).
type Logger struct {
	mu     sync.RWMutex // guards inner and format (SetFormat may recreate inner)
	inner  *slog.Logger
	lv     *slog.LevelVar
	format string
	output io.Writer
}

// parseSlogLevel converts the legacy level strings to slog.Level.
func parseSlogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// newInner constructs a *slog.Logger for the given format and LevelVar.
func newInner(format string, lv *slog.LevelVar, out io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(out, opts)
	} else {
		h = slog.NewJSONHandler(out, opts)
	}
	return slog.New(h)
}

// New creates a Logger with the given level and format ("json" or "text").
// The signature is unchanged from the previous implementation (no error return).
func New(level, format string) *Logger {
	lv := &slog.LevelVar{}
	lv.Set(parseSlogLevel(level))
	if format == "" {
		format = "json"
	}
	out := os.Stdout
	return &Logger{
		inner:  newInner(format, lv, out),
		lv:     lv,
		format: format,
		output: out,
	}
}

// SetLevel updates the logging level; it is safe to call concurrently (hot-reload).
func (l *Logger) SetLevel(level string) {
	l.lv.Set(parseSlogLevel(level))
}

// SetFormat changes the output format ("json" or "text") and rebuilds the
// underlying handler.  It is safe to call concurrently.
func (l *Logger) SetFormat(format string) {
	if format == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == format {
		return
	}
	l.format = format
	l.inner = newInner(format, l.lv, l.output)
}

// fieldsToAttrs converts the legacy map[string]interface{} into slog.Attr
// key-value pairs so callers need no source changes.
func fieldsToAttrs(fields map[string]interface{}) []any {
	if len(fields) == 0 {
		return nil
	}
	attrs := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	return attrs
}

func (l *Logger) Debug(msg string, fields map[string]interface{}) {
	l.mu.RLock()
	inner := l.inner
	l.mu.RUnlock()
	inner.Debug(msg, fieldsToAttrs(fields)...)
}

func (l *Logger) Info(msg string, fields map[string]interface{}) {
	l.mu.RLock()
	inner := l.inner
	l.mu.RUnlock()
	inner.Info(msg, fieldsToAttrs(fields)...)
}

func (l *Logger) Warn(msg string, fields map[string]interface{}) {
	l.mu.RLock()
	inner := l.inner
	l.mu.RUnlock()
	inner.Warn(msg, fieldsToAttrs(fields)...)
}

func (l *Logger) Error(msg string, fields map[string]interface{}) {
	l.mu.RLock()
	inner := l.inner
	l.mu.RUnlock()
	inner.Error(msg, fieldsToAttrs(fields)...)
}

// ── package-level default logger ──────────────────────────────────────────────

var defaultLogger = New(InfoLevel, "json")

// Debug logs at debug level on the default logger.
func Debug(msg string, fields map[string]interface{}) {
	defaultLogger.Debug(msg, fields)
}

// Info logs at info level on the default logger.
func Info(msg string, fields map[string]interface{}) {
	defaultLogger.Info(msg, fields)
}

// Warn logs at warn level on the default logger.
func Warn(msg string, fields map[string]interface{}) {
	defaultLogger.Warn(msg, fields)
}

// Error logs at error level on the default logger.
func Error(msg string, fields map[string]interface{}) {
	defaultLogger.Error(msg, fields)
}

// SetLevel updates the default logger's level.
func SetLevel(level string) {
	defaultLogger.SetLevel(level)
}

// Configure applies level and format to the default logger.  Called at
// startup so that config.logging settings actually take effect.
func Configure(level, format string) {
	defaultLogger.SetLevel(level)
	defaultLogger.SetFormat(format)
}

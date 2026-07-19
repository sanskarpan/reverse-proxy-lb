package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Level string

const (
	DebugLevel Level = "debug"
	InfoLevel  Level = "info"
	WarnLevel  Level = "warn"
	ErrorLevel Level = "error"
)

type Logger struct {
	mu     sync.Mutex
	level  Level
	format string
	output io.Writer
}

type LogEntry struct {
	Timestamp string                 `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

var defaultLogger *Logger

func init() {
	defaultLogger = &Logger{
		level:  InfoLevel,
		format: "json",
		output: os.Stdout,
	}
}

func New(level, format string) *Logger {
	l := &Logger{
		level:  parseLevel(level),
		format: format,
		output: os.Stdout,
	}
	return l
}

func parseLevel(level string) Level {
	switch level {
	case "debug":
		return DebugLevel
	case "warn":
		return WarnLevel
	case "error":
		return ErrorLevel
	default:
		return InfoLevel
	}
}

func (l *Logger) SetLevel(level string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = parseLevel(level)
}

func (l *Logger) SetFormat(format string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if format != "" {
		l.format = format
	}
}

func (l *Logger) shouldLog(level Level) bool {
	switch l.level {
	case DebugLevel:
		return true
	case InfoLevel:
		return level != DebugLevel
	case WarnLevel:
		return level == WarnLevel || level == ErrorLevel
	case ErrorLevel:
		return level == ErrorLevel
	}
	return false
}

func (l *Logger) log(level Level, msg string, fields map[string]interface{}) {
	// Hold the mutex across the level check AND the write. l.level and l.format
	// are mutated by SetLevel/SetFormat (e.g. from a live config reload), so
	// reading them here without the lock races with a concurrent reload while
	// requests are being logged. shouldLog reads l.level, so it must run under
	// the lock too.
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.shouldLog(level) {
		return
	}

	entry := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     string(level),
		Message:   msg,
		Fields:    fields,
	}

	if l.format == "json" {
		data, _ := json.Marshal(entry)
		fmt.Fprintln(l.output, string(data))
	} else {
		fmt.Fprintf(l.output, "[%s] %s: %s\n", entry.Timestamp, entry.Level, entry.Message)
	}
}

func (l *Logger) Debug(msg string, fields map[string]interface{}) {
	l.log(DebugLevel, msg, fields)
}

func (l *Logger) Info(msg string, fields map[string]interface{}) {
	l.log(InfoLevel, msg, fields)
}

func (l *Logger) Warn(msg string, fields map[string]interface{}) {
	l.log(WarnLevel, msg, fields)
}

func (l *Logger) Error(msg string, fields map[string]interface{}) {
	l.log(ErrorLevel, msg, fields)
}

func Debug(msg string, fields map[string]interface{}) {
	defaultLogger.Debug(msg, fields)
}

func Info(msg string, fields map[string]interface{}) {
	defaultLogger.Info(msg, fields)
}

func Warn(msg string, fields map[string]interface{}) {
	defaultLogger.Warn(msg, fields)
}

func Error(msg string, fields map[string]interface{}) {
	defaultLogger.Error(msg, fields)
}

func SetLevel(level string) {
	defaultLogger.SetLevel(level)
}

// Configure applies the log level and format to the default logger. Called at
// startup so that config.logging settings actually take effect.
func Configure(level, format string) {
	defaultLogger.SetLevel(level)
	defaultLogger.SetFormat(format)
}

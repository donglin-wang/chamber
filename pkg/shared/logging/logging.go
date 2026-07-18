// Package logging owns Chamber SDK host-side logging defaults.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

const (
	DefaultLevel  = "info"
	DefaultFormat = "json"
)

type Config struct {
	Level  string       `json:"level,omitempty"`
	Format string       `json:"format,omitempty"`
	Logger *slog.Logger `json:"-"`
}

type Override struct {
	Level  *string `json:"level,omitempty"`
	Format *string `json:"format,omitempty"`
}

var (
	loggerMu sync.RWMutex
	logger   = newDefaultLogger()
)

// Logger returns the current Chamber SDK logger.
func Logger() *slog.Logger {
	loggerMu.RLock()
	defer loggerMu.RUnlock()
	return logger
}

// With returns the current Chamber SDK logger with additional attributes.
func With(args ...any) *slog.Logger {
	return Logger().With(args...)
}

// Debug logs a debug-level Chamber SDK event.
func Debug(ctx context.Context, msg string, args ...any) {
	Logger().DebugContext(contextOrBackground(ctx), msg, args...)
}

// Info logs an info-level Chamber SDK event.
func Info(ctx context.Context, msg string, args ...any) {
	Logger().InfoContext(contextOrBackground(ctx), msg, args...)
}

// Warn logs a warning-level Chamber SDK event.
func Warn(ctx context.Context, msg string, args ...any) {
	Logger().WarnContext(contextOrBackground(ctx), msg, args...)
}

// Error logs an error-level Chamber SDK event.
func Error(ctx context.Context, msg string, args ...any) {
	Logger().ErrorContext(contextOrBackground(ctx), msg, args...)
}

// DefaultConfig returns the default Chamber SDK logging behavior.
func DefaultConfig() Config {
	return Config{
		Level:  DefaultLevel,
		Format: DefaultFormat,
	}
}

// Resolve applies overrides and validates the resulting logging config.
func Resolve(defaultConfig Config, override Override) (Config, error) {
	if override.Level != nil {
		defaultConfig.Level = *override.Level
	}
	if override.Format != nil {
		defaultConfig.Format = *override.Format
	}

	if defaultConfig.Level == "" {
		defaultConfig.Level = DefaultLevel
	}
	if defaultConfig.Format == "" {
		defaultConfig.Format = DefaultFormat
	}
	if _, err := parseLevel(defaultConfig.Level); err != nil {
		return Config{}, err
	}
	switch normalizeFormat(defaultConfig.Format) {
	case "json", "text":
	default:
		return Config{}, fmt.Errorf("unsupported log format %q", defaultConfig.Format)
	}
	return defaultConfig, nil
}

// Configure applies a non-zero logging config to the process-wide Chamber SDK
// logger. A zero config inherits the current logger.
func Configure(config Config, w io.Writer) error {
	if config == (Config{}) {
		return nil
	}
	if config.Logger != nil {
		SetLogger(config.Logger)
		return nil
	}
	logger, err := NewLogger(w, config)
	if err != nil {
		return err
	}
	SetLogger(logger)
	return nil
}

// SetLogger replaces the Chamber SDK logger. Passing nil restores the default
// JSON logger on stderr.
func SetLogger(next *slog.Logger) {
	if next == nil {
		next = newDefaultLogger()
	}

	loggerMu.Lock()
	defer loggerMu.Unlock()
	logger = next
}

// NewJSONLogger creates a slog JSON logger suitable for Chamber SDK host logs.
func NewJSONLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	}))
}

// NewLogger creates a slog logger from Chamber logging config.
func NewLogger(w io.Writer, config Config) (*slog.Logger, error) {
	if config.Logger != nil {
		return config.Logger, nil
	}
	resolved, err := Resolve(DefaultConfig(), Override{
		Level:  stringPtr(config.Level),
		Format: stringPtr(config.Format),
	})
	if err != nil {
		return nil, err
	}
	if w == nil {
		w = os.Stderr
	}

	level, _ := parseLevel(resolved.Level)
	options := &slog.HandlerOptions{Level: level}
	switch normalizeFormat(resolved.Format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, options)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, options)), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", resolved.Format)
	}
}

func newDefaultLogger() *slog.Logger {
	return NewJSONLogger(os.Stderr, slog.LevelInfo)
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level %q", raw)
	}
}

func normalizeFormat(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

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

	chamberErrors "github.com/donglin-wang/chamber/pkg/shared/errors"
)

const (
	// DefaultLevel is the default minimum host-log level.
	DefaultLevel = "info"
	// DefaultFormat is the default host-log encoding.
	DefaultFormat = "json"
)

// Config controls Chamber SDK host-side logging.
type Config struct {
	// Level is the minimum log level: debug, info, warn, or error.
	Level string `json:"level,omitempty"`

	// Format is the log encoding: json or text.
	Format string `json:"format,omitempty"`

	// Logger, when non-nil, is used directly instead of constructing one from
	// Level and Format.
	Logger *slog.Logger `json:"-"`
}

// SlogLogger is an alias for slog.Logger for callers that want to refer to the
// concrete logger type through Chamber's logging package.
type SlogLogger = slog.Logger

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

// InfoWith logs an info-level Chamber SDK event with the supplied logger. A nil
// logger falls back to the current package logger.
func InfoWith(logger *slog.Logger, ctx context.Context, msg string, args ...any) {
	if logger == nil {
		logger = Logger()
	}
	logger.InfoContext(contextOrBackground(ctx), msg, args...)
}

// DefaultConfig returns the default Chamber SDK logging behavior.
func DefaultConfig() Config {
	return Config{
		Level:  DefaultLevel,
		Format: DefaultFormat,
	}
}

func normalized(config Config) (Config, error) {
	if config.Level == "" {
		config.Level = DefaultLevel
	}
	if config.Format == "" {
		config.Format = DefaultFormat
	}
	if _, err := parseLevel(config.Level); err != nil {
		return Config{}, err
	}
	switch normalizeFormat(config.Format) {
	case "json", "text":
	default:
		return Config{}, fmt.Errorf("%w: unsupported log format %q", chamberErrors.ErrInvalidRequest, config.Format)
	}
	return config, nil
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

// LoggerFromConfig returns the logger an SDK component should use without
// replacing the process-wide Chamber logger. A zero config inherits the current
// package logger.
func LoggerFromConfig(config Config, w io.Writer) (*slog.Logger, error) {
	if config == (Config{}) {
		return Logger(), nil
	}
	return NewLogger(w, config)
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
	config, err := normalized(config)
	if err != nil {
		return nil, err
	}
	if w == nil {
		w = os.Stderr
	}

	level, _ := parseLevel(config.Level)
	options := &slog.HandlerOptions{Level: level}
	switch normalizeFormat(config.Format) {
	case "json":
		return slog.New(slog.NewJSONHandler(w, options)), nil
	case "text":
		return slog.New(slog.NewTextHandler(w, options)), nil
	default:
		return nil, fmt.Errorf("%w: unsupported log format %q", chamberErrors.ErrInvalidRequest, config.Format)
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
		return slog.LevelInfo, fmt.Errorf("%w: unsupported log level %q", chamberErrors.ErrInvalidRequest, raw)
	}
}

func normalizeFormat(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

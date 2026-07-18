package directory

import (
	"context"
	"log/slog"
	"sync"

	apexlog "github.com/apex/log"
	chamberLogging "github.com/donglin-wang/chamber/pkg/shared/logging"
)

var installApexBridgeOnce sync.Once

func installApexBridge() {
	installApexBridgeOnce.Do(func() {
		apexlog.SetLevel(apexlog.DebugLevel)
		apexlog.SetHandler(apexHandler{})
	})
}

type apexHandler struct{}

func (apexHandler) HandleLog(entry *apexlog.Entry) error {
	if entry == nil {
		return nil
	}

	ctx := context.Background()
	level := slogLevel(entry.Level)
	logger := chamberLogging.Logger().With("library", "umoci")
	handler := logger.Handler()
	if !handler.Enabled(ctx, level) {
		return nil
	}

	record := slog.NewRecord(entry.Timestamp, level, entry.Message, 0)
	for _, name := range entry.Fields.Names() {
		record.AddAttrs(slog.Any(name, entry.Fields.Get(name)))
	}
	return handler.Handle(ctx, record)
}

func slogLevel(level apexlog.Level) slog.Level {
	switch level {
	case apexlog.DebugLevel:
		return slog.LevelDebug
	case apexlog.InfoLevel:
		return slog.LevelInfo
	case apexlog.WarnLevel:
		return slog.LevelWarn
	case apexlog.ErrorLevel, apexlog.FatalLevel:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

package config

import (
	"log/slog"
	"os"
)

// NewLogger returns a structured JSON logger writing to stderr.
// Level can be tightened via LOG_LEVEL=debug|info|warn|error.
func NewLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}

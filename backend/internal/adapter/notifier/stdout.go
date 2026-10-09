// Package notifier holds concrete port.Notifier implementations.
//
// stdout.go is the MVP notifier — it writes events through the structured
// slog logger configured on the bot.
package notifier

import (
	"context"
	"log/slog"

	"fx-bot/backend/internal/port"
)

// Stdout writes events through slog. Title becomes the message and Meta
// flattens into structured fields.
type Stdout struct {
	Logger *slog.Logger
}

func NewStdout(logger *slog.Logger) *Stdout {
	if logger == nil {
		logger = slog.Default()
	}
	return &Stdout{Logger: logger}
}

func (s *Stdout) Notify(_ context.Context, e port.Event) error {
	attrs := []any{"title", e.Title, "body", e.Body}
	for k, v := range e.Meta {
		attrs = append(attrs, k, v)
	}
	switch e.Level {
	case port.LevelError:
		s.Logger.Error("notify", attrs...)
	case port.LevelWarn:
		s.Logger.Warn("notify", attrs...)
	default:
		s.Logger.Info("notify", attrs...)
	}
	return nil
}

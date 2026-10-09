package port

import "context"

// Level enumerates notification severity.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Event is one notification payload. Body is plain text; Meta gives
// structured fields (e.g. config_id, position_id) for downstream filtering.
type Event struct {
	Level Level
	Title string
	Body  string
	Meta  map[string]any
}

// Notifier emits notifications to an external sink (Slack, stdout, etc).
// MVP uses the stdout implementation; Slack would be a separate adapter.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

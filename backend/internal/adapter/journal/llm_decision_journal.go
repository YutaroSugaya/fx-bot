// Package journal persists the autonomous LLM trade loop's per-cycle decisions as an append-only
// JSONL history under the runtime dir. File-backed (not DB) on purpose, mirroring the playbook
// store: it is observability/audit data, not authoritative trade/position data, so it stays off
// the DB write surface while still giving an immutable, restart-surviving "一目で見る" trail the
// operator (and a later investigation) can read to spot a slow drawdown or a silent regression.
package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fx-bot/backend/internal/port"
)

// FileJournal appends one JSON line per entry to a SINGLE file (all symbols interleaved
// chronologically, so the whole loop reads top-to-bottom in time order). The mutex serialises
// concurrent writes from the parallel pairs (the loop runs up to llmDecisionMaxParallel cycles at
// once); writing each line in one O_APPEND write() is the second guard against interleaving.
type FileJournal struct {
	Path string
	mu   sync.Mutex
}

// Record appends e as a JSONL line, creating the dir if needed and stamping Time when the caller
// left it zero (the adapter parse-fallback path has no clock). Errors are returned for the caller
// to log; they must never abort a decision cycle.
func (j *FileJournal) Record(e port.LLMDecisionLogEntry) error {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal llm decision entry: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(j.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir journal dir: %w", err)
	}
	f, err := os.OpenFile(j.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open journal file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append llm decision entry: %w", err)
	}
	return nil
}

var _ port.LLMDecisionJournal = (*FileJournal)(nil)

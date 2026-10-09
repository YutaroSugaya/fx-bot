// Package playbook persists the LLM trading "playbook" as an append-only JSONL history under the
// runtime dir (one line per version). File-backed (not DB) on purpose: the playbook is advisory LLM
// text, not trade/position data, so it stays out of the authoritative DB and the loop-writes-DB
// surface; the append-only log still gives an immutable audit trail and survives restart.
package playbook

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Version is one immutable playbook revision.
type Version struct {
	Symbol    string    `json:"symbol"`
	Rules     string    `json:"rules"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// FileStore reads/appends versions at <Dir>/playbook_<symbol>.jsonl.
type FileStore struct {
	Dir string
}

func (s *FileStore) path(symbol string) string {
	return filepath.Join(s.Dir, "playbook_"+strings.ReplaceAll(symbol, "/", "_")+".jsonl")
}

// Append writes a new immutable version line. Creates the dir/file as needed.
func (s *FileStore) Append(v Version) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("mkdir playbook dir: %w", err)
	}
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal playbook version: %w", err)
	}
	f, err := os.OpenFile(s.path(v.Symbol), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open playbook file: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append playbook version: %w", err)
	}
	return nil
}

// Latest returns the most recent version for symbol. ok=false when no history exists yet (the
// decision cycle then runs with an empty playbook — safe, the deterministic guards still apply).
func (s *FileStore) Latest(symbol string) (Version, bool, error) {
	f, err := os.Open(s.path(symbol))
	if err != nil {
		if os.IsNotExist(err) {
			return Version{}, false, nil
		}
		return Version{}, false, fmt.Errorf("open playbook file: %w", err)
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // allow long rules blocks
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			last = t
		}
	}
	if err := sc.Err(); err != nil {
		return Version{}, false, fmt.Errorf("scan playbook file: %w", err)
	}
	if last == "" {
		return Version{}, false, nil
	}
	var v Version
	if err := json.Unmarshal([]byte(last), &v); err != nil {
		return Version{}, false, fmt.Errorf("unmarshal playbook version: %w", err)
	}
	return v, true, nil
}

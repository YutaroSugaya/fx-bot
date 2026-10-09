package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/port"
)

func readJournal(t *testing.T, path string) []port.LLMDecisionLogEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()
	var out []port.LLMDecisionLogEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // raw_stdout excerpts can be long
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e port.LLMDecisionLogEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line is not valid JSON: %v (%q)", err, sc.Text())
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan journal: %v", err)
	}
	return out
}

// Record appends one JSON line per entry, creating the (possibly missing) dir, and stamps Time
// when the caller leaves it zero (the adapter parse-fallback path does — it has no clock).
func TestFileJournal_Record_AppendsJSONLAndCreatesDir(t *testing.T) {
	// Path deliberately points into a not-yet-existing subdir to prove Record mkdirs it.
	p := filepath.Join(t.TempDir(), "logs", "llm_decisions.jsonl")
	j := &FileJournal{Path: p}

	at := time.Date(2026, 6, 23, 1, 0, 0, 0, time.UTC)
	if err := j.Record(port.LLMDecisionLogEntry{
		Time: at, Symbol: "USD_JPY", Event: "cycle", Stage: "no_trade", Reason: "range_unclear",
	}); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := j.Record(port.LLMDecisionLogEntry{
		Symbol: "EUR_JPY", Event: "cycle", Stage: "submitted", Go: true, Side: "BUY", TPPips: 30, SLPips: 15,
	}); err != nil {
		t.Fatalf("record 2: %v", err)
	}

	lines := readJournal(t, p)
	if len(lines) != 2 {
		t.Fatalf("want 2 appended lines, got %d", len(lines))
	}
	if lines[0].Symbol != "USD_JPY" || lines[0].Stage != "no_trade" ||
		lines[0].Reason != "range_unclear" || !lines[0].Time.Equal(at) {
		t.Errorf("entry0 wrong: %+v", lines[0])
	}
	if lines[1].Symbol != "EUR_JPY" || lines[1].Side != "BUY" || lines[1].TPPips != 30 || lines[1].Time.IsZero() {
		t.Errorf("entry1 wrong (Time must be stamped when left zero): %+v", lines[1])
	}
}

// The decision loop runs up to llmDecisionMaxParallel pairs at once, so Record must be safe under
// concurrency: every line lands intact (no interleaving / no lost writes).
func TestFileJournal_Record_ConcurrentWritesAllLandIntact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "llm_decisions.jsonl")
	j := &FileJournal{Path: p}

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = j.Record(port.LLMDecisionLogEntry{Symbol: "USD_JPY", Event: "cycle", Stage: "no_trade"})
		}()
	}
	wg.Wait()

	lines := readJournal(t, p) // unmarshal-per-line fails the test if any line was corrupted
	if len(lines) != n {
		t.Fatalf("want %d intact JSONL lines, got %d", n, len(lines))
	}
}

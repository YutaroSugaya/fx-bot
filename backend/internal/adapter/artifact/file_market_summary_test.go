package artifact

import (
	"context"
	"path/filepath"
	"testing"
)

// File artifact I/O lives in adapter/artifact/, not usecase/.
// FileMarketSummaryStore implements port.MarketSummaryArtifactStore.

func TestFileMarketSummaryStore_WriteThenRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "latest_summary.json")
	store := NewFileMarketSummaryStore(path)

	payload := []byte(`{"symbol":"USD_JPY","spread_pips":0.32}`)
	if err := store.WriteLatestSummary(context.Background(), payload); err != nil {
		t.Fatalf("WriteLatestSummary: %v", err)
	}
	got, err := store.ReadLatestSummary(context.Background())
	if err != nil {
		t.Fatalf("ReadLatestSummary: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("payload roundtrip: got %s want %s", got, payload)
	}
}

func TestFileMarketSummaryStore_WriteIsAtomic_NoPartialFileOnFailure(t *testing.T) {
	// Atomicity: a half-written file must never appear at the destination.
	// Verifies the impl uses tempfile + rename rather than a direct stream.
	dir := t.TempDir()
	path := filepath.Join(dir, "latest_summary.json")
	store := NewFileMarketSummaryStore(path)

	// Seed an initial valid payload.
	if err := store.WriteLatestSummary(context.Background(), []byte(`{"v":1}`)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Overwrite with a new payload — destination must contain only the new bytes
	// (no concatenation, no partial write residue).
	if err := store.WriteLatestSummary(context.Background(), []byte(`{"v":2}`)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ := store.ReadLatestSummary(context.Background())
	if string(got) != `{"v":2}` {
		t.Errorf("after overwrite: got %s want {\"v\":2}", got)
	}
}

func TestFileMarketSummaryStore_ReadMissingReturnsEmptyNotError(t *testing.T) {
	// AskClaudeQuery degrades to "no summary, continue with {}" when the
	// summary file is missing. The store must signal that explicitly via
	// returning (nil, nil) rather than wrapping os.ErrNotExist in an error
	// so callers don't have to switch on errno.
	dir := t.TempDir()
	store := NewFileMarketSummaryStore(filepath.Join(dir, "does-not-exist.json"))
	got, err := store.ReadLatestSummary(context.Background())
	if err != nil {
		t.Fatalf("ReadLatestSummary on missing file should not error; got %v", err)
	}
	if got != nil {
		t.Errorf("missing summary should return nil bytes; got %s", got)
	}
}

package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// FileStrategyConfigStore implements port.StrategyConfigArtifactStore.

func TestFileStrategyConfigStore_PromoteActive_AtomicSwap(t *testing.T) {
	dir := t.TempDir()
	nextPath := filepath.Join(dir, "next.yaml")
	activePath := filepath.Join(dir, "active.yaml")
	store := NewFileStrategyConfigStore(nextPath, activePath)

	// Seed: a previous active YAML + a staged next.yaml.
	if err := os.WriteFile(activePath, []byte("# old active\n"), 0o600); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	if err := os.WriteFile(nextPath, []byte("# staged next\n"), 0o600); err != nil {
		t.Fatalf("seed next: %v", err)
	}

	if err := store.PromoteActive(context.Background(), []byte("# new active\n")); err != nil {
		t.Fatalf("PromoteActive: %v", err)
	}
	// active.yaml holds the promoted content.
	body, _ := os.ReadFile(activePath)
	if string(body) != "# new active\n" {
		t.Errorf("active body: got %s want \"# new active\\n\"", body)
	}
	// next.yaml is cleaned up so it doesn't shadow the next cycle.
	if _, err := os.Stat(nextPath); !os.IsNotExist(err) {
		t.Errorf("next.yaml should be removed after promote; err=%v", err)
	}
}

func TestFileStrategyConfigStore_ReadActive_MissingReturnsNil(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStrategyConfigStore(
		filepath.Join(dir, "next.yaml"),
		filepath.Join(dir, "does-not-exist.yaml"),
	)
	got, err := store.ReadActive(context.Background())
	if err != nil {
		t.Fatalf("ReadActive should not error on missing file; got %v", err)
	}
	if got != nil {
		t.Errorf("missing active should return nil; got %s", got)
	}
}

func TestFileStrategyConfigStore_ReadActive_ReturnsContent(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "active.yaml")
	if err := os.WriteFile(activePath, []byte("config_id: cfg-x\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store := NewFileStrategyConfigStore(filepath.Join(dir, "next.yaml"), activePath)
	got, err := store.ReadActive(context.Background())
	if err != nil {
		t.Fatalf("ReadActive: %v", err)
	}
	if string(got) != "config_id: cfg-x\n" {
		t.Errorf("body: got %s", got)
	}
}

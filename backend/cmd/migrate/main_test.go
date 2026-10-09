package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadMigrations_OrdersByVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0002_b.up.sql", "select 2;")
	writeFile(t, dir, "0002_b.down.sql", "select -2;")
	writeFile(t, dir, "0001_a.up.sql", "select 1;")
	writeFile(t, dir, "0001_a.down.sql", "select -1;")

	migs, err := loadMigrations(dir)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) != 2 {
		t.Fatalf("expected 2, got %d", len(migs))
	}
	if migs[0].Version != 1 || migs[1].Version != 2 {
		t.Errorf("order: %+v", migs)
	}
	if migs[0].Name != "a" || migs[1].Name != "b" {
		t.Errorf("name: %+v", migs)
	}
}

func TestLoadMigrations_OrphanUp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0001_a.up.sql", "select 1;")
	// missing down
	_, err := loadMigrations(dir)
	if err == nil || !strings.Contains(err.Error(), "missing down") {
		t.Fatalf("expected missing down error, got: %v", err)
	}
}

func TestLoadMigrations_OrphanDown(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0001_a.down.sql", "select -1;")
	// missing up
	_, err := loadMigrations(dir)
	if err == nil || !strings.Contains(err.Error(), "missing up") {
		t.Fatalf("expected missing up error, got: %v", err)
	}
}

func TestLoadMigrations_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "0001_a.up.sql", "select 1;")
	writeFile(t, dir, "0001_a.down.sql", "select -1;")
	writeFile(t, dir, "README.md", "ignore me")
	writeFile(t, dir, "no_version.sql", "ignore me too")
	migs, err := loadMigrations(dir)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migs) != 1 {
		t.Errorf("expected 1, got %d", len(migs))
	}
}

func TestLoadMigrations_MissingDirectory(t *testing.T) {
	_, err := loadMigrations(t.TempDir() + "/nope")
	if err == nil || !strings.Contains(err.Error(), "read migrations dir") {
		t.Fatalf("expected read error, got: %v", err)
	}
}

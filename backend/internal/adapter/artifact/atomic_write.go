package artifact

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes raw to path via a same-dir tempfile + os.Rename so a
// consumer (e.g. the claude CLI reading the summary, or the bot reloading
// active.yaml) never observes a half-written file — the rename is atomic on
// POSIX. The parent directory is created if missing. Shared by the artifact
// stores so the tempfile + rename ritual lives in one place.
func writeFileAtomic(path string, raw []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", name, path, err)
	}
	return nil
}

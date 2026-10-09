package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// FileStrategyConfigStore implements port.StrategyConfigArtifactStore as a
// file-pair (active.yaml + next.yaml staging). PromoteActive uses
// tempfile + os.Rename so consumers never see a half-written active.yaml,
// then best-effort removes the next.yaml staging file.
type FileStrategyConfigStore struct {
	nextPath   string
	activePath string
}

// NewFileStrategyConfigStore constructs a store wired to the two paths.
// activePath is mandatory; nextPath is optional and only used by PromoteActive
// to clean up the staging file. When nextPath is empty, the promote step
// just writes active.yaml.
func NewFileStrategyConfigStore(nextPath, activePath string) *FileStrategyConfigStore {
	return &FileStrategyConfigStore{nextPath: nextPath, activePath: activePath}
}

// PromoteActive writes raw atomically to activePath and removes nextPath
// (best-effort). Tempfile lives in the same dir as activePath so the
// rename is atomic on POSIX.
func (s *FileStrategyConfigStore) PromoteActive(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.activePath == "" {
		return errors.New("file_strategy_config: active path not configured")
	}
	if err := writeFileAtomic(s.activePath, raw); err != nil {
		return err
	}
	if s.nextPath != "" && s.nextPath != s.activePath {
		_ = os.Remove(s.nextPath) // best-effort staging cleanup
	}
	return nil
}

// ReadActive returns the current on-disk active YAML, or (nil, nil) when
// no artifact exists.
func (s *FileStrategyConfigStore) ReadActive(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.activePath == "" {
		return nil, errors.New("file_strategy_config: active path not configured")
	}
	body, err := os.ReadFile(s.activePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.activePath, err)
	}
	return body, nil
}

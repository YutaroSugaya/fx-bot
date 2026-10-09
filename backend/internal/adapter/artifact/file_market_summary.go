// Package artifact holds adapters that persist runtime artifacts —
// MarketSummary snapshots, strategy config YAML — as files (or future
// blob/object storage). Distinct from adapter/repository, which is
// DB-backed; artifacts are file-level streams that may be consumed by
// other processes (e.g. claude CLI reads the summary JSON).
package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// FileMarketSummaryStore implements port.MarketSummaryArtifactStore by
// writing/reading a single JSON-ish blob at a fixed path. Write uses
// tempfile + os.Rename so consumers never see a half-written file.
type FileMarketSummaryStore struct {
	path string
}

// NewFileMarketSummaryStore constructs a store rooted at path. Empty path
// is allowed at construction time but every method will error — the
// wiring layer is expected to set a non-empty path before publishing.
func NewFileMarketSummaryStore(path string) *FileMarketSummaryStore {
	return &FileMarketSummaryStore{path: path}
}

// WriteLatestSummary writes raw atomically to path.
// 入口で ctx.Err() を確認: 呼出側が既に cancel した状態で重い I/O を
// 走らせない。
func (s *FileMarketSummaryStore) WriteLatestSummary(ctx context.Context, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.path == "" {
		return errors.New("file_market_summary: path not configured")
	}
	return writeFileAtomic(s.path, raw)
}

// ReadLatestSummary reads the artifact. Missing file → (nil, nil) so the
// caller can degrade gracefully (e.g. AskClaudeQuery treats it as "{}").
func (s *FileMarketSummaryStore) ReadLatestSummary(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.path == "" {
		return nil, errors.New("file_market_summary: path not configured")
	}
	body, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	return body, nil
}

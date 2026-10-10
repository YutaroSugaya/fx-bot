package query

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"fx-bot/backend/internal/adapter/artifact"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAskClaudeQuery(t *testing.T) {
	// tests use the file artifact adapter (not direct os.WriteFile)
	// so they exercise the same boundary the wiring layer does.
	dir := t.TempDir()
	seededStore := artifact.NewFileMarketSummaryStore(dir + "/summary.json")
	_ = seededStore.WriteLatestSummary(context.Background(),
		[]byte(`{"symbol":"USD_JPY","spread_pips":0.3}`))
	missingStore := artifact.NewFileMarketSummaryStore(dir + "/does-not-exist.json")

	cases := []struct {
		name              string
		summaryStoreKey   string // "seeded" | "missing"
		runner            PromptRunner
		question          string
		wantErr           bool
		wantErrPart       string
		wantAnswer        string
		wantPromptHas     string
		wantPromptHasJSON string
	}{
		{
			name:              "happy path builds prompt with summary and question",
			summaryStoreKey:   "seeded",
			runner:            nil, // injected below to capture prompt
			question:          "現在のトレンドは？",
			wantAnswer:        "回答: 上昇トレンドです",
			wantPromptHas:     "現在のトレンドは？",
			wantPromptHasJSON: `"symbol":"USD_JPY"`,
		},
		{
			name:              "missing summary falls back to empty JSON",
			summaryStoreKey:   "missing",
			runner:            nil,
			question:          "test",
			wantAnswer:        "ok",
			wantPromptHasJSON: "```json\n{}\n```",
		},
		{
			name:            "runner error propagates",
			summaryStoreKey: "seeded",
			runner: func(context.Context, string) (string, error) {
				return "", errors.New("cli crashed")
			},
			question:    "test",
			wantErr:     true,
			wantErrPart: "cli crashed",
		},
		{
			name:            "nil runner returns error",
			summaryStoreKey: "seeded",
			runner:          nil, // intentional sentinel: caller sets nil in test
			question:        "test",
			wantErr:         true,
			wantErrPart:     "PromptRunner is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPrompt string
			runner := tc.runner
			// 第 1/2 ケース: prompt 検査のため stub runner を注入
			if tc.name == "happy path builds prompt with summary and question" ||
				tc.name == "missing summary falls back to empty JSON" {
				runner = func(ctx context.Context, p string) (string, error) {
					gotPrompt = p
					return tc.wantAnswer, nil
				}
			}
			// 第 4 ケース (nil runner): 明示的に nil をセット
			if tc.name == "nil runner returns error" {
				runner = nil
			}

			var store *artifact.FileMarketSummaryStore
			switch tc.summaryStoreKey {
			case "seeded":
				store = seededStore
			case "missing":
				store = missingStore
			}
			q := &AskClaudeQuery{SummaryStore: store, Runner: runner, Logger: quiet()}
			out, err := q.Execute(context.Background(), AskClaudeInput{Question: tc.question})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				if tc.wantErrPart != "" && !strings.Contains(err.Error(), tc.wantErrPart) {
					t.Errorf("error %q should contain %q", err.Error(), tc.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.wantAnswer != "" && out.Answer != tc.wantAnswer {
				t.Errorf("answer: got %q want %q", out.Answer, tc.wantAnswer)
			}
			if tc.wantPromptHas != "" && !strings.Contains(gotPrompt, tc.wantPromptHas) {
				t.Errorf("prompt should contain %q; got %q", tc.wantPromptHas, gotPrompt)
			}
			if tc.wantPromptHasJSON != "" && !strings.Contains(gotPrompt, tc.wantPromptHasJSON) {
				t.Errorf("prompt should contain %q; got %q", tc.wantPromptHasJSON, gotPrompt)
			}
		})
	}
}

// buildAskPrompt は純粋関数なので独立 (table 化候補なし)。
func TestBuildAskPrompt_IncludesBothPartsInOrder(t *testing.T) {
	p := buildAskPrompt(`{"a":1}`, "what?")
	sumIdx := strings.Index(p, "市場サマリー")
	qIdx := strings.Index(p, "ユーザの質問")
	if sumIdx < 0 || qIdx < 0 {
		t.Fatalf("prompt missing required sections: %q", p)
	}
	if sumIdx > qIdx {
		t.Errorf("market summary should come before user question; got summary@%d, question@%d", sumIdx, qIdx)
	}
}

package query

import (
	"context"
	"fmt"
	"log/slog"

	"fx-bot/backend/internal/port"
)

// AskClaudeInput は AskClaudeQuery.Execute の引数。
type AskClaudeInput struct {
	Question string
}

// AskClaudeOutput は AskClaudeQuery.Execute の戻り値。
type AskClaudeOutput struct {
	Answer string
}

// PromptRunner はプロンプト文字列を入力として外部 CLI (claude 等) を実行し
// stdout を返す関数型。実装は CLI を呼ぶアダプタ側で注入する。テストでは
// stub に差し替えて I/O を排除する。
type PromptRunner func(ctx context.Context, prompt string) (string, error)

// AskClaudeQuery は「市場サマリー JSON を添えてユーザの自由質問を Claude に
// 投げ、回答を返す」 read-only な usecase (CQRS Query)。
//
// 状態変更しない:
//   - DB 書き込みなし
//   - broker 呼び出しなし
//   - 市場サマリー JSON ファイルを read するのみ
//
// 設計判断:
//   - exec.Command を直接呼ばず PromptRunner 経由で抽象化。テスト時は in-memory
//     な stub を渡し、本番は cmd/bot/main.go が claude CLI 起動関数を渡す。
type AskClaudeQuery struct {
	// SummaryStore is the artifact store that holds the latest market
	// summary JSON. A1 fix: replaces SummaryPath (direct os.ReadFile)
	// — usecase no longer does file I/O. nil → Execute degrades to
	// "no summary, continue with {}".
	SummaryStore port.MarketSummaryArtifactStore
	Runner       PromptRunner // 必須
	Logger       *slog.Logger
}

// Execute はユーザ質問 + 現在の市場サマリー JSON を 1 プロンプトに組み立てて
// Runner に渡し、返ってきた文字列を Answer として返す。
func (q *AskClaudeQuery) Execute(ctx context.Context, in AskClaudeInput) (AskClaudeOutput, error) {
	if q.Runner == nil {
		return AskClaudeOutput{}, fmt.Errorf("ask_claude: PromptRunner is required")
	}

	var summaryJSON []byte
	if q.SummaryStore != nil {
		body, err := q.SummaryStore.ReadLatestSummary(ctx)
		if err == nil && body != nil {
			summaryJSON = body
		}
		// Read errors or missing artifact → fall through to {} default below.
	}
	if len(summaryJSON) == 0 {
		// 存在しないとき (= bot 起動直後など) は空 JSON で続行。
		summaryJSON = []byte("{}")
	}

	prompt := buildAskPrompt(string(summaryJSON), in.Question)

	answer, err := q.Runner(ctx, prompt)
	if err != nil {
		return AskClaudeOutput{}, fmt.Errorf("runner: %w", err)
	}
	return AskClaudeOutput{Answer: answer}, nil
}

// buildAskPrompt は「市場サマリー JSON + ユーザの自由質問」を 1 プロンプト
// 文字列に組み立てる。Claude 用のフォーマットは usecase 内に閉じ込めて
// runner 側 (= adapter) は形式を知らない。
func buildAskPrompt(summaryJSON, question string) string {
	return "あなたは FX トレードのアシスタントです。\n" +
		"以下の「現在の市場サマリー JSON」を参考に、ユーザの質問に日本語で答えてください。\n\n" +
		"## 現在の市場サマリー\n```json\n" + summaryJSON + "\n```\n\n" +
		"## ユーザの質問\n" + question + "\n"
}

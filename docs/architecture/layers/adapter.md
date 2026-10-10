# Layer: Adapter

## 役割 (1 行)

`port` interface の **具体実装**。Postgres / GMO REST / Claude CLI / Slack 等の外部システムとのアダプタ。

---

## やること (do)

- `port.*Repository` を Postgres + `pgxpool` で実装する
- `port.Broker` を GMO REST + httptest-replay で実装する (Live と Paper の両方)
- `port.Advisor` を Claude CLI (subprocess) で実装する
- `port.Notifier` を実装する (現在は Stdout のみ。外部通知は adapter を追加して実装する)
- 外部システムの DTO ↔ port record の変換を **adapter 内側** で完結させる
- DB record ↔ domain entity の変換は repository adapter が担当する

---

## やらないこと (don't)

- ❌ ビジネス判定をする (TP/SL 計算、risk gate 判定など)
- ❌ usecase / handler / app パッケージを import する
- ❌ adapter 同士で直接呼び出す (= usecase 経由で合成する)
- ❌ port を経由せず concrete adapter を返すコンストラクタを書く (`func New(...) *Repo` ではなく `func New(...) port.Repository` の方向で wiring に渡す)

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Repository Impl | `backend/internal/adapter/repository/` | `<aggregate>_repo.go` | `position_repo.go` |
| Broker Impl | `backend/internal/adapter/broker/` | `<vendor>_broker.go` or `<vendor>_fx.go` | `gmo_fx.go`, `paper_broker.go` |
| Notifier Impl | `backend/internal/adapter/notifier/` | `<vendor>.go` | `stdout.go` |
| Advisor Impl | `backend/internal/adapter/advisor/` | `<vendor>.go` | `claude_cli.go` |
| Artifact Impl | `backend/internal/adapter/artifact/` | `<medium>_<artifact>.go` | `file_market_summary.go`, `file_strategy_config.go` |
| Journal Impl | `backend/internal/adapter/journal/` | `<subject>_journal.go` | `llm_decision_journal.go` (append-only JSONL 観測ログ) |
| Data ingest (offline) | `backend/internal/adapter/histdata/` | `histdata.go` / `symbol.go` | HistData M1 CSV → `port.CandleRecord` |

### artifact subdir の責務

`adapter/artifact/` は **file / blob / object storage に書く副成果物** (= summary JSON / active YAML 等) の port 実装を集める。repository (DB) と broker (外部 API) の中間カテゴリ。

- `port.MarketSummaryArtifactStore` を `FileMarketSummaryStore` で実装 (`runtime/ai_input/latest_summary.json` への tempfile + rename atomic write)
- `port.StrategyConfigArtifactStore` を `FileStrategyConfigStore` で実装 (`configs/strategy_config.{active,next}.yaml` の human-readable outbox)
- usecase / query から直接 `os.ReadFile` / `os.WriteFile` / `os.Rename` を呼ばない方針の受け皿
- 真の SoT は DB (`strategy_configs.raw_yaml`)。YAML / JSON file は best-effort outbox

---

## テスト方法 (この層特有)

### Repository adapter

- **integration test**: 実 Postgres コンテナを使う (`//go:build integration` tag)
- `INTEGRATION_TEST_DB_URL` 環境変数で test DB に繋ぐ
- 通常の `make test` ではスキップ ([TESTING.md](../../workflows/TESTING.md) 参照)

```go
//go:build integration

func TestPositionCloser_AtomicTxRollsBackOnInsertError(t *testing.T) {
    dsn := os.Getenv("INTEGRATION_TEST_DB_URL")
    if dsn == "" { t.Skip(...) }
    // open real pool, exec tests
}
```

### Broker adapter (GMO)

- **録画再生 (httptest server)**: 実 API への HTTP リクエストを `httptest.NewServer` で fake し、fixture response を返す
- 失敗 case (timeout / 5xx / partial response) も再現可能

### Notifier / Advisor

- Notifier: stdout は実行して文字列を assert。外部通知の adapter を足すなら HTTP fake で検証する
- Advisor (Claude CLI): `PromptRunner func(ctx, prompt) (yaml, err)` を関数注入し、test fake で yaml を返す

詳細は [TESTING.md](../../workflows/TESTING.md)。

---

## 既存実装の代表例

### Repository

- [backend/internal/adapter/repository/position_repo.go](../../../backend/internal/adapter/repository/position_repo.go) — `PositionRepository` 実装
- [backend/internal/adapter/repository/position_closer.go](../../../backend/internal/adapter/repository/position_closer.go) — `CloseAndRecord` (1 Tx で positions UPDATE + trades INSERT)
- [backend/internal/adapter/repository/trade_repo.go](../../../backend/internal/adapter/repository/trade_repo.go) — `TradeRepository` (UI 用は `ListSince` (opened_at 基準)、risk gate 用は `ListClosedSince` / `SumClosedLossJPYSince` / `CountClosedSince` (closed_at 基準))
- [backend/internal/adapter/repository/strategy_config_repo.go](../../../backend/internal/adapter/repository/strategy_config_repo.go) — `StrategyConfigRepository` + `ConfigPromoter` (PromoteActive)
- [backend/internal/adapter/repository/candle_repo.go](../../../backend/internal/adapter/repository/candle_repo.go) — UPSERT で同 (symbol, timeframe, opened_at) を重複登録防止
- [backend/internal/adapter/repository/signal_rejection_repo.go](../../../backend/internal/adapter/repository/signal_rejection_repo.go) — risk gate reject 履歴
- [backend/internal/adapter/repository/market_summary_repo.go](../../../backend/internal/adapter/repository/market_summary_repo.go) — `market_summaries` 履歴
- [backend/internal/adapter/repository/validation_event_repo.go](../../../backend/internal/adapter/repository/validation_event_repo.go) — validation pass/fail 履歴
- [backend/internal/adapter/repository/advisor_run_repo.go](../../../backend/internal/adapter/repository/advisor_run_repo.go) — Claude advisor 1 実行のメタ + I/O 保存

### Broker

- [backend/internal/adapter/broker/gmo_fx.go](../../../backend/internal/adapter/broker/gmo_fx.go) — GMO Coin REST 実装 (Public + Private)
- [backend/internal/adapter/broker/paper.go](../../../backend/internal/adapter/broker/paper.go) — シミュレータ。tick ベースで TP/SL 約定。pip サイズは `PaperBrokerConfig` に持たず、fill 時に `market.PipSize(req.Symbol)` で都度解決する (= 1 broker instance で multi-symbol を扱える)。`PnLJPY` は `(exit - entry) * quantity` で JPY quote symbol (USD_JPY / EUR_JPY / GBP_JPY) を前提にしているため、非 JPY quote (例 EUR_USD) を入れると損益単位が USD になる (この helper は test assertion 用。本番の close PnL は `position.ComputeClosePnL` が quote→JPY 換算倍率を受け取る — [usecase.md](usecase.md) の `resolve_quote_jpy_rate.go`)。
- [backend/internal/adapter/broker/gmo_fx_integration_test.go](../../../backend/internal/adapter/broker/gmo_fx_integration_test.go) — httptest 録画再生

### Notifier

- [backend/internal/adapter/notifier/stdout.go](../../../backend/internal/adapter/notifier/stdout.go)

### Advisor

- [backend/internal/adapter/advisor/claude_cli.go](../../../backend/internal/adapter/advisor/claude_cli.go) — `claude -p` を 1 回起動 (プロンプト側で 4 subagent を並列に使う) + 出力から YAML 抽出

### Artifact (file)

- [backend/internal/adapter/artifact/file_market_summary.go](../../../backend/internal/adapter/artifact/file_market_summary.go) — `port.MarketSummaryArtifactStore` の file 実装。tempfile + rename で atomic write
- [backend/internal/adapter/artifact/file_strategy_config.go](../../../backend/internal/adapter/artifact/file_strategy_config.go) — `port.StrategyConfigArtifactStore` の file 実装。active / next YAML の outbox

### Journal (file, append-only 観測ログ)

- [backend/internal/adapter/journal/llm_decision_journal.go](../../../backend/internal/adapter/journal/llm_decision_journal.go) — `port.LLMDecisionJournal` の file 実装。自律 LLM ループの判断履歴を `runtime/logs/llm_decisions.jsonl` に 1 行 1 サイクルで追記 (全 symbol を時系列で interleave)。`event:cycle` (stage/side/TP/SL/reason) と `event:parse_fallback` (claude raw stdout) を残し、健全な no_trade と silent parse 失敗を区別可能にする。**観測のみ — 発注経路から独立**で、`Record` 失敗はサイクルを止めない (nil = no-op)。並列ペアの書込は内部 mutex + 1 行 O_APPEND で直列化。playbook FileStore と同じく DB を避けた append-only ログ。

### Data ingest (offline backtest only — live 発注経路からは独立)

- [backend/internal/adapter/histdata/histdata.go](../../../backend/internal/adapter/histdata/histdata.go) — HistData.com 無料 M1 CSV を **EST(no-DST = 固定 UTC-5)→ UTC** 正規化して `port.CandleRecord` に変換 (fail-loud)。GMO 外為 API が返せない pre-2023 (2015–2023 の多レジーム履歴) を隔離 DB `fxbot_backtest` に流すための parser。価格は BID (GMO klines は ASK → 継ぎ目 ~1pip 定数オフセット、相対水準戦略には無害)。`cmd/histdata-ingest` が消費する。
- [backend/internal/adapter/histdata/symbol.go](../../../backend/internal/adapter/histdata/symbol.go) — `DAT_ASCII_<PAIR>_M1_*.csv` のファイル名から DB symbol を導出 (USD_JPY / EUR_JPY / GBP_JPY / EUR_USD / GBP_USD の 5 ペア限定、未知ペアは reject)。
- 注: **ライブ candles テーブルには書かない**。書込先は [repository.SafeBacktestDSN](../../../backend/internal/adapter/repository/backtestdsn_guard.go) (`_backtest` 接尾辞 + live DSN と異なる事を強制) でガードされ、`cmd/histdata-ingest -apply` だけが隔離 DB へ `Candles.UpsertBatch` する。

---

## アンチパターン (= 過去にやってしまった失敗)

- ❌ GMO API レスポンスの JSON 構造体を usecase に流して直接読ませた — adapter で `port.PositionRecord` に変換するように直した
- ❌ `pgx.Conn.Begin` の呼び出しが usecase 側に漏れていた — `Repository.WithTx(ctx, fn)` 形に閉じ込めた
- ❌ Live broker が rate limit に当たって panic 連鎖した — limiter を adapter 内部に内蔵 + retry 制御を usecase に通知する形に直した
- ❌ Claude CLI subprocess を `os/exec.Command` で生で叩いて timeout を制御しなかった — `context.WithTimeout` + 強制 kill ハンドリングを adapter 内に書いた

---

## 関連 docs

- [port.md](port.md) — interface 定義
- [MIGRATIONS.md](../../workflows/MIGRATIONS.md) — DB schema 変更時の adapter 影響
- [TESTING.md](../../workflows/TESTING.md) — integration / 録画再生
- [FAILURE_MODES.md](../FAILURE_MODES.md) — 外部 I/O の失敗ハンドリング

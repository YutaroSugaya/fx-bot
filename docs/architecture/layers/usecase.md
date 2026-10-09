# Layer: Usecase

## 役割 (1 行)

ビジネスフローの調整役。複数 domain 操作を順序立て、Repository / Broker / Notifier を port 経由で呼び、状態遷移を完結させる。

---

## CQRS — Command と Query は明確に分離する

1 つの usecase が **書きと読みの両方** を持つことは禁止。

### Command の規約

- 場所: `backend/internal/usecase/command/`
- 命名: 動詞 (例: `PlaceOrder`, `ClosePosition`, `PromoteConfig`, `Reconcile`)
- 戻り値: **副作用 ID + 最小限の確認情報のみ** (大きな読み取り結果を返さない)
- DB 操作:
  - 複数テーブルを更新する場合は **必ず 1 Transaction**
  - 失敗時はロールバック + (重要なら) emergency_stop
- 副作用が外部システム (broker / notifier) を含むなら **冪等性 or claim パターン** を考慮
- メソッド名は **`Execute`** を基本とする (例外は下記「event-driven 命名」)

### Query の規約

- 場所: `backend/internal/usecase/query/`
- 命名: `Get`, `List`, `Find` プレフィックス
- 戻り値: View 向けの DTO (entity を直接返さない)
- 読み取り専用 (Tx 不要)
- N+1 を避ける (JOIN や bulk select でまとめる)
- 集計・派生値は Query 層で計算 (例: 未実現損益、PnL サマリ)

### event-driven 命名 (例外)

長時間動く / tick / signal 駆動の usecase は `Execute` ではなく以下を許可:

| 種類 | 例 | パターン |
|---|---|---|
| Tick 駆動 | `ManageOpenPositions.OnTick` | 1 秒/1 分などの周期で呼ばれる |
| Signal 駆動 | `ExecuteOrder.OnSignal` | strategy が signal を返したときだけ呼ばれる |
| Loop 駆動 | `AdvisorCycle.Run` | scheduler から起動される長尺フロー |
| Promote 系 | `Promoter.PromoteFromYAML` | YAML 入力 → DB promote の 1 連 |
| Evaluate 系 | `EvaluateEntry.Evaluate` | strategy + risk gate の 1 段だけ走る pure-ish |

event-driven な usecase はこの命名のほうがコードと概念が一致する。命名で実態を曲げない。

### Command / Query を分ける効果

- 読み取りの cache が独立して打てる (将来 redis 等)
- 書き込み専用 model と読み取り専用 view を別物にできる
- 並列化・スケールの方向が分けられる
- テストが疎結合になる

---

## やること (do)

- Repository / Broker / Notifier を **port** 経由で呼ぶ
- Tx の開始 / commit / rollback を制御する
- emergency_stop の trip 判断 (broker 成功 + DB 失敗など)
- mutex / claim によるレース防止 (`EntryMutex`, `CloseMutex`)
- domain の純粋関数 (`ComputeTPSLPrices`, `risk.Gate.EvaluateSignal`) を呼んで業務判定を作る

---

## やらないこと (don't)

- ❌ `*broker.GmoBroker` などの **具体型** を知る (interface 経由のみ)
- ❌ `pgxpool.Pool` を直接持つ (Repository interface 経由)
- ❌ `http.Server` / `slog` の薄いラップを書かない (it's the handler's / safety's job)
- ❌ Tx の中で外部 API (GMO 等) を呼ばない (失敗時の整合性が破綻する)
- ❌ Domain entity の代わりに Repository record をビジネス判定で使う (domain 経由)

### file I/O は usecase に置かない (方針)

usecase / query / command レイヤーは直接 file I/O をせず、port/adapter 経由に統一している:

- `port.MarketSummaryArtifactStore` / `adapter/artifact/FileMarketSummaryStore`
  が summary JSON 永続化を担当。
- `port.StrategyConfigArtifactStore` / `adapter/artifact/FileStrategyConfigStore`
  が active / next YAML 入出力を担当 (`command.Promoter` は port 経由)。
- `query.AskClaudeQuery` も `MarketSummaryArtifactStore.Read` 経由で読む。

usecase は port interface のみを参照する不変条件を維持する。新規 file I/O が
必要になった場合も同様に `port.<Name>Store` + `adapter/artifact/File<Name>Store`
で導入すること。

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Command Usecase | `usecase/command/` | `<action>.go` (snake_case) | `close_position.go` |
| Query Usecase | `usecase/query/` | `<action>.go` | `list_positions.go` |
| Root Usecase | `usecase/` | helper 系 | `build_market_summary.go` |

### 構造体・コンストラクタ

```go
type ClosePositionCommand struct {
    Broker   port.Broker
    Closer   port.PositionCloser
    Mutex    *sync.Mutex
    Counters *Counters
    Logger   *slog.Logger
}

func NewClosePositionCommand(deps ClosePositionDeps) *ClosePositionCommand { ... }

func (c *ClosePositionCommand) Execute(ctx, input) (output, error) { ... }
```

- 依存は struct field、外部から差し込めるように
- input/output は struct 化 (引数羅列を避ける)

---

## テスト方法 (この層特有)

- **strict t_wada 流 TDD** — Red → Green → **Refactor** の 3 段すべて必須。
  Refactor 段の省略は禁止 ([TESTING.md](../../workflows/TESTING.md) §5)。
- **古典派 (Classical / Detroit) TDD** — mock 過多を避け、real 実装を組み合わせる。
- 主要 real collaborator:
  - `broker.PaperBroker` — broker 境界の real 実装 (in-memory match engine)
  - `backtest.InMemoryPositionRepo` / `InMemoryTradeRepo` / `InMemoryCandleRepo` /
    `InMemoryStrategyConfigRepo` / `InMemoryValidationEventRepo` — DB 境界の real 実装
  - `backtest.InMemoryPositionCloser` — close saga (CLOSING→CLOSED + trade insert) の real 実装
  - `safety.Trip()` — emergency_stop 副作用 (flag file 書き込み)
- mock の使用は以下 3 用途に限定 (= **Refactor 段階で必ず再評価** する):
  1. システム境界 (= プロセス外部) を切り出すとき (DB / GMO / Claude CLI)
  2. 特定の失敗シナリオを注入したいとき (timeout / cancel 失敗 / race)
  3. 時刻・乱数など非決定性を排除したいとき (`Clock func() time.Time` 注入)
- mock を残す場合は **ファイル / 型の先頭コメントに 3 用途のどれに該当するかを明記** する
  (本リポジトリ既存例: `usecase/command/fakes_test.go` の `fakeBroker` / `fakeLiveBroker` /
  `fakeCloser`、`adapter/broker/mock.go` の `MockBroker`)。
- 同じ動作軸の複数ケースは **table-driven** にする
- 詳細は [TESTING.md](../../workflows/TESTING.md) を参照

---

## 既存実装の代表例

### Command

- [backend/internal/usecase/command/trading_cycle.go](../../../backend/internal/usecase/command/trading_cycle.go) — 1 tick の決定パス
- [backend/internal/usecase/command/execute_order.go](../../../backend/internal/usecase/command/execute_order.go) — `OnSignal` で broker 発注。発注直前に `ValidateSignalBoundaries` で実 Signal 値を検査
- [backend/internal/usecase/command/signal_boundary.go](../../../backend/internal/usecase/command/signal_boundary.go) — `ValidateSignalBoundaries` 純粋関数: 実 Signal の qty 範囲 / MaxHold≤cap / SL・TP sanity / **per-trade 最悪損失 JPY ≤ hard_limits.order_boundary.max_loss_per_trade_jpy** を発注境界で検査。execute_order (auto) と manual_trade (manual) が共用
- [backend/internal/usecase/command/manage_open_positions.go](../../../backend/internal/usecase/command/manage_open_positions.go) — `OnTick` で TP/SL/MaxHold/Ratchet close。`evaluateExit` の優先順序: **ratchet TP** (armed && peak から giveback だけ retrace) → **ratchet STOP** (loss_armed && trough から giveback だけ回復 → `ratchet_stoploss` で満額 SL 手前で浅く撤退。利確側が armed の玉は TP が先に return するので届くのは利確未 arm の玉のみ) → **MaxHold 4 段** (early-exit window → hard deadline → soft + extension → soft 単独) → paper TP/SL。各フィールドは `PositionRecord` に snapshot / runtime 保持 ([port.md `Ratchet TP フィールド + Ratchet STOP`](port.md) 参照)
- [backend/internal/usecase/command/manage_open_positions_exits.go](../../../backend/internal/usecase/command/manage_open_positions_exits.go) — `evaluateRatchetExit` / `evaluateMaxHoldExit` 純粋関数
- [backend/internal/usecase/command/close_position.go](../../../backend/internal/usecase/command/close_position.go) — manual close
- [backend/internal/usecase/command/extend_maxhold.go](../../../backend/internal/usecase/command/extend_maxhold.go) — 開いている position の保有上限延長 (`POST /api/positions/extend` =「延長ボタン」)。`max_hold_minutes` に `add_minutes` (1〜720/回) を加算し新 deadline/remaining を返す。broker/pip/mutex は持たず純粋に `PositionRepository.ExtendMaxHold` を叩くだけ (TP/SL の GMO 側 OCO には触れない)。`max_hold_minutes` は per-position 凍結値 ([config snapshot ルール](port.md)) で config 変更では動かせないため、開いた建玉の上限を伸ばす唯一の経路。`OnTick` は毎 tick で `max_hold_minutes` を読み直すので UPDATE は次 tick から効く (close/再 build 不要)
- [backend/internal/usecase/command/manual_trade.go](../../../backend/internal/usecase/command/manual_trade.go) — manual entry
- [backend/internal/usecase/command/promote_config.go](../../../backend/internal/usecase/command/promote_config.go) — 1 Tx promote + `LoadActiveFromDB` (起動時の active config 読み込み)
- [backend/internal/usecase/command/reconcile.go](../../../backend/internal/usecase/command/reconcile.go) — 起動時 + 周期 (30s, Live) broker→DB sync
- [backend/internal/usecase/command/reconcile_handlers.go](../../../backend/internal/usecase/command/reconcile_handlers.go) — Reconcile dispatcher の分割実装 (naked / stale / adopt 各ハンドラ)
- [backend/internal/usecase/command/advisor_cycle.go](../../../backend/internal/usecase/command/advisor_cycle.go) — Claude advisor 全フロー
- [backend/internal/usecase/command/evaluate_entry.go](../../../backend/internal/usecase/command/evaluate_entry.go) — strategy + risk gate 1 段
- [backend/internal/usecase/command/live_close_helpers.go](../../../backend/internal/usecase/command/live_close_helpers.go) — Live 決済前の cancel saga
- [backend/internal/usecase/command/live_exit_protector.go](../../../backend/internal/usecase/command/live_exit_protector.go) — Live entry 後の OCO 後付けフェーズ統一 (entry saga 続行中の pending tracking 含む)
- [backend/internal/usecase/command/close_saga.go](../../../backend/internal/usecase/command/close_saga.go) — CAS-based close saga (claim → cancel settle legs → market close → resolve → CloseAndRecord)
- [backend/internal/usecase/command/entry_admission.go](../../../backend/internal/usecase/command/entry_admission.go) — auto / manual entry の統一 pre-trade gate (mutex + 最新 live state の再読込)
- [backend/internal/usecase/command/trade_aggregates.go](../../../backend/internal/usecase/command/trade_aggregates.go) — closed_at DESC trade スライスから `ConsecutiveLosses` / `LastLossClosedAt` / `Buy,SellStopLossesToday` を 1-pass で算出する純粋 helper (`DeriveTradeAggregates`)。`app.Worker.accountSnapshot` / `EntryAdmission.snapshot` / `app.BuildPromotionAccountState` の 3 caller が同じ helper を使うことで、連敗後の段階的 cooldown / 同方向 SL ブロックの集計が caller 間で drift しない
- [backend/internal/usecase/command/edge_metrics.go](../../../backend/internal/usecase/command/edge_metrics.go) — closed trade スライスから edge の質指標 (`ProfitFactor` / `RewardRisk` / `AvgWin,LossPips` / `ExpectancyJPY` / `WinRatePct` / `MaxConsecutiveLosses`) を 1-pass で算出する純粋 helper (`DeriveEdgeMetrics`)。`/api/status` の dashboard 計測パネル (query `EdgeMetricsView`) と日次サマリ (`app.OpsSummary`) が同じ helper を使う。「勝率は高いが逆RR (勝ち pips < 負け pips)」を可視化する
- [backend/internal/usecase/command/llm_event_retrigger.go](../../../backend/internal/usecase/command/llm_event_retrigger.go) — event_retrigger の判断コーディネータ `LLMEventRetrigger`。定期 LLM 判断サイクルに加えて、(a) 決済確定 (`OnPositionClosed` — close_saga / reconcile のフックから) と (b) 急変動 (`OnPriceTick` — worker の 1s tick から、|mid − MoveWindow 前の 1m close| ≥ MovePips) で全ペア再判断を追加起動する。自分ではサイクルを走らせず注入された `Trigger` (= 手動「全ペア再判断」ボタンと同じ `triggerLLMDecisionCycle` 経路: 週末ゲート + `llmCycleRunning` ガード) を呼ぶだけなので、定期サイクル実行中のイベントは拒否され二重実行しない。cooldown は move/close 別・全ペア global・fire **試行**で arm (拒否でも再連打しない)。tick 側は per-symbol 10s throttle + candles getter 渡しで 1s ループに copy コストを載せない。config は `llm_decision.event_retrigger` (`config.EventRetriggerSection`、負値は load 時 loud-fail・省略 = OFF)。全経路 nil-safe (未配線 = 機能 OFF)
- [backend/internal/usecase/command/resolve_quote_jpy_rate.go](../../../backend/internal/usecase/command/resolve_quote_jpy_rate.go) — close PnL を JPY 建てに換算する倍率 (`quoteJPYRate`) を解決する純粋 helper (`resolveQuoteJPYRate`)。JPY-quote ペア (USD_JPY 等) は broker を呼ばず `1.0`、USD-quote ペア (EUR_USD 等) は broker から USD/JPY を取得しその mid を倍率にする (USD 建て損益 × USD/JPY = JPY)。レート欠損・broker error は伝播し誤った JPY 損益を記録しない (fail-close)。`close_position.go` / `reconcile.go` (resolveAndRecordClose / recordEstimatedClose) / `close_saga.go` / `manage_open_positions.go` の 5 close 経路が `position.ComputeClosePnL` の `quoteJPYRate` 引数にこの値を渡す。換算ロジック本体は domain の [`market.QuoteCurrency` / `market.QuoteJPYRate`](domain.md)

### Query

- [backend/internal/usecase/query/list_open_positions.go](../../../backend/internal/usecase/query/list_open_positions.go)
- [backend/internal/usecase/query/list_trades.go](../../../backend/internal/usecase/query/list_trades.go)
- [backend/internal/usecase/query/get_bot_status.go](../../../backend/internal/usecase/query/get_bot_status.go)
- [backend/internal/usecase/query/list_recent_decisions.go](../../../backend/internal/usecase/query/list_recent_decisions.go)
- [backend/internal/usecase/query/get_market_state.go](../../../backend/internal/usecase/query/get_market_state.go)
- [backend/internal/usecase/query/ask_claude.go](../../../backend/internal/usecase/query/ask_claude.go)

### Root

- [backend/internal/usecase/build_market_summary.go](../../../backend/internal/usecase/build_market_summary.go) — candles + ticker + BotState → MarketSummary, plus `PublishMarketSummary` thin wrapper that calls `port.MarketSummaryArtifactStore` (the file I/O itself lives in [adapter/artifact/file_market_summary.go](../../../backend/internal/adapter/artifact/file_market_summary.go), which does the atomic tempfile + rename)

---

## アンチパターン (= 過去にやってしまった失敗)

- ❌ `usecase.command.X` を test 用 `mockX` で置き換える (古典派 TDD 違反、リファクタ耐性なし)
- ❌ Green で増やした fake を **Refactor 段階で再評価せず** PR に出した
  (= 3 用途 (boundary / failure-injection / determinism) のどれにも該当しない fake が
  残り、リファクタ耐性が落ちる)。Refactor 段では必ず「この mock は 3 用途のどれか?」を
  自問し、該当しないなら `backtest.InMemory*Repo` / `broker.PaperBroker` 等の
  real collaborator に置換する。
- ❌ **Refactor 段を skip した** ("Green で通ったから OK" は cycle 不完全。
  次の cycle で技術的負債を踏む。詳細は [TESTING.md §5.3-§5.4](../../workflows/TESTING.md))
- ❌ Broker 成功 + Position.Insert 失敗で log + return (silent fail-open) — emergency_stop trip に直した
- ❌ ResolveExecution timeout で 0 fill price のまま DB INSERT — trip + DB を書かないに直した
- ❌ Live close で `GetActiveOrders(symbol)` の全件を cancel — position-scoped に直した (TP/SL の orderId を `positions` 行に記録し、close saga は記録済み id だけを cancel)
- ❌ ManualTradeCommand が emergency_stop / risk gate を見ない — EntryAdmission で auto / manual を統一した
- ❌ Query の戻り値で domain entity をそのまま返す — View DTO 化

---

## 関連 docs

- [domain.md](domain.md) — usecase が呼ぶ純粋ロジック
- [port.md](port.md) — interface 設計
- [FAILURE_MODES.md](../FAILURE_MODES.md) — Rollback > Fallback / Tx / mutex
- [TESTING.md](../../workflows/TESTING.md) — usecase テスト戦略

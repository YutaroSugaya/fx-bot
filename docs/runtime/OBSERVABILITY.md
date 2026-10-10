# Observability — Counters / Logs / Status API / Audit Tables / Runtime Files

bot の動作状況を「外から見える状態」にする経路すべての正本ドキュメント。
Counters / ログ / `/api/status` / audit table / runtime ファイルを扱う。

---

## 1. Counters (in-memory, atomic)

[backend/internal/app/counters.go](../../backend/internal/app/counters.go):

```go
type Counters struct {
    TickerErrors        atomic.Int64 // worker の Broker.GetTicker 失敗
    EmergencyTrips      atomic.Int64 // emergency_stop.flag を発火した回数
    ResolveTimeouts     atomic.Int64 // ResolveExecution が timeout した回数
    CloseRaces          atomic.Int64 // CloseAndRecord ok=false (二重 close 検出) 回数
    NakedPositions      atomic.Int64 // reconcile で naked_broker_position を検出した回数
    TradingCycleMissing atomic.Int64 // priceTick: Worker.TradingCycle == nil (配線抜け検知)
}
```

`/api/status` で `CountersSnapshot` 経由で JSON 出力。

新規 Counters を追加する手順:

1. `Counters` 構造体にフィールド追加 (型は `atomic.Int64`)
2. `CountersSnapshot` 構造体に対応する JSON タグ付きフィールド追加
3. `Snapshot()` でロード
4. 該当の trip / 失敗を起こす usecase に `c.X.Add(1)` を入れる

---

## 2. Logger (slog)

`log/slog` を使う。key=value 形式の構造化ログ。

```go
logger.Info("position_closed",
    "position_id", id,
    "close_reason", reason,
    "pnl_jpy", pnl)
```

ルール:

- 重要な状態遷移は `Info`、失敗は `Warn` / `Error`
- key は **snake_case** (`position_id`, `close_reason`, `consecutive_losses`)
- emergency_stop trip 時は `Error` で `reason=...` を含める

### 2.1 監視 alert ループ (ops alert)

[backend/cmd/bot/ops_alert_loop.go](../../backend/cmd/bot/ops_alert_loop.go) が 30 分ごとに symbol 別に集計し、
当日の実現損が `risk.max_daily_loss_jpy` に届いた / 最後の約定から 6 時間取引が無い / 日次サマリ (bot の
タイムゾーンで 07:00 以降に 1 日 1 回) を Notifier に出す。Notifier は stdout (slog) だけなので、今はログに出るだけ。
外へ通知するには Notifier の adapter を足す。

---

## 3. `/api/status` レスポンス

[backend/internal/app/handler/status_handler.go](../../backend/internal/app/handler/status_handler.go):

```json
{
  "mode": "paper_config",
  "uptime": "3h27m12s",
  "emergency_stop": false,
  "symbols": [
    { "symbol": "USD_JPY", "open_positions": 0, "active_config_id": "20260520-070000-usdjpy",
      "strategy": "momentum_pullback", "enabled": true, "pnl_24h_jpy": -27.0, "early_exit_count_24h": 0 }
  ],
  "account_open_positions": 0,
  "symbol": "USD_JPY",
  "open_positions": 0,
  "active_config_id": "20260520-070000-usdjpy",
  "valid_until": "2026-05-20T08:00:00Z",
  "strategy": "momentum_pullback",
  "enabled": true,
  "ticker_errors": 0,
  "counters": {
    "ticker_errors": 0,
    "emergency_trips": 0,
    "resolve_timeouts": 0,
    "close_races": 0,
    "naked_positions": 0,
    "trading_cycle_missing": 0
  },
  "pnl_24h_jpy": -27.0,
  "reject_count_24h": 2,
  "early_exit_count_24h": 0,
  "last_advisor_duration_ms": 142718
}
```

(フィールド構成の SoT は [BotStatusView](../../backend/internal/usecase/query/get_bot_status.go) struct。per-symbol の値は `symbols[]`、top-level の `symbol` / `open_positions` / `active_config_id` 等は `bot_config.symbols[0]` を映す legacy 互換。`ticker_errors` (legacy top-level) と `counters.ticker_errors` の二重出力も経過措置)

`pnl_24h_jpy / reject_count_24h / early_exit_count_24h / last_advisor_duration_ms / edge_metrics` は fn-typed 依存性で wire される ([api_wire.go](../../backend/cmd/bot/api_wire.go))。fn が nil or err の場合は `omitempty` で省略される。

### 監視で見るべきしきい値

- `emergency_trips > 0` → **即対応**
- `ticker_errors / hour > N` → broker / network 異常
- `close_races > 0` → mutex 設計の見直し or race
- `naked_positions > 0` → reconcile の取り込み確認
- `resolve_timeouts > 0` → GMO 約定確認の遅延
- `pnl_24h_jpy` が日次損失 cap (`risk.max_daily_loss_jpy`) に近い → 単日損失大、[OPERATIONS_RUNBOOK.md §2](OPERATIONS_RUNBOOK.md) 参照
- `reject_count_24h > 20` → advisor 出力品質低下 or hard_limits の閾値見直し
- `last_advisor_duration_ms > 120000` (= 2 分) → Claude CLI が重い、timeout 設定見直し

---

## 4. Audit Tables (= 履歴保存)

| テーブル | 役割 | 主用途 |
|---|---|---|
| [strategy_configs](DATA_MODEL.md#strategy_configs-main) | 生成された config の全履歴 (raw_yaml 含む) | audit + backtest replay |
| [config_validation_events](DATA_MODEL.md#config_validation_events-main) | validation 各段階の pass/fail | reject 理由の後追い |
| [ai_advisor_runs](DATA_MODEL.md#ai_advisor_runs-main) | Claude advisor 1 回ごとの input/output | Claude debug + duration 計測 |
| [positions](DATA_MODEL.md#positions-main) | 全ポジション (open / closed) | 取引履歴 + PnL 元データ |
| [trades](DATA_MODEL.md#trades-main) | 決済済みラウンドトリップ | PnL 集計 + 連続損失カウント |
| [signal_rejections](DATA_MODEL.md#signal_rejections-main) | risk gate reject 履歴 | プロンプト改善ループの主要入力 |
| [market_summaries](DATA_MODEL.md#market_summaries-main) | advisor サイクルごとの市場サマリスナップショット (INSERT するのは AdvisorCycle だけ。advisor が off なら空のまま) | spread 較正 (`cmd/spread-calibrate`) / 将来の advisor-replay backtest 入力 |
| [candles](DATA_MODEL.md#candles-main) | OHLCV 履歴 (UPSERT) | backtest 入力 + 起動 backfill |

スキーマ詳細は [DATA_MODEL.md](DATA_MODEL.md)、運用シーケンスは [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) §6。

---

## 5. Runtime ファイル

| ファイル | 用途 | 観測手段 |
|---|---|---|
| `runtime/emergency_stop.flag` | trip するたびに `<RFC3339> <reason>` の 1 行で上書きされる (最新の理由だけが残る) | `cat runtime/emergency_stop.flag` |
| `runtime/ai_input/latest_summary.json` | 1 分ごとの市場サマリ (bot_config の symbols が 2 つ以上なら `latest_summary_<SYMBOL>.json`) | `jq . runtime/ai_input/latest_summary.json` (複数なら `jq . runtime/ai_input/latest_summary_USD_JPY.json`) |
| `runtime/ai_output/*.yaml` | Claude が生成した raw YAML (legacy) | `ls -la runtime/ai_output/` |
| `runtime/logs/llm_decisions.jsonl` | LLM 決定ループの判断ジャーナル (1 行 1 サイクル: stage / side / TP / SL / reason / price / spread / range_pos_24h / htf_veto_exempt。`event:parse_fallback` 行は raw stdout を保全)。emergency_stop 中のサイクルは LLM を呼ばず stage `emergency_stop` で記録される | `tail -5 runtime/logs/llm_decisions.jsonl \| jq` |
| `runtime/llm_decision_status.json` / `runtime/advisor_v2_status.json` | 直近サイクルの symbol 別結果 (ダッシュボード表示用) | `jq . runtime/llm_decision_status.json` |
| `runtime/logs/pre-stop-checks.log` | Claude Code Stop hook の `make check-backend` 出力 | `tail -40 runtime/logs/pre-stop-checks.log` |

`.gitignore` に runtime/ は入っているので、リポジトリには出ない。

---

## 6. Frontend dashboard

[frontend/app/page.tsx](../../frontend/app/page.tsx) で `/api/status`、`/api/positions`、`/api/trades`、`/api/advisor/recent` 等をポーリングして表示。
詳細は [API_CONTRACT.md](../integrations/API_CONTRACT.md)。

---

## 7. 新規 Live 失敗パターンを観測したい時の手順

1. **Counters にフィールド追加**: 上記 §1 の手順で `Counters.NewFailure`
2. **Trip if 重大**: `safety.Trip(path, "new_failure_reason")` を呼ぶ
3. **Logger に Error**: `Logger.Error("new_failure", "key", value)`
4. **Audit Table に記録 (必要なら)**: 例えば `signal_rejections` への INSERT
5. **`/api/status` のレスポンス更新**: `CountersSnapshot` のフィールド追加 ([types.go](../../backend/internal/app/handler/types.go) も更新)
6. **frontend に表示** (任意): `frontend/app/page.tsx` を更新

---

## 8. アンチパターン

- ❌ Counters なしで `Logger.Warn` だけ吐く (= grep しないと気付かない)
- ❌ trip 時に Counters increment を忘れる (= `/api/status` の `emergency_trips` が 0 のまま)
- ❌ JSON key を camelCase で出す (= snake_case 統一の規約違反)
- ❌ 大量データ (例: `ai_advisor_runs.input_json` 全文) を `/api/status` に乗せる
- ❌ Logger key が `pos` / `err1` などの省略形 (= grep しにくい)

---

## 9. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [RUNTIME.md](RUNTIME.md) — Counters / runtime ファイル
- [DATA_MODEL.md](DATA_MODEL.md) — audit テーブル
- [layers/handler.md](../architecture/layers/handler.md) — `/api/status` 実装
- [API_CONTRACT.md](../integrations/API_CONTRACT.md) — frontend ↔ backend 型同期
- [FAILURE_MODES.md](../architecture/FAILURE_MODES.md) — どんな失敗で何を観測すべきか

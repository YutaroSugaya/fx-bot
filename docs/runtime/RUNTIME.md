# Runtime — Goroutines, Mutexes, Counters

bot が起動中に動かしている goroutine / 並行性制御 / runtime artifact の正本ドキュメント。

設計の契約は [ARCHITECTURE.md](../ARCHITECTURE.md) と [layers/](../architecture/layers/) を参照。
ここは **「何が並行で動いていて、どうやって直列化しているか」** を集約する。

---

## 1. 常駐 goroutine

`bot_config.symbols` に列挙された symbol N 個ごとに `SymbolBundle` を構築し
([backend/internal/app/symbol_bundle.go](../../backend/internal/app/symbol_bundle.go))、
各 bundle に対して priceLoop / minuteLoop (+ Live 時は reconcileLoop) を生成する。
全 symbol 共有の goroutine は scheduler / apiServer / advisor_v2 / llm_decision / llm_reflection
(+ ops alert)。LLM を呼ぶ経路 (advisor / advisor_v2 / llm_decision / reflection) は opt-in で、
無効なときは起動ログを 1 行出して即 return する (tracked の `configs/bot_config.yaml` では全部 off)。

```
┌─ priceLoop (per symbol) ─────────────────────────────────┐
│ 1 秒間隔: ticker pull → aggregator → ManageOpenPositions │
│            → TradingCycle (entry 判断 + 発注)            │
│ 実装: backend/internal/app/worker.go: RunPriceLoop       │
└──────────────────────────────────────────────────────────┘

┌─ minuteLoop (per symbol) ────────────────────────────────┐
│ 1 分境界: 最新 ticker + aggregator candles で summary     │
│           を JSON に書き、各 timeframe の最新 bar を      │
│           candles テーブルに UPSERT                       │
│ 実装: backend/internal/app/worker.go: RunMinuteLoop       │
└──────────────────────────────────────────────────────────┘

┌─ scheduler (全 symbol 共有 1 本・ai_advisor.enabled 時) ──┐
│ 全 symbol 横断で advisor cycle 駆動。tick 毎に bundles    │
│ を fan-out (max_concurrent_symbols ガード) し各 symbol の │
│ AdvisorCycle.Run を auto / event で起動                   │
│ 実装: backend/internal/app/scheduler.go                   │
│       backend/cmd/bot/loops.go: configureScheduler        │
└──────────────────────────────────────────────────────────┘

┌─ apiServer (1 本) ────────────────────────────────────────┐
│ HTTP 常時 listen: /api/* + BasicAuth + CORS               │
│ 実装: backend/internal/app/api_server.go: APIServer.Run   │
└──────────────────────────────────────────────────────────┘

┌─ reconcileLoop (Live mode のみ, per symbol) ─────────────┐
│ 30 秒間隔: broker open positions ↔ DB open positions      │
│           を照合。broker にだけある玉は外部建玉として取込、 │
│           DB にだけある玉は実約定を解決して記録 (猶予後も   │
│           解決できなければ emergency_stop trip)           │
│ 実装: backend/cmd/bot/loops.go: runReconcileLoop          │
│       (LiveRuntimeReconcileInterval = 30 * time.Second)   │
└──────────────────────────────────────────────────────────┘

┌─ runSignatureV2Scheduler (advisor_v2.enabled 時, 1 本) ──┐
│ 日足チャートブレイク検出 → 判定 subagent → 既存発注経路   │
│ 実装: backend/cmd/bot/loops.go: runSignatureV2Scheduler   │
└──────────────────────────────────────────────────────────┘

┌─ runLLMDecisionScheduler (llm_decision.enabled 時, 1 本) ┐
│ interval_minutes (既定 60分) 毎: 各 symbol の自律 LLM     │
│ 判断サイクル (claude -p → コード veto → OnSignal →        │
│ risk Gate → broker OCO)。休場中は LLM を呼ばずに skip、   │
│ emergency_stop 中は stage=emergency_stop で LLM を呼ばない│
│ 実装: backend/cmd/bot/loops.go: runLLMDecisionScheduler   │
└──────────────────────────────────────────────────────────┘

┌─ runReflectionScheduler (llm_decision.enabled かつ ──────┐
│   reflection_enabled 時, 1 本)                            │
│ reflection_interval_minutes (既定 1日) 毎: Reflexion 反省 │
│ ループが確定トレードを分析し playbook_<SYMBOL>.jsonl に    │
│ 改訂版を追記する。休場中は skip                           │
│ 実装: backend/cmd/bot/loops.go: runReflectionScheduler    │
└──────────────────────────────────────────────────────────┘
```

起動は [backend/cmd/bot/loops.go](../../backend/cmd/bot/loops.go) の `runLoops` で 1 つの `sync.WaitGroup` 配下に。
goroutine は `recoverGoroutine` で panic-protected (1 symbol crash で全体は落ちない)。
graceful shutdown は `<-ctx.Done()` 後に `wg.Wait()`。Paper mode では reconcileLoop は起動しない。

### Debug 用 goroutine

`BOT_DEBUG_FORCE_ADVISOR=1` を設定すると起動 2 秒後に advisor を 1 回 manual で fire する debug 用 goroutine が起動 (`loops.go` 内)。`ai_advisor.enabled: false` のときは WARN を出して fire しない。

---

## 2. Mutex 一覧 (= 共有リソースの直列化)

| Mutex | フィールド / package | 何を直列化 |
|---|---|---|
| `sharedEntryMu` | `*sync.Mutex` (cmd/bot/main.go で生成 → ExecuteOrder.EntryMutex + EntryAdmission.Mutex に共有) | 全 symbol 横断で `broker.PlaceOrder` を直列化 (account-wide entry serialization)。auto / manual / per-symbol 全経路 |
| `closeMu` | `*sync.Mutex` (cmd/bot/main.go で生成、全 bundle 共有) | `ManageOpenPositions.closeOne` + `ClosePositionCommand.Execute` の `broker.ClosePosition` |
| `advisorFireMu` | `*sync.Mutex` (cmd/bot/main.go closure) | scheduler tick の fan-out fire と `/api/advisor/trigger` 手動 fire を直列化 |
| `Aggregator.spreadMu` | `sync.Mutex` (worker 内) | 24h spread サンプル append (priceTick: writer / advisor: reader) |
| `ActiveConfigHolder.mu` | `sync.RWMutex` (app/active_config.go) | 現在の active config の atomic read/write |
| `llmCycleRunning` | `atomic.Bool` (cmd/bot/loops.go) | LLM 判断サイクル (定期 tick / 手動 trigger / event 再判断) の重複実行を防ぐ CAS |

### Mutex を取る箇所のルール

- broker 操作 ([port.Broker.PlaceOrder] / [port.Broker.ClosePosition]) は **必ず対応する mutex 配下**
- mutex の **取得→解放** は 1 関数内で閉じる (defer)
- mutex 内で外部 I/O が長時間ブロックしないように context timeout を付ける ([safety/timeouts.go](../../backend/internal/safety/timeouts.go))

---

## 3. Counters (= Live 観測指標)

[backend/internal/app/counters.go](../../backend/internal/app/counters.go) に `atomic.Int64` で集約。

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

`/api/status` で `CountersSnapshot` 経由で expose ([OBSERVABILITY.md](OBSERVABILITY.md) 参照)。

新規 Live 失敗パターンを観測したくなったらここに追加 + `Snapshot()` に反映する。

---

## 4. Runtime ファイル (永続化されないが重要)

| パス | 役割 |
|---|---|
| `runtime/emergency_stop.flag` | trip 時に reason 文字列が append される。worker / handler が `safety.Active(flag)` で読む |
| `runtime/ai_input/latest_summary.json` | minuteLoop が 1 分ごとに書く市場サマリ。Claude advisor の入力 |
| `runtime/ai_output/<run_id>.yaml` | (legacy / 検証用) Claude が生成した raw YAML 履歴 |
| `runtime/logs/llm_decisions.jsonl` | LLM 決定ループの判断ジャーナル (append-only、1 行 1 サイクル。event:cycle / parse_fallback) |
| `runtime/playbook_<SYMBOL>.jsonl` | LLM 決定ループの playbook (append-only。判断時は最新行を読む。`reflection_enabled` 時は Reflexion 反省ループが追記、false なら人が追記した行だけ) |
| `runtime/llm_decision_status.json` / `runtime/advisor_v2_status.json` | 直近サイクルの symbol 別結果 (ダッシュボード表示用) |
| `runtime/logs/*` | hook / debug 用ログ (`pre-stop-checks.log` / `docs-sync-check.log` / `make start` の `bot_stdout.log` 等) |

これらは `.gitignore` に入っている (= リポジトリには含めない)。

---

## 5. メモリ状態 (process 内のみ)

| 何 | どこ | 役割 |
|---|---|---|
| `app.ActiveConfigHolder` | メモリ | 現在の active config を atomic に保持 (sync.RWMutex 経由) |
| `app.Counters` | メモリ | atomic.Int64 で集約 |
| `app.Aggregator` | メモリ | 1m/5m/15m/1h の rolling candle buffer (24h 分) |
| `priceTick の spreadHistory` | worker 内 | 直近 24h の spread サンプル (advisor prompt 入力) |
| `consecutiveTickerErrors` | worker 内 (single-goroutine) | 連続 ticker 失敗カウント。3 回で WARN |
| `entryAllowedAt` | worker 内 (single-goroutine) | ticker 復旧直後の新規エントリ停止期限 |

---

## 6. 起動 / 終了 シーケンス

詳細は [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) §7 (Bootstrap)。
シャットダウンは `<-ctx.Done()` → `wg.Wait()` で全常駐 goroutine が落ちるのを待つ。

graceful shutdown が遅れる主因:

- HTTP server: in-flight リクエストの完了待ち (max 30s default)
- AdvisorCycle / LLM 判断サイクルが走行中: Claude CLI subprocess が timeout までブロック
- priceTick の broker call が timeout 内に終わらない

---

## 7. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md) — 設計契約の入口
- [SYSTEM_DESIGN.md](SYSTEM_DESIGN.md) — 起動シーケンスとフロー
- [OBSERVABILITY.md](OBSERVABILITY.md) — Counters + `/api/status`
- [FAILURE_MODES.md](../architecture/FAILURE_MODES.md) — 各 goroutine の失敗時挙動
- [CONFIG.md](CONFIG.md) — 起動時に読む config ファイル

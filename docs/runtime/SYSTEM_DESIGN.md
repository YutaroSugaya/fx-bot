# fx-bot System Design

このドキュメントは「**何が、どんな順番で、どんな依存関係で動くか**」を図中心でまとめた運用 / 開発の入口。
設計ルール (層の契約、PR チェックリスト) は [ARCHITECTURE.md](../ARCHITECTURE.md)、
DB スキーマは [DATA_MODEL.md](DATA_MODEL.md) を参照。

---

## 1. 全体ユースケース図

```
                    ┌─────────────────────────────────────┐
                    │           User / Operator           │
                    └─┬──────┬───────────┬────────┬───────┘
                      │      │           │        │
                      ▼      ▼           ▼        ▼
              [Dashboard] [手動trade] [手動advisor] [緊急停止]
              (Next.js)    button     trigger      flag
                      │      │           │        │
        ┌─────────────┴──────┴───────────┴────────┴────────────┐
        │ Bot API Server (Go)  /api/* HTTP + BasicAuth          │
        └─┬──────────┬─────────────┬─────────────┬──────────────┘
          │          │             │             │
          ▼          ▼             ▼             ▼
       [Query]   [Command]    [Command]      [Safety]
       系                                     (emergency_stop)
          │          │             │             │
          ▼          ▼             ▼             │
      ┌────────────────────────────────────────┐ │
      │             Usecase Layer              │ │
      │  TradingCycle / ExecuteOrder /         │ │
      │  ClosePosition / ManualTrade /         │ │
      │  PromoteConfig / Reconcile / AdvisorCycle│ │
      └─┬────────────┬──────────────┬──────────┘ │
        │ Port       │ Port         │ Port       │
        ▼            ▼              ▼            ▼
   ┌──────────┐ ┌────────────┐ ┌──────────┐ ┌──────────┐
   │ Postgres │ │ GMO Forex  │ │ Claude   │ │ Notifier │
   │ (pgxpool)│ │   API      │ │   CLI    │ │ (stdout) │
   └──────────┘ └────────────┘ └──────────┘ └──────────┘

                    ┌─────────────────────────────────────┐
                    │  Bot 内部 goroutine (主なもの)        │
                    │   ① priceLoop  1秒    (ticker pull → │
                    │      Aggregator → ManageOpenPositions │
                    │      → TradingCycle)                  │
                    │   ② minuteLoop 1分    (summary 書出し │
                    │      + candle UPSERT)                 │
                    │   ③ scheduler  interval_minutes 毎    │
                    │      (Claude advisor fire。opt-in)    │
                    │   ④ apiServer  常時    (HTTP listen) │
                    └─────────────────────────────────────┘
```

(③ の advisor、§6.5 の LLM 決定ループ (`runLLMDecisionScheduler` / `runReflectionScheduler`)、
advisor_v2 はいずれも opt-in で、tracked の `configs/bot_config.yaml` では全部 off。その場合の
エントリー判断は、DB に seed した active config ([CONFIG.md §4.1](CONFIG.md)) に従って ① の
TradingCycle が決定論で行う。並行性の詳細は [RUNTIME.md](RUNTIME.md))

---

## 2. 主要アクター

| アクター | 役割 |
|---|---|
| **Claude (advisor / LLM 決定ループ)** | どちらも opt-in。advisor は過去 1h/6h/24h の市場サマリと bot 状態を見て次の戦略 YAML を生成する (promote されるまでは候補)。LLM 決定ループ (§6.5) は毎サイクル trade / no_trade を判断する。発注は常に決定論の risk Gate + broker OCO 経由。 |
| **Bot (worker goroutines)** | 1 秒ごとに ticker を引いて aggregator に流す + 既存ポジを管理 + 新規エントリ判断する。Active config に従って trade する。 |
| **GMO Coin API** | Public (ticker / klines) + Private (発注 / 約定 / ポジ参照)。Live モードのみ実お金。 |
| **Operator (人間)** | Dashboard から手動 trade / 手動 advisor / 緊急停止 / 結果確認。 |
| **Postgres** | strategy_configs / positions / trades / signal_rejections / candles 等の永続化。 |

---

## 3. レイヤー構成 (詳細)

```
┌────────────────────────────────────────────────────────┐
│ cmd/bot/main.go (+ api_wire.go + loops.go)             │ ← Composition Root
│   - 環境変数 + config + DB pool                         │
│   - 全 usecase / handler の wiring                      │
│   - goroutine 起動 (runLoops)                           │
└────┬───────────────────────────────────────────────────┘
     │
     ▼
┌────────────────────────────────────────────────────────┐
│ app/ (HTTP + worker + counters)                         │
│   - handler/        HTTP endpoint (薄い、usecase 直呼)  │
│   - api_server.go   route 登録 + auth + CORS            │
│   - worker.go       priceLoop / minuteLoop (I/O 役)     │
│   - scheduler.go    advisor の定期 fire (interval_minutes)│
│   - counters.go     atomic.Int64 で /api/status に露出  │
│   - live_guard.go   Live 二重ロック (env 不足は paper へ) │
│   - active_config.go  in-memory active config           │
└────┬───────────────────────────────────────────────────┘
     │
     ▼
┌────────────────────────────────────────────────────────┐
│ usecase/                                                │
│   - command/  (状態変更)                                │
│     • TradingCycle  ← 1 tick の決定パス                 │
│     • ExecuteOrder, ManualTradeCommand                  │
│     • ClosePositionCommand, ManageOpenPositions         │
│     • Promoter (config promote 1 Tx)                    │
│     • Reconcile (起動時 broker→DB sync)                 │
│     • AdvisorCycle (Claude 呼び出し → promote 全フロー)  │
│     • LLMDecisionCycle / ReflectionCycle / ArmedFire    │
│   - query/    (読み取り専用)                            │
│     • ListOpenPositions, ListTrades, ListRecentDecisions│
│     • GetBotStatus, GetMarketState, AskClaude           │
└────┬───────────────────────────────────────────────────┘
     │ port (interface)
     ▼
┌────────────────────────────────────────────────────────┐
│ domain/                                                 │
│   - market/    Ticker / Candle / Aggregator / Resample  │
│   - strategy/  Engine + 登録戦略 (momentum_pullback /    │
│                ma_pullback / trend_follow 等)。Signal を │
│                返す純粋関数                               │
│   - position/  ComputeTPSLPrices / ComputeClosePnL / State│
│   - order/     PlaceOrderRequest / Order / Execution     │
│   - risk/      Gate (cooldown / max_open_positions /     │
│                daily_loss / consecutive_losses 判定)      │
└────────────────────────────────────────────────────────┘
     │ port
     ▼
┌────────────────────────────────────────────────────────┐
│ adapter/                                                │
│   - broker/        GmoBroker (REST) / PaperBroker / Mock │
│   - repository/    pgx + sqlc を使った具体実装           │
│   - notifier/      Stdout (外部通知は adapter 追加で対応) │
│   - journal/ playbook/  LLM 判断ジャーナル / playbook     │
│   - advisor/       ClaudeCLIAdvisor (claude -p を 1 回  │
│                    起動 + 出力から YAML 取り出し)         │
└────────────────────────────────────────────────────────┘
```

依存方向: 上から下のみ (循環禁止)。port は逆向き interface だけ通る。

---

## 4. メインフロー: エントリーから決済まで (paper モード)

```
┌─ priceLoop (1秒間隔) ───────────────────────────────────────────┐
│                                                                 │
│ ① Broker.GetTicker(symbol)                                      │
│      └─ 失敗 → consecutive_errors++ / WARN ログ / return         │
│ ② Aggregator.OnTick(ticker)                                     │
│      └─ 1m/5m/15m/1h bar を内部 buffer に積む                    │
│ ③ recordSpread(ticker.SpreadPips) — 24h rolling                 │
│ ④ ManageOpenPositions.OnTick(ticker)                            │
│      ├─ List OPEN positions                                     │
│      └─ for each: evaluateExit → 'take_profit'/'stop_loss'/     │
│         'max_hold'/ratchet 等 → closeOne → close saga (§5:      │
│         CAS で CLOSING → broker close → CloseAndRecord)         │
│ ⑤ Active config 取得 (ActiveConfigHolder.Get)                   │
│ ⑥ AccountSnapshot 構築 (DB から daily loss / consec losses /    │
│    in-cooldown 等を集計)                                         │
│ ⑦ BuildMarketSummary (history candles + ticker + bot state)     │
│ ⑧ TradingCycle.Execute ─────────────────────────────────────┐   │
│   ⑧a EvaluateEntry                                          │   │
│      ├─ strategy.Engine.Evaluate (IsActive + IsHourAllowed   │   │
│      │  + strategy 固有ロジック)                              │   │
│      └─ risk.Gate.EvaluateSignal (cooldown・同方向・損失 cap 等)│   │
│        └─ reject → signal_rejections INSERT                  │   │
│   ⑧b ExecuteOrder.OnSignal (gate allowed のみ)                │   │
│      ├─ EntryMutex.Lock (auto と manual の衝突防止)            │   │
│      ├─ Broker.PlaceOrder (Paper/Live: MARKET)               │   │
│      ├─ Live のみ: ResolveExecution で positionId と fill   │   │
│      │  価格を解決 (失敗時 emergency_stop trip)                │   │
│      ├─ Live のみ: GMO に TP/SL の OCO 決済注文を設定          │   │
│      └─ Positions.Insert (失敗時も emergency_stop trip)       │   │
│ ───────────────────────────────────────────────────────────┘   │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

---

## 5. 決済フロー (close saga)

bot 側の決済 (MaxHold / ratchet / 朝の一斉手仕舞い / paper の TP・SL / 手動決済) はすべて
`ExecuteCloseSaga` ([close_saga.go](../../backend/internal/usecase/command/close_saga.go)) を通る。
live で broker の OCO が先に約定した場合は、bot ではなく reconcile が実約定を解決して記録する。

```
ManageOpenPositions.closeOne / ClosePositionCommand.Execute
│
├─ CloseMutex.Lock (priceLoop と手動決済 API の二重決済防止)
│
├─ ⓪ quote→JPY レートの解決 (USD-quote ペアのみ。失敗なら何もせず OPEN のまま次 tick で再試行)
├─ ① ClaimForClose: positions を OPEN → CLOSING に CAS (取れなければ何もしない)
├─ ② Live のみ: 記録済みの TP/SL 脚を TP → SL の順に cancel (途中で失敗しても SL が残る順序)
│     記録が無ければ broker から脚を探す。cancel 失敗 → emergency_stop trip
├─ ③ Broker.ClosePosition
│     ├─ broker 側に建玉が無い (GMO ERR-254 = OCO が先に約定) → skip。CLOSING のまま reconcile が記録
│     ├─ Live で拒否 + 脚を cancel 済み → 元の TP/SL で OCO を置き直す (失敗なら emergency_stop trip)
│     ├─ Live で拒否 + 脚を 1 本も cancel していない → 元の OCO が生きているので skip
│     │   (Live で拒否された 2 経路では行は CLOSING のまま残り、bot 側の出口 (MaxHold / ratchet /
│     │    朝の手仕舞い) はその玉にもう効かない。決済は OCO の約定を reconcile が記録する)
│     └─ Paper で失敗 → emergency_stop trip
├─ ④ Live のみ: ResolveExecution で実約定価格・手数料・スワップを取る (失敗 → emergency_stop trip)
├─ ⑤ Closer.CloseAndRecord  ← 1 Tx
│     ├─ position_state_events (CLOSED) + positions.status='CLOSED' (CLOSING からのみ)
│     ├─ trades INSERT
│     └─ ok=false (CLOSING でない) → close_race_double_broker_call → emergency_stop trip
│
└─ Logger.Info("position_closed", pnl_jpy=...)
```

**MaxHold 延長ポリシー** (非対称):
- elapsed < soft (= MaxHoldMinutes): 通常通り open
- soft ≤ elapsed < hard (= soft + ExtensionMaxMinutes) AND unrealized_pips ≥ −threshold (勝ち / フラット): 待つ
- それ以外 (hard 到達、または含み損が threshold を超えた): max_hold で close

---

## 6. Claude advisor フロー (scheduler 経路)

(scheduler は `ai_advisor.enabled: true` のときだけ動く。既定は off。手動の `POST /api/advisor/trigger` は `enabled` に関係なく claude を呼ぶ)

```
[scheduler goroutine] ai_advisor.interval_minutes ごと (または active config の next_advisor_run_in_minutes)
       │
       ▼
fireAdvisors(ctx, source) (scheduler) / triggerAdvisor(ctx, symbol) (手動 API)
       │   — どちらも cmd/bot/main.go のクロージャで、bundle ごとに runBundleAdvisor を呼ぶ
       │
       ├─ advisorFireMu.Lock (scheduler tick と手動 trigger を直列化)
       │
       ├─ AdvisorCycle.Run(ctx, source)
       │   ├─ ① Worker.SnapshotForAdvisor で bot_state 構築
       │   ├─ ② Aggregator から最新 candles 取得 + BuildMarketSummary
       │   ├─ ③ market_summaries に INSERT (DB に書くのはこの経路だけ。
       │   │      latest_summary.json は minuteLoop が書く)
       │   ├─ ④ ClaudeCLIAdvisor.Generate(ctx, summary)
       │   │      └─ claude -p を 1 回 (timeout = claude_cli_timeout_seconds)。
       │   │         root が 4 subagent (skill 01〜04) を並列起動し、
       │   │         統合後に skill 07 → 05 → 06 を自分で読んで YAML 出力。
       │   │         skill 08 / 09 は使わない
       │   ├─ ⑤ ai_advisor_runs INSERT (status / source / duration)
       │   └─ ⑥ Promoter.PromoteFromYAML(raw, accountState, source)
       │       ├─ Validate (schema / hard_limit / semantic / risk)
       │       ├─ 失敗: rejected 行 INSERT + validation_events INSERT × 4
       │       │       (semantic も persist)
       │       └─ 成功: ConfigPromoter.PromoteActive (1 Tx で旧 expire +
       │              新 insert)。partial unique index でも保護。
       │              YAML 書き込みは best-effort (DB が SoT)
       │
       └─ Promoted=true → holder.Set(parsed) — 全 goroutine に伝播
```

**手動経路** (`POST /api/advisor/trigger`): `triggerAdvisor` が source="manual" で同じ `runBundleAdvisor` を呼ぶ。
**緊急停止**: advisor (と advisor v2) は trip 中も claude を呼び、出てきた新規は risk Gate が `safety.Active(emergency_stop.flag)` で拒否する。
サイクル冒頭で止まって LLM を呼ばないのは LLM 判断ループだけ (§6.5)。

---

## 6.5 LLM 決定ループ (opt-in)

`llm_decision.enabled: true` のときだけ動く (tracked の `configs/bot_config.yaml` には `llm_decision` セクションが無い = off)。
advisor のように config YAML を生成するのではなく、LLM が毎サイクル直接 trade / no_trade を判断する:

1. `runLLMDecisionScheduler` ([backend/cmd/bot/loops.go](../../backend/cmd/bot/loops.go)) が `interval_minutes` (既定 60 分) 毎に各 symbol の判断サイクル (`LLMDecisionCycle`) を起動する。
   休場中は LLM を呼ばずに skip。定期 tick / 手動 trigger (`/api/llm-decision/trigger`) / event 再判断 (`event_retrigger`) は同じ経路を通り、`llmCycleRunning` で重複実行しない
2. サイクル冒頭の決定論ゲート: `runtime/emergency_stop.flag` があれば stage `emergency_stop` で終了 (LLM を呼ばない)。
   続いて `exclude_hours_jst` / `no_entry_hours_jst` / `daily_loss_stop_count` も LLM を呼ぶ前に判定する
3. `BuildMarketSummary` → `claude -p` (single-agent。[llm_decision_cli.go](../../backend/internal/adapter/advisor/llm_decision_cli.go) の `BuildSingleAgentDecisionPayload` = Go 内テンプレート + playbook `runtime/playbook_<SYMBOL>.jsonl` + MarketSummary JSON) で trade / no_trade + side + TP/SL を判断
4. コード側 veto 群を通過したら、
   既存の発注経路 `OnSignal` → risk Gate → broker OCO で発注する。数量は config で固定 (LLM は決めない)。
   veto はそれぞれ: night_buy (指定した JST 時間の BUY) / chase_buy (24h レンジの上端での BUY = 高値追い) /
   sell_low (24h レンジの下端での SELL) / htf_trend (24h の動きに逆らう向き) / exhaustion (24h で既に大きく動いた向きへの追随) /
   spike (15 分で急変している最中) / wide_spread (`max_spread_pips` 超え) / max_concurrent (同方向の建玉数の上限) を拒否する。
   `arm_enabled` 時は条件付きエントリー計画を置き、`ArmedFire` が tick ごとに veto を再検証してから発火する
5. 保有中は通常の出口 (broker OCO の TP/SL・ratchet = 含み益が arm pips に届いたら追跡し、ピークから giveback pips 戻ったら成行で利確するトレーリング出口・MaxHold) に加え、`session_flatten_jst` 設定時は毎朝の全玉手仕舞いが効く
6. `reflection_enabled` 時 (省略時 true) は `runReflectionScheduler` が Reflexion 反省ループ ([reflection_cli.go](../../backend/internal/adapter/advisor/reflection_cli.go)、reflection-{regime,risk,strategy} の 3 subagent) を回し playbook に改訂版を追記する。false なら playbook は人が追記したものだけ
7. 全サイクルの判断は `runtime/logs/llm_decisions.jsonl` (event:cycle / parse_fallback) に append される判断ジャーナルで監査可能

キーの意味・既定値は `bot_config.go` の `LLMDecisionSection` が正 ([CONFIG.md §2.1](CONFIG.md))。

---

## 7. 起動フロー (Bootstrap)

```
main()  →  run()  in cmd/bot/main.go
│
├─ ① 環境: .env load + Logger + runtimePaths
├─ ② config load: bot_config.yaml + hard_limits.yaml
├─ ③ ctx + SIGINT/TERM 監視
├─ ④ DB connectDB (pgxpool)
├─ ⑤ broker setup:
│     - publicBroker (ticker / klines 用 = Live でも paper でも同じ GMO public)
│     - execBroker = paperBroker または GmoBroker (mode + env チェック)
│     - Live には mode=live_config + LIVE_TRADING_ENABLED=true +
│       LIVE_CONFIRM_SYMBOLS が必要。env が欠ければ paper に降格 (WARN ログ)
│     - GMO_API_KEY/SECRET が無ければ起動エラー (数量上限は hard_limits.yaml が SSOT)
├─ ⑥ paperBroker のみ: restorePaperPositions で DB の OPEN を in-memory に復元
├─ ⑦ Promoter / Validator wiring
├─ ⑧ Aggregator + candle 復元 (DB → GMO klines の順。1m/5m/15m は 24h、1h は 14 日。
│     両方とも 0 本なら emergency_stop trip)
├─ ⑨ Reconcile.Run (startup mode):
│     - broker にあって DB にない → adopt (recovered_positions に記録。
│       Live は external_broker = bot は触らない)
│     - DB にあるが broker にない → paper は ⑥ で OPEN を戻しているので、
│       CLOSING のまま残った行だけ synthetic close (reconcile_cold_close)、
│       Live は実 fill を解決して記録 (まだ解決できなければ runtime reconcile に
│       回す = 猶予付きで再試行し、hard window を超えたら emergency_stop)
│     - reconcile がエラーを返す / emergency_stop を trip した → Live は起動中断
├─ ⑩ holder = ActiveConfigHolder
├─ ⑪ Promoter.LoadActiveFromDB (symbol ごとに DB の active 行を読み、起動時検証を掛ける)
│     DB 失敗 / parse・検証失敗時に YAML へ fallback しない。
│     Live は起動中断 (fail-closed)、paper は active なしで起動 (何も建てない)。
│     active config は起動時にしか読まれない ([CONFIG.md §4.1](CONFIG.md))。
├─ ⑫ TradingCycle 組み立て + Worker 構築 (per-symbol bundle で N 個)
├─ ⑬ AdvisorCycle + fireAdvisors (advisorFireMu) + triggerAdvisor クロージャ
├─ ⑭ closeCmd / manualCmd (sharedEntryMu / closeMu 共有)
├─ ⑮ counters + safety.SetOnTrip (trip 時カウンタ increment)
├─ ⑯ wireHTTPHandlers (api_wire.go で全 handler 構築)
├─ ⑰ configureScheduler (active config の next_advisor_run_in_minutes 反映)
├─ ⑱ runLoops (loops.go で goroutine 起動 + ctx 待機)
│      ├─ priceLoop / minuteLoop (symbol ごと) + reconcileLoop (Live のみ)
│      ├─ scheduler (ai_advisor) / advisor_v2 / llm_decision / llm_reflection
│      │    (opt-in。無効なら起動ログだけ出して終了)
│      └─ apiServer
└─ <-ctx.Done() → graceful shutdown (wg.Wait)
```

---

## 8. 失敗時の挙動 (= 絶対ルール)

| 障害 | 反応 |
|---|---|
| Ticker fetch 連続失敗 | counters.TickerErrors++、3 回で WARN、復旧後 10 秒は新規エントリ停止 |
| Broker.PlaceOrder 失敗 | error 返却。DB に何も書かない。次 tick で再評価 |
| Broker 成功 + ResolveExecution 失敗 (Live) | **emergency_stop trip** (= "execute_resolve_execution_failed")。reconciler が裸ポジ拾う |
| Broker 成功 + Position.Insert 失敗 | **emergency_stop trip** (= "execute_position_insert_failed") |
| Live close で cancel 失敗 | **emergency_stop trip** (= "cancel_settle_leg_failed_before_close")。TP は cancel 済みでも SL が残る順序設計 |
| Live close を broker が拒否 (脚 cancel 後) | 元の TP/SL で OCO を置き直す。置き直しにも失敗したら **emergency_stop trip** (= "close_position_failed_after_settle_cancel") |
| CloseAndRecord で ok=false (CLOSING でない) | **emergency_stop trip** (= "close_race_double_broker_call") |
| DB が一時的に落ちる | priceLoop の accountSnapshot エラーで「その tick はスキップ」(loss を 0 と誤認しない) |
| 起動時に broker と DB がズレてた | Reconcile が揃える。broker にだけある玉は adopt (Live は external = 不可侵)、DB にだけある玉は paper なら CLOSING のまま残った行だけ synthetic close (OPEN 行は起動時に PaperBroker へ戻す)、Live は実 fill を解決して記録 (まだ解決できなければ runtime reconcile が猶予付きで再試行し、hard window 超過で **emergency_stop trip**。0 円の架空決済は記帳しない) |
| 起動 mode の active config が DB に無い | paper / live とも起動を続けるが、そのシンボルは何も建てない (ログは出ない。LLM 判断ループは stage `no_active_config`)。active 行の読込・検証に失敗すると paper は WARN で続行、Live は起動中断 |
| `runtime/emergency_stop.flag` がある | 新規エントリー全停止。LLM 判断ループは LLM を呼ばずに stage `emergency_stop` で終わる。既存玉の出口 (OCO / ratchet / MaxHold) は動く |

---

## 9. 主要な並行性制御

| Mutex | 何を直列化 |
|---|---|
| `sharedEntryMu` | 全 symbol 横断で ExecuteOrder.OnSignal + ManualTradeCommand.Execute + EntryAdmission を直列化 (account-wide) |
| `closeMu` | ManageOpenPositions.closeOne + ClosePositionCommand.Execute の broker.ClosePosition |
| `advisorFireMu` | scheduler tick fan-out + `/api/advisor/trigger` 手動 fire + `BOT_DEBUG_FORCE_ADVISOR` を直列化 |
| `Worker.spreadMu` | 24h spread サンプル append (priceTick: writer / advisor: reader)。bundle 内 |
| (mutex なし) `ActiveConfigHolder` | symbol→config を `sync.Map` で保持 (per-symbol active config) |

---

## 10. 設定ファイル

| ファイル | 役割 |
|---|---|
| `configs/bot_config.yaml` | bot mode / symbols / scheduler / advisor・LLM ループ設定 / risk caps (tracked の安全側既定) |
| `configs/bot_config.live.yaml` | live 用 bot_config (gitignore。`BOT_CONFIG_PATH` で指定) |
| `configs/hard_limits.yaml` | strategy config が出せる値の絶対上下限 (quantity / TP / SL / max_hold / per-trade 損失 etc.) |
| `configs/strategy_config.active.yaml` | DB と同期する artifact (read-only on boot — DB が SoT) |
| `configs/strategy_config.next.yaml` | Claude が直近に書き出した提案 (validator 待ち。symbols が 2 つ以上なら `strategy_config.next_<SYMBOL>.yaml`) |
| `runtime/emergency_stop.flag` | trip 中は新規エントリ全停止 |
| `runtime/ai_input/latest_summary.json` | 1 分ごとに更新される market summary (Claude 入力。symbols が 2 つ以上なら `latest_summary_<SYMBOL>.json`) |
| `.env` | DATABASE_URL / GMO_API_KEY / LIVE_* / DASHBOARD_USER/PASS など |

---

## 11. 開発時に「どこに何があるか」

| やりたいこと | 開く場所 |
|---|---|
| 新しい strategy 実装 | `internal/domain/strategy/` に新ファイル + `engine.go` の Register |
| 新しい risk gate 追加 | `internal/domain/risk/gate.go` |
| 新しい HTTP endpoint | `internal/app/handler/` に新 handler + `cmd/bot/api_wire.go` で wire |
| 新しい Query DTO | `internal/usecase/query/` |
| schema 変更 | `backend/migrations/000N_xxx.up.sql` + down.sql + `port.PositionRecord` 等の更新 |
| Live mode の env 制御 | `internal/app/live_guard.go` |
| Claude prompt 調整 | `prompts/generate_strategy_config.md` + `prompts/skills/*.md` (advisor) / `internal/adapter/advisor/llm_decision_cli.go` (LLM 決定ループ) |
| active config を DB に入れる | `scripts/seed_active_config.sh` (検証は `cmd/config-check`。[CONFIG.md §4.1](CONFIG.md)) |
| Backtest 走らせる | `cmd/backtest` / `cmd/sweep` / `cmd/edge-judge` (手順は [BACKTEST.md](../workflows/BACKTEST.md)) |
| Candles 取得 | `cmd/fetch-candles/main.go` (GMO klines → DB) / `cmd/histdata-ingest` (HistData M1 CSV → backtest DB) |

---

## 12. 関連ドキュメント

- [ARCHITECTURE.md](../ARCHITECTURE.md) — 層の契約 + PR チェックリスト + テスト戦略 (古典派 + table-driven)
- [DATA_MODEL.md](DATA_MODEL.md) — DB スキーマ詳細
- [OPERATIONS_RUNBOOK.md](OPERATIONS_RUNBOOK.md) — 不変条件 / ハマりポイント / ロールバック手順
- [../../CLAUDE.md](../../CLAUDE.md) — 不変条件 (Live で broker-side OCO 必須 / config snapshot on position 等)

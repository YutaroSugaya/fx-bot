# Failure Modes — Rollback > Fallback / Tx / Mutex

エラー発生時の挙動の絶対ルール。運用フロー側の要約は [SYSTEM_DESIGN.md](../runtime/SYSTEM_DESIGN.md) §8。

**ルール: 失敗時は "log + continue" ではなく "rollback / emergency_stop trip / DEFER"。**

> **DEFER** は第 3 の正式カテゴリ。
> DEFER = 「今は解決不能だが破壊もしない。grace/hard window 付きで次パスに再試行し、
> hard window 超過で初めて trip」(例: 起動時 reconcile の stale DB position は、entry saga の
> race-window と区別できないため即 trip せず DEFER する)。
> DEFER は "log+continue" とは異なり ①状態を破壊しない ②上限 (hard window) があり最終的に trip する
> ③発生をカウント/ログする。無条件の握り潰しは引き続き禁止。
>
> Live の runtime reconcile の時間定数 (`backend/cmd/bot/loops.go`): 間隔 30 秒 / stale DB position の grace 90 秒 /
> その後の hard window 10 分 / external 建玉 adopt の grace 3 分。

---

## 1. Rollback > Fallback の原則

エラー発生時:

- **悪い**: log して continue / 不完全な状態で先に進む
- **良い**: 途中で作ったリソースを巻き戻す or emergency_stop を発火する

### 具体例

| 状況 | 正しい挙動 |
|---|---|
| DB Tx 失敗 | ROLLBACK が走ることを確認 (`defer tx.Rollback`) |
| Broker 注文成功 + DB 失敗 | **emergency_stop trip** (broker 側を巻き戻せないので人手介入を強制) |
| ResolveExecution timeout (Live) | 詳細未確定で DB に書かない + **emergency_stop trip** |
| Live close で cancel 失敗 | **emergency_stop trip** (= `cancel_settle_leg_failed_before_close`)。TP は cancel 済みでも SL が残る順序設計 |
| CloseAndRecord で ok=false (CLOSING でない) | **emergency_stop trip** (= `close_race_double_broker_call`) |
| 起動時 reconcile failure (Live) | **起動中断** (`main.go` が `return` する = fail-closed) |

---

## 2. Transaction が必要なケース

複数テーブル更新 / Broker と DB の同期 / 状態遷移を含むものは Tx 必須:

| ケース | Tx 内に閉じる操作 |
|---|---|
| ポジ決済 | `positions UPDATE status='CLOSED'` + `trades INSERT` (CloseAndRecord) |
| Config promote | `strategy_configs UPDATE expired` + `strategy_configs INSERT active` (PromoteActive) |
| Signal reject | `signal_rejections INSERT` + entry スキップ (Tx 内一貫性) |

### Tx 内では外部 API を呼ばない

Tx 中に GMO REST 呼び出しを入れると、API が遅延した場合に行ロックと pool の接続を長時間保持してしまう (advisory lock は使っていない)。
broker call → DB Tx の順、または DB Tx → broker call → DB compensation の Saga にする。

---

## 3. Mutex が必要なケース

複数 goroutine が同じリソースを変更する可能性があるとき:

| Mutex | 直列化する組 |
|---|---|
| `sharedEntryMu` | 全 symbol 横断で `ExecuteOrder.OnSignal` + `ManualTradeCommand.Execute` + `EntryAdmission` を直列化 (account-wide entry serialization) |
| `closeMu` | `ManageOpenPositions.closeOne` + `ClosePositionCommand.Execute` (全 symbol 共有) |
| `advisorFireMu` | scheduler tick の fan-out fire + `/api/advisor/trigger` 手動 fire + `BOT_DEBUG_FORCE_ADVISOR` |

詳細は [RUNTIME.md](../runtime/RUNTIME.md) §2。

---

## 4. 各障害シナリオ — bot の反応

| 障害 | 反応 |
|---|---|
| Ticker fetch 連続失敗 | `Counters.TickerErrors++`、3 回で WARN、復旧後 10 秒は新規エントリ停止 (`TickerRecoveryCooldown`) |
| Broker.PlaceOrder 失敗 | error 返却。DB に何も書かない。次 tick で再評価 |
| Bootstrap で candle 復元失敗 | **emergency_stop trip** (= `candle_restore_failed`)。bot は起動を続けるが entry gate が block |
| 起動時 reconcile (paper) | trip しない。adopt / MarkClosed で揃える |
| Reconcile で naked broker pos 検出 (Paper runtime) | **emergency_stop trip** (= `reconcile:naked_broker_position:<broker_position_id>`)。paper の broker は PaperBroker なので bot のバグ扱い。fail-closed |
| Reconcile で naked broker pos 検出 (Live startup / runtime) | trip しない。`external_broker` として取り込む (bot は管理しない・同方向の新規は止める)。entry saga 進行中の玉 (`PendingPositions`) は skip |
| Reconcile で active config なし (adopt 不可) | **emergency_stop trip** (= `reconcile:adopt_no_active_config:<broker_position_id>`) |
| Reconcile adopt 中の DB Insert 失敗 | **emergency_stop trip** (= `reconcile:adopt_insert_failed:<broker_position_id>`) |
| Reconcile で external (= bot 以外で建てた) 建玉 adopt 中に active config なし | runtime は **DEFER → grace (3 分) 超過で trip** (= `reconcile:external_adopt_no_active_config:<broker_position_id>`)。起動時の reconcile は grace 0 なので即 trip し、Live は起動を中断する |
| Reconcile で external 建玉 adopt 中の DB Insert 失敗 | **emergency_stop trip** (= `reconcile:external_adopt_insert_failed:<broker_position_id>`) |
| 起動時 reconcile で stale DB pos 解決不可 (Live) | **DEFER → hard window 超過で trip** (= `reconcile:stale_db_position:<id>`)。即 trip しないのは entry saga の race-window と区別できないため |
| 起動時 reconcile で closer 未配線 (Live) | **emergency_stop trip** (= `reconcile:startup_no_closer_live:<id>`) |
| 起動時 reconcile で closer 未配線 (Paper) | **emergency_stop trip** (= `reconcile:startup_no_closer:<id>`) |
| Reconcile ClaimForClose 失敗 | **emergency_stop trip** (= `reconcile:claim_for_close_failed:<id>`) |
| Runtime reconcile で broker 消失 (DB は OPEN) | TP/SL 脚の実約定を解決して記録。grace 中は DEFER、grace 後は現在値が OCO 水準を明確に越えていればその水準で推定記帳、それ以外は hard window 超過で **emergency_stop trip** (= `reconcile:stale_db_position:<id>`)。0 円の架空決済は作らない |
| Reconcile MarkClosed 失敗 | **emergency_stop trip** (= `reconcile:mark_closed_failed:<id>`) |
| **ExecuteOrder (Live)** ResolveExecution 失敗 | **emergency_stop trip** (= `execute_resolve_execution_failed`)。reconciler が裸ポジ拾う |
| ExecuteOrder (Live) ExecutionResolver 未配線 | **emergency_stop trip** (= `execute_no_resolver`) |
| ExecuteOrder (Live) SettleLegResolver 未配線 | **emergency_stop trip** (= `execute_no_settle_leg_resolver`) |
| ExecuteOrder (Live) OCOCloseOrderPlacer 未配線 | **emergency_stop trip** (= `execute_no_oco_placer`) |
| ExecuteOrder (Live) 約定の positionId が数値でない | **emergency_stop trip** (= `execute_broker_position_id_not_numeric`) |
| ExecuteOrder (Live) TP/SL の OCO が置けない | trip しない。成行で建玉を畳む (補償 close。WARN `live_exit_protector_compensated_close_ok`)。補償 close も失敗したら **emergency_stop trip** (= `execute_place_settle_oco_failed_and_compensate_close_failed`) |
| ExecuteOrder (Live) ResolveSettleLegs 失敗 | trip しない (soft)。OCO は GMO 側にあるので、脚の ID 無しで記録して続行する (WARN `execute_resolve_settle_legs_soft_failure`。決済時は close saga が broker から脚を探して cancel する) |
| Broker 成功 + Position.Insert 失敗 | **emergency_stop trip** (= `execute_position_insert_failed`) |
| **Close Saga** ClaimForClose DB エラー | **emergency_stop trip** (= `claim_for_close_db_failed`) |
| Close Saga GetLive DB エラー | **emergency_stop trip** (= `get_live_db_failed`) |
| Close Saga cancel settle leg 失敗 | **emergency_stop trip** (= `cancel_settle_leg_failed_before_close`)。TP cancel 済みでも SL 残し優先の順序設計 |
| Close Saga close position 失敗: broker に建玉が無い (GMO ERR-254) | trip しない。OCO が先に約定した良性のレース。CLOSING のまま reconcile が実約定を記録 |
| Close Saga close position 失敗 (Live・settle cancel 後) | 元の TP/SL で OCO を置き直す (trip しない)。置き直しにも失敗したら **emergency_stop trip** (= `close_position_failed_after_settle_cancel`。naked position) |
| Close Saga close position 失敗 (Live・脚を 1 本も cancel していない) | trip しない。元の OCO が生きている (置き直すと OCO が二重になる) |
| Close Saga close position 失敗 (Paper) | **emergency_stop trip** (= `close_position_failed`) |
| Close Saga ExecutionResolver 未配線 | **emergency_stop trip** (= `close_no_resolver`) |
| Close Saga ResolveExecution 失敗 | **emergency_stop trip** (= `close_resolve_execution_failed`) |
| Close Saga closer 未配線 | **emergency_stop trip** (= `close_no_closer_wired`) |
| Close Saga CloseAndRecord DB 失敗 | **emergency_stop trip** (= `close_and_record_db_failed`) |
| Close Saga CloseAndRecord ok=false (CLOSING でない) | **emergency_stop trip** (= `close_race_double_broker_call`) |
| **Manual Trade (Live)** ExecutionResolver 未配線 | **emergency_stop trip** (= `manual_no_resolver`) |
| Manual Trade (Live) ResolveExecution 失敗 | **emergency_stop trip** (= `manual_resolve_execution_failed`) |
| Manual Trade (Live) SettleLegResolver 未配線 | **emergency_stop trip** (= `manual_no_settle_leg_resolver`) |
| Manual Trade (Live) OCOCloseOrderPlacer 未配線 / positionId が数値でない | **emergency_stop trip** (= `manual_no_oco_placer` / `manual_broker_position_id_not_numeric`) |
| Manual Trade (Live) TP/SL の OCO が置けない | trip しない。補償 close で畳む。補償 close も失敗したら **emergency_stop trip** (= `manual_place_settle_oco_failed_and_compensate_close_failed`) |
| Manual Trade (Live) ResolveSettleLegs 失敗 | trip しない (soft)。脚の ID 無しで記録して続行する (WARN `manual_resolve_settle_legs_soft_failure`) |
| Manual Trade active config なし | **emergency_stop trip** (= `manual_no_active_config`) |
| Manual Trade position INSERT 失敗 | **emergency_stop trip** (= `manual_trade_position_insert_failed`) |
| 手動 API による emergency stop | **emergency_stop trip** (= `manual_via_api`) — `/api/emergency-stop` |
| DB が一時的に落ちる | `priceLoop` の accountSnapshot エラーで「その tick はスキップ」(loss を 0 と誤認しない) |
| Claude advisor timeout / parse_error | `ai_advisor_runs.status` に記録、元の active config を維持 (= 安全側に倒す) |
| Validator reject | rejected 行 INSERT + 4 validation_events INSERT、active config 不変 |
| TradingCycle == nil | `Logger.Error("trading_cycle_missing")` + `Counters.TradingCycleMissing++` + return (= その tick はスキップ) |

---

## 5. emergency_stop trip — 名前付け規約

`safety.Trip(path, reason)` の `reason` 文字列は **grep 可能** に統一:

- `<scope>_<action>_<state>` の 3 要素を `_` 区切り
- scope は `execute` / `close` / `manual` / `candle` / `manual` (via API) などの呼出元
- 例: `execute_position_insert_failed`, `close_race_double_broker_call`
- Reconcile は `reconcile:<variant>:<id>` の prefix 形式。variant は `naked_broker_position` / `adopt_no_active_config` / `adopt_insert_failed` / `external_adopt_no_active_config` / `external_adopt_insert_failed` / `stale_db_position` / `startup_no_closer_live` / `startup_no_closer` / `claim_for_close_failed` / `mark_closed_failed` のいずれか

新しい trip 経路を追加するときは:

1. 上記命名規約に従う
2. [safety.md](layers/safety.md) の「trip 経路」リストを更新
3. テストで `safety.Active(path) == true` を assert する

---

## 6. アンチパターン

- ❌ `if err != nil { log.Warn(...) }` だけで続行 (= silent fail-open)
- ❌ Tx 中で `broker.PlaceOrder` を呼ぶ (timeout まで行ロックと pool の接続を握り続ける)
- ❌ `defer tx.Rollback()` を忘れて、return 経由で Tx が leak
- ❌ Live close 失敗を `cancelled > 0` でだけ判定 (= 一部 cancel で「成功」と扱う罠)
- ❌ `recover()` で panic を呑んで同じ処理を続行させる (= 状態が不明で危険)。`recoverGoroutine` はログを残してその goroutine を終わらせるだけで、続行はしない

---

## 7. 関連 docs

- [ARCHITECTURE.md](../ARCHITECTURE.md)
- [SYSTEM_DESIGN.md](../runtime/SYSTEM_DESIGN.md) §8 — 障害一覧
- [layers/safety.md](layers/safety.md) — emergency_stop trip 経路
- [layers/usecase.md](layers/usecase.md) — usecase エラー処理
- [OBSERVABILITY.md](../runtime/OBSERVABILITY.md) — Counters / status

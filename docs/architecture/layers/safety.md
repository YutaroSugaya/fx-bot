# Layer: Safety

## 役割 (1 行)

emergency_stop / timeouts / pending position tracker / 共通定数 を持つ **cross-cutting** な層。全レイヤから依存可能 (= リーフ依存)。

---

## やること (do)

- `emergency_stop.flag` ファイルの read / write / trip / reset を提供
- timeout の定数を一元管理 (`safety/timeouts.go`)
- magic number / 共通定数の集約 (pip size のように domain 寄りのものは domain 側に置く)
- trip hook (`SetOnTrip`) を提供し、Counters.EmergencyTrips を atomic に increment できるようにする
- entry saga 進行中の broker_position_id を保持する in-memory tracker (`PendingPositions`) を提供し、reconcile race-window のガードに使う

---

## やらないこと (don't)

- ❌ ビジネス判定 (TP / SL / risk gate) を持つ
- ❌ DB 接続 / HTTP / Goroutine を持つ
- ❌ 上位レイヤー (usecase / app / handler / adapter / domain) を import する

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Emergency stop | `backend/internal/safety/` | `emergency.go` | `Active`, `Trip`, `TripWithDetail`, `SetOnTrip` |
| Trip reason (型安全) | `backend/internal/safety/` | `trip_reason.go` | `TripReason`, `TripFor`, `TripForWithDetail`, `Reason*` 定数 (`NakedBrokerPosition`, `StaleDBPosition`, `StartupNoCloser`, `ReconcileMarkClosedFailed`, `CandleRestoreFailed`, `ManualViaAPI`) |
| Timeout / 運用デフォルト定数 | `backend/internal/safety/` | `timeouts.go` | `ResolveExecutionTimeout`, `DefaultManualMaxHoldMinutes`, `DefaultQuantity` |
| Pending position tracker | `backend/internal/safety/` | `pending_positions.go` | `PendingPositions`, `NewPendingPositions`, `MarkPending`, `MarkResolved`, `IsPending` (`port.PendingPositionTracker` interface を満たす) |

### Magic number 集約のルール

- timeout 値 → `safety/timeouts.go`
- 数量・最大保有時間のデフォルト → 同上 (domain entity の初期値ではなく、運用デフォルト値)
- pip サイズ → `domain/market/tick.go` (= domain の責務)
- TP/SL 価格計算式 → `domain/position/price.go` の `ComputeTPSLPrices`
- broker rate limit → broker config / adapter 内部の定数 (`bot_config.gmo.private_get_limit_per_sec`)

新規追加するときは **1 ファイル 1 アグリゲートの原則** を守る (= timeouts.go に pip size を混ぜない)。

### 運用デフォルト定数の現在値

| 定数 | 値 | 根拠 |
|---|---|---|
| `ResolveExecutionTimeout` | 10s | GMO `/v1/executions` polling の MARKET fill バッファ |
| `DefaultManualMaxHoldMinutes` | 240 | デイトレ前提 (= 4 時間) |
| `DefaultQuantity` | **1000** | GMO 外為 FX の**最小発注単位** (= 0.1 lot)。`hard_limits.quantity.min` と一致。`manual_trade.HardLimits == nil` 時 / `backtest/engine.go` の ultimate fallback で参照 |

`DefaultQuantity` を変えるときは下記もまとめて更新:
- `configs/hard_limits.yaml` の `quantity.min` (運用 cap)
- `prompts/skills/04_risk_rules.md` / `05_output_format.md` (Claude advisor が出す値)
- 整合性は [usecase.md](usecase.md) `manual_trade` 規約 と合わせる

---

## テスト方法 (この層特有)

- emergency_stop: tempdir に flag file を作って `Active`/`Trip`/`TripWithDetail` を実 file で検証 (reset は file 削除 = 運用責任。bot は trip しか書かない)
- timeouts: 定数値の sanity check (整数性、上下限) を unit test で確認

```go
func TestActive_TripCreatesFile(t *testing.T) {
    dir := t.TempDir()
    flag := filepath.Join(dir, "emergency_stop.flag")
    if safety.Active(flag) { t.Fatal("should be inactive at start") }
    if err := safety.Trip(flag, "test"); err != nil { t.Fatal(err) }
    if !safety.Active(flag) { t.Fatal("should be active after Trip") }
}
```

---

## 既存実装の代表例

- [backend/internal/safety/emergency.go](../../../backend/internal/safety/emergency.go) — flag file 読み書き + trip hook
- [backend/internal/safety/trip_reason.go](../../../backend/internal/safety/trip_reason.go) — TripReason 型 + 定数 + TripFor / TripForWithDetail wrapper
- [backend/internal/safety/timeouts.go](../../../backend/internal/safety/timeouts.go) — timeout 定数
- [backend/internal/safety/pending_positions.go](../../../backend/internal/safety/pending_positions.go) — entry saga 進行中の broker_position_id を保持する in-memory tracker (`port.PendingPositionTracker` 実装)

### PendingPositions の不変条件 (race-window ガード)

Live entry saga: `PlaceOrder` → `ResolveExecution` → `PlaceSettleOCO` → `ResolveSettleLegs` → DB INSERT の間、broker には position が存在するが DB にはまだ INSERT されていない race-window が数秒〜十数秒ある。並行 reconcile がこの window で `naked_broker_position` を誤検出すると、external adopt → active config 解決失敗で emergency_stop が trip し、trip 解除まで新規エントリーも config 更新も止まる。

このため `PendingPositions` を **bot プロセスごとに 1 instance** だけ生成して、以下の責任分担で運用する:

| 主体 | timing | 操作 |
|---|---|---|
| `LiveExitProtector.Attach` | `ResolveExecution` 直後 (positionId 取得時) | `MarkPending(brokerPositionID)` |
| `LiveExitProtector` 内部 | OCO 失敗 → 補償 close 成功 / critical 経路 | 即 `MarkResolved` (broker 側にポジが残らないため) |
| caller (`manual_trade.go` / `execute_order.go`) | Attach happy-path の `defer` 経由 / DB INSERT 終端 | `MarkResolved(brokerPositionID)` |
| `Reconcile.handleNakedBroker` | naked-broker 検出の入口 | `IsPending` が true なら **skip** (次の reconcile pass を待つ) |

`MarkPending` / `MarkResolved` は対称呼び出しで、`defer` で確実に解除する。In-memory 実装なので bot restart 中の race は対象外 (= 再起動後の startup reconcile が adoption 経路で取り扱う前提)。

### trip 経路 (= emergency_stop が発火する条件)

`Trip(path, reason)` を呼ぶ箇所:

- `ExecuteOrder.OnSignal` で broker 成功 + DB insert 失敗 → `execute_position_insert_failed`
- `ExecuteOrder.OnSignal` で broker 成功 + ResolveExecution timeout (Live) → `execute_resolve_execution_failed`
- `ManageOpenPositions.closeOne` で cancel 失敗 → `cancel_settle_leg_failed_before_close`
- `ManageOpenPositions.closeOne` で cancel 後に close を拒否され、OCO の置き直しにも失敗 → `close_position_failed_after_settle_cancel`
  (置き直しに成功したとき・broker に建玉が無いとき (ERR-254) は trip しない。[FAILURE_MODES.md](../FAILURE_MODES.md) §4)
- `ManageOpenPositions.closeOne` で CloseAndRecord DB 失敗 → `close_and_record_db_failed`
- `closer.CloseAndRecord` で ok=false (二重 close 検出) → `close_race_double_broker_call`
- `ManualTradeCommand` / `ClosePositionCommand` の各 critical → `manual_*` / `close_*` prefix で同様
- `Reconcile` 系は `reconcile:<variant>:<id>` の prefix 形式。
  variant は `naked_broker_position` / `adopt_no_active_config` / `adopt_insert_failed` /
  `external_adopt_no_active_config` / `external_adopt_insert_failed` / `stale_db_position` /
  `startup_no_closer_live` / `startup_no_closer` / `claim_for_close_failed` / `mark_closed_failed` 等。
  **完全な一覧は [FAILURE_MODES.md](../FAILURE_MODES.md) の trip 表 + コード(reconcile_handlers.go の `safety.TripFor*` 呼び出し)を SSOT とする**。代表例:
  - `reconcile:naked_broker_position:<broker_id>` (runtime: broker にあって DB にない)
  - `reconcile:adopt_insert_failed:<broker_id>` (startup: adopt 中の DB Insert 失敗)
  - `reconcile:stale_db_position:<db_id>` (DB OPEN だが broker に無い。startup は DEFER→hard window 超過で trip)
  - `reconcile:mark_closed_failed:<db_id>` (MarkClosed の DB 失敗)

trip 中は worker / scheduler / handler / advisor がすべて新規エントリを拒否する (`risk.Gate.EvaluateSignal` で EmergencyStop=true で reject)。

---

## アンチパターン (= 過去にやってしまった失敗)

- ❌ emergency_stop flag を usecase 内で `os.WriteFile` して直接管理した — `safety.Trip(path, reason)` に統一した
- ❌ timeout 値が ad-hoc に複数箇所で 30 秒 / 60 秒と書かれていた — `safety/timeouts.go` に集約した
- ❌ trip 時に Counters increment を usecase 側で呼び忘れた — `safety.SetOnTrip(hook)` で hook を一箇所登録 + main.go で wiring した
- ❌ `pip size = 0.01` を strategy ファイルに hardcode していた — `domain/market/tick.go` (`market.PipSize`) に集約

---

## 関連 docs

- [OBSERVABILITY.md](../../runtime/OBSERVABILITY.md) — Counters.EmergencyTrips の観測
- [FAILURE_MODES.md](../FAILURE_MODES.md) — どんな失敗で trip するかの絶対ルール
- [usecase.md](usecase.md) — trip を発火する側

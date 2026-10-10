# Layer: Port

## 役割 (1 行)

Repository / Broker / Notifier / Advisor の **interface 定義のみ**。
usecase はここに依存し、adapter がここを実装する (= 依存性逆転)。

---

## やること (do)

- interface 定義のみを置く
- usecase が必要とする最小 method set を定義する
- DB record / Broker DTO のような **境界 record 型** を定義する (`port.PositionRecord`, `port.TradeRecord`, `port.StrategyConfigRecord` など)
- 抽象的なエラー型を定義する (例: `ErrCloseRaceLost`)

---

## やらないこと (don't)

- ❌ 具体実装 (Postgres SQL / GMO HTTP) を書く (= adapter の責務)
- ❌ Domain entity をそのまま境界 record として再利用する (Domain ↔ DB の独立性を担保)
- ❌ 上位レイヤー (usecase / app / handler / adapter) を import する
- ❌ `config` package を import する (現状: import なし、これを **R1 guardrail** として保つ)

### config 依存の禁止 (R1 guardrail)

`port.StrategyConfigRecord.Mode` / `port.AdvisorRunRecord.Mode` は **`string` 型のまま** にする。
`config.Mode` enum を port に持ち込むと `port → config` 依存が生まれ、依存方向が崩れる。
mode の type 変換は port の **外側** (usecase / adapter) で `string(cfg.Bot.Mode)` するのが正しい。

---

## 命名 / 配置

| 種別 | 場所 | ファイル名 | 例 |
|---|---|---|---|
| Repository Interface | `backend/internal/port/` | `repository.go` (集約) | `PositionRepository`, `TradeRepository`, `StrategyConfigRepository`, `AdvisorRunRepository` |
| Broker Interface | 同上 | `broker.go` (集約) | `Broker`, `OrderQuerier` |
| Notifier Interface | 同上 | `notifier.go` | `Notifier` |
| Advisor Interface | 同上 | `ai_advisor.go` | `Advisor` |
| Pending tracker | 同上 | `pending_positions.go` | `PendingPositionTracker` (entry saga 中の broker_position_id を保持。reconcile 中に DB 未挿入の position を adopt しないため) |
| 境界 record 型 | 同上 | `repository.go` | `PositionRecord`, `TradeRecord` (`CloseReason` の許容値は `take_profit`/`stop_loss`/`max_hold`/`early_exit`/`ratchet_takeprofit`/`ratchet_stoploss`/`manual`/`reconcile_cold_close`/`broker_close`/`session_flatten`。`trades.close_reason` の CHECK 制約と必ず同時拡張 — 値の正本は [DATA_MODEL.md](../../runtime/DATA_MODEL.md))、`AdvisorRunRecord` |
| Artifact Store Interface | 同上 | `repository.go` | `MarketSummaryArtifactStore`, `StrategyConfigArtifactStore` (file I/O 境界) |

### Repository interface ナーミング

メソッドは「動詞 + 名詞」で、Tx 必要なものは内部で開始する:

```go
type PositionRepository interface {
    Insert(ctx context.Context, r PositionRecord) (int64, error)
    ListOpen(ctx context.Context, symbol string) ([]PositionRecord, error)
    MarkClosed(ctx context.Context, id int64, closedAt time.Time) error
    GetByID(ctx context.Context, id int64) (PositionRecord, error)
}
```

### MaxHold 関連フィールド (early-exit 含む)

`PositionRecord` は MaxHold deadline 周りの 5 フィールドを snapshot 保持する (entry 時の active config から凍結保存、config snapshot ルール):

| フィールド | 役割 |
|---|---|
| `MaxHoldMinutes` | soft deadline。経過したら原則 close。0 = MaxHold 機能 OFF |
| `ExtensionMaxMinutes` | soft 到達後の延長窓 (max)。非対称: `unrealized_pips ≥ -threshold` (勝ち/フラット) の間はここまで延長して続伸を待ち、負けが `-threshold` 超で close。0 = 延長なし |
| `ExtensionUnrealizedPipsThreshold` | 上記延長中に許容する含み損の上限 (pips)。0 = 延長 OFF |
| `EarlyExitWindowMinutes` | soft の **前** に置く早期 exit 窓。`unrealized_pips ≥ EarlyExitTargetPips` なら soft より早く close (`close_reason="early_exit"`、`max_hold` とは別 reason)。0 = early-exit OFF (back-compat) |
| `EarlyExitTargetPips` | early-exit を発火させる PnL 閾値。マイナス値で「マイナスを浅く抑える」用途に使う |

`evaluateExit` の評価順序は 6 段階 (ratchet TP → ratchet STOP → early-exit window → hard deadline → soft + extension → soft 単独)。詳細は [usecase.md `ManageOpenPositions`](usecase.md) 参照。

### Ratchet TP フィールド (trailing take-profit) + Ratchet STOP (trailing stop)

`PositionRecord` は trailing take-profit 用に 2 snapshot + 2 runtime state を、その鏡像の trailing stop (損切り側) 用に **同 snapshot を共用しつつ 2 runtime state** を持つ:

| フィールド | 種別 | 役割 |
|---|---|---|
| `RatchetArmPips` | snapshot | 利確: peak がこの値に達したら armed。損切り: trough が `-この値` に達したら loss_armed。0=両側 OFF |
| `RatchetGivebackPips` | snapshot | 利確: armed && `peak - 現 unrealized ≥ Giveback` で `ratchet_takeprofit`。損切り: loss_armed && `現 unrealized - trough ≥ Giveback` で `ratchet_stoploss` |
| `PeakUnrealizedPips` | runtime state | OnTick で `unrealized > 既存 peak` のとき更新 (monotonic increasing) |
| `RatchetArmed` | runtime state | 一度 true になったら false に戻らない (monotonic) |
| `TroughUnrealizedPips` | runtime state | OnTick で `unrealized < 既存 trough` のとき更新 (monotonic decreasing) |
| `LossRatchetArmed` | runtime state | trough が `-RatchetArmPips` 到達で true、以後 false に戻らない (monotonic) |

method `PositionRepository.UpdateRatchetState(ctx, id, peak, armed, trough, lossArmed)` は利確側 (peak/armed) + 損切り側 (trough/loss_armed) を 1 write で `WHERE status='OPEN'` のみ更新 (CLOSING/CLOSED は silently no-op)。`evaluateExit` は ratchet TP → ratchet STOP の順で判定する。利確側が armed のときは TP 判定が先に return するので、STOP 判定に届くのは利確未 arm の玉 = 満額 SL まで往復する対象のみ。Live でも有効 (既存 `ExecuteCloseSaga` の cancel→MARKET フロー再利用、broker OCO の満額 SL は backstop として残る)。

### Per-trade 実コスト

往復コスト (手数料 + スプレッド + スリッページ) を差し引いた net で edge を判定するための境界 record 拡張。

`PositionRecord` — entry 時点コスト捕捉 (migration 0008、**全て `*float64` = nullable**。
`nil` = 未捕捉と `0` = broker 実報告ゼロ (API 手数料無料期間) を区別する):

| フィールド | 役割 |
|---|---|
| `EntryFeeJPY` | entry fill の broker 実報告手数料。live のみ (`LiveExitProtectionResult.EntryFeeJPY` 経由)。close 時に `trades.fee_jpy` (往復) へ合成 |
| `EntrySpreadPips` | 発注直前 ticker の実測スプレッド。live フォワードが backtest スプレッドモデル較正 (`cmd/spread-calibrate`) のデータ源 |
| `EntrySlippagePips` | 符号付き adverse slippage = BUY: (fill−ask)/pip, SELL: (bid−fill)/pip。正 = 不利方向 (`entryCostSnapshot`) |

`TradeRecord` — close 時のコスト確定 (migration 0007、`composeLiveCloseCosts` が合成):

| フィールド | 役割 |
|---|---|
| `FeeJPY` | 往復手数料 = entry leg (`positions.entry_fee_jpy`) + close leg (close fill 実報告 or 0.002% 推定)。**正 = コスト** |
| `SwapJPY` | close fill の settledSwap (**受取 = 正** の符号付き) |
| `FeeEstimated` | true = いずれかの leg を 0.002% 推定で補完 (旧建玉 / reconcile 推定 close) |

`ProfitLossJPY` は **GROSS のまま維持** — net = gross − FeeJPY + SwapJPY は導出側
(`DeriveEdgeMetrics` / `cmd/edge-judge`) で計算する。

`broker.go` 側: `ExecutionResolver.ResolveExecution` は `port.ResolvedExecution`
(positionID / price + fill 合算の FeeJPY / SettledSwapJPY / LossGainJPY) を返す。**符号規約**: GMO wire は
キャッシュフロー符号 (徴収が負、`amount = lossGain + fee + settledSwap`) — adapter の
`decodeExecutionList` が fee を反転して内部規約「正 = コスト」に正規化する。

### 保有上限の延長 (`ExtendMaxHold`)

`PositionRepository.ExtendMaxHold(ctx, id, addMinutes) (*MaxHoldExtended, error)` は開いている建玉の `max_hold_minutes` に `addMinutes` を加算する (UI/API の「延長ボタン」= `POST /api/positions/extend`)。`WHERE status='OPEN'` のみ対象で、CLOSING/CLOSED/未知 id には **(nil, nil)** を返す (= 延長対象なし)。戻り値 `MaxHoldExtended{MaxHoldMinutes, OpenedAt}` で deadline を再計算する。

- `max_hold_minutes` は entry 時 snapshot の per-position 凍結値 (上記「MaxHold 関連フィールド」・config snapshot ルール) なので、active config を切り替えても既存建玉には効かない。開いた建玉の上限を伸ばす **唯一の経路** がこの method。
- `OnTick` は毎 tick で `ListOpenOrClosing` から `max_hold_minutes` を読み直して `evaluateMaxHoldExit` に渡すので、UPDATE は **次 tick から効く** (close/再 build 不要)。
- extend の UPDATE と close saga の `ClaimForClose` は両方とも `status='OPEN'` をゲートにするので、deadline 際で互いにレースしても一方しか当たらず安全 (締切ギリギリで CLOSING に倒れた直後の extend は 0 行ヒット → 404)。
- TP/SL は GMO 側 OCO に乗ったまま。延長は **bot の時間決済 (MaxHold) を先送りするだけ**で下方向の歯止め (SL) には触れない。usecase は `ExtendMaxHoldCommand` ([usecase.md](usecase.md))、add_minutes の範囲検証 (1〜720) は usecase 層。

### 派生フィールド (`PositionRecord.Source`)

`PositionRecord.Source` (`PositionSource`: `bot` / `external_broker` / `paper_recovered`) は **positions テーブルの列ではなく**、`recovered_positions` を LEFT JOIN した結果から `RecoveryReasonToSource` で導出する派生フィールド。

- 空 (`""`) は `bot` と同義 (default)
- `IsExternal()` を usecase 層から呼ぶ。`EntryAdmission` は external を `max_open_positions` カウントから除外、`ManageOpenPositions.OnTick` は external を TP/SL/MaxHold close 対象から skip
- 詳細: [docs/runtime/DATA_MODEL.md `recovered_positions`](../../runtime/DATA_MODEL.md) / [docs/runtime/OPERATIONS_RUNBOOK.md §1](../../runtime/OPERATIONS_RUNBOOK.md) 不変条件「外部ポジは触らない不可侵 inventory」

### 多 symbol risk gate 用の per-symbol / account-wide 集計分離

`TradeRepository` は同じ closed_at-based 集計を **2 経路** 持つ:

| 用途 | account-wide (既存) | per-symbol (追加) |
|---|---|---|
| 件数 | `CountClosedSince(ctx, since)` | `CountClosedBySymbolSince(ctx, sym, since)` |
| 損失合計 (JPY, 絶対値) | `SumClosedLossJPYSince(ctx, since)` | `SumClosedLossJPYBySymbolSince(ctx, sym, since)` |
| 一覧 (closed_at DESC) | `ListClosedSince(ctx, since, limit)` | `ListClosedBySymbolSince(ctx, sym, since, limit)` |

これは **per-symbol cap (`MaxTradesInThisWindow` / `MaxLossInThisWindowJPY` / `MaxDailyLossJPY` / `MaxConsecutiveLosses`) と account-wide cap (`AccountMaxDailyLossJPY`) を 1 つの snapshot で同時に satisfy する** ため。
multi-symbol 化以前は account-wide method を per-symbol cap にも流用していたが、複数 symbol を抱えた瞬間に「他 symbol の trade が自 symbol の cap を誤発火させる」regression になる。`Worker.accountSnapshot` / `EntryAdmission.snapshot` の両者がこの 2 経路を併用して `risk.AccountSnapshot` の per-symbol 側 / account-wide 側を埋める。

`PositionRepository.CountOpenAllSymbols(ctx)` は account-wide `AccountOpenPositions` cap 用。OPEN+CLOSING を全 symbol で count し、**外部建玉 (`source=external_broker`) も含める** — account-wide cap は margin protection が目的で、外部建玉も margin pool を消費するため。一方 per-symbol cap (`ListOpenOrClosing` の結果から external を除外) は外部建玉を「bot が touch しない不可侵 inventory」とするルールに従う、と意図的に逆方向のポリシーを取る。

### Broker interface の ISP (Interface Segregation Principle)

現状 `port.Broker` は ticker / klines / placeOrder / closePosition / activeOrders / cancelOrder / positionsList をまとめて持っている。
callsite で実害が出ていない (= broker は 1 つしか居ない) ため、**現時点では分割しない**。

将来 Live close saga が膨らんで `Broker` interface に 15+ メソッドが乗るようになったら、以下のように分割を検討:

- `port.PublicBroker` — `GetTicker`, `GetKlines` (ticker / klines)
- `port.OrderBroker` — `PlaceOrder`, `ClosePosition`, `CancelOrder` (注文 / 決済)
- `port.OrderQuerier` — `GetActiveOrders`, `GetPositions`, `GetExecutions` (照会)

### Broker error sentinel: `ErrBrokerPositionNotFound`

Broker 実装は「建玉が broker 側に既に存在しない」ことを `port.ErrBrokerPositionNotFound` を
wrap して返す (GMO adapter は ERR-254 "Not found position" を `errorFromEnvelope` で wrap)。

close saga (usecase) はクローズ時の `errors.Is(err, port.ErrBrokerPositionNotFound)` を
**良性の「既決済レース」** (SL/TP OCO が先に約定して建玉が消えた) として扱い、emergency_stop を
踏まずに `ErrPositionAlreadyClosing` を返す (reconcile が実約定を記録)。これは naked position
(建玉が残ったまま保護注文だけ消える) の真逆なので、止血ではなくスルーが正しい。ERR-254 以外の
close エラーは従来どおり emergency_stop。背景: MaxHold 決済と broker 側 SL 約定が同秒で競合すると、
この扱いが無ければ close 失敗として emergency_stop が発火し bot 全体が止まる。

---

## テスト方法 (この層特有)

interface 自体には実装がないので **直接テストしない**。
代わりに以下:

1. usecase テストで fake 実装を作るときに interface を満たしているか go compile が検証する
2. adapter 実装テスト (integration) で同じ interface を実装している adapter が正しく動くか検証する

```go
var _ port.PositionRepository = (*adapter.PositionRepo)(nil)  // compile-time assert
```

---

## 既存実装の代表例

- [backend/internal/port/repository.go](../../../backend/internal/port/repository.go) — 全 Repository interface + 境界 record (`PositionRecord`, `TradeRecord`, `StrategyConfigRecord`, `AdvisorRunRecord`, `SignalRejectionRecord`, `CandleRecord`)
  - `CandleRepository` は 3 用途: restart 後の bar 復元 / backtest engine への replay 入力 / advisor が見た bar の監査
  - `TradeRecord.CloseReason` の取りうる値: `take_profit` / `stop_loss` / `max_hold` / `early_exit` / `ratchet_takeprofit` / `ratchet_stoploss` / `manual` / `reconcile_cold_close` / `broker_close` (reconcile が GMO 実約定から復元した TP/SL 非一致 close) / `session_flatten`。**値を増やすときは必ず `trades.close_reason` の CHECK 制約 migration も同時拡張** (忘れると INSERT が CHECK 違反 → close saga 停止 / synthetic 0 フォールバックになる)
- [backend/internal/port/broker.go](../../../backend/internal/port/broker.go) — `Broker` interface (GMO / Paper 共通)
- [backend/internal/port/notifier.go](../../../backend/internal/port/notifier.go) — `Notifier` interface (現在の実装は Stdout のみ)
- [backend/internal/port/ai_advisor.go](../../../backend/internal/port/ai_advisor.go) — `Advisor` interface (Claude CLI)

---

## アンチパターン (= 過去にやってしまった失敗)

- ❌ Repository record に `gorm.Model` や `bun` tag を埋め込んだ — adapter 側に閉じ込めた
- ❌ `port.Broker` に GMO 特有のメソッド (`GetGMOPositionDetail`) を追加した — adapter のメソッドとして閉じ込めた
- ❌ Repository が `time.Now()` を内部呼び出ししていた (= 境界の決定論性を破壊) — caller が `now` を渡す方式に直した
- ❌ `port.StrategyConfigRecord.Mode` を `config.Mode` 型にした (= 依存方向違反) — `string` で据え置きに直した

---

## 関連 docs

- [adapter.md](adapter.md) — port を実装する具体
- [usecase.md](usecase.md) — port に依存する側
- [FAILURE_MODES.md](../FAILURE_MODES.md) — Repository / Broker 失敗時の挙動
